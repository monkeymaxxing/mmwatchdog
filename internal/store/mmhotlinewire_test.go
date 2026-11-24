package store_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// The tests of what MMWatchdog sends MMHotline, from the design of
// grouping by exact finding, "MMHotline wire format": each message as
// MMHotline's intake decodes it, which refuses unknown fields and applies
// MMHotline's limits.

const nodeAlertSource = "mmwatchdog_nodes_without_the_hub"

// hotlineWithNodeAlertSource is hotline() with an alert source for nodes
// sending without the hub.
func hotlineWithNodeAlertSource() checksandalerts.Destination {
	d := hotline()
	p := *d.MMHotline()
	p.AlertSourceForNodesSendingWithoutTheHub = &checksandalerts.MMHotlineAlertSource{AlertSourceID: nodeAlertSource, SigningSecretFile: "/etc/mmwatchdog/hotline-nodes"}
	d.Inputs = &p
	return d
}

// wire is what MMHotline receives for a queued delivery.
func wire(t *testing.T, queued checksandalerts.Event) testhelpers.MMHotlineAlert {
	t.Helper()
	if queued.MMHotlineAlert == nil {
		t.Fatalf("not an MMHotline delivery: %+v", queued)
	}
	a, e := testhelpers.DecodeMMHotlineAlert(queued.MMHotlineAlert.Encode())
	if e != nil {
		t.Fatal(e)
	}
	return a
}

func firstLine(a testhelpers.MMHotlineAlert) string {
	line, _, _ := strings.Cut(a["message"], "\n")
	return line
}

// sourceOf names the alert source that a queued delivery goes under.
func sourceOf(queued checksandalerts.Event) string {
	if queued.MMHotlineAlertSource == "" {
		return "source-1"
	}
	return queued.MMHotlineAlertSource
}

// Each kind of message of a key with exact findings maps to MMHotline's
// severity and the key: the first alert at its own severity; something new
// (a higher severity, an exact finding not announced, a changed exact
// finding) at once at the severity of the node that brought it, never
// promoted; growth on the shared tick at the highest severity it adds, or
// each minute when it adds sos nodes; an update of recovered nodes alone
// as info with the key. Each message has its own sender_alert_id.
func TestTheMMHotlineWireOfAKeyWithExactFindings(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline()})
	const versionD = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u7; fixed version: none"
	first := h.alert("app-1", "failed", "warn", versionA, 0, tenOFive)
	key := first.FleetIncidentKey
	sent := []testhelpers.MMHotlineAlert{}
	expect := func(at time.Time, severity, line string) {
		t.Helper()
		got := h.take("hotline")
		if len(got) != 1 {
			t.Fatalf("at %s: %q", at.Format("15:04"), kinds(got))
		}
		a := wire(t, got[0])
		if a["severity"] != severity || a["sender_incident_key"] != key || line != "" && firstLine(a) != line || strings.Contains(firstLine(a), "app-") {
			t.Fatalf("at %s: %+v, want %s with the first line %q", at.Format("15:04"), a, severity, line)
		}
		sent = append(sent, a)
	}
	h.arrive(tenOFive, first)
	expect(tenOFive, "warn", versionA)
	if !strings.HasPrefix(sent[0]["title"], "app-1: ") {
		t.Fatal("the node's name is not in the title", sent[0]["title"])
	}
	at := tenOFive.Add(5 * time.Minute)
	h.arrive(at, h.alert("app-2", "failed", "warn", versionA, 0, at))
	expectKinds(t, h.take("hotline"))
	at = tenOFive.Add(10 * time.Minute)
	h.arrive(at, h.alert("app-3", "failed", "sos", versionA, 0, at))
	expect(at, "sos", versionA)
	at = tenOFive.Add(15 * time.Minute)
	h.arrive(at, h.alert("app-4", "failed", "warn", versionB, 0, at))
	expect(at, "warn", versionB)
	at = tenOFive.Add(20 * time.Minute)
	h.arrive(at, h.alert("app-1", checksandalerts.ExactFindingChanged, "warn", versionD, 1, at))
	expect(at, "warn", versionD)
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	expect(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), "warn", "")
	if !strings.Contains(sent[len(sent)-1]["message"], "app-2") {
		t.Fatal("the update does not name the node it adds", sent[len(sent)-1]["message"])
	}
	at = time.Date(2026, 11, 24, 17, 5, 0, 0, time.UTC)
	h.arrive(at, h.alert("app-5", "failed", "sos", versionA, 0, at))
	expectKinds(t, h.take("hotline"))
	h.tick(at.Add(time.Minute))
	expect(at.Add(time.Minute), "sos", "")
	at = time.Date(2026, 11, 24, 17, 10, 0, 0, time.UTC)
	h.arrive(at, h.alert("app-2", "recovered", "warn", versionA, 0, at))
	expectKinds(t, h.take("hotline"))
	h.tick(time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC))
	expect(time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC), "info", "")
	ids := []string{}
	for _, a := range sent {
		ids = append(ids, a["sender_alert_id"])
	}
	slices.Sort(ids)
	if len(slices.Compact(ids)) != len(sent) {
		t.Fatal("two messages share a sender_alert_id", ids)
	}
}

