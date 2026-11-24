package store_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// An sos alert goes ahead of every alert below sos for the same
// destination, across keys, and supersedes an alert below sos of its own
// key that waits to be retried. Within one key a resolution and what
// follows it keep their order, and a notice keeps its place among the
// alerts below sos.
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
	// http raised supersedes the http failure that waits to be retried; dns
	// raised goes ahead of the dns failure that was not attempted yet; disk
	// failed again waits for the resolution of its key.
	want := []string{"http raised", "tls failed", "dns raised", "notice", "dns failed", "disk failed", "disk recovered", "disk failed again"}
	if !slices.Equal(delivered, want) {
		t.Fatalf("delivered in the order %q, want %q", delivered, want)
	}
}

// An sos never supersedes the alert that announces an exact finding of its
// key, waiting to be retried: that alert alone names the exact finding and
// its node, which the destination would otherwise never hear of, while the
// key counts the exact finding as announced. It goes after the sos.
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
