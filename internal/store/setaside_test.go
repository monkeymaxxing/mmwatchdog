package store_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/store"
)

// The tests of setting a finding aside on the hub, from the design of
// setting a finding aside, "Test". The hub takes its nodes' alerts through
// its intake and alert grouping step and its reports through
// ReconcileIncidentSets (groupingHub), and puts the entries of
// set_aside_findings.yml in force as a start or a reload does.

const reasonNoPAM = "Needs musl without PAM"

func entryFor(exactFinding, reason string) config.SetAsideFinding {
	return config.SetAsideFinding{Check: "packages/flaws", Finding: flaw, ExactFinding: exactFinding, Reason: reason,
		InvestigationNote: "triage_agent_notes/openssh.md", SetAsideBy: "triage agent", SetAsideOn: "2026-11-23"}
}

// setAside puts entries in force at at, as a start or a reload does.
func (h *groupingHub) setAside(at time.Time, entries ...config.SetAsideFinding) {
	h.t.Helper()
	h.advance(at)
	if e := h.s.SettleSetAsideFindings(entries, h.destinations, at); e != nil {
		h.t.Fatal(e)
	}
}

// heldRows counts the membership rows of a node's incident in every
// destination's set, in any state.
func (h *groupingHub) heldRows(node string) int {
	h.t.Helper()
	var n int
	if e := h.s.DB.QueryRow("SELECT COUNT(*) FROM fleet_incident_set WHERE reporter=?", node).Scan(&n); e != nil {
		h.t.Fatal(e)
	}
	return n
}

// An alert whose exact finding an entry covers goes out nowhere, and
// neither does anything later of its incident. An alert with any other
// exact finding goes out.
func TestACoveredFindingSendsNothing(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn", "sos"), "hotline": hotline()})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"))
	expectKinds(t, h.take("hotline"))
	h.report(tenOFive.Add(time.Minute), map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionA, 0, tenOFive, tenOFive.Add(time.Minute))})
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	h.arrive(tenOFive.Add(3*time.Hour), h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(3*time.Hour)))
	expectKinds(t, h.take("chat"))
	expectKinds(t, h.take("hotline"))
	if n := h.heldRows("app-1"); n != 0 {
		t.Fatal("a covered incident is in a set:", n)
	}
	// Another version of the package is another exact finding.
	h.arrive(tenOFive.Add(4*time.Hour), h.alert("db-1", "failed", "warn", versionB, 0, tenOFive.Add(4*time.Hour)))
	expectKinds(t, h.take("chat"), "failed warn")
	expectKinds(t, h.take("hotline"), "failed warn")
}

// sos is never set aside: an sos alert with a covered exact finding goes
// out, and so does a covered incident's raise to sos.
func TestSosIsNeverSetAside(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn", "sos")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	h.arrive(tenOFive, h.alert("app-1", "failed", "sos", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed sos")
	h.arrive(tenOFive, h.alert("app-2", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"))
	h.arrive(tenOFive.Add(time.Hour), h.alert("app-2", checksandalerts.FailureIncidentEvents.Raised, "sos", versionA, 0, tenOFive.Add(time.Hour)))
	// app-2 joins the key that app-1's sos announced: growth at sos goes
	// within a minute.
	h.tick(tenOFive.Add(time.Hour + time.Minute))
	got := h.take("chat")
	expectKinds(t, got, "finding_nodes_changed sos")
	if got[0].NodeCount != 1 || got[0].Nodes[0] != "app-2" {
		t.Fatal(got[0].Nodes)
	}
}

// An entry added while the finding is announced closes the key at every
// destination that got it, once, and leaves no row behind: the node's
// later recovery sends nothing.
func TestAddingAnEntryClosesTheAnnouncedFinding(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn"), "hotline": hotline()})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive.Add(time.Minute), h.alert("db-1", "failed", "warn", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("chat"), "failed warn")
	expectKinds(t, h.take("hotline"), "failed warn")
	h.setAside(tenOFive.Add(time.Hour), entryFor(versionA, reasonNoPAM))
	got := h.take("chat")
	expectKinds(t, got, "closed_without_recovery warn")
	if got[0].Message != "Set aside after investigation: "+reasonNoPAM || got[0].Title != "packages/flaws: "+flaw+" set aside after investigation on 2 nodes" {
		t.Fatalf("%q / %q", got[0].Title, got[0].Message)
	}
	expectKinds(t, h.take("hotline"), "closed_without_recovery resolved-warn")
	for _, node := range []string{"app-1", "db-1"} {
		if n := h.heldRows(node); n != 0 {
			t.Fatal("a row is left behind for", node, n)
		}
	}
	// The pending growth of db-1 never goes out, and the closing is not
	// sent again at the next reload.
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	h.setAside(tenOFive.Add(2*time.Hour), entryFor(versionA, reasonNoPAM))
	h.arrive(tenOFive.Add(3*time.Hour), h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(3*time.Hour)))
	expectKinds(t, h.take("chat"))
	expectKinds(t, h.take("hotline"))
}

