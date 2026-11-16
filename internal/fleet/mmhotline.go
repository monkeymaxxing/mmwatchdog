package fleet

import (
	"fmt"
	"sort"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// The mmhotline destination: MMWatchdog alert events become MMHotline's generic
// signed alerts, with one MMHotline incident key per check across the
// fleet, and the MMWatchdog fleet hub or a standalone node sends check-ins. The
// rules are in docs/changing-mmwatchdog/design/alerts.md under "MMHotline" and
// docs/changing-mmwatchdog/design/fleet.md under "MMHotline in a fleet".

const (
	CheckInEvery = 30 * time.Second
	// MaxNodesInWholeNodeMaintenance is MMHotline's limit for one check-in.
	MaxNodesInWholeNodeMaintenance = 1000
	// CheckInBytes keeps a check-in well under MMHotline's
	// 64 KiB body limit, which it answers with an unsigned 413.
	CheckInBytes = 60 * 1024
)

// NodeInMaintenance is one node in a whole-node maintenance window, as an
// check-in lists it.
type NodeInMaintenance struct {
	NodeName         string    `json:"node_name"`
	MaintenanceUntil time.Time `json:"maintenance_until"`
	Reason           string    `json:"reason,omitempty"`
	StartedBy        string    `json:"started_by,omitempty"`
}

// CheckIn tells MMHotline that the MMWatchdog fleet hub or standalone node
// sending it is running, and which nodes are in whole-node maintenance.
type CheckIn struct {
	SentAt                      time.Time           `json:"sent_at"`
	SentByNodeName              string              `json:"sent_by_node_name"`
	NodesInWholeNodeMaintenance []NodeInMaintenance `json:"nodes_in_whole_node_maintenance,omitempty"`
}

// FitCheckIn encodes a check-in within MMHotline's limits: at
// most maxNodes windows and at most maxBytes. The sender's own window comes
// first and is always kept, because MMHotline treats only that one as planned
// downtime for the check-ins; the others follow by node name. It leaves
// out every window's reason and user first, then the last windows, and says
// so.
func FitCheckIn(m CheckIn, maxBytes, maxNodes int) ([]byte, string) {
	nodes := m.NodesInWholeNodeMaintenance
	total := len(nodes)
	own := func(n NodeInMaintenance) bool { return n.NodeName == m.SentByNodeName }
	sort.Slice(nodes, func(i, j int) bool {
		if own(nodes[i]) != own(nodes[j]) {
			return own(nodes[i])
		}
		return nodes[i].NodeName < nodes[j].NodeName
	})
	nodes = nodes[:min(len(nodes), maxNodes)]
	m.NodesInWholeNodeMaintenance = nodes
	body := checksandalerts.EncodeWithoutHTMLEscaping(m)
	if len(body) <= maxBytes && len(nodes) == total {
		return body, ""
	}
	bare := len(body) > maxBytes
	if bare {
		plain := make([]NodeInMaintenance, len(nodes))
		for i, n := range nodes {
			plain[i] = NodeInMaintenance{NodeName: n.NodeName, MaintenanceUntil: n.MaintenanceUntil}
		}
		m.NodesInWholeNodeMaintenance = plain
		body = checksandalerts.EncodeWithoutHTMLEscaping(m)
	}
	for len(body) > maxBytes && len(m.NodesInWholeNodeMaintenance) > 1 {
		listed := len(m.NodesInWholeNodeMaintenance)
		keep := max(min(listed*maxBytes/len(body), listed-1), 1)
		m.NodesInWholeNodeMaintenance = m.NodesInWholeNodeMaintenance[:keep]
		body = checksandalerts.EncodeWithoutHTMLEscaping(m)
	}
	note := fmt.Sprintf("The check-in listed %d of the %d nodes in whole-node maintenance", len(m.NodesInWholeNodeMaintenance), total)
	if bare {
		note += ", without reasons and users,"
	}
	return body, note + " to stay within MMHotline's limits of 1,000 nodes and 64 KiB."
}

// MMHotlineStatus is what mmwatchdog status shows about the mmhotline destination.
// The status JSON takes its field names from these tags.
type MMHotlineStatus struct {
	Destination           string     `json:"destination"`
	URL                   string     `json:"mmhotline_url"`
	AlertSourceID         string     `json:"mmhotline_alert_source_id"`
	LastSignedAnswerAt    *time.Time `json:"last_signed_answer_at,omitempty"`
	NodeClockAheadSeconds *int64     `json:"node_clock_ahead_of_mmhotline_seconds"`
	DeliveryProblems      []string   `json:"delivery_problems"`
	ClockWarning          string     `json:"clock_warning,omitempty"`
	CheckInShortened      string     `json:"check_in_shortened,omitempty"`
}

// WholeNodeMaintenance lists the other nodes whose latest report shows a
// current whole-node window, a silent node's included.
func (h *Hub) WholeNodeMaintenance(now time.Time) []NodeInMaintenance {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	nodes := []NodeInMaintenance{}
	for name, m := range h.Nodes {
		if name == h.Name || m.NodeStatusReport == nil {
			continue
		}
		if w, ok := m.NodeStatusReport.Maintenance[checksandalerts.EveryCheckGroup]; ok && w.Until.After(now) {
			nodes = append(nodes, NodeInMaintenance{name, w.Until, w.Reason, w.User})
		}
	}
	return nodes
}
