package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
)

// Grouping by exact finding on the hub. A finding whose module names its
// exact finding (checksandalerts.HasExactFinding) gets one alert per
// destination and key across the fleet: the first node's failure goes out
// at once, and so does anything new for the destination, a severity above
// the announced one or an exact finding not announced before. Later nodes
// with an announced exact finding at or below the announced severity are
// covered growth: they join the destination's set with a pending mark, and
// one finding_nodes_changed update per key lists them on a shared tick
// (send_newly_affected_nodes_every), or within a minute when it adds sos
// nodes. Every other alert keeps fleet alert grouping's one-minute window.
//
// The data: fleet_node_incidents holds each open node incident that the
// hub knows, with its sequenced exact finding; fleet_incident_set holds,
// for every destination, the membership rows of the incidents routed to
// it, with their pending_change; fleet_incident_keys holds, for each
// destination and key, the announced severity and exact findings, written
// only with the outbox entry that carries them, and when the next update
// and reminder are due. The rules are in the design of grouping by exact
// finding, "The rule, per destination and key".

// The pending change of a membership row: what the key's next update says
// of the node.
const (
	pendingJoined    = "joined"
	pendingChanged   = "changed"
	pendingRecovered = "recovered"
)

// sosUpdateWindow is how long the nodes that an update adds at sos gather
// before the update goes out: as long as fleet alert grouping's window, so
// that sos growth goes out each minute.
const sosUpdateWindow = time.Minute

// UpdateMessageLimit bounds the message and the body of a
// finding_nodes_changed update, inside MMHotline's 16 KiB limit for a
// message. What does not fit is counted, with the command that lists it.
const UpdateMessageLimit = 12 * 1024

// nextSharedTick is the shared tick after now: the next multiple of every
// since 1970-01-01T00:00Z. Every key's updates go out together on it, so a
// triage agent covers them in one run. It counts on the wall clock, which
// is what aligns it to the times that people see.
func nextSharedTick(now time.Time, every time.Duration) time.Time {
	return time.Unix(0, (now.UnixNano()/int64(every)+1)*int64(every)).UTC()
}

// opensOrChanges reports an alert that opens, raises, or changes the exact
// finding of an incident.
func opensOrChanges(eventType string) bool {
	return checksandalerts.Opens(eventType) || eventType == checksandalerts.ExactFindingChanged
}

