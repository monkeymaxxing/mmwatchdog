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
