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
	"mmwatchdog/internal/primitives"
)

// Setting findings aside on the hub. An entry of set_aside_findings.yml
// covers each open node incident below sos whose check, finding, and exact
// finding equal the entry's (config.SetAsideFindings.Covering). A covered
// node incident stays in fleet_node_incidents with the entry's key and
// reason (set_aside_entry, set_aside_reason) and is in no destination's
// set, so no alert, reminder, update, or closing alert goes out for it.
//
// Whether an entry covers a node incident is settled each time the hub
// learns something of the incident (settleCoverage): an alert of it, a
// report that shows it, and at every start and reload against every node
// incident (SettleSetAsideFindings). Each change is applied to every
// destination's set together (applyCoverageChanges): a newly covered
// incident leaves the sets, and a destination whose set for the key
// becomes empty gets one closed_without_recovery; incidents no longer
// covered join the sets, and each destination gets one opening alert for
// each key. The rules are in docs/changing-mmwatchdog/design/fleet.md under
// "Set a finding aside".

// nodeIncident is a row of fleet_node_incidents, as setting aside needs it.
type nodeIncident struct {
	reporter, incident, key, node, checkGroup, check, finding, exactFinding, severity string
	sendAlertsOnlyToDestinations                                                      *[]string
	opened                                                                            int64
	// entry and reason are the key and the reason of the set-aside entry
	// that covers the incident, or "".
	entry, reason string
}

const nodeIncidentColumns = "reporter, incident_id, incident_key, node, check_group, check_name, finding, exact_finding, alert_severity, send_alerts_only_to_destinations, opened, set_aside_entry, set_aside_reason"

func scanNodeIncident(scan func(...any) error) (nodeIncident, error) {
	var n nodeIncident
	var destinations string
	e := scan(&n.reporter, &n.incident, &n.key, &n.node, &n.checkGroup, &n.check, &n.finding, &n.exactFinding, &n.severity, &destinations, &n.opened, &n.entry, &n.reason)
	if e == nil && destinations != "" {
		n.sendAlertsOnlyToDestinations = &[]string{}
		e = json.Unmarshal([]byte(destinations), n.sendAlertsOnlyToDestinations)
	}
	return n, e
}

// destinationsColumn is how fleet_node_incidents keeps the destinations
// that an incident's check names: "" when it names none, so its alerts go
// by severity.
func destinationsColumn(destinations *[]string) string {
	if destinations == nil {
		return ""
	}
	b, _ := json.Marshal(*destinations)
	return string(b)
}

// coverageChange is a node incident that a set-aside entry now covers, or
// no longer covers, with the reason of that entry.
type coverageChange struct {
	nodeIncident
	covered bool
	reason  string
}

// settleCoverage records which entry of setAside covers the node incident
// n now, and returns the change when one starts or stops covering it. A
// change from one entry to another is no change of coverage.
func settleCoverage(tx *sql.Tx, setAside config.SetAsideFindings, n nodeIncident) (*coverageChange, error) {
	key, reason := "", ""
	if entry, covered := setAside.Covering(n.checkGroup+"/"+n.check, n.finding, n.exactFinding, n.severity); covered {
		key, reason = entry.Key(), entry.Reason
	}
	if key == n.entry && reason == n.reason {
		return nil, nil
	}
	if _, e := tx.Exec("UPDATE fleet_node_incidents SET set_aside_entry=?, set_aside_reason=? WHERE reporter=? AND incident_id=?", key, reason, n.reporter, n.incident); e != nil {
		return nil, e
	}
	switch {
	case key != "" && n.entry == "":
		return &coverageChange{nodeIncident: n, covered: true, reason: reason}, nil
	case key == "" && n.entry != "":
		return &coverageChange{nodeIncident: n, covered: false, reason: n.reason}, nil
	}
	return nil, nil
}

