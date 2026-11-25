package fleet_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
)

// The hub's views of set-aside findings, from the design of setting a
// finding aside, "The fleet view" and "Listing for the weekly review".

const (
	flaw     = "CVE-2026-123158 openssh"
	versionA = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none"
	versionB = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u5; fixed version: none"
	versionC = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u6; fixed version: none"
	reason   = "Needs musl without PAM"
)

func entry(exactFinding string) config.SetAsideFinding {
	return config.SetAsideFinding{Check: "packages/flaws", Finding: flaw, ExactFinding: exactFinding, Reason: reason, InvestigationNote: "notes.md", SetAsideBy: "triage agent", SetAsideOn: "2026-11-23"}
}

// report is a node's report at at. With a severity, its check
// packages/flaws has the flaw open at that severity with exactFinding; with
// the state unknown, the check could not run.
func report(at time.Time, state, severity, exactFinding string) *fleet.NodeStatusReport {
	r := &fleet.NodeStatusReport{StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: -1, Sent: at, AlertSeverityWhenThisNodeGoesSilent: "warn",
		Summary: incidents.StatusSummary{Attention: []incidents.Attention{}}, Checks: map[string]fleet.ReportedCheck{}}
	switch {
	case state == "unknown":
		r.Summary = incidents.StatusSummary{ExitCode: 3, Attention: []incidents.Attention{{Check: "packages/flaws", State: "unknown", Message: "timed out"}}}
		r.Checks["packages/flaws"] = fleet.ReportedCheck{State: "unknown"}
	case severity != "":
		t := &incidents.Track{Incident: incidents.ID(), Opened: at, AlertSeverity: severity, CurrentExactFinding: exactFinding, Evidence: &checksandalerts.Evidence{FailingSince: at.Add(-time.Hour)}}
		r.Summary = incidents.StatusSummary{ExitCode: 1, Attention: []incidents.Attention{{Check: "packages/flaws", Finding: flaw, State: "failed", AlertSeverity: severity, Since: at, Message: exactFinding}}}
		r.Checks["packages/flaws"] = fleet.ReportedCheck{State: "failed", Findings: map[string]fleet.ReportedCheck{flaw: {State: "failed", Message: exactFinding, FailureIncident: t}}}
	default:
		r.Checks["packages/flaws"] = fleet.ReportedCheck{State: "ok"}
	}
	return r
}

type testFleet struct {
	t   *testing.T
	hub *fleet.Hub
}

func newTestFleet(t *testing.T, at time.Time, entries ...config.SetAsideFinding) testFleet {
	t.Helper()
	h, e := fleet.OpenHub(t.TempDir(), "hub.test:8445", "util", 30*time.Second, config.DefaultHubLimits, at)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close() })
	setAside := config.SetAsideFindings(entries)
	h.SetAsideFindings.Store(&setAside)
	return testFleet{t, h}
}

// node joins a node at at and, with a report, saves it.
func (f testFleet) node(name string, at time.Time, r *fleet.NodeStatusReport) {
	f.t.Helper()
	if name != f.hub.Name {
		if _, status, e := f.hub.Join(name, f.hub.Code.Secret, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn"}, "10.0.0.1", at); status != http.StatusOK || e != nil {
			f.t.Fatal(status, e)
		}
	}
	if r != nil {
		if _, status, e := f.hub.Accept(name, r, at); status != http.StatusOK || e != nil {
			f.t.Fatal(status, e)
		}
	}
}

