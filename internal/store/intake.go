package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/incidents"
)

const intakeMemory = time.Hour // how long a delivered alert's ID is remembered

// CombinedNames is how many nodes one message names; it counts the rest.
const CombinedNames = 20

const intakeQueued = 0

const intakeHeld = 1 // an open maintenance group's alerts

const intakeDone = 2

var ErrIntakeFull = errors.New("the hub's alert queue is full")

// NodeShareOfTheIntake is the part of
// fleet_hub_role.undelivered_alerts_of_nodes_content_size_limit that the
// alerts of one node may hold. Reason: an outage of one node never needs
// more, and the rest keeps the hub open to the other nodes, so one node,
// also a hostile one, cannot make every node's alerts wait for the direct
// way. A node's alert beyond its share is refused like one beyond the
// limit, and the node delivers it without the hub.
const NodeShareOfTheIntake = 4

// IntakeSubject is what an alert in the alert grouping step is about: its
// check group and check, and for an alert about one failed unit of a
// every_systemd_unit check that unit, so the alerts of each unit are
// ordered and grouped by themselves. IntakeSubjectEvent reads it back.
func IntakeSubject(e checksandalerts.Event) string {
	subject := e.CheckGroup + "\x00" + e.Check
	if e.Finding != "" {
		subject += "\x00" + e.Finding
	}
	return subject
}

// IntakeSubjectEvent is an alert of the given kind from node about the
// intake subject, with the fields that say what the alert is about.
func IntakeSubjectEvent(node, subject, kind string) checksandalerts.Event {
	group, rest, _ := strings.Cut(subject, "\x00")
	check, unit, _ := strings.Cut(rest, "\x00")
	return checksandalerts.Event{Node: node, CheckGroup: group, Check: check, Finding: unit, Type: kind}
}

// queueIntake adds an alert to the alert grouping step, keyed for ordering by its
// node and check, for maintenance by its group: reason and user, and
// for a reminder by the destination it names and whether it is about a
// failure or a check that cannot run.
func queueIntake(tx *sql.Tx, node string, event checksandalerts.Event, b []byte, now time.Time) error {
	group := ""
	if w := event.Maintenance; w != nil {
		group = w.Reason + "\x00" + w.User
	}
	if checksandalerts.ReminderIsAboutCannotCheck(event) {
		group = checksandalerts.CannotCheckIncidentEvents.Failed
	}
	// The hub reminds of a key grouped by exact finding itself, once per
	// destination and key, so a node's reminder of it is dropped: one of
	// a 0.1.x node, which knows no exact finding, too.
	if drop, e := hubRemindsOf(tx, event); drop || e != nil {
		return e
	}
	// A node's new reminder replaces its undelivered one for the same
	// incident and destination; see replaceQueuedReminder.
	if event.Type == "reminder" {
		var replaced int64
		if e := tx.QueryRow("SELECT COALESCE(SUM(length(event)),0) FROM intake WHERE node=? AND type='reminder' AND destination=? AND state!=? AND json_extract(event,'$.incident_id')=?", node, event.Destination, intakeDone, event.Incident).Scan(&replaced); e != nil {
			return e
		}
		if _, e := tx.Exec("DELETE FROM intake WHERE node=? AND type='reminder' AND destination=? AND state!=? AND json_extract(event,'$.incident_id')=?", node, event.Destination, intakeDone, event.Incident); e != nil {
			return e
		}
		if e := addIntakeBytes(tx, node, -replaced); e != nil {
			return e
		}
	}
	r, e := tx.Exec("INSERT OR IGNORE INTO intake(node,id,received,type,subject,grp,destination,severity,event,exact_finding) VALUES(?,?,?,?,?,?,?,?,?,?)", node, event.ID, now.UnixNano(), event.Type, IntakeSubject(event), group, event.Destination, event.RoutingGroup(), b, checksandalerts.HasExactFinding(event))
	if e != nil {
		return e
	}
	if added, _ := r.RowsAffected(); added == 0 {
		return nil
	}
	return addIntakeBytes(tx, node, int64(len(b)))
}

// hubRemindsOf reports a node's reminder of a key that the hub reminds of
// itself: a reminder with an exact finding, or one of a failure key that
// some node has open with an exact finding on the hub.
func hubRemindsOf(tx *sql.Tx, event checksandalerts.Event) (bool, error) {
	if event.Type != "reminder" || event.Finding == "" || checksandalerts.ReminderIsAboutCannotCheck(event) {
		return false, nil
	}
	if checksandalerts.HasExactFinding(event) {
		return true, nil
	}
	var grouped bool
	e := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM fleet_node_incidents WHERE incident_key=? AND exact_finding!='')", checksandalerts.ReadableFleetIncidentKey(event, "failed", "")).Scan(&grouped)
	return grouped, e
}