// An update stays within MMHotline's limits however many exact findings it
// covers: its message within 12 KiB, inside MMHotline's 16 KiB, and its
// title within 256 characters. What does not fit is counted, with the
// command that lists every node.
func TestAnUpdateStaysWithinMMHotlinesLimits(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline()})
	exactFinding := func(i int) string {
		return fmt.Sprintf("%03d %s", i, strings.Repeat(`"quoted\" `, 30))[:255]
	}
	const exactFindings = 120
	for i := range exactFindings {
		h.arrive(tenOFive, h.alert(fmt.Sprintf("first-%03d", i), "failed", "warn", exactFinding(i), 0, tenOFive))
	}
	if got := h.take("hotline"); len(got) != exactFindings {
		t.Fatal("each exact finding goes out once", len(got))
	}
	for i := range exactFindings {
		h.arrive(tenOFive.Add(time.Minute), h.alert(fmt.Sprintf("later-%03d", i), "failed", "warn", exactFinding(i), 0, tenOFive.Add(time.Minute)))
	}
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	got := h.take("hotline")
	expectKinds(t, got, "finding_nodes_changed warn")
	a := wire(t, got[0])
	if len(a["message"]) > store.UpdateMessageLimit || len([]rune(a["title"])) > 256 {
		t.Fatalf("the update has %d bytes and the title %d characters", len(a["message"]), len([]rune(a["title"])))
	}
	if !strings.Contains(a["message"], " more exact findings on ") || !strings.Contains(a["message"], "mmwatchdog fleet findings packages/flaws --finding '"+flaw+"'") {
		t.Fatal("the update does not count what does not fit", a["message"])
	}
}

// The bound holds with long node names and nodes that are ok again in the
// same update: the lines after the exact findings, the nodes that are ok
// again and the command that lists every node, always fit.
func TestAnUpdateWithLongNodeNamesStaysWithinItsBound(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline()})
	exactFinding := func(i int) string { return fmt.Sprintf("%03d %s", i, strings.Repeat("x", 260))[:255] }
	node := func(kind string, i int) string { return fmt.Sprintf("%s-%03d.%s", kind, i, strings.Repeat("a", 240)) }
	const exactFindings = 40
	for i := range exactFindings {
		h.arrive(tenOFive, h.alert(node("first", i), "failed", "warn", exactFinding(i), 0, tenOFive))
		h.arrive(tenOFive, h.alert(node("second", i), "failed", "warn", exactFinding(i), 0, tenOFive))
	}
	h.take("hotline")
	h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	h.take("hotline")
	later := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	for i := range exactFindings {
		h.arrive(later, h.alert(node("later", i), "failed", "warn", exactFinding(i), 0, later))
		h.arrive(later, h.alert(node("first", i), "recovered", "warn", exactFinding(i), 0, later))
	}
	h.tick(time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC))
	got := h.take("hotline")
	expectKinds(t, got, "finding_nodes_changed warn")
	a := wire(t, got[0])
	if len(a["message"]) > store.UpdateMessageLimit {
		t.Fatalf("the update has %d bytes", len(a["message"]))
	}
	if !strings.Contains(a["message"], "ok again on 40 nodes: "+node("first", 0)) || !strings.Contains(a["message"], "mmwatchdog fleet findings packages/flaws") {
		t.Fatal("the update lost its last lines", a["message"][len(a["message"])-400:])
	}
}

