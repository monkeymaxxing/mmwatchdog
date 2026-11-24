package store_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// The tests of grouping by exact finding on the hub, from the design of
// grouping by exact finding, "The rule, per destination and key". The hub
// takes its nodes' alerts through its actual intake and alert grouping
// step, and its reports through ReconcileIncidentSets, on a fake clock.

const groupingFleet = "fleet-certificate-fingerprint"

// The flaw of the test fleet, and its exact findings.
const (
	flaw          = "CVE-2026-123158 openssh"
	versionA      = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none"
	versionB      = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u5; fixed version: none"
	versionC      = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u6; fixed version: none"
	readableFlaw  = "packages/flaws: " + flaw + " failed"
	everySixHours = 6 * time.Hour
)

// tenOFive is the time of the first failure in these tests: the shared tick
// of 6 h after it is at 12:00 UTC.
var tenOFive = time.Date(2026, 11, 24, 10, 5, 0, 0, time.UTC)

func chatTaking(url string, severities ...string) checksandalerts.Destination {
	return checksandalerts.Destination{RemindEvery: "never", AlertSeverities: severities, Inputs: &checksandalerts.Webhook{URL: url}, TimeLimitPerDeliveryAttempt: "1s"}
}

func hotline() checksandalerts.Destination {
	return checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.MMHotlineDestination{URL: "https://hotline.example.com", AlertSourceID: "source-1", SigningSecretFile: "/etc/mmwatchdog/hotline"}, TimeLimitPerDeliveryAttempt: "1s"}
}

// groupingHub is a hub's store with its alert grouping step.
type groupingHub struct {
	t            *testing.T
	dir          string
	s            *store.Store
	destinations map[string]checksandalerts.Destination
	grouping     fleet.AlertGroupingState
	events       int
}

func newGroupingHub(t *testing.T, destinations map[string]checksandalerts.Destination) *groupingHub {
	t.Helper()
	h := &groupingHub{t: t, dir: filepath.Join(t.TempDir(), "state"), destinations: destinations, grouping: fleet.AlertGroupingState{Sent: map[string]time.Time{}, Quiet: map[string]time.Time{}, Window: fleet.AlertGroupingWindow}}
	h.open()
	return h
}

func (h *groupingHub) open() {
	h.t.Helper()
	s, e := store.OpenStore(h.dir, config.DefaultStorageLimits)
	if e != nil {
		h.t.Fatal(e)
	}
	s.FleetIdentity = groupingFleet
	s.NewlyAffectedNodesEvery.Store(int64(everySixHours))
	h.s = s
	h.t.Cleanup(func() { s.Close() })
}

// alert is node's alert of kind about the flaw, at severity, with
// exactFinding and its sequence number.
func (h *groupingHub) alert(node, kind, severity, exactFinding string, sequence int64, at time.Time) checksandalerts.Event {
	h.events++
	e := checksandalerts.Event{ID: fmt.Sprintf("event-%06d", h.events), Incident: "incident-" + node, Node: node, CheckGroup: "packages", Check: "flaws", Finding: flaw, Type: kind,
		AlertSeverity: severity, ExactFinding: exactFinding, ExactFindingSequence: sequence, At: at, Message: exactFinding}
	if checksandalerts.Closes(kind) {
		e.AlertSeveritiesSentForIncident = []string{severity}
	}
	e.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(e, "failed", "")), node, groupingFleet)
	e.WriteAlertTexts([]checksandalerts.Event{e}, "")
	return e
}

// arrive passes alerts through the hub's intake and alert grouping step
// at at.
func (h *groupingHub) arrive(at time.Time, alerts ...checksandalerts.Event) {
	h.t.Helper()
	for _, a := range alerts {
		if e := h.s.Accept(a.Node, a, at); e != nil {
			h.t.Fatal(e)
		}
	}
	rows, e := h.s.Intake()
	if e != nil {
		h.t.Fatal(e)
	}
	p := h.grouping.Plan(rows, nil, at)
	if e = h.s.Emit(p, h.destinations, false, at); e != nil {
		h.t.Fatal(e)
	}
	for _, key := range p.Sent {
		h.grouping.Sent[key] = at
	}
}

