package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
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
