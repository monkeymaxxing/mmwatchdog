package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/testhelpers"
)

// hubWatchAfter is the default alert_after_hub_unreachable_for.
const hubWatchAfter = 3 * time.Minute

// tick runs one scheduler tick at now and waits for the executions it started.
// The tick also commits the transitions of the receipts before it.
func tick(d *Daemon, now time.Time) {
	d.schedule(context.Background(), now)
	d.workers.Wait()
}

// fleetCheck copies a fleet check's state, or returns nil.
func fleetCheck(d *Daemon, name string) *incidents.CheckState {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.snapshot.Checks[fleet.FleetKey(name)]
	if st == nil {
		return nil
	}
	c := *st
	return &c
}

// silenceAlerts lists the hub's alerts about its nodes, as queued for the
// alert grouping step, as "type check: message".
func silenceAlerts(t *testing.T, hub *Daemon) []string {
	t.Helper()
	rows, e := hub.store.DB.Query("SELECT event FROM intake WHERE node=? AND json_extract(event,'$.check_group')='node_reports' AND json_extract(event,'$.check')!='' ORDER BY seq", hub.store.Node)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var b []byte
		var event checksandalerts.Event
		if e = rows.Scan(&b); e == nil {
			e = json.Unmarshal(b, &event)
		}
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, event.Type+" "+event.Check+": "+event.Message)
	}
	return out
}

// Each joined node is a heartbeat created by joining: three missed report
// intervals open its incident, and its next current report resolves it, even
// one the hub cannot save. An old report never counts.
func TestSilentServersAlertAndRecover(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	tick(hub, time.Now())
	st := fleetCheck(hub, "app-1")
	if st == nil || st.Last.Status != "ok" || st.Receipt.IsZero() || !st.Deadline.Equal(st.Receipt.Add(3*reportInterval)) {
		t.Fatalf("fleet check after the first report %+v", st)
	}
	received := st.Receipt
	var b []byte
	var persisted incidents.CheckState
	if e := hub.store.DB.QueryRow("SELECT data FROM checks WHERE key=?", fleet.FleetKey("app-1")).Scan(&b); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(b, &persisted); e != nil {
		t.Fatal(e)
	}
	if persisted.CheckGroup != checksandalerts.NodeReportsCheckGroup || !persisted.Receipt.IsZero() || !persisted.Deadline.IsZero() {
		t.Fatal("receipts are kept in memory only", string(b))
	}
	tick(hub, received.Add(3*reportInterval-time.Second))
	if alerts := silenceAlerts(t, hub); len(alerts) != 0 {
		t.Fatal("alerted before three missed intervals", alerts)
	}
	tick(hub, received.Add(3*reportInterval))
	want := "failed app-1: app-1 has sent no report to the hub since " + received.UTC().Format("15:04 UTC")
	if alerts := silenceAlerts(t, hub); !reflect.DeepEqual(alerts, []string{want}) {
		t.Fatal(alerts)
	}
	hub.mu.Lock()
	checks, _, _ := hub.status("", received.Add(3*reportInterval), nil)
	hub.mu.Unlock()
	if s := incidents.Summarize(checks, ""); s.ExitCode < 1 || checks[fleet.FleetKey("app-1")].State != "failed" {
		t.Fatal("hub status", s)
	}
	m := hub.hub.Nodes["app-1"]
	old, _ := json.Marshal(fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: m.StartID, Seq: 1, History: m.History, After: -1, MeasurementHistoryID: incidents.ID(), ReadingsAfter: -1, Summary: incidents.StatusSummary{Attention: []incidents.Attention{}}})
	r := httptest.NewRequest("POST", "https://hub.test/v1/report", bytes.NewReader(old))
	r.Header.Set("Authorization", "Bearer "+fleet.ReadCredential(filepath.Join(node.config.StateDir, fleet.CredentialFile)).Credential)
	w := httptest.NewRecorder()
	hub.hubRoutes().ServeHTTP(w, r)
	if st := fleetCheck(hub, "app-1"); w.Code != http.StatusConflict || st.FailureIncident.Incident == "" || !st.Receipt.Equal(received) {
		t.Fatal("an old report counted as a receipt", w.Code)
	}
	if _, e := hub.hub.DB.Exec("CREATE TRIGGER reject_server BEFORE UPDATE ON nodes BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); e != nil {
		t.Fatal(e)
	}
	node.report(context.Background())
	tick(hub, time.Now())
	alerts := silenceAlerts(t, hub)
	if !strings.Contains(node.link.LastError, "503") || len(alerts) != 2 || alerts[1] != "recovered app-1: app-1 is reporting again" {
		t.Fatal("an unsaved report did not keep the node reporting", node.link.LastError, alerts)
	}
}

