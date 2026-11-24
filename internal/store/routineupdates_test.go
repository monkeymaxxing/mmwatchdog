package store_test

import (
	"fmt"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

// The alerts that a fleet gets from the checks of security updates, whose
// waiting updates are one finding keyed to the node's release, and from a
// known exploited flaw, which is one finding for each flaw. Simulated
// nodes run their check through the node's incident rules
// (incidents.CheckState) and send what it alerts through the hub's actual
// intake, alert grouping step, report reconciliation, and update step, on
// a fake clock, one simulated minute at a time, from 00:00 to the shared
// tick at 12:00 UTC. The counts are those of the destinations, an
// MMHotline destination and a chat, and of the fleet incidents that the
// hub shows. Nothing here depends on the machine's speed.

// The finding of a known exploited flaw, and the release of the nodes.
const (
	exploitedFlaw = "CVE-2026-123158 openssh"
	debian13      = "debian 13"
)

// simulatedNode is a node that runs one check whose module reports
// findings, and keeps its open incidents for its reports.
type simulatedNode struct {
	name  string
	check *incidents.CheckState
	open  map[string]checksandalerts.Event
}

// simulatedTiming alerts at the first failed run and recovers at the
// first healthy one, as the module pages' examples of these checks do.
var simulatedTiming = checksandalerts.Timing{CheckEvery: "1h", TimeLimitPerRun: "2m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}

func newSimulatedNode(name, check string) *simulatedNode {
	return &simulatedNode{name: name, open: map[string]checksandalerts.Event{},
		check: &incidents.CheckState{CheckGroup: "software", Check: check, OneIncidentForEachFinding: true, Timing: simulatedTiming, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}}
}

// run observes one run at at: failed with the finding, its exact finding,
// and the message, or healthy for an empty finding. It returns the alerts
// that the node sends, as the hub receives them.
func (n *simulatedNode) run(h *groupingHub, at time.Time, finding, exactFinding, message string) []checksandalerts.Event {
	o := checksandalerts.Observation{Protocol: 1, Status: "ok", Message: "no security update waits to be installed", At: at}
	if finding != "" {
		o.Status, o.Message = "failed", message
		o.Findings = []checksandalerts.Finding{{ID: finding, Message: message, ExactFinding: exactFinding}}
	}
	sent := []checksandalerts.Event{}
	for _, e := range n.check.Observe(o, simulatedTiming, false) {
		if !e.Notify {
			continue
		}
		h.events++
		e.ID, e.Node = fmt.Sprintf("event-%07d", h.events), n.name
		e.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(e, "failed", "")), n.name, groupingFleet)
		e.WriteAlertTexts([]checksandalerts.Event{e}, "")
		switch {
		case e.Type == "failed":
			n.open[e.Incident] = e
		case checksandalerts.Closes(e.Type):
			delete(n.open, e.Incident)
		}
		sent = append(sent, e)
	}
	return sent
}

// report is the node's status report sent at sent: its open incidents.
func (n *simulatedNode) report(sent time.Time) store.ReportedIncidents {
	r := store.ReportedIncidents{Sent: sent, Incidents: map[string]checksandalerts.Event{}, ReminderEligible: map[string]bool{}}
	for id, e := range n.open {
		e.AlertSeveritiesSentForIncident = []string{e.AlertSeverity}
		r.Incidents[id], r.ReminderEligible[id] = e, true
	}
	return r
}

// routineHub is a hub with an MMHotline destination and a chat, which
// counts what each destination gets by the alert's type and severity.
type routineHub struct {
	*groupingHub
	counts map[string]map[string]int
}

func newRoutineHub(t *testing.T) *routineHub {
	return &routineHub{groupingHub: newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline(), "chat": chatTaking("https://chat.example.com", "warn", "sos")}), counts: map[string]map[string]int{"hotline": {}, "chat": {}}}
}

