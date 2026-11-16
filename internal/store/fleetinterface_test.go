package store_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/store"
)

func TestFallbackIncidentRecordsWaitForAcknowledgementWithoutAgeOrCountLimit(t *testing.T) {
	t.Parallel()
	s, err := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 1201; i++ {
		if _, err := tx.Exec(`INSERT INTO incidents_sent_without_the_hub(destination,incident_id,check_group,check_name,finding,kind,alert_severities_sent_for_incident,at) VALUES('ops',?,'app','shell','','failed','warn',?)`, fmt.Sprint(i), time.Now().Add(-72*time.Hour).UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	records, err := s.SentWithoutTheHub()
	if err != nil || len(records) != 1201 {
		t.Fatalf("unconfirmed incidents removed: %d, %v", len(records), err)
	}
	if err := s.CarriedToHub([]store.IncidentKnownToTheHub{{Destination: "ops", Incident: "0"}}); err != nil {
		t.Fatal(err)
	}
	records, err = s.SentWithoutTheHub()
	if err != nil || len(records) != 1200 {
		t.Fatal(len(records), err)
	}
	for _, record := range records {
		if record.Incident == "0" {
			t.Fatal("confirmed record remains")
		}
	}
}
