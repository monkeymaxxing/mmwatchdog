package store

import (
	"database/sql"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// Receives reports that destination name receives message, which stands for
// members. An alert goes to the destinations that GoesTo gives it. A
// recovery or retirement goes where its incident's alerts went, as this
// store recorded them, so a change of alert_severities during the incident
// neither loses nor invents one; without a record, such as for a failure a
// joined node delivered directly, it goes to the destinations that take one
// of the severities the incident went out with. For a destination that
// keeps a fleet incident set this keeps the set closing the incidents
// whose alerts it got.
func Receives(tx *sql.Tx, name string, d checksandalerts.Destination, message checksandalerts.Event, members []checksandalerts.Event, destinations map[string]checksandalerts.Destination) (bool, error) {
	if !checksandalerts.Closes(message.Type) {
		return config.GoesTo(name, d, message, destinations), nil
	}
	recorded := false
	for _, m := range members {
		var any, here int
		if e := tx.QueryRow("SELECT COUNT(*), COALESCE(SUM(destination=?),0) FROM incident_alert_destinations WHERE incident_id=?", name, m.Incident).Scan(&any, &here); e != nil {
			return false, e
		}
		if here > 0 {
			return true, nil
		}
		recorded = recorded || any > 0
	}
	return !recorded && config.GoesTo(name, d, message, destinations), nil
}

// RememberIncidentDestinations records the destinations an alert that
// announces or raises an incident went to, and forgets them when the
// incident's recovery or retirement has gone out.
func RememberIncidentDestinations(tx *sql.Tx, at int64, message checksandalerts.Event, members []checksandalerts.Event, destinations map[string]checksandalerts.Destination) error {
	for _, m := range members {
		if m.Incident == "" {
			continue
		}
		if checksandalerts.Closes(message.Type) {
			if _, e := tx.Exec("DELETE FROM incident_alert_destinations WHERE incident_id=?", m.Incident); e != nil {
				return e
			}
			continue
		}
		if !checksandalerts.Opens(message.Type) {
			continue
		}
		for _, name := range checksandalerts.Names(destinations) {
			if !config.GoesTo(name, destinations[name], message, destinations) {
				continue
			}
			if _, e := tx.Exec("INSERT OR IGNORE INTO incident_alert_destinations(incident_id,destination,at) VALUES(?,?,?)", m.Incident, name, at); e != nil {
				return e
			}
		}
	}
	return nil
}