// report applies node reports to the hub's sets at at.
func (h *groupingHub) report(at time.Time, reports map[string]store.ReportedIncidents) {
	h.t.Helper()
	if e := h.s.ReconcileIncidentSets(reports, h.destinations, false, at); e != nil {
		h.t.Fatal(e)
	}
}

// shows is a report of node sent at sent that shows its incident of the
// flaw, open since opened.
func shows(node, severity, exactFinding string, sequence int64, opened, sent time.Time) store.ReportedIncidents {
	incident := checksandalerts.Event{Node: node, CheckGroup: "packages", Check: "flaws", Finding: flaw, Type: "failed", Incident: "incident-" + node, AlertSeverity: severity, AlertSeveritiesSentForIncident: []string{severity},
		At: opened, ExactFinding: exactFinding, ExactFindingSequence: sequence}
	return store.ReportedIncidents{Sent: sent, Incidents: map[string]checksandalerts.Event{incident.Incident: incident}, ReminderEligible: map[string]bool{incident.Incident: true}}
}

func (h *groupingHub) tick(at time.Time) {
	h.t.Helper()
	if e := h.s.SendDueUpdatesAndReminders(h.destinations, at); e != nil {
		h.t.Fatal(e)
	}
}

// take returns the alerts queued for the destination, in order, and
// delivers them.
func (h *groupingHub) take(destination string) []checksandalerts.Event {
	h.t.Helper()
	rows, e := h.s.DB.Query("SELECT seq, event FROM outbox WHERE destination=? ORDER BY seq", destination)
	if e != nil {
		h.t.Fatal(e)
	}
	defer rows.Close()
	out := []checksandalerts.Event{}
	for rows.Next() {
		var seq int64
		var b []byte
		var event checksandalerts.Event
		if e = rows.Scan(&seq, &b); e == nil {
			e = json.Unmarshal(b, &event)
		}
		if e != nil {
			h.t.Fatal(e)
		}
		out = append(out, event)
	}
	if _, e = h.s.DB.Exec("DELETE FROM outbox WHERE destination=?", destination); e != nil {
		h.t.Fatal(e)
	}
	return out
}

func kinds(alerts []checksandalerts.Event) []string {
	out := []string{}
	for _, a := range alerts {
		kind := a.Type + " " + a.AlertSeverity
		if a.MMHotlineAlert != nil {
			kind = a.Type + " " + a.MMHotlineAlert.Severity
		}
		out = append(out, kind)
	}
	return out
}

func expectKinds(t *testing.T, got []checksandalerts.Event, want ...string) {
	t.Helper()
	if !slices.Equal(kinds(got), want) {
		t.Fatalf("got %q, want %q", kinds(got), want)
	}
}

// The first node's failure goes out at once. Later nodes with the
// announced exact finding at the announced severity are covered growth:
// nothing goes out until the shared tick, which lists them in one update.
func TestCoveredGrowthWaitsForTheSharedTick(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn", "sos")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	h.arrive(tenOFive.Add(5*time.Minute), h.alert("db-1", "failed", "warn", versionA, 0, tenOFive.Add(5*time.Minute)))
	h.arrive(tenOFive.Add(90*time.Minute), h.alert("util-1", "failed", "warn", versionA, 0, tenOFive.Add(90*time.Minute)))
	expectKinds(t, h.take("chat"))
	twelve := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	h.tick(twelve.Add(-time.Second))
	expectKinds(t, h.take("chat"))
	h.tick(twelve)
	got := h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	update := got[0]
	if !slices.Equal(update.Nodes, []string{"db-1", "util-1"}) || update.NodeCount != 2 || !update.At.Equal(twelve) ||
		len(update.NodesByExactFinding) != 1 || update.NodesByExactFinding[0].ExactFinding != versionA || update.NodesByExactFinding[0].NodeCount != 2 {
		t.Fatalf("the update %+v", update)
	}
	if !strings.Contains(update.Body, "mmwatchdog fleet findings packages/flaws --finding '"+flaw+"'") {
		t.Fatal("the update does not say how to list every node:", update.Body)
	}
	h.tick(twelve.Add(everySixHours))
	expectKinds(t, h.take("chat"))
}

