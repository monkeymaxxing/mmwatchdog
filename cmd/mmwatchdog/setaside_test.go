package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
)

const (
	setAsideFlaw     = "CVE-2026-123158 openssh"
	setAsideVersionA = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none"
	setAsideVersionB = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u5; fixed version: none"
)

// mmwatchdog fleet shows set-aside findings in one line, in the right
// number form, and they set no exit code; mmwatchdog fleet NODE shows the
// finding as set aside until the exact finding changes.
func TestTheFleetViewShowsSetAsideFindingsInOneLine(t *testing.T) {
	now := time.Now().UTC()
	healthy := &fleet.NodeStatusReport{Summary: incidents.StatusSummary{Attention: []incidents.Attention{}}}
	for _, c := range []struct {
		findings, nodes int
		want            string
	}{{1, 1, "Set aside: 1 finding on 1 node\n"}, {1, 5000, "Set aside: 1 finding on 5000 nodes\n"}, {2, 3, "Set aside: 2 findings on 3 nodes\n"}} {
		b, _ := json.Marshal(fleet.Fleet{Address: "util:8445", Nodes: []fleet.FleetNode{{Name: "app-1", State: "reporting", LastReport: now, NodeStatusReport: healthy}},
			SetAsideFindingCount: c.findings, NodesWithSetAsideFindings: c.nodes})
		output, err := capture(t, func() error { return printFleet(b, false, now) })
		if err != nil || !strings.Contains(output, "1 node, 1 healthy\n"+c.want) {
			t.Fatalf("%q %v", output, err)
		}
	}
	b, _ := json.Marshal(fleet.Fleet{Address: "util:8445", Nodes: []fleet.FleetNode{{Name: "app-1", State: "reporting", LastReport: now, NodeStatusReport: healthy}}})
	if output, _ := capture(t, func() error { return printFleet(b, false, now) }); strings.Contains(output, "Set aside") {
		t.Fatal(output)
	}
	node := fleet.FleetNode{Name: "app-1", State: "reporting", LastReport: now, NodeStatusReport: &fleet.NodeStatusReport{Summary: incidents.StatusSummary{Attention: []incidents.Attention{}},
		Checks: map[string]fleet.ReportedCheck{"packages/flaws": {State: "failed", Message: "1 finding", Findings: map[string]fleet.ReportedCheck{setAsideFlaw: {State: "failed", Message: setAsideVersionA}}}}},
		SetAsideFindings: []fleet.SetAsideFindingOfNode{{Check: "packages/flaws", Finding: setAsideFlaw, ExactFinding: setAsideVersionA, Reason: "Needs musl without PAM"}}}
	b, _ = json.Marshal(node)
	output, err := capture(t, func() error { return printNode(b, false, now) })
	if err != nil || !strings.Contains(output, "  "+setAsideFlaw+"    failed       set aside (Needs musl without PAM) until the exact finding changes\n") {
		t.Fatalf("%q %v", output, err)
	}
}

func TestFleetFindingsShowsEachExactFindingAndItsNodes(t *testing.T) {
	since := time.Date(2026, 11, 22, 22, 51, 12, 0, time.UTC)
	found := fleet.FindingsOfCheck{Check: "packages/flaws", Findings: []fleet.FindingOnNodes{{Finding: setAsideFlaw, Nodes: []fleet.NodeFinding{
		{Node: "app-1", NodeState: "reporting", State: "failed", AlertSeverity: "warn", ExactFinding: setAsideVersionA, FailingSince: since, SetAside: true, SetAsideReason: "Needs musl without PAM"},
		{Node: "db-1", NodeState: "silent", State: "failed", AlertSeverity: "sos", ExactFinding: setAsideVersionA, FailingSince: since},
		{Node: "util-1", NodeState: "reporting", State: "failed", AlertSeverity: "warn", ExactFinding: setAsideVersionB, FailingSince: since},
	}}}}
	b, _ := json.Marshal(found)
	output, err := capture(t, func() error { return printFindings(b, false) })
	want := "packages/flaws: 1 finding on 3 nodes\n" + setAsideFlaw + "\n" +
		"  " + setAsideVersionA + ": 2 nodes\n" +
		"    app-1                    failed       warn  failing since 2026-11-22T22:51:12Z set aside (Needs musl without PAM)\n" +
		"    db-1                     failed       sos   failing since 2026-11-22T22:51:12Z (node silent)\n" +
		"  " + setAsideVersionB + ": 1 node\n" +
		"    util-1                   failed       warn  failing since 2026-11-22T22:51:12Z\n"
	if err != nil || output != want {
		t.Fatalf("got\n%s\nwant\n%s", output, want)
	}
	if output, _ = capture(t, func() error { return printFindings(b, true) }); !strings.Contains(output, `"set_aside": true`) || !strings.Contains(output, `"failing_since": "2026-11-22T22:51:12Z"`) {
		t.Fatal(output)
	}
}

