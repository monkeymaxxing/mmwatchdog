package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
)

// Fleet incident sets. A destination whose service keeps incidents of its
// own has one: the mmhotline destination, and every destination whose
// module's description states service_keeps_incidents: true. For each
// readable fleet incident key (checksandalerts.ReadableFleetIncidentKey),
// the set holds one row for each MMWatchdog incident of a node whose alerts went to
// the destination, from the alerts that the destination gets and from each
// node's latest report, and the destination gets one closing alert once the
// last of them has closed. A standalone node keeps the same sets, with its
// own incidents only. A joined node keeps none: it records the incidents
// whose alerts it sent to such a destination without the hub, for its
// reports. The rules are in docs/changing-mmwatchdog/design/fleet.md under "One incident per
// check across the fleet"; what only the mmhotline destination does is in
// mmhotlinequeue.go.

// The state of one MMWatchdog incident in a fleet incident set: open until the
// node's incident closes, closed until every incident of its key has closed
// and the one closing alert is queued, then resolved. A resolved row
// remains until a newer report no longer shows the incident, or a day, so a
// report sent before the incident closed cannot open it again.
const (
	FleetIncidentOpen = iota
	fleetIncidentClosed
	fleetIncidentResolved
)

// IncidentSet names the fleet incident set of one destination: the
// destination's name, and for the mmhotline destination its alert source,
// so that connecting another alert source starts the set over.
type IncidentSet struct {
	Destination string
	AlertSource string
}

// IncidentSetOf returns the fleet incident set of the destination named
// name, and whether the destination keeps incidents, so that its set holds
// every incident of its alerts: on the hub and on a standalone node, where
// the module's description decides. Every other destination's set holds
// only the incidents of findings with an exact finding, on the hub.
func IncidentSetOf(name string, d checksandalerts.Destination) (IncidentSet, bool) {
	if p := d.MMHotline(); p != nil {
		return IncidentSet{Destination: name, AlertSource: p.AlertSourceID}, true
	}
	return IncidentSet{Destination: name}, d.ServiceKeepsIncidents()
}

// inSet selects the rows of one fleet incident set in a statement;
// IncidentSet.with gives its arguments.
const inSet = "destination=? AND alert_source=?"

func (set IncidentSet) with(more ...any) []any {
	return append([]any{set.Destination, set.AlertSource}, more...)
}

// senderRole is who queues an alert for its destinations.
type senderRole int

const (
	// StandaloneSender keeps the fleet incident sets for its own incidents,
	// so its recovery is the closing alert.
	StandaloneSender senderRole = iota
	// FleetHubSender keeps the fleet incident sets across the fleet.
	FleetHubSender
	// FallbackSender is a joined node's direct delivery without the hub: it
	// sends failures, raises, reminders, and notices, and never an alert
	// that closes an incident to a destination that keeps a fleet incident
	// set.
	FallbackSender
)

// QueuedBy is who queues an alert: its role and its own node name, and
// what it knows about the fleet incident sets.
type QueuedBy struct {
	Role          senderRole
	Node          string
	FleetIdentity string
	// AwaitingFirstReports is set on the hub while some joined node has not
	// reported since the hub started (fleet.Hub.AwaitingFirstReports). No
	// key closes then, because that node may still have an incident of the
	// key open that the hub does not know of; the hub's set step closes the
	// keys afterwards (ReconcileIncidentSets).
	AwaitingFirstReports bool
	// DestinationsWhoseServiceKeepsIncidents are, for a joined node's
	// fallback, the destinations that the fleet settings say keep
	// incidents: the hub's own modules decide, not the node's copies.
	DestinationsWhoseServiceKeepsIncidents []string
	// NewlyAffectedNodesEvery is, on the hub, the shared tick of the updates
	// of grouped keys: fleet_hub_role.send_newly_affected_nodes_every.
	NewlyAffectedNodesEvery time.Duration
	// SetAsideFindings are, on the hub, the entries of
	// set_aside_findings.yml in force (setaside.go).
	SetAsideFindings config.SetAsideFindings
	// Now is the time of the hub's step that queues, from its entry point;
	// zero takes the clock's.
	Now time.Time
}

func (f QueuedBy) now() time.Time {
	if f.Now.IsZero() {
		return time.Now()
	}
	return f.Now
}

func (f QueuedBy) newlyAffectedNodesEvery() time.Duration {
	if f.NewlyAffectedNodesEvery > 0 {
		return f.NewlyAffectedNodesEvery
	}
	return config.DefaultSendNewlyAffectedNodesEvery.Value()
}

// keepsIncidents reports a destination other than the mmhotline
// destination whose service keeps incidents of its own: as the hub's
// settings say on a joined node, and as the module's description says
// everywhere else.
func (f QueuedBy) keepsIncidents(name string, d checksandalerts.Destination) bool {
	if f.Role == FallbackSender {
		return slices.Contains(f.DestinationsWhoseServiceKeepsIncidents, name)
	}
	return d.ServiceKeepsIncidents()
}

func (f QueuedBy) hub() string {
	if f.Role == FleetHubSender {
		return f.Node
	}
	return ""
}

