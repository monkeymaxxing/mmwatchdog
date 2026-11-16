package store_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

func TestStoreRestartAndOutboxOrder(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	s, e := store.OpenStore(dir, config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	snap := incidents.FreshSnapshot()
	now := time.Now().UTC()
	snap.Checks["app/http"] = &incidents.CheckState{CheckGroup: "app", Check: "http", FailureIncident: incidents.Track{Incident: "incident"}}
	snap.RecoveryAttemptsByTarget["app.service"] = &incidents.RecoveryAttempts{Attempts: 2}
	snap.Maintenance["app"] = checksandalerts.Window{Until: now.Add(time.Minute)}
	snap.Maintenance[checksandalerts.EveryCheckGroup] = checksandalerts.Window{Until: now.Add(time.Hour), Reason: "deploy v1.4", User: "alice"}
	events := []checksandalerts.Event{{AlertSeverity: "warn", ID: "failure", Incident: "incident", CheckGroup: "app", Check: "http", Type: "failed", At: now, Notify: true}, {AlertSeverity: "warn", ID: "recovery", Incident: "incident", CheckGroup: "app", Check: "http", Type: "recovered", At: now.Add(time.Second), Notify: true}}
	if e = s.Save(snap, events, store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, TimeLimitPerDeliveryAttempt: "1s"}, "b": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.org"}, TimeLimitPerDeliveryAttempt: "1s"}}}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = store.OpenStore(dir, config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	got, e := s.Load()
	if e != nil {
		t.Fatal(e)
	}
	if got.Checks["app/http"].FailureIncident.Incident != "incident" || got.RecoveryAttemptsByTarget["app.service"].Attempts != 2 {
		t.Fatal(got)
	}
	if w := got.Maintenance[checksandalerts.EveryCheckGroup]; !w.Until.Equal(now.Add(time.Hour)) || w.Reason != "deploy v1.4" || w.User != "alice" || len(got.Maintenance) != 2 {
		t.Fatal("maintenance windows did not survive restart", got.Maintenance)
	}
	ds, e := s.Pending()
	if e != nil || len(ds) != 2 {
		t.Fatal(ds, e)
	}
	for _, d := range ds {
		if d.Event.ID != "failure" {
			t.Fatal("recovery overtook failure", d)
		}
		if d.Destination == "a" {
			if delay, e := s.Delivered(d, nil); delay != 0 || e != nil {
				t.Fatal("a delivered alert has a next attempt", delay, e)
			}
		} else if delay, e := s.Delivered(d, fmt.Errorf("offline")); delay != time.Second || e != nil {
			t.Fatal("the first failed attempt is not followed by another a second later", delay, e)
		}
	}
	// The head of each queue: the recovery for a, and for b the failure,
	// whose next attempt the outbox keeps for status and for a restart.
	ds, e = s.Pending()
	if e != nil || len(ds) != 2 || ds[0].Event.ID != "failure" || ds[0].Destination != "b" || ds[0].Attempts != 1 || !ds[0].Next.After(time.Now()) || ds[1].Event.ID != "recovery" || ds[1].Destination != "a" {
		t.Fatal(ds, e)
	}
}

// The outbox keeps the newest undelivered alerts within its size limit, and
// the ones it drops leave a counted delivery gap. No count bounds it.
func TestOutboxBoundsExposeGap(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	events := []checksandalerts.Event{}
	for i := 0; i < 1500; i++ {
		events = append(events, checksandalerts.Event{AlertSeverity: "warn", ID: incidents.ID(), At: time.Now(), Notify: true})
	}
	route := store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, TimeLimitPerDeliveryAttempt: "1s"}}}
	if e = s.Save(incidents.FreshSnapshot(), events[:1200], route); e != nil {
		t.Fatal(e)
	}
	stats, e := s.DeliveryStats()
	if e != nil || stats.Pending != 1200 || stats.Gaps != 0 {
		t.Fatal("1200 small alerts fit the default limit, which no count bounds", stats, e)
	}
	// With a limit of 64 KiB, the next commit keeps only the newest alerts.
	limits := config.DefaultStorageLimits
	limits.UndeliveredAlertsBytes = 64 * 1024
	s.SetLimits(limits)
	if e = s.Save(incidents.FreshSnapshot(), events[1200:], route); e != nil {
		t.Fatal(e)
	}
	var bytes int64
	if e = s.DB.QueryRow("SELECT SUM(length(event)+length(target)) FROM outbox").Scan(&bytes); e != nil {
		t.Fatal(e)
	}
	stats, e = s.DeliveryStats()
	if e != nil || bytes > limits.UndeliveredAlertsBytes || bytes < limits.UndeliveredAlertsBytes/2 || stats.Pending == 0 || int64(stats.Pending)+stats.Gaps != 1500 {
		t.Fatal(bytes, stats, e)
	}
	// The newest alert is kept.
	var newest string
	if e = s.DB.QueryRow("SELECT json_extract(event,'$.id') FROM outbox ORDER BY seq DESC LIMIT 1").Scan(&newest); e != nil || newest != events[1499].ID {
		t.Fatal(newest, e)
	}
}

