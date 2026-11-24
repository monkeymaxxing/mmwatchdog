package fleet

import (
	"slices"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
)

// The hub's views of set-aside findings and of the findings of one check,
// from each node's latest saved report and the entries of
// set_aside_findings.yml in force (Hub.SetAsideFindings). The hub's alerts
// follow the same entries through its store (store.SettleSetAsideFindings);
// these views only read. The rules are in
// docs/changing-mmwatchdog/design/fleet.md under "Set a finding aside".

func (h *Hub) setAsideFindings() config.SetAsideFindings {
	if p := h.SetAsideFindings.Load(); p != nil {
		return *p
	}
	return nil
}

// SetAsideFindingOfNode is a finding of a node that a set-aside entry
// covers now: the fleet views count it apart, and it needs no attention.
type SetAsideFindingOfNode struct {
	Check        string `json:"check"`
	Finding      string `json:"finding"`
	ExactFinding string `json:"exact_finding"`
	Reason       string `json:"reason"`
}

// setAsideOnNode lists the findings of report that an entry of setAside
// covers: each with an open failure incident below sos whose exact finding
// the entry names.
func setAsideOnNode(report *NodeStatusReport, setAside config.SetAsideFindings) []SetAsideFindingOfNode {
	if report == nil || len(setAside) == 0 {
		return nil
	}
	var out []SetAsideFindingOfNode
	for _, key := range checksandalerts.Names(report.Checks) {
		c := report.Checks[key]
		for _, id := range checksandalerts.Names(c.Findings) {
			t := c.Findings[id].FailureIncident
			if t == nil || t.Incident == "" {
				continue
			}
			if entry, covered := setAside.Covering(key, id, t.CurrentExactFinding, t.AlertSeverity); covered {
				out = append(out, SetAsideFindingOfNode{Check: key, Finding: id, ExactFinding: entry.ExactFinding, Reason: entry.Reason})
			}
		}
	}
	return out
}

// withoutSetAsideFindings is the summary of report without the findings
// covered: they leave its attention list and no longer count for its exit
// code. Every other entry counts as Summarize counts it: a finding or a
// failure incident needs attention (1), anything else is unknown health
// (3), and so is a storage problem of the node.
func withoutSetAsideFindings(report *NodeStatusReport, covered []SetAsideFindingOfNode) incidents.StatusSummary {
	summary := incidents.StatusSummary{Attention: []incidents.Attention{}}
	if report.StorageError != "" {
		summary.ExitCode = 3
	}
	for _, a := range report.Summary.Attention {
		if a.Finding != "" && slices.ContainsFunc(covered, func(f SetAsideFindingOfNode) bool { return f.Check == a.Check && f.Finding == a.Finding }) {
			continue
		}
		summary.Attention = append(summary.Attention, a)
		code := 3
		if t := report.Checks[a.Check].FailureIncident; a.Finding != "" || t != nil && t.Incident != "" {
			code = 1
		}
		summary.ExitCode = incidents.HealthExitCode(summary.ExitCode, code)
	}
	return summary
}

// NodeFinding is one node's finding of a check, for mmwatchdog fleet
// findings: the finding's state and severity, its exact finding, when its
// failing time began, and whether a set-aside entry covers it.
type NodeFinding struct {
	Node           string    `json:"node"`
	NodeState      string    `json:"node_state"`
	State          string    `json:"state"`
	AlertSeverity  string    `json:"alert_severity,omitempty"`
	ExactFinding   string    `json:"exact_finding,omitempty"`
	FailingSince   time.Time `json:"failing_since,omitzero"`
	SetAside       bool      `json:"set_aside"`
	SetAsideReason string    `json:"set_aside_reason,omitempty"`
}

// FindingOnNodes is one finding of a check and every node that has it.
type FindingOnNodes struct {
	Finding string        `json:"finding"`
	Nodes   []NodeFinding `json:"nodes"`
}

// FindingsOfCheck is the answer of mmwatchdog fleet findings.
type FindingsOfCheck struct {
	Check    string           `json:"check"`
	Findings []FindingOnNodes `json:"findings"`
}

