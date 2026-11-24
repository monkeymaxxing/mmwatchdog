package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"strconv"

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
	if _, e = tx.Exec("PRAGMA user_version=21"); e != nil {
		return e
	}
	return tx.Commit()
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