// nodeIncidentsOfMessage keeps fleet_node_incidents for an alert message
// that the hub sends, standing for members: an alert that opens, raises, or
// changes a node incident records it, and one that closes it removes it.
// It returns the members that count and whether the message is grouped by
// exact finding: every member that counts has an exact finding, the one
// that the hub holds, newer than the alert's when a report brought it. An
// exact_finding_changed older than the exact finding the hub holds does
// not count, nor one that an opening after a set aside named already
// (fleet_node_incidents.exact_finding_sequence_named). Neither does an alert of an incident that a set-aside entry
// of from covers (fleet_node_incidents.set_aside_entry), nor one that a
// set-aside entry no longer covers below sos: it returns the changes of
// coverage, which the destinations' sets apply instead
// (applyCoverageChanges). A raise to sos of an incident that an entry
// covered goes out as it is: sos is never set aside.
func nodeIncidentsOfMessage(tx *sql.Tx, message checksandalerts.Event, members []checksandalerts.Event, from QueuedBy) ([]checksandalerts.Event, bool, []coverageChange, error) {
	hub := from.hub()
	opening, closing := opensOrChanges(message.Type), checksandalerts.Closes(message.Type)
	if !opening && !closing {
		return members, false, nil, nil
	}
	kept, grouped, coverage := []checksandalerts.Event{}, true, []coverageChange{}
	for _, m := range members {
		if m.Incident == "" {
			kept, grouped = append(kept, m), false
			continue
		}
		var exactFinding, severity, setAside string
		var sequence, named int64
		e := tx.QueryRow("SELECT exact_finding, exact_finding_sequence, alert_severity, set_aside_entry, exact_finding_sequence_named FROM fleet_node_incidents WHERE reporter=? AND incident_id=?", m.Node, m.Incident).Scan(&exactFinding, &sequence, &severity, &setAside, &named)
		known := e == nil
		if e != nil && e != sql.ErrNoRows {
			return nil, false, nil, e
		}
		switch {
		case closing:
			if known {
				if _, e = tx.Exec("DELETE FROM fleet_node_incidents WHERE reporter=? AND incident_id=?", m.Node, m.Incident); e != nil {
					return nil, false, nil, e
				}
			}
			if m.ExactFinding == "" {
				m.ExactFinding = exactFinding
			}
			// Its alerts never went out, so neither does its closing alert.
			if setAside != "" {
				continue
			}
			kept, grouped = append(kept, m), grouped && checksandalerts.HasExactFinding(m)
			continue
		case known && message.Type == checksandalerts.ExactFindingChanged && (m.ExactFindingSequence < sequence || m.ExactFindingSequence <= named):
			// An older exact finding, or one that the opening after a set
			// aside named already, from the node's report.
			continue
		case known:
			if m.ExactFindingSequence >= sequence {
				exactFinding, sequence = m.ExactFinding, m.ExactFindingSequence
			}
			// The alert stands for the exact finding that the hub holds,
			// which a newer report may have brought.
			m.ExactFinding, m.ExactFindingSequence = exactFinding, sequence
			_, e = tx.Exec("UPDATE fleet_node_incidents SET exact_finding=?, exact_finding_sequence=?, alert_severity=? WHERE reporter=? AND incident_id=?", exactFinding, sequence, config.HigherSeverity(severity, m.AlertSeverity), m.Node, m.Incident)
		default:
			_, e = tx.Exec(`INSERT INTO fleet_node_incidents(reporter,incident_id,incident_key,node,check_group,check_name,finding,exact_finding,exact_finding_sequence,alert_severity,opened,reminder_eligible,send_alerts_only_to_destinations) VALUES(?,?,?,?,?,?,?,?,?,?,?,1,?)`,
				m.Node, m.Incident, checksandalerts.ReadableFleetIncidentKey(m, checksandalerts.IncidentKind(m.Type), hub), checksandalerts.AlertNode(m, hub), m.CheckGroup, m.Check, m.Finding, m.ExactFinding, m.ExactFindingSequence, m.AlertSeverity, m.At.UnixNano(), destinationsColumn(m.SendAlertsOnlyToDestinations))
		}
		if e != nil {
			return nil, false, nil, e
		}
		change, reason, e := settleCoverageOf(tx, from.SetAsideFindings, m.Node, m.Incident)
		switch {
		case e != nil:
			return nil, false, nil, e
		case reason != "":
			// A newly covered incident leaves the sets that hold it.
			if change != nil {
				coverage = append(coverage, *change)
			}
			continue
		case change != nil && m.AlertSeverity != "sos":
			coverage = append(coverage, *change)
			continue
		}
		kept, grouped = append(kept, m), grouped && checksandalerts.HasExactFinding(m)
	}
	return kept, grouped && len(kept) > 0, coverage, nil
}

// keyAnnouncement is what has been announced to one destination for one
// key, and when the key's next update and reminder are due (0: none).
type keyAnnouncement struct {
	severity                       string
	exactFindings                  []string
	nextUpdateDue, nextReminderDue int64
}

// covers reports that the announcement covers an alert at severity with
// exactFinding: the key is announced, at that severity or above, with that
// exact finding.
func (a keyAnnouncement) covers(severity, exactFinding string) bool {
	return a.severity != "" && config.SeverityRank(severity) <= config.SeverityRank(a.severity) && slices.Contains(a.exactFindings, exactFinding)
}

func (set IncidentSet) announcement(tx *sql.Tx, key string) (keyAnnouncement, error) {
	var a keyAnnouncement
	var exactFindings string
	e := tx.QueryRow("SELECT announced_alert_severity, announced_exact_findings, next_update_due, next_reminder_due FROM fleet_incident_keys WHERE "+inSet+" AND incident_key=?", set.with(key)...).Scan(&a.severity, &exactFindings, &a.nextUpdateDue, &a.nextReminderDue)
	if e == sql.ErrNoRows {
		return a, nil
	}
	if e == nil {
		e = json.Unmarshal([]byte(exactFindings), &a.exactFindings)
	}
	return a, e
}