// settleCoverageOf is settleCoverage for the node incident of reporter with
// the ID incident, as the hub holds it now. It also returns the reason
// that covers the incident now, or "".
func settleCoverageOf(tx *sql.Tx, setAside config.SetAsideFindings, reporter, incident string) (*coverageChange, string, error) {
	n, e := scanNodeIncident(tx.QueryRow("SELECT "+nodeIncidentColumns+" FROM fleet_node_incidents WHERE reporter=? AND incident_id=?", reporter, incident).Scan)
	if e != nil {
		return nil, "", e
	}
	change, e := settleCoverage(tx, setAside, n)
	if e != nil {
		return nil, "", e
	}
	entry, _ := setAside.Covering(n.checkGroup+"/"+n.check, n.finding, n.exactFinding, n.severity)
	return change, entry.Reason, nil
}

// SettleSetAsideFindings puts setAside, the entries of
// set_aside_findings.yml, in force on the hub, at a start and at each
// reload, and settles them against every open node incident that the hub
// knows, so that a file changed while the hub was stopped takes effect at
// its start.
func (s *Store) SettleSetAsideFindings(setAside config.SetAsideFindings, destinations map[string]checksandalerts.Destination, now time.Time) error {
	s.setAside.Store(&setAside)
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.Query("SELECT " + nodeIncidentColumns + " FROM fleet_node_incidents ORDER BY reporter, incident_id")
	if e != nil {
		return e
	}
	held := []nodeIncident{}
	for rows.Next() {
		n, e := scanNodeIncident(rows.Scan)
		if e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
		held = append(held, n)
	}
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e = rows.Err(); e != nil {
		return e
	}
	changes := []coverageChange{}
	for _, n := range held {
		change, e := settleCoverage(tx, setAside, n)
		if e != nil {
			return e
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}
	from := QueuedBy{Role: FleetHubSender, Node: s.Node, FleetIdentity: s.FleetIdentity, NewlyAffectedNodesEvery: s.newlyAffectedNodesEvery(), SetAsideFindings: setAside, Now: now}
	if e = applyCoverageChanges(tx, now.UnixNano(), changes, destinations, from, now); e != nil {
		return e
	}
	return tx.Commit()
}

// applyCoverageChanges applies changes of coverage to the set of every
// destination. The incidents no longer covered are applied first, all
// together: each joins the set of every destination that its alerts go to,
// and each destination gets one opening alert for each key, with the note
// "an earlier exact finding was set aside: REASON". Then each newly covered
// incident leaves every set, its row deleted rather than left resolved, and
// a destination whose set for the key becomes empty, and that got the key,
// gets one closed_without_recovery, "Set aside after investigation: REASON".
func applyCoverageChanges(tx *sql.Tx, at int64, changes []coverageChange, destinations map[string]checksandalerts.Destination, from QueuedBy, now time.Time) error {
	if len(changes) == 0 {
		return nil
	}
	for _, name := range checksandalerts.Names(destinations) {
		dest := destinations[name]
		set, _ := IncidentSetOf(name, dest)
		opening := map[string][]coverageChange{}
		for _, c := range changes {
			if c.covered || !config.GoesTo(name, dest, checksandalerts.Event{Type: "failed", AlertSeverity: c.severity, SendAlertsOnlyToDestinations: c.sendAlertsOnlyToDestinations}, destinations) {
				continue
			}
			if _, e := tx.Exec(`INSERT OR IGNORE INTO fleet_incident_set(destination,alert_source,reporter,incident_id,incident_key,check_group,check_name,finding,node,opened,alert_severity,state,changed,grouped_by_exact_finding) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
				set.with(c.reporter, c.incident, c.key, c.checkGroup, c.check, c.finding, c.node, c.opened, c.severity, FleetIncidentOpen, now.UnixNano())...); e != nil {
				return e
			}
			opening[c.key] = append(opening[c.key], c)
		}
		closing := map[string][]coverageChange{}
		for _, c := range changes {
			if !c.covered {
				continue
			}
			var state int
			e := tx.QueryRow("SELECT state FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(c.reporter, c.incident)...).Scan(&state)
			if e == sql.ErrNoRows {
				continue
			}
			if e == nil {
				_, e = tx.Exec("DELETE FROM fleet_incident_set WHERE "+inSet+" AND reporter=? AND incident_id=?", set.with(c.reporter, c.incident)...)
			}
			if e != nil {
				return e
			}
			if state == FleetIncidentOpen {
				closing[c.key] = append(closing[c.key], c)
			}
		}
		for _, key := range checksandalerts.Names(closing) {
			if len(opening[key]) > 0 {
				continue
			}
			if e := set.closeSetAside(tx, at, name, dest, key, closing[key], from, now); e != nil {
				return e
			}
		}
		for _, key := range checksandalerts.Names(opening) {
			if e := set.openNoLongerSetAside(tx, at, name, dest, key, opening[key], from, now); e != nil {
				return e
			}
		}
	}
	for _, c := range changes {
		if c.covered {
			continue
		}
		failure := checksandalerts.Event{Type: "failed", AlertSeverity: c.severity, SendAlertsOnlyToDestinations: c.sendAlertsOnlyToDestinations}
		if e := RememberIncidentDestinations(tx, at, failure, []checksandalerts.Event{{Incident: c.incident}}, destinations); e != nil {
			return e
		}
	}
	return nil
}

// closeSetAside closes the key for the destination name after the
// incidents covered, the last of its open incidents, left the set: the
// destination gets one closed_without_recovery when it got the key, and
// the key starts over, as after any closing alert.
func (set IncidentSet) closeSetAside(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, key string, covered []coverageChange, from QueuedBy, now time.Time) error {
	if open, e := set.hasOpen(tx, key, ""); e != nil || open {
		return e
	}
	sent, e := set.keySent(tx, key)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(`UPDATE fleet_incident_keys SET alert_severities_sent='', last_closing_alert_at=?, announced_alert_severity='', announced_exact_findings='[]', next_update_due=0, next_reminder_due=0 WHERE `+inSet+` AND incident_key=?`, append([]any{now.UnixNano()}, set.with(key)...)...); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE fleet_incident_set SET state=?, changed=? WHERE "+inSet+" AND incident_key=? AND state=?", append([]any{fleetIncidentResolved, now.UnixNano()}, set.with(key, fleetIncidentClosed)...)...); e != nil {
		return e
	}
	if len(sent) == 0 {
		return nil
	}
	first := covered[0]
	about := checksandalerts.Event{Node: first.reporter, CheckGroup: first.checkGroup, Check: first.check, Finding: first.finding, Incident: first.incident, ExactFinding: first.exactFinding, AlertSeveritiesSentForIncident: sent}
	nodes, reasons := []string{}, []string{}
	for _, c := range covered {
		if !slices.Contains(nodes, c.node) {
			nodes = append(nodes, c.node)
		}
		if !slices.Contains(reasons, c.reason) {
			reasons = append(reasons, c.reason)
		}
	}
	slices.Sort(nodes)
	return queueSetAsideClosing(tx, at, name, dest, setAsideClosingAlert(about, nodes, key, strings.Join(reasons, "; "), from, now), from)
}

// openNoLongerSetAside sends the destination name one opening alert for the
// key, for the incidents that a set-aside entry no longer covers, which
// joined its set, with the note "an earlier exact finding was set aside:
// REASON". The alert announces their exact findings, as a key's first
// alert does.
func (set IncidentSet) openNoLongerSetAside(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, key string, members []coverageChange, from QueuedBy, now time.Time) error {
	hub := from.hub()
	slices.SortFunc(members, func(a, b coverageChange) int { return strings.Compare(a.node, b.node) })
	events, reasons, highest := []checksandalerts.Event{}, []string{}, ""
	for _, c := range members {
		events = append(events, checksandalerts.Event{ID: incidents.ID(), Type: "failed", Incident: c.incident, Node: c.reporter, CheckGroup: c.checkGroup, Check: c.check, Finding: c.finding,
			ExactFinding: c.exactFinding, AlertSeverity: c.severity, SendAlertsOnlyToDestinations: c.sendAlertsOnlyToDestinations, At: time.Unix(0, c.opened).UTC()})
		if !slices.Contains(reasons, c.reason) {
			reasons = append(reasons, c.reason)
		}
		highest = config.HigherSeverity(highest, c.severity)
	}
	note := "an earlier exact finding was set aside: " + strings.Join(reasons, "; ")
	alert := events[0]
	if len(events) == 1 {
		alert.Message = alert.ExactFinding + "; " + note
	} else {
		alert = Combine(events, hub)
		alert.ExactFinding = events[0].ExactFinding
		alert.Message += "; " + note
	}
	alert.AlertSeverity = highest
	alert.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity)
	alert.WriteAlertTexts(events, hub)
	for _, c := range members {
		if e := set.announce(tx, key, c.severity, c.exactFinding, remindEvery(dest), now); e != nil {
			return e
		}
	}
	if e := set.recordSent(tx, key, highest); e != nil {
		return e
	}
	return queueToDestination(tx, at, name, dest, alert, from, key)
}

// setAsideClosingAlert is the closed_without_recovery that tells a
// destination that the key's incidents on nodes are set aside after an
// investigation: "Set aside after investigation: REASON". about is the
// first incident, with the severities that the destination got for it or
// for its key.
func setAsideClosingAlert(about checksandalerts.Event, nodes []string, key, reason string, from QueuedBy, now time.Time) checksandalerts.Event {
	e := checksandalerts.Event{ID: incidents.ID(), Type: "closed_without_recovery", Node: about.Node, CheckGroup: about.CheckGroup, Check: about.Check, Finding: about.Finding, Incident: about.Incident,
		AlertSeverity: config.HighestSeverity(about.AlertSeveritiesSentForIncident), AlertSeveritiesSentForIncident: about.AlertSeveritiesSentForIncident, At: now,
		FleetIncidentKey: checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity), ExactFinding: about.ExactFinding}
	e.Message = "Set aside after investigation: " + reason
	title := about.Node + ": " + e.AboutCheck() + " set aside after investigation"
	if len(nodes) > 1 {
		e.Node, e.Incident = from.Node, ""
		e.NodeCount = len(nodes)
		e.Nodes = nodes[:min(len(nodes), CombinedNames)]
		title = e.AboutCheck() + " set aside after investigation on " + countNodes(len(nodes))
	}
	e.Title, e.TitleWasShortened = checksandalerts.FitTitle([]checksandalerts.TitlePart{{Text: title}})
	e.WriteTitleWithAlertSeverity()
	lines := []string{e.Message}
	if len(nodes) > 1 {
		names := strings.Join(e.Nodes, ", ")
		if more := len(nodes) - len(e.Nodes); more > 0 {
			names += fmt.Sprintf(", and %d more", more)
		}
		lines = append(lines, "on "+countNodes(len(nodes))+": "+names)
	}
	e.Body = primitives.AsReadableLines(strings.Join(lines, "\n"))
	return e
}

// queueSetAsideClosing queues a set-aside closing alert for the destination
// name: for the mmhotline destination as MMHotline's resolution.
func queueSetAsideClosing(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, closing checksandalerts.Event, from QueuedBy) error {
	if dest.MMHotline() != nil {
		return queueAlerts(tx, at, name, dest, []checksandalerts.Event{MMHotlineEvent(closing, closing.ID, closing.FleetIncidentKey, "resolved-"+closing.AlertSeverity, closing.Title, []string{closing.Body, from.SentBy()})})
	}
	return queueEvent(tx, at, name, dest, closing)
}

func countNodes(n int) string {
	if n == 1 {
		return "1 node"
	}
	return fmt.Sprintf("%d nodes", n)
}