// While another node of the key stays open for a destination, a newly
// covered node only leaves its set: the key is not closed.
func TestAnEntryClosesAKeyOnlyWhenNothingElseOfItIsOpen(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive, h.alert("db-1", "failed", "warn", versionB, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn", "failed warn")
	h.setAside(tenOFive.Add(time.Hour), entryFor(versionA, reasonNoPAM))
	expectKinds(t, h.take("chat"))
	h.arrive(tenOFive.Add(2*time.Hour), h.alert("db-1", "recovered", "warn", versionB, 0, tenOFive.Add(2*time.Hour)))
	got := h.take("chat")
	expectKinds(t, got, "recovered warn")
	// The closing alert claims recovery only for db-1: app-1 is still open.
	if !strings.Contains(got[0].Title, "db-1") {
		t.Fatal(got[0].Title)
	}
}

// Removing an entry that covers 5000 nodes sends one opening alert to each
// destination, with the note and the reason, naming 20 nodes and counting
// the rest; the nodes are then in the sets, so one closing follows.
func TestRemovingAnEntryOpensOnceForEveryNode(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn"), "hotline": hotline()})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	const nodes = 5000
	reports := map[string]store.ReportedIncidents{}
	for i := range nodes {
		node := fmt.Sprintf("app-%04d", i)
		reports[node] = shows(node, "warn", versionA, 0, tenOFive, tenOFive.Add(time.Minute))
	}
	h.report(tenOFive.Add(time.Minute), reports)
	expectKinds(t, h.take("chat"))
	expectKinds(t, h.take("hotline"))
	h.setAside(tenOFive.Add(time.Hour))
	got := h.take("chat")
	expectKinds(t, got, "failed warn")
	if got[0].NodeCount != nodes || len(got[0].Nodes) != store.CombinedNames || !strings.HasSuffix(got[0].Message, "; an earlier exact finding was set aside: "+reasonNoPAM) {
		t.Fatalf("%d %d %q", got[0].NodeCount, len(got[0].Nodes), got[0].Message)
	}
	expectKinds(t, h.take("hotline"), "failed warn")
	// Nothing more for the nodes the opening named: no update at the tick.
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	expectKinds(t, h.take("chat"))
	for i := range nodes {
		node := fmt.Sprintf("app-%04d", i)
		reports[node] = store.ReportedIncidents{Sent: tenOFive.Add(7 * time.Hour)}
	}
	h.report(tenOFive.Add(7*time.Hour), reports)
	expectKinds(t, h.take("chat"), "recovered warn")
	expectKinds(t, h.take("hotline"), "recovered resolved-warn")
}

// An entry added while the hub was stopped takes effect at its start.
func TestAStartSettlesAFileChangedWhileTheHubWasStopped(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive)
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	h.s.Close()
	h.open()
	h.setAside(tenOFive.Add(time.Hour), entryFor(versionA, reasonNoPAM))
	got := h.take("chat")
	expectKinds(t, got, "closed_without_recovery warn")
	if got[0].Title != "app-1: packages/flaws: "+flaw+" set aside after investigation" {
		t.Fatal(got[0].Title)
	}
	// And the entry stays in force for alerts after the start.
	h.arrive(tenOFive.Add(2*time.Hour), h.alert("db-1", "failed", "warn", versionA, 0, tenOFive.Add(2*time.Hour)))
	expectKinds(t, h.take("chat"))
}