// failureOf is node's failure of the check myapp/dns, which has no exact
// finding, with message.
func failureOf(h *groupingHub, node, message string, at time.Time) checksandalerts.Event {
	e := h.alert(node, "failed", "warn", "", 0, at)
	e.CheckGroup, e.Check, e.Finding, e.Message = "myapp", "dns", "", message
	e.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(e, "failed", "")), node, groupingFleet)
	e.WriteAlertTexts([]checksandalerts.Event{e}, "")
	return e
}

// newJoinedNode is a joined node's store, as its fallback queues into it.
func newJoinedNode(t *testing.T) *groupingHub {
	t.Helper()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return &groupingHub{t: t, s: s}
}

// The first line of every warn and sos to MMHotline names the problem in
// the same words on every node, from the hub and from a node sending
// without the hub: the exact finding when the finding has one, otherwise
// the node's own failure message with its node name taken out. The node's
// name is in the title and in later lines. A direct send keeps the fleet's
// key.
func TestTheFirstLineNamesTheProblemTheSameWayOnEveryNode(t *testing.T) {
	t.Parallel()
	destinations := map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()}
	const problem = "cannot resolve db.internal"
	direct := []testhelpers.MMHotlineAlert{}
	for _, node := range []string{"app-1", "app-12"} {
		n := newJoinedNode(t)
		fallBack(t, n.s, failureOf(n, node, node+" "+problem, tenOFive), destinations)
		got := n.take("hotline")
		if len(got) != 1 {
			t.Fatal(kinds(got))
		}
		direct = append(direct, wire(t, got[0]))
	}
	for i, node := range []string{"app-1", "app-12"} {
		if firstLine(direct[i]) != problem || !strings.HasPrefix(direct[i]["title"], node+": ") {
			t.Fatalf("%s sent %+v", node, direct[i])
		}
	}
	h := newGroupingHub(t, destinations)
	h.arrive(tenOFive, failureOf(h, "app-1", "app-1 "+problem, tenOFive))
	h.arrive(tenOFive.Add(10*time.Second), failureOf(h, "app-12", "app-12 "+problem, tenOFive.Add(10*time.Second)), failureOf(h, "app-13", "app-13 "+problem, tenOFive.Add(10*time.Second)))
	h.arrive(tenOFive.Add(2 * time.Minute))
	got := h.take("hotline")
	if len(got) != 2 {
		t.Fatal(kinds(got))
	}
	one, grouped := wire(t, got[0]), wire(t, got[1])
	if firstLine(one) != problem || firstLine(grouped) != problem || !strings.Contains(grouped["message"], "app-12, app-13") || grouped["title"] != "myapp/dns failed on 2 nodes" {
		t.Fatalf("the hub sent %+v and %+v", one, grouped)
	}
	// Direct sends use the fleet's key, as the hub does.
	if direct[0]["sender_incident_key"] != one["sender_incident_key"] || direct[1]["sender_incident_key"] != one["sender_incident_key"] {
		t.Fatal("a direct send has another key than the hub's", direct[0]["sender_incident_key"], direct[1]["sender_incident_key"], one["sender_incident_key"])
	}
	// A node's name inside a longer name stays.
	n := newJoinedNode(t)
	fallBack(t, n.s, failureOf(n, "app-1", "app-1: no answer from app-10.internal", tenOFive), destinations)
	if a := wire(t, n.take("hotline")[0]); firstLine(a) != "no answer from app-10.internal" {
		t.Fatalf("%q", firstLine(a))
	}
	// A finding with an exact finding names it on the first line.
	n = newJoinedNode(t)
	fallBack(t, n.s, n.alert("app-1", "failed", "sos", versionA, 0, tenOFive), destinations)
	if a := wire(t, n.take("hotline")[0]); firstLine(a) != versionA || !strings.HasPrefix(a["title"], "app-1: ") || a["severity"] != "sos" {
		t.Fatalf("%+v", a)
	}
}

