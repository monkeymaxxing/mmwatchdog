package store_test

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// The queue rule (queueorder.go): an sos alert goes ahead of every alert
// below sos of other keys for the same destination, and never removes one.
// Within its own key it goes ahead only of an alert below sos that waits
// to be retried; an alert of its key not attempted yet keeps its place, so
// an incident's opening reaches a healthy destination before its raise. An
// alert that waits to be retried goes after the alerts not attempted yet.
// Within one key a resolution and what follows it keep their order, and a
// notice keeps its place among the alerts below sos.
func TestSosGoesFirstAndKeepsTheOrderWithinItsKey(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	destinations := map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", testhelpers.AllSeverities...)}
	at := time.Unix(1_800_000_000, 0)
	queue := func(id, kind, severity, check, incident string) {
		t.Helper()
		e := checksandalerts.Event{ID: id, Node: "web-1", CheckGroup: "app", Check: check, Type: kind, AlertSeverity: severity, Incident: incident, At: at, Message: id}
		if checksandalerts.BelongsToAnIncident(kind) {
			e.FleetIncidentKey = checksandalerts.FleetIncidentKey("app/" + check + " failed")
		}
		if kind == "notice" {
			e.Node, e.CheckGroup, e.Check = "web-1", "", ""
		}
		tx, err := s.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = store.QueueDirect(tx, at.UnixNano(), e, []checksandalerts.Event{e}, destinations, store.QueuedBy{Role: store.StandaloneSender, Node: "web-1"}); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// A warn alert that waits to be retried.
	queue("http failed", "failed", "warn", "http", "i-http")
	heads, e := s.Pending()
	if e != nil || len(heads) != 1 {
		t.Fatal(heads, e)
	}
	if _, e = s.Delivered(heads[0], &checksandalerts.RetryLater{After: time.Minute}); e != nil {
		t.Fatal(e)
	}
	queue("notice", "notice", "warn", "", "")
	queue("dns failed", "failed", "warn", "dns", "i-dns")
	queue("disk failed", "failed", "warn", "disk", "i-disk-1")
	queue("disk recovered", "recovered", "warn", "disk", "i-disk-1")
	queue("http raised", checksandalerts.FailureIncidentEvents.Raised, "sos", "http", "i-http")
	queue("disk failed again", "failed", "sos", "disk", "i-disk-2")
	queue("tls failed", "failed", "sos", "tls", "i-tls")
	queue("dns raised", checksandalerts.FailureIncidentEvents.Raised, "sos", "dns", "i-dns")
	delivered := []string{}
	for {
		heads, e := s.Pending()
		if e != nil {
			t.Fatal(e)
		}
		if len(heads) == 0 {
			break
		}
		delivered = append(delivered, heads[0].Event.ID)
		if _, e = s.Delivered(heads[0], nil); e != nil {
			t.Fatal(e)
		}
	}
	// http raised goes ahead of the http failure that waits to be retried,
	// which still goes, after every alert not attempted yet; dns raised
	// waits for the dns failure that was not attempted yet, and then goes
	// ahead of the alerts below sos of other keys; the notice keeps its
	// place; disk failed again waits for the resolution of its key.
	want := []string{"http raised", "tls failed", "notice", "dns failed", "dns raised", "disk failed", "disk recovered", "disk failed again", "http failed"}
	if !slices.Equal(delivered, want) {
		t.Fatalf("delivered in the order %q, want %q", delivered, want)
	}
}

// queueOf is a destination's queue for the queue-rule tests: queue adds an
// alert of the check's key, and next returns the destination's head.
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

// attempt takes the destination's head, delivers it when accepts says so,
// and otherwise fails its attempt. It returns the head's event ID.
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

// An alert of one key that keeps failing holds back no other key: another
// key's opening, not attempted yet, goes ahead of it, and so its sos then
// goes, opening first.
func TestAFailingAlertOfOneKeyHoldsBackNoSosOfAnother(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	q.queue("A failed", "failed", "warn", "http", "i-a")
	refusesA := func(id string) bool { return id != "A failed" }
	if got := q.attempt(refusesA); got != "A failed" {
		t.Fatal(got)
	}
	q.queue("B failed", "failed", "warn", "dns", "i-b")
	q.queue("B raised", checksandalerts.FailureIncidentEvents.Raised, "sos", "dns", "i-b")
	delivered := []string{}
	for range 6 {
		if id := q.attempt(refusesA); id != "A failed" {
			delivered = append(delivered, id)
		}
	}
	if want := []string{"B failed", "B raised"}; !slices.Equal(delivered, want) {
		t.Fatalf("delivered %q while A keeps failing, want %q first", delivered, want)
	}
}

// An sos passes an alert of its own key that waits to be retried, but not
// one of its key that was not attempted yet: that one goes first, and the
// sos right after it.
func TestAnSosWaitsForAnUntriedAlertOfItsOwnKey(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	q.queue("B failed", "failed", "warn", "dns", "i-b")
	q.queue("B raised", checksandalerts.FailureIncidentEvents.Raised, "sos", "dns", "i-b")
	delivered := []string{}
	for id := q.attempt(func(string) bool { return true }); id != ""; id = q.attempt(func(string) bool { return true }) {
		delivered = append(delivered, id)
	}
	if want := []string{"B failed", "B raised"}; !slices.Equal(delivered, want) {
		t.Fatalf("delivered in the order %q, want %q", delivered, want)
	}
}

// Once the oldest alert of a key waits for a retry, the key is stuck: its
// sos passes every alert of the key that is not a closing, also an untried
// one behind the stuck alert, so a warn that keeps failing never holds the
// sos back. The passed alerts keep their order after it.
func TestAnSosPassesItsStuckKey(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	q.queue("B failed", "failed", "warn", "dns", "i-b")
	if got := q.attempt(func(string) bool { return false }); got != "B failed" {
		t.Fatal(got)
	}
	q.queue("B changed", checksandalerts.ExactFindingChanged, "warn", "dns", "i-b")
	q.queue("B raised", checksandalerts.FailureIncidentEvents.Raised, "sos", "dns", "i-b")
	if got := q.attempt(func(id string) bool { return id != "B failed" }); got != "B raised" {
		t.Fatalf("the sos waited behind its stuck key: %q went first", got)
	}
	delivered := []string{}
	for id := q.attempt(func(string) bool { return true }); id != ""; id = q.attempt(func(string) bool { return true }) {
		delivered = append(delivered, id)
	}
	if want := []string{"B failed", "B changed"}; !slices.Equal(delivered, want) {
		t.Fatalf("after the sos, delivered in the order %q, want %q", delivered, want)
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