// minute passes the alerts and reports of one simulated minute through the
// hub at at, runs the update step, and counts what the destinations get.
func (h *routineHub) minute(at time.Time, alerts []checksandalerts.Event, reports map[string]store.ReportedIncidents) {
	h.t.Helper()
	h.arrive(at, alerts...)
	if len(reports) > 0 {
		h.report(at, reports)
	}
	h.tick(at)
	for name, counts := range h.counts {
		for _, kind := range kinds(h.take(name)) {
			counts[kind]++
		}
	}
}

// shown is what the hub's fleet view counts of the simulated check: the
// fleet incidents, one for each key with an open node incident, and the
// open node incidents.
func (h *routineHub) shown() (fleetIncidents, nodeIncidents int) {
	h.t.Helper()
	if e := h.s.DB.QueryRow("SELECT COUNT(DISTINCT incident_key), COUNT(*) FROM fleet_node_incidents").Scan(&fleetIncidents, &nodeIncidents); e != nil {
		h.t.Fatal(e)
	}
	return fleetIncidents, nodeIncidents
}

// expectCounts fails unless each destination got exactly want, by alert
// type and severity.
func (h *routineHub) expectCounts(want map[string]int) {
	h.t.Helper()
	for name, got := range h.counts {
		if fmt.Sprint(got) != fmt.Sprint(want) {
			h.t.Errorf("%s got %v, want %v", name, got, want)
		}
	}
}

// waitingAdvisories is the message of dnf_security_updates for n advisories
// of the kernel, as the module writes it: one package, however many
// advisories name it.
func waitingAdvisories(n int) string {
	return fmt.Sprintf("1 security update waits to be installed: kernel (%d advisories)", n)
}

// simulatedDay is every minute from 00:00 to the shared tick at 12:00 UTC.
var simulatedDay = time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

func everyMinuteUntilNoon(each func(at time.Time)) {
	for at := simulatedDay; !at.After(simulatedDay.Add(12 * time.Hour)); at = at.Add(time.Minute) {
		each(at)
	}
}

// (a) One node with 27 waiting kernel advisories, and one more advisory
// later: one alert to each destination, and one fleet incident.
func TestOneNodeWith27KernelAdvisoriesSendsOneAlert(t *testing.T) {
	t.Parallel()
	h := newRoutineHub(t)
	n := newSimulatedNode("db01", "dnf_security_updates")
	advisories := 27
	everyMinuteUntilNoon(func(at time.Time) {
		if at.Minute() != 7 {
			h.minute(at, nil, nil)
			return
		}
		if at.Hour() == 5 {
			advisories++
		}
		alerts := n.run(h.groupingHub, at, checksandalerts.PackageSecurity, debian13, waitingAdvisories(advisories))
		h.minute(at, alerts, map[string]store.ReportedIncidents{n.name: n.report(at)})
	})
	fleetIncidents, nodeIncidents := h.shown()
	t.Logf("(a) 1 node, 27 kernel advisories, a 28th at 05:07: hotline %v, chat %v; fleet incidents shown %d, node incidents %d", h.counts["hotline"], h.counts["chat"], fleetIncidents, nodeIncidents)
	h.expectCounts(map[string]int{"failed warn": 1})
	if fleetIncidents != 1 || nodeIncidents != 1 {
		t.Fatalf("the hub shows %d fleet incidents of %d node incidents, want 1 of 1", fleetIncidents, nodeIncidents)
	}
}

