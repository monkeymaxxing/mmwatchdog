package store_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/store"
)

// fallBack queues node's alert as its fallback does when the hub is
// unreachable, for the destinations of the fleet settings.
func fallBack(t *testing.T, s *store.Store, alert checksandalerts.Event, destinations map[string]checksandalerts.Destination) {
	t.Helper()
	tx, e := s.DB.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e = store.QueueDirect(tx, alert.At.UnixNano(), alert, []checksandalerts.Event{alert}, destinations, store.QueuedBy{Role: store.FallbackSender, Node: alert.Node, FleetIdentity: groupingFleet}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}

// accept delivers every alert that waits in s's outbox, each accepted by
// its destination, and returns them in the order of delivery.
func accept(t *testing.T, s *store.Store) []checksandalerts.Event {
	t.Helper()
	out := []checksandalerts.Event{}
	for {
		heads, e := s.Pending()
		if e != nil {
			t.Fatal(e)
		}
		if len(heads) == 0 {
			return out
		}
		for _, d := range heads {
			out = append(out, d.Event)
			if _, e = s.Delivered(d, nil); e != nil {
				t.Fatal(e)
			}
		}
	}
}

// A joined node that delivers a finding with an exact finding without the
// hub keeps the record of the direct send for a chat too, and leaves the
// closing alert to the hub. Other findings keep today's direct recovery.
// The record exists only once the destination has accepted the alert; while
// the alert waits to be retried, the recovery still leaves the closing to
// the hub.
func TestAJoinedNodeLeavesTheClosingAlertOfAGroupedFindingToTheHub(t *testing.T) {
	t.Parallel()
	node, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer node.Close()
	h := &groupingHub{t: t, s: node}
	destinations := map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")}
	grouped := h.alert("app-2", "failed", "warn", versionA, 0, tenOFive)
	plain := h.alert("app-2", "failed", "warn", "", 0, tenOFive)
	plain.Incident, plain.Finding = "incident-plain", "other"
	for _, alert := range []checksandalerts.Event{grouped, plain} {
		fallBack(t, node, alert, destinations)
		if records, e := node.SentWithoutTheHub(); e != nil || len(records) != 0 {
			t.Fatalf("a direct send is recorded before the destination accepted it: %+v %v", records, e)
		}
		recovery := alert
		recovery.ID, recovery.Type, recovery.At = alert.ID+"-recovered", "recovered", tenOFive.Add(time.Minute)
		fallBack(t, node, recovery, destinations)
	}
	expectKinds(t, accept(t, node), "failed warn", "failed warn", "recovered warn")
	records, e := node.SentWithoutTheHub()
	if e != nil || len(records) != 1 || records[0].Incident != grouped.Incident || records[0].ExactFinding != versionA || records[0].Destination != "chat" {
		t.Fatalf("the records of direct sends %+v %v", records, e)
	}
}

// The hub settles a direct send of a finding with an exact finding,
// whether or not the incident is in the destination's set: the node joins
// the next update, so the agent sees the node, and the record is
// acknowledged afterwards.
func TestTheHubSettlesADirectSendAndTheNodeJoinsTheNextUpdate(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	record := store.IncidentSentWithoutTheHub{Destination: "chat", Incident: "incident-app-2", CheckGroup: "packages", Check: "flaws", Finding: flaw, Type: "failed", AlertSeveritiesSentForIncident: []string{"warn"}, ExactFinding: versionB}
	known, e := h.s.IncidentsKnownToTheHub("app-2", []store.IncidentSentWithoutTheHub{record})
	if e != nil || len(known) != 0 {
		t.Fatal("a record is acknowledged before the hub settled it", known, e)
	}
	report := shows("app-2", "warn", versionB, 0, tenOFive.Add(time.Minute), tenOFive.Add(10*time.Minute))
	report.SentWithoutTheHub = map[string]map[string]checksandalerts.Event{"chat": {record.Incident: {Node: "app-2", CheckGroup: "packages", Check: "flaws", Finding: flaw, Type: "failed", Incident: record.Incident, AlertSeverity: "warn", AlertSeveritiesSentForIncident: []string{"warn"}, ExactFinding: versionB}}}
	h.report(tenOFive.Add(10*time.Minute), map[string]store.ReportedIncidents{"app-2": report})
	// The node's direct send never announces on the hub.
	expectKinds(t, h.take("chat"))
	known, e = h.s.IncidentsKnownToTheHub("app-2", []store.IncidentSentWithoutTheHub{record})
	if e != nil || len(known) != 1 {
		t.Fatal("the settled record is not acknowledged", known, e)
	}
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	got := h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	if g := got[0].NodesByExactFinding; len(g) != 1 || g[0].ExactFinding != versionB || !slices.Equal(g[0].Nodes, []string{"app-2"}) {
		t.Fatalf("the update %+v", g)
	}
	// The hub sends the closing alert when the last node recovers.
	h.arrive(tenOFive.Add(7*time.Hour), h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(7*time.Hour)))
	h.report(tenOFive.Add(8*time.Hour), map[string]store.ReportedIncidents{"app-2": {Sent: tenOFive.Add(8 * time.Hour)}})
	expectKinds(t, h.take("chat"), "recovered warn")
}

