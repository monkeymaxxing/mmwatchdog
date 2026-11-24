package fleet

import (
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

// What the hub's fleet incident sets learn from the reports: each report's
// announced incidents, and the incidents that the node sent without the
// hub, kept in memory until the delivery worker's next step applies them
// (store.ReconcileIncidentSets). The rules are in docs/changing-mmwatchdog/design/fleet.md under
// "One incident per check across the fleet".

// FleetIncidentSets is the name of the storage operation that applies the
// reports to the hub's fleet incident sets.
const FleetIncidentSets = "fleet incident sets"

// SetAsideFindingsSettlement is the name of the storage operation that
// settles the entries of set_aside_findings.yml against the open node
// incidents, at a start and at each reload.
const SetAsideFindingsSettlement = "set-aside findings"

// announcedIncidents lists what a report of reporter says for the fleet
// incident sets: its announced incidents, each with when it opened and the
// message of its check, and the incidents that it sent without the hub, by
// destination.
func announcedIncidents(reporter string, r *NodeStatusReport) store.ReportedIncidents {
	found := store.ReportedIncidents{Sent: r.Sent, Incidents: map[string]checksandalerts.Event{}, SentWithoutTheHub: map[string]map[string]checksandalerts.Event{}, ReminderEligible: map[string]bool{}}
	for _, d := range r.IncidentsSentWithoutTheHub {
		if found.SentWithoutTheHub[d.Destination] == nil {
			found.SentWithoutTheHub[d.Destination] = map[string]checksandalerts.Event{}
		}
		found.SentWithoutTheHub[d.Destination][d.Incident] = d.Carried(reporter)
	}
	for key, c := range r.Checks {
		group, check, _ := strings.Cut(key, "/")
		type track struct {
			t                   *incidents.Track
			kind, unit, message string
		}
		tracks := []track{{c.FailureIncident, "failed", "", c.Message}, {c.CannotCheckIncident, "cannot_check", "", c.Message}}
		// Each failed unit of an every_systemd_unit check has its own
		// incident and its own key.
		for unit, u := range c.Findings {
			tracks = append(tracks, track{u.FailureIncident, "failed", unit, u.Message})
		}
		for _, track := range tracks {
			if t := track.t; t != nil && t.Incident != "" && t.Announced == t.Incident {
				found.Incidents[t.Incident] = checksandalerts.Event{Node: reporter, CheckGroup: group, Check: check, Finding: track.unit, Type: track.kind, Incident: t.Incident, AlertSeverity: t.AlertSeverity, AlertSeveritiesSentForIncident: t.AnnouncedAlertSeverities, SendAlertsOnlyToDestinations: c.SendAlertsOnlyToDestinations,
					At: t.Opened, Message: track.message, ExactFinding: t.CurrentExactFinding, ExactFindingSequence: t.ExactFindingSequence}
				found.ReminderEligible[t.Incident] = t.ReminderEligible
			}
		}
	}
	return found
}

// AwaitingFirstReports reports that some joined node has not reported since
// the hub started or it joined, and has not yet gone silent: three report
// intervals bound the wait.
func (h *Hub) AwaitingFirstReports(now time.Time) bool {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	for name, m := range h.Nodes {
		if name != h.Name && h.State(m, now) == "waiting_for_first_report" {
			return true
		}
	}
	return false
}

// TakeReported returns the reports accepted since the last call, by node.
func (h *Hub) TakeReported() map[string]store.ReportedIncidents {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	changes := h.Reported
	h.Reported = map[string]store.ReportedIncidents{}
	return changes
}

// RestoreReported puts back reports that could not be applied, unless a
// newer one arrived meanwhile.
func (h *Hub) RestoreReported(changes map[string]store.ReportedIncidents) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	for name, r := range changes {
		if _, newer := h.Reported[name]; !newer {
			h.Reported[name] = r
		}
	}
}