// The event history and the outbox follow the storage limits a configuration
// states, by age and by size, and a reload's new limits apply to the next
// pruning.
func TestStoresFollowTheStatedLimits(t *testing.T) {
	t.Parallel()
	limits := config.DefaultStorageLimits
	limits.KeepEventHistoryFor, limits.KeepUndeliveredAlertsFor, limits.EventHistoryBytes = 7*24*time.Hour, time.Hour, 1024*1024
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), limits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	now := time.Now().UTC()
	route := store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, TimeLimitPerDeliveryAttempt: "1s"}}}
	old := checksandalerts.Event{AlertSeverity: "warn", ID: "three-days-old", At: now.Add(-3 * 24 * time.Hour), Notify: true}
	fresh := checksandalerts.Event{AlertSeverity: "warn", ID: "fresh", At: now, Notify: true}
	if e = s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{old, fresh}, route); e != nil {
		t.Fatal(e)
	}
	ids := func(query string) []string {
		t.Helper()
		rows, e := s.DB.Query(query)
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				t.Fatal(e)
			}
			out = append(out, id)
		}
		return out
	}
	// Events are kept for 7 days here, undelivered alerts for 1 hour.
	if got := ids("SELECT json_extract(data,'$.id') FROM events WHERE json_extract(data,'$.type') IS NOT 'delivery_gap' ORDER BY seq"); !reflect.DeepEqual(got, []string{"three-days-old", "fresh"}) {
		t.Fatal("events", got)
	}
	if got := ids("SELECT json_extract(event,'$.id') FROM outbox ORDER BY seq"); !reflect.DeepEqual(got, []string{"fresh"}) {
		t.Fatal("outbox", got)
	}
	// A reload that keeps events for one day drops the older one.
	limits.KeepEventHistoryFor = 24 * time.Hour
	s.SetLimits(limits)
	if e = s.Prune(now); e != nil {
		t.Fatal(e)
	}
	if got := ids("SELECT json_extract(data,'$.id') FROM events WHERE json_extract(data,'$.type') IS NOT 'delivery_gap' ORDER BY seq"); !reflect.DeepEqual(got, []string{"fresh"}) {
		t.Fatal("events after the reload", got)
	}
	// The size limit keeps the newest events within 1 MiB.
	large := []checksandalerts.Event{}
	for i := 0; i < 200; i++ {
		large = append(large, checksandalerts.Event{ID: incidents.ID(), Type: "failed", AlertSeverity: "warn", At: now, Message: strings.Repeat("x", 4000)})
	}
	large[199].ID = "newest"
	if e = s.Save(incidents.FreshSnapshot(), large[:100], route); e != nil {
		t.Fatal(e)
	}
	if e = s.Save(incidents.FreshSnapshot(), large[100:], route); e != nil {
		t.Fatal(e)
	}
	var bytes int64
	var newest string
	if e = s.DB.QueryRow("SELECT SUM(length(data)) FROM events").Scan(&bytes); e != nil || bytes > limits.EventHistoryBytes || bytes < limits.EventHistoryBytes/2 {
		t.Fatal(bytes, e)
	}
	if e = s.DB.QueryRow("SELECT json_extract(data,'$.id') FROM events ORDER BY seq DESC LIMIT 1").Scan(&newest); e != nil || newest != "newest" {
		t.Fatal(newest, e)
	}
}

