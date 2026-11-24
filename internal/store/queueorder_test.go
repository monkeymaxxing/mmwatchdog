package store_test

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// queueOf is a destination's queue for the queue-rule tests (queueorder.go):
// queue adds an alert of the check's key, and attempt tries the
// destination's next delivery.
type queueOf struct {
	t *testing.T
	s *store.Store
}

func newQueue(t *testing.T) queueOf {
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return queueOf{t, s}
}

func (q queueOf) queue(id, kind, severity, check, incident string) {
	q.t.Helper()
	at := time.Unix(1_800_000_000, 0)
	e := checksandalerts.Event{ID: id, Node: "web-1", CheckGroup: "app", Check: check, Type: kind, AlertSeverity: severity, Incident: incident, At: at, Message: id}
	if checksandalerts.BelongsToAnIncident(kind) {
		e.FleetIncidentKey = checksandalerts.FleetIncidentKey("app/" + check + " failed")
	}
	if kind == "notice" {
		e.CheckGroup, e.Check = "", ""
	}
	tx, err := q.s.DB.Begin()
	if err != nil {
		q.t.Fatal(err)
	}
	defer tx.Rollback()
	destinations := map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", testhelpers.AllSeverities...)}
	if err = store.QueueDirect(tx, at.UnixNano(), e, []checksandalerts.Event{e}, destinations, store.QueuedBy{Role: store.StandaloneSender, Node: "web-1"}); err != nil {
		q.t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		q.t.Fatal(err)
	}
}

// attempt takes the destination's next delivery, delivers it when accepts
// says so, and otherwise fails its attempt. It returns the delivery's
// event ID.
func (q queueOf) attempt(accepts func(id string) bool) string {
	q.t.Helper()
	heads, e := q.s.Pending()
	if e != nil || len(heads) > 1 {
		q.t.Fatal(heads, e)
	}
	if len(heads) == 0 {
		return ""
	}
	var failed error
	if !accepts(heads[0].Event.ID) {
		failed = errors.New("refused for now")
	}
	if _, e = q.s.Delivered(heads[0], failed); e != nil {
		q.t.Fatal(e)
	}
	return heads[0].Event.ID
}

// accepts and refuses are answers of the destination in attempt.
func accepts(string) bool { return true }
func refuses(string) bool { return false }

// drain delivers the whole queue and returns the order of its event IDs.
func (q queueOf) drain() []string {
	q.t.Helper()
	delivered := []string{}
	for id := q.attempt(accepts); id != ""; id = q.attempt(accepts) {
		delivered = append(delivered, id)
	}
	return delivered
}

// A destination that accepts every alert gets the queue first in, first
// out: an sos waits its turn like every other alert.
func TestAHealthyDestinationGetsTheQueueInOrder(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	q.queue("A failed", "failed", "warn", "http", "i-a")
	q.queue("notice", "notice", "warn", "", "")
	q.queue("B failed", "failed", "sos", "dns", "i-b")
	q.queue("A raised", checksandalerts.FailureIncidentEvents.Raised, "sos", "http", "i-a")
	q.queue("A recovered", "recovered", "sos", "http", "i-a")
	q.queue("A failed again", "failed", "sos", "http", "i-a2")
	want := []string{"A failed", "notice", "B failed", "A raised", "A recovered", "A failed again"}
	if got := q.drain(); !slices.Equal(got, want) {
		t.Fatalf("delivered in the order %q, want %q", got, want)
	}
}

// While a warn waits for a retry, the next attempt goes to a later sos,
// of another key or of the warn's own incident; then the queue goes on in
// order.
func TestAnSosGoesAheadOfAStuckWarn(t *testing.T) {
	t.Parallel()
	for _, check := range []string{"dns", "http"} {
		q := newQueue(t)
		q.queue("A failed", "failed", "warn", "http", "i-http")
		if got := q.attempt(refuses); got != "A failed" {
			t.Fatal(got)
		}
		q.queue("notice", "notice", "warn", "", "")
		q.queue("sos", checksandalerts.FailureIncidentEvents.Raised, "sos", check, "i-"+check)
		if got := q.attempt(accepts); got != "sos" {
			t.Fatalf("%s: %q went ahead of the sos", check, got)
		}
		if got, want := q.drain(), []string{"A failed", "notice"}; !slices.Equal(got, want) {
			t.Fatalf("%s: after the sos, delivered in the order %q, want %q", check, got, want)
		}
	}
}