// Findings lists every node whose latest saved report has a finding of the
// check (CHECK_GROUP/CHECK), or only the finding with the ID finding when
// it is not "", with no limit on the count: silent nodes and the hub too.
// Findings are in the order of their IDs, and the nodes of each in the
// order of their exact findings, then of their names.
func (h *Hub) Findings(check, finding string, now time.Time) FindingsOfCheck {
	setAside := h.setAsideFindings()
	h.Mu.Lock()
	defer h.Mu.Unlock()
	byFinding := map[string][]NodeFinding{}
	// The nodes of each finding are sorted below, so the map's order does
	// not matter here.
	for name, m := range h.Nodes {
		if m.NodeStatusReport == nil {
			continue
		}
		c, has := m.NodeStatusReport.Checks[check]
		if !has {
			continue
		}
		for id, f := range c.Findings {
			if finding != "" && id != finding {
				continue
			}
			n := NodeFinding{Node: name, NodeState: h.State(m, now), State: f.State}
			if t := f.FailureIncident; t != nil {
				n.AlertSeverity, n.ExactFinding = t.AlertSeverity, t.CurrentExactFinding
				if t.Evidence != nil {
					n.FailingSince = t.Evidence.FailingSince
				}
				if t.Incident != "" {
					if entry, covered := setAside.Covering(check, id, t.CurrentExactFinding, t.AlertSeverity); covered {
						n.SetAside, n.SetAsideReason = true, entry.Reason
					}
				}
			}
			byFinding[id] = append(byFinding[id], n)
		}
	}
	out := FindingsOfCheck{Check: check, Findings: []FindingOnNodes{}}
	for _, id := range checksandalerts.Names(byFinding) {
		nodes := byFinding[id]
		slices.SortFunc(nodes, func(a, b NodeFinding) int {
			if c := strings.Compare(a.ExactFinding, b.ExactFinding); c != 0 {
				return c
			}
			return strings.Compare(a.Node, b.Node)
		})
		out.Findings = append(out.Findings, FindingOnNodes{Finding: id, Nodes: nodes})
	}
	return out
}

// SetAsideFindingNow is an entry of set_aside_findings.yml in force, with
// what it covers now, for mmwatchdog fleet set-aside-findings: the nodes
// that it covers, up to CoveredNodesShown of them; whether it matches no
// node, over every node's latest saved report, also the hub's and those of
// silent nodes; and how many nodes cannot say whether they have its exact
// finding. An entry may go only when it matches no node and that count is 0.
type SetAsideFindingNow struct {
	config.SetAsideFinding
	CoveredNodeCount        int      `json:"covered_node_count"`
	CoveredNodes            []string `json:"covered_nodes"`
	MatchesNoNodeNow        bool     `json:"matches_no_node_now"`
	NodesThatCannotCheckNow int      `json:"nodes_that_cannot_check_now"`
}

// CoveredNodesShown is how many covered nodes the listing of a set-aside
// entry names, so that the listing stays small in a large fleet; it counts
// the rest, and mmwatchdog fleet findings lists them all.
const CoveredNodesShown = 20

// SetAsideFindingsNow lists the entries of set_aside_findings.yml in force,
// in the file's order, each with what it covers now. A node cannot check
// now when it has no saved report, waits for its first report, or its
// check of the entry is unknown.
func (h *Hub) SetAsideFindingsNow(now time.Time) []SetAsideFindingNow {
	setAside := h.setAsideFindings()
	h.Mu.Lock()
	defer h.Mu.Unlock()
	out := []SetAsideFindingNow{}
	for _, entry := range setAside {
		e := SetAsideFindingNow{SetAsideFinding: entry, CoveredNodes: []string{}, MatchesNoNodeNow: true}
		for _, name := range checksandalerts.Names(h.Nodes) {
			m := h.Nodes[name]
			report := m.NodeStatusReport //nolint:nilaway // every writer of h.Nodes (Load, Join) stores an allocated member, and name comes from h.Nodes.
			c, has := ReportedCheck{}, false
			if report != nil {
				c, has = report.Checks[entry.Check]
			}
			// A check that cannot run while another finding keeps its
			// incident open is failed, not unknown: its open cannot-check
			// incident says that it cannot check.
			cannotCheck := has && (c.State == "unknown" || c.CannotCheckIncident != nil && c.CannotCheckIncident.Incident != "")
			if report == nil || h.State(m, now) == "waiting_for_first_report" || cannotCheck {
				e.NodesThatCannotCheckNow++
			}
			t := c.Findings[entry.Finding].FailureIncident
			if t == nil || t.CurrentExactFinding != entry.ExactFinding {
				continue
			}
			e.MatchesNoNodeNow = false
			if t.Incident != "" && t.AlertSeverity != "sos" {
				e.CoveredNodeCount++
				if len(e.CoveredNodes) < CoveredNodesShown {
					e.CoveredNodes = append(e.CoveredNodes, name)
				}
			}
		}
		out = append(out, e)
	}
	return out
}