// (b) 1,000 nodes report the same waiting updates, the first failures
// drifting over 6 hours, each node running hourly. Every 10th node
// installs every update at its second run and gets a new one at its
// third, which opens a new incident; every 7th installs some at its second
// run and still waits. The destinations get one first alert and the
// growth updates of the shared ticks at 06:00 and 12:00, never one alert
// for each node.
func TestAThousandNodesWithWaitingUpdatesSendOneAlertAndTheTicksUpdates(t *testing.T) {
	t.Parallel()
	const nodes = 1000
	h := newRoutineHub(t)
	fleet := make([]*simulatedNode, nodes)
	firstRun := make([]time.Duration, nodes)
	for i := range fleet {
		fleet[i] = newSimulatedNode(fmt.Sprintf("app-%04d", i), "apt_security_updates")
		firstRun[i] = (time.Duration(i) * 6 * time.Hour / nodes).Truncate(time.Minute)
	}
	everyMinuteUntilNoon(func(at time.Time) {
		alerts, reports := []checksandalerts.Event{}, map[string]store.ReportedIncidents{}
		for i, n := range fleet {
			since := at.Sub(simulatedDay.Add(firstRun[i]))
			if since < 0 || since%time.Hour != 0 {
				continue
			}
			finding, message := checksandalerts.PackageSecurity, "3 security updates wait to be installed: curl, libssl3, openssl"
			switch run := int(since / time.Hour); {
			case run == 1 && i%10 == 0:
				finding = ""
			case run >= 1 && i%7 == 0:
				message = "1 security update waits to be installed: curl"
			}
			alerts = append(alerts, n.run(h.groupingHub, at, finding, debian13, message)...)
			reports[n.name] = n.report(at)
		}
		h.minute(at, alerts, reports)
	})
	fleetIncidents, nodeIncidents := h.shown()
	t.Logf("(b) 1,000 nodes, drifting over 6 h, every 10th recovering and reopening: hotline %v, chat %v; fleet incidents shown %d, node incidents %d", h.counts["hotline"], h.counts["chat"], fleetIncidents, nodeIncidents)
	for name, counts := range h.counts {
		updates := counts[checksandalerts.FindingNodesChanged+" warn"] + counts[checksandalerts.FindingNodesChanged+" info"]
		if counts["failed warn"] != 1 || updates < 1 || updates > 2 || counts["failed warn"]+updates != sum(counts) {
			t.Errorf("%s got %v, want 1 first alert and 1 or 2 updates", name, counts)
		}
	}
	if fleetIncidents != 1 || nodeIncidents != nodes {
		t.Fatalf("the hub shows %d fleet incidents of %d node incidents, want 1 of %d", fleetIncidents, nodeIncidents, nodes)
	}
}

// (b2) 300 nodes wait for updates that nobody installs. They get one first
// alert and the 06:00 update; a new advisory that reaches every node at
// 07:00 changes their messages and no exact finding, and sends nothing.
func TestANewAdvisoryOnStuckNodesSendsNothing(t *testing.T) {
	t.Parallel()
	const nodes = 300
	h := newRoutineHub(t)
	fleet := make([]*simulatedNode, nodes)
	for i := range fleet {
		fleet[i] = newSimulatedNode(fmt.Sprintf("web-%03d", i), "zypper_security_patches")
	}
	var beforeTheAdvisory map[string]map[string]int
	everyMinuteUntilNoon(func(at time.Time) {
		if at.Equal(simulatedDay.Add(7 * time.Hour)) {
			beforeTheAdvisory = map[string]map[string]int{"hotline": copyCounts(h.counts["hotline"]), "chat": copyCounts(h.counts["chat"])}
		}
		alerts, reports := []checksandalerts.Event{}, map[string]store.ReportedIncidents{}
		for i, n := range fleet {
			if at.Sub(simulatedDay.Add(time.Duration(i)*12*time.Second))%time.Hour >= time.Minute {
				continue
			}
			message := "1 security patch waits to be installed: openSUSE-Leap-16.1-12 (important): Security update for libXtst"
			if at.Hour() >= 7 {
				message = "2 security patches wait to be installed: openSUSE-Leap-16.1-12 (important): Security update for libXtst, openSUSE-Leap-16.1-31 (important): Security update for curl"
			}
			alerts = append(alerts, n.run(h.groupingHub, at, checksandalerts.PackageSecurity, debian13, message)...)
			reports[n.name] = n.report(at)
		}
		h.minute(at, alerts, reports)
	})
	fleetIncidents, nodeIncidents := h.shown()
	t.Logf("(b2) 300 stuck nodes: before the new advisory at 07:00 hotline %v, chat %v; at 12:00 hotline %v, chat %v; fleet incidents shown %d, node incidents %d",
		beforeTheAdvisory["hotline"], beforeTheAdvisory["chat"], h.counts["hotline"], h.counts["chat"], fleetIncidents, nodeIncidents)
	h.expectCounts(map[string]int{"failed warn": 1, checksandalerts.FindingNodesChanged + " warn": 1})
	for name := range h.counts {
		if fmt.Sprint(beforeTheAdvisory[name]) != fmt.Sprint(h.counts[name]) {
			t.Errorf("the new advisory sent %s more than %v: %v", name, beforeTheAdvisory[name], h.counts[name])
		}
	}
	if fleetIncidents != 1 || nodeIncidents != nodes {
		t.Fatalf("the hub shows %d fleet incidents of %d node incidents, want 1 of %d", fleetIncidents, nodeIncidents, nodes)
	}
}

