package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"time"
)

// OpenReadOnlyDatabase opens an existing SQLite file without schema changes or
// writer locks. Normal SQLite locks keep reads consistent with a live WAL.
func OpenReadOnlyDatabase(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	location := url.URL{Scheme: "file", Path: absolute}
	query := location.Query()
	query.Set("mode", "ro")
	query.Set("_pragma", "query_only(1)")
	location.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if err = database.Ping(); err != nil {
		database.Close()
		return nil, fmt.Errorf("cannot read %s: %w; check the state_dir in mmwatchdog_node_settings.yml and run with sudo", path, err)
	}
	return database, nil
}

// HistoryCheck names a check that has recorded measurements, even if its
// readings fall outside the requested span or it is no longer configured.
type HistoryCheck struct{ CheckGroup, Check string }

// ReadMeasurementHistory reads one node's runs in an inclusive time span.
// It closes both cursors and the connection before the caller prints anything.
func ReadMeasurementHistory(path, node, checkGroup, check string, since, until time.Time) (readings []RunReadings, checks []HistoryCheck, resultError error) {
	defer func() {
		if resultError != nil {
			resultError = fmt.Errorf("cannot read measurement history %s: %w; check state_dir and the measurement history file", path, resultError)
		}
	}()
	database, err := OpenReadOnlyDatabase(path)
	if err != nil {
		return nil, nil, err
	}
	defer database.Close()
	rows, err := database.Query("SELECT DISTINCT check_group, check_name FROM readings WHERE node=?", node)
	if err != nil {
		return nil, nil, err
	}
	catalog := []HistoryCheck{}
	for rows.Next() {
		var entry HistoryCheck
		if err = rows.Scan(&entry.CheckGroup, &entry.Check); err != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return nil, nil, err
		}
		catalog = append(catalog, entry)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	runs := []RunReadings{}
	// SQLite stores run times as nanoseconds since 1970. Clamp query bounds,
	// rather than overflowing UnixNano for otherwise valid RFC 3339 dates.
	if since.Before(time.Unix(0, 0)) {
		since = time.Unix(0, 0)
	}
	if until.After(time.Unix(0, math.MaxInt64)) {
		until = time.Unix(0, math.MaxInt64)
	}
	if since.After(until) {
		return runs, catalog, nil
	}
	statement := "SELECT id, at, check_group, check_name, measurements FROM readings WHERE node=? AND check_group=? AND at>=? AND at<=?"
	arguments := []any{node, checkGroup, since.UnixNano(), until.UnixNano()}
	if check != "" {
		statement += " AND check_name=?"
		arguments = append(arguments, check)
	}
	rows, err = database.Query(statement+" ORDER BY at, id", arguments...)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var run RunReadings
		var at int64
		var measurements []byte
		if err = rows.Scan(&run.SequenceNumber, &at, &run.CheckGroup, &run.Check, &measurements); err != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return nil, nil, err
		}
		run.At = time.Unix(0, at).UTC()
		if err = json.Unmarshal(measurements, &run.Measurements); err != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return nil, nil, fmt.Errorf("cannot decode readings of %s/%s: %w; check the measurement history file", run.CheckGroup, run.Check, err)
		}
		runs = append(runs, run)
	}
	return runs, catalog, errors.Join(rows.Err(), rows.Close())
}
