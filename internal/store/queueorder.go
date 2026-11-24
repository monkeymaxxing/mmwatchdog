package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// The order of each destination's queue in the outbox, one rule
// (Store.Pending). The queue holds a line of deliveries for each fleet
// incident key, and one line of the deliveries without a key, such as
// notices, which so keep their place among themselves. A line's head is
// its oldest delivery, except that an sos alert may pass deliveries of its
// own line when the line is stuck, its oldest delivery having had a failed
// attempt and waiting to be retried: then it passes every delivery of the
// line but a closing alert, so a warn that keeps failing never holds the
// sos back. A line that is not stuck keeps its order, so an incident's
// opening reaches a healthy destination before its raise, and a resolution
// and what follows it keep their order. Of the heads of its
// lines, the destination gets an sos head first, else a head that does not
// wait to be retried, else the oldest: an alert that keeps failing holds
// back no other key, and so no other key's sos. Nothing is removed. The
// wait before the next attempt stays destination-wide. The rule is in the
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