// A joined node delivering without the hub sends under the destination's
// alert source for nodes sending without the hub, and its record of the
// direct send, which its reports carry, names that source. Without the
// block it sends under the destination's own source, as before.
func TestAJoinedNodeSendsWithoutTheHubUnderTheNodeAlertSource(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		destination checksandalerts.Destination
		source      string
	}{{hotlineWithNodeAlertSource(), nodeAlertSource}, {hotline(), ""}} {
		n := newJoinedNode(t)
		fallBack(t, n.s, failureOf(n, "app-2", "app-2 "+"cannot resolve db.internal", tenOFive), map[string]checksandalerts.Destination{"hotline": c.destination})
		got := accept(t, n.s)
		records, e := n.s.SentWithoutTheHub()
		if e != nil || len(got) != 1 || got[0].MMHotlineAlertSource != c.source || len(records) != 1 || records[0].AlertSource != c.source {
			t.Fatalf("want the source %q: %+v %+v %v", c.source, got, records, e)
		}
	}
}

// The hub sends a grouped key's opening under the key that it derives from
// its own identity and the alert's check and finding, the key of the set
// and of the resolution, whatever key the node's alert carried.
func TestAGroupedOpeningGoesUnderTheHubsOwnKey(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline(), "chat": chatTaking("https://chat.example.com", "warn")})
	failure := h.alert("app-1", "failed", "warn", versionA, 0, tenOFive)
	failure.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey("another key"), "app-1", groupingFleet)
	h.arrive(tenOFive, failure)
	opening := h.take("hotline")
	chat := h.take("chat")
	h.arrive(tenOFive.Add(time.Hour), h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(time.Hour)))
	resolution := h.take("hotline")
	expectKinds(t, opening, "failed warn")
	expectKinds(t, resolution, "recovered resolved-warn")
	if a, b := wire(t, opening[0]), wire(t, resolution[0]); a["sender_incident_key"] != b["sender_incident_key"] {
		t.Fatal("the opening and the resolution have other keys", a["sender_incident_key"], b["sender_incident_key"])
	}
	if chat[0].FleetIncidentKey != resolution[0].FleetIncidentKey {
		t.Fatal("the chat's opening has another key", chat[0].FleetIncidentKey)
	}
}

// A destination whose service keeps incidents gets the opening and the
// closing alert of a key under the key that the hub derives, whatever key
// the node's alerts carried, for a grouped key and for any other.
func TestAServiceThatKeepsIncidentsGetsTheHubsOwnKey(t *testing.T) {
	t.Parallel()
	pager := checksandalerts.Destination{Inputs: &checksandalerts.ModuleCall{Name: "own_pager", Module: &checksandalerts.InstalledModule{Name: "own_pager", Description: &checksandalerts.ModuleDescription{ServiceKeepsIncidents: true}}}, RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities}
	bogus := checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey("another key"), "app-1", groupingFleet)
	for _, grouped := range []bool{true, false} {
		h := newGroupingHub(t, map[string]checksandalerts.Destination{"pager": pager})
		failure := h.alert("app-1", "failed", "warn", versionA, 0, tenOFive)
		if !grouped {
			failure = failureOf(h, "app-1", "app-1 cannot resolve db.internal", tenOFive)
		}
		failure.FleetIncidentKey = bogus
		h.arrive(failure.At, failure)
		h.arrive(failure.At.Add(2 * time.Minute))
		opening := h.take("pager")
		recovery := failure
		recovery.ID, recovery.Type, recovery.At, recovery.AlertSeveritiesSentForIncident = failure.ID+"-recovered", "recovered", tenOFive.Add(time.Hour), []string{"warn"}
		h.arrive(recovery.At, recovery)
		h.arrive(recovery.At.Add(2 * time.Minute))
		closing := h.take("pager")
		want := checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(failure, "failed", "")), h.s.Node, groupingFleet)
		if len(opening) != 1 || len(closing) != 1 || opening[0].FleetIncidentKey != want || closing[0].FleetIncidentKey != want {
			t.Fatalf("%s: the opening %+v and the closing %+v", failure.Check, opening, closing)
		}
	}
}

