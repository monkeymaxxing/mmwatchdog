package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/testhelpers"
)

// The tests of this file make the system clock jump with
// testhelpers.Clock, as a time service steps it, and check that MMWatchdog's waits
// count on the monotonic clock: a backward jump holds back no check run,
// confirmation, delivery retry, fallback, or silence alert, and a forward
// jump ends no wait early.

// runThatCounts is a check module that passes at once and counts its runs.
type runThatCounts struct{ runs *atomic.Int32 }

func (r runThatCounts) Validate(checksandalerts.InputValidation) *checksandalerts.FieldError {
	return nil
}
func (r runThatCounts) Run(context.Context, *checksandalerts.CheckRun) checksandalerts.Observation {
	r.runs.Add(1)
	return checksandalerts.Result("ok", "the test counts this run")
}

// newDaemonOnClock starts a daemon of the configuration in path whose clock
// is clock.
func newDaemonOnClock(t *testing.T, path string, clock *testhelpers.Clock) *Daemon {
	t.Helper()
	d, e := newDaemon(path, clock.Now)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// A backward jump holds back no check run: the next run is due a check
// interval after the last one on the monotonic clock. The scheduler runs as
// in MMWatchdog, from Run.
func TestABackwardClockJumpHoldsBackNoCheckRun(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	// Keep the Unix socket below macOS's path-length limit.
	c.Socket = filepath.Join(os.TempDir(), "mmwatchdog-"+incidents.ID()+".sock")
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Timing: checksandalerts.Timing{CheckEvery: "1h", TimeLimitPerRun: "1m", AlertAfterFailingFor: "1h", RecoveredAfterHealthyFor: "1h"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:9/"}}}}
	writeConfig(t, path, c)
	clock := testhelpers.NewClock()
	d := newDaemonOnClock(t, path, clock)
	var runs atomic.Int32
	d.checks["app/http"].def.Check.Inputs = runThatCounts{&runs}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	testhelpers.WaitFor(t, "the first run", func() bool { return runs.Load() == 1 })
	clock.Pass(time.Hour)
	clock.Jump(-time.Hour)
	testhelpers.WaitFor(t, "the second run, an hour after the first on the monotonic clock, while the wall clock shows the time of the first", func() bool { return runs.Load() == 2 })
}

// A backward jump holds back no confirmation: the incident opens once the
// check has failed for alert_after_failing_for on the monotonic clock. A
// maintenance command between the runs builds the new snapshot on a clone,
// which keeps the start of the failing time on the monotonic clock.
func TestABackwardClockJumpHoldsBackNoConfirmation(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"app":   {Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "30s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "1m"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:9/"}}},
		"other": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}},
	}
	writeConfig(t, path, c)
	clock := testhelpers.NewClock()
	d := newDaemonOnClock(t, path, clock)
	var failing atomic.Bool
	failing.Store(true)
	d.checks["app/http"].def.Check.Inputs = runThatAnswersAtOnce{&failing}
	tick(d, d.now())
	setMaintenance(t, d, "other", "1h")
	clock.Jump(-time.Hour)
	for range 2 {
		clock.Pass(time.Minute)
		tick(d, d.now())
	}
	d.mu.Lock()
	st := *d.snapshot.Checks["app/http"]
	d.mu.Unlock()
	if st.Last.Status != "failed" || st.FailureIncident.Incident == "" {
		t.Fatalf("the incident did not open after 2 minutes of failed runs on the monotonic clock; the failing time started at %s, the latest run was at %s", st.FailingSince, st.Last.At)
	}
}

