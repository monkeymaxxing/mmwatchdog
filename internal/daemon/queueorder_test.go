package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// When a destination is rate-limited (429, Retry-After), its wait holds for
// the destination as a whole: an sos alert that goes ahead of the warn
// alert waiting to be retried, of another key or of its own, is sent first
// once the next attempt is allowed, not at once.
func TestAnSosPassesAWaitingWarnButNotTheDestinationsWait(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	destinations := map[string]checksandalerts.Destination{"hotline": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://chat.example.com"}, TimeLimitPerDeliveryAttempt: "1s"}}
	clock := time.Unix(1_800_000_000, 0)
	d := &Daemon{now: func() time.Time { return clock }, nextAttempts: map[string]queueHead{}}
	queue := func(id, check, kind, severity string) {
		t.Helper()
		e := checksandalerts.Event{ID: id, Node: "web-1", CheckGroup: "app", Check: check, Type: kind, AlertSeverity: severity, Incident: "i-" + check, At: clock, FleetIncidentKey: checksandalerts.FleetIncidentKey("app/" + check + " failed")}
		tx, err := s.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = store.QueueDirect(tx, clock.UnixNano(), e, []checksandalerts.Event{e}, destinations, store.QueuedBy{Role: store.StandaloneSender, Node: "web-1"}); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	head := func() store.Delivery {
		t.Helper()
		heads, err := s.Pending()
		if err != nil || len(heads) != 1 {
			t.Fatal(heads, err)
		}
		return heads[0]
	}
	for _, own := range []bool{false, true} {
		queue("dns failed", "dns", "failed", "warn")
		warn := head()
		if !d.attemptDue(warn, clock) {
			t.Fatal("a new alert is not due at once")
		}
		delay, err := s.Delivered(warn, &checksandalerts.RetryLater{After: time.Minute})
		if err != nil || delay != time.Minute {
			t.Fatal(delay, err)
		}
		d.afterAttempt(warn, delay)
		if own {
			queue("dns raised", "dns", checksandalerts.FailureIncidentEvents.Raised, "sos")
		} else {
			queue("tls failed", "tls", "failed", "sos")
		}
		sos := head()
		if sos.Event.AlertSeverity != "sos" {
			t.Fatal("the sos alert does not go ahead of the waiting warn alert", sos.Event.ID)
		}
		if d.attemptDue(sos, clock.Add(59*time.Second)) {
			t.Fatal("the sos alert cut the destination's wait short")
		}
		if !d.attemptDue(sos, clock.Add(time.Minute)) {
			t.Fatal("the sos alert is not due when the destination's wait ends")
		}
		for heads, _ := s.Pending(); len(heads) > 0; heads, _ = s.Pending() {
			if _, err = s.Delivered(heads[0], nil); err != nil {
				t.Fatal(err)
			}
			d.afterAttempt(heads[0], 0)
		}
		clock = clock.Add(time.Hour)
	}
}

// failOnce fails the attempt of the head of d's queue for destination with
// err, as the notifications worker records it, and returns the delay.
func failOnce(t *testing.T, d *Daemon, destination string, err error) time.Duration {
	t.Helper()
	heads, e := d.store.Pending()
	if e != nil {
		t.Fatal(e)
	}
	for _, head := range heads {
		if head.Destination != destination {
			continue
		}
		delay, e := d.store.Delivered(head, err)
		if e != nil {
			t.Fatal(e)
		}
		d.mu.Lock()
		d.afterAttempt(head, delay)
		if destination == config.HubDestination {
			d.latestHubAttemptError = err
		}
		d.mu.Unlock()
		return delay
	}
	t.Fatal("nothing queued for", destination)
	return 0
}

// headIsDue reports that the head of d's queue for destination is due for
// an attempt now.
func headIsDue(t *testing.T, d *Daemon, destination string) bool {
	t.Helper()
	heads, e := d.store.Pending()
	if e != nil {
		t.Fatal(e)
	}
	for _, head := range heads {
		if head.Destination == destination {
			d.mu.Lock()
			defer d.mu.Unlock()
			return d.attemptDue(head, d.now())
		}
	}
	t.Fatal("nothing queued for", destination)
	return false
}

// A reload that replaces a destination with another receiver starts its
// failed attempts in a row and its wait over: the failures of the old
// receiver hold back no alert to the new one.
func TestAReloadThatChangesADestinationStartsItsWaitOver(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	ops := func(url string) map[string]checksandalerts.Destination {
		return map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: url}, TimeLimitPerDeliveryAttempt: "1m"}}
	}
	c.Alerts.Destinations = ops("http://127.0.0.1:9/old")
	writeConfig(t, path, c)
	d, e := newDaemon(path, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	alert := func() {
		t.Helper()
		d.mu.Lock()
		defer d.mu.Unlock()
		if e := d.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: d.now(), Message: "the test's alert", Notify: true}}); e != nil {
			t.Fatal(e)
		}
	}
	alert()
	for range 8 {
		failOnce(t, d, "ops", errors.New("unreachable"))
	}
	if headIsDue(t, d, "ops") {
		t.Fatal("the failing destination has no wait")
	}
	c.Alerts.Destinations = ops("http://127.0.0.1:9/new")
	writeConfig(t, path, c)
	if _, e = d.reload(); e != nil {
		t.Fatal(e)
	}
	alert()
	if !headIsDue(t, d, "ops") {
		t.Fatal("the new receiver waits for the failures of the old one")
	}
	if delay := failOnce(t, d, "ops", errors.New("unreachable")); delay != time.Second {
		t.Fatalf("the new receiver's first failure waits %s, want 1s", delay)
	}
}