// addIntakeBytes changes the running totals of the bytes of the alerts that
// wait in the intake, of all nodes and of the node, by delta. Acceptance
// reads them in place of adding up the intake.
func addIntakeBytes(tx *sql.Tx, node string, delta int64) error {
	if delta == 0 {
		return nil
	}
	if _, e := tx.Exec("INSERT INTO counters(name,value) VALUES('intake_queued_bytes',?) ON CONFLICT(name) DO UPDATE SET value=value+excluded.value", delta); e != nil {
		return e
	}
	if _, e := tx.Exec("INSERT INTO intake_queued_bytes(node,bytes) VALUES(?,?) ON CONFLICT(node) DO UPDATE SET bytes=bytes+excluded.bytes", node, delta); e != nil {
		return e
	}
	_, e := tx.Exec("DELETE FROM intake_queued_bytes WHERE node=? AND bytes<=0", node)
	return e
}

// intakeBytes are the bytes of the alerts that wait in the intake: of all
// nodes, and of node.
func intakeBytes(tx *sql.Tx, node string) (all, ofNode int64, e error) {
	e = tx.QueryRow("SELECT COALESCE((SELECT value FROM counters WHERE name='intake_queued_bytes'),0), COALESCE((SELECT bytes FROM intake_queued_bytes WHERE node=?),0)", node).Scan(&all, &ofNode)
	return all, ofNode, e
}

// IntakeRow is an alert in the alert grouping step, without its event. silence marks
// the hub's own alert about a silent node; destination is the one a
// reminder names; severity is its Event.severityGroup.
type IntakeRow struct {
	Seq                                               int64
	Node, Kind, Subject, Group, Destination, severity string
	Held, silence                                     bool
	// ExactFinding marks an alert of a finding with an exact finding: it
	// goes out at once and alone, and grouping by exact finding decides
	// what each destination gets (findinggrouping.go).
	ExactFinding bool
}

// AlertGroupKey is the check group, check, change, and severity, and for a
// reminder the destination it names and its incident kind. A silence
// alert's check names its node, so its key leaves the check out.
func (r IntakeRow) AlertGroupKey() string {
	subject := r.Subject
	if r.silence {
		subject = checksandalerts.NodeReportsCheckGroup + "\x00"
	}
	key := subject + "\x00" + r.Kind + "\x00" + r.Destination + "\x00" + r.severity
	if r.Kind == "reminder" {
		key += "\x00" + r.Group
	}
	return key
}

// AlertGroupMessage is one message the alert grouping step sends: its members' alert
// when there is one member, otherwise one combined message. Held members stay
// in their open maintenance group.
type AlertGroupMessage struct {
	Members []int64
	Hold    bool
}

type AlertGroupingPlan struct {
	Messages   []AlertGroupMessage
	Hold, Done []int64 // rows held or finished without a message of their own
	Sent       []string
	Closed     []string
}

// Combine returns the one alert, or a combined message naming up to
// CombinedNames nodes and counting the rest. It happened when its earliest
// member did, and a maintenance end when its latest member did: the group's
// maintenance was over then. hub is the hub's own name.
func Combine(events []checksandalerts.Event, hub string) checksandalerts.Event {
	if len(events) == 1 {
		return events[0]
	}
	first := events[0]
	// Members share their severity: see AlertGroupKey.
	m := checksandalerts.Event{ID: incidents.ID(), FleetIncidentKey: first.FleetIncidentKey, Node: first.Node, CheckGroup: first.CheckGroup, CheckGroups: first.CheckGroups, Check: first.Check, Finding: first.Finding, Type: first.Type, AlertSeverity: first.AlertSeverity, AlertSeveritiesSentForIncident: first.AlertSeveritiesSentForIncident, SendAlertsOnlyToDestinations: first.SendAlertsOnlyToDestinations, At: first.At, Maintenance: first.Maintenance, Destination: first.Destination}
	seen := map[string]bool{}
	nodes := []string{}
	for _, e := range events {
		if e.At.Before(m.At) {
			m.At = e.At
		}
		// Grouped alerts of one check share their key; retirements of a
		// failure and of a cannot-check incident of the check do not.
		if e.FleetIncidentKey != m.FleetIncidentKey {
			m.FleetIncidentKey = ""
		}
		if e.Node != m.Node {
			m.Node = ""
		}
		if e.CheckGroup != m.CheckGroup {
			m.CheckGroup = ""
		}
		if e.Check != m.Check {
			m.Check = ""
		}
		if e.Finding != m.Finding {
			m.Finding = ""
		}
		if name := checksandalerts.AlertNode(e, hub); !seen[name] {
			seen[name] = true
			nodes = append(nodes, name)
		}
	}
	m.NodeCount = len(nodes)
	m.Nodes = nodes[:min(len(nodes), CombinedNames)]
	names := strings.Join(m.Nodes, ", ")
	if more := len(nodes) - len(m.Nodes); more > 0 {
		names += fmt.Sprintf(", and %d more", more)
	}
	if w := m.Maintenance; w != nil {
		change := "maintenance started"
		if m.Type == "maintenance_off" {
			causes := map[string]bool{}
			for _, event := range events {
				if event.At.After(m.At) {
					m.At = event.At
				}
				// Match MMWatchdog's own message start, never the operator's reason.
				cause := "turned off"
				if strings.HasPrefix(event.Message, incidents.MaintenanceRemovedMessage) {
					cause = "ended because the check group was removed"
				} else if strings.HasPrefix(event.Message, incidents.MaintenanceExpiredMessage) {
					cause = "expired"
				}
				causes[cause] = true
			}
			change = "maintenance " + strings.Join(checksandalerts.Names(causes), " or ")
		}
		m.Message = fmt.Sprintf("%s on %d nodes: %s", change, len(nodes), names)
		by := []string{}
		if w.User != "" {
			by = append(by, "by "+w.User)
		}
		if w.Reason != "" {
			by = append(by, w.Reason)
		}
		if len(by) > 0 {
			m.Message += " (" + strings.Join(by, ": ") + ")"
		}
		return m
	}
	m.Message = groupedChange(m, first, len(nodes), hub) + ": " + names
	return m
}