// A node whose exact finding moves onto an entry is newly covered; one
// whose exact finding moves off it opens again with the note, through its
// alert or through its report.
func TestANodeWhoseExactFindingMovesOntoAndOffAnEntry(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionB, reasonNoPAM))
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	onto := tenOFive.Add(time.Hour)
	h.arrive(onto, h.alert("app-1", checksandalerts.ExactFindingChanged, "warn", versionB, 1, onto))
	got := h.take("chat")
	expectKinds(t, got, "closed_without_recovery warn")
	if got[0].Message != "Set aside after investigation: "+reasonNoPAM {
		t.Fatal(got[0].Message)
	}
	off := tenOFive.Add(2 * time.Hour)
	h.arrive(off, h.alert("app-1", checksandalerts.ExactFindingChanged, "warn", versionC, 2, off))
	got = h.take("chat")
	expectKinds(t, got, "failed warn")
	if got[0].Message != versionC+"; an earlier exact finding was set aside: "+reasonNoPAM || got[0].ExactFinding != versionC {
		t.Fatalf("%q", got[0].Message)
	}
	// The same through a report, for a second node: onto the entry, and off.
	h.arrive(off, h.alert("db-1", "failed", "warn", versionA, 0, off))
	expectKinds(t, h.take("chat"), "failed warn")
	h.report(off.Add(time.Minute), map[string]store.ReportedIncidents{"db-1": shows("db-1", "warn", versionB, 1, off, off.Add(time.Minute))})
	expectKinds(t, h.take("chat"))
	if n := h.heldRows("db-1"); n != 0 {
		t.Fatal("db-1 is still in the set", n)
	}
	const versionD = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u7; fixed version: none"
	h.report(off.Add(2*time.Minute), map[string]store.ReportedIncidents{"db-1": shows("db-1", "warn", versionD, 2, off, off.Add(2*time.Minute))})
	got = h.take("chat")
	expectKinds(t, got, "failed warn")
	if !strings.HasSuffix(got[0].Message, "an earlier exact finding was set aside: "+reasonNoPAM) || got[0].ExactFinding != versionD {
		t.Fatal(got[0].Message)
	}
	// Moving off the entry onto an exact finding that the destination has
	// already is growth for the next update.
	h.report(off.Add(3*time.Minute), map[string]store.ReportedIncidents{"db-1": shows("db-1", "warn", versionB, 3, off, off.Add(3*time.Minute))})
	h.report(off.Add(4*time.Minute), map[string]store.ReportedIncidents{"db-1": shows("db-1", "warn", versionC, 4, off, off.Add(4*time.Minute))})
	expectKinds(t, h.take("chat"))
	h.tick(time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC))
	got = h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	if g := got[0].NodesByExactFinding; len(g) != 1 || g[0].ExactFinding != versionC || !slices.Equal(g[0].Nodes, []string{"db-1"}) {
		t.Fatalf("%+v", g)
	}
}

// An entry covers only incidents whose alerts go by the check's own
// destinations: removing it opens the key only where the check sends.
func TestAnIncidentNoLongerCoveredOpensWhereItsCheckSends(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat_a": chatTaking("https://a.example.com", "warn"), "chat_b": chatTaking("https://b.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	alert := h.alert("app-1", "failed", "warn", versionA, 0, tenOFive)
	alert.SendAlertsOnlyToDestinations = &[]string{"chat_b"}
	h.arrive(tenOFive, alert)
	h.setAside(tenOFive.Add(time.Hour))
	expectKinds(t, h.take("chat_a"))
	expectKinds(t, h.take("chat_b"), "failed warn")
}

// Nodes that move off an entry one by one, onto an exact finding that is
// announced already, are covered growth: the first one's opening announces
// it, and the others wait for the shared tick, as nodes that move between
// two exact findings that are not set aside do.
func TestNodesMovingOffAnEntryOneByOneAreGrowth(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	for _, node := range []string{"app-1", "app-2", "app-3"} {
		h.arrive(tenOFive, h.alert(node, "failed", "warn", versionA, 0, tenOFive))
	}
	expectKinds(t, h.take("chat"))
	at := tenOFive.Add(time.Hour)
	h.arrive(at, h.alert("app-1", checksandalerts.ExactFindingChanged, "warn", versionB, 1, at))
	got := h.take("chat")
	expectKinds(t, got, "failed warn")
	if got[0].ExactFinding != versionB {
		t.Fatal(got[0].ExactFinding)
	}
	h.arrive(at.Add(10*time.Minute), h.alert("app-2", checksandalerts.ExactFindingChanged, "warn", versionB, 1, at.Add(10*time.Minute)))
	h.report(at.Add(20*time.Minute), map[string]store.ReportedIncidents{"app-3": shows("app-3", "warn", versionB, 1, tenOFive, at.Add(20*time.Minute))})
	expectKinds(t, h.take("chat"))
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	got = h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	if !slices.Equal(got[0].Nodes, []string{"app-2", "app-3"}) {
		t.Fatal(got[0].Nodes)
	}
}