// A reported whole-node window holds the node's silence alert until it
// ends; a node still silent then alerts. mmwatchdog maintenance fleet holds every
// node's.
func TestMaintenanceHoldsSilenceAlerts(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	setMaintenance(t, node, checksandalerts.EveryCheckGroup, "10m")
	node.report(context.Background())
	received := fleetCheck(hub, "app-1").Receipt
	tick(hub, received.Add(3*reportInterval))
	if st := fleetCheck(hub, "app-1"); st.FailureIncident.Incident == "" || st.FailureIncident.Announced != "" || len(silenceAlerts(t, hub)) != 0 {
		t.Fatal("the reported window did not hold the alert", st.FailureIncident)
	}
	tick(hub, received.Add(9*time.Minute))
	if len(silenceAlerts(t, hub)) != 0 {
		t.Fatal("alerted while the window lasts")
	}
	tick(hub, received.Add(11*time.Minute))
	if alerts := silenceAlerts(t, hub); len(alerts) != 1 || !strings.HasPrefix(alerts[0], "failed app-1: app-1 has sent no report to the hub since ") {
		t.Fatal("a node still silent after its window did not alert", alerts)
	}

	other := joinedNode(t, code, "app-2")
	other.report(context.Background())
	setMaintenance(t, hub, checksandalerts.NodeReportsCheckGroup, "1h")
	tick(hub, fleetCheck(hub, "app-2").Receipt.Add(3*reportInterval))
	if st := fleetCheck(hub, "app-2"); st.FailureIncident.Incident == "" || len(silenceAlerts(t, hub)) != 1 {
		t.Fatal("fleet maintenance did not hold the alert", st.FailureIncident)
	}
	other.report(context.Background())
	setMaintenance(t, hub, checksandalerts.NodeReportsCheckGroup, "off")
	tick(hub, time.Now().UTC())
	if st := fleetCheck(hub, "app-2"); st.FailureIncident.Incident != "" || len(silenceAlerts(t, hub)) != 1 {
		t.Fatal("a node that returned during fleet maintenance alerted", st.FailureIncident, silenceAlerts(t, hub))
	}
}

// A hub restart grants every node a fresh window and never manufactures
// alerts or recoveries; an open incident stays open.
func TestHubRestartGrantsFreshWindow(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	silent := joinedNode(t, code, "app-1")
	silent.report(context.Background())
	tick(hub, fleetCheck(hub, "app-1").Receipt.Add(3*reportInterval))
	reporting := joinedNode(t, code, "app-2")
	reporting.report(context.Background())
	received := fleetCheck(hub, "app-2").Receipt
	before := silenceAlerts(t, hub)
	hub.Close()
	restarted, e := NewDaemon(hub.path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { restarted.Close() })
	start := fleetCheck(restarted, "app-2").Deadline.Add(-3 * reportInterval)
	a, b := fleetCheck(restarted, "app-1"), fleetCheck(restarted, "app-2")
	// The restarted hub has the time of the last report on the wall clock:
	// its monotonic reading is of the restarted process (OnThisClock).
	if a.FailureIncident.Incident == "" || b.FailureIncident.Incident != "" || !b.Deadline.Equal(start.Add(3*reportInterval)) || !b.Receipt.Round(0).Equal(received.Round(0)) {
		t.Fatalf("restarted fleet checks %+v %+v", a.FailureIncident, b)
	}
	tick(restarted, start.Add(3*reportInterval-time.Second))
	if !reflect.DeepEqual(silenceAlerts(t, restarted), before) {
		t.Fatal("a hub restart manufactured alerts", silenceAlerts(t, restarted))
	}
	tick(restarted, start.Add(3*reportInterval))
	after := silenceAlerts(t, restarted)
	want := "failed app-2: app-2 has sent no report to the hub since " + received.UTC().Format("15:04 UTC")
	if len(after) != len(before)+1 || after[len(after)-1] != want {
		t.Fatal(after)
	}
}