// A failure that the old receiver answers just after a reload replaced
// the destination records nothing for the new receiver: no failed attempt
// in a row and no wait hold back its next alert.
func TestAFailureOfTheOldReceiverAfterAReloadLeavesTheNewOneAlone(t *testing.T) {
	t.Parallel()
	release, answered := make(chan struct{}), make(chan struct{})
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(old.Close)
	path, c := daemonConfig(t)
	ops := func(url string) map[string]checksandalerts.Destination {
		return map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: url}, TimeLimitPerDeliveryAttempt: "1m"}}
	}
	c.Alerts.Destinations = ops(old.URL)
	writeConfig(t, path, c)
	d, e := newDaemon(path, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	alert := func() {
		t.Helper()
		d.mu.Lock()
		defer d.mu.Unlock()
		if e := d.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: d.now(), Message: "the test's alert", Notify: true}}); e != nil {
			t.Fatal(e)
		}
	}
	alert()
	heads, e := d.store.Pending()
	if e != nil || len(heads) != 1 {
		t.Fatal(heads, e)
	}
	d.mu.Lock()
	sender, ok := d.sender(heads[0])
	d.mu.Unlock()
	if !ok {
		t.Fatal("no sender")
	}
	var attemptError error
	go func() {
		attemptError = send(context.Background(), sender, heads[0].Event)
		close(answered)
	}()
	c.Alerts.Destinations = ops("http://127.0.0.1:9/new")
	writeConfig(t, path, c)
	if _, e = d.reload(); e != nil {
		t.Fatal(e)
	}
	close(release)
	<-answered
	if attemptError == nil {
		t.Fatal("the old receiver accepted the alert")
	}
	d.mu.Lock()
	d.attemptFinished(heads[0], heads[0].Event, attemptError)
	d.mu.Unlock()
	var failures int
	if e = d.store.DB.QueryRow("SELECT COUNT(*) FROM counters WHERE name LIKE 'failed_attempts_in_a_row:%'").Scan(&failures); e != nil || failures != 0 {
		t.Fatal("the old receiver's failure counts for the new one", failures, e)
	}
	alert()
	if !headIsDue(t, d, "ops") {
		t.Fatal("the new receiver waits for the old receiver's failure")
	}
}