// groupedChange says what changed on count nodes, for the first line of a
// grouped message, in the words of the alert titles (AlertTitleParts).
// Each kind of alert that fleet alert grouping combines has its words
// here. first is the first member, whose message tells a reminder of a
// cannot-check incident.
func groupedChange(m, first checksandalerts.Event, count int, hub string) string {
	nodes := fmt.Sprintf("%d nodes", count)
	now := checksandalerts.AlertSeverityLabel(m.AlertSeverity)
	raised := m.Type == checksandalerts.FailureIncidentEvents.Raised
	if m.Type == "rebooted" {
		return nodes + " rebooted"
	}
	// The hub's alerts about silent nodes.
	if m.CheckGroup == checksandalerts.NodeReportsCheckGroup && m.Node == hub {
		switch {
		case m.Type == "failed":
			return nodes + " stopped reporting"
		case m.Type == "recovered":
			return nodes + " are reporting again"
		case m.Type == "reminder":
			return nodes + " still not reporting"
		case raised:
			return nodes + " that stopped reporting are now " + now
		case m.Type == "closed_without_recovery":
			return nodes + " stopped reporting; alerts closed, maybe not fixed"
		}
	}
	// The alerts of hub-outage alerter nodes that could not reach the hub.
	if m.CheckGroup == checksandalerts.NodeReportsCheckGroup && m.Check == checksandalerts.HubCheck {
		switch {
		case m.Type == "failed":
			return nodes + " cannot reach the MMWatchdog fleet hub"
		case m.Type == "recovered":
			return nodes + " reach the MMWatchdog fleet hub again"
		case m.Type == "reminder":
			return nodes + " still cannot reach the MMWatchdog fleet hub"
		case raised:
			return nodes + " that cannot reach the MMWatchdog fleet hub are now " + now
		case m.Type == "closed_without_recovery":
			return nodes + " no longer watch the MMWatchdog fleet hub; their alerts about the hub are closed"
		}
	}
	about := m.AboutCheck()
	switch {
	case m.Type == "failed":
		return about + " failed on " + nodes
	case raised:
		return about + " is now " + now + " on " + nodes
	case m.Type == "cannot_check":
		return "MMWatchdog cannot check " + about + " on " + nodes
	case m.Type == checksandalerts.CannotCheckIncidentEvents.Raised:
		return "MMWatchdog still cannot check " + about + ", now " + now + " on " + nodes
	case m.Type == "recovered":
		return about + " is ok again on " + nodes
	case m.Type == "can_check_again":
		return "MMWatchdog can check " + about + " again on " + nodes
	case m.Type == "closed_without_recovery":
		return about + " incidents closed on " + nodes + ", maybe not fixed"
	case m.Type == "reminder" && checksandalerts.ReminderIsAboutCannotCheck(first):
		// It starts as each node's reminder does, so its kind can be told
		// again from it (ReminderIsAboutCannotCheck).
		return checksandalerts.CannotCheckReminderStart(about) + "on " + nodes
	case m.Type == "reminder":
		return about + " still failing on " + nodes
	}
	return about + " on " + nodes
}