// A replacement retires the silent node's incident without claiming
// recovery; the new node starts with its own check.
func TestReplacementRetiresSilenceIncident(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	original := joinedNode(t, code, "app-1")
	original.report(context.Background())
	tick(hub, fleetCheck(hub, "app-1").Receipt.Add(3*reportInterval))
	m := hub.hub.Nodes["app-1"]
	m.Received, m.Joined = time.Now().Add(-4*reportInterval), time.Now().Add(-time.Hour)
	m.Deadline = time.Now().Add(-time.Second)
	replacement := joinedNode(t, code, "app-1")
	replacement.report(context.Background())
	tick(hub, time.Now())
	alerts := silenceAlerts(t, hub)
	if len(alerts) != 2 || alerts[1] != "closed_without_recovery app-1: app-1 joined the fleet again as a new node; MMWatchdog does not know whether the problem is fixed" {
		t.Fatal(alerts)
	}
	if st := fleetCheck(hub, "app-1"); st.FailureIncident.Incident != "" || st.Last.Status != "ok" {
		t.Fatal("the replacement's check", st)
	}
}

// The hub keeps two reporting hub-outage alerters, preferring stable current roles
// and then the longest continuous reporting. Missing roles fill on a report.
func TestHubChoosesTwoOutageAlerters(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	h := hub.hub
	t0 := time.Now().UTC()
	history := incidents.ID()
	starts, seqs := map[string]string{}, map[string]int64{}
	for _, name := range []string{"a", "b", "c", "d"} {
		if _, status, e := h.Join(name, code.Secret, nodeSettings("warn"), "10.0.0.5", t0); status != 200 {
			t.Fatal(e)
		}
		starts[name] = incidents.ID()
	}
	report := func(at int, preferred map[string]bool, names ...string) {
		t.Helper()
		for _, name := range names {
			seqs[name]++
			reply, status, e := h.Accept(name, &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: starts[name], Seq: seqs[name], History: history, After: -1, PreferOutageAlerter: preferred[name]}, t0.Add(time.Duration(at)*time.Second))
			if status != 200 || reply.OutageAlerter != slices.Contains(h.OutageAlerters, name) {
				t.Fatal(reply, status, e)
			}
		}
	}
	wantOutageAlerters := func(names ...string) {
		t.Helper()
		got := slices.Clone(h.Fleet(t0).OutageAlerters)
		slices.Sort(got)
		slices.Sort(names)
		if !slices.Equal(got, names) {
			t.Fatalf("hub-outage alerters: got %v, want %v", got, names)
		}
		for _, node := range h.Fleet(t0).Nodes {
			if node.OutageAlerter != slices.Contains(names, node.Name) || node.Hub && node.OutageAlerter {
				t.Fatal("node hub-outage alerter role", node)
			}
		}
	}
	b, e := json.Marshal(h.Fleet(t0))
	if e != nil || !strings.Contains(string(b), `"hub_outage_alerter_nodes":[]`) || strings.Contains(string(b), `"hub_outage_alerter_node":`) {
		t.Fatal("empty hub-outage alerter list", string(b), e)
	}
	report(0, nil, "a")
	wantOutageAlerters("a") // A fleet of two has one hub-outage alerter.
	report(10, nil, "b")
	report(20, nil, "c")
	wantOutageAlerters("a", "b") // Equals never flip.
	report(30, map[string]bool{"d": true}, "d")
	wantOutageAlerters("a", "d") // The oldest current hub-outage alerter keeps the other role.
	pref := map[string]bool{"b": true, "c": true, "d": true}
	report(40, pref, "c")
	report(50, pref, "b")
	wantOutageAlerters("c", "d") // A preferred equal cannot replace a preferred hub-outage alerter.
	pref["c"] = false
	report(55, pref, "c")
	wantOutageAlerters("b", "d") // A hub-outage alerter dropping preference yields to a preferred node.
	report(60, pref, "a", "b", "c", "d")
	report(90, pref, "a", "c", "d")
	report(120, pref, "a", "c", "d")
	report(150, pref, "a")
	wantOutageAlerters("a", "d") // Silent b is replaced; d keeps its role.
	if _, status, e := h.Forget("d"); status != 200 || e != nil {
		t.Fatal(status, e)
	}
	wantOutageAlerters("a")
	report(151, pref, "c")
	wantOutageAlerters("a", "c") // The next accepted report fills a forgotten role.
	h.Close()
	reopened, e := fleet.OpenHub(hub.config.StateDir, hub.config.FleetHubRole.AddressForNodes, hub.store.Node, hub.config.FleetHubRole.ReportEvery.Value(), config.DefaultHubLimits, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	hub.hub, h = reopened, reopened
	wantOutageAlerters("a", "c")
}