// A database file that already holds more than its setting allows is
// refused with the size to state, when the store opens and, without opening
// it, in runtime validation.
func TestDatabaseLargerThanItsLimitIsRefused(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	s, e := store.OpenStore(dir, config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("CREATE TABLE filler (data BLOB); INSERT INTO filler VALUES (zeroblob(3*1024*1024))"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	small := config.DefaultStorageLimits
	small.StateDatabaseBytes = 1024 * 1024
	if s, e = store.OpenStore(dir, small); e == nil || !strings.Contains(e.Error(), "state.db already holds 4 MiB, more than state_database_size_limit allows (1 MiB); state at least 4MiB") {
		if s != nil {
			s.Close()
		}
		t.Fatal(e)
	}
	path := filepath.Join(dir, "state.db")
	if e = store.StateDatabaseLimit(1024 * 1024).SizeError(path); e == nil || !strings.Contains(e.Error(), "more than state_database_size_limit allows (1 MiB); state at least 4MiB") {
		t.Fatal(e)
	}
	if e = store.StateDatabaseLimit(8 * 1024 * 1024).SizeError(path); e != nil {
		t.Fatal(e)
	}
	if e = store.StateDatabaseLimit(1).SizeError(filepath.Join(dir, "absent.db")); e != nil {
		t.Fatal("a database that does not exist yet has no size", e)
	}
	// fleet.db may hold twice what its setting states, so a file of 4 MiB
	// fits a stated 2 MiB and needs at least that.
	if e = store.FleetDatabaseLimit(1024 * 1024).SizeError(path); e == nil || !strings.Contains(e.Error(), "more than twice fleet_hub_role.event_history_of_nodes_content_size_limit allows (2 MiB); state at least 2MiB") {
		t.Fatal(e)
	}
	if e = store.FleetDatabaseLimit(2 * 1024 * 1024).SizeError(path); e != nil {
		t.Fatal(e)
	}
}

func TestDeliveryResultMatchesEventIdentity(t *testing.T) {
	t.Parallel()
	for _, deliveryError := range []error{nil, fmt.Errorf("receiver unavailable")} {
		t.Run(fmt.Sprint(deliveryError), func(t *testing.T) {
			s, err := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			now := time.Now()
			destinations := map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, TimeLimitPerDeliveryAttempt: "5s"}}
			event := checksandalerts.Event{AlertSeverity: "warn", ID: "expired", At: now.Add(-config.DefaultStorageLimits.KeepUndeliveredAlertsFor + time.Minute), Notify: true}
			if err := s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{event}, store.AlertRoute{Destinations: destinations}); err != nil {
				t.Fatal(err)
			}
			pending, err := s.Pending()
			if err != nil || len(pending) != 1 {
				t.Fatal(pending, err)
			}
			inFlight := pending[0]
			if err := s.Prune(now.Add(2 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			event = checksandalerts.Event{AlertSeverity: "warn", ID: "new", At: now, Notify: true}
			if err := s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{event}, store.AlertRoute{Destinations: destinations}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Delivered(inFlight, deliveryError); err != nil {
				t.Fatal(err)
			}
			pending, err = s.Pending()
			if err != nil || len(pending) != 1 || pending[0].Event.ID != "new" || pending[0].Attempts != 0 {
				t.Fatal("delivery result changed a different event", pending, err)
			}
		})
	}
}
func TestSnapshotAndEventAreAtomic(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	snap := incidents.FreshSnapshot()
	if e = s.Save(snap, nil, store.AlertRoute{}); e != nil {
		t.Fatal(e)
	}
	s.DB.Exec("CREATE TRIGGER reject_event BEFORE INSERT ON events BEGIN SELECT RAISE(FAIL, 'fixture failure'); END")
	snap.RecoveryAttemptsByTarget["x"] = &incidents.RecoveryAttempts{Attempts: 2}
	if e = s.Save(snap, []checksandalerts.Event{{ID: incidents.ID(), At: time.Now()}}, store.AlertRoute{}); e == nil {
		t.Fatal("write succeeded")
	}
	got, e := s.Load()
	if e != nil || len(got.RecoveryAttemptsByTarget) != 0 {
		t.Fatal("partial transaction", got, e)
	}
}

func TestStoreFullRollsBackAndRecovers(t *testing.T) {
	t.Parallel()
	s, err := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap := incidents.FreshSnapshot()
	if err = s.Save(snap, nil, store.AlertRoute{}); err != nil {
		t.Fatal(err)
	}
	var pages int
	if err = s.DB.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatal(err)
	}
	snap.Checks["app/shell"] = &incidents.CheckState{FailureIncident: incidents.Track{Incident: "new"}, Last: checksandalerts.Observation{Stdout: strings.Repeat("x", 32768)}}
	if err = s.Save(snap, []checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), Type: "failed", At: time.Now(), Notify: true}}, store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"test": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://example.com"}}}}); err == nil || !strings.Contains(err.Error(), "database or disk is full") {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || len(got.Checks) != 0 {
		t.Fatal(got, err)
	}
	events, err := s.Events(100)
	if err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
	deliveries, err := s.Pending()
	if err != nil || len(deliveries) != 0 {
		t.Fatal(deliveries, err)
	}
	if _, err = s.DB.Exec("PRAGMA max_page_count=32768"); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(snap, nil, store.AlertRoute{}); err != nil {
		t.Fatal(err)
	}
}

func TestStorageByteLimitsAndPhysicalGrowth(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.OpenStore(dir, config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var first int64
	diskBytes := func() int64 {
		var total int64
		for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
			info, e := os.Stat(filepath.Join(dir, name))
			if e == nil {
				total += info.Size()
			} else if !os.IsNotExist(e) {
				t.Fatal(e)
			}
		}
		return total
	}
	for batch := 0; batch < 16; batch++ {
		events := []checksandalerts.Event{}
		for i := 0; i < 180; i++ {
			events = append(events, checksandalerts.Event{ID: incidents.ID(), Type: "failed", AlertSeverity: "warn", At: time.Now(), Notify: true, Evidence: checksandalerts.CaptureEvidence("incident", checksandalerts.Observation{Message: "failed", Stdout: strings.Repeat("x", 14000)}, checksandalerts.SourceLocation{}, checksandalerts.Timing{})})
		}
		if err = s.Save(incidents.FreshSnapshot(), events, store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"test": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://example.com"}}}}); err != nil {
			t.Fatal(err)
		}
		if batch == 7 {
			first = diskBytes()
		}
	}
	var history, outbox int64
	if err = s.DB.QueryRow("SELECT COALESCE(SUM(length(data)),0) FROM events").Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow("SELECT COALESCE(SUM(length(event)+length(target)),0) FROM outbox").Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	stats, err := s.DeliveryStats()
	if err != nil || history > config.DefaultStorageLimits.EventHistoryBytes || outbox > config.DefaultStorageLimits.UndeliveredAlertsBytes || stats.Gaps == 0 {
		t.Fatal(history, outbox, stats, err)
	}
	final := diskBytes()
	if final > config.DefaultStorageLimits.StateDatabaseBytes || final > first+8*1024*1024 {
		t.Fatalf("database/WAL kept growing: first=%d final=%d", first, final)
	}
	t.Logf("retained bytes history=%d outbox=%d; database/WAL/SHM after 8/16 churn batches=%d/%d", history, outbox, first, final)
	// Expiry removes evidence as well as summary rows.
	if err = s.Prune(time.Now().Add(2 * config.DefaultStorageLimits.KeepEventHistoryFor)); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Evidence != nil {
			t.Fatal("expired evidence retained", event.ID)
		}
	}
}