// announce records, in the transaction that queues the alert carrying
// them, that the destination got the key at severity with exactFinding. The
// key's first announcement starts its reminders (remindEvery, 0 for none).
func (set IncidentSet) announce(tx *sql.Tx, key, severity, exactFinding string, remindEvery time.Duration, now time.Time) error {
	a, e := set.announcement(tx, key)
	if e != nil {
		return e
	}
	if a.severity == "" && remindEvery > 0 {
		a.nextReminderDue = now.Add(remindEvery).UnixNano()
	}
	a.severity = config.HigherSeverity(a.severity, severity)
	if exactFinding != "" && !slices.Contains(a.exactFindings, exactFinding) {
		a.exactFindings = append(a.exactFindings, exactFinding)
	}
	b, _ := json.Marshal(append([]string{}, a.exactFindings...))
	_, e = tx.Exec(`INSERT INTO fleet_incident_keys(destination,alert_source,incident_key,alert_severities_sent,last_closing_alert_at,announced_alert_severity,announced_exact_findings,next_reminder_due) VALUES(?,?,?,'',0,?,?,?)
 ON CONFLICT(destination,alert_source,incident_key) DO UPDATE SET announced_alert_severity=excluded.announced_alert_severity, announced_exact_findings=excluded.announced_exact_findings, next_reminder_due=excluded.next_reminder_due`, set.with(key, a.severity, string(b), a.nextReminderDue)...)
	return e
}

// remindEvery is the destination's remind_every for the hub's reminders of
// a grouped key, or 0 when it gets none: never, and an mmhotline
// destination, which gets no reminders.
func remindEvery(dest checksandalerts.Destination) time.Duration {
	if dest.MMHotline() != nil {
		return 0
	}
	return max(dest.RemindEvery.Value(), 0)
}

// markPending marks the membership row of the node incident with change
// for the key's next update, at now (the row's changed), and makes the
// update due: within a minute for a node at sos that joined or changed,
// otherwise on the shared tick. A node that joined stays joined when it
// also changes before the update.
func (set IncidentSet) markPending(tx *sql.Tx, key, reporter, incident, change, severity string, from QueuedBy, now time.Time) error {
	if _, e := tx.Exec("UPDATE fleet_incident_set SET pending_change=CASE WHEN ?='"+pendingChanged+"' AND pending_change='"+pendingJoined+"' THEN pending_change ELSE ? END, changed=? WHERE "+inSet+" AND reporter=? AND incident_id=?", append([]any{change, change, now.UnixNano()}, set.with(reporter, incident)...)...); e != nil {
		return e
	}
	due := nextSharedTick(now, from.newlyAffectedNodesEvery())
	if severity == "sos" && change != pendingRecovered {
		due = now.Add(sosUpdateWindow)
	}
	_, e := tx.Exec(`INSERT INTO fleet_incident_keys(destination,alert_source,incident_key,alert_severities_sent,last_closing_alert_at,next_update_due) VALUES(?,?,?,'',0,?)
 ON CONFLICT(destination,alert_source,incident_key) DO UPDATE SET next_update_due=CASE WHEN next_update_due=0 OR next_update_due>excluded.next_update_due THEN excluded.next_update_due ELSE next_update_due END`, set.with(key, due.UnixNano())...)
	return e
}

// hasOpen reports that some incident of the key is open in the set, and,
// with severity, one at that severity.
func (set IncidentSet) hasOpen(tx *sql.Tx, key, severity string) (bool, error) {
	statement, args := "SELECT EXISTS(SELECT 1 FROM fleet_incident_set WHERE "+inSet+" AND incident_key=? AND state=?", set.with(key, FleetIncidentOpen)
	if severity != "" {
		statement, args = statement+" AND alert_severity=?", append(args, severity)
	}
	var exists bool
	e := tx.QueryRow(statement+")", args...).Scan(&exists)
	return exists, e
}