// A covered finding below sos does not make its node unhealthy, leaves the
// node's attention list, and is counted apart: one finding on two nodes.
// An sos node with the same exact finding, and a node with another exact
// finding, still need attention.
func TestTheFleetViewCountsSetAsideFindingsApart(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	f := newTestFleet(t, at, entry(versionA))
	f.node("util", at, report(at, "ok", "", ""))
	f.node("app-1", at, report(at, "failed", "warn", versionA))
	f.node("app-2", at, report(at, "failed", "info", versionA))
	f.node("app-3", at, report(at, "failed", "sos", versionA))
	f.node("db-1", at, report(at, "failed", "warn", versionB))
	view := f.hub.Fleet(at.Add(time.Second))
	if view.ExitCode != 1 || view.SetAsideFindingCount != 1 || view.NodesWithSetAsideFindings != 2 {
		t.Fatalf("exit %d, %d findings on %d nodes", view.ExitCode, view.SetAsideFindingCount, view.NodesWithSetAsideFindings)
	}
	codes := map[string]int{}
	for _, n := range view.Nodes {
		codes[n.Name] = n.ExitCode
	}
	if codes["app-1"] != 0 || codes["app-2"] != 0 || codes["app-3"] != 1 || codes["db-1"] != 1 {
		t.Fatal(codes)
	}
	node, e := f.hub.Node("app-1", at.Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	if node.ExitCode != 0 || len(node.NodeStatusReport.Summary.Attention) != 0 || node.NodeStatusReport.Summary.ExitCode != 0 ||
		len(node.SetAsideFindings) != 1 || node.SetAsideFindings[0] != (fleet.SetAsideFindingOfNode{Check: "packages/flaws", Finding: flaw, ExactFinding: versionA, Reason: reason}) {
		t.Fatalf("%+v", node)
	}
	// The node's own report is not changed: only the view leaves the
	// finding out.
	again, _ := f.hub.Node("app-1", at.Add(time.Second))
	if len(again.SetAsideFindings) != 1 {
		t.Fatal("the view changed the saved report")
	}
	// A node with both a set-aside finding and another attention entry keeps
	// the remaining entry's AlertSeverity after set-aside filtering.
	mixed := report(at, "failed", "warn", versionA)
	mixed.Summary.Attention = append(mixed.Summary.Attention, incidents.Attention{Check: "web/http", State: "failed", AlertSeverity: "sos", Since: at, Message: "503"})
	mixed.Checks["web/http"] = fleet.ReportedCheck{State: "failed", FailureIncident: &incidents.Track{Incident: incidents.ID(), Opened: at, AlertSeverity: "sos"}}
	f.node("app-4", at, mixed)
	mixedView, err := f.hub.Node("app-4", at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(mixedView.NodeStatusReport.Summary.Attention) != 1 || mixedView.NodeStatusReport.Summary.Attention[0].AlertSeverity != "sos" || mixedView.NodeStatusReport.Summary.Attention[0].Check != "web/http" {
		t.Fatalf("set-aside filtering lost alert_severity: %+v", mixedView.NodeStatusReport.Summary.Attention)
	}
	// Without the entry, the fleet is as the nodes report it.
	none := config.SetAsideFindings{}
	f.hub.SetAsideFindings.Store(&none)
	if view := f.hub.Fleet(at.Add(time.Second)); view.SetAsideFindingCount != 0 || view.Nodes[0].ExitCode != 1 {
		t.Fatalf("%+v", view)
	}
}

// The listing for the weekly review: each entry with the nodes that it
// covers now, whether it matches no node over every latest saved report
// (silent nodes too), and how many nodes cannot say.
func TestTheListingOfSetAsideFindings(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	at := start.Add(time.Hour)
	f := newTestFleet(t, start, entry(versionA), entry(versionC), entry(versionB))
	// old-1 reported versionC once and went silent.
	f.node("old-1", start, report(start, "failed", "warn", versionC))
	f.node("util", at, report(at, "ok", "", ""))
	for i := range 25 {
		f.node(nodeName(i), at, report(at, "failed", "warn", versionA))
	}
	f.node("app-sos", at, report(at, "failed", "sos", versionA))
	f.node("unk-1", at, report(at, "unknown", "", ""))
	// unk-2's check cannot run now, while another flaw keeps its incident
	// open, so its state is failed.
	other := report(at, "failed", "warn", "another exact finding")
	check := other.Checks["packages/flaws"]
	check.Findings = map[string]fleet.ReportedCheck{"CVE-2026-1 other": check.Findings[flaw]}
	check.CannotCheckIncident = &incidents.Track{Incident: incidents.ID(), Opened: at, AlertSeverity: "warn"}
	other.Checks["packages/flaws"] = check
	f.node("unk-2", at, other)
	// new-1 joined and has not reported yet.
	f.node("new-1", at, nil)
	listing := f.hub.SetAsideFindingsNow(at.Add(time.Second))
	if len(listing) != 3 || listing[0].ExactFinding != versionA || listing[1].ExactFinding != versionC || listing[2].ExactFinding != versionB {
		t.Fatalf("%+v", listing)
	}
	a, c, b := listing[0], listing[1], listing[2]
	if a.CoveredNodeCount != 25 || len(a.CoveredNodes) != fleet.CoveredNodesShown || a.CoveredNodes[0] != nodeName(0) || slices.Contains(a.CoveredNodes, "app-sos") || a.MatchesNoNodeNow {
		t.Fatalf("A: %+v", a)
	}
	if c.CoveredNodeCount != 1 || c.CoveredNodes[0] != "old-1" || c.MatchesNoNodeNow {
		t.Fatalf("C, on a silent node: %+v", c)
	}
	if b.CoveredNodeCount != 0 || len(b.CoveredNodes) != 0 || !b.MatchesNoNodeNow {
		t.Fatalf("B: %+v", b)
	}
	// new-1 waits for its first report, unk-1's check is unknown, and
	// unk-2's has an open cannot-check incident.
	for _, e := range listing {
		if e.NodesThatCannotCheckNow != 3 {
			t.Fatalf("%s: %d nodes cannot check", e.ExactFinding, e.NodesThatCannotCheckNow)
		}
	}
}

// mmwatchdog fleet findings lists every node with the check's findings, with no
// limit, each with its exact finding, when its failing time began, and
// whether an entry covers it.
func TestTheFindingsOfACheck(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	f := newTestFleet(t, at, entry(versionA))
	for i := range 30 {
		f.node(nodeName(i), at, report(at, "failed", "warn", versionA))
	}
	f.node("app-sos", at, report(at, "failed", "sos", versionA))
	f.node("db-1", at, report(at, "failed", "warn", versionB))
	f.node("ok-1", at, report(at, "ok", "", ""))
	found := f.hub.Findings("packages/flaws", "", at.Add(time.Second))
	if found.Check != "packages/flaws" || len(found.Findings) != 1 || found.Findings[0].Finding != flaw || len(found.Findings[0].Nodes) != 32 {
		t.Fatalf("%+v", found)
	}
	nodes := found.Findings[0].Nodes
	first, sos, other := nodes[0], nodes[30], nodes[31]
	if first.Node != nodeName(0) || first.ExactFinding != versionA || !first.FailingSince.Equal(at.Add(-time.Hour)) || !first.SetAside || first.SetAsideReason != reason || first.NodeState != "reporting" {
		t.Fatalf("%+v", first)
	}
	if sos.Node != "app-sos" || sos.SetAside || other.Node != "db-1" || other.ExactFinding != versionB || other.SetAside {
		t.Fatalf("%+v %+v", sos, other)
	}
	if found := f.hub.Findings("packages/flaws", "CVE-0000-1 other", at); len(found.Findings) != 0 {
		t.Fatalf("%+v", found)
	}
}

func nodeName(i int) string {
	return "app-" + string(rune('a'+i/10)) + string(rune('0'+i%10))
}