// Removing two entries of one key at once sends one opening alert that
// names both exact findings, so a later node with either of them is
// covered growth that the destination already knows of.
func TestTheOpeningAfterRemovingEntriesNamesEveryExactFinding(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM), entryFor(versionB, "Not reachable from outside"))
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive, h.alert("app-2", "failed", "warn", versionB, 0, tenOFive))
	h.arrive(tenOFive, h.alert("app-3", "failed", "warn", versionB, 0, tenOFive))
	expectKinds(t, h.take("chat"))
	h.setAside(tenOFive.Add(time.Hour))
	got := h.take("chat")
	expectKinds(t, got, "failed warn")
	for _, want := range []string{versionA + "\n  on 1 node: app-1", versionB + "\n  on 2 nodes: app-2, app-3"} {
		if !strings.Contains(got[0].Body, want) {
			t.Fatalf("the opening does not name %q:\n%s", want, got[0].Body)
		}
	}
}

// A node's report that its exact finding moved off an entry opens the key
// with the new exact finding. The node's own exact_finding_changed with
// that same sequence number, arriving after the report, has nothing new to
// say: the next update does not name the node again.
func TestAnExactFindingChangeThatTheOpeningNamedIsNotGrowth(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"))
	at := tenOFive.Add(time.Hour)
	h.report(at, map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionB, 1, tenOFive, at)})
	got := h.take("chat")
	expectKinds(t, got, "failed warn")
	if got[0].ExactFinding != versionB {
		t.Fatal(got[0].ExactFinding)
	}
	h.arrive(at.Add(time.Second), h.alert("app-1", checksandalerts.ExactFindingChanged, "warn", versionB, 1, at.Add(time.Second)))
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	expectKinds(t, h.take("chat"))
}

// When the outbox's size limit drops the opening that named the node's
// exact finding before the destination got it, the node's own
// exact_finding_changed of that exact finding still tells the destination
// which exact finding the node has: it goes out at once.
func TestAnExactFindingChangeCountsWhereTheOpeningThatNamedItWasDropped(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"))
	at := tenOFive.Add(time.Hour)
	h.report(at, map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionB, 1, tenOFive, at)})
	limits := *h.s.Limits.Load()
	small := limits
	small.UndeliveredAlertsBytes = 1
	h.s.SetLimits(small)
	if e := h.s.Prune(at); e != nil {
		t.Fatal(e)
	}
	h.s.SetLimits(limits)
	expectKinds(t, h.take("chat"))
	h.arrive(at.Add(time.Second), h.alert("app-1", checksandalerts.ExactFindingChanged, "warn", versionB, 1, at.Add(time.Second)))
	got := h.take("chat")
	expectKinds(t, got, checksandalerts.ExactFindingChanged+" warn")
	if got[0].ExactFinding != versionB {
		t.Fatal(got[0].ExactFinding)
	}
}

// A node's failure that reaches the hub after the node's report of a newer
// exact finding goes out with the exact finding that the hub holds: the
// report's newer one, not the set-aside one that the failure carried.
func TestAFailureArrivingAfterANewerReportNamesTheNewerExactFinding(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	h.report(tenOFive.Add(time.Hour), map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionB, 1, tenOFive, tenOFive.Add(time.Hour))})
	expectKinds(t, h.take("chat"))
	h.arrive(tenOFive.Add(time.Hour), h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	got := h.take("chat")
	expectKinds(t, got, "failed warn")
	if got[0].ExactFinding != versionB {
		t.Fatal("the failure names", got[0].ExactFinding)
	}
	// Its text names the newer exact finding first, and says that the rest
	// is from the earlier one.
	if first, _, _ := strings.Cut(got[0].Body, "\n"); !strings.Contains(first, versionB) || !strings.Contains(got[0].Body, "earlier") || !strings.Contains(got[0].Body, versionA) {
		t.Fatalf("the failure's body %q", got[0].Body)
	}
	// And versionA, set aside, was never announced: another node with it
	// sends nothing.
	h.arrive(tenOFive.Add(2*time.Hour), h.alert("db-1", "failed", "warn", versionA, 0, tenOFive.Add(2*time.Hour)))
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	expectKinds(t, h.take("chat"))
}

