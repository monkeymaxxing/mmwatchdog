package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"

	"mmwatchdog/internal/checksandalerts"
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
// incidents when it was queued; this format records it once the
// destination accepts it (recordOnAcceptance). A queued delivery of an
// incident that has such a record gets the record of its acceptance, and
// the record keeps only the severities that left the outbox, so that the
// hub settles a send only once the destination has it. A severity goes out
// once for each incident, so a severity that waits in the outbox was not
// accepted yet. A record left with no severity is removed: the delivery's
// acceptance writes it again.
func recordQueuedDirectSendsOnAcceptance(tx *sql.Tx) error {
	rows, e := tx.Query(`SELECT o.seq, o.destination, o.event, r.alert_severities_sent_for_incident FROM outbox o JOIN incidents_sent_without_the_hub r
 ON r.destination=o.destination AND r.incident_id=json_extract(CAST(o.event AS TEXT),'$.incident_id') ORDER BY o.seq`)
	if e != nil {
		return e
	}
	type queued struct {
		seq                    int64
		destination, incident  string
		record, severity, sent string
	}
	all := []queued{}
	for rows.Next() {
		var q queued
		var b []byte
		var event checksandalerts.Event
		if e = rows.Scan(&q.seq, &q.destination, &b, &q.sent); e == nil {
			e = json.Unmarshal(b, &event)
		}
		if e == nil && opensOrChanges(event.Type) {
			q.incident, q.severity = event.Incident, event.AlertSeverity
			q.record, e = acceptanceRecord([]checksandalerts.Event{event}, event.MMHotlineAlertSource)
			all = append(all, q)
		}
		if e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
	}
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e = rows.Err(); e != nil {
		return e
	}
	accepted := map[[2]string][]string{}
	for _, q := range all {
		if _, e = tx.Exec("UPDATE outbox SET sent_without_the_hub=? WHERE seq=?", q.record, q.seq); e != nil {
			return e
		}
		id := [2]string{q.destination, q.incident}
		if _, seen := accepted[id]; !seen {
			accepted[id] = strings.Split(q.sent, ",")
		}
		accepted[id] = slices.DeleteFunc(accepted[id], func(s string) bool { return s == q.severity })
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