// carriedTo is report with its incident also carried as sent without the
// hub to destination, under the MMHotline alert source source.
func carriedTo(report store.ReportedIncidents, destination, source string) store.ReportedIncidents {
	report.SentWithoutTheHub = map[string]map[string]checksandalerts.Event{destination: {}}
	for id, incident := range report.Incidents {
		incident.MMHotlineAlertSource = source
		report.SentWithoutTheHub[destination][id] = incident
	}
	return report
}

// The direct send of a set-aside incident closes its key only when no
// other node holds the key open: decided after every report of the batch
// is applied, and not before every node has reported since the hub
// started. Until then the node keeps carrying the record.
func TestASetAsideDirectSendClosesNothingThatAnotherNodeHoldsOpen(t *testing.T) {
	t.Parallel()
	destinations := map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()}
	set := tenOFive.Add(10 * time.Minute)
	setAside := carriedTo(shows("app-1", "warn", versionA, 0, tenOFive, set), "hotline", nodeAlertSource)
	open := carriedTo(shows("app-2", "warn", versionB, 0, tenOFive, set), "hotline", nodeAlertSource)
	record := store.IncidentSentWithoutTheHub{Destination: "hotline", Incident: "incident-app-1", CheckGroup: "packages", Check: "flaws", Finding: flaw, Type: "failed", AlertSeveritiesSentForIncident: []string{"warn"}, ExactFinding: versionA, AlertSource: nodeAlertSource}
	t.Run("in one batch", func(t *testing.T) {
		t.Parallel()
		h := newGroupingHub(t, destinations)
		h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
		h.report(set, map[string]store.ReportedIncidents{"app-1": setAside, "app-2": open})
		expectKinds(t, h.take("hotline"))
	})
	t.Run("while the hub awaits reports", func(t *testing.T) {
		t.Parallel()
		h := newGroupingHub(t, destinations)
		h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
		if e := h.s.ReconcileIncidentSets(map[string]store.ReportedIncidents{"app-1": setAside}, destinations, true, set); e != nil {
			t.Fatal(e)
		}
		expectKinds(t, h.take("hotline"))
		if known, e := h.s.IncidentsKnownToTheHub("app-1", []store.IncidentSentWithoutTheHub{record}); e != nil || len(known) != 0 {
			t.Fatal("the record is acknowledged before the hub could decide", known, e)
		}
		h.report(set.Add(time.Minute), map[string]store.ReportedIncidents{"app-2": open, "app-1": setAside})
		expectKinds(t, h.take("hotline"))
		if known, e := h.s.IncidentsKnownToTheHub("app-1", []store.IncidentSentWithoutTheHub{record}); e != nil || len(known) != 1 {
			t.Fatal("the record is not acknowledged", known, e)
		}
	})
}

