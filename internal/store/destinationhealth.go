package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// DestinationHealth is the durable verification schedule and notice lifecycle
// for one receiving address and credential, stored with the events it emits.
type DestinationHealth struct {
	Target                   checksandalerts.Destination `json:"target"`
	VerificationFailingSince time.Time                   `json:"verification_failing_since,omitempty"`
	VerificationError        string                      `json:"verification_error,omitempty"`
	FailingNoticeSent        bool                        `json:"failing_notice_sent"`
	NextVerificationAt       time.Time                   `json:"next_verification_at"`
}

// storedDestinationHealth keeps the private target fingerprint in state.db.
type storedDestinationHealth struct {
	DestinationHealth
	Target storedDestination `json:"target"`
}

func (s *Store) DestinationHealth() (map[string]DestinationHealth, error) {
	rows, err := s.DB.Query("SELECT destination,data FROM destination_health")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	health := map[string]DestinationHealth{}
	for rows.Next() {
		var name string
		var encoded []byte
		if err := rows.Scan(&name, &encoded); err != nil {
			return nil, err
		}
		var record storedDestinationHealth
		if err := json.Unmarshal(encoded, &record); err != nil {
			return nil, err
		}
		record.DestinationHealth.Target = record.Target.destination()
		health[name] = record.DestinationHealth
	}
	return health, rows.Err()
}

// saveDestinationHealth runs in Store.Save's transaction so the notice and
// whether it was announced cannot disagree after a restart.
func saveDestinationHealth(tx *sql.Tx, health map[string]DestinationHealth) error {
	if health == nil {
		return nil
	}
	names, err := json.Marshal(checksandalerts.Names(health))
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM destination_health WHERE destination NOT IN (SELECT value FROM json_each(?))", string(names)); err != nil {
		return err
	}
	for _, name := range checksandalerts.Names(health) {
		encoded, err := json.Marshal(storedDestinationHealth{health[name], destinationForStorage(health[name].Target)})
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO destination_health(destination,data) VALUES(?,?) ON CONFLICT(destination) DO UPDATE SET data=excluded.data WHERE data!=excluded.data", name, encoded); err != nil {
			return err
		}
	}
	return nil
}