// The hub-outage alerter's hub check fails after hubWatchAfter of failed reports,
// alerting directly through the usual fallback, and the next successful report
// resolves it. The role survives a restart and moves to a preferred node.
func TestOutageAlerterAlertsWhenTheHubIsUnreachable(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, outageAlerterDestinations())
	node := joinedNode(t, code, "db-1")
	node.report(context.Background())
	st := fleetCheck(node, checksandalerts.HubCheck)
	if !node.watching || st == nil || st.Last.Status != "ok" || fleetServer(t, hub, "db-1").OutageAlerter != true {
		t.Fatal("the only node did not become the hub-outage alerter", st)
	}
	received := st.Receipt
	good := node.joinCode
	unreachable := *good
	unreachable.Address = "127.0.0.1:1"
	node.joinCode = &unreachable
	node.report(context.Background())
	tick(node, received.Add(hubWatchAfter-time.Second))
	if st := fleetCheck(node, checksandalerts.HubCheck); st.FailureIncident.Incident != "" {
		t.Fatal("failed before hubWatchAfter")
	}
	tick(node, received.Add(hubWatchAfter))
	queued := queuedEvents(t, node)
	// The alert says what happened in plain words; the report's network
	// error, detail for whoever looks into it, stays in status.
	want := "no report from this node has reached the MMWatchdog fleet hub since " + received.UTC().Format("15:04 UTC") + "; mmwatchdog status on this node shows the latest error"
	if len(queued) != 1 || queued[0].Type != "failed" || queued[0].CheckGroup != checksandalerts.NodeReportsCheckGroup || queued[0].Message != want || outboxCount(t, node, config.HubDestination) != 0 {
		t.Fatal("hub-outage alerter alert", queued)
	}
	if node.link.LastError == "" {
		t.Fatal("status lost the latest report error")
	}
	node.joinCode = good
	node.report(context.Background())
	tick(node, time.Now())
	queued = queuedEvents(t, node)
	if len(queued) != 2 || queued[1].Type != "recovered" || queued[1].Message != "reports reach the hub again" {
		t.Fatal("the next successful report did not resolve the incident", queued)
	}

	other := joinedNode(t, code, "db-2")
	other.config.JoinFleetHub.PreferThisNodeAsHubOutageAlerter = true
	other.report(context.Background())
	third := joinedNode(t, code, "db-3")
	third.config.JoinFleetHub.PreferThisNodeAsHubOutageAlerter = true
	third.report(context.Background())
	node.report(context.Background())
	if node.watching || fleetCheck(node, checksandalerts.HubCheck) != nil || !other.watching || len(queuedEvents(t, node)) != 2 {
		t.Fatal("the role did not move to the preferred node quietly")
	}
	other.Close()
	restarted, e := NewDaemon(other.path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { restarted.Close() })
	if st := fleetCheck(restarted, checksandalerts.HubCheck); !restarted.watching || st == nil || st.Deadline.Before(time.Now().Add(hubWatchAfter-time.Minute)) {
		t.Fatal("a restarted hub-outage alerter lost its role or its fresh deadline", st)
	}
}

