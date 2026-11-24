package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

// failWhileStateDBCannotBeWritten makes every commit of d fail, as on a full
// disk, and lets the check app/shell fail. It returns the failure's alert,
// which waits in d.pending.
func failWhileStateDBCannotBeWritten(t *testing.T, d *Daemon) checksandalerts.Event {
	t.Helper()
	if _, e := d.store.DB.Exec("CREATE TRIGGER reject_snapshot BEFORE UPDATE ON snapshot BEGIN SELECT RAISE(FAIL, 'fixture disk full'); END"); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	events := d.snapshot.Checks["app/shell"].Observe(checksandalerts.Observation{Status: "failed", At: d.now(), Message: "app down"}, checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, false)
	if e := d.record(events); e == nil {
		t.Fatal("the commit succeeded")
	}
	for _, event := range events {
		if event.Type == "failed" && event.Notify {
			return event
		}
	}
	t.Fatal("no failure alert", events)
	return checksandalerts.Event{}
}

// saveAgain lets d's commits succeed again and commits what waits.
func saveAgain(t *testing.T, d *Daemon) {
	t.Helper()
	if _, e := d.store.DB.Exec("DROP TRIGGER reject_snapshot"); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.record(nil); e != nil {
		t.Fatal(e)
	}
}

// While state.db cannot be written, the alert that waits for a commit goes
// straight from memory, once, to the destinations that take it, as the
// outbox would send it. The commit that succeeds later queues it, and it
// goes out again: a duplicate alert is better than a dropped one.
func TestAlertsGoFromMemoryWhileStateDBCannotBeWritten(t *testing.T) {
	t.Parallel()
	ops, sosOnly, titleOnly := captureAlerts(t), captureAlerts(t), captureAlerts(t)
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{
		"ops":        {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: ops.URL}, TimeLimitPerDeliveryAttempt: "1m"},
		"sos_only":   {RemindEvery: "never", AlertSeverities: []string{"sos"}, Inputs: &checksandalerts.Webhook{URL: sosOnly.URL}, TimeLimitPerDeliveryAttempt: "1m"},
		"title_only": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, SendOnlyTheAlertTitle: true, Inputs: &checksandalerts.Webhook{URL: titleOnly.URL}, TimeLimitPerDeliveryAttempt: "1m"},
	}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	failure := failWhileStateDBCannotBeWritten(t, d)
	if failure.AlertSeverity == "sos" {
		t.Fatal("the fixture needs an alert that sos_only does not take", failure)
	}
	runWorker(t, d)
	got, short := ops.wait(t, 1)[0], titleOnly.wait(t, 1)[0]
	if got.ID != failure.ID || got.Title == "" || got.Body == "" || got.Node != d.store.Node || outboxCount(t, d, "ops") != 0 {
		t.Fatal("the alert from memory", got)
	}
	if short.ID != failure.ID || short.Title != got.Title || short.Body != "" {
		t.Fatal("send_only_the_alert_title", short)
	}
	// Many reads of the queue later, nothing went twice, and nothing went
	// to the destination that takes only sos.
	time.Sleep(20 * readNotificationQueueEvery)
	if len(ops.received()) != 1 || len(titleOnly.received()) != 1 || len(sosOnly.received()) != 0 {
		t.Fatal("sent more than once, or to a destination that does not take the alert", ops.received(), titleOnly.received(), sosOnly.received())
	}
	saveAgain(t, d)
	committed, e := d.store.Events(100)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, event := range committed {
		found = found || event.ID == failure.ID
	}
	if !found {
		t.Fatal("the commit did not keep the alert", committed)
	}
	if again := ops.wait(t, 2)[1]; again.ID != failure.ID {
		t.Fatal("the outbox's delivery", again)
	}
}

// A joined node sends an alert from memory to the hub. When the hub does
// not accept it within send_alert_without_hub_after_waiting_for, the node
// sends it straight to the destinations of its copy of the fleet settings,
// as the outbox's fallback does.
func TestAJoinedNodeSendsFromMemoryThroughTheHubOrWithoutIt(t *testing.T) {
	t.Parallel()
	for _, hubAnswers := range []bool{true, false} {
		name := "the hub accepts the alert"
		if !hubAnswers {
			name = "the hub cannot be reached"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dest := captureAlerts(t)
			hub, code, srv := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1m"}})
			node := joinedNode(t, code, "app-1")
			node.report(context.Background())
			if node.fleetSettings == nil {
				t.Fatal("the node has no copy of the fleet settings")
			}
			node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms" // below what configuration allows
			if hubAnswers {
				runWorker(t, hub)
			} else {
				srv.Close()
			}
			failure := failWhileStateDBCannotBeWritten(t, node)
			runWorker(t, node)
			got := dest.wait(t, 1)[0]
			if got.ID != failure.ID || got.Node != "app-1" || outboxCount(t, node, config.HubDestination) != 0 {
				t.Fatal("the alert from memory", got)
			}
			var intake int
			if e := hub.store.DB.QueryRow("SELECT COUNT(*) FROM intake WHERE node='app-1' AND id=?", failure.ID).Scan(&intake); e != nil {
				t.Fatal(e)
			}
			if (intake == 1) != hubAnswers {
				t.Fatal("the hub's intake has the alert", intake, "times")
			}
		})
	}
}