// An sos of one key that keeps failing holds back no sos of another key:
// that one gets the next attempt.
func TestAFailingSosHoldsBackNoSosOfAnotherKey(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	q.queue("A failed", "failed", "sos", "http", "i-a")
	q.queue("B failed", "failed", "sos", "dns", "i-b")
	refusesA := func(id string) bool { return id != "A failed" }
	tried := []string{}
	for range 3 {
		tried = append(tried, q.attempt(refusesA))
	}
	if want := []string{"A failed", "B failed", "A failed"}; !slices.Equal(tried, want) {
		t.Fatalf("tried %q while A keeps failing, want %q", tried, want)
	}
}

// An sos never passes a closing alert of its own key that precedes it: the
// key closed and opened again, and the sos belongs after the closing.
func TestAnSosNeverPassesTheClosingOfItsKey(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	q.queue("A failed", "failed", "warn", "http", "i-a")
	if got := q.attempt(refuses); got != "A failed" {
		t.Fatal(got)
	}
	q.queue("A recovered", "recovered", "warn", "http", "i-a")
	q.queue("A failed again", "failed", "sos", "http", "i-a2")
	if got := q.attempt(accepts); got != "A failed" {
		t.Fatalf("%q went ahead of the oldest alert", got)
	}
	if got, want := q.drain(), []string{"A recovered", "A failed again"}; !slices.Equal(got, want) {
		t.Fatalf("delivered in the order %q, want %q", got, want)
	}
}

// The wait after a failed attempt doubles with the destination's failed
// attempts in a row, also when each attempt tries another alert, and
// starts over once the destination accepts one.
func TestTheWaitDoublesWhileTheDestinationKeepsFailing(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	q.queue("A failed", "failed", "warn", "http", "i-a")
	q.queue("B failed", "failed", "sos", "dns", "i-b")
	q.queue("C failed", "failed", "sos", "tls", "i-c")
	fail := func(want string, wantDelay time.Duration) {
		t.Helper()
		heads, e := q.s.Pending()
		if e != nil || len(heads) != 1 || heads[0].Event.ID != want {
			t.Fatal(heads, e)
		}
		delay, e := q.s.Delivered(heads[0], errors.New("unreachable"))
		if e != nil || delay != wantDelay {
			t.Fatalf("after %s failed: wait %s, %v; want %s", want, delay, e, wantDelay)
		}
	}
	fail("A failed", time.Second)
	fail("B failed", 2*time.Second)
	fail("C failed", 4*time.Second)
	fail("A failed", 8*time.Second)
	if got := q.attempt(accepts); got != "A failed" {
		t.Fatal(got)
	}
	fail("B failed", time.Second)
}

// A joined node queues its alerts for the hub by the same rule as every
// other route: an sos goes ahead of a warn that waits for a retry.
func TestTheHubRouteQueuesByTheSameRule(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	q.s.Node = "web-1"
	at := time.Unix(1_800_000_000, 0)
	save := func(id, check, severity string) {
		t.Helper()
		e := checksandalerts.Event{ID: id, Node: "web-1", CheckGroup: "app", Check: check, Type: "failed", AlertSeverity: severity, Incident: "i-" + check, At: at, Message: id, Notify: true}
		if err := q.s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{e}, store.AlertRoute{Joined: true}); err != nil {
			t.Fatal(err)
		}
	}
	save("A failed", "http", "warn")
	if got := q.attempt(refuses); got != "A failed" {
		t.Fatal(got)
	}
	save("B failed", "dns", "sos")
	if got := q.attempt(accepts); got != "B failed" {
		t.Fatalf("%q went ahead of the sos to the hub", got)
	}
}

// An sos goes ahead of the alert that announces an exact finding of its
// key, waiting to be retried, and that alert still goes after it: it alone
// names the exact finding and its node.
func TestSosKeepsTheAlertThatAnnouncesAnExactFinding(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn", "sos")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	heads, e := h.s.Pending()
	if e != nil || len(heads) != 1 {
		t.Fatal(heads, e)
	}
	if _, e = h.s.Delivered(heads[0], &checksandalerts.RetryLater{After: time.Minute}); e != nil {
		t.Fatal(e)
	}
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-2", "failed", "sos", versionB, 0, tenOFive.Add(time.Minute)))
	got := []checksandalerts.Event{}
	for {
		heads, e := h.s.Pending()
		if e != nil {
			t.Fatal(e)
		}
		if len(heads) == 0 {
			break
		}
		got = append(got, heads[0].Event)
		if _, e = h.s.Delivered(heads[0], nil); e != nil {
			t.Fatal(e)
		}
	}
	expectKinds(t, got, "failed sos", "failed warn")
	if got[1].Node != "app-1" || got[1].ExactFinding != versionA {
		t.Fatalf("%+v", got[1])
	}
}