// The hub's silence alerts roll up by change alone, naming the nodes; a
// late one says whether its node reports now.
func TestSilenceAlertsRollUp(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1m"}})
	nodes := []*Daemon{}
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		s := joinedNode(t, code, name)
		s.report(context.Background())
		nodes = append(nodes, s)
	}
	tick(hub, fleetCheck(hub, "app-3").Receipt.Add(3*reportInterval))
	for _, s := range nodes {
		s.report(context.Background())
	}
	tick(hub, time.Now())
	runWorker(t, hub)
	got := dest.wait(t, 2)
	for i, want := range []string{"3 nodes stopped reporting: app-1, app-2, app-3", "3 nodes are reporting again: app-1, app-2, app-3"} {
		if got[i].Message != want || got[i].Node != hub.store.Node || got[i].CheckGroup != checksandalerts.NodeReportsCheckGroup || got[i].Check != "" || got[i].NodeCount != 3 {
			t.Fatalf("message %d: %+v", i, got[i])
		}
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(dest.received()); n != 2 {
		t.Fatal("expected two messages", dest.received())
	}
	now := time.Now().UTC()
	late := hub.hub.Late(checksandalerts.Event{Node: hub.store.Node, CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: "app-1", Type: "failed", AlertSeverity: "warn", At: now.Add(-5 * time.Minute), Message: "app-1 has sent no report to the hub since 14:00 UTC"}, now)
	if !strings.HasSuffix(late.Message, ". Now app-1 is reporting to the hub)") {
		t.Fatal(late.Message)
	}
}

// Forgetting a node deletes its entry, events, and credential, and retires
// its silence incident without claiming recovery. A forgotten hub-outage alerter leaves
// the role; a node still running joins again visibly.
func TestForgetServer(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	tick(hub, fleetCheck(hub, "app-1").Receipt.Add(3*reportInterval))
	m := hub.hub.Nodes["app-1"]
	if !slices.Equal(hub.hub.OutageAlerters, []string{"app-1"}) {
		t.Fatal("hub-outage alerter", hub.hub.OutageAlerters)
	}
	if _, e := hub.hub.DB.Exec("CREATE TRIGGER reject_delete BEFORE DELETE ON nodes BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); e != nil {
		t.Fatal(e)
	}
	if status, _ := fleetCommand(t, hub, "POST", "/fleet/forget/app-1"); status != 503 || hub.hub.Nodes["app-1"] == nil || fleetCheck(hub, "app-1") == nil {
		t.Fatal("an unsaved forget changed something", status)
	}
	hub.hub.DB.Exec("DROP TRIGGER reject_delete")
	if status, body := fleetCommand(t, hub, "POST", "/fleet/forget/app-1"); status != 200 {
		t.Fatal(status, body)
	}
	tick(hub, time.Now())
	alerts := silenceAlerts(t, hub)
	if len(alerts) != 2 || alerts[1] != "closed_without_recovery app-1: app-1 was removed from the fleet; MMWatchdog does not know whether the problem is fixed" {
		t.Fatal(alerts)
	}
	var rows int
	var outageAlerters int
	hub.hub.DB.QueryRow("SELECT (SELECT COUNT(*) FROM nodes WHERE name='app-1') + (SELECT COUNT(*) FROM events WHERE node='app-1')").Scan(&rows)
	hub.hub.DB.QueryRow("SELECT COUNT(*) FROM hub_watcher_nodes").Scan(&outageAlerters)
	if rows != 0 || outageAlerters != 0 || len(hub.hub.OutageAlerters) != 0 || hub.hub.Nodes["app-1"] != nil || hub.hub.Credentials[m.Credential] != "" || fleetCheck(hub, "app-1") != nil {
		t.Fatal("the node was not forgotten", rows, outageAlerters)
	}
	if status, _ := fleetCommand(t, hub, "POST", "/fleet/forget/app-1"); status != 404 {
		t.Fatal("forgot an unknown node", status)
	}
	if status, _ := fleetCommand(t, hub, "POST", "/fleet/forget/"+hub.store.Node); status != 400 {
		t.Fatal("forgot the hub", status)
	}
	node.report(context.Background())
	if !strings.Contains(node.link.LastError, "will join again") {
		t.Fatal("the forgotten credential was accepted", node.link)
	}
	node.report(context.Background())
	detail, _ := hub.hub.Node("app-1", time.Now())
	if node.link.LastError != "" || !detail.Joined.After(m.Joined) || len(detail.Events) != 1 || detail.Events[0].Type != "node_joined" || !slices.Equal(hub.hub.OutageAlerters, []string{"app-1"}) {
		t.Fatal("the running node did not join again", node.link, detail.Events)
	}
	if st := fleetCheck(hub, "app-1"); st == nil || st.Last.Status != "ok" {
		t.Fatal("the new membership's check", st)
	}
	// A forget applied after the node joined again changes nothing.
	hub.forgot("app-1", m.Joined, time.Now())
	if fleetCheck(hub, "app-1") == nil || len(silenceAlerts(t, hub)) != 2 {
		t.Fatal("an old forget removed a newer membership")
	}
}