// A backward jump holds back no retry of a delivery: an alert whose first
// attempt failed is tried again a second later on the monotonic clock. The
// clock jumps back while the attempt is under way and again once the
// worker has set its wait after the attempt, so the wait counts on the
// monotonic clock whenever the clock jumps, and the alert is not tried
// again before that second.
func TestABackwardClockJumpHoldsBackNoDeliveryRetry(t *testing.T) {
	t.Parallel()
	clock := testhelpers.NewClock()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			clock.Jump(-time.Hour)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	path, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: server.URL}, TimeLimitPerDeliveryAttempt: "1m"}}
	writeConfig(t, path, c)
	d := newDaemonOnClock(t, path, clock)
	d.mu.Lock()
	e := d.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: d.now(), Message: "the test's alert", Notify: true}})
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitForFinishedAttempts(t, d, "ops", 1)
	clock.Jump(-time.Hour)
	// No time passes on the clock, so the worker must not try again. A
	// short wait proves that it does not.
	time.Sleep(200 * time.Millisecond)
	if n := requests.Load(); n != 1 {
		t.Fatal("the alert was tried again before the delay of a second after its failed attempt:", n, "requests")
	}
	clock.Pass(2 * time.Second)
	testhelpers.WaitFor(t, "the second attempt, a second after the first on the monotonic clock", func() bool { return requests.Load() == 2 })
}

// After a restart with the clock behind the time that the outbox keeps
// for a delivery's next attempt, the wait for that attempt is no longer
// than the doubling delay of the delivery's attempts: a second after one
// failed attempt. So a server whose clock battery has run out, and on
// which MMWatchdog starts before the time service steps the clock right, does not
// hold back its alert for the time that the outbox's time is off. On a
// joined node such a wait would hold back the fallback too: the hub gets
// no attempt, and so none fails.
func TestARestartWithTheClockBehindHoldsBackNoDeliveryRetryLongerThanItsDelay(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	path, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: server.URL}, TimeLimitPerDeliveryAttempt: "1m"}}
	writeConfig(t, path, c)
	clock := testhelpers.NewClock()
	d, e := newDaemon(path, clock.Now)
	if e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	e = d.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: d.now(), Message: "the test's alert", Notify: true}})
	d.mu.Unlock()
	if e != nil {
		d.Close()
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	waitForFinishedAttempts(t, d, "ops", 1)
	cancel()
	<-done
	d.Close()
	// The server boots a year behind, and MMWatchdog starts before the time
	// service has stepped the clock.
	clock.Jump(-365 * 24 * time.Hour)
	d = newDaemonOnClock(t, path, clock)
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	testhelpers.WaitFor(t, "the worker's first look at the alert", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, seen := d.nextAttempts["ops"]
		return seen
	})
	// The time service steps the clock right, and a second passes.
	clock.Jump(365 * 24 * time.Hour)
	clock.Pass(time.Second)
	testhelpers.WaitFor(t, "the second attempt, a second after the restart on the monotonic clock", func() bool { return requests.Load() == 2 })
}

// waitForFinishedAttempts waits until the notifications worker of d has
// finished n attempts to deliver to destination, and has recorded when it
// tries again.
func waitForFinishedAttempts(t *testing.T, d *Daemon, destination string, n int) {
	t.Helper()
	testhelpers.WaitFor(t, fmt.Sprintf("attempt %d to deliver to %s, finished", n, destination), func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.deliveriesFinished[destination] == n
	})
}

// A backward jump holds back no silence alert on the hub: a node that has
// not reported for three report intervals on the monotonic clock is
// silent, whatever the wall clock shows.
func TestABackwardClockJumpHoldsBackNoSilenceAlert(t *testing.T) {
	t.Parallel()
	clock := testhelpers.NewClock()
	hub, code, _ := startHubOnClock(t, nil, nil, clock.Now)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	tick(hub, hub.now())
	clock.Pass(3 * reportInterval)
	clock.Jump(-time.Hour)
	tick(hub, hub.now())
	if alerts := silenceAlerts(t, hub); len(alerts) != 1 || !strings.HasPrefix(alerts[0], "failed app-1: app-1 has sent no report to the hub since ") {
		t.Fatal("no silence alert three report intervals after the node's last report on the monotonic clock:", alerts)
	}
}

