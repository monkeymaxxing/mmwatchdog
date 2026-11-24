package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// The order of each destination's queue in the outbox. A destination gets
// its alerts oldest first, except that an sos alert of an incident goes
// ahead of every alert below sos, across keys, and supersedes an
// undelivered alert below sos of its own key. Within one key a resolution
// and what follows it keep their order, and a notice keeps its place. The
// rules are in the design of grouping by exact finding, "Queue order".

// outboxOrderOf says where a queued delivery stands: urgent for an sos
// alert of an incident that does not close it, key for its fleet incident
// key, and closes for an alert that closes its key, also an MMHotline
// resolution.
func outboxOrderOf(event checksandalerts.Event) (urgent bool, key string, closes bool) {
	severity := event.AlertSeverity
	if a := event.MMHotlineAlert; a != nil {
		severity = a.Severity
	}
	closes = checksandalerts.Closes(event.Type) || strings.HasPrefix(severity, "resolved-")
	urgent = severity == "sos" && !closes && event.Type != "notice" && checksandalerts.BelongsToAnIncident(event.Type)
	return urgent, event.FleetIncidentKey, closes
}

// forgetUndeliveredAnnouncements makes each key whose announcement an
// outbox row that matches where carries no longer announced to the row's
// destination, before the rows leave the outbox undelivered: by retention,
// a permanent refusal, or a replaced destination. The next matching alert
// then opens the key again.
func forgetUndeliveredAnnouncements(tx *sql.Tx, where string, args ...any) error {
	_, e := tx.Exec(`UPDATE fleet_incident_keys SET announced_alert_severity='', announced_exact_findings='[]'
 WHERE (destination,incident_key) IN (SELECT destination, announced_key FROM outbox WHERE announced_key!='' AND (`+where+`))`, args...)
	return e
}

// insertOutbox queues one delivery of alert for the destination name, whose
// identity at this time is target. announces is the readable fleet
// incident key whose announcement for the destination the alert carries,
// or "": when the alert leaves the outbox undelivered, the key is no longer
// announced. An urgent alert supersedes the undelivered alerts below sos of
// its key that wait to be retried after a failed attempt and follow the
// key's latest closing alert: they would only tell less, later, and the
// destination's wait holds them back. An alert below sos of its key that
// has not been attempted yet stays, and goes after the urgent one
// (Store.Pending).
func insertOutbox(tx *sql.Tx, at int64, name string, alert checksandalerts.Event, target []byte, announces string) error {
	event, e := json.Marshal(alert)
	if e != nil {
		return e
	}
	urgent, key, closes := outboxOrderOf(alert)
	if urgent && key != "" {
		if _, e = tx.Exec(`DELETE FROM outbox WHERE destination=? AND incident_key=? AND urgent=0 AND closes=0 AND attempts>0 AND json_extract(event,'$.type')!='notice'
 AND seq > COALESCE((SELECT MAX(seq) FROM outbox WHERE destination=? AND incident_key=? AND closes=1),0)`, name, key, name, key); e != nil {
			return e
		}
	}
	_, e = tx.Exec("INSERT INTO outbox(at,queued_at,destination,event,target,urgent,incident_key,closes,announced_key) VALUES(?,?,?,?,?,?,?,?,?)", at, time.Now().UnixNano(), name, event, target, urgent, key, closes, announces)
	return e
}
