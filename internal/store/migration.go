package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// migrateStateDatabase moves state.db at path from the previous format to
// stateFormat in place, on every role: a hub, a joined node, and a
// standalone node. A reset would lose the open incidents. It changes
// nothing when the file does not exist or has another format; opening the
// database then creates it or refuses it (OpenDatabaseFile).
func migrateStateDatabase(path string) error {
	if _, e := os.Stat(path); e != nil {
		return nil
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 20 {
		return e
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`ALTER TABLE outbox ADD COLUMN urgent INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE outbox ADD COLUMN incident_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE outbox ADD COLUMN closes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE outbox ADD COLUMN announced_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE outbox ADD COLUMN sent_without_the_hub TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE intake ADD COLUMN exact_finding INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE fleet_incident_set ADD COLUMN pending_change TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE fleet_incident_set ADD COLUMN grouped_by_exact_finding INTEGER NOT NULL DEFAULT 0`,
		`DROP INDEX IF EXISTS fleet_incident_set_key`,
		`ALTER TABLE incidents_sent_without_the_hub ADD COLUMN exact_finding TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE incidents_sent_without_the_hub ADD COLUMN exact_finding_sequence INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE incidents_sent_without_the_hub ADD COLUMN alert_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE fleet_incident_keys ADD COLUMN announced_alert_severity TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE fleet_incident_keys ADD COLUMN announced_exact_findings TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE fleet_incident_keys ADD COLUMN next_update_due INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE fleet_incident_keys ADD COLUMN next_reminder_due INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE fleet_incident_keys ADD COLUMN mmhotline_alert_sources_sent TEXT NOT NULL DEFAULT ''`,
		// Before this format every MMHotline alert went under the
		// destination's own alert source, so a key that got alerts since its
		// last resolution has its incident open there: its resolution must go
		// there too, also after a node opens the key on the node source.
		`UPDATE fleet_incident_keys SET mmhotline_alert_sources_sent=alert_source WHERE alert_source!='' AND alert_severities_sent!=''`,
		// What the destinations got for a key that is open stays announced,
		// at the highest severity sent since the key's last closing alert. No
		// exact finding was announced before this format.
		`UPDATE fleet_incident_keys SET announced_alert_severity = CASE
		  WHEN instr(alert_severities_sent,'sos')>0 THEN 'sos' WHEN instr(alert_severities_sent,'warn')>0 THEN 'warn' WHEN instr(alert_severities_sent,'info')>0 THEN 'info' ELSE '' END
		 WHERE EXISTS (SELECT 1 FROM fleet_incident_set s WHERE s.destination=fleet_incident_keys.destination AND s.alert_source=fleet_incident_keys.alert_source AND s.incident_key=fleet_incident_keys.incident_key AND s.state=` + strconv.Itoa(FleetIncidentOpen) + `)`,
		stateTables,
		// The running totals of the alerts that wait in the intake.
		`INSERT INTO counters(name,value) SELECT 'intake_queued_bytes', COALESCE(SUM(length(event)),0) FROM intake WHERE state!=` + strconv.Itoa(intakeDone),
		`INSERT INTO intake_queued_bytes(node,bytes) SELECT node, SUM(length(event)) FROM intake WHERE state!=` + strconv.Itoa(intakeDone) + ` GROUP BY node`,
		`UPDATE intake SET exact_finding=1 WHERE state!=` + strconv.Itoa(intakeDone) + ` AND COALESCE(json_extract(event,'$.finding'),'')!='' AND COALESCE(json_extract(event,'$.exact_finding'),'')!=''`,
	} {
		if _, e = tx.Exec(statement); e != nil {
			return e
		}
	}
	if e = orderQueuedDeliveries(tx); e != nil {
		return e
	}
	if e = recordQueuedDirectSendsOnAcceptance(tx); e != nil {
		return e
	}
	if _, e = tx.Exec("PRAGMA user_version=21"); e != nil {
		return e
	}
	return tx.Commit()
}

// recordQueuedDirectSendsOnAcceptance moves a joined node's records of
// direct sends that still wait in the outbox to their acceptance. Format 20
// recorded a direct failure or raise to a destination that keeps
// incidents, or to an mmhotline destination, when it was queued; this
// format records it once the destination accepts it (recordOnAcceptance).
// A queued direct send gets the record of its acceptance, and the
// incident's format-20 record keeps only the severities that left the
// outbox, so that the hub settles a send only once the destination has it.
// A severity goes out once for each incident, so a severity that waits in
// the outbox was not accepted yet. A record left with no severity is
// removed: the delivery's acceptance writes it again.
//
// Format 20 queued a direct send to a destination that keeps incidents as
// the node's own event, with its incident. An MMHotline direct send it
// queued as MMHotline's alert, without the incident: the alert's ID is the
// ID of the node's event, which names the incident while the event
// history holds it. Otherwise the incident is the one format-20 record of
// the destination, check, kind, and severity, or of those that the alert's
// title names by its finding. When none or several remain, the record
// stays whole, and the hub settles the send before it lands, as in format
// 20.
func recordQueuedDirectSendsOnAcceptance(tx *sql.Tx) error {
	type record struct {
		incident, checkGroup, check, finding, kind string
		sent                                       []string
	}
	recorded := map[string][]record{}
	rows, e := tx.Query("SELECT destination, incident_id, check_group, check_name, finding, kind, alert_severities_sent_for_incident FROM incidents_sent_without_the_hub")
	if e != nil {
		return e
	}
	for rows.Next() {
		var destination, sent string
		var r record
		if e = rows.Scan(&destination, &r.incident, &r.checkGroup, &r.check, &r.finding, &r.kind, &sent); e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
		r.sent = strings.Split(sent, ",")
		recorded[destination] = append(recorded[destination], r)
	}
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e = rows.Err(); e != nil {
		return e
	}
	type queued struct {
		seq         int64
		destination string
		event       checksandalerts.Event
		alertSource string
	}
	all, eventIDs := []queued{}, []any{}
	if rows, e = tx.Query("SELECT seq, destination, event, target FROM outbox WHERE destination!=? ORDER BY seq", config.HubDestination); e != nil {
		return e
	}
	for rows.Next() {
		var q queued
		var b, w []byte
		var target storedDestination
		if e = rows.Scan(&q.seq, &q.destination, &b, &w); e == nil {
			e = json.Unmarshal(b, &q.event)
		}
		if e == nil && q.event.MMHotlineAlert != nil {
			e = json.Unmarshal(w, &target)
		}
		if e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
		a := q.event.MMHotlineAlert
		switch {
		case !opensOrChanges(q.event.Type):
		case a == nil:
			all = append(all, q)
		// Only a node's direct send ends with these words; a hub's or a
		// standalone node's MMHotline alert is no direct send.
		case strings.HasSuffix(a.Message, "\n"+(QueuedBy{Role: FallbackSender, Node: q.event.Node}).SentBy()) && target.destination().MMHotline() != nil:
			q.alertSource = target.destination().MMHotline().AlertSourceID
			all = append(all, q)
			eventIDs = append(eventIDs, q.event.ID)
		}
	}
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e = rows.Err(); e != nil {
		return e
	}
	// The node's events of the MMHotline direct sends, by ID.
	events := map[string]checksandalerts.Event{}
	if len(eventIDs) > 0 {
		if rows, e = tx.Query("SELECT data FROM events WHERE json_extract(CAST(data AS TEXT),'$.id') IN (?"+strings.Repeat(",?", len(eventIDs)-1)+")", eventIDs...); e != nil {
			return e
		}
		for rows.Next() {
			var b []byte
			var event checksandalerts.Event
			if e = rows.Scan(&b); e == nil {
				e = json.Unmarshal(b, &event)
			}
			if e != nil {
				_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
				return e
			}
			events[event.ID] = event
		}
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
		if e = rows.Err(); e != nil {
			return e
		}
	}
	// incidentOf is the incident of a queued direct send, or nil when it is
	// not known.
	incidentOf := func(q queued) *checksandalerts.Event {
		a := q.event.MMHotlineAlert
		if a == nil {
			for _, r := range recorded[q.destination] {
				if r.incident == q.event.Incident {
					return &q.event
				}
			}
			return nil
		}
		if event, ok := events[q.event.ID]; ok && event.Incident != "" {
			event.AlertSeverity = a.Severity
			return &event
		}
		var matches []record
		for _, r := range recorded[q.destination] {
			if r.checkGroup == q.event.CheckGroup && r.check == q.event.Check && r.kind == checksandalerts.IncidentKind(q.event.Type) && slices.Contains(r.sent, a.Severity) {
				matches = append(matches, r)
			}
		}
		if len(matches) > 1 {
			matches = slices.DeleteFunc(matches, func(r record) bool { return r.finding == "" || !strings.Contains(a.Title, r.finding) })
		}
		if len(matches) != 1 {
			return nil
		}
		r := matches[0]
		return &checksandalerts.Event{Incident: r.incident, CheckGroup: r.checkGroup, Check: r.check, Finding: r.finding, Type: q.event.Type, AlertSeverity: a.Severity}
	}
	accepted := map[[2]string][]string{}
	for _, q := range all {
		incident := incidentOf(q)
		if incident == nil {
			continue
		}
		b, e := acceptanceRecord([]checksandalerts.Event{*incident}, q.alertSource)
		if e == nil {
			_, e = tx.Exec("UPDATE outbox SET sent_without_the_hub=? WHERE seq=?", b, q.seq)
		}
		if e != nil {
			return e
		}
		for _, r := range recorded[q.destination] {
			if r.incident != incident.Incident {
				continue
			}
			id := [2]string{q.destination, r.incident}
			if _, seen := accepted[id]; !seen {
				accepted[id] = r.sent
			}
			accepted[id] = slices.DeleteFunc(accepted[id], func(s string) bool { return s == incident.AlertSeverity })
		}
	}
	for id, severities := range accepted {
		if len(severities) == 0 {
			_, e = tx.Exec("DELETE FROM incidents_sent_without_the_hub WHERE destination=? AND incident_id=?", id[0], id[1])
		} else {
			_, e = tx.Exec("UPDATE incidents_sent_without_the_hub SET alert_severities_sent_for_incident=? WHERE destination=? AND incident_id=?", strings.Join(severities, ","), id[0], id[1])
		}
		if e != nil {
			return e
		}
	}
	return nil
}

// orderQueuedDeliveries gives each delivery in the outbox the columns that
// its place in the queue follows (outboxOrderOf).
func orderQueuedDeliveries(tx *sql.Tx) error {
	rows, e := tx.Query("SELECT seq, event FROM outbox")
	if e != nil {
		return e
	}
	type ordered struct {
		seq          int64
		urgent, ends bool
		key          string
	}
	all := []ordered{}
	for rows.Next() {
		var o ordered
		var b []byte
		var event checksandalerts.Event
		if e = rows.Scan(&o.seq, &b); e == nil {
			e = json.Unmarshal(b, &event)
		}
		if e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
		o.urgent, o.key, o.ends = outboxOrderOf(event)
		all = append(all, o)
	}
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e = rows.Err(); e != nil {
		return e
	}
	for _, o := range all {
		if _, e = tx.Exec("UPDATE outbox SET urgent=?, incident_key=?, closes=? WHERE seq=?", o.urgent, o.key, o.ends, o.seq); e != nil {
			return e
		}
	}
	return nil
}