// During a rolling upgrade, a 0.1.x node's failure without an exact finding
// and a 0.2.0 node's failure of the same finding with one arrive together.
// The old alert keeps the one-minute grouping; the new one goes out on its
// own and announces its exact finding, whichever arrived first, so a later
// node with that exact finding is covered growth.
func TestAnOlderNodesAlertNeverSwallowsAnExactFinding(t *testing.T) {
	t.Parallel()
	for _, oldFirst := range []bool{true, false} {
		h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
		old, current := h.alert("old-1", "failed", "warn", "", 0, tenOFive), h.alert("app-1", "failed", "warn", versionA, 0, tenOFive)
		if oldFirst {
			h.arrive(tenOFive, old, current)
		} else {
			h.arrive(tenOFive, current, old)
		}
		got := h.take("chat")
		if len(got) != 2 || !slices.ContainsFunc(got, func(a checksandalerts.Event) bool { return a.Node == "app-1" && a.ExactFinding == versionA }) {
			t.Fatalf("old first %v: %q %+v", oldFirst, kinds(got), got)
		}
		h.arrive(tenOFive.Add(5*time.Minute), h.alert("db-1", "failed", "warn", versionA, 0, tenOFive.Add(5*time.Minute)))
		expectKinds(t, h.take("chat"))
	}
}

// A node's failure and its recovery that the hub takes in one alert
// grouping step, such as after a short network interruption, send the
// failure and then the closing alert: the failure sent in the same step
// no longer waits in the intake.
func TestAFailureAndItsRecoveryInOneStepCloseTheKey(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn"), "hotline": hotline()})
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-1", "failed", "warn", versionA, 0, tenOFive), h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("chat"), "failed warn", "recovered warn")
	expectKinds(t, h.take("hotline"), "failed warn", "recovered resolved-warn")
}

// A node whose incident closes without recovery, because its check was
// retired or the node was forgotten, is never named as ok again while
// other nodes keep the key open: recovered_nodes are confirmed recoveries.
// Nor is it named as added when it closes before the update that would
// have named it: its pending change closes with it.
func TestARetiredIncidentIsNotOkAgain(t *testing.T) {
	t.Parallel()
	for _, tickFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("an update before the retirement: %v", tickFirst), func(t *testing.T) {
			t.Parallel()
			h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
			for _, node := range []string{"app-1", "app-2", "app-3"} {
				h.arrive(tenOFive, h.alert(node, "failed", "warn", versionA, 0, tenOFive))
			}
			expectKinds(t, h.take("chat"), "failed warn")
			if tickFirst {
				h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
				expectKinds(t, h.take("chat"), "finding_nodes_changed warn")
			}
			at := tenOFive.Add(time.Hour)
			h.arrive(at, h.alert("app-2", "closed_without_recovery", "warn", versionA, 0, at))
			h.report(at, map[string]store.ReportedIncidents{"app-3": {Forgotten: true}})
			h.tick(time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC))
			for _, a := range h.take("chat") {
				named := slices.Concat(a.RecoveredNodes, a.Nodes)
				for _, g := range a.NodesByExactFinding {
					named = append(named, g.Nodes...)
				}
				if slices.Contains(named, "app-2") || slices.Contains(named, "app-3") {
					t.Fatalf("an update names a retired node: %+v", a)
				}
			}
		})
	}
}