// A key that an entry closes starts over: a node that recovered before the
// closing is not named as ok again in an update after the key opens again.
func TestAKeyThatAnEntryClosesStartsOver(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive, h.alert("app-2", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-2", "recovered", "warn", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("chat"), "failed warn")
	h.setAside(tenOFive.Add(time.Hour), entryFor(versionA, reasonNoPAM))
	expectKinds(t, h.take("chat"), "closed_without_recovery warn")
	h.arrive(tenOFive.Add(2*time.Hour), h.alert("db-1", "failed", "warn", versionB, 0, tenOFive.Add(2*time.Hour)))
	h.arrive(tenOFive.Add(2*time.Hour), h.alert("db-2", "failed", "warn", versionB, 0, tenOFive.Add(2*time.Hour)))
	expectKinds(t, h.take("chat"), "failed warn")
	h.tick(time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC))
	got := h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	if got[0].RecoveredNodeCount != 0 || !slices.Equal(got[0].Nodes, []string{"db-2"}) {
		t.Fatalf("%+v %+v", got[0].Nodes, got[0].RecoveredNodes)
	}
}

// A node whose only open incidents are set aside is in no destination's
// set, but the hub still holds them: a forget that a restart interrupted is
// applied to it too (IncidentSetReporters), so removing the entry later
// opens nothing for the forgotten node.
func TestAForgottenNodeWithOnlySetAsideIncidentsIsForgotten(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	h.report(tenOFive, map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionA, 0, tenOFive, tenOFive)})
	reporters, e := h.s.IncidentSetReporters()
	if e != nil || !slices.Equal(reporters, []string{"app-1"}) {
		t.Fatal(reporters, e)
	}
	h.report(tenOFive.Add(time.Minute), map[string]store.ReportedIncidents{"app-1": {Forgotten: true}})
	h.setAside(tenOFive.Add(time.Hour))
	expectKinds(t, h.take("chat"))
}

// A report sent before a node raised its covered incident to sos, and
// applied after the raise, never lowers the incident below sos: the entry
// does not cover it again, so the sos stays open everywhere.
func TestAnOlderReportNeverSetsAsideAnSos(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn", "sos"), "hotline": hotline()})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	raise := tenOFive.Add(time.Hour)
	h.arrive(raise, h.alert("app-1", checksandalerts.FailureIncidentEvents.Raised, "sos", versionA, 0, raise))
	expectKinds(t, h.take("chat"), "failure_alert_severity_raised sos")
	expectKinds(t, h.take("hotline"), "failure_alert_severity_raised sos")
	// The report that the node sent just before the raise.
	h.report(raise.Add(time.Second), map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionA, 0, tenOFive, raise.Add(-time.Second))})
	expectKinds(t, h.take("chat"))
	expectKinds(t, h.take("hotline"))
	if n := h.heldRows("app-1"); n != 2 {
		t.Fatal("the sos incident left the sets:", n)
	}
}

// A set-aside entry that closes a key ends the pending changes of every row
// of the key, also of a node that recovered after resolved-sos: the key
// that opens again names it in no update.
func TestASetAsideClosingEndsThePendingChangesOfTheKey(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline()})
	h.arrive(tenOFive, h.alert("app-1", "failed", "sos", versionA, 0, tenOFive), h.alert("app-2", "failed", "warn", versionA, 0, tenOFive))
	h.take("hotline")
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-1", "recovered", "sos", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("hotline"), "recovered resolved-sos")
	h.setAside(tenOFive.Add(2*time.Minute), entryFor(versionA, reasonNoPAM))
	expectKinds(t, h.take("hotline"), "closed_without_recovery resolved-warn")
	later := tenOFive.Add(time.Hour)
	h.arrive(later, h.alert("app-3", "failed", "warn", versionB, 0, later))
	expectKinds(t, h.take("hotline"), "failed warn")
	h.arrive(later.Add(5*time.Minute), h.alert("app-4", "failed", "warn", versionB, 0, later.Add(5*time.Minute)))
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	got := h.take("hotline")
	expectKinds(t, got, "finding_nodes_changed warn")
	if strings.Contains(got[0].MMHotlineAlert.Message, "app-1") {
		t.Fatalf("the update names app-1: %q", got[0].MMHotlineAlert.Message)
	}
}