// A signed answer that MMHotline gives to an alert after a reload
// cancelled it records nothing and goes nowhere, by the rule that the
// reload cancelled it by: a 429 from an alert source for nodes sending
// without the hub that the reload removed puts no wait on the destination,
// and a 400 from an alert source that the reload replaced passes the alert
// on to no other destination.
func TestASignedAnswerToAnAlertThatAReloadCancelledRecordsNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		reload func(*checksandalerts.MMHotlineDestination)
		answer error
	}{
		{"a 429 from a removed alert source for nodes", func(p *checksandalerts.MMHotlineDestination) { p.AlertSourceForNodesSendingWithoutTheHub = nil }, &checksandalerts.RetryLater{After: time.Hour}},
		{"a 400 from a replaced alert source", func(p *checksandalerts.MMHotlineDestination) { p.AlertSourceID = "another_source" }, &checksandalerts.MMHotlineRefusal{Status: http.StatusBadRequest}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path, cfg := daemonConfig(t)
			destinations := func(reload func(*checksandalerts.MMHotlineDestination)) map[string]checksandalerts.Destination {
				out := mmhotlineDestinations(t)
				p := out[mmhotlineDestinationName].MMHotline()
				p.AlertSourceForNodesSendingWithoutTheHub = &checksandalerts.MMHotlineAlertSource{AlertSourceID: "mmwatchdog_nodes", SigningSecretFile: p.SigningSecretFile}
				if reload != nil {
					reload(p)
				}
				out["pager"] = checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/pager"}, TimeLimitPerDeliveryAttempt: "1m"}
				return out
			}
			cfg.Alerts.Destinations = destinations(nil)
			writeConfig(t, path, cfg)
			d, e := newDaemon(path, time.Now)
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { d.Close() })
			alert := func() {
				t.Helper()
				d.mu.Lock()
				defer d.mu.Unlock()
				if e := d.record([]checksandalerts.Event{{AlertSeverity: "sos", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: d.now(), Message: "the test's alert", Notify: true}}); e != nil {
					t.Fatal(e)
				}
			}
			alert()
			// The alert was queued under the alert source for nodes, as a
			// joined node's direct send is.
			if _, e = d.store.DB.Exec("UPDATE outbox SET event=json_set(event,'$.mmhotline_alert_source','mmwatchdog_nodes') WHERE destination=?", mmhotlineDestinationName); e != nil {
				t.Fatal(e)
			}
			heads, e := d.store.Pending()
			if e != nil {
				t.Fatal(e)
			}
			var head store.Delivery
			for _, h := range heads {
				if h.Destination == mmhotlineDestinationName {
					head = h
				}
			}
			if head.Event.MMHotlineAlertSource != "mmwatchdog_nodes" {
				t.Fatalf("no alert queued under the alert source for nodes: %+v", heads)
			}
			cfg.Alerts.Destinations = destinations(c.reload)
			writeConfig(t, path, cfg)
			if _, e = d.reload(); e != nil {
				t.Fatal(e)
			}
			queuedForPager := func() int {
				t.Helper()
				var n int
				if e := d.store.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE destination='pager'").Scan(&n); e != nil {
					t.Fatal(e)
				}
				return n
			}
			before := queuedForPager()
			d.mu.Lock()
			d.attemptFinished(head, head.Event, c.answer)
			d.mu.Unlock()
			if after := queuedForPager(); after != before {
				t.Fatal("the cancelled alert went on to pager", before, after)
			}
			var failures int
			if e = d.store.DB.QueryRow("SELECT COUNT(*) FROM counters WHERE name LIKE 'failed_attempts_in_a_row:%'").Scan(&failures); e != nil || failures != 0 {
				t.Fatal("the old alert source's answer counts for the destination", failures, e)
			}
			alert()
			if !headIsDue(t, d, mmhotlineDestinationName) {
				t.Fatal("the destination waits for the old alert source's answer")
			}
		})
	}
}