func TestFleetSetAsideFindingsShowsEachEntry(t *testing.T) {
	entry := config.SetAsideFinding{Check: "packages/flaws", Finding: setAsideFlaw, ExactFinding: setAsideVersionA, Reason: "Needs musl without PAM",
		InvestigationNote: "triage_agent_notes/openssh.md", SetAsideBy: "triage agent (MMHotline run b82fdd8b)", SetAsideOn: "2026-11-23"}
	nodes := []string{}
	for i := range fleet.CoveredNodesShown {
		nodes = append(nodes, "app-"+string(rune('a'+i)))
	}
	listing := map[string]any{"set_aside_findings": []fleet.SetAsideFindingNow{
		{SetAsideFinding: entry, CoveredNodeCount: 25, CoveredNodes: nodes, NodesThatCannotCheckNow: 1},
		{SetAsideFinding: entry, CoveredNodes: []string{}, MatchesNoNodeNow: true},
	}}
	b, _ := json.Marshal(listing)
	output, err := capture(t, func() error { return printSetAsideFindings(b, false) })
	for _, want := range []string{
		"packages/flaws: " + setAsideFlaw + "\n  Exact finding: " + setAsideVersionA + "\n  Reason: Needs musl without PAM\n  Investigation note: triage_agent_notes/openssh.md\n  Set aside by triage agent (MMHotline run b82fdd8b) on 2026-11-23\n",
		"  Covers 25 nodes now: app-a, app-b, ", ", app-t, and 5 more\n  Matches a node now\n  Nodes that cannot check now: 1\n\n",
		"  Covers 0 nodes now\n  Matches no node now\n  Nodes that cannot check now: 0\n",
	} {
		if err != nil || !strings.Contains(output, want) {
			t.Fatalf("missing %q in\n%s (%v)", want, output, err)
		}
	}
	output, _ = capture(t, func() error { return printSetAsideFindings(b, true) })
	for _, field := range []string{"check", "finding", "exact_finding", "reason", "investigation_note", "set_aside_by", "set_aside_on", "covered_node_count", "covered_nodes", "matches_no_node_now", "nodes_that_cannot_check_now"} {
		if !strings.Contains(output, `"`+field+`":`) {
			t.Errorf("the JSON lacks %s: %s", field, output)
		}
	}
	b, _ = json.Marshal(map[string]any{"set_aside_findings": []fleet.SetAsideFindingNow{}})
	if output, _ = capture(t, func() error { return printSetAsideFindings(b, false) }); output != "No finding is set aside: the hub has no entry in /etc/mmwatchdog/set_aside_findings.yml.\n" {
		t.Fatal(output)
	}
}

// The two commands check their arguments before they ask the service.
func TestFleetFindingsArguments(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"fleet", "findings"}, "fleet findings requires CHECK_GROUP/CHECK"},
		{[]string{"fleet", "findings", "flaws"}, "fleet findings requires CHECK_GROUP/CHECK"},
		{[]string{"fleet", "findings", "packages/flaws", "--finding", ""}, "--finding requires the id of a finding"},
		{[]string{"fleet", "findings", "packages/flaws", "--finding", setAsideFlaw, "--socket", socket}, "absent.sock"},
		{[]string{"fleet", "app-1", "--finding", setAsideFlaw}, "--finding is a flag of mmwatchdog fleet findings"},
		{[]string{"fleet", "findings", "packages/flaws", "--last", "1h"}, "event time options require mmwatchdog fleet NODE"},
		{[]string{"fleet", "set-aside-findings", "--socket", socket}, "absent.sock"},
		{[]string{"fleet", "set-aside-findings", "extra"}, "unexpected arguments"},
	} {
		err := run(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v", tc.args, err)
		}
		var code exitStatus
		if errors.As(err, &code) || errorCode(err) != 4 {
			t.Errorf("%v: exit %d", tc.args, errorCode(err))
		}
	}
}
