package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mmwatchdog/internal/store"
)

func TestReadMeasurementHistoryWithoutDaemon(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), store.MeasurementHistoryFile)
	history, err := store.OpenMeasurementHistory(path, 16*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	for index, value := range []float64{41, 95, 60} {
		if err = history.Add(store.RunReadings{At: start.Add(time.Duration(index) * time.Minute), CheckGroup: "machine", Check: "memory", Measurements: map[string]store.MeasurementReading{"memory_used_percent": {Value: value, Unit: "percent"}}}); err != nil {
			t.Fatal(err)
		}
	}
	history.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runs, catalog, err := store.ReadMeasurementHistory(path, "", "machine", "memory", start.Add(time.Minute), start.Add(2*time.Minute))
	if err != nil || len(runs) != 2 || len(catalog) != 1 || runs[0].Measurements["memory_used_percent"].Value != 95 || !runs[1].At.Equal(start.Add(2*time.Minute)) {
		t.Fatal(runs, catalog, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("reading changed the file", err)
	}
	database, err := store.OpenReadOnlyDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err = database.Exec("DELETE FROM readings"); err == nil {
		t.Fatal("the reader can write")
	}
}

func TestReadMeasurementHistoryWhileWriterHasTransaction(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), store.MeasurementHistoryFile)
	history, err := store.OpenMeasurementHistory(path, 16*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	now := time.Now().UTC()
	if err = history.Add(store.RunReadings{At: now, CheckGroup: "machine", Check: "memory", Measurements: map[string]store.MeasurementReading{"memory_used_percent": {Value: 41, Unit: "percent"}}}); err != nil {
		t.Fatal(err)
	}
	transaction, err := history.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	if _, err = transaction.Exec("UPDATE readings SET measurements=measurements"); err != nil {
		t.Fatal(err)
	}
	runs, _, err := store.ReadMeasurementHistory(path, "", "machine", "", now.Add(-time.Hour), now)
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	if err = transaction.Commit(); err != nil {
		t.Fatal(err)
	}
}