// A node that sends an incident directly again, in a later hub outage,
// after the hub settled its first direct send, sends a new version of the
// record: the hub settles that version too, and acknowledges it only
// then. Here the set-aside incident rose to sos, which is never set aside,
// went to MMHotline under the node source, and recovered before the hub
// was back: the hub resolves it there.
func TestALaterDirectSendOfTheSameIncidentIsSettledAgain(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	first := carriedTo(shows("app-1", "warn", versionA, 0, tenOFive, tenOFive.Add(10*time.Minute)), "hotline", nodeAlertSource)
	h.report(tenOFive.Add(10*time.Minute), map[string]store.ReportedIncidents{"app-1": first})
	expectKinds(t, h.take("hotline"), "closed_without_recovery resolved-warn")
	record := store.IncidentSentWithoutTheHub{Destination: "hotline", Incident: "incident-app-1", CheckGroup: "packages", Check: "flaws", Finding: flaw, Type: "failed", AlertSeveritiesSentForIncident: []string{"warn"}, ExactFinding: versionA, AlertSource: nodeAlertSource}
	if known, e := h.s.IncidentsKnownToTheHub("app-1", []store.IncidentSentWithoutTheHub{record}); e != nil || len(known) != 1 {
		t.Fatal(known, e)
	}
	// The second outage: the raise to sos went directly, and the incident
	// recovered before the hub was back.
	record.AlertSeveritiesSentForIncident = []string{"warn", "sos"}
	if known, e := h.s.IncidentsKnownToTheHub("app-1", []store.IncidentSentWithoutTheHub{record}); e != nil || len(known) != 0 {
		t.Fatal("a new direct send is acknowledged before the hub settled it", known, e)
	}
	later := tenOFive.Add(5 * time.Hour)
	raised := first.SentWithoutTheHub["hotline"]["incident-app-1"]
	raised.AlertSeverity, raised.AlertSeveritiesSentForIncident = "sos", []string{"warn", "sos"}
	h.report(later, map[string]store.ReportedIncidents{"app-1": {Sent: later, SentWithoutTheHub: map[string]map[string]checksandalerts.Event{"hotline": {"incident-app-1": raised}}}})
	got := h.take("hotline")
	expectKinds(t, got, "recovered resolved-sos")
	if sourceOf(got[0]) != nodeAlertSource {
		t.Fatal("the resolution goes under", sourceOf(got[0]))
	}
	if known, e := h.s.IncidentsKnownToTheHub("app-1", []store.IncidentSentWithoutTheHub{record}); e != nil || len(known) != 1 {
		t.Fatal("the settled record is not acknowledged", known, e)
	}
}

// A set-aside incident never blocks its node's direct-send records: the
// hub settles the record, and the destination, which got the direct send,
// gets closed_without_recovery when nothing else of the key is open for
// it.
func TestADirectSendOfASetAsideIncidentIsSettled(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	// The set-aside entry that covers the incident on the hub.
	h.setAside(tenOFive, entryFor(versionA, "Needs musl without PAM"))
	record := store.IncidentSentWithoutTheHub{Destination: "chat", Incident: "incident-app-2", CheckGroup: "packages", Check: "flaws", Finding: flaw, Type: "failed", AlertSeveritiesSentForIncident: []string{"warn"}, ExactFinding: versionA}
	report := shows("app-2", "warn", versionA, 0, tenOFive, tenOFive.Add(10*time.Minute))
	report.SentWithoutTheHub = map[string]map[string]checksandalerts.Event{"chat": {record.Incident: {Node: "app-2", CheckGroup: "packages", Check: "flaws", Finding: flaw, Type: "failed", Incident: record.Incident, AlertSeverity: "warn", AlertSeveritiesSentForIncident: []string{"warn"}, ExactFinding: versionA}}}
	h.report(tenOFive.Add(10*time.Minute), map[string]store.ReportedIncidents{"app-2": report})
	got := h.take("chat")
	expectKinds(t, got, "closed_without_recovery warn")
	if got[0].Message != "Set aside after investigation: Needs musl without PAM" {
		t.Fatal(got[0].Message)
	}
	if known, e := h.s.IncidentsKnownToTheHub("app-2", []store.IncidentSentWithoutTheHub{record}); e != nil || len(known) != 1 {
		t.Fatal("the record of a set-aside incident is not acknowledged", known, e)
	}
	h.report(tenOFive.Add(11*time.Minute), map[string]store.ReportedIncidents{"app-2": report})
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	expectKinds(t, h.take("chat"))
}

// carrying is a report of reporter, sent at sent, that shows no incident
// and carries the node's records of direct sends, as the node's report
// carries them (fleet.announcedIncidents).
func carrying(t *testing.T, node *store.Store, reporter string, sent time.Time) store.ReportedIncidents {
	t.Helper()
	records, e := node.SentWithoutTheHub()
	if e != nil {
		t.Fatal(e)
	}
	r := store.ReportedIncidents{Sent: sent, SentWithoutTheHub: map[string]map[string]checksandalerts.Event{}}
	for _, d := range records {
		if r.SentWithoutTheHub[d.Destination] == nil {
			r.SentWithoutTheHub[d.Destination] = map[string]checksandalerts.Event{}
		}
		r.SentWithoutTheHub[d.Destination][d.Incident] = checksandalerts.Event{Node: reporter, CheckGroup: d.CheckGroup, Check: d.Check, Finding: d.Finding, Type: d.Type, Incident: d.Incident,
			AlertSeverity: config.HighestSeverity(d.AlertSeveritiesSentForIncident), AlertSeveritiesSentForIncident: d.AlertSeveritiesSentForIncident, ExactFinding: d.ExactFinding, ExactFindingSequence: d.ExactFindingSequence, MMHotlineAlertSource: d.AlertSource}
	}
	return r
}