// An alert queued under the node alert source, when the destination no
// longer has that source, is cancelled with the destination's other
// changes, never sent under the destination's own source, where its
// resolution could close another incident. Alerts under the destination's
// own source stay.
func TestAnAlertQueuedUnderARemovedNodeAlertSourceIsCancelled(t *testing.T) {
	t.Parallel()
	n := newJoinedNode(t)
	fallBack(t, n.s, failureOf(n, "app-2", "app-2 cannot resolve db.internal", tenOFive), map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()})
	fallBack(t, n.s, failureOf(n, "app-3", "app-3 cannot resolve db.internal", tenOFive), map[string]checksandalerts.Destination{"hotline": hotline()})
	for _, c := range []struct {
		name        string
		destination checksandalerts.Destination
		cancelled   int64
	}{{"kept", hotlineWithNodeAlertSource(), 0}, {"removed", hotline(), 1}} {
		other := hotlineWithNodeAlertSource()
		p := *other.MMHotline()
		p.AlertSourceForNodesSendingWithoutTheHub = &checksandalerts.MMHotlineAlertSource{AlertSourceID: "another_source", SigningSecretFile: "/etc/mmwatchdog/another"}
		other.Inputs = &p
		if c.name == "removed" {
			// A replaced node source cancels as a removed one does.
			c.destination = other
		}
		tx, e := n.s.DB.Begin()
		if e != nil {
			t.Fatal(e)
		}
		cancelled, e := store.CancelDeliveries(tx, map[string]checksandalerts.Destination{"hotline": c.destination})
		if e != nil || cancelled != c.cancelled {
			t.Fatal(c.name, cancelled, e)
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	got := n.take("hotline")
	if len(got) != 1 || got[0].MMHotlineAlertSource != "" || got[0].Node != "app-3" {
		t.Fatalf("%+v", got)
	}
	// Nor does a delivery under a source the destination lacks fall back to
	// the destination's own.
	if _, _, ok := hotline().MMHotline().AlertSourceNamed(nodeAlertSource); ok {
		t.Fatal("an unknown alert source is sent under the destination's own")
	}
}

// reportOf is node's report at sent that shows its open incident of
// myapp/dns at severity, and, with source, says that the node sent it
// without the hub under that alert source.
func reportOf(node, severity, source string, opened, sent time.Time) store.ReportedIncidents {
	incident := checksandalerts.Event{Node: node, CheckGroup: "myapp", Check: "dns", Type: "failed", Incident: "incident-" + node, AlertSeverity: severity, AlertSeveritiesSentForIncident: []string{severity}, At: opened, Message: "cannot resolve db.internal"}
	r := store.ReportedIncidents{Sent: sent, Incidents: map[string]checksandalerts.Event{incident.Incident: incident}}
	if source != "-" {
		carried := incident
		carried.MMHotlineAlertSource = source
		r.SentWithoutTheHub = map[string]map[string]checksandalerts.Event{"hotline": {incident.Incident: carried}}
	}
	return r
}

// While an incident that a node opened under the alert source for nodes
// sending without the hub is open in MMHotline, the hub sends every
// further alert of the key, and its resolution, under that source: an sos
// from the hub joins that incident instead of waking someone again. Once
// the key is resolved, its alerts go under the hub's own source again.
func TestTheHubSendsUnderTheNodeAlertSourceWhileANodeOpenedTheIncidentThere(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()})
	h.report(tenOFive.Add(time.Minute), map[string]store.ReportedIncidents{"app-2": reportOf("app-2", "sos", nodeAlertSource, tenOFive, tenOFive.Add(time.Minute))})
	expectKinds(t, h.take("hotline"))
	failure := failureOf(h, "app-3", "app-3 cannot resolve db.internal", tenOFive.Add(2*time.Minute))
	failure.AlertSeverity = "sos"
	h.arrive(tenOFive.Add(2*time.Minute), failure)
	got := h.take("hotline")
	expectKinds(t, got, "failed sos")
	if sourceOf(got[0]) != nodeAlertSource {
		t.Fatal("the hub's sos goes under", sourceOf(got[0]))
	}
	h.arrive(tenOFive.Add(3*time.Minute), recoveryOf(h, "app-3", tenOFive.Add(3*time.Minute)))
	expectKinds(t, h.take("hotline"))
	h.report(tenOFive.Add(4*time.Minute), map[string]store.ReportedIncidents{"app-2": {Sent: tenOFive.Add(4 * time.Minute)}})
	got = h.take("hotline")
	expectKinds(t, got, "recovered resolved-sos")
	if sourceOf(got[0]) != nodeAlertSource {
		t.Fatal("the resolution goes under", sourceOf(got[0]))
	}
	failure = failureOf(h, "app-4", "app-4 cannot resolve db.internal", tenOFive.Add(time.Hour))
	h.arrive(tenOFive.Add(time.Hour), failure)
	if got = h.take("hotline"); len(got) != 1 || sourceOf(got[0]) != "source-1" {
		t.Fatal("after the resolution the key's alert goes under", kinds(got), sourceOf(got[0]))
	}
}