// A forward jump sends no silence alert for a node that reports: the node
// is silent only after three report intervals without a report on the
// monotonic clock. A real run saw this alert after a step of 2.3 seconds,
// for a node that reported every second.
func TestAForwardClockJumpSendsNoSilenceAlertForANodeThatReports(t *testing.T) {
	t.Parallel()
	clock := testhelpers.NewClock()
	hub, code, _ := startHubOnClock(t, nil, nil, clock.Now)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	tick(hub, hub.now())
	clock.Pass(reportInterval)
	clock.Jump(time.Hour)
	tick(hub, hub.now())
	if state := stateInTheFleetView(t, hub, "app-1"); state != "reporting" {
		t.Fatal("the fleet view shows a node that reported a report interval ago as", state)
	}
	node.report(context.Background())
	tick(hub, hub.now())
	if alerts := silenceAlerts(t, hub); len(alerts) != 0 {
		t.Fatal("a forward jump of the clock made a node that reports silent:", alerts)
	}
}

// stateInTheFleetView is the state of the named node in the hub's fleet
// view at the hub's time.
func stateInTheFleetView(t *testing.T, hub *Daemon, name string) string {
	t.Helper()
	node, e := hub.hub.Node(name, hub.now())
	if e != nil {
		t.Fatal(e)
	}
	return node.State
}

// A backward jump holds back no fallback on a joined node: once an attempt
// to reach the hub has failed, an alert goes directly to the destinations
// send_alert_without_hub_after_waiting_for after it happened, on the
// monotonic clock.
func TestABackwardClockJumpHoldsBackNoFallbackToTheDestinations(t *testing.T) {
	t.Parallel()
	_, code, srv := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "1m"}})
	clock := testhelpers.NewClock()
	node := joinedNodeOnClock(t, code, "app-1", clock.Now)
	node.report(context.Background())
	srv.Close()
	node.mu.Lock()
	e := node.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: node.now(), Message: "the test's alert", Notify: true}})
	node.latestHubAttemptError = errors.New("the hub did not answer")
	node.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	node.fallBack(node.now())
	if outboxCount(t, node, config.HubDestination) != 1 {
		t.Fatal("the alert fell back before send_alert_without_hub_after_waiting_for")
	}
	clock.Jump(-time.Hour)
	clock.Pass(config.DefaultFleetTimings.SendAlertWithoutHubAfterWaitingFor.Value())
	node.fallBack(node.now())
	if outboxCount(t, node, config.HubDestination) != 0 || outboxCount(t, node, "ops") != 1 {
		t.Fatal("the alert did not fall back send_alert_without_hub_after_waiting_for after it happened on the monotonic clock")
	}
}

// A backward jump between an alert and the fallback step's first look at
// it holds back no fallback either. The outbox keeps the alert's time on
// the wall clock, so after the jump that time lies an hour ahead; the
// fallback counts from no later than its first look at the alert.
func TestABackwardClockJumpBeforeTheFirstLookOfTheFallbackHoldsBackNoFallback(t *testing.T) {
	t.Parallel()
	_, code, srv := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "1m"}})
	clock := testhelpers.NewClock()
	node := joinedNodeOnClock(t, code, "app-1", clock.Now)
	node.report(context.Background())
	srv.Close()
	node.mu.Lock()
	e := node.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: node.now(), Message: "the test's alert", Notify: true}})
	node.latestHubAttemptError = errors.New("the hub did not answer")
	node.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	clock.Jump(-time.Hour)
	node.fallBack(node.now())
	if outboxCount(t, node, config.HubDestination) != 1 {
		t.Fatal("the alert fell back before send_alert_without_hub_after_waiting_for")
	}
	clock.Pass(config.DefaultFleetTimings.SendAlertWithoutHubAfterWaitingFor.Value())
	node.fallBack(node.now())
	if outboxCount(t, node, config.HubDestination) != 0 || outboxCount(t, node, "ops") != 1 {
		t.Fatal("the alert did not fall back send_alert_without_hub_after_waiting_for after the first look at it, on the monotonic clock")
	}
}