func TestCommittedStateSurvivesProcessKill(t *testing.T) {
	t.Parallel()
	if dir := os.Getenv("MMWATCHDOG_CRASH_TEST_ROOT"); dir != "" {
		s, err := store.OpenStore(dir, config.DefaultStorageLimits)
		if err != nil {
			t.Fatal(err)
		}
		snap := incidents.FreshSnapshot()
		snap.RecoveryAttemptsByTarget["app.service"] = &incidents.RecoveryAttempts{Attempts: 2}
		snap.Checks["app/shell"] = &incidents.CheckState{FailureIncident: incidents.Track{Incident: "durable"}}
		if err = s.Save(snap, []checksandalerts.Event{{AlertSeverity: "warn", ID: "durable-event", Incident: "durable", At: time.Now(), Notify: true}}, store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"test": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://example.com"}}}}); err != nil {
			t.Fatal(err)
		}
		fmt.Println("committed")
		time.Sleep(time.Hour)
		return
	}
	dir := filepath.Join(t.TempDir(), "state")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommittedStateSurvivesProcessKill$")
	cmd.Env = append(os.Environ(), "MMWATCHDOG_CRASH_TEST_ROOT="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "committed\n" {
			t.Fatal(line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not commit")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	s, err := store.OpenStore(dir, config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Load()
	if err != nil || snap.Checks["app/shell"].FailureIncident.Incident != "durable" || snap.RecoveryAttemptsByTarget["app.service"].Attempts != 2 {
		t.Fatal(snap, err)
	}
	ds, err := s.Pending()
	if err != nil || len(ds) != 1 || ds[0].Event.ID != "durable-event" {
		t.Fatal(ds, err)
	}
	var integrity string
	if err = s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
}

func TestSaveWritesOnlyChangedConditions(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.OpenStore(dir, config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	snap := incidents.FreshSnapshot()
	for _, key := range []string{"a/shell", "b/shell", "c/shell"} {
		snap.Checks[key] = &incidents.CheckState{CheckGroup: key[:1], Check: "shell", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}, Last: checksandalerts.Observation{Status: "ok", Stdout: strings.Repeat("x", 1000)}}
	}
	if err = s.Save(snap, nil, store.AlertRoute{}); err != nil {
		t.Fatal(err)
	}
	counting := func(s *store.Store) func() int {
		t.Helper()
		for _, statement := range []string{
			"CREATE TABLE IF NOT EXISTS writes (n INTEGER NOT NULL)",
			"DELETE FROM writes",
			"INSERT INTO writes VALUES (0)",
			"CREATE TRIGGER IF NOT EXISTS count_insert AFTER INSERT ON checks BEGIN UPDATE writes SET n=n+1; END",
			"CREATE TRIGGER IF NOT EXISTS count_update AFTER UPDATE ON checks BEGIN UPDATE writes SET n=n+1; END",
			"CREATE TRIGGER IF NOT EXISTS count_delete AFTER DELETE ON checks BEGIN UPDATE writes SET n=n+1; END",
		} {
			if _, err := s.DB.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		return func() int {
			var n int
			if err := s.DB.QueryRow("SELECT n FROM writes").Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	writes := counting(s)
	snap.Checks["b/shell"].Last.Status = "failed"
	if err = s.Save(snap, nil, store.AlertRoute{}); err != nil || writes() != 1 {
		t.Fatal("one changed check should write one row", writes(), err)
	}
	delete(snap.Checks, "c/shell")
	if err = s.Save(snap, nil, store.AlertRoute{}); err != nil || writes() != 2 {
		t.Fatal("a removed check should delete one row", writes(), err)
	}
	// A reopened store knows what is already written.
	s.Close()
	if s, err = store.OpenStore(dir, config.DefaultStorageLimits); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.Load()
	if err != nil || len(loaded.Checks) != 2 || loaded.Checks["b/shell"].Last.Status != "failed" {
		t.Fatal(loaded, err)
	}
	writes = counting(s)
	if err = s.Save(loaded, nil, store.AlertRoute{}); err != nil || writes() != 0 {
		t.Fatal("an unchanged snapshot rewrote check rows", writes(), err)
	}
	// A fleet check is written again only after a state method changed
	// it; its receipt is memory only, and a clone keeps what was written.
	fleetCheck := &incidents.CheckState{CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: "app-1", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	fleetCheck.Touch()
	loaded.Checks[fleet.FleetKey("app-1")] = fleetCheck
	if err = s.Save(loaded, nil, store.AlertRoute{}); err != nil || writes() != 1 {
		t.Fatal("a new fleet check should write one row", writes(), err)
	}
	fleetCheck.Receipt = time.Now()
	clone := loaded.Clone()
	if err = s.Save(clone, nil, store.AlertRoute{}); err != nil || writes() != 1 {
		t.Fatal("an unchanged fleet check was rewritten", writes(), err)
	}
	clone.Checks[fleet.FleetKey("app-1")].Observe(checksandalerts.Result("failed", "app-1 stopped reporting"), checksandalerts.Timing{}, false)
	if err = s.Save(clone, nil, store.AlertRoute{}); err != nil || writes() != 2 {
		t.Fatal("a changed fleet check was not written", writes(), err)
	}
}

func TestSavePrunesOnlyWhenAddingEvents(t *testing.T) {
	t.Parallel()
	s, err := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Save(incidents.FreshSnapshot(), nil, store.AlertRoute{}); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-2 * config.DefaultStorageLimits.KeepEventHistoryFor)
	if _, err = s.DB.Exec("INSERT INTO events(at,data) VALUES(?,?)", expired.UnixNano(), `{"id":"expired"}`); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM events").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err = s.Save(incidents.FreshSnapshot(), nil, store.AlertRoute{}); err != nil || count() != 1 {
		t.Fatal("a save without events pruned history", count(), err)
	}
	if err = s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{{ID: "new", At: time.Now()}}, store.AlertRoute{}); err != nil || count() != 1 {
		t.Fatal("a save that adds events did not enforce retention", count(), err)
	}
}

// A database in another state format is refused rather than partly loaded.
func TestStoreRefusesOtherStateFormat(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	s, e := store.OpenStore(dir, config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	if s, e = store.OpenStore(dir, config.DefaultStorageLimits); e != nil {
		t.Fatal("reopening the current format failed:", e)
	}
	if _, e = s.DB.Exec("PRAGMA user_version=1"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if s, e = store.OpenStore(dir, config.DefaultStorageLimits); e == nil || !strings.Contains(e.Error(), "state format 1") {
		if s != nil {
			s.Close()
		}
		t.Fatal("opened a database in another format:", e)
	}
}

// Event sequence numbers are never reused, so a hub's cursor stays valid
// after local retention; each database has its own history ID.
func TestEventSequenceAndHistory(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.OpenStore(dir, config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	history := s.History
	message := strings.Repeat("x", 1000)
	save := func(ids ...string) {
		t.Helper()
		events := []checksandalerts.Event{}
		for _, id := range ids {
			events = append(events, checksandalerts.Event{ID: id, At: time.Now(), Message: message})
		}
		if err := s.Save(incidents.FreshSnapshot(), events, store.AlertRoute{}); err != nil {
			t.Fatal(err)
		}
	}
	save("a", "b", "c")
	if _, err = s.DB.Exec("DELETE FROM events"); err != nil {
		t.Fatal(err)
	}
	save("d", "e", "f")
	if events, err := s.EventsAfter(0, 10, 0); err != nil || len(events) != 3 || events[0].ID != "d" || events[0].Seq != 4 {
		t.Fatal("a sequence number was reused", events, err)
	}
	if events, err := s.EventsAfter(4, 1, 0); err != nil || len(events) != 1 || events[0].Seq != 5 {
		t.Fatal("count limit", events, err)
	}
	if events, err := s.EventsAfter(3, 10, 2500); err != nil || len(events) != 2 {
		t.Fatal("byte limit", len(events), err)
	}
	s.Close()
	if s, err = store.OpenStore(dir, config.DefaultStorageLimits); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if s.History != history || !fleet.IDPattern.MatchString(history) || other.History == history {
		t.Fatal("history IDs", history, s.History, other.History)
	}
}

// remind_every takes any positive duration, so reminders must not be able to
// fill the outbox: a new reminder replaces the undelivered reminder for the
// same incident and destination, in a node's own queues and in the hub's
// alert intake. Alerts and the reminders of other incidents stay.
func TestNewReminderReplacesTheUndeliveredOne(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	now := time.Now().UTC()
	destinations := map[string]checksandalerts.Destination{
		"oncall": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/oncall"}, TimeLimitPerDeliveryAttempt: "1s", RemindEvery: "1m"},
		"chat":   {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/chat"}, TimeLimitPerDeliveryAttempt: "1s", RemindEvery: "1m"},
	}
	reminder := func(incident, destination string, minute int) checksandalerts.Event {
		return checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "web", Check: "http", Type: "reminder", AlertSeverity: "warn", At: now.Add(time.Duration(minute) * time.Minute), Message: "still failing", Destination: destination, Notify: true}
	}
	queued := func(table, where string) []string {
		t.Helper()
		rows, e := s.DB.Query("SELECT json_extract(event,'$.id') FROM " + table + " WHERE " + where + " ORDER BY seq")
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		ids := []string{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				t.Fatal(e)
			}
			ids = append(ids, id)
		}
		return ids
	}
	failed := checksandalerts.Event{ID: incidents.ID(), Incident: "incident-1", CheckGroup: "web", Check: "http", Type: "failed", AlertSeverity: "warn", At: now, Message: "down", Notify: true}
	first, other, chat := reminder("incident-1", "oncall", 1), reminder("incident-2", "oncall", 1), reminder("incident-1", "chat", 1)
	second, third := reminder("incident-1", "oncall", 2), reminder("incident-1", "oncall", 3)
	for _, routes := range []struct {
		route store.AlertRoute
		queue string
	}{{store.AlertRoute{Destinations: destinations}, "oncall"}, {store.AlertRoute{Joined: true, Destinations: destinations}, config.HubDestination}} {
		if _, e = s.DB.Exec("DELETE FROM outbox"); e != nil {
			t.Fatal(e)
		}
		for _, events := range [][]checksandalerts.Event{{failed, first, other, chat}, {second}, {third}} {
			if e = s.Save(incidents.FreshSnapshot(), events, routes.route); e != nil {
				t.Fatal(e)
			}
		}
		// In the queue named oncall, or in the joined node's queue for the
		// hub: the failure, the other incident's reminder, and the newest
		// reminder of incident-1 for each destination it names.
		want := []string{failed.ID, other.ID, third.ID}
		if routes.route.Joined {
			want = []string{failed.ID, other.ID, chat.ID, third.ID}
		}
		if got := queued("outbox", "destination='"+routes.queue+"'"); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: queued %v, want %v", routes.queue, got, want)
		}
	}
	// The hub's alert intake keeps one undelivered reminder per node,
	// incident, and destination.
	for _, event := range []checksandalerts.Event{failed, first, other, chat, second, third} {
		if e = s.Accept("app-1", event, now); e != nil {
			t.Fatal(e)
		}
	}
	if e = s.Accept("app-2", first, now); e != nil {
		t.Fatal(e)
	}
	if got := queued("intake", "node='app-1'"); !reflect.DeepEqual(got, []string{failed.ID, other.ID, chat.ID, third.ID}) {
		t.Fatal(got)
	}
	if got := queued("intake", "node='app-2'"); !reflect.DeepEqual(got, []string{first.ID}) {
		t.Fatal("another node's reminder was replaced", got)
	}
}

// A destination whose oldest undelivered alert has a failed attempt is
// listed with the number of alerts that wait for it, the error of the
// latest attempt on one line, and the time of the next attempt. A
// destination that only has alerts queued is not listed, and neither is
// one after its oldest alert was delivered.
func TestFailingDestinationsAreListed(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	webhook := func(url string) checksandalerts.Destination {
		return checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: url}, TimeLimitPerDeliveryAttempt: "1s"}
	}
	route := store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"team_chat": webhook("https://chat.example.com"), "operations": webhook("https://alerts.example.com"), "quiet": webhook("https://quiet.example.com")}}
	now := time.Now()
	events := []checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), At: now, Type: "failed", Notify: true}, {AlertSeverity: "warn", ID: incidents.ID(), At: now, Type: "recovered", Notify: true}}
	if e = s.Save(incidents.FreshSnapshot(), events, route); e != nil {
		t.Fatal(e)
	}
	if failing, e := s.FailingDestinations(""); e != nil || failing == nil || len(failing) != 0 {
		t.Fatal("alerts that only wait are not a failing destination:", failing, e)
	}
	// oldest returns the oldest undelivered alert of a destination.
	oldest := func(destination string) store.Delivery {
		t.Helper()
		pending, e := s.Pending()
		if e != nil {
			t.Fatal(e)
		}
		for _, d := range pending {
			if d.Destination == destination {
				return d
			}
		}
		t.Fatal("nothing queued for", destination)
		return store.Delivery{}
	}
	before := time.Now()
	if _, e = s.Delivered(oldest("team_chat"), errors.New("the module slack reported:\nSlack answered 404: the webhook\tdoes not exist")); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Delivered(oldest("operations"), errors.New("webhook returned HTTP 503")); e != nil {
		t.Fatal(e)
	}
	failing, e := s.FailingDestinations("")
	if e != nil || len(failing) != 2 {
		t.Fatal(failing, e)
	}
	for i, want := range []store.FailingDestination{
		{Destination: "operations", UndeliveredAlerts: 2, LatestAttemptError: "webhook returned HTTP 503"},
		{Destination: "team_chat", UndeliveredAlerts: 2, LatestAttemptError: "the module slack reported: Slack answered 404: the webhook does not exist"},
	} {
		got := failing[i]
		// The first retry comes a second after the failed attempt.
		if wait := got.NextAttemptAt.Sub(before); wait < 900*time.Millisecond || wait > 5*time.Second {
			t.Errorf("%s: the next attempt is %s after the failed one", got.Destination, wait)
		}
		if got.OldestUndeliveredAlertQueuedAt.Before(now) || got.OldestUndeliveredAlertQueuedAt.After(before) {
			t.Fatal("lost oldest queued time", got)
		}
		got.NextAttemptAt, got.OldestUndeliveredAlertQueuedAt = time.Time{}, time.Time{}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	}
	if failing, e = s.FailingDestinations("operations"); e != nil || len(failing) != 1 || failing[0].Destination != "team_chat" {
		t.Fatal("the destination to leave out is listed:", failing, e)
	}
	// Once the oldest alert is delivered, the next one has no failed
	// attempt, so the destination is no longer failing.
	if _, e = s.Delivered(oldest("operations"), nil); e != nil {
		t.Fatal(e)
	}
	if failing, e = s.FailingDestinations(""); e != nil || len(failing) != 1 || failing[0].Destination != "team_chat" {
		t.Fatal(failing, e)
	}

	// On a joined node the queue for the MMWatchdog fleet hub is listed under the
	// name hub.
	joined, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer joined.Close()
	if e = joined.Save(incidents.FreshSnapshot(), events[:1], store.AlertRoute{Joined: true}); e != nil {
		t.Fatal(e)
	}
	pending, e := joined.Pending()
	if e != nil || len(pending) != 1 {
		t.Fatal(pending, e)
	}
	if _, e = joined.Delivered(pending[0], errors.New("the hub did not answer")); e != nil {
		t.Fatal(e)
	}
	if failing, e = joined.FailingDestinations(""); e != nil || len(failing) != 1 || failing[0].Destination != config.HubDestination || failing[0].UndeliveredAlerts != 1 {
		t.Fatal(failing, e)
	}
}