// recoveryOf is node's recovery of its incident of myapp/dns.
func recoveryOf(h *groupingHub, node string, at time.Time) checksandalerts.Event {
	e := failureOf(h, node, "resolves db.internal", at)
	e.Type = "recovered"
	e.AlertSeveritiesSentForIncident = []string{"sos"}
	e.WriteAlertTexts([]checksandalerts.Event{e}, "")
	return e
}

// When both alert sources got alerts of a key, the hub's own before the
// hub went away and the node source from a node's direct send, the key's
// resolution goes under each, with its own sender_alert_id, so neither
// MMHotline incident stays open. A grouped key's update after the direct
// send goes under the node source.
func TestTheResolutionGoesUnderEachAlertSourceThatTheKeyReached(t *testing.T) {
	t.Parallel()
	t.Run("a key without an exact finding", func(t *testing.T) {
		t.Parallel()
		h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()})
		failure := failureOf(h, "app-1", "app-1 cannot resolve db.internal", tenOFive)
		failure.AlertSeverity = "sos"
		h.arrive(tenOFive, failure)
		got := h.take("hotline")
		expectKinds(t, got, "failed sos")
		if sourceOf(got[0]) != "source-1" {
			t.Fatal(sourceOf(got[0]))
		}
		h.report(tenOFive.Add(time.Minute), map[string]store.ReportedIncidents{"app-2": reportOf("app-2", "sos", nodeAlertSource, tenOFive, tenOFive.Add(time.Minute))})
		recovery := recoveryOf(h, "app-1", tenOFive.Add(2*time.Minute))
		h.arrive(tenOFive.Add(2*time.Minute), recovery)
		h.report(tenOFive.Add(3*time.Minute), map[string]store.ReportedIncidents{"app-2": {Sent: tenOFive.Add(3 * time.Minute)}})
		got = h.take("hotline")
		expectKinds(t, got, "recovered resolved-sos", "recovered resolved-sos")
		a, b := wire(t, got[0]), wire(t, got[1])
		if !slices.Equal([]string{sourceOf(got[0]), sourceOf(got[1])}, []string{"source-1", nodeAlertSource}) || a["sender_alert_id"] == b["sender_alert_id"] || a["sender_incident_key"] != b["sender_incident_key"] {
			t.Fatalf("%s %+v, %s %+v", sourceOf(got[0]), a, sourceOf(got[1]), b)
		}
	})
	t.Run("a key with an exact finding", func(t *testing.T) {
		t.Parallel()
		h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()})
		h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
		expectKinds(t, h.take("hotline"), "failed warn")
		report := shows("app-2", "warn", versionA, 0, tenOFive.Add(time.Minute), tenOFive.Add(10*time.Minute))
		carried := report.Incidents["incident-app-2"]
		carried.MMHotlineAlertSource = nodeAlertSource
		report.SentWithoutTheHub = map[string]map[string]checksandalerts.Event{"hotline": {"incident-app-2": carried}}
		h.report(tenOFive.Add(10*time.Minute), map[string]store.ReportedIncidents{"app-2": report})
		h.tick(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
		got := h.take("hotline")
		expectKinds(t, got, "finding_nodes_changed warn")
		if sourceOf(got[0]) != nodeAlertSource {
			t.Fatal("the update goes under", sourceOf(got[0]))
		}
		h.arrive(tenOFive.Add(7*time.Hour), h.alert("app-1", "recovered", "warn", versionA, 0, tenOFive.Add(7*time.Hour)))
		h.report(tenOFive.Add(8*time.Hour), map[string]store.ReportedIncidents{"app-2": {Sent: tenOFive.Add(8 * time.Hour)}})
		got = h.take("hotline")
		expectKinds(t, got, "recovered resolved-warn", "recovered resolved-warn")
		if !slices.Equal([]string{sourceOf(got[0]), sourceOf(got[1])}, []string{"source-1", nodeAlertSource}) {
			t.Fatal(sourceOf(got[0]), sourceOf(got[1]))
		}
	})
}