// queueToDestination queues one alert of the grouped key for the
// destination name: for the mmhotline destination as MMHotline's alert
// (mmhotlineGroupedAlert) under the alert source of the key's incident in
// MMHotline, otherwise as the destination takes it, a reminder replacing
// the undelivered one it supersedes. announces is the key when the alert
// carries its announcement, or "".
func queueToDestination(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, key string, alert checksandalerts.Event, from QueuedBy, announces string) error {
	if dest.MMHotline() != nil {
		set, _ := IncidentSetOf(name, dest)
		var e error
		if alert, e = set.underTheKeysAlertSource(tx, dest, key, mmhotlineGroupedAlert(alert, from)); e != nil {
			return e
		}
	} else {
		if e := replaceQueuedReminder(tx, name, alert); e != nil {
			return e
		}
		if dest.SendOnlyTheAlertTitle {
			alert = alert.OnlyTheTitle()
		}
	}
	target, _ := json.Marshal(destinationForStorage(dest))
	return insertOutbox(tx, at, name, alert, target, announces)
}

// queueGrouped queues an alert message of a grouped key for the
// destination name, standing for members, through the destination's set.
func queueGrouped(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, message checksandalerts.Event, members []checksandalerts.Event, from QueuedBy, now time.Time) error {
	set, _ := IncidentSetOf(name, dest)
	if !checksandalerts.Closes(message.Type) {
		for _, m := range members {
			if e := set.groupedOpening(tx, at, name, dest, m, from, now); e != nil {
				return e
			}
		}
		return nil
	}
	return set.groupedClosing(tx, at, name, dest, message, members, from, now)
}