// setFleetTimings rewrites the hub's configuration with these timings and
// reloads it.
func setFleetTimings(t *testing.T, hub *Daemon, timings config.FleetTimings) {
	t.Helper()
	writeConfig(t, hub.path, &config.Config{StateDir: hub.config.StateDir, Socket: hub.config.Socket, CheckGroups: map[string]checksandalerts.CheckGroup{"app": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}}, FleetHubRole: &config.FleetHubRole{ListenOnAddress: "0.0.0.0:8445", AddressForNodes: hub.config.FleetHubRole.AddressForNodes, FleetTimings: timings}})
	if _, e := hub.reload(); e != nil {
		t.Fatal(e)
	}
}

// The hub's fleet timings reach its nodes with its settings: the silence
// deadline, the hub-outage alerter's hub check, and the report interval follow them.
// A changed timing keeps each check's state and applies from its next
// receipt, and a restarted node keeps its copy's timings.
func TestFleetTimingsFollowTheHub(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	short := config.FleetTimings{ReportEvery: "10s", AlertAfterHubUnreachableFor: "25s", SendAlertWithoutHubAfterWaitingFor: "5s"}
	setFleetTimings(t, hub, short)
	node := joinedNode(t, code, "db-1")
	node.report(context.Background())
	silence, watch := fleetCheck(hub, "db-1"), fleetCheck(node, checksandalerts.HubCheck)
	if node.fleetSettings.Fleet != short || node.reportEvery() != 10*time.Second || hub.reportEvery() != 10*time.Second || !silence.Deadline.Equal(silence.Receipt.Add(30*time.Second)) || !watch.Deadline.Equal(watch.Receipt.Add(25*time.Second)) {
		t.Fatalf("timings on the hub %+v and the hub-outage alerter %+v", silence, watch)
	}
	if s := hub.hub.View("db-1", hub.hub.Nodes["db-1"], silence.Receipt.Add(30*time.Second)); s.State != "silent" {
		t.Fatal("the fleet view did not follow report_every", s.State)
	}
	tick(hub, silence.Receipt.Add(30*time.Second))
	longer := config.FleetTimings{ReportEvery: "20s", AlertAfterHubUnreachableFor: "1m", SendAlertWithoutHubAfterWaitingFor: "5s"}
	setFleetTimings(t, hub, longer)
	if st := fleetCheck(hub, "db-1"); st.FailureIncident.Incident == "" || len(silenceAlerts(t, hub)) != 1 {
		t.Fatal("a timing change retired the silence incident", silenceAlerts(t, hub))
	}
	node.report(context.Background())
	tick(hub, time.Now())
	silence, watch = fleetCheck(hub, "db-1"), fleetCheck(node, checksandalerts.HubCheck)
	alerts := silenceAlerts(t, hub)
	if node.fleetSettings.Fleet != longer || node.reportEvery() != 20*time.Second || len(alerts) != 2 || alerts[1] != "recovered db-1: db-1 is reporting again" || !silence.Deadline.Equal(silence.Receipt.Add(time.Minute)) || !watch.Deadline.Equal(watch.Receipt.Add(time.Minute)) {
		t.Fatalf("changed timings %v %+v %+v", alerts, silence, watch)
	}
	node.Close()
	restarted, e := NewDaemon(node.path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { restarted.Close() })
	if st := fleetCheck(restarted, checksandalerts.HubCheck); restarted.reportEvery() != 20*time.Second || st == nil || st.Deadline.Before(time.Now().Add(50*time.Second)) {
		t.Fatal("a restarted node lost its copy's timings", st)
	}
}