// An exact finding that was not announced goes out at once, once per
// destination and key: nodes moving from A to B give one message for B,
// and the others are growth for the next update.
func TestANewExactFindingGoesOutOnceForAllNodesThatMoveToIt(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn", "sos")})
	const nodes = 300
	for i := range nodes {
		h.arrive(tenOFive, h.alert(fmt.Sprintf("app-%03d", i), "failed", "warn", versionA, 0, tenOFive))
	}
	expectKinds(t, h.take("chat"), "failed warn")
	later := tenOFive.Add(time.Hour)
	for i := range nodes {
		h.arrive(later, h.alert(fmt.Sprintf("app-%03d", i), checksandalerts.ExactFindingChanged, "warn", versionB, 1, later))
	}
	got := h.take("chat")
	expectKinds(t, got, "exact_finding_changed warn")
	if got[0].ExactFinding != versionB {
		t.Fatal("the message does not name the new exact finding", got[0].ExactFinding)
	}
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	got = h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	// Every node but app-000, whose own alerts announced A and B.
	if g := got[0].NodesByExactFinding; got[0].NodeCount != nodes-1 || len(g) != 1 || g[0].ExactFinding != versionB || g[0].NodeCount != nodes-1 || len(g[0].Nodes) != store.CombinedNames {
		t.Fatalf("the update %+v", got[0].NodesByExactFinding)
	}
}

// A severity above the announced one goes out at once. Growth that adds
// sos nodes goes within a minute at sos, and growth below sos waits for the
// shared tick at the highest severity it adds, so a warn node never wakes
// the person handling the SOS. A raise on a key that is already sos is
// growth.
func TestEachUpdateHasTheSeverityOfTheNodesItAdds(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", testhelpers.AllSeverities...)})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-2", "failed", "sos", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("chat"), "failed sos")
	at := tenOFive.Add(10 * time.Minute)
	h.arrive(at, h.alert("app-3", "failed", "sos", versionA, 0, at))
	h.arrive(at.Add(20*time.Second), h.alert("app-4", "failed", "warn", versionA, 0, at.Add(20*time.Second)))
	h.arrive(at.Add(30*time.Second), h.alert("app-1", checksandalerts.FailureIncidentEvents.Raised, "sos", versionA, 0, at.Add(30*time.Second)))
	expectKinds(t, h.take("chat"))
	h.tick(at.Add(time.Minute - time.Second))
	expectKinds(t, h.take("chat"))
	h.tick(at.Add(time.Minute))
	got := h.take("chat")
	expectKinds(t, got, "finding_nodes_changed sos")
	if !slices.Equal(got[0].Nodes, []string{"app-1", "app-3"}) {
		t.Fatal("the sos update names", got[0].Nodes)
	}
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	got = h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	if !slices.Equal(got[0].Nodes, []string{"app-4"}) {
		t.Fatal("the warn update names", got[0].Nodes)
	}
}

// A new exact finding goes out at the severity of the node that brought
// it: an info key's stays info.
func TestANewExactFindingIsNeverPromoted(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", testhelpers.AllSeverities...)})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive, h.alert("app-2", "failed", "info", versionB, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn", "failed info")
}

// resolved-sos goes out as soon as no node left in the destination's set
// is sos, and a later sos node wakes someone again: A recovers, B stays
// warn, and C reaches sos.
func TestALaterSosAfterResolvedSosWakesSomeone(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline()})
	h.arrive(tenOFive, h.alert("app-a", "failed", "sos", versionA, 0, tenOFive))
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-b", "failed", "warn", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("hotline"), "failed sos")
	h.arrive(tenOFive.Add(5*time.Minute), h.alert("app-a", "recovered", "sos", versionA, 0, tenOFive.Add(5*time.Minute)))
	expectKinds(t, h.take("hotline"), "recovered resolved-sos")
	h.arrive(tenOFive.Add(10*time.Minute), h.alert("app-c", "failed", "sos", versionA, 0, tenOFive.Add(10*time.Minute)))
	got := h.take("hotline")
	expectKinds(t, got, "failed sos")
	if got[0].Node != "app-c" {
		t.Fatal(got[0].Node)
	}
}