// groupedOpening applies the alert m that opens, raises, or changes the
// exact finding of a node incident to the destination's set: the
// incident joins the set, and the alert goes out at once when it is new
// for the destination (the key's first alert, a severity above the
// announced one, or an exact finding not announced before), at its own
// severity. Otherwise it is covered growth: the node's membership row is
// marked for the key's next update. An incident that the set holds as
// closed or resolved is over already and changes nothing.
func (set IncidentSet) groupedOpening(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, m checksandalerts.Event, from QueuedBy, now time.Time) error {
	hub := from.hub()
	key := checksandalerts.ReadableFleetIncidentKey(m, "failed", hub)
	change := pendingChanged
	if m.Type == "failed" {
		change = pendingJoined
	}
	var state int
	var severity string
	e := tx.QueryRow("SELECT state, alert_severity FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(m.Node, m.Incident)...).Scan(&state, &severity)
	switch {
	case e == sql.ErrNoRows:
		change = pendingJoined
		_, e = tx.Exec(`INSERT INTO fleet_incident_set(destination,alert_source,reporter,incident_id,incident_key,check_group,check_name,finding,node,opened,alert_severity,state,changed,grouped_by_exact_finding) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
			set.with(m.Node, m.Incident, key, m.CheckGroup, m.Check, m.Finding, checksandalerts.AlertNode(m, hub), m.At.UnixNano(), m.AlertSeverity, FleetIncidentOpen, now.UnixNano())...)
	case e != nil:
	case state != FleetIncidentOpen:
		return nil
	default:
		_, e = tx.Exec("UPDATE fleet_incident_set SET alert_severity=?, grouped_by_exact_finding=1 WHERE "+inSet+" AND reporter=? AND incident_id=?", append([]any{config.HigherSeverity(severity, m.AlertSeverity)}, set.with(m.Node, m.Incident)...)...)
	}
	if e != nil {
		return e
	}
	a, e := set.announcement(tx, key)
	if e != nil {
		return e
	}
	if a.covers(m.AlertSeverity, m.ExactFinding) {
		return set.markPending(tx, key, m.Node, m.Incident, change, m.AlertSeverity, from, now)
	}
	if e = set.announce(tx, key, m.AlertSeverity, m.ExactFinding, remindEvery(dest), now); e != nil {
		return e
	}
	if e = set.recordSent(tx, key, m.AlertSeverity); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE fleet_incident_set SET pending_change='' WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(m.Node, m.Incident)...); e != nil {
		return e
	}
	// The key that the set, the updates, and the closing alert use, from the
	// hub's own identity, not the key that the node's alert carried.
	m.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity)
	return queueToDestination(tx, at, name, dest, key, m, from, key)
}

// groupedClosing applies an alert that closes node incidents of a grouped
// key, standing for members, to the destination's set. A member whose
// incident the set does not hold, such as one opened before the hub
// grouped its key, goes as it would without grouping. While other
// incidents of the key stay open, the closed members are marked recovered
// for the key's next update; when the last one closes, the key's pending
// changes go out and then its one closing alert (queueClosings).
func (set IncidentSet) groupedClosing(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, message checksandalerts.Event, members []checksandalerts.Event, from QueuedBy, now time.Time) error {
	unknown, held := []checksandalerts.Event{}, []checksandalerts.Event{}
	for _, m := range members {
		var exists bool
		if e := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?)", set.with(m.Node, m.Incident)...).Scan(&exists); e != nil {
			return e
		}
		if exists {
			held = append(held, m)
		} else {
			unknown = append(unknown, m)
		}
	}
	if len(unknown) > 0 && dest.MMHotline() == nil && !from.keepsIncidents(name, dest) {
		if e := queueEvent(tx, at, name, dest, onlyStillOpen(message, unknown, members, from.hub())); e != nil {
			return e
		}
	}
	changed, closedBy, e := set.closeIncidents(tx, held, message.Type, now)
	if e != nil || len(changed) == 0 {
		return e
	}
	for _, key := range checksandalerts.Names(changed) {
		open, e := set.hasOpen(tx, key, "")
		if e != nil {
			return e
		}
		for _, m := range closedBy[key] {
			if open {
				if e = set.markPending(tx, key, m.Node, m.Incident, pendingRecovered, m.AlertSeverity, from, now); e != nil {
					return e
				}
			}
		}
	}
	if from.AwaitingFirstReports {
		return nil
	}
	waiting, e := waitingFailureKeys(tx, from.hub(), members)
	if e != nil {
		return e
	}
	closings, e := set.closeKeys(tx, changed, waiting, dest.MMHotline() != nil, now)
	if e != nil {
		return e
	}
	return set.queueClosings(tx, at, name, dest, closings, closedBy, message, from, now)
}

// queueClosings queues each key's closing alert for the destination name
// after the key's pending changes (queueUpdate): first the update, then the
// closing alert, in one transaction. The mmhotline destination gets
// MMHotline's resolution, or its resolved-sos while the key stays open below
// sos, which leaves the pending changes for the update. closedBy are the
// alerts whose incidents closed now, and cause the message that closed
// the keys, when an alert did.
func (set IncidentSet) queueClosings(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, closings []keyClosing, closedBy map[string][]checksandalerts.Event, cause checksandalerts.Event, from QueuedBy, now time.Time) error {
	for _, c := range closings {
		if c.stillOpen == nil {
			if e := set.queueUpdate(tx, at, name, dest, c.key, true, from, now); e != nil {
				return e
			}
		}
		var e error
		if dest.MMHotline() != nil {
			step := closesKey
			if c.stillOpen != nil {
				step = resolvesSos
			}
			e = set.queueMMHotlineAlerts(tx, at, name, dest, c.key, []checksandalerts.Event{c.mmhotlineAlert(cause, from, now)}, step)
		} else {
			e = queueEvent(tx, at, name, dest, c.closingAlert(closedBy[c.key], cause, from, now))
		}
		if e != nil {
			return e
		}
	}
	return nil
}

// pendingNode is a membership row with a pending change, for an update.
type pendingNode struct {
	reporter, incident, node, change, severity, exactFinding string
	marked                                                   int64
}

// queueUpdate queues the key's due pending changes for the destination name
// as one finding_nodes_changed update, and clears them. Nodes that joined
// or changed at sos are due at once; every other change is due once a
// shared tick has passed since it was marked, so a warn node never rides in
// an sos update. The update's severity is the highest among the nodes that
// it adds or changes, and info when it only names nodes that are ok again.
// When the key closes (closing), every change is due, and an update of
// recovered nodes alone is left out: the closing alert names them.
func (set IncidentSet) queueUpdate(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, key string, closing bool, from QueuedBy, now time.Time) error {
	rows, e := tx.Query(`SELECT s.reporter, s.incident_id, s.node, s.pending_change, s.alert_severity, s.changed, s.check_group, s.check_name, s.finding, COALESCE(n.exact_finding,'') FROM fleet_incident_set s
 LEFT JOIN fleet_node_incidents n ON n.reporter=s.reporter AND n.incident_id=s.incident_id
 WHERE s.destination=? AND s.alert_source=? AND s.incident_key=? AND s.pending_change!='' ORDER BY s.node`, set.with(key)...)
	if e != nil {
		return e
	}
	every := from.newlyAffectedNodesEvery()
	lastTick := now.UnixNano() / int64(every) * int64(every)
	pending, waiting := []pendingNode{}, false
	var about checksandalerts.Event
	for rows.Next() {
		var p pendingNode
		var group, check, finding string
		if e = rows.Scan(&p.reporter, &p.incident, &p.node, &p.change, &p.severity, &p.marked, &group, &check, &finding, &p.exactFinding); e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
		if !closing && p.marked >= lastTick && (p.change == pendingRecovered || p.severity != "sos") {
			waiting = true
			continue
		}
		about = checksandalerts.Event{CheckGroup: group, Check: check, Finding: finding}
		pending = append(pending, p)
	}
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e = rows.Err(); e != nil {
		return e
	}
	for _, p := range pending {
		if _, e = tx.Exec("UPDATE fleet_incident_set SET pending_change='' WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(p.reporter, p.incident)...); e != nil {
			return e
		}
	}
	nextDue := int64(0)
	if waiting {
		nextDue = nextSharedTick(now, every).UnixNano()
	}
	if _, e = tx.Exec("UPDATE fleet_incident_keys SET next_update_due=? WHERE "+inSet+" AND incident_key=?", append([]any{nextDue}, set.with(key)...)...); e != nil {
		return e
	}
	added := slices.ContainsFunc(pending, func(p pendingNode) bool { return p.change != pendingRecovered })
	if len(pending) == 0 || closing && !added {
		return nil
	}
	a, e := set.announcement(tx, key)
	if e != nil {
		return e
	}
	update := findingNodesUpdate(key, about, pending, a.exactFindings, from, now)
	if added {
		if e = set.recordSent(tx, key, update.AlertSeverity); e != nil {
			return e
		}
	}
	return queueToDestination(tx, at, name, dest, key, update, from, "")
}

// findingNodesUpdate is the finding_nodes_changed update of the key about,
// for the pending nodes. It names up to CombinedNames nodes of each exact
// finding, the exact findings not announced before first, and the nodes
// that are ok again, and counts the rest. Its at is the time it is sent.
func findingNodesUpdate(key string, about checksandalerts.Event, pending []pendingNode, announced []string, from QueuedBy, now time.Time) checksandalerts.Event {
	e := checksandalerts.Event{ID: incidents.ID(), Type: checksandalerts.FindingNodesChanged, Node: from.Node, CheckGroup: about.CheckGroup, Check: about.Check, Finding: about.Finding, At: now,
		FleetIncidentKey: checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity)}
	byExactFinding := map[string]*checksandalerts.ExactFindingNodes{}
	recovered := []string{}
	for _, p := range pending {
		if p.change == pendingRecovered {
			recovered = append(recovered, p.node)
			continue
		}
		e.AlertSeverity = config.HigherSeverity(e.AlertSeverity, p.severity)
		group := byExactFinding[p.exactFinding]
		if group == nil {
			group = &checksandalerts.ExactFindingNodes{ExactFinding: p.exactFinding, Nodes: []string{}}
			byExactFinding[p.exactFinding] = group
		}
		if len(group.Nodes) < CombinedNames {
			group.Nodes = append(group.Nodes, p.node)
		}
		group.NodeCount++
		if len(e.Nodes) < CombinedNames {
			e.Nodes = append(e.Nodes, p.node)
		}
		e.NodeCount++
	}
	if e.AlertSeverity == "" {
		e.AlertSeverity = "info"
	}
	exactFindings := checksandalerts.Names(byExactFinding)
	// The exact findings that were not announced before come first.
	slices.SortStableFunc(exactFindings, func(a, b string) int {
		return boolRank(slices.Contains(announced, a)) - boolRank(slices.Contains(announced, b))
	})
	e.RecoveredNodeCount = len(recovered)
	e.RecoveredNodes = recovered[:min(len(recovered), CombinedNames)]
	nameNodes := func(nodes []string, count int) string {
		names := strings.Join(nodes, ", ")
		if more := count - len(nodes); more > 0 {
			names += fmt.Sprintf(", and %d more", more)
		}
		return names
	}
	nodes := func(n int) string {
		if n == 1 {
			return "1 node"
		}
		return fmt.Sprintf("%d nodes", n)
	}
	parts := []string{}
	if e.NodeCount > 0 {
		parts = append(parts, nodes(e.NodeCount)+" added")
	}
	if e.RecoveredNodeCount > 0 {
		parts = append(parts, nodes(e.RecoveredNodeCount)+" ok again")
	}
	title := about.AboutCheck() + ": " + strings.Join(parts, ", ")
	e.Title, e.TitleWasShortened = checksandalerts.FitTitle([]checksandalerts.TitlePart{{Text: title}})
	e.WriteTitleWithAlertSeverity()
	e.Message = title
	// The lines after the exact findings always fit: the nodes that are ok
	// again, with up to CombinedNames names, and the command that lists
	// every node. The exact findings take what is left, less a reserve for
	// the line that counts those that do not fit and for the line that
	// MMHotline's alert adds (QueuedBy.SentBy).
	last := []string{}
	if e.RecoveredNodeCount > 0 {
		last = append(last, "ok again on "+nodes(e.RecoveredNodeCount)+": "+nameNodes(e.RecoveredNodes, e.RecoveredNodeCount))
	}
	listAll := "mmwatchdog fleet findings " + about.CheckGroup + "/" + about.Check
	if about.Finding != "" {
		listAll += " --finding '" + strings.ReplaceAll(about.Finding, "'", `'\''`) + "'"
	}
	last = append(last, "List every node on the MMWatchdog fleet hub: "+listAll)
	lines := []string{title}
	listed, budget := 0, UpdateMessageLimit-len(title)-len(strings.Join(last, "\n"))-512
	for _, exactFinding := range exactFindings {
		group := byExactFinding[exactFinding]
		block := exactFinding + "\n  on " + nodes(group.NodeCount) + ": " + nameNodes(group.Nodes, group.NodeCount)
		if !slices.Contains(announced, exactFinding) {
			block = "new: " + block
		}
		if budget -= len(block) + 1; budget < 0 {
			break
		}
		lines = append(lines, block)
		e.NodesByExactFinding = append(e.NodesByExactFinding, *group)
		listed++
	}
	if more := len(exactFindings) - listed; more > 0 {
		count := 0
		for _, exactFinding := range exactFindings[listed:] {
			count += byExactFinding[exactFinding].NodeCount
		}
		lines = append(lines, fmt.Sprintf("and %d more exact findings on %s", more, nodes(count)))
	}
	e.Body = strings.Join(append(lines, last...), "\n")
	return e
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SendDueUpdatesAndReminders is the hub's step for grouped keys on the
// delivery worker's tick: each key whose update is due sends its pending
// changes to the destination, and each key whose reminder is due sends one
// reminder, while one of its members is eligible. destinations are the
// destinations in force.
func (s *Store) SendDueUpdatesAndReminders(destinations map[string]checksandalerts.Destination, now time.Time) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	from := QueuedBy{Role: FleetHubSender, Node: s.Node, FleetIdentity: s.FleetIdentity, NewlyAffectedNodesEvery: s.newlyAffectedNodesEvery()}
	for _, due := range []struct {
		column string
		send   func(set IncidentSet, name string, dest checksandalerts.Destination, key string) error
	}{
		{"next_update_due", func(set IncidentSet, name string, dest checksandalerts.Destination, key string) error {
			return set.queueUpdate(tx, now.UnixNano(), name, dest, key, false, from, now)
		}},
		{"next_reminder_due", func(set IncidentSet, name string, dest checksandalerts.Destination, key string) error {
			return set.remind(tx, name, dest, key, from, now)
		}},
	} {
		rows, e := tx.Query("SELECT destination, alert_source, incident_key FROM fleet_incident_keys WHERE "+due.column+">0 AND "+due.column+"<=?", now.UnixNano())
		if e != nil {
			return e
		}
		type dueKey struct {
			set IncidentSet
			key string
		}
		keys := []dueKey{}
		for rows.Next() {
			var k dueKey
			if e = rows.Scan(&k.set.Destination, &k.set.AlertSource, &k.key); e != nil {
				_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
				return e
			}
			keys = append(keys, k)
		}
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
		if e = rows.Err(); e != nil {
			return e
		}
		for _, k := range keys {
			dest, configured := destinations[k.set.Destination]
			if current, _ := IncidentSetOf(k.set.Destination, dest); !configured || current != k.set {
				// The set of a destination that is gone; the next commit
				// deletes it (keepOnlyIncidentSets).
				if _, e = tx.Exec("UPDATE fleet_incident_keys SET "+due.column+"=0 WHERE "+inSet+" AND incident_key=?", k.set.with(k.key)...); e != nil {
					return e
				}
				continue
			}
			if e = due.send(k.set, k.set.Destination, dest, k.key); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}

// remind queues the hub's one reminder for the destination name and the
// key, while at least one member is eligible: open in the set, eligible
// on its node (reminder_eligible: neither in maintenance nor in quiet time),
// and still routed to the destination at its current severity, which the
// membership row has as long as every raise went to the destination. The
// next reminder is due remind_every later; without an eligible member, the
// hub looks again a minute later.
func (set IncidentSet) remind(tx *sql.Tx, name string, dest checksandalerts.Destination, key string, from QueuedBy, now time.Time) error {
	every := remindEvery(dest)
	if every <= 0 {
		_, e := tx.Exec("UPDATE fleet_incident_keys SET next_reminder_due=0 WHERE "+inSet+" AND incident_key=?", set.with(key)...)
		return e
	}
	rows, e := tx.Query(`SELECT s.node, s.alert_severity, s.check_group, s.check_name, s.finding, n.opened FROM fleet_incident_set s
 JOIN fleet_node_incidents n ON n.reporter=s.reporter AND n.incident_id=s.incident_id
 WHERE s.destination=? AND s.alert_source=? AND s.incident_key=? AND s.state=? AND n.reminder_eligible=1 AND n.set_aside_entry='' AND n.alert_severity=s.alert_severity ORDER BY s.node`, set.with(key, FleetIncidentOpen)...)
	if e != nil {
		return e
	}
	reminder := checksandalerts.Event{Type: "reminder", Destination: name, Node: from.Node}
	var opened int64
	for rows.Next() {
		var node, severity string
		var since int64
		if e = rows.Scan(&node, &severity, &reminder.CheckGroup, &reminder.Check, &reminder.Finding, &since); e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
		reminder.AlertSeverity = config.HigherSeverity(reminder.AlertSeverity, severity)
		if len(reminder.Nodes) < CombinedNames {
			reminder.Nodes = append(reminder.Nodes, node)
		}
		reminder.NodeCount++
		if opened == 0 || since < opened {
			opened = since
		}
	}
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e = rows.Err(); e != nil {
		return e
	}
	next := now.Add(every)
	if reminder.NodeCount > 0 {
		reminder.ID, reminder.At = incidents.ID(), now
		reminder.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity)
		names := strings.Join(reminder.Nodes, ", ")
		if more := reminder.NodeCount - len(reminder.Nodes); more > 0 {
			names += fmt.Sprintf(", and %d more", more)
		}
		reminder.Message = fmt.Sprintf("%s still failing on %d nodes since %s: %s", reminder.AboutCheck(), reminder.NodeCount, checksandalerts.ClockTime(time.Unix(0, opened), now), names)
		if reminder.NodeCount == 1 {
			reminder.Node, reminder.Nodes, reminder.NodeCount = reminder.Nodes[0], nil, 0
			reminder.Message = fmt.Sprintf("%s still failing since %s", reminder.AboutCheck(), checksandalerts.ClockTime(time.Unix(0, opened), now))
		}
		reminder.WriteAlertTexts([]checksandalerts.Event{reminder}, from.hub())
		if e = queueToDestination(tx, now.UnixNano(), name, dest, key, reminder, from, ""); e != nil {
			return e
		}
	} else {
		next = now.Add(time.Minute)
	}
	_, e = tx.Exec("UPDATE fleet_incident_keys SET next_reminder_due=? WHERE "+inSet+" AND incident_key=?", append([]any{next.UnixNano()}, set.with(key)...)...)
	return e
}