// After a restart, the waits of a maintenance window and of a heartbeat
// count on the monotonic clock too: a backward jump holds back neither the
// window's end nor the heartbeat's deadline.
func TestABackwardClockJumpAfterARestartHoldsBackNoMaintenanceEndAndNoHeartbeatDeadline(t *testing.T) {
	t.Parallel()
	path, c := heartbeatConfig(t)
	c.CheckGroups["backup"] = checksandalerts.CheckGroup{Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: "1h", ExpectedBearerTokenFile: c.CheckGroups["backup"].Heartbeat.ExpectedBearerTokenFile}}
	c.CheckGroups["app"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	writeConfig(t, path, c)
	clock := testhelpers.NewClock()
	d, e := newDaemon(path, clock.Now)
	if e != nil {
		t.Fatal(e)
	}
	setMaintenance(t, d, "app", "1h")
	if code := postHeartbeat(d); code != http.StatusNoContent {
		d.Close()
		t.Fatal("heartbeat", code)
	}
	d.Close()
	d = newDaemonOnClock(t, path, clock)
	clock.Jump(-time.Hour)
	clock.Pass(time.Hour)
	tick(d, d.now())
	d.mu.Lock()
	defer d.mu.Unlock()
	if w := d.snapshot.Window("app"); w != nil {
		t.Fatal("a window of an hour still lasts an hour after its start on the monotonic clock:", w.Describe())
	}
	if st := d.snapshot.Checks["backup/heartbeat"]; st.FailureIncident.Incident == "" {
		t.Fatalf("the heartbeat is not overdue an hour after it was received on the monotonic clock: %+v", st.Last)
	}
}

// A forward jump removes no record early: the retention steps count back
// from the daemon's start plus the time that has passed since then on the
// monotonic clock, when the wall clock shows a later time. An alert that is
// still queued when a time service steps a clock forward by days, as on a
// server whose clock battery has run out, is delivered.
func TestAForwardClockJumpRemovesNoUndeliveredAlert(t *testing.T) {
	t.Parallel()
	var accepting atomic.Bool
	delivered := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !accepting.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var event checksandalerts.Event
		if e := json.NewDecoder(r.Body).Decode(&event); e != nil {
			t.Error(e)
		}
		delivered <- event.ID
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	path, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: server.URL}, TimeLimitPerDeliveryAttempt: "1m"}}
	writeConfig(t, path, c)
	clock := testhelpers.NewClock()
	d := newDaemonOnClock(t, path, clock)
	alert := checksandalerts.Event{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: d.now(), Message: "the test's alert", Notify: true}
	d.mu.Lock()
	e := d.record([]checksandalerts.Event{alert})
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitForFinishedAttempts(t, d, "ops", 1)
	clock.Jump(48 * time.Hour)
	// Both retention steps run: the notifications worker's, and the one of
	// a commit that adds an event.
	d.runRetentionNow <- struct{}{}
	testhelpers.WaitFor(t, "the retention step", func() bool { return len(d.runRetentionNow) == 0 })
	d.mu.Lock()
	e = d.record([]checksandalerts.Event{{ID: incidents.ID(), Type: "recovery_attempts_reset", CheckGroup: "app", At: d.now(), Message: "an event that only the event history keeps"}})
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	var queued int
	if e = d.store.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE json_extract(event,'$.id')=?", alert.ID).Scan(&queued); e != nil || queued != 1 {
		t.Fatal("a forward jump of the clock removed an undelivered alert:", queued, e)
	}
	accepting.Store(true)
	clock.Pass(2 * time.Second)
	select {
	case id := <-delivered:
		if id != alert.ID {
			t.Fatal("delivered", id)
		}
	case <-time.After(testhelpers.LongestWaitForAnEvent):
		t.Fatal("the alert was not delivered after the jump")
	}
}

