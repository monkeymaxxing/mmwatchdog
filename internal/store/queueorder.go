package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// The order of each destination's queue in the outbox, one rule
// (Store.Pending). The queue is first in, first out, with one exception:
// while its oldest delivery has had a failed attempt and waits for a
// retry, the next attempt goes to the oldest sos alert that has not had a
// failed attempt yet and that no closing alert of its own key precedes in
// the queue; if there is none, to the oldest delivery again. So an alert
// that keeps failing holds no sos back, an sos that keeps failing holds
// back no sos of another key, and an sos never passes the closing alert of
// its own key. Nothing is removed. The cost: while a destination is
// failing, an sos can reach it before the warn of its own incident. The
// wait before the next attempt is the destination's (Store.Delivered), so
// a change of the delivery tried never shortens it. The rule is in the
// design of alert delivery, "An sos goes first".

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
// announced.
func insertOutbox(tx *sql.Tx, at int64, name string, alert checksandalerts.Event, target []byte, announces string) error {
	event, e := json.Marshal(alert)
	if e != nil {
		return e
	}
	urgent, key, closes := outboxOrderOf(alert)
	_, e = tx.Exec("INSERT INTO outbox(at,queued_at,destination,event,target,urgent,incident_key,closes,announced_key) VALUES(?,?,?,?,?,?,?,?,?)", at, time.Now().UnixNano(), name, event, target, urgent, key, closes, announces)
	return e
}