// A report that reaches the hub ends what failed alert attempts to the hub
// left: an attempt that failed before the node joined, or while the hub
// was down, makes the next alert neither wait out the failures' delay nor
// go the direct way. That happened on the fleet suite: after a deploy
// gave the node a new join code, its first attempt failed without a
// credential, and its next alert went directly while it waited.
func TestAReportThatReachesTheHubEndsTheWaitOfFailedAlertAttempts(t *testing.T) {
	t.Parallel()
	_, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/"}, TimeLimitPerDeliveryAttempt: "1m"}})
	node := joinedNode(t, code, "app-1")
	if e := node.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	node.mu.Lock()
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "1s"
	alert := func() {
		t.Helper()
		if e := node.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: node.now(), Message: "the test's alert", Notify: true}}); e != nil {
			t.Fatal(e)
		}
	}
	alert()
	node.mu.Unlock()
	for range 3 {
		failOnce(t, node, config.HubDestination, errors.New("the node has no valid credential for the fleet hub"))
	}
	if e := node.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	// The alert that waits is due at once, also when the delivery worker
	// reads it from the outbox again.
	if !headIsDue(t, node, config.HubDestination) {
		t.Fatal("the alert waits out the failures after a report reached the hub")
	}
	node.fallBack(node.now().Add(time.Minute))
	if n := outboxCount(t, node, config.HubDestination); n != 1 {
		t.Fatal("the alert went the direct way after a report reached the hub", n)
	}
	if delay := failOnce(t, node, config.HubDestination, errors.New("refused")); delay != time.Second {
		t.Fatalf("the next failure waits %s, want 1s", delay)
	}
}

// attemptOnce makes one attempt to deliver the head of the destination's
// queue as the delivery worker does: only when it is due, through the
// destination's sender, with the worker's bookkeeping. It returns the
// attempt's error, or reports that nothing was due.
func attemptOnce(t *testing.T, d *Daemon, destination string) (attempted bool, err error) {
	t.Helper()
	heads, e := d.store.Pending()
	if e != nil {
		t.Fatal(e)
	}
	for _, head := range heads {
		if head.Destination != destination {
			continue
		}
		d.mu.Lock()
		sender, ok := d.sender(head)
		due := ok && d.attemptDue(head, d.now())
		d.mu.Unlock()
		if !due {
			return false, nil
		}
		err = send(context.Background(), sender, head.Event)
		delay, e := d.store.Delivered(head, err)
		if e != nil {
			t.Fatal(e)
		}
		d.mu.Lock()
		d.afterAttempt(head, delay)
		if destination == config.HubDestination {
			d.latestHubAttemptError = err
		}
		d.mu.Unlock()
		return true, err
	}
	return false, nil
}

// A hub that accepts reports while its alert intake refuses alerts, with
// HTTP 503 because the intake is full, keeps the node's failure: a report
// proves nothing about alert intake, and the overdue alert goes the
// direct way.
func TestAReportDoesNotEndTheFailureOfAlertsThatTheHubsIntakeRefuses(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/"}, TimeLimitPerDeliveryAttempt: "1m"}})
	node := joinedNode(t, code, "app-1")
	if e := node.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	limits := *hub.store.Limits.Load()
	limits.UndeliveredAlertsOfNodesBytes = 1
	hub.store.Limits.Store(&limits)
	node.mu.Lock()
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "1s"
	e := node.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", At: node.now(), Message: "the test's alert", Notify: true}})
	node.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	attempted, e := attemptOnce(t, node, config.HubDestination)
	var refusal *fleet.HubRefusal
	if !attempted || !errors.As(e, &refusal) || refusal.Status != 503 {
		t.Fatal("the hub's alert intake did not refuse the alert", attempted, e)
	}
	for range 2 {
		if e = node.report(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if node.latestHubAttemptError == nil {
		t.Fatal("a report ended the failure of the alert that the hub's alert intake refused")
	}
	node.fallBack(node.now().Add(2 * time.Second))
	if n := outboxCount(t, node, config.HubDestination); n != 0 {
		t.Fatal("the overdue alert took neither route after reports reached the hub", n)
	}
}