// The line of a failing destination says how many alerts wait, when MMWatchdog
// tries again, and last the error.
func TestFailingDestinationIsDescribedInOneLine(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 29, 14, 5, 30, 0, time.UTC)
	for _, tc := range []struct {
		failing store.FailingDestination
		want    string
	}{
		{store.FailingDestination{Destination: "team_chat", UndeliveredAlerts: 3, LatestAttemptError: "the module slack reported: Slack answered 404: the webhook does not exist", NextAttemptAt: at},
			"Destination team_chat: 3 alerts are not delivered. MMWatchdog tries again at 14:05 UTC. The latest attempt failed: the module slack reported: Slack answered 404: the webhook does not exist"},
		{store.FailingDestination{Destination: "on_call_mailbox", UndeliveredAlerts: 1, LatestAttemptError: "the mail server smtp.example.com:587 refused the login of mmwatchdog@example.com: 535 5.7.8 the name or the password is wrong", NextAttemptAt: at.In(time.FixedZone("EEST", 3*3600))},
			"Destination on_call_mailbox: 1 alert is not delivered. MMWatchdog tries again at 14:05 UTC. The latest attempt failed: the mail server smtp.example.com:587 refused the login of mmwatchdog@example.com: 535 5.7.8 the name or the password is wrong"},
		{store.FailingDestination{Destination: config.HubDestination, UndeliveredAlerts: 2, LatestAttemptError: "the hub\ndid not answer", NextAttemptAt: at},
			"The MMWatchdog fleet hub has not accepted 2 alerts. MMWatchdog tries again at 14:05 UTC. The latest attempt failed: the hub did not answer"},
		{store.FailingDestination{Destination: config.HubDestination, UndeliveredAlerts: 1, LatestAttemptError: "the hub did not answer", NextAttemptAt: at},
			"The MMWatchdog fleet hub has not accepted 1 alert. MMWatchdog tries again at 14:05 UTC. The latest attempt failed: the hub did not answer"},
	} {
		if got := tc.failing.Describe(); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
	shown := store.FailingDestination{Destination: "team_chat", UndeliveredAlerts: 1, LatestAttemptError: "webhook returned HTTP 503", NextAttemptAt: at}
	if !shown.ShowsError("webhook returned\nHTTP 503") || shown.ShowsError("another error") {
		t.Error("the latest error of the delivery summary is not matched to the destination's line")
	}
}