// openIncidents adds the incidents of members, the alerts of a failure or
// raise message, to the set, or raises the severity of those that it holds
// open. A raise can be the first alert of an incident that a destination
// taking only higher severities gets. A member whose incident the set holds
// as closed or resolved is over already: it is not kept, so it is not sent,
// and its key is in over, because it no longer holds its key open.
func (set IncidentSet) openIncidents(tx *sql.Tx, members []checksandalerts.Event, hub string, now time.Time) (kept []checksandalerts.Event, over map[string]string, e error) {
	kept, over = []checksandalerts.Event{}, map[string]string{}
	for _, m := range members {
		key := checksandalerts.ReadableFleetIncidentKey(m, checksandalerts.IncidentKind(m.Type), hub)
		var state int
		var severity string
		e = tx.QueryRow("SELECT state, alert_severity FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(m.Node, m.Incident)...).Scan(&state, &severity)
		switch {
		case e == sql.ErrNoRows:
			_, e = tx.Exec(`INSERT INTO fleet_incident_set(destination,alert_source,reporter,incident_id,incident_key,check_group,check_name,finding,node,opened,alert_severity,state,changed) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				set.with(m.Node, m.Incident, key, m.CheckGroup, m.Check, m.Finding, checksandalerts.AlertNode(m, hub), m.At.UnixNano(), m.AlertSeverity, FleetIncidentOpen, now.UnixNano())...)
			kept = append(kept, m)
		case e == nil && state == FleetIncidentOpen:
			if higher := config.HigherSeverity(severity, m.AlertSeverity); higher != severity {
				_, e = tx.Exec("UPDATE fleet_incident_set SET alert_severity=? WHERE "+inSet+" AND reporter=? AND incident_id=?", append([]any{higher}, set.with(m.Node, m.Incident)...)...)
			}
			kept = append(kept, m)
		case e == nil:
			over[key] = "recovered"
		}
		if e != nil {
			return nil, nil, e
		}
	}
	return kept, over, nil
}

// closeIncidents closes the open incidents of members, the alerts of a
// recovery or retirement whose type is change. It returns the key of each
// with the change, and by key the members whose incidents it closed. A
// member whose incident the set does not hold open is none whose failure
// the destination got.
func (set IncidentSet) closeIncidents(tx *sql.Tx, members []checksandalerts.Event, change string, now time.Time) (map[string]string, map[string][]checksandalerts.Event, error) {
	closed, closedBy := map[string]string{}, map[string][]checksandalerts.Event{}
	for _, m := range members {
		var key string
		e := tx.QueryRow("SELECT incident_key FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=? AND state=?", set.with(m.Node, m.Incident, FleetIncidentOpen)...).Scan(&key)
		if e == sql.ErrNoRows {
			continue
		}
		if e != nil {
			return nil, nil, e
		}
		if _, e = tx.Exec("UPDATE fleet_incident_set SET state=?, changed=? WHERE "+inSet+" AND reporter=? AND incident_id=?", append([]any{fleetIncidentClosed, now.UnixNano()}, set.with(m.Node, m.Incident)...)...); e != nil {
			return nil, nil, e
		}
		closed[key] = change
		closedBy[key] = append(closedBy[key], m)
	}
	return closed, closedBy, nil
}

// keyClosing is a key of a fleet incident set that closes: none of its
// incidents is open any more, or, for the mmhotline destination alone, none
// of its open incidents is sos any more although the key was sent at sos.
type keyClosing struct {
	// key is the readable fleet incident key, and change what closed its
	// last open incident: recovered, can_check_again, or closed_without_recovery.
	key, change string
	// closed are the incidents of the key that closed since its last
	// closing alert, and sent the severities that the destination got for
	// the key in that time, lowest first.
	closed keyRows
	sent   []string
	// stillOpen, for the mmhotline destination's resolved-sos, are the
	// incidents that keep the key open at a severity below sos.
	stillOpen *keyRows
	// othersStillOpen reports, on the hub, that other nodes still have an
	// incident of the key open with an exact finding, routed elsewhere or
	// set aside: the closing alert claims recovery only for the nodes that
	// it covered.
	othersStillOpen bool
}

// closeKeys returns the keys among changed that close, and marks the
// closed rows of each resolved. changed gives the change that closed each
// key's last incident. A key closes once all its incidents have closed and
// no failure of the key waits in fleet alert grouping (waiting). With
// sosResolvesFirst, for the mmhotline destination, a key that was sent at
// sos closes its SOS as soon as none of its open incidents is sos: the key
// is then a key of the severity that remains.
//
// A key that closes is no longer announced: a key that opens again starts
// fresh. After resolved-sos the announced severity is the highest severity
// still open. Whether a key closes is decided from indexed lookups, so an
// alert that closes one of many incidents of a key costs the same as any.
func (set IncidentSet) closeKeys(tx *sql.Tx, changed map[string]string, waiting map[string]bool, sosResolvesFirst bool, now time.Time) ([]keyClosing, error) {
	closings := []keyClosing{}
	for _, key := range checksandalerts.Names(changed) {
		if waiting[key] {
			continue
		}
		sent, e := set.keySent(tx, key)
		if e != nil {
			return nil, e
		}
		open, e := set.hasOpen(tx, key, "")
		if e != nil {
			return nil, e
		}
		var stillOpen *keyRows
		if open {
			if !sosResolvesFirst || config.HighestSeverity(sent) != "sos" {
				continue
			}
			if openSos, e := set.hasOpen(tx, key, "sos"); e != nil || openSos {
				if e != nil {
					return nil, e
				}
				continue
			}
			rows, e := set.keyRows(tx, key, FleetIncidentOpen)
			if e != nil {
				return nil, e
			}
			stillOpen = &rows
		}
		closed, e := set.keyRows(tx, key, fleetIncidentClosed)
		if e != nil {
			return nil, e
		}
		if len(closed.nodes) == 0 {
			continue
		}
		c := keyClosing{key: key, change: changed[key], closed: closed, sent: sent, stillOpen: stillOpen}
		// After a closing alert the key starts over with the severities of
		// what stays open, and the key keeps the time of the closing alert
		// (reopenings).
		if stillOpen != nil {
			_, e = tx.Exec(`INSERT INTO fleet_incident_keys(destination,alert_source,incident_key,alert_severities_sent,last_closing_alert_at,announced_alert_severity) VALUES(?,?,?,?,?,?)
 ON CONFLICT(destination,alert_source,incident_key) DO UPDATE SET alert_severities_sent=excluded.alert_severities_sent, last_closing_alert_at=excluded.last_closing_alert_at, announced_alert_severity=excluded.announced_alert_severity`, set.with(key, stillOpen.highest, now.UnixNano(), stillOpen.highest)...)
		} else {
			_, e = tx.Exec(`INSERT INTO fleet_incident_keys(destination,alert_source,incident_key,alert_severities_sent,last_closing_alert_at) VALUES(?,?,?,'',?)
 ON CONFLICT(destination,alert_source,incident_key) DO UPDATE SET alert_severities_sent='', last_closing_alert_at=excluded.last_closing_alert_at, announced_alert_severity='', announced_exact_findings='[]', next_update_due=0, next_reminder_due=0`, set.with(key, now.UnixNano())...)
			if e == nil {
				e = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM fleet_node_incidents WHERE incident_key=? AND exact_finding!='')", key).Scan(&c.othersStillOpen)
			}
		}
		if e != nil {
			return nil, e
		}
		if _, e = tx.Exec("UPDATE fleet_incident_set SET state=?, changed=? WHERE "+inSet+" AND incident_key=? AND state=?", append([]any{fleetIncidentResolved, now.UnixNano()}, set.with(key, fleetIncidentClosed)...)...); e != nil {
			return nil, e
		}
		closings = append(closings, c)
	}
	return closings, nil
}

// keyRows are the rows of a key in one state: their nodes, each once and
// in order; what the rows share, in an event: the reporter as its node,
// the check group, the check, and the finding, each left empty when the
// rows differ in it; and the highest severity among the rows.
type keyRows struct {
	nodes   []string
	about   checksandalerts.Event
	highest string
}

func (set IncidentSet) keyRows(tx *sql.Tx, key string, state int) (keyRows, error) {
	var out keyRows
	rows, e := tx.Query("SELECT node, reporter, check_group, check_name, finding, alert_severity FROM fleet_incident_set WHERE "+inSet+" AND incident_key=? AND state=? ORDER BY node", set.with(key, state)...)
	if e != nil {
		return out, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	for rows.Next() {
		var node, severity string
		var about checksandalerts.Event
		if e = rows.Scan(&node, &about.Node, &about.CheckGroup, &about.Check, &about.Finding, &severity); e != nil {
			return out, e
		}
		switch {
		case len(out.nodes) == 0:
			out.about = about
		default:
			if about.Node != out.about.Node {
				out.about.Node = ""
			}
			if about.CheckGroup != out.about.CheckGroup {
				out.about.CheckGroup = ""
			}
			if about.Check != out.about.Check {
				out.about.Check = ""
			}
			if about.Finding != out.about.Finding {
				out.about.Finding = ""
			}
		}
		if len(out.nodes) == 0 || out.nodes[len(out.nodes)-1] != node {
			out.nodes = append(out.nodes, node)
		}
		out.highest = config.HigherSeverity(out.highest, severity)
	}
	return out, rows.Err()
}

// keySent lists the severities that the destination got for a key since
// the key's last closing alert, lowest first.
func (set IncidentSet) keySent(tx *sql.Tx, key string) ([]string, error) {
	var sent string
	e := tx.QueryRow("SELECT alert_severities_sent FROM fleet_incident_keys WHERE "+inSet+" AND incident_key=?", set.with(key)...).Scan(&sent)
	if e == sql.ErrNoRows || sent == "" {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return strings.Split(sent, ","), nil
}

// lastClosingAlert is when the destination's last closing alert for a key
// went out, or 0 when none is remembered: for a day after it, while the key
// has no incident open (Store.PruneInTransaction).
func (set IncidentSet) lastClosingAlert(tx *sql.Tx, key string) (int64, error) {
	var at int64
	e := tx.QueryRow("SELECT last_closing_alert_at FROM fleet_incident_keys WHERE "+inSet+" AND incident_key=?", set.with(key)...).Scan(&at)
	if e == sql.ErrNoRows {
		return 0, nil
	}
	return at, e
}

// recordSent records that the destination got the key at these
// severities.
func (set IncidentSet) recordSent(tx *sql.Tx, key string, severities ...string) error {
	sent, e := set.keySent(tx, key)
	if e != nil {
		return e
	}
	all := lowestFirst(append(slices.Clone(sent), severities...))
	if len(all) == 0 || slices.Equal(all, sent) {
		return nil
	}
	_, e = tx.Exec("INSERT INTO fleet_incident_keys(destination,alert_source,incident_key,alert_severities_sent,last_closing_alert_at) VALUES(?,?,?,?,0) ON CONFLICT(destination,alert_source,incident_key) DO UPDATE SET alert_severities_sent=excluded.alert_severities_sent", set.with(key, strings.Join(all, ","))...)
	return e
}

// severitiesSentTo lists the severities that the destination name got for
// an incident that a report shows: those that the incident's alerts went
// out with, or its severity when the report has none, that the destination
// takes, by the check's send_alerts_only_to_destinations or by its
// alert_severities (config.GoesTo), lowest first.
func severitiesSentTo(name string, destinations map[string]checksandalerts.Destination, m checksandalerts.Event) []string {
	sent := m.AlertSeveritiesSentForIncident
	if len(sent) == 0 {
		sent = []string{m.AlertSeverity}
	}
	taken := []string{}
	for _, severity := range lowestFirst(sent) {
		if config.GoesTo(name, destinations[name], checksandalerts.Event{Type: "failed", AlertSeverity: severity, SendAlertsOnlyToDestinations: m.SendAlertsOnlyToDestinations}, destinations) {
			taken = append(taken, severity)
		}
	}
	return taken
}

// lowestFirst lists each severity of severities once, the lowest first.
func lowestFirst(severities []string) []string {
	out := []string{}
	for _, s := range severities {
		if config.SeverityRank(s) >= 0 && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return config.SeverityRank(a) - config.SeverityRank(b) })
	return out
}

// waitingFailureKeys names the keys of failures still waiting in the hub's
// fleet alert grouping, other than the members being sent now. Such a
// failure holds its key open: it goes to the destinations after the
// grouping window. hub is the fleet hub's name; elsewhere nothing waits.
func waitingFailureKeys(tx *sql.Tx, hub string, sending []checksandalerts.Event) (map[string]bool, error) {
	if hub == "" {
		return nil, nil
	}
	now := map[string]bool{}
	for _, m := range sending {
		now[m.Node+"\x00"+m.ID] = true
	}
	rows, e := tx.Query("SELECT node, id, type, subject FROM intake WHERE state!=? AND type IN ('failed','cannot_check',?,?)", intakeDone, checksandalerts.FailureIncidentEvents.Raised, checksandalerts.CannotCheckIncidentEvents.Raised)
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	keys := map[string]bool{}
	for rows.Next() {
		var node, id, kind, subject string
		if e = rows.Scan(&node, &id, &kind, &subject); e != nil {
			return nil, e
		}
		if now[node+"\x00"+id] {
			continue
		}
		keys[checksandalerts.ReadableFleetIncidentKey(IntakeSubjectEvent(node, subject, kind), checksandalerts.IncidentKind(kind), hub)] = true
	}
	return keys, rows.Err()
}

// queueWithIncidentSet queues an alert message for the destination name,
// whose service keeps incidents of its own, and keeps the destination's
// fleet incident set in transaction tx. members are the alerts the message
// stands for. A failure or raise goes out as it is, and only for the
// members whose incident the set does not know is over. A reminder and a
// notice go out as they are. An alert that closes an incident goes out
// only when its key's set has no open incident left, and then once:
// closingAlert. A joined node's fallback sends no such alert and records
// each failure or raise for its reports instead.
func queueWithIncidentSet(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, message checksandalerts.Event, members []checksandalerts.Event, from QueuedBy, now time.Time) error {
	set := IncidentSet{Destination: name}
	var changed map[string]string
	var closedBy map[string][]checksandalerts.Event
	switch {
	case opensOrChanges(message.Type) && from.Role == FallbackSender:
		// Reports carry it until the hub has heard of it, so the hub sends
		// the closing alert, even when the incident closes before the hub is
		// back.
		if e := recordSentWithoutTheHub(tx, name, members, "", now); e != nil {
			return e
		}
		return queueEvent(tx, at, name, dest, message)
	case checksandalerts.Opens(message.Type):
		kept, over, e := set.openIncidents(tx, members, from.hub(), now)
		if e != nil {
			return e
		}
		changed = over
		if len(kept) > 0 {
			severities := []string{}
			for _, m := range kept {
				severities = append(severities, m.AlertSeverity)
			}
			if e = set.recordSent(tx, checksandalerts.ReadableFleetIncidentKey(kept[0], checksandalerts.IncidentKind(kept[0].Type), from.hub()), severities...); e != nil {
				return e
			}
			if e = queueEvent(tx, at, name, dest, onlyStillOpen(message, kept, members, from.hub())); e != nil {
				return e
			}
		}
	case checksandalerts.Closes(message.Type) && from.Role == FallbackSender:
		return nil
	case checksandalerts.Closes(message.Type):
		var e error
		if changed, closedBy, e = set.closeIncidents(tx, members, message.Type, now); e != nil {
			return e
		}
	case message.Type == "reminder":
		// A reminder whose incident the set knows is over would open the
		// service's incident again after its closing alert.
		open, e := set.notOver(tx, members)
		if e != nil || len(open) == 0 {
			return e
		}
		return queueEvent(tx, at, name, dest, onlyStillOpen(message, open, members, from.hub()))
	default:
		return queueEvent(tx, at, name, dest, message)
	}
	if len(changed) == 0 || from.AwaitingFirstReports {
		return nil
	}
	waiting, e := waitingFailureKeys(tx, from.hub(), members)
	if e != nil {
		return e
	}
	closings, e := set.closeKeys(tx, changed, waiting, false, now)
	if e != nil {
		return e
	}
	return set.queueClosings(tx, at, name, dest, closings, closedBy, message, from, now)
}

// notOver lists the members whose incident the set does not hold as closed
// or resolved.
func (set IncidentSet) notOver(tx *sql.Tx, members []checksandalerts.Event) ([]checksandalerts.Event, error) {
	open := []checksandalerts.Event{}
	for _, m := range members {
		var state int
		e := tx.QueryRow("SELECT state FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(m.Node, m.Incident)...).Scan(&state)
		switch {
		case e == sql.ErrNoRows || (e == nil && state == FleetIncidentOpen):
			open = append(open, m)
		case e != nil:
			return nil, e
		}
	}
	return open, nil
}

// onlyStillOpen is a failure, raise, or reminder message as it goes out
// when only kept of its members still have their incident open: the
// message itself when they all do, and otherwise one that counts only
// those.
func onlyStillOpen(message checksandalerts.Event, kept, members []checksandalerts.Event, hub string) checksandalerts.Event {
	if len(kept) == len(members) {
		return message
	}
	shown := Combine(kept, hub)
	if len(kept) > 1 || shown.Title == "" {
		shown.WriteAlertTexts(kept, hub)
	}
	return shown
}

// closingAlert is the one alert that tells a destination whose service
// keeps incidents that the key's incident is over. It carries the
// severities that the destination got for the key since its last closing
// alert, and the highest of them, so that two destinations of one service
// that take different severities each close only what they opened there.
// When every closed incident of the key came from one node, it is that
// node's own closing alert, unchanged but for those severities, so a
// standalone node and a single failing node send as they would without the
// set. Otherwise it is a new alert about the key across the fleet: the
// closing type, the key, the severities, and the nodes that failed.
// closedNow are the alerts whose incidents of the key closed in this step,
// and cause is the message that closed the key, when an alert did.
func (c keyClosing) closingAlert(closedNow []checksandalerts.Event, cause checksandalerts.Event, from QueuedBy, now time.Time) checksandalerts.Event {
	if len(c.closed.nodes) == 1 && len(closedNow) > 0 {
		own := closedNow[len(closedNow)-1]
		// A message of one alert carries the texts of that alert.
		if cause.ID == own.ID {
			own = cause
		}
		if len(c.sent) > 0 {
			own.AlertSeveritiesSentForIncident, own.AlertSeverity = c.sent, config.HighestSeverity(c.sent)
			own.WriteTitleWithAlertSeverity()
		}
		return own
	}
	e := c.closed.about
	e.ID, e.FleetIncidentKey, e.Type = incidents.ID(), checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(c.key), from.Node, from.FleetIdentity), checksandalerts.FailureIncidentEvents.Recovered
	switch {
	case c.change == "closed_without_recovery":
		e.Type = "closed_without_recovery"
	case checksandalerts.ReadableKeyIsAboutCannotCheck(c.key):
		e.Type = checksandalerts.CannotCheckIncidentEvents.Recovered
	}
	e.AlertSeverity, e.AlertSeveritiesSentForIncident = config.HighestSeverity(c.sent), c.sent
	e.At = now
	latest := ""
	if cause.ID != "" {
		e.At, latest = cause.At, cause.Message
	}
	e.NodeCount = len(c.closed.nodes)
	e.Nodes = c.closed.nodes[:min(len(c.closed.nodes), CombinedNames)]
	e.WriteFleetIncidentClosingTexts(c.key, latest)
	if c.othersStillOpen && e.NodeCount > 1 {
		parts, was := closingTitle(c.key, e, e.Nodes, e.NodeCount-len(e.Nodes), e.Type, true)
		e.Message = checksandalerts.TitleText(parts) + " (" + was + ")"
		e.Title, e.TitleWasShortened = checksandalerts.FitTitle(parts)
		e.WriteTitleWithAlertSeverity()
		lines := []string{e.Message}
		if latest != "" {
			lines = append(lines, "latest: "+latest)
		}
		e.Body = primitives.AsReadableLines(strings.Join(lines, "\n"))
	}
	return e
}

// closingTitle is the title of a key's closing alert before it is fitted,
// and what was open (checksandalerts.FleetIncidentClosingTitle). While
// other nodes of the fleet still have an incident of the key open
// (othersStillOpen), a closing alert for several nodes claims recovery
// only for the nodes that it covered: "CVE-2026-123158 openssh is ok again
// on the 3 nodes that this alert covered". "on every node" is said only
// when no node has the key open.
func closingTitle(readable string, about checksandalerts.Event, nodes []string, more int, change string, othersStillOpen bool) ([]checksandalerts.TitlePart, string) {
	parts, was := checksandalerts.FleetIncidentClosingTitle(readable, about, nodes, more, change)
	if !othersStillOpen || len(nodes)+more <= 1 {
		return parts, was
	}
	covered := fmt.Sprintf(" on the %d nodes that this alert covered", len(nodes)+more)
	if change == "closed_without_recovery" {
		return []checksandalerts.TitlePart{{Text: about.AboutCheck() + " incidents closed" + covered + ", maybe not fixed"}}, was
	}
	return []checksandalerts.TitlePart{{Text: about.AboutCheck() + " is ok again" + covered}}, was
}

// recordSentWithoutTheHub records, on a joined node, each incident of
// members whose failure or raise its fallback sent to the destination
// without the hub, with every severity sent, and the MMHotline alert
// source that it sent under ("" for the destination's own).
func recordSentWithoutTheHub(tx *sql.Tx, destination string, members []checksandalerts.Event, alertSource string, now time.Time) error {
	for _, m := range members {
		var earlier string
		e := tx.QueryRow("SELECT alert_severities_sent_for_incident FROM incidents_sent_without_the_hub WHERE destination=? AND incident_id=?", destination, m.Incident).Scan(&earlier)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		sent := strings.Join(lowestFirst(append(strings.Split(earlier, ","), m.AlertSeverity)), ",")
		if _, e = tx.Exec(`INSERT INTO incidents_sent_without_the_hub(destination,incident_id,check_group,check_name,finding,kind,alert_severities_sent_for_incident,at,exact_finding,exact_finding_sequence,alert_source) VALUES(?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(destination,incident_id) DO UPDATE SET alert_severities_sent_for_incident=excluded.alert_severities_sent_for_incident, alert_source=excluded.alert_source,
 exact_finding=CASE WHEN excluded.exact_finding_sequence>=exact_finding_sequence THEN excluded.exact_finding ELSE exact_finding END, exact_finding_sequence=MAX(exact_finding_sequence,excluded.exact_finding_sequence)`,
			destination, m.Incident, m.CheckGroup, m.Check, m.Finding, checksandalerts.IncidentKind(m.Type), sent, now.UnixNano(), m.ExactFinding, m.ExactFindingSequence, alertSource); e != nil {
			return e
		}
	}
	return nil
}

// keepOnlyIncidentSets deletes, in the commit tx, the rows of every fleet
// incident set that the destinations in force do not keep: of a
// destination that is gone, and the mmhotline destination's rows of another
// alert source. A destination whose service keeps no incidents keeps only
// the rows of keys grouped by exact finding. A joined node keeps no set.
func keepOnlyIncidentSets(tx *sql.Tx, destinations map[string]checksandalerts.Destination) error {
	keep, args := []string{}, []any{}
	grouped, groupedArgs := []string{}, []any{}
	for _, name := range checksandalerts.Names(destinations) {
		set, keeps := IncidentSetOf(name, destinations[name])
		keep = append(keep, "(?,?)")
		args = append(args, set.with()...)
		if !keeps {
			grouped = append(grouped, "(?,?)")
			groupedArgs = append(groupedArgs, set.with()...)
		}
	}
	for _, table := range []string{"fleet_incident_set", "fleet_incident_keys"} {
		statement := "DELETE FROM " + table
		if len(keep) > 0 {
			statement += " WHERE (destination,alert_source) NOT IN (VALUES " + strings.Join(keep, ",") + ")"
		}
		if _, e := tx.Exec(statement, args...); e != nil {
			return e
		}
	}
	if len(grouped) == 0 {
		return nil
	}
	in := " (destination,alert_source) IN (VALUES " + strings.Join(grouped, ",") + ")"
	if _, e := tx.Exec("DELETE FROM fleet_incident_set WHERE grouped_by_exact_finding=0 AND"+in, groupedArgs...); e != nil {
		return e
	}
	_, e := tx.Exec(`DELETE FROM fleet_incident_keys WHERE`+in+` AND NOT EXISTS (SELECT 1 FROM fleet_incident_set s
 WHERE s.destination=fleet_incident_keys.destination AND s.alert_source=fleet_incident_keys.alert_source AND s.incident_key=fleet_incident_keys.incident_key)`, groupedArgs...)
	return e
}

// ReportedIncidents is what one node's latest report says for the hub's
// fleet incident sets: the announced incidents that it shows, and those
// that the node sent to a destination with a set without the hub, by
// destination and then by incident ID, each described as the alert that
// announced it, and when the node sent the report. A forgotten node has
// none.
type ReportedIncidents struct {
	Sent              time.Time
	Incidents         map[string]checksandalerts.Event
	SentWithoutTheHub map[string]map[string]checksandalerts.Event
	Forgotten         bool
	// ReminderEligible names the announced incidents that are eligible for
	// the hub's reminders of a grouped key (incidents.Track.ReminderEligible).
	ReminderEligible map[string]bool
}

// IncidentSentWithoutTheHub is an incident whose failure or raise a joined
// node sent without the hub to a destination that keeps a fleet incident
// set. The node's reports carry it until a reply says that the hub holds
// it, so that the hub can send the closing alert of its key.
type IncidentSentWithoutTheHub struct {
	Destination string `json:"destination"`
	Incident    string `json:"incident_id"`
	CheckGroup  string `json:"check_group"`
	Check       string `json:"check"`
	// Finding is the finding, such as the failed unit, for an incident of
	// one finding of a check; the finding is part of its key.
	Finding string `json:"finding,omitempty"`
	Type    string `json:"type"`
	// AlertSeveritiesSentForIncident are the severities that its alerts
	// went to the destination with, lowest first.
	AlertSeveritiesSentForIncident []string `json:"alert_severities_sent_for_incident"`
	// ExactFinding is the latest exact finding that the node sent, with
	// its sequence number. A joined node keeps the record of a finding with
	// an exact finding for every destination, also a chat or a mailbox: the
	// hub sends the closing alert.
	ExactFinding         string `json:"exact_finding,omitempty"`
	ExactFindingSequence int64  `json:"exact_finding_sequence,omitempty"`
	// AlertSource is the MMHotline alert source that the direct send used,
	// or empty for the destination's own.
	AlertSource string `json:"alert_source,omitempty"`
}

// IncidentKnownToTheHub is an incident that a report carried as sent
// without the hub and that the hub's fleet incident set of the destination
// now holds: the node stops carrying it.
type IncidentKnownToTheHub struct {
	Destination string `json:"destination"`
	Incident    string `json:"incident_id"`
}

// IncidentsKnownToTheHub lists the incidents among carried that the hub's
// fleet incident sets hold for reporter, so that the node can stop
// carrying them.
func (s *Store) IncidentsKnownToTheHub(reporter string, carried []IncidentSentWithoutTheHub) ([]IncidentKnownToTheHub, error) {
	// Read this reporter's confirmations once. Searching the incident set
	// for each carried record makes a full report batch take quadratic work.
	rows, err := s.DB.Query("SELECT destination, incident_id FROM fleet_incident_set WHERE reporter=?", reporter)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	held := map[IncidentKnownToTheHub]bool{}
	for rows.Next() {
		var incident IncidentKnownToTheHub
		if err := rows.Scan(&incident.Destination, &incident.Incident); err != nil {
			return nil, err
		}
		held[incident] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The hub settles the record of a finding with an exact finding by
	// itself, whether or not the incident is in the destination's set, one
	// version of the record at a time.
	settled := map[IncidentKnownToTheHub]string{}
	settledRows, err := s.DB.Query("SELECT destination, incident_id, settled_version FROM fleet_direct_sends_settled WHERE reporter=?", reporter)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = settledRows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	for settledRows.Next() {
		var incident IncidentKnownToTheHub
		var version string
		if err := settledRows.Scan(&incident.Destination, &incident.Incident, &version); err != nil {
			return nil, err
		}
		settled[incident] = version
	}
	if err := settledRows.Err(); err != nil {
		return nil, err
	}
	known := []IncidentKnownToTheHub{}
	for _, d := range carried {
		incident := IncidentKnownToTheHub{Destination: d.Destination, Incident: d.Incident}
		if d.ExactFinding != "" && settled[incident] == directSendVersion(d.AlertSeveritiesSentForIncident, d.AlertSource, d.ExactFindingSequence) || d.ExactFinding == "" && held[incident] {
			known = append(known, incident)
		}
	}
	return known, nil
}

// ReconcileIncidentSets applies nodes' latest reports to the fleet incident
// set of each destination: an announced incident that the set lacks joins
// it when its alerts go to that destination, and an incident that a newer
// report no longer shows closes, such as one whose recovery a joined
// node's fallback did not send. A destination whose service keeps no
// incidents takes only the incidents of findings with an exact finding. An
// incident that the node sent to the destination without the hub joins
// the set too, closed when it is over already. A forgotten node's
// incidents close. Each key this empties, with no failure waiting in fleet
// alert grouping, gets its one closing alert. An open incident that joins
// the set after the destination's closing alert for its key, although it
// opened before that alert, gets its failure sent again: the closing alert
// closed what its failure had opened in the service. Reports keep
// fleet_node_incidents too (reconcileNodeIncidents).
//
// For a finding with an exact finding, an incident that a report adds
// goes through the rules of grouping by exact finding: covered growth is
// marked for the key's next update, a reopened failure goes out as new,
// and a report never announces anything. The hub settles each direct
// send of such a finding: the node joins the next update, or, when a
// set-aside entry covers its incident and nothing else of the key is open
// for the destination, the destination gets closed_without_recovery.
//
// While awaitingReports, some node has not reported since the hub started,
// and may hold a key open without the hub knowing, so no key closes; once
// every node has reported, or three report intervals have passed, keys left
// with only closed incidents close.
func (s *Store) ReconcileIncidentSets(changes map[string]ReportedIncidents, destinations map[string]checksandalerts.Destination, awaitingReports bool, now time.Time) error {
	if len(destinations) == 0 {
		return nil
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	from := QueuedBy{Role: FleetHubSender, Node: s.Node, FleetIdentity: s.FleetIdentity, NewlyAffectedNodesEvery: s.newlyAffectedNodesEvery(), SetAsideFindings: s.setAsideFindings(), Now: now}
	at := now.UnixNano()
	setAside, coverage, e := reconcileNodeIncidents(tx, changes, s.Node, from.SetAsideFindings)
	if e != nil {
		return e
	}
	// A node whose exact finding moved onto or off a set-aside entry
	// settles as a change of the file does.
	if e = applyCoverageChanges(tx, at, coverage, destinations, from, now); e != nil {
		return e
	}
	waiting, e := waitingFailureKeys(tx, s.Node, nil)
	if e != nil {
		return e
	}
	for _, name := range checksandalerts.Names(destinations) {
		dest := destinations[name]
		set, keeps := IncidentSetOf(name, dest)
		r, e := set.reconcile(tx, name, destinations, changes, keeps, awaitingReports, waiting, setAside, from, now)
		if e != nil {
			return e
		}
		for _, failure := range r.reopened {
			switch {
			case checksandalerts.HasExactFinding(failure):
				e = set.groupedOpening(tx, at, name, dest, failure, from, now)
			case dest.MMHotline() != nil:
				key := checksandalerts.ReadableFleetIncidentKey(failure, checksandalerts.IncidentKind(failure.Type), s.Node)
				var open keyRows
				if open, e = set.keyRows(tx, key, FleetIncidentOpen); e == nil {
					if e = set.recordSent(tx, key, open.highest); e == nil {
						e = set.queueMMHotlineAlerts(tx, at, name, dest, key, []checksandalerts.Event{MMHotlineFailure(failure, []checksandalerts.Event{failure}, from, open.highest)}, sendsMore)
					}
				}
			default:
				failure.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(failure, checksandalerts.IncidentKind(failure.Type), s.Node)), s.Node, s.FleetIdentity)
				e = queueEvent(tx, at, name, dest, failure)
			}
			if e != nil {
				return e
			}
		}
		for _, closing := range r.setAside {
			key := checksandalerts.ReadableFleetIncidentKey(closing, "failed", s.Node)
			if e = set.queueSetAsideClosing(tx, at, name, dest, key, closing, from); e != nil {
				return e
			}
		}
		if e = set.queueClosings(tx, at, name, dest, r.closings, nil, checksandalerts.Event{}, from, now); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// reconcileNodeIncidents applies the reports to fleet_node_incidents: each
// incident that a report shows is recorded with its severity, its
// eligibility for the hub's reminders, and its exact finding, unless the
// hub holds a newer one (exact_finding_sequence), whichever path it came
// by. An incident that a newer report no longer shows, and each incident of
// a forgotten node, is removed. Whether a set-aside entry of setAside
// covers each reported incident is settled (settleCoverage). It returns
// the set-aside reason of each reported incident that an entry covers, by
// reporter and incident ID (reporter, a zero byte, the ID), and the
// changes of coverage.
func reconcileNodeIncidents(tx *sql.Tx, changes map[string]ReportedIncidents, hub string, setAside config.SetAsideFindings) (map[string]string, []coverageChange, error) {
	aside, coverage := map[string]string{}, []coverageChange{}
	for _, reporter := range checksandalerts.Names(changes) {
		r := changes[reporter]
		rows, e := tx.Query("SELECT incident_id, opened, alert_severity FROM fleet_node_incidents WHERE reporter=?", reporter)
		if e != nil {
			return nil, nil, e
		}
		type heldRow struct {
			opened   int64
			severity string
		}
		heldRows := map[string]heldRow{}
		for rows.Next() {
			var id string
			var x heldRow
			if e = rows.Scan(&id, &x.opened, &x.severity); e != nil {
				_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
				return nil, nil, e
			}
			heldRows[id] = x
		}
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
		if e = rows.Err(); e != nil {
			return nil, nil, e
		}
		for _, id := range checksandalerts.Names(heldRows) {
			if _, shown := r.Incidents[id]; shown || !r.Forgotten && heldRows[id].opened >= r.Sent.UnixNano() {
				continue
			}
			if _, e = tx.Exec("DELETE FROM fleet_node_incidents WHERE reporter=? AND incident_id=?", reporter, id); e != nil {
				return nil, nil, e
			}
		}
		for _, id := range checksandalerts.Names(r.Incidents) {
			m := r.Incidents[id]
			held, known := heldRows[id]
			if !known {
				opened := m.At
				if opened.IsZero() {
					opened = r.Sent
				}
				_, e = tx.Exec(`INSERT INTO fleet_node_incidents(reporter,incident_id,incident_key,node,check_group,check_name,finding,exact_finding,exact_finding_sequence,alert_severity,opened,reminder_eligible,send_alerts_only_to_destinations) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					reporter, id, checksandalerts.ReadableFleetIncidentKey(m, checksandalerts.IncidentKind(m.Type), hub), checksandalerts.AlertNode(m, hub), m.CheckGroup, m.Check, m.Finding, m.ExactFinding, m.ExactFindingSequence, m.AlertSeverity, opened.UnixNano(), r.ReminderEligible[id], destinationsColumn(m.SendAlertsOnlyToDestinations))
			} else {
				// An incident's severity only rises, and a report sent before a
				// raise can arrive after the raise's alert: it never lowers the
				// severity, which would let a set-aside entry cover an sos.
				_, e = tx.Exec(`UPDATE fleet_node_incidents SET alert_severity=?, reminder_eligible=?, exact_finding=CASE WHEN ?>=exact_finding_sequence THEN ? ELSE exact_finding END, exact_finding_sequence=MAX(exact_finding_sequence,?), send_alerts_only_to_destinations=? WHERE reporter=? AND incident_id=?`,
					config.HigherSeverity(held.severity, m.AlertSeverity), r.ReminderEligible[id], m.ExactFindingSequence, m.ExactFinding, m.ExactFindingSequence, destinationsColumn(m.SendAlertsOnlyToDestinations), reporter, id)
			}
			if e != nil {
				return nil, nil, e
			}
			change, reason, e := settleCoverageOf(tx, setAside, reporter, id)
			if e != nil {
				return nil, nil, e
			}
			if change != nil {
				coverage = append(coverage, *change)
			}
			if reason != "" {
				aside[reporter+"\x00"+id] = reason
			}
		}
	}
	return aside, coverage, nil
}

// reconciled is what applying the reports to one set sends: the failures
// to send again, the closed_without_recovery of direct sends whose
// incident a set-aside entry covers, and the keys that close.
type reconciled struct {
	reopened, setAside []checksandalerts.Event
	closings           []keyClosing
}

// setAsideDirectSend is the settled direct send of an incident that a
// set-aside entry covers, for its key's closed_without_recovery: the
// incident's alert as the node sent it, and the entry's reason.
type setAsideDirectSend struct {
	sent   checksandalerts.Event
	reason string
}

// reconcile applies the reports to one set, the set of the destination
// name, which keeps incidents (keeps) or holds only the incidents of
// findings with an exact finding. setAside are the set-aside reasons of
// reconcileNodeIncidents.
func (set IncidentSet) reconcile(tx *sql.Tx, name string, destinations map[string]checksandalerts.Destination, changes map[string]ReportedIncidents, keeps, awaitingReports bool, waiting map[string]bool, setAside map[string]string, from QueuedBy, now time.Time) (reconciled, error) {
	var out reconciled
	hub := from.hub()
	changed := map[string]string{}
	type closedRow struct{ key, reporter, id, severity string }
	closedGrouped := []closedRow{}
	// The keys of settled direct sends of set-aside incidents, whose closing
	// is decided once every report is applied.
	setAsideSends := map[string][]setAsideDirectSend{}
	for _, reporter := range checksandalerts.Names(changes) {
		r := changes[reporter]
		carried := r.SentWithoutTheHub[name]
		type row struct {
			id, key, severity string
			state             int
			opened            int64
			grouped           bool
		}
		rows, e := tx.Query("SELECT incident_id, incident_key, state, opened, alert_severity, grouped_by_exact_finding FROM fleet_incident_set WHERE "+inSet+" AND reporter=?", set.with(reporter)...)
		if e != nil {
			return out, e
		}
		held := map[string]row{}
		for rows.Next() {
			var x row
			if e = rows.Scan(&x.id, &x.key, &x.state, &x.opened, &x.severity, &x.grouped); e != nil {
				_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
				return out, e
			}
			held[x.id] = x
		}
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
		if e = rows.Err(); e != nil {
			return out, e
		}
		for _, id := range checksandalerts.Names(held) {
			x := held[id]
			// A resolved row stays while the node still carries the incident,
			// so it is not added again as a new one.
			shownIncident, shown := r.Incidents[id]
			carriedIncident, isCarried := carried[id]
			// A raise that the hub did not deliver, such as one that the node
			// sent without the hub, still decides the key's closing alert, as
			// far as the destination takes its severity.
			reported := carriedIncident.AlertSeveritiesSentForIncident
			if shown {
				reported = append(slices.Clone(reported), severitiesSentTo(name, destinations, shownIncident)...)
			}
			if x.state != fleetIncidentResolved && len(reported) > 0 {
				if higher := config.HigherSeverity(x.severity, config.HighestSeverity(reported)); higher != x.severity {
					if _, e = tx.Exec("UPDATE fleet_incident_set SET alert_severity=? WHERE "+inSet+" AND reporter=? AND incident_id=?", append([]any{higher}, set.with(reporter, id)...)...); e != nil {
						return out, e
					}
				}
				if e = set.recordSent(tx, x.key, reported...); e != nil {
					return out, e
				}
			}
			if shown || (!r.Forgotten && (x.opened >= r.Sent.UnixNano() || (isCarried && x.state == fleetIncidentResolved))) {
				continue
			}
			switch x.state {
			case FleetIncidentOpen:
				_, e = tx.Exec("UPDATE fleet_incident_set SET state=?, changed=? WHERE "+inSet+" AND reporter=? AND incident_id=?", append([]any{fleetIncidentClosed, now.UnixNano()}, set.with(reporter, id)...)...)
				// A report cannot tell a recovery from a retirement; a forget
				// retires.
				if r.Forgotten {
					changed[x.key] = "closed_without_recovery"
				} else if changed[x.key] == "" {
					changed[x.key] = "recovered"
				}
				if x.grouped {
					closedGrouped = append(closedGrouped, closedRow{x.key, reporter, id, x.severity})
				}
			case fleetIncidentResolved:
				_, e = tx.Exec("DELETE FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(reporter, id)...)
			}
			if e != nil {
				return out, e
			}
		}
		add := func(id string, m checksandalerts.Event, sent []string, state int, grouped bool) (string, error) {
			held[id] = row{id: id, state: state}
			key := checksandalerts.ReadableFleetIncidentKey(m, checksandalerts.IncidentKind(m.Type), hub)
			_, e := tx.Exec(`INSERT INTO fleet_incident_set(destination,alert_source,reporter,incident_id,incident_key,check_group,check_name,finding,node,opened,alert_severity,state,changed,grouped_by_exact_finding) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				set.with(reporter, id, key, m.CheckGroup, m.Check, m.Finding, checksandalerts.AlertNode(m, hub), r.Sent.UnixNano(), config.HighestSeverity(sent), state, now.UnixNano(), grouped)...)
			if e != nil {
				return "", e
			}
			// The incident's alerts reached the destination, through the hub
			// before a restart or straight from the node.
			return key, set.recordSent(tx, key, sent...)
		}
		for _, id := range checksandalerts.Names(r.Incidents) {
			// Only an incident whose alerts go to this destination belongs
			// in its set, with the severities that the destination took. A
			// set-aside incident is never in a set.
			shown := r.Incidents[id]
			grouped := checksandalerts.HasExactFinding(shown)
			sent := severitiesSentTo(name, destinations, shown)
			if _, ok := held[id]; ok || len(sent) == 0 || !keeps && !grouped || setAside[reporter+"\x00"+id] != "" {
				continue
			}
			key, e := add(id, shown, sent, FleetIncidentOpen, grouped)
			if e != nil {
				return out, e
			}
			// The destination's closing alert for the key went out while this
			// incident was open, so it closed what the incident's failure had
			// opened in the service: the failure goes out again.
			closedAt, e := set.lastClosingAlert(tx, key)
			if e != nil {
				return out, e
			}
			if !shown.At.IsZero() && closedAt > shown.At.UnixNano() {
				failure := checksandalerts.Event{ID: incidents.ID(), Incident: id, FleetIncidentKey: checksandalerts.FleetIncidentKey(key), Node: reporter, CheckGroup: shown.CheckGroup, Check: shown.Check, Finding: shown.Finding,
					Type: shown.Type, AlertSeverity: config.HighestSeverity(sent), At: shown.At, Message: shown.Message, SendAlertsOnlyToDestinations: shown.SendAlertsOnlyToDestinations,
					ExactFinding: shown.ExactFinding, ExactFindingSequence: shown.ExactFindingSequence}
				failure.WriteAlertTexts([]checksandalerts.Event{failure}, hub)
				if grouped {
					failure.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(failure.FleetIncidentKey, from.Node, from.FleetIdentity)
				}
				out.reopened = append(out.reopened, failure)
				continue
			}
			// A report never announces: an incident that the announcement
			// covers is growth for the next update, and anything else waits
			// for its alert, or for the settling of its direct send.
			if grouped {
				a, e := set.announcement(tx, key)
				if e != nil {
					return out, e
				}
				if a.covers(config.HighestSeverity(sent), shown.ExactFinding) {
					if e = set.markPending(tx, key, reporter, id, pendingJoined, config.HighestSeverity(sent), from, now); e != nil {
						return out, e
					}
				}
			}
		}
		for _, id := range checksandalerts.Names(carried) {
			c := carried[id]
			if !checksandalerts.HasExactFinding(c) {
				if _, ok := held[id]; !ok && !r.Forgotten && keeps {
					key, e := add(id, c, c.AlertSeveritiesSentForIncident, fleetIncidentClosed, false)
					if e != nil {
						return out, e
					}
					if changed[key] == "" {
						changed[key] = "recovered"
					}
				}
				if x, ok := held[id]; ok && x.state != fleetIncidentResolved && !r.Forgotten {
					if e = set.recordMMHotlineSource(tx, destinations[name], checksandalerts.ReadableFleetIncidentKey(c, checksandalerts.IncidentKind(c.Type), hub), c.MMHotlineAlertSource); e != nil {
						return out, e
					}
				}
				continue
			}
			if e = set.settleDirectSend(tx, destinations[name], reporter, id, c, r, func() (string, error) {
				state := fleetIncidentClosed
				if _, shown := r.Incidents[id]; shown {
					state = FleetIncidentOpen
				}
				return add(id, c, c.AlertSeveritiesSentForIncident, state, true)
			}, setAside[reporter+"\x00"+id], awaitingReports, changed, setAsideSends, from, now); e != nil {
				return out, e
			}
		}
	}
	for _, x := range closedGrouped {
		if open, e := set.hasOpen(tx, x.key, ""); e != nil || !open {
			if e != nil {
				return out, e
			}
			continue
		}
		if e := set.markPending(tx, x.key, x.reporter, x.id, pendingRecovered, x.severity, from, now); e != nil {
			return out, e
		}
	}
	// A set-aside direct send closes its key for the destination only when
	// nothing of the key is open there once every report is applied: another
	// node of the same batch may hold it open.
	for _, key := range checksandalerts.Names(setAsideSends) {
		open, e := set.hasOpen(tx, key, "")
		if e != nil {
			return out, e
		}
		if open {
			continue
		}
		sends := setAsideSends[key]
		nodes, reasons, sent := []string{}, []string{}, []string{}
		for _, d := range sends {
			if !slices.Contains(nodes, d.sent.Node) {
				nodes = append(nodes, d.sent.Node)
			}
			if !slices.Contains(reasons, d.reason) {
				reasons = append(reasons, d.reason)
			}
			sent = append(sent, d.sent.AlertSeveritiesSentForIncident...)
		}
		slices.Sort(nodes)
		about := sends[0].sent
		about.AlertSeveritiesSentForIncident = lowestFirst(sent)
		out.setAside = append(out.setAside, setAsideClosingAlert(about, nodes, key, strings.Join(reasons, "; "), from, now))
	}
	// Until every node has reported since the hub started, another node
	// may still hold a key open without the hub knowing.
	if awaitingReports {
		return out, nil
	}
	// Close the keys whose closing waited for those reports.
	rows, e := tx.Query("SELECT DISTINCT incident_key FROM fleet_incident_set WHERE "+inSet+" AND state=?", set.with(fleetIncidentClosed)...)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var key string
		if e = rows.Scan(&key); e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return out, e
		}
		if changed[key] == "" {
			changed[key] = "recovered"
		}
	}
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e = rows.Err(); e != nil {
		return out, e
	}
	out.closings, e = set.closeKeys(tx, changed, waiting, destinations[name].MMHotline() != nil, now)
	return out, e
}

// directSendVersion names one version of a node's record of a direct
// send: the severities that the node sent of the incident, the MMHotline
// alert source it sent under, and the sequence of its exact finding. A
// later direct send of the same incident, such as a raise in another hub
// outage, makes a new version, which the hub settles again before it
// acknowledges the record.
func directSendVersion(severities []string, alertSource string, exactFindingSequence int64) string {
	return strings.Join(lowestFirst(severities), ",") + "|" + alertSource + "|" + strconv.FormatInt(exactFindingSequence, 10)
}

// settleDirectSend settles, once for each version, the record of a direct send that the
// reporter's node made to the set's destination without the hub, for a
// finding with an exact finding (c), whatever the set holds. A direct
// send is never the last word: an incident that is not set aside joins the
// set, added by add when the set lacks it, and an open one is marked as
// joined for the key's next update, so the agent sees the node. When a
// set-aside entry covers the incident (reason), the send is added to
// setAsideSends by its key: the destination gets closed_without_recovery
// when nothing else of the key is open for it once every report is
// applied. While awaitingReports, another node may hold the key open
// without the hub knowing, so such a send is not settled yet, and the node
// keeps carrying it. The record is acknowledged afterwards
// (IncidentsKnownToTheHub).
func (set IncidentSet) settleDirectSend(tx *sql.Tx, dest checksandalerts.Destination, reporter, id string, c checksandalerts.Event, r ReportedIncidents, add func() (string, error), reason string, awaitingReports bool, changed map[string]string, setAsideSends map[string][]setAsideDirectSend, from QueuedBy, now time.Time) error {
	version := directSendVersion(c.AlertSeveritiesSentForIncident, c.MMHotlineAlertSource, c.ExactFindingSequence)
	var settled bool
	if e := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM fleet_direct_sends_settled WHERE destination=? AND reporter=? AND incident_id=? AND settled_version=?)", set.Destination, reporter, id, version).Scan(&settled); e != nil || settled || r.Forgotten || reason != "" && awaitingReports {
		return e
	}
	key := checksandalerts.ReadableFleetIncidentKey(c, checksandalerts.IncidentKind(c.Type), from.hub())
	// The key's further alerts and its resolution go under the alert
	// source that the node sent under (queueMMHotlineAlerts).
	e := set.recordMMHotlineSource(tx, dest, key, c.MMHotlineAlertSource)
	switch {
	case e != nil:
	case reason != "":
		setAsideSends[key] = append(setAsideSends[key], setAsideDirectSend{c, reason})
	default:
		var state int
		e = tx.QueryRow("SELECT state FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(reporter, id)...).Scan(&state)
		if e == sql.ErrNoRows {
			state = fleetIncidentClosed
			if _, e = add(); e == nil {
				e = tx.QueryRow("SELECT state FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(reporter, id)...).Scan(&state)
			}
			if e == nil && state == fleetIncidentClosed && changed[key] == "" {
				changed[key] = "recovered"
			}
		}
		if e == nil && state == FleetIncidentOpen {
			e = set.markPending(tx, key, reporter, id, pendingJoined, c.AlertSeverity, from, now)
		}
	}
	if e != nil {
		return e
	}
	_, e = tx.Exec("INSERT INTO fleet_direct_sends_settled(destination,reporter,incident_id,settled,settled_version) VALUES(?,?,?,?,?) ON CONFLICT(destination,reporter,incident_id) DO UPDATE SET settled=excluded.settled, settled_version=excluded.settled_version", set.Destination, reporter, id, now.UnixNano(), version)
	return e
}

// SentWithoutTheHub lists the incidents that this node sent without the
// hub to a destination that keeps a fleet incident set, and that no
// report reply has named as known to the hub yet.
func (s *Store) SentWithoutTheHub() ([]IncidentSentWithoutTheHub, error) {
	rows, e := s.DB.Query("SELECT destination, incident_id, check_group, check_name, finding, kind, alert_severities_sent_for_incident, exact_finding, exact_finding_sequence, alert_source FROM incidents_sent_without_the_hub ORDER BY at")
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	out := []IncidentSentWithoutTheHub{}
	for rows.Next() {
		var d IncidentSentWithoutTheHub
		var sent string
		if e = rows.Scan(&d.Destination, &d.Incident, &d.CheckGroup, &d.Check, &d.Finding, &d.Type, &sent, &d.ExactFinding, &d.ExactFindingSequence, &d.AlertSource); e != nil {
			return nil, e
		}
		d.AlertSeveritiesSentForIncident = strings.Split(sent, ",")
		out = append(out, d)
	}
	return out, rows.Err()
}

// CarriedToHub forgets the incidents sent without the hub that the hub
// says its fleet incident sets hold.
func (s *Store) CarriedToHub(known []IncidentKnownToTheHub) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, k := range known {
		if _, e = tx.Exec("DELETE FROM incidents_sent_without_the_hub WHERE destination=? AND incident_id=?", k.Destination, k.Incident); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// IncidentSetReporters names the nodes whose reports keep incidents in the
// hub's fleet incident sets.
func (s *Store) IncidentSetReporters() ([]string, error) {
	rows, e := s.DB.Query("SELECT DISTINCT reporter FROM fleet_incident_set")
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	out := []string{}
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			return nil, e
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
