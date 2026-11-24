package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/store"
)

// The intake's limits hold across acceptance, delivery, and the
// replacement of a node's reminder: what waits counts, and what left the
// intake no longer counts.
func TestTheIntakeCountsWhatWaits(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	failure := func(node, id string) checksandalerts.Event {
		return checksandalerts.Event{ID: id, Node: node, CheckGroup: "app", Check: "http", Type: "failed", AlertSeverity: "warn", At: now, Message: "expected 200, received 503", Body: strings.Repeat("expected 200, received 503 from https://app.example.com/health; the load balancer marked it unhealthy. ", 6)}
	}
	// Acceptance checks the node's share before a reminder replaces the
	// waiting one, so the share holds two reminders.
	reminder := func(id string) checksandalerts.Event {
		r := failure("node-6", id)
		r.Type, r.Incident, r.Destination, r.FleetIncidentKey, r.Body = "reminder", "i-1", "chat", "0123456789abcdef0123456789abcdef", ""
		return r
	}
	b, _ := json.Marshal(failure("node-1", "a-00"))
	size := int64(len(b))
	if r, _ := json.Marshal(reminder("r-00")); 2*int64(len(r)) > size {
		t.Fatal("two reminders of this test must fit in the size of one failure")
	}
	limits := config.DefaultStorageLimits
	// Four alerts fit, and one node may hold one alert.
	limits.UndeliveredAlertsOfNodesBytes = 4*size + size/2
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), limits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for i := 1; i <= 4; i++ {
		if e = s.Accept(fmt.Sprintf("node-%d", i), failure(fmt.Sprintf("node-%d", i), "a-00"), now); e != nil {
			t.Fatal(i, e)
		}
	}
	if e = s.Accept("node-5", failure("node-5", "a-00"), now); !errors.Is(e, store.ErrIntakeFull) {
		t.Fatal("an alert beyond the intake's limit was accepted", e)
	}
	if e = s.Accept("node-1", failure("node-1", "a-01"), now); !errors.Is(e, store.ErrIntakeFull) {
		t.Fatal("an alert beyond the node's share was accepted", e)
	}
	rows, e := s.Intake()
	if e != nil || len(rows) != 4 {
		t.Fatal(rows, e)
	}
	done := []int64{}
	for _, row := range rows {
		done = append(done, row.Seq)
	}
	if e = s.Emit(store.AlertGroupingPlan{Done: done}, nil, false, now); e != nil {
		t.Fatal(e)
	}
	for _, node := range []string{"node-5", "node-1"} {
		if e = s.Accept(node, failure(node, "a-02"), now); e != nil {
			t.Fatal("delivered alerts still count against the intake's limit", node, e)
		}
	}
	// A node's new reminder replaces its waiting one, so it never fills
	// the node's share.
	for i := range 10 {
		if e = s.Accept("node-6", reminder(fmt.Sprintf("r-%02d", i)), now); e != nil {
			t.Fatal("a replaced reminder still counts", i, e)
		}
	}
}