// The line of the notification queue says how many deliveries were dropped,
// and when, so a reader can tell an old drop from a new one.
func TestDeliverySummaryIsDescribedInOneLine(t *testing.T) {
	t.Parallel()
	first, newest := time.Date(2026, 10, 25, 9, 10, 0, 0, time.UTC), time.Date(2026, 10, 27, 17, 52, 30, 0, time.FixedZone("EET", 2*3600))
	for _, tc := range []struct {
		delivery store.DeliverySummary
		want     string
	}{
		{store.DeliverySummary{Pending: 3}, "Alert deliveries: 3 waiting, 0 dropped"},
		{store.DeliverySummary{Gaps: 1, FirstGapAt: newest, NewestGapAt: newest}, "Alert deliveries: 0 waiting, 1 dropped at 2026-10-27 15:52 UTC"},
		{store.DeliverySummary{Gaps: 5, FirstGapAt: newest, NewestGapAt: newest}, "Alert deliveries: 0 waiting, 5 dropped at 2026-10-27 15:52 UTC"},
		{store.DeliverySummary{Pending: 2, Gaps: 3, FirstGapAt: first, NewestGapAt: newest}, "Alert deliveries: 2 waiting, 3 dropped since 2026-10-25 09:10 UTC, the newest at 2026-10-27 15:52 UTC"},
		// A database from before the times were kept knows the newest from
		// its event history at most.
		{store.DeliverySummary{Gaps: 1, NewestGapAt: newest}, "Alert deliveries: 0 waiting, 1 dropped at 2026-10-27 15:52 UTC"},
		{store.DeliverySummary{Gaps: 3, NewestGapAt: newest}, "Alert deliveries: 0 waiting, 3 dropped, the newest at 2026-10-27 15:52 UTC"},
		{store.DeliverySummary{Gaps: 3}, "Alert deliveries: 0 waiting, 3 dropped (the time is not recorded)"},
	} {
		if got := tc.delivery.Describe(); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
}

// Each drop keeps the time of the first and of the newest drop beside the
// count, and a database from before those times were kept takes the newest
// from its event history when it opens.
func TestDroppedDeliveriesKeepTheirTimes(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	limits := config.DefaultStorageLimits
	limits.UndeliveredAlertsBytes = 16 * 1024
	s, e := store.OpenStore(dir, limits)
	if e != nil {
		t.Fatal(e)
	}
	route := store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, TimeLimitPerDeliveryAttempt: "1s"}}}
	overflow := func() store.DeliverySummary {
		events := []checksandalerts.Event{}
		for i := 0; i < 300; i++ {
			events = append(events, checksandalerts.Event{AlertSeverity: "warn", ID: incidents.ID(), At: time.Now(), Notify: true})
		}
		if e := s.Save(incidents.FreshSnapshot(), events, route); e != nil {
			t.Fatal(e)
		}
		stats, e := s.DeliveryStats()
		if e != nil {
			t.Fatal(e)
		}
		return stats
	}
	start := time.Now()
	dropped := overflow()
	if dropped.Gaps == 0 || dropped.FirstGapAt.Before(start) || !dropped.NewestGapAt.Equal(dropped.FirstGapAt) {
		t.Fatal("the first drop has no time", dropped)
	}
	later := overflow()
	if later.Gaps <= dropped.Gaps || !later.FirstGapAt.Equal(dropped.FirstGapAt) || !later.NewestGapAt.After(dropped.NewestGapAt) {
		t.Fatal("a later drop must move the newest time and keep the first", dropped, later)
	}
	if _, e = s.DB.Exec("DELETE FROM counters WHERE name IN ('first_delivery_gap_at','newest_delivery_gap_at')"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if s, e = store.OpenStore(dir, limits); e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	old, e := s.DeliveryStats()
	if e != nil || old.Gaps != later.Gaps || !old.FirstGapAt.IsZero() || !old.NewestGapAt.Equal(later.NewestGapAt) {
		t.Fatal("an older database must take the newest drop from its event history", later, old, e)
	}
}