// The one opening for the incidents that set-aside entries no longer
// cover names an exact finding only when MMHotline gets its line whole:
// the alert's own text, with every node's name and every entry's reason,
// counts toward the bound. An exact finding that does not fit goes in the
// next update, so each one reaches MMHotline.
func TestTheOpeningAfterASetAsideNamesOnlyWhatFits(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline()})
	versions := []string{versionA, versionB, versionC}
	entries := []config.SetAsideFinding{}
	for i, v := range versions {
		entries = append(entries, entryFor(v, fmt.Sprintf("reason %d: %s", i, strings.Repeat("r", 4000))))
	}
	h.setAside(tenOFive, entries...)
	at := tenOFive.Add(time.Minute)
	reports := map[string]store.ReportedIncidents{}
	for i, v := range versions {
		for n := range 20 {
			node := fmt.Sprintf("app-%d-%02d-%s", i, n, strings.Repeat("x", 200))
			reports[node] = shows(node, "warn", v, 0, tenOFive, at)
		}
	}
	h.report(at, reports)
	expectKinds(t, h.take("hotline"))
	h.setAside(at.Add(time.Minute))
	sent := []string{}
	for _, a := range h.take("hotline") {
		sent = append(sent, wire(t, a)["message"])
	}
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	for _, a := range h.take("hotline") {
		sent = append(sent, wire(t, a)["message"])
	}
	all := strings.Join(sent, "\n")
	for _, v := range versions {
		if !strings.Contains(all, v) {
			t.Fatalf("MMHotline never got %q", v)
		}
	}
}

// The opening's bound counts its own text as it goes out, with only the
// exact findings that it names. Here leaving the last exact finding out
// changes which 20 nodes the text names, from that finding's names to
// long ones of the others. The message stays within its bound, so
// MMHotline clips no exact finding that the opening names, and the rest
// goes in the next update.
func TestTheOpeningAfterASetAsideCountsTheTextItSends(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline()})
	versions := []string{versionA, versionB, versionC}
	entries := []config.SetAsideFinding{}
	for i, v := range versions {
		entries = append(entries, entryFor(v, fmt.Sprintf("reason %d", i)))
	}
	h.setAside(tenOFive, entries...)
	at := tenOFive.Add(time.Minute)
	reports := map[string]store.ReportedIncidents{}
	add := func(node, exactFinding string) {
		reports[node] = shows(node, "warn", exactFinding, 0, tenOFive, at)
	}
	// The nodes in the order of their names: a and b each lead an exact
	// finding of 21 nodes, then the 19 of the last exact finding, then the
	// other 40 with long names.
	add("a", versionA)
	add("b", versionB)
	for n := range 19 {
		add(fmt.Sprintf("c-%02d-%s", n, strings.Repeat("c", 60)), versionC)
	}
	for n := range 20 {
		add(fmt.Sprintf("z-a-%02d-%s", n, strings.Repeat("x", 240)), versionA)
		add(fmt.Sprintf("z-b-%02d-%s", n, strings.Repeat("x", 240)), versionB)
	}
	h.report(at, reports)
	expectKinds(t, h.take("hotline"))
	h.setAside(at.Add(time.Minute))
	opening := h.take("hotline")
	expectKinds(t, opening, "failed warn")
	sent := []string{wire(t, opening[0])["message"]}
	if len(sent[0]) > store.UpdateMessageLimit {
		t.Fatalf("the opening is %d bytes, more than %d", len(sent[0]), store.UpdateMessageLimit)
	}
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	for _, a := range h.take("hotline") {
		sent = append(sent, wire(t, a)["message"])
	}
	all := strings.Join(sent, "\n")
	for _, v := range versions {
		if !strings.Contains(all, v) {
			t.Fatalf("MMHotline never got %q", v)
		}
	}
}

// The one opening for several nodes that set-aside entries no longer cover
// names their exact finding in its body, also when they share one, so a
// chat shows which exact finding it announces.
func TestTheOpeningAfterASetAsideNamesItsExactFinding(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	at := tenOFive.Add(time.Minute)
	h.report(at, map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionA, 0, tenOFive, at), "app-2": shows("app-2", "warn", versionA, 0, tenOFive, at)})
	expectKinds(t, h.take("chat"))
	h.setAside(at.Add(time.Minute))
	got := h.take("chat")
	expectKinds(t, got, "failed warn")
	if !strings.Contains(got[0].Body, versionA) || !strings.Contains(got[0].Body, reasonNoPAM) {
		t.Fatalf("the opening's body %q", got[0].Body)
	}
}