// (c) 5,000 nodes report one known exploited flaw with the same exact
// finding, each once in 6 hours on its own schedule: one alert and the
// growth update of the 06:00 tick, with every node but the first.
func TestFiveThousandNodesWithOneExploitedFlawSendOneAlertAndAnUpdate(t *testing.T) {
	t.Parallel()
	const nodes = 5000
	h := newRoutineHub(t)
	fleet := make([]*simulatedNode, nodes)
	firstRun := make([]time.Duration, nodes)
	for i := range fleet {
		fleet[i] = newSimulatedNode(fmt.Sprintf("node-%04d", i), "known_exploited_flaws")
		firstRun[i] = (time.Duration(i) * 6 * time.Hour / nodes).Truncate(time.Minute)
	}
	updated := 0
	everyMinuteUntilNoon(func(at time.Time) {
		alerts, reports := []checksandalerts.Event{}, map[string]store.ReportedIncidents{}
		for i, n := range fleet {
			if since := at.Sub(simulatedDay.Add(firstRun[i])); since < 0 || since%(6*time.Hour) != 0 {
				continue
			}
			alerts = append(alerts, n.run(h.groupingHub, at, exploitedFlaw, versionA, "openssh has "+exploitedFlaw)...)
			reports[n.name] = n.report(at)
		}
		h.arrive(at, alerts...)
		if len(reports) > 0 {
			h.report(at, reports)
		}
		h.tick(at)
		for name, counts := range h.counts {
			for _, a := range h.take(name) {
				counts[kinds([]checksandalerts.Event{a})[0]]++
				if name == "chat" && a.Type == checksandalerts.FindingNodesChanged {
					updated += a.NodeCount
				}
			}
		}
	})
	fleetIncidents, nodeIncidents := h.shown()
	t.Logf("(c) 5,000 nodes, one exploited flaw: hotline %v, chat %v; the updates name %d nodes; fleet incidents shown %d, node incidents %d", h.counts["hotline"], h.counts["chat"], updated, fleetIncidents, nodeIncidents)
	for name, counts := range h.counts {
		updates := counts[checksandalerts.FindingNodesChanged+" warn"]
		if counts["failed warn"] != 1 || updates < 1 || updates > 2 || counts["failed warn"]+updates != sum(counts) {
			t.Errorf("%s got %v, want 1 first alert and 1 or 2 updates", name, counts)
		}
	}
	if updated != nodes-1 || fleetIncidents != 1 || nodeIncidents != nodes {
		t.Fatalf("the updates name %d nodes, want %d; the hub shows %d fleet incidents of %d node incidents, want 1 of %d", updated, nodes-1, fleetIncidents, nodeIncidents, nodes)
	}
}

func sum(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

func copyCounts(counts map[string]int) map[string]int {
	out := map[string]int{}
	for kind, n := range counts {
		out[kind] = n
	}
	return out
}