// A direct sos that MMHotline asks the node to retry, and that lands only
// after the node recovered and the hub resolved the key, opens an incident
// under the node source. The node records the send only once MMHotline
// accepted it, so no report settles it before it lands, and the hub,
// learning of it from the next report, resolves it there although it had
// resolved the key already. The same holds when no report came between
// the recovery and the landing. A later genuine sos is a new alert, not
// part of a stale incident.
func TestARetriedDirectSosThatLandsAfterTheResolutionIsResolved(t *testing.T) {
	t.Parallel()
	for _, reportInBetween := range []bool{true, false} {
		t.Run(fmt.Sprintf("a report in between: %v", reportInBetween), func(t *testing.T) {
			t.Parallel()
			destinations := map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()}
			h := newGroupingHub(t, destinations)
			h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
			expectKinds(t, h.take("hotline"), "failed warn")
			// The hub is unreachable: the node raises to sos and sends it
			// directly; MMHotline asks it to retry.
			node := newJoinedNode(t)
			raised := h.alert("app-1", checksandalerts.FailureIncidentEvents.Raised, "sos", versionA, 0, tenOFive.Add(time.Hour))
			fallBack(t, node.s, raised, destinations)
			heads, e := node.s.Pending()
			if e != nil || len(heads) != 1 {
				t.Fatal(heads, e)
			}
			if _, e = node.s.Delivered(heads[0], &checksandalerts.RetryLater{After: time.Minute}); e != nil {
				t.Fatal(e)
			}
			if records, e := node.s.SentWithoutTheHub(); e != nil || len(records) != 0 {
				t.Fatal("a send that waits to be retried is recorded", records, e)
			}
			// The node recovers and reaches the hub again: the hub resolves
			// what it knows.
			back := tenOFive.Add(2 * time.Hour)
			h.arrive(back, h.alert("app-1", "recovered", "warn", versionA, 0, back))
			got := h.take("hotline")
			expectKinds(t, got, "recovered resolved-warn")
			if sourceOf(got[0]) != "source-1" {
				t.Fatal("the hub resolved under", sourceOf(got[0]))
			}
			if reportInBetween {
				h.report(back.Add(time.Second), map[string]store.ReportedIncidents{"app-1": carrying(t, node.s, "app-1", back.Add(time.Second))})
				expectKinds(t, h.take("hotline"))
			}
			// The retry lands in MMHotline, under the node source, and the
			// next report carries it.
			expectKinds(t, accept(t, node.s), checksandalerts.FailureIncidentEvents.Raised+" sos")
			later := back.Add(time.Minute)
			report := carrying(t, node.s, "app-1", later)
			if len(report.SentWithoutTheHub["hotline"]) != 1 {
				t.Fatalf("the accepted send is not carried: %+v", report)
			}
			h.report(later, map[string]store.ReportedIncidents{"app-1": report})
			got = h.take("hotline")
			expectKinds(t, got, "recovered resolved-sos")
			if sourceOf(got[0]) != nodeAlertSource {
				t.Fatal("the resolution of the direct sos goes under", sourceOf(got[0]))
			}
			records, e := node.s.SentWithoutTheHub()
			if e != nil {
				t.Fatal(e)
			}
			known, e := h.s.IncidentsKnownToTheHub("app-1", records)
			if e != nil || len(known) != 1 {
				t.Fatal("the settled record is not acknowledged", known, e)
			}
			// A later genuine sos opens anew.
			again := h.alert("app-1", "failed", "sos", versionA, 0, later.Add(time.Hour))
			again.Incident = "incident-app-1-again"
			h.arrive(later.Add(time.Hour), again)
			got = h.take("hotline")
			expectKinds(t, got, "failed sos")
			if sourceOf(got[0]) != "source-1" {
				t.Fatal("the new sos goes under", sourceOf(got[0]))
			}
		})
	}
}