// Accept saves a node's alert for the alert grouping step, once per node and
// event ID, within the intake limits.
func (s *Store) Accept(node string, event checksandalerts.Event, now time.Time) error {
	b, e := json.Marshal(event)
	if e != nil {
		return e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// The alerts that wait in the alert grouping step stay within
	// fleet_hub_role.undelivered_alerts_of_nodes_content_size_limit. The
	// running totals make each acceptance cost the same, however many
	// alerts wait.
	size, nodeSize, e := intakeBytes(tx, node)
	if e != nil {
		return e
	}
	limit := s.Limits.Load().UndeliveredAlertsOfNodesBytes
	if size+int64(len(b)) > limit {
		return ErrIntakeFull
	}
	if nodeSize+int64(len(b)) > limit/NodeShareOfTheIntake {
		return fmt.Errorf("%w for the alerts of %s: one node may hold a quarter of it", ErrIntakeFull, node)
	}
	if e = queueIntake(tx, node, event, b, now); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) Intake() ([]IntakeRow, error) {
	rows, e := s.DB.Query("SELECT seq, node, type, subject, grp, destination, severity, state, exact_finding FROM intake WHERE state!=? ORDER BY seq", intakeDone)
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	out := []IntakeRow{}
	for rows.Next() {
		var r IntakeRow
		var state int
		if e = rows.Scan(&r.Seq, &r.Node, &r.Kind, &r.Subject, &r.Group, &r.Destination, &r.severity, &state, &r.ExactFinding); e != nil {
			return nil, e
		}
		r.Held = state == intakeHeld
		r.silence = r.Node == s.Node && strings.HasPrefix(r.Subject, checksandalerts.NodeReportsCheckGroup+"\x00")
		out = append(out, r)
	}
	return out, rows.Err()
}

// Emit commits a alert grouping plan in one transaction: each message queued for every
// destination, or a reminder for the one it names, in order, and each alert's
// new state. A delivered alert keeps only its ID. awaitingFirstReports is
// QueuedBy.AwaitingFirstReports.
func (s *Store) Emit(p AlertGroupingPlan, destinations map[string]checksandalerts.Destination, awaitingFirstReports bool, now time.Time) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	setState := func(seq int64, state int) error {
		if state != intakeDone {
			_, e := tx.Exec("UPDATE intake SET state=? WHERE seq=?", state, seq)
			return e
		}
		var node string
		var size int64
		var was int
		if e := tx.QueryRow("SELECT node, length(event), state FROM intake WHERE seq=?", seq).Scan(&node, &size, &was); e != nil || was == intakeDone {
			return e
		}
		if _, e := tx.Exec("UPDATE intake SET state=?, event=x'' WHERE seq=?", state, seq); e != nil {
			return e
		}
		return addIntakeBytes(tx, node, -size)
	}
	for _, m := range p.Messages {
		events := []checksandalerts.Event{}
		for _, seq := range m.Members {
			var b []byte
			var event checksandalerts.Event
			if e = tx.QueryRow("SELECT event FROM intake WHERE seq=?", seq).Scan(&b); e == nil {
				e = json.Unmarshal(b, &event)
			}
			if e != nil {
				return e
			}
			events = append(events, event)
		}
		message := Combine(events, s.Node)
		// One alert kept the title and body its node wrote; a grouped
		// message gets its own.
		if len(events) > 1 || message.Title == "" {
			message.WriteAlertTexts(events, s.Node)
		}
		if e = QueueDirect(tx, now.UnixNano(), message, events, destinations, QueuedBy{Role: FleetHubSender, Node: s.Node, FleetIdentity: s.FleetIdentity, AwaitingFirstReports: awaitingFirstReports, NewlyAffectedNodesEvery: s.newlyAffectedNodesEvery(), SetAsideFindings: s.setAsideFindings(), Now: now}); e != nil {
			return e
		}
		// The message's alerts take their new state before the next message
		// is queued: a failure sent in this step no longer waits in the
		// intake, so its recovery later in the step closes its key
		// (waitingFailureKeys).
		state := intakeDone
		if m.Hold {
			state = intakeHeld
		}
		for _, seq := range m.Members {
			if e = setState(seq, state); e != nil {
				return e
			}
		}
	}
	for _, seq := range p.Hold {
		if e = setState(seq, intakeHeld); e != nil {
			return e
		}
	}
	for _, seq := range p.Done {
		if e = setState(seq, intakeDone); e != nil {
			return e
		}
	}
	if len(p.Messages) > 0 {
		if e = s.PruneInTransaction(tx, s.retentionNow()); e != nil {
			return e
		}
	}
	return tx.Commit()
}