// When the destination's set for the key empties, the pending changes go
// out first and then the one closing alert, which says "on every node"
// when no node of the fleet has the key open.
func TestThePendingChangesGoOutBeforeTheClosingAlert(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn", "sos")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-2", "failed", "warn", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("chat"), "failed warn")
	h.arrive(tenOFive.Add(20*time.Minute), h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(20*time.Minute)))
	expectKinds(t, h.take("chat"))
	h.arrive(tenOFive.Add(30*time.Minute), h.alert("app-2", "recovered", "warn", versionA, 0, tenOFive.Add(30*time.Minute)))
	got := h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn", "recovered warn")
	if !slices.Equal(got[0].Nodes, []string{"app-2"}) || !slices.Equal(got[0].RecoveredNodes, []string{"app-1"}) {
		t.Fatal("the update", got[0].Nodes, got[0].RecoveredNodes)
	}
	if !strings.HasSuffix(got[1].Title, "is ok on every node again") {
		t.Fatal("the closing alert", got[1].Title)
	}
	// The key starts fresh.
	h.arrive(tenOFive.Add(time.Hour), h.alert("app-3", "failed", "warn", versionA, 0, tenOFive.Add(time.Hour)))
	expectKinds(t, h.take("chat"), "failed warn")
}

// Each destination's messages come from its own set, which holds only the
// incidents routed to it, and a closing alert claims recovery only for the
// nodes it covered while another node still has the key open. A node
// whose only update is that it is ok again gets an info update.
func TestEachDestinationClosesOnlyWhatItCovered(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat_a": chatTaking("https://a.example.com", "warn"), "chat_b": chatTaking("https://b.example.com", "warn")})
	to := func(alert checksandalerts.Event, destination string) checksandalerts.Event {
		alert.SendAlertsOnlyToDestinations = &[]string{destination}
		return alert
	}
	h.arrive(tenOFive, to(h.alert("app-1", "failed", "warn", versionA, 0, tenOFive), "chat_a"))
	h.arrive(tenOFive, to(h.alert("app-2", "failed", "warn", versionA, 0, tenOFive), "chat_a"))
	h.arrive(tenOFive, to(h.alert("db-1", "failed", "warn", versionA, 0, tenOFive), "chat_b"))
	expectKinds(t, h.take("chat_a"), "failed warn")
	expectKinds(t, h.take("chat_b"), "failed warn")
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	expectKinds(t, h.take("chat_a"), "finding_nodes_changed warn")
	h.arrive(tenOFive.Add(3*time.Hour), to(h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(3*time.Hour)), "chat_a"))
	h.tick(time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC))
	got := h.take("chat_a")
	expectKinds(t, got, "finding_nodes_changed info")
	if !slices.Equal(got[0].RecoveredNodes, []string{"app-1"}) || got[0].NodeCount != 0 {
		t.Fatal(got[0].RecoveredNodes, got[0].Nodes)
	}
	h.arrive(tenOFive.Add(9*time.Hour), to(h.alert("app-2", "recovered", "warn", versionA, 0, tenOFive.Add(9*time.Hour)), "chat_a"))
	got = h.take("chat_a")
	expectKinds(t, got, "recovered warn")
	if want := "packages/flaws: " + flaw + " is ok again on the 2 nodes that this alert covered"; got[0].Title != want {
		t.Fatalf("the closing alert %q, want %q", got[0].Title, want)
	}
	expectKinds(t, h.take("chat_b"))
}

// A report never announces: one that arrives before the first alert does
// not hold it back, and one that shows a covered node makes it growth.
func TestAReportNeverAnnounces(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.report(tenOFive, map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionA, 0, tenOFive.Add(-time.Second), tenOFive)})
	expectKinds(t, h.take("chat"))
	h.arrive(tenOFive.Add(time.Second), h.alert("app-1", "failed", "warn", versionA, 0, tenOFive.Add(-time.Second)))
	expectKinds(t, h.take("chat"), "failed warn")
	h.report(tenOFive.Add(time.Minute), map[string]store.ReportedIncidents{
		"app-1": shows("app-1", "warn", versionA, 0, tenOFive.Add(-time.Second), tenOFive.Add(time.Minute)),
		"app-2": shows("app-2", "warn", versionA, 0, tenOFive.Add(30*time.Second), tenOFive.Add(time.Minute))})
	expectKinds(t, h.take("chat"))
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	got := h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	if !slices.Equal(got[0].Nodes, []string{"app-2"}) {
		t.Fatal(got[0].Nodes)
	}
}