// A joined node's direct send from memory that a destination keeping
// incidents accepts is recorded for the node's reports in the first commit
// that succeeds, as the outbox's acceptance records it. That commit
// routes the alert itself to the hub; without the record, the hub would
// never learn of the direct send and never close the service's incident
// that it opened.
func TestADirectSendFromMemoryIsRecordedInTheFirstCommit(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	_, code, srv := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1m"}})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	if node.fleetSettings == nil {
		t.Fatal("the node has no copy of the fleet settings")
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms" // below what configuration allows
	node.fleetSettings.DestinationsWhoseServiceKeepsIncidents = []string{"ops"}
	srv.Close()
	failure := failWhileStateDBCannotBeWritten(t, node)
	runWorker(t, node)
	if got := dest.wait(t, 1)[0]; got.ID != failure.ID {
		t.Fatal("the alert from memory", got)
	}
	// The worker notes the acceptance once the send has returned.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		node.mu.Lock()
		r := node.fromMemory[memoryDeliveryKey{failure.ID, "ops"}]
		done := r != nil && r.done
		node.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the delivery from memory does not finish")
		}
	}
	saveAgain(t, node)
	records, e := node.store.SentWithoutTheHub()
	if e != nil || len(records) != 1 || records[0].Destination != "ops" || records[0].Incident != failure.Incident {
		t.Fatal("the first commit does not record the direct send", records, e)
	}
	if outboxCount(t, node, config.HubDestination) != 1 {
		t.Fatal("the commit does not route the alert to the hub")
	}
}

// A destination module that panics fails its attempt; the worker goes on.
func TestADeliveryThatPanicsIsAFailedAttempt(t *testing.T) {
	t.Parallel()
	e := send(context.Background(), panickingSender{}, checksandalerts.Event{ID: "x"})
	if e == nil || e.Error() != "the destination's module stopped with an internal error: a fixture bug second line" {
		t.Fatal(e)
	}
}

type panickingSender struct{}

func (panickingSender) Send(context.Context, checksandalerts.Event) error {
	panic("a fixture bug\nsecond line")
}

// runThatPanics is the module of a check of a test whose run panics, as a
// bug in a core module would.
type runThatPanics struct{ runs *atomic.Int32 }

func (w runThatPanics) Validate(checksandalerts.InputValidation) *checksandalerts.FieldError {
	return nil
}
func (w runThatPanics) Run(context.Context, *checksandalerts.CheckRun) checksandalerts.Observation {
	w.runs.Add(1)
	panic("index out of range [3] with length 3\ngoroutine 7 [running]")
}

// A check whose module panics is unknown, with the panic named on one
// line, and its next run still comes: the node monitoring process goes on.
func TestACheckWhoseModulePanicsIsUnknown(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups["app"] = checksandalerts.CheckGroup{Timing: checksandalerts.Timing{CheckEvery: "100ms", TimeLimitPerRun: "80ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	runs := &atomic.Int32{}
	d.checks["app/shell"].def.Check.Inputs = runThatPanics{runs}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.workers.Wait() }()
	for want := int32(1); want <= 2; want++ {
		runOnce(t, ctx, d, "app/shell")
		o := lastObservation(d, "app/shell")
		if runs.Load() != want || o.Status != "unknown" || o.Message != "the check's module stopped with an internal error: index out of range [3] with length 3 goroutine 7 [running]. The journal of mmwatchdog shows where" || o.At.IsZero() {
			t.Fatalf("run %d: %d runs, %+v", want, runs.Load(), o)
		}
	}
}

// An alert that waits for the scheduler's next commit, with state.db
// writable (the hub closing a silent node's alert on a node's report, a
// join, or a forget), is not sent from memory: the commit queues it, and
// sending it from memory too would send it twice.
func TestAnAlertWaitingForTheNextCommitIsNotSentFromMemoryWhileStateDBCanBeWritten(t *testing.T) {
	t.Parallel()
	ops := captureAlerts(t)
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{
		"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: ops.URL}, TimeLimitPerDeliveryAttempt: "1m"},
	}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	d.mu.Lock()
	d.pending = append(d.pending, d.snapshot.Checks["app/shell"].Observe(checksandalerts.Observation{Status: "failed", At: d.now(), Message: "app down"}, checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, false)...)
	d.mu.Unlock()
	runWorker(t, d)
	time.Sleep(20 * readNotificationQueueEvery)
	if got := ops.received(); len(got) != 0 {
		t.Fatal("sent from memory while state.db can be written", got)
	}
	d.mu.Lock()
	e = d.record(nil)
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	ops.wait(t, 1)
	time.Sleep(20 * readNotificationQueueEvery)
	if got := ops.received(); len(got) != 1 {
		t.Fatal("expected one delivery", got)
	}
}