// A delivery gap that retention records has the time of the system clock.
// Retention counts back from its own clock, which a forward step of the
// system clock leaves behind it (Daemon.retentionNow); the gap still says
// when it happened.
func TestARetentionGapHasTheTimeOfTheSystemClock(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	// The retention clock is an hour behind the system clock, as an hour
	// after a step of the system clock forward by an hour.
	behind := func() time.Time { return time.Now().Add(-time.Hour) }
	s.RetentionNow = behind
	destinations := map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, TimeLimitPerDeliveryAttempt: "5s"}}
	// The alert is older than keep_undelivered_alerts_for also on the
	// retention clock, so the commit that queues it removes it.
	old := checksandalerts.Event{AlertSeverity: "warn", ID: "old", At: time.Now().Add(-config.DefaultStorageLimits.KeepUndeliveredAlertsFor - 2*time.Hour), Notify: true}
	if e = s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{old}, store.AlertRoute{Destinations: destinations}); e != nil {
		t.Fatal(e)
	}
	events, e := s.Events(10)
	if e != nil {
		t.Fatal(e)
	}
	for _, event := range events {
		if event.Type == "delivery_gap" {
			if off := time.Since(event.At); off < -time.Minute || off > time.Minute {
				t.Fatalf("the delivery gap is dated %s, %s off the system clock", event.At, off)
			}
			return
		}
	}
	t.Fatal("retention recorded no delivery gap for the expired alert", events)
}