// An exact finding older than the one the hub holds for a node incident is
// ignored, whichever path it came by.
func TestAnOlderExactFindingIsIgnored(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	h.report(tenOFive.Add(time.Hour), map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionC, 2, tenOFive, tenOFive.Add(time.Hour))})
	h.arrive(tenOFive.Add(time.Hour+time.Second), h.alert("app-1", checksandalerts.ExactFindingChanged, "warn", versionB, 1, tenOFive.Add(50*time.Minute)))
	expectKinds(t, h.take("chat"))
	h.arrive(tenOFive.Add(time.Hour+2*time.Second), h.alert("app-1", checksandalerts.ExactFindingChanged, "warn", versionC, 2, tenOFive.Add(55*time.Minute)))
	got := h.take("chat")
	expectKinds(t, got, "exact_finding_changed warn")
	if got[0].ExactFinding != versionC {
		t.Fatal(got[0].ExactFinding)
	}
}

// The pending marks and what was announced come back from state.db after
// a hub restart.
func TestTheHoldSurvivesAHubRestart(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-2", "failed", "warn", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("chat"), "failed warn")
	h.s.Close()
	h.open()
	h.arrive(tenOFive.Add(time.Hour), h.alert("app-3", "failed", "warn", versionA, 0, tenOFive.Add(time.Hour)))
	expectKinds(t, h.take("chat"))
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	got := h.take("chat")
	expectKinds(t, got, "finding_nodes_changed warn")
	if !slices.Equal(got[0].Nodes, []string{"app-2", "app-3"}) {
		t.Fatal(got[0].Nodes)
	}
}

// An alert that announced a key and leaves the outbox undelivered, by
// retention, a permanent refusal, or a replaced destination, leaves the key
// unannounced: the next node's failure goes out as the first alert.
func TestAnAnnouncementThatWasNeverDeliveredIsForgotten(t *testing.T) {
	t.Parallel()
	for _, leave := range []struct {
		name string
		do   func(h *groupingHub, at time.Time)
	}{
		{"retention", func(h *groupingHub, at time.Time) {
			if e := h.s.Prune(at.Add(48 * time.Hour)); e != nil {
				h.t.Fatal(e)
			}
		}},
		{"permanent refusal", func(h *groupingHub, at time.Time) {
			pending, e := h.s.Pending()
			if e != nil || len(pending) != 1 {
				h.t.Fatal(pending, e)
			}
			if _, e = h.s.Delivered(pending[0], &checksandalerts.MMHotlineRefusal{Status: 422}); e != nil {
				h.t.Fatal(e)
			}
		}},
		{"replaced destination", func(h *groupingHub, at time.Time) {
			h.destinations = map[string]checksandalerts.Destination{"chat": chatTaking("https://other-chat.example.com", "warn")}
			if e := h.s.Save(incidents.FreshSnapshot(), nil, store.AlertRoute{Destinations: h.destinations, Hub: true}); e != nil {
				h.t.Fatal(e)
			}
		}},
	} {
		t.Run(leave.name, func(t *testing.T) {
			t.Parallel()
			h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
			h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
			leave.do(h, tenOFive)
			expectKinds(t, h.take("chat"))
			h.arrive(tenOFive.Add(time.Minute), h.alert("app-2", "failed", "warn", versionA, 0, tenOFive.Add(time.Minute)))
			expectKinds(t, h.take("chat"), "failed warn")
		})
	}
}

// An incident that report reconciliation adds after the destination's
// closing alert for its key, although it opened before, goes through the
// same rules: the key starts fresh, so its failure goes out as the first.
func TestAReopeningGoesThroughTheSameRules(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chatTaking("https://chat.example.com", "warn")})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive.Add(time.Hour), h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(time.Hour)))
	expectKinds(t, h.take("chat"), "failed warn", "recovered warn")
	h.report(tenOFive.Add(time.Hour+time.Minute), map[string]store.ReportedIncidents{"app-2": shows("app-2", "warn", versionA, 0, tenOFive.Add(30*time.Minute), tenOFive.Add(time.Hour+time.Minute))})
	got := h.take("chat")
	expectKinds(t, got, "failed warn")
	if got[0].Node != "app-2" || got[0].ExactFinding != versionA {
		t.Fatal(got[0].Node, got[0].ExactFinding)
	}
}