func TestFleetTimingChangePreservesCurrentDeadlines(t *testing.T) {
	t.Parallel()
	d := newHubDaemon(t, nil)
	h := d.hub
	at := time.Now().UTC()
	if _, status, err := h.Join("app-1", h.Code.Secret, nodeSettings("warn"), "original", at); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	d.joined("app-1", at, nodeSettings("warn"))
	r := fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: -1, Sent: at}
	if _, status, err := h.Accept("app-1", &r, at); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	d.receipt("app-1", time.Time{}, nodeSettings("warn"), at)
	deadline := fleetCheck(d, "app-1").Deadline
	setFleetTimings(t, d, config.FleetTimings{ReportEvery: "1s", AlertAfterHubUnreachableFor: "3m", SendAlertWithoutHubAfterWaitingFor: "30s"})
	if got := fleetCheck(d, "app-1").Deadline; !got.Equal(deadline) {
		t.Fatal("timing change altered the existing receipt deadline", got, deadline)
	}
	claimAt := at.Add(5 * time.Second)
	if _, status, _ := h.Join("app-1", h.Code.Secret, nodeSettings("warn"), "claimant", claimAt); status != http.StatusConflict {
		t.Fatalf("live node was replaced at %v before its %v report deadline: status=%d", claimAt, deadline, status)
	}
	r.Seq++
	if _, status, err := h.Accept("app-1", &r, claimAt); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	d.receipt("app-1", time.Time{}, nodeSettings("warn"), claimAt)
	next := claimAt.Add(3 * time.Second)
	if !h.Nodes["app-1"].Deadline.Equal(next) || !fleetCheck(d, "app-1").Deadline.Equal(next) {
		t.Fatal("next report did not apply the new interval")
	}
}

// The message of a raise by node group names the silent nodes in plain
// English, and at most 20 of them, like a combined alert.
func TestSilentNodesText(t *testing.T) {
	t.Parallel()
	many := []string{}
	for i := 1; i <= 25; i++ {
		many = append(many, fmt.Sprintf("jobs-%02d", i))
	}
	for nodes, want := range map[string]string{
		"jobs-1":                "jobs-1 is silent",
		"jobs-1 jobs-2":         "jobs-1 and jobs-2 are silent",
		"jobs-1 jobs-2 jobs-3":  "jobs-1, jobs-2, and jobs-3 are silent",
		strings.Join(many, " "): strings.Join(many[:20], ", ") + ", and 5 more are silent",
	} {
		if got := silentNodesText(strings.Fields(nodes)); got != want {
			t.Errorf("%s: got %q, want %q", nodes, got, want)
		}
	}
}

// outageAlerterDestinations leaves the direct alerts queued without a worker.
func outageAlerterDestinations() map[string]checksandalerts.Destination {
	return map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/alerts"}, TimeLimitPerDeliveryAttempt: "1m"}}
}