// Every time of the daemon's state that a wait counts from carries a
// monotonic reading: after a start, a tick, a heartbeat receipt,
// maintenance on and off, a reminder, a recovery command, a restart, a
// reload that adds a heartbeat, and on the hub and a joined node after
// reports, the hub's alert grouping, and a restart of the hub. A time without one would make
// the wait count on the wall clock again, and nothing else would show it.
func TestEveryTimeOfTheSnapshotCarriesAMonotonicReading(t *testing.T) {
	t.Parallel()
	path, c := heartbeatConfig(t)
	ops := c.Alerts.Destinations["ops"]
	ops.RemindEvery = "1m"
	c.Alerts.Destinations["ops"] = ops
	c.CheckGroups["app"] = commandCheck(t, "true", "5s", checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"shell"}, Attempts: "3", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"})
	c.CheckGroups["other"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	writeConfig(t, path, c)
	clock := testhelpers.NewClock()
	var failing atomic.Bool
	failing.Store(true)
	d, e := newDaemon(path, clock.Now)
	if e != nil {
		t.Fatal(e)
	}
	d.checks["app/shell"].def.Check.Inputs = runThatAnswersAtOnce{&failing}
	requireMonotonicReadings(t, d, "a start")
	tick(d, d.now())
	requireMonotonicReadings(t, d, "a tick with a failed run, an incident, and a recovery command")
	if code := postHeartbeat(d); code != http.StatusNoContent {
		t.Fatal("heartbeat", code)
	}
	requireMonotonicReadings(t, d, "a heartbeat receipt")
	setMaintenance(t, d, "other", "1h")
	requireMonotonicReadings(t, d, "maintenance on")
	clock.Pass(time.Minute)
	tick(d, d.now())
	requireMonotonicReadings(t, d, "a tick with a reminder")
	d.Close()
	d = newDaemonOnClock(t, path, clock)
	d.checks["app/shell"].def.Check.Inputs = runThatAnswersAtOnce{&failing}
	requireMonotonicReadings(t, d, "a restart")
	setMaintenance(t, d, "other", "off")
	requireMonotonicReadings(t, d, "maintenance off")
	clock.Pass(time.Minute)
	tick(d, d.now())
	requireMonotonicReadings(t, d, "a tick after the restart")
	d.mu.Lock()
	st, attempts := d.snapshot.Checks["app/shell"], d.snapshot.RecoveryAttemptsByTarget["app"]
	reached := st.FailureIncident.Incident != "" && st.FailureIncident.Evidence != nil && !st.FailureIncident.Reminded["ops"].IsZero() && attempts != nil && !attempts.Last.IsZero() && !d.snapshot.Checks["backup/heartbeat"].Receipt.IsZero()
	d.mu.Unlock()
	if !reached {
		t.Fatalf("the test did not reach the state whose times it checks: %+v %+v %+v", st.FailureIncident, st.Last, attempts)
	}
	// A reload that adds a heartbeat gives it its deadline.
	restoreToken := filepath.Join(t.TempDir(), "restore-token")
	if e = os.WriteFile(restoreToken, []byte("test-restore-heartbeat-token"), 0600); e != nil {
		t.Fatal(e)
	}
	c.CheckGroups["restore"] = checksandalerts.CheckGroup{Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: "1h", ExpectedBearerTokenFile: restoreToken}}
	writeConfig(t, path, c)
	if _, e = d.reload(); e != nil {
		t.Fatal(e)
	}
	requireMonotonicReadings(t, d, "a reload that adds a heartbeat")
	d.mu.Lock()
	added := d.snapshot.Checks["restore/heartbeat"]
	d.mu.Unlock()
	if added == nil || added.Deadline.IsZero() {
		t.Fatal("the reload added no heartbeat with a deadline")
	}

	hub, code, _ := startHubOnClock(t, nil, nil, clock.Now)
	node := joinedNodeOnClock(t, code, "app-1", clock.Now)
	for range 2 {
		node.report(context.Background())
		tick(hub, hub.now())
		tick(node, node.now())
	}
	requireMonotonicReadings(t, hub, "reports on the hub")
	requireMonotonicReadings(t, node, "reports from a joined node")
	if fleetCheck(node, checksandalerts.HubCheck) == nil {
		t.Fatal("the joined node did not become the hub-outage alerter node, whose hub check the test checks")
	}
	// An alert and a maintenance window of the hub's own go through its
	// alert grouping, which remembers when each sent its message and since
	// when each maintenance group has had no node in maintenance.
	hub.mu.Lock()
	e = hub.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: hub.now(), Message: "the test's alert", Notify: true}})
	hub.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	setMaintenance(t, hub, "app", "1h")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hub.notifications(ctx); close(done) }()
	testhelpers.WaitFor(t, "the hub's alert grouping of the alert and of the window", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.alertGroups.Sent) > 0 && len(hub.alertGroups.Quiet) > 0
	})
	cancel()
	<-done
	requireMonotonicReadings(t, hub, "the hub's alert grouping")
	hub.Close()
	hub = newDaemonOnClock(t, hub.path, clock)
	requireMonotonicReadings(t, hub, "a restart of the hub")
	if st := fleetCheck(hub, "app-1"); st == nil || st.Receipt.IsZero() {
		t.Fatal("the restarted hub lost the time of the node's last report")
	}
}