// The age limits of a commit, of the hub's alert grouping step, and of the
// alerts count back from Store.RetentionNow, the
// daemon's retention clock, and not from the system clock. Here that clock
// is two days ahead, so an alert of now is older than every limit.
func TestTheStoreCountsItsAgeLimitsFromItsRetentionClock(t *testing.T) {
	t.Parallel()
	open := func(t *testing.T) *store.Store {
		t.Helper()
		s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	twoDaysAhead := func() time.Time { return time.Now().Add(48 * time.Hour) }
	destinations := map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, TimeLimitPerDeliveryAttempt: "5s"}}
	alert := func(id string) checksandalerts.Event {
		return checksandalerts.Event{AlertSeverity: "warn", ID: id, CheckGroup: "app", Check: "http", Type: "failed", At: time.Now(), Notify: true}
	}
	queued := func(t *testing.T, s *store.Store, id string) bool {
		t.Helper()
		var n int
		if e := s.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE json_extract(event,'$.id')=?", id).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n > 0
	}
	t.Run("a commit", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		s.RetentionNow = twoDaysAhead
		if e := s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{alert("now")}, store.AlertRoute{Destinations: destinations}); e != nil {
			t.Fatal(e)
		}
		if queued(t, s, "now") {
			t.Fatal("the commit's age limit counted from the system clock")
		}
	})
	t.Run("the alert grouping step", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		if e := s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{alert("queued")}, store.AlertRoute{Destinations: destinations}); e != nil || !queued(t, s, "queued") {
			t.Fatal("the alert was not queued", e)
		}
		if e := s.Accept("app-1", alert("from-a-node"), time.Now()); e != nil {
			t.Fatal(e)
		}
		rows, e := s.Intake()
		if e != nil || len(rows) != 1 {
			t.Fatal(rows, e)
		}
		s.RetentionNow = twoDaysAhead
		if e = s.Emit(store.AlertGroupingPlan{Messages: []store.AlertGroupMessage{{Members: []int64{rows[0].Seq}}}}, destinations, false, time.Now()); e != nil {
			t.Fatal(e)
		}
		if queued(t, s, "queued") {
			t.Fatal("the alert grouping step's age limit counted from the system clock")
		}
	})

}

func TestCredentialChangesCancelQueuedDeliveriesAfterRestart(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.OpenStore(dir, config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	destination := checksandalerts.Destination{Inputs: &checksandalerts.ModuleCall{Name: "pushover", Inputs: map[string]any{"user_key_file": "/etc/mmwatchdog/secrets/key"}}, RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities}
	destination.CaptureCredentialContent(map[string]string{"user_key_file": "original-credential"})
	route := store.AlertRoute{Destinations: map[string]checksandalerts.Destination{"chat": destination}, DestinationHealth: map[string]store.DestinationHealth{"chat": {Target: destination, FailingNoticeSent: true}}}
	snapshot := incidents.FreshSnapshot()
	if err := s.Save(snapshot, []checksandalerts.Event{{ID: "queued", Type: "notice", AlertSeverity: "warn", At: time.Now(), Notify: true}}, route); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.OpenStore(dir, config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending()
	if err != nil || len(pending) != 1 || !checksandalerts.SameDestination(pending[0].Target, destination) {
		t.Fatal("restart lost the queued receiver", pending, err)
	}
	changed := destination
	changed.CaptureCredentialContent(map[string]string{"user_key_file": "rotated-credential"})
	if checksandalerts.SameDestination(pending[0].Target, changed) {
		t.Fatal("restart lost the credential comparison for queued delivery")
	}
	health, err := s.DestinationHealth()
	if err != nil || !health["chat"].FailingNoticeSent || !checksandalerts.SameDestination(health["chat"].Target, destination) || checksandalerts.SameDestination(health["chat"].Target, changed) {
		t.Fatal("restart lost the credential comparison for destination health", err)
	}
	route.Destinations["chat"] = changed
	if err := s.Save(snapshot, nil, route); err != nil {
		t.Fatal(err)
	}
	pending, err = s.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatal("credential rotation kept the queued delivery after restart", pending, err)
	}
}

// A machine without a host name cannot run MMWatchdog: the node is named by
// its host name.
func TestTheStoreRefusesAMachineWithoutAHostName(t *testing.T) {
	real := store.SetHostNameForTest(func() (string, error) { return "", nil })
	defer func() { store.SetHostNameForTest(real) }()
	_, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e == nil || !strings.Contains(e.Error(), "MMWatchdog cannot run: this machine has no host name, and MMWatchdog names the node by it: set a host name with hostnamectl set-hostname NAME") {
		t.Fatal(e)
	}
}