// Removing the alert source for nodes sending without the hub while
// MMHotline keeps a key's incident open under it: mmwatchdog validate
// names the key, and the resolution that cannot go out there is a visible
// delivery gap that says to close the incident in MMHotline.
func TestARemovedNodeAlertSourceWithAnOpenIncidentIsVisible(t *testing.T) {
	t.Parallel()
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()})
	at := tenOFive.Add(10 * time.Minute)
	h.report(at, map[string]store.ReportedIncidents{"app-1": carriedTo(shows("app-1", "sos", versionA, 0, tenOFive, at), "hotline", nodeAlertSource)})
	h.take("hotline")
	h.destinations = map[string]checksandalerts.Destination{"hotline": hotline()}
	open := store.MMHotlineKeysUnderRemovedAlertSources(h.dir, h.destinations)
	if keys := open["hotline"][nodeAlertSource]; !slices.Equal(keys, []string{readableFlaw}) {
		t.Fatalf("the keys open under the removed source %+v", open)
	}
	h.report(at.Add(time.Hour), map[string]store.ReportedIncidents{"app-1": {Sent: at.Add(time.Hour)}})
	h.take("hotline")
	stats, e := h.s.DeliveryStats()
	if e != nil || stats.Gaps != 1 {
		t.Fatal("no delivery gap for the resolution that cannot go out", stats, e)
	}
	events, e := h.s.Events(10)
	if e != nil || len(events) == 0 || events[0].Type != "delivery_gap" || !strings.Contains(events[0].Message, nodeAlertSource) || !strings.Contains(events[0].Message, "close its incident in MMHotline") {
		t.Fatal(events, e)
	}
	if open := store.MMHotlineKeysUnderRemovedAlertSources(h.dir, h.destinations); len(open) != 0 {
		t.Fatal("a resolved key is still named", open)
	}
}