// requireMonotonicReadings fails the test when a time of d's state that a
// wait can count from has no monotonic reading: a time of the snapshot,
// the schedule of a check, and on the hub the memory of its alert grouping
// and the deadline of each member.
func requireMonotonicReadings(t *testing.T, d *Daemon, after string) {
	t.Helper()
	d.mu.Lock()
	found := timesWithoutAMonotonicReading("snapshot", reflect.ValueOf(d.snapshot))
	for key, rc := range d.checks {
		for name, at := range map[string]time.Time{"next": rc.next, "scheduled": rc.scheduled, "finished": rc.finished} {
			if !at.IsZero() && at == at.Round(0) {
				found = append(found, fmt.Sprintf("checks[%s].%s", key, name))
			}
		}
	}
	for name, times := range map[string]map[string]time.Time{"alertGroups.Sent": d.alertGroups.Sent, "alertGroups.Quiet": d.alertGroups.Quiet} {
		for key, at := range times {
			if at == at.Round(0) {
				found = append(found, fmt.Sprintf("%s[%q]", name, key))
			}
		}
	}
	d.mu.Unlock()
	// The fleet store's lock is never held with d.mu.
	if d.hub != nil {
		d.hub.Mu.Lock()
		for name, m := range d.hub.Nodes {
			if m.Deadline == m.Deadline.Round(0) {
				found = append(found, fmt.Sprintf("hub.Nodes[%s].Deadline", name))
			}
		}
		d.hub.Mu.Unlock()
	}
	if len(found) > 0 {
		slices.Sort(found)
		t.Fatalf("after %s, these times have no monotonic reading, so the waits that count from them count on the wall clock: %s", after, strings.Join(found, ", "))
	}
}

// timesWithoutAMonotonicReading names, below path, every time in v that is
// not zero and has no monotonic reading.
func timesWithoutAMonotonicReading(path string, v reflect.Value) []string {
	if v.Type() == reflect.TypeFor[time.Time]() {
		if at := v.Interface().(time.Time); !at.IsZero() && at == at.Round(0) {
			return []string{path}
		}
		return nil
	}
	found := []string{}
	// Only containers can hold time values; scalar values need no traversal.
	//exhaustive:ignore
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			found = append(found, timesWithoutAMonotonicReading(path, v.Elem())...)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if field := v.Type().Field(i); field.IsExported() {
				found = append(found, timesWithoutAMonotonicReading(path+"."+field.Name, v.Field(i))...)
			}
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			found = append(found, timesWithoutAMonotonicReading(fmt.Sprintf("%s[%v]", path, key), v.MapIndex(key))...)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			found = append(found, timesWithoutAMonotonicReading(fmt.Sprintf("%s[%d]", path, i), v.Index(i))...)
		}
	}
	return found
}
