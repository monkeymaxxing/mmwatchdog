package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// reportInterval is the default report_every the test hubs use.
const reportInterval = 30 * time.Second

// startHub starts a hub daemon and serves its joins, reports, and alerts over
// TLS with its own certificate. The join code names that node's address.
func startHub(t *testing.T) (*Daemon, fleet.JoinCode) {
	t.Helper()
	d, code, _ := startHubWith(t, nil)
	return d, code
}

// startHubWith starts a hub with these alert destinations and returns its
// TLS node too.
func startHubWith(t *testing.T, destinations map[string]checksandalerts.Destination) (*Daemon, fleet.JoinCode, *httptest.Server) {
	t.Helper()
	return startHubWithModules(t, destinations, nil)
}

// startHubWithModules is startHubWith for destinations that deliver through
// modules of the project's own: install writes those modules into the hub's
// external_module_dir before the hub starts.
func startHubWithModules(t *testing.T, destinations map[string]checksandalerts.Destination, install func(externalModuleDir string)) (*Daemon, fleet.JoinCode, *httptest.Server) {
	t.Helper()
	return startHubOnClock(t, destinations, install, time.Now)
}

// startHubOnClock is startHubWithModules for a hub whose clock is clock.
func startHubOnClock(t *testing.T, destinations map[string]checksandalerts.Destination, install func(externalModuleDir string), clock func() time.Time) (*Daemon, fleet.JoinCode, *httptest.Server) {
	t.Helper()
	d := newHubDaemonOnClock(t, destinations, install, clock)
	srv := httptest.NewUnstartedServer(d.hubRoutes())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{d.hub.Cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	code := d.hub.Code
	code.Address = srv.Listener.Addr().String()
	return d, code, srv
}

// joinedNode starts a daemon named name with join_fleet_hub set to code, and its
// one check observed healthy.
func joinedNode(t *testing.T, code fleet.JoinCode, name string) *Daemon {
	t.Helper()
	return joinedNodeOnClock(t, code, name, time.Now)
}

// joinedNodeOnClock is joinedNode for a node whose clock is clock.
func joinedNodeOnClock(t *testing.T, code fleet.JoinCode, name string, clock func() time.Time) *Daemon {
	t.Helper()
	p, c := daemonConfig(t)
	c.JoinFleetHub = &config.JoinFleetHub{JoinCodeFile: filepath.Join(p, "hub-join")}
	if e := os.WriteFile(c.JoinFleetHub.JoinCodeFile, []byte(code.String()+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	writeConfig(t, p, c)
	d, e := newDaemon(p, clock)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	d.store.Node, d.link.Node = name, name
	d.Build = fleet.Build{Version: "test", Revision: name}
	d.snapshot.Checks["app/shell"].Last = checksandalerts.Observation{Status: "ok", Message: "fine", At: d.now()}
	d.checks["app/shell"].fresh = true
	return d
}

func fleetServer(t *testing.T, hub *Daemon, name string) fleet.FleetNode {
	t.Helper()
	for _, s := range hub.hub.Fleet(time.Now()).Nodes {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("fleet has no node %q", name)
	return fleet.FleetNode{}
}

// nodeSettings are the settings that a node sends for the fleet when it
// states only the severity of its silence alert.
func nodeSettings(alertSeverityWhenThisNodeGoesSilent string) config.NodeFleetSettings {
	return config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: alertSeverityWhenThisNodeGoesSilent}
}

// On a joined node, maintenance on and off return once the hub has accepted
// the node's report with the change, so a reboot right after "maintenance
// on" sends no alert that the node stopped reporting. When the hub does not
// accept the report, the change stays on the node and the reply says why.
func TestMaintenanceOnAJoinedNodeReachesTheHubAtOnce(t *testing.T) {
	t.Parallel()
	hub, code, srv := startHubWith(t, nil)
	node := joinedNode(t, code, "app-1")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go node.reports(ctx)
	// The node has joined, so its next periodic report is a whole
	// report_every away.
	testhelpers.WaitFor(t, "the node joins", func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return !node.link.LastReport.IsZero()
	})
	maintenance := func(action, body string) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		node.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/maintenance/"+action+"/"+checksandalerts.EveryCheckGroup, strings.NewReader(body)))
		reply := map[string]any{}
		if e := json.Unmarshal(w.Body.Bytes(), &reply); w.Code != 200 || e != nil {
			t.Fatal(w.Code, w.Body.String(), e)
		}
		return reply
	}
	reply := maintenance("on", `{"for":"15m","reason":"reboot after updates"}`)
	report := fleetServer(t, hub, "app-1").NodeStatusReport
	if reply["fleet_hub_accepted_the_report"] != true || reply["fleet_hub_report_error"] != nil || report == nil || report.Maintenance[checksandalerts.EveryCheckGroup].Reason != "reboot after updates" {
		t.Fatalf("the hub must have the window when the command returns: %v %+v", reply, report)
	}
	hub.mu.Lock()
	held := hub.fleetSuppressed(hub.checks[fleet.FleetKey("app-1")], time.Now().Add(10*time.Minute))
	hub.mu.Unlock()
	if !held {
		t.Fatal("the hub must hold the node's silence while the window lasts")
	}
	srv.Close()
	reply = maintenance("off", `{}`)
	node.mu.Lock()
	_, open := node.snapshot.Maintenance[checksandalerts.EveryCheckGroup]
	node.mu.Unlock()
	if reply["fleet_hub_accepted_the_report"] != nil || reply["fleet_hub_report_error"] == nil || open {
		t.Fatalf("without the hub the window ends on the node, and the reply says the hub did not get it: %v", reply)
	}
}

func TestJoinAndReport(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	if node.link.LastError != "" || node.link.LastReport.IsZero() {
		t.Fatal("report failed", node.link)
	}
	// The node keeps its credential privately; the hub keeps only its hash.
	path := filepath.Join(node.config.StateDir, fleet.CredentialFile)
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file", info, e)
	}
	cred := fleet.ReadCredential(path)
	m := hub.hub.Nodes["app-1"]
	if cred == nil || cred.Hub != code.Fingerprint || m.Credential != fleet.HashCredential(cred.Credential) || strings.Contains(string(mustJSON(t, m)), cred.Credential) {
		t.Fatal("credential", cred, m)
	}
	s := fleetServer(t, hub, "app-1")
	if s.State != "reporting" || s.ExitCode != 0 || s.NodeStatusReport == nil || s.NodeStatusReport.Checks != nil || s.NodeStatusReport.Build != node.Build {
		t.Fatalf("fleet view %+v", s)
	}
	// Events follow once the hub has told the node its cursor.
	setMaintenance(t, node, checksandalerts.EveryCheckGroup, "10m")
	node.report(context.Background())
	node.report(context.Background())
	// The report carries the status summary: starting maintenance requires a
	// fresh observation, so the check's health is unknown.
	detail, e := hub.hub.Node("app-1", time.Now())
	if e != nil || detail.ExitCode != 3 || detail.NodeStatusReport.Checks["app/shell"].State != "unknown" || detail.NodeStatusReport.Maintenance[checksandalerts.EveryCheckGroup].Until.IsZero() {
		t.Fatal(detail, e)
	}
	types := []string{}
	for _, ev := range detail.Events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, " ") != "maintenance_on node_joined" || detail.Events[0].Node != "app-1" {
		t.Fatal("hub events", types, detail.Events)
	}
	if node.rep.After != detail.Events[0].Seq || hub.hub.Nodes["app-1"].Cursor != node.rep.After {
		t.Fatal("cursor", node.rep.After, hub.hub.Nodes["app-1"].Cursor)
	}
	// A restarted hub keeps nodes, credentials, reports, and events.
	hub.report(context.Background())
	if self := fleetServer(t, hub, hub.store.Node); !self.Hub || self.State != "reporting" {
		t.Fatalf("hub's own entry %+v", self)
	}
	old := hub.hub
	old.Close()
	reopened, e := fleet.OpenHub(hub.config.StateDir, hub.config.FleetHubRole.AddressForNodes, hub.store.Node, hub.config.FleetHubRole.ReportEvery.Value(), config.DefaultHubLimits, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	hub.hub = reopened
	if reopened.Code != old.Code || reopened.Credentials[m.Credential] != "app-1" || reopened.Nodes["app-1"].NodeStatusReport == nil || reopened.Nodes["app-1"].Cursor != node.rep.After {
		t.Fatal("hub state did not survive restart")
	}
}

// Joining a hub changes nothing that a node owns. A standalone node whose
// check has failed, with its incident open and its recovery command run,
// joins a hub through a reload, as a deploy that adds join_fleet_hub does:
// its checks with their states and schedules, its incident, its events, its
// recovery attempts, and its status exit code stay as they were, and the
// joined node, the hub-outage alerter node here, has node_reports/to_hub besides.
// With the hub unreachable after the join, the check recovers on the node
// as before. Only the route of the alerts changed: the recovery is queued
// for the hub.
func TestJoiningAHubChangesOnlyTheRouteOfTheAlerts(t *testing.T) {
	t.Parallel()
	_, code, srv := startHubWith(t, nil)
	p, c := daemonConfig(t)
	dir := t.TempDir()
	failsWhileThisFileExists, recoveryCommandRan := filepath.Join(dir, "fails"), filepath.Join(dir, "recovery-command-ran")
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
		Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks: map[string]checksandalerts.NamedCheck{"health": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "test ! -e " + testhelpers.ShellQuote(failsWhileThisFileExists), SuccessExitCodes: []int{0}, ExecOptions: testhelpers.OwnOptions(t)}}}},
		RecoveryCommand: &checksandalerts.RecoveryCommand{Script: "echo ran >> " + testhelpers.ShellQuote(recoveryCommandRan), TimeLimitPerAttempt: "5s", ExecOptions: testhelpers.OwnOptions(t),
			RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"health"}, Attempts: "1", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "1s"}},
	}}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://" + testhelpers.AddressWhereNothingListens(t, "tcp") + "/hook"}, TimeLimitPerDeliveryAttempt: "1s"}}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	d.store.Node = "app-1"
	at := time.Now().UTC()
	observe := func(failing bool) {
		t.Helper()
		if failing {
			if e := os.WriteFile(failsWhileThisFileExists, nil, 0600); e != nil {
				t.Fatal(e)
			}
		} else if e := os.Remove(failsWhileThisFileExists); e != nil {
			t.Fatal(e)
		}
		d.schedule(testhelpers.TestContext(t), at)
		d.workers.Wait()
		at = at.Add(time.Minute)
	}
	// What the node owns: each check as mmwatchdog status shows it, the status
	// summary with its exit code, the attempts of its recovery command, the
	// types of its check group's events, and how often the command ran.
	type owned struct {
		Checks                  map[string]any
		Summary                 any
		RecoveryCommandAttempts any
		Events                  []string
		RecoveryCommandRuns     string
	}
	ownedNow := func() owned {
		t.Helper()
		status := statusJSON(t, d)
		o := owned{Checks: status["checks"].(map[string]any), Summary: status["summary"], RecoveryCommandAttempts: status["recovery_command_attempts"]}
		events, e := d.store.CheckGroupEvents("app", 100)
		if e != nil {
			t.Fatal(e)
		}
		for i := len(events) - 1; i >= 0; i-- {
			o.Events = append(o.Events, events[i].Type)
		}
		ran, _ := os.ReadFile(recoveryCommandRan)
		o.RecoveryCommandRuns = string(ran)
		return o
	}
	exitCode := func(o owned) int { return int(o.Summary.(map[string]any)["exit_code"].(float64)) }
	routes := func() []string {
		t.Helper()
		rows, e := d.store.DB.Query("SELECT destination, json_extract(event,'$.type') FROM outbox ORDER BY seq")
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var destination, kind string
			if e := rows.Scan(&destination, &kind); e != nil {
				t.Fatal(e)
			}
			out = append(out, kind+" to "+destination)
		}
		return out
	}
	observe(true)
	standalone := ownedNow()
	d.mu.Lock()
	incident := d.snapshot.Checks["app/health"].FailureIncident.Incident
	d.mu.Unlock()
	if incident == "" || exitCode(standalone) != 1 || standalone.RecoveryCommandRuns != "ran\n" || !reflect.DeepEqual(routes(), []string{"failed to ops"}) {
		t.Fatalf("the standalone node's failure opened no incident, exited with %d, did not run the recovery command, or queued %v: %+v", exitCode(standalone), routes(), standalone)
	}
	// The node joins the hub through a reload. A joined node gets its alert
	// destinations from the hub.
	c.Alerts.Destinations = nil
	c.JoinFleetHub = &config.JoinFleetHub{JoinCodeFile: filepath.Join(p, "hub-join")}
	if e := os.WriteFile(c.JoinFleetHub.JoinCodeFile, []byte(code.String()+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	writeConfig(t, p, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	d.report(context.Background())
	if d.link == nil || d.link.LastError != "" || !d.watching {
		t.Fatal("the node did not join or is not the hub-outage alerter node", d.link)
	}
	// From here on the hub is unreachable.
	srv.Close()
	joined := ownedNow()
	hubCheck, ok := joined.Checks[fleet.FleetKey(checksandalerts.HubCheck)].(map[string]any)
	if !ok || hubCheck["state"] != "ok" {
		t.Fatalf("the hub-outage alerter node has no healthy node_reports/to_hub: %+v", joined.Checks)
	}
	delete(joined.Checks, fleet.FleetKey(checksandalerts.HubCheck))
	if !reflect.DeepEqual(standalone, joined) {
		t.Fatalf("joining changed what the node owns\nstandalone: %+v\njoined:     %+v", standalone, joined)
	}
	d.mu.Lock()
	kept := d.snapshot.Checks["app/health"].FailureIncident.Incident
	d.mu.Unlock()
	if kept != incident {
		t.Fatal("joining replaced the open incident", incident, kept)
	}
	observe(false)
	recovered := ownedNow()
	if exitCode(recovered) != 0 || recovered.RecoveryCommandRuns != "ran\n" || !reflect.DeepEqual(recovered.Events, append(append([]string{}, standalone.Events...), "recovered")) {
		t.Fatalf("the check did not recover on the node by itself: exit code %d, %+v", exitCode(recovered), recovered)
	}
	if got := routes(); len(got) == 0 || got[len(got)-1] != "recovered to "+config.HubDestination {
		t.Fatalf("the recovery is not queued for the hub: %v", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// Joining needs the secret; a name belongs to its node while it reports,
// and passes to a new node only once that one is silent.
func TestJoinSecretConflictAndReplacement(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	wrong := code
	wrong.Secret = strings.Repeat("0", 64)
	stranger := joinedNode(t, wrong, "app-2")
	stranger.report(context.Background())
	if !strings.Contains(stranger.link.LastError, "invalid join code") || hub.hub.Refused.Count != 1 || hub.hub.Nodes["app-2"] != nil {
		t.Fatal("wrong secret", stranger.link, hub.hub.Refused)
	}
	var rows int
	hub.hub.DB.QueryRow("SELECT COUNT(*) FROM nodes WHERE name='app-2'").Scan(&rows)
	if rows != 0 {
		t.Fatal("a refused join added a row")
	}
	original := joinedNode(t, code, "app-1")
	original.report(context.Background())
	credential := hub.hub.Nodes["app-1"].Credential
	clone := joinedNode(t, code, "app-1")
	clone.report(context.Background())
	m := hub.hub.Nodes["app-1"]
	if !strings.Contains(clone.link.LastError, "is in use") || m.Credential != credential || m.Conflict == nil {
		t.Fatal("claim on a reporting node", clone.link, m)
	}
	if _, status, _ := hub.hub.Join(hub.store.Node, code.Secret, nodeSettings("warn"), "10.0.0.9", time.Now()); status != 409 {
		t.Fatal("joined under the hub's own name", status)
	}
	original.report(context.Background())
	if original.link.LastError != "" {
		t.Fatal("conflict disturbed the reporting node", original.link)
	}
	// Once silent, the name passes to the new node and the old credential stops working.
	m.Received, m.Joined = time.Now().Add(-4*reportInterval), time.Now().Add(-time.Hour)
	m.Deadline = time.Now().Add(-time.Second)
	clone.report(context.Background())
	if clone.link.LastError != "" || hub.hub.Nodes["app-1"].Replaced.IsZero() {
		t.Fatal("replacement", clone.link)
	}
	original.report(context.Background())
	if !strings.Contains(original.link.LastError, "will join again") || fleet.ReadCredential(filepath.Join(original.config.StateDir, fleet.CredentialFile)) != nil {
		t.Fatal("replaced node kept its credential", original.link)
	}
	detail, _ := hub.hub.Node("app-1", time.Now())
	if detail.Events[0].Type != "node_replaced" {
		t.Fatal(detail.Events)
	}
}

// A report replaces the saved one only if newer: by sequence within a daemon
// start, and never from the start most recently replaced.
func TestReportsReplaceOnlyWhenNewer(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	h := hub.hub
	if _, status, e := h.Join("app-1", code.Secret, nodeSettings("warn"), "10.0.0.5", time.Now()); status != 200 {
		t.Fatal(e)
	}
	history, a, b := incidents.ID(), incidents.ID(), incidents.ID()
	send := func(start string, seq int64) int {
		_, status, _ := h.Accept("app-1", &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: start, Seq: seq, History: history, After: -1}, time.Now())
		return status
	}
	for _, step := range []struct {
		start  string
		seq    int64
		status int
	}{{a, 2, 200}, {a, 1, 409}, {a, 2, 409}, {a, 3, 200}, {b, 1, 200}, {a, 4, 409}, {b, 2, 200}} {
		if status := send(step.start, step.seq); status != step.status {
			t.Fatal(step, status)
		}
	}
	received := h.Nodes["app-1"].Received
	send(a, 5)
	if !h.Nodes["app-1"].Received.Equal(received) {
		t.Fatal("an old report counted as current")
	}
}

// Events count only when they continue from the hub's cursor; events lost to
// local retention leave a gap, and another database starts over.
func TestReportEventsFollowCursor(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	h := hub.hub
	h.Join("app-1", code.Secret, nodeSettings("warn"), "10.0.0.5", time.Now())
	start, history := incidents.ID(), incidents.ID()
	seq := int64(0)
	send := func(history string, after int64, events ...int64) int64 {
		t.Helper()
		seq++
		r := &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: start, Seq: seq, History: history, After: after, MeasurementHistoryID: start, ReadingsAfter: -1}
		for _, s := range events {
			r.Events = append(r.Events, checksandalerts.Event{ID: incidents.ID(), Seq: s, Type: "failed", AlertSeverity: "warn", At: time.Now()})
		}
		if e := r.Validate(); e != nil {
			t.Fatal(e)
		}
		reply, status, e := h.Accept("app-1", r, time.Now())
		if status != 200 {
			t.Fatal(status, e)
		}
		return reply.EventsCursor
	}
	count := func(kind string) int {
		var n int
		h.DB.QueryRow("SELECT COUNT(*) FROM events WHERE node='app-1' AND json_extract(data,'$.type')=?", kind).Scan(&n)
		return n
	}
	if cursor := send(history, -1); cursor != 0 {
		t.Fatal(cursor)
	}
	if cursor := send(history, 0, 1, 2); cursor != 2 || count("failed") != 2 {
		t.Fatal(cursor, count("failed"))
	}
	if cursor := send(history, 1, 3); cursor != 2 || count("failed") != 2 {
		t.Fatal("events not following the cursor were saved", cursor)
	}
	if cursor := send(history, 2, 5); cursor != 5 || count("history_gap") != 1 {
		t.Fatal("lost events left no gap", cursor)
	}
	if cursor := send(incidents.ID(), 0, 1); cursor != 1 || count("failed") != 4 {
		t.Fatal("a new database did not start the cursor over", cursor)
	}
	if e := (&fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: start, Seq: 9, History: history, After: 3, MeasurementHistoryID: start, ReadingsAfter: -1, Events: []checksandalerts.Event{{Seq: 3}}}).Validate(); e == nil {
		t.Fatal("accepted events that do not follow the cursor")
	}
}

// The node's local retention can remove events before the hub has them.
func TestReporterSendsEventsAfterCursorWithGap(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	setMaintenance(t, node, "app", "10m")
	setMaintenance(t, node, "app", "off")
	setMaintenance(t, node, "app", "10m")
	if _, e := node.store.DB.Exec("DELETE FROM events WHERE seq < (SELECT MAX(seq) FROM events)"); e != nil {
		t.Fatal(e)
	}
	node.report(context.Background())
	detail, _ := hub.hub.Node("app-1", time.Now())
	if len(detail.Events) != 3 || detail.Events[0].Type != "maintenance_on" || detail.Events[1].Type != "history_gap" || !strings.HasPrefix(detail.Events[1].Message, "2 events") {
		t.Fatal(detail.Events)
	}
}

// A node whose state.db was restored from an older copy has given out fewer
// event sequence numbers than the hub holds. Its next events continue after
// the hub's number, so the hub takes them: without that, the hub would ignore
// every new event until the node's numbers had passed its own.
func TestANodeWithARestoredStateDatabaseKeepsSendingEvents(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	setMaintenance(t, node, "app", "10m")
	setMaintenance(t, node, "app", "off")
	setMaintenance(t, node, "app", "10m")
	node.report(context.Background())
	held := hub.hub.Nodes["app-1"].Cursor
	if held < 3 || node.rep.After != held {
		t.Fatal(held, node.rep.After)
	}
	// state.db goes back to the copy that held one event, and MMWatchdog starts: its
	// first report asks where the hub is.
	if _, e := node.store.DB.Exec("DELETE FROM events WHERE seq>1; UPDATE sqlite_sequence SET seq=1 WHERE name='events'"); e != nil {
		t.Fatal(e)
	}
	node.rep.After = -1
	node.report(context.Background())
	if node.link.LastError != "" || node.rep.After != held {
		t.Fatal(node.link.LastError, node.rep.After)
	}
	setMaintenance(t, node, "app", "off")
	node.report(context.Background())
	detail, _ := hub.hub.Node("app-1", time.Now())
	if hub.hub.Nodes["app-1"].Cursor != held+1 || detail.Events[0].Type != "maintenance_off" || detail.Events[0].Seq != held+1 {
		t.Fatal(hub.hub.Nodes["app-1"].Cursor, detail.Events[0])
	}
	// A hub that holds less than the node changes nothing, and neither does
	// a node that has recorded no event yet and a hub that holds none.
	if e := node.store.ContinueEventsAfter(held - 1); e != nil {
		t.Fatal(e)
	}
	setMaintenance(t, node, "app", "10m")
	node.report(context.Background())
	if hub.hub.Nodes["app-1"].Cursor != held+2 {
		t.Fatal(hub.hub.Nodes["app-1"].Cursor)
	}
	fresh := joinedNode(t, code, "app-2")
	if e := fresh.store.ContinueEventsAfter(0); e != nil {
		t.Fatal(e)
	}
	if e := fresh.store.ContinueEventsAfter(7); e != nil {
		t.Fatal(e)
	}
	setMaintenance(t, fresh, "app", "10m")
	if events, e := fresh.store.EventsAfter(0, 10, 0); e != nil || len(events) != 1 || events[0].Seq != 8 {
		t.Fatal(events, e)
	}
	// A number is never used again, also when retention has removed its
	// event and the hub holds less.
	if _, e := fresh.store.DB.Exec("DELETE FROM events"); e != nil {
		t.Fatal(e)
	}
	if e := fresh.store.ContinueEventsAfter(3); e != nil {
		t.Fatal(e)
	}
	setMaintenance(t, fresh, "app", "off")
	if events, e := fresh.store.EventsAfter(0, 10, 0); e != nil || len(events) != 1 || events[0].Seq != 9 {
		t.Fatal(events, e)
	}

	// The hub reports to itself, and its own state.db can be restored from
	// an older copy as well.
	own := hub.store.Node
	hub.report(context.Background())
	setMaintenance(t, hub, "app", "10m")
	setMaintenance(t, hub, "app", "off")
	setMaintenance(t, hub, "app", "10m")
	hub.report(context.Background())
	held = hub.hub.Nodes[own].Cursor
	if held < 3 {
		t.Fatal(held)
	}
	if _, e := hub.store.DB.Exec("DELETE FROM events WHERE seq>1; UPDATE sqlite_sequence SET seq=1 WHERE name='events'"); e != nil {
		t.Fatal(e)
	}
	hub.rep.After = -1
	hub.report(context.Background())
	setMaintenance(t, hub, "app", "off")
	hub.report(context.Background())
	detail, _ = hub.hub.Node(own, time.Now())
	if hub.hub.Nodes[own].Cursor != held+1 || detail.Events[0].Type != "maintenance_off" || detail.Events[0].Seq != held+1 {
		t.Fatal(hub.hub.Nodes[own].Cursor, detail.Events[0])
	}

	// A node continues its numbers after the hub's, so the hub takes no
	// number from a report that a node could not continue after.
	report := func(change func(*fleet.NodeStatusReport)) error {
		r := &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: 0, MeasurementHistoryID: incidents.ID(), ReadingsAfter: -1}
		change(r)
		return r.Validate()
	}
	if e := report(func(r *fleet.NodeStatusReport) {
		r.After, r.Events = fleet.MaxSequenceNumber-1, []checksandalerts.Event{{Seq: fleet.MaxSequenceNumber}}
	}); e != nil {
		t.Fatal(e)
	}
	if e := report(func(r *fleet.NodeStatusReport) {
		r.After, r.Events = fleet.MaxSequenceNumber, []checksandalerts.Event{{Seq: fleet.MaxSequenceNumber + 1}}
	}); e == nil || !strings.Contains(e.Error(), "node status report events must have increasing sequence numbers after events_after_sequence_number") {
		t.Fatal(e)
	}
	if e := report(func(r *fleet.NodeStatusReport) { r.After = fleet.MaxSequenceNumber + 1 }); e == nil {
		t.Fatal("accepted a cursor that no node reaches")
	}
}

// The hub acknowledges only after saving. An authenticated report still keeps
// its node reporting, and the fleet view shows the storage error.
func TestHubStorageFailure(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	cursor := hub.hub.Nodes["app-1"].Cursor
	setMaintenance(t, node, "app", "10m")
	if _, e := hub.hub.DB.Exec("CREATE TRIGGER reject_server BEFORE UPDATE ON nodes BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); e != nil {
		t.Fatal(e)
	}
	before := hub.hub.Nodes["app-1"].Received
	node.report(context.Background())
	m := hub.hub.Nodes["app-1"]
	if !strings.Contains(node.link.LastError, "503") || !m.Received.After(before) || m.Cursor != cursor || node.rep.After != cursor {
		t.Fatal("unsaved report", node.link, m.Cursor, node.rep.After)
	}
	f := hub.hub.Fleet(time.Now())
	if f.ExitCode != 3 || !strings.Contains(f.StorageError, "fixture storage failure") || fleetServer(t, hub, "app-1").State != "reporting" {
		t.Fatalf("fleet view %+v", f)
	}
	status, body := fleetCommand(t, hub, "GET", "/fleet?node=app-1")
	var detail fleet.FleetNode
	if err := json.Unmarshal([]byte(body), &detail); err != nil || status != 200 || detail.ExitCode != 3 || !strings.Contains(detail.StorageError, "fixture storage failure") {
		t.Fatal(status, body, err)
	}
	hub.hub.DB.Exec("DROP TRIGGER reject_server")
	node.report(context.Background())
	if node.link.LastError != "" || hub.hub.Fleet(time.Now()).StorageError != "" || hub.hub.Nodes["app-1"].Cursor == cursor {
		t.Fatal("storage recovery", node.link)
	}
}

func TestFleetStatesAndExitCodes(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	h := hub.hub
	now := time.Now()
	h.Join("app-1", code.Secret, nodeSettings("warn"), "10.0.0.5", now)
	view := func(at time.Time) fleet.FleetNode { return h.View("app-1", h.Nodes["app-1"], at) }
	if s := view(now); s.State != "waiting_for_first_report" || s.ExitCode != 0 {
		t.Fatal(s)
	}
	if s := view(now.Add(3 * reportInterval)); s.State != "silent" || s.ExitCode != 1 {
		t.Fatal(s)
	}
	h.Accept("app-1", &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: -1, Summary: incidents.StatusSummary{ExitCode: 1, Attention: []incidents.Attention{}}}, now)
	if s := view(now); s.State != "reporting" || s.ExitCode != 1 {
		t.Fatal(s)
	}
	// A restarted hub grants a fresh window before a node can be silent.
	h.Nodes["app-1"].Deadline = now.Add(7 * reportInterval)
	h.Nodes["app-1"].Reported = false
	if s := view(now.Add(5 * reportInterval)); s.State != "waiting_for_first_report" || s.ExitCode != 0 {
		t.Fatal(s)
	}
	if s := view(now.Add(7 * reportInterval)); s.State != "silent" {
		t.Fatal(s)
	}
}

// The join code identifies the hub by its certificate. A node given a code
// for another hub joins that hub; a certificate that does not match the code
// is refused before anything is sent.
func TestJoinCodeIdentifiesTheHub(t *testing.T) {
	t.Parallel()
	first, code := startHub(t)
	second, other := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	if first.hub.Nodes["app-1"] == nil {
		t.Fatal("did not join the first hub")
	}
	forged := code
	forged.Address = other.Address
	os.WriteFile(node.config.JoinFleetHub.JoinCodeFile, []byte(forged.String()), 0600)
	if _, e := node.reload(); e != nil {
		t.Fatal(e)
	}
	node.report(context.Background())
	if !strings.Contains(node.link.LastError, "certificate does not match") || second.hub.Nodes["app-1"] != nil {
		t.Fatal("pinning", node.link)
	}
	os.WriteFile(node.config.JoinFleetHub.JoinCodeFile, []byte(other.String()), 0600)
	if _, e := node.reload(); e != nil {
		t.Fatal(e)
	}
	node.report(context.Background())
	cred := fleet.ReadCredential(filepath.Join(node.config.StateDir, fleet.CredentialFile))
	if node.link.LastError != "" || second.hub.Nodes["app-1"] == nil || cred.Hub != other.Fingerprint {
		t.Fatal("did not join the second hub", node.link)
	}
}

// A node that holds no credential for its hub reports at once, at its start
// and after the reload that gives it a join code for another hub, because
// that report joins. A node that holds one keeps the random wait that spreads
// out nodes restarted together.
func TestANodeWithoutACredentialJoinsAtOnce(t *testing.T) {
	t.Parallel()
	first, code := startHub(t)
	second, other := startHub(t)
	node := joinedNode(t, code, "app-1")
	knows := func(hub *Daemon) func() bool {
		return func() bool {
			for _, s := range hub.hub.Fleet(time.Now()).Nodes {
				if s.Name == "app-1" {
					return true
				}
			}
			return false
		}
	}
	if !node.joinsWithNextReport() {
		t.Fatal("a node without a credential would wait for its first report")
	}
	ctx, cancel := context.WithCancel(context.Background())
	reporting := make(chan struct{})
	go func() { defer close(reporting); node.reports(ctx) }()
	testhelpers.WaitFor(t, "the join at the start", knows(first))
	os.WriteFile(node.config.JoinFleetHub.JoinCodeFile, []byte(other.String()), 0600)
	if _, e := node.reload(); e != nil {
		t.Fatal(e)
	}
	testhelpers.WaitFor(t, "the join after the reload", knows(second))
	// The hub knows the node as soon as it has answered the join; the node
	// holds its credential only once it has read that answer and saved it.
	testhelpers.WaitFor(t, "the node's credential for the second hub", func() bool {
		saved := fleet.ReadCredential(filepath.Join(node.config.StateDir, fleet.CredentialFile))
		return saved != nil && saved.Hub == other.Fingerprint
	})
	cancel()
	<-reporting
	if node.joinsWithNextReport() {
		t.Fatal("a node that holds a credential for its hub would skip the random wait")
	}
}

// Identity comes from the credential before the body is read, and reports are
// decoded strictly.
func TestReportRequestsAreAuthenticatedAndStrict(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	cred := fleet.ReadCredential(filepath.Join(node.config.StateDir, fleet.CredentialFile)).Credential
	post := func(credential, body string) int {
		r := httptest.NewRequest("POST", "https://hub.test/v1/report", bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		hub.hubRoutes().ServeHTTP(w, r)
		return w.Code
	}
	if status := post(strings.Repeat("a", 64), "not json"); status != http.StatusUnauthorized {
		t.Fatal(status)
	}
	valid := `{"daemon_start_id":"` + incidents.ID() + `","report_sequence_number":99,"event_history_id":"` + incidents.ID() + `","events_after_sequence_number":-1,"measurement_history_id":"` + incidents.ID() + `","readings_after_sequence_number":-1,"summary":{"exit_code":0,"attention":[]},"alert_severity_when_this_node_goes_silent":"warn"`
	if status := post(cred, valid+`,"extra":1}`); status != http.StatusBadRequest {
		t.Fatal("unknown field accepted", status)
	}
	if status := post(cred, strings.Replace(valid, `"warn"`, `"loud"`, 1)+`}`); status != http.StatusBadRequest {
		t.Fatal("an invalid silence severity was accepted", status)
	}
	if status := post(cred, valid+`}`); status != http.StatusOK {
		t.Fatal(status)
	}
	// Incidents a node sent without the hub are checked like the rest.
	entryWithSeverity := func(incident, group, check, kind, severity string) string {
		return `,"incidents_sent_without_the_hub":[{"destination":"on_call_pager","incident_id":"` + incident + `","check_group":"` + group + `","check":"` + check + `","type":"` + kind + `","alert_severities_sent_for_incident":[` + severity + `]}]}`
	}
	entry := func(incident, group, check, kind string) string {
		return entryWithSeverity(incident, group, check, kind, `"warn","sos"`)
	}
	invalidDestination := strings.Replace(entry(incidents.ID(), "app", "shell", "failed"), `"destination":"on_call_pager",`, `"destination":"a/b",`, 1)
	for _, bad := range []string{invalidDestination, entry("x", "app", "shell", "failed"), entry(incidents.ID(), "a/b", "shell", "failed"), entry(incidents.ID(), "app", "", "failed"), entry(incidents.ID(), "app", "shell", "recovered"), entryWithSeverity(incidents.ID(), "app", "shell", "failed", ""), entryWithSeverity(incidents.ID(), "app", "shell", "failed", `"sos","sos"`)} {
		if status := post(cred, valid+bad); status != http.StatusBadRequest {
			t.Fatal("invalid incident sent without the hub accepted", bad, status)
		}
	}
	if status := post(cred, strings.Replace(valid, `"report_sequence_number":99`, `"report_sequence_number":100`, 1)+entry(incidents.ID(), checksandalerts.NodeReportsCheckGroup, checksandalerts.HubCheck, "failed")); status != http.StatusOK {
		t.Fatal(status)
	}
	if status := post(cred, `{"daemon_start_id":"`+strings.Repeat("x", fleet.ReportLimit)+`"}`); status != http.StatusBadRequest {
		t.Fatal("oversized report accepted", status)
	}
}

// fleetCommand sends a fleet command to the hub's socket routes.
func fleetCommand(t *testing.T, hub *Daemon, method, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	hub.routes().ServeHTTP(w, httptest.NewRequest(method, "http://localhost"+path, nil))
	return w.Code, w.Body.String()
}

// Rotation replaces the join secret: joined nodes keep reporting with their
// credentials, only new joins need the new code, and the old code joins
// nothing. The new code survives a hub restart.
func TestRotateJoinCode(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	joined := joinedNode(t, code, "app-1")
	joined.report(context.Background())
	credential := hub.hub.Nodes["app-1"].Credential
	status, body := fleetCommand(t, hub, "POST", "/fleet/rotate-join-code")
	var reply struct {
		JoinCode string `json:"join_code"`
	}
	json.Unmarshal([]byte(body), &reply)
	rotated, e := fleet.ParseJoinCode(reply.JoinCode)
	if status != 200 || e != nil || rotated.Secret == code.Secret || rotated.Fingerprint != code.Fingerprint || hub.hub.JoinCode() != rotated {
		t.Fatal("rotation", status, body)
	}
	rotated.Address = code.Address
	joined.report(context.Background())
	os.WriteFile(joined.config.JoinFleetHub.JoinCodeFile, []byte(rotated.String()), 0600)
	if _, e := joined.reload(); e != nil {
		t.Fatal(e)
	}
	joined.report(context.Background())
	if joined.link.LastError != "" || hub.hub.Nodes["app-1"].Credential != credential {
		t.Fatal("a joined node did not keep its credential", joined.link)
	}
	late := joinedNode(t, code, "app-2")
	late.report(context.Background())
	if !strings.Contains(late.link.LastError, "invalid join code") || hub.hub.Nodes["app-2"] != nil {
		t.Fatal("the old code joined", late.link)
	}
	os.WriteFile(late.config.JoinFleetHub.JoinCodeFile, []byte(rotated.String()), 0600)
	if _, e := late.reload(); e != nil {
		t.Fatal(e)
	}
	late.report(context.Background())
	if late.link.LastError != "" || hub.hub.Nodes["app-2"] == nil {
		t.Fatal("the new code did not join", late.link)
	}
	hub.hub.Close()
	reopened, e := fleet.OpenHub(hub.config.StateDir, hub.config.FleetHubRole.AddressForNodes, hub.store.Node, hub.config.FleetHubRole.ReportEvery.Value(), config.DefaultHubLimits, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	hub.hub = reopened
	if reopened.Code.Secret != rotated.Secret {
		t.Fatal("the rotated secret did not survive a hub restart")
	}
}

// A hub restored from its state on another machine keeps its identity. A join
// code naming its new address keeps every node's credential.
func TestRestoredHubKeepsItsServers(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	path := filepath.Join(node.config.StateDir, fleet.CredentialFile)
	cred := fleet.ReadCredential(path)
	hub.Close()
	p, c := daemonConfig(t)
	c.FleetHubRole = &config.FleetHubRole{ListenOnAddress: "0.0.0.0:8445", AddressForNodes: "hub2.test:8445"}
	writeConfig(t, p, c)
	if e := os.CopyFS(c.StateDir, os.DirFS(hub.config.StateDir)); e != nil {
		t.Fatal(e)
	}
	os.Chmod(c.StateDir, 0700)
	restored, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { restored.Close() })
	srv := httptest.NewUnstartedServer(restored.hubRoutes())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{restored.hub.Cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	moved := restored.hub.JoinCode()
	if moved.Fingerprint != code.Fingerprint || moved.Secret != code.Secret || moved.Address != "hub2.test:8445" {
		t.Fatal("the restored hub has another identity", moved)
	}
	moved.Address = srv.Listener.Addr().String()
	os.WriteFile(node.config.JoinFleetHub.JoinCodeFile, []byte(moved.String()), 0600)
	if _, e := node.reload(); e != nil {
		t.Fatal(e)
	}
	node.report(context.Background())
	detail, _ := restored.hub.Node("app-1", time.Now())
	if node.link.LastError != "" || *fleet.ReadCredential(path) != *cred || detail.State != "reporting" || len(detail.Events) != 1 {
		t.Fatal("the node did not keep reporting with its credential", node.link, detail.Events)
	}
}

func newHubDaemon(t *testing.T, destinations map[string]checksandalerts.Destination) *Daemon {
	t.Helper()
	return newHubDaemonWithModules(t, destinations, nil)
}

func newHubDaemonWithModules(t *testing.T, destinations map[string]checksandalerts.Destination, install func(externalModuleDir string)) *Daemon {
	t.Helper()
	return newHubDaemonOnClock(t, destinations, install, time.Now)
}

// newHubDaemonOnClock is newHubDaemonWithModules for a hub whose clock is
// clock.
func newHubDaemonOnClock(t *testing.T, destinations map[string]checksandalerts.Destination, install func(externalModuleDir string), clock func() time.Time) *Daemon {
	t.Helper()
	p, c := daemonConfig(t)
	if install != nil {
		install(c.ExternalModuleDir)
	}
	c.FleetHubRole = &config.FleetHubRole{ListenOnAddress: "0.0.0.0:8445", AddressForNodes: "hub.test:8445"}
	c.Alerts.Destinations = destinations
	writeConfig(t, p, c)
	d, err := newDaemon(p, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	// Tests count the alerts that they study; TestAJoinSendsANotice turns
	// the join notices back on.
	d.withoutJoinNotices = true
	return d
}

func TestRestoredHubUpdatesOwnName(t *testing.T) {
	t.Parallel()
	d := newHubDaemon(t, nil)
	h := d.hub
	at := time.Now().UTC()
	credential, status, err := h.Join("app-1", h.Code.Secret, nodeSettings("warn"), "node", at)
	if status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	r := fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: -1, Sent: at}
	if _, status, err := h.Accept("app-1", &r, at); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	if _, status, err := h.Accept(h.Name, &r, at); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	oldName, code := h.Name, h.JoinCode()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := fleet.OpenHub(d.config.StateDir, "new-hub.test:8445", "new-hub", time.Duration(h.ReportEvery.Load()), config.DefaultHubLimits, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	d.hub = reopened
	if got := reopened.JoinCode(); got.Fingerprint != code.Fingerprint || got.Secret != code.Secret {
		t.Fatal("restored hub lost its identity")
	}
	if _, remains := reopened.Members()[oldName]; remains {
		t.Fatalf("restored hub monitors its own former name %q as a joined node that can never report", oldName)
	}
	member := reopened.Nodes["app-1"]
	if len(reopened.Nodes) != 2 || member == nil || member.NodeStatusReport == nil || member.NodeStatusReport.StartID != r.StartID || reopened.Credentials[fleet.HashCredential(credential)] != "app-1" {
		t.Fatal("restoring with a new hub name changed joined nodes", reopened.Nodes)
	}
	var oldRows int
	if err := reopened.DB.QueryRow("SELECT COUNT(*) FROM nodes WHERE name=?", oldName).Scan(&oldRows); err != nil || oldRows != 0 {
		t.Fatal("former hub entry remains in storage", oldRows, err)
	}
}

// A restart forgets receipt freshness and grants the configured grace, while
// keeping the last report available as history.
func TestHubRestartRequiresFreshReport(t *testing.T) {
	t.Parallel()
	d := newHubDaemon(t, nil)
	setFleetTimings(t, d, config.FleetTimings{ReportEvery: "10s", AlertAfterHubUnreachableFor: "25s", SendAlertWithoutHubAfterWaitingFor: "5s"})
	h := d.hub
	at := time.Now().UTC()
	if _, status, err := h.Join("app-1", h.Code.Secret, nodeSettings("warn"), "node", at); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	r := fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: -1, Sent: at, Summary: incidents.StatusSummary{ExitCode: 0}}
	if _, status, err := h.Accept("app-1", &r, at); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	beforeRestart := time.Now()
	restarted, err := NewDaemon(d.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	member := restarted.hub.Nodes["app-1"]
	deadline := fleetCheck(restarted, "app-1").Deadline
	if !member.Deadline.Equal(deadline) || deadline.After(time.Now().Add(30*time.Second)) || deadline.Before(beforeRestart.Add(30*time.Second)) {
		t.Fatal("restart did not apply the configured report interval", member.Deadline, deadline)
	}
	s, err := restarted.hub.Node("app-1", time.Now())
	if err != nil || s.State != "waiting_for_first_report" || s.NodeStatusReport == nil || !s.LastReport.Equal(at) {
		t.Fatal("restart did not retain the old report as history only", s, err)
	}
	if state := restarted.hub.State(member, deadline); state != "silent" {
		t.Fatal("configured grace did not expire", state)
	}
	r.Seq++
	if _, status, err := restarted.hub.Accept("app-1", &r, time.Now()); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	if state := restarted.hub.State(member, time.Now()); state != "reporting" {
		t.Fatal("fresh report did not restore reporting", state)
	}
}

func TestFleetFailureBeforeUnknown(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	for _, name := range []string{"a", "z"} {
		node := joinedNode(t, code, name)
		node.report(context.Background())
	}
	now := time.Now().UTC()
	for _, reverse := range []bool{false, true} {
		failed, unknown := "a", "z"
		if reverse {
			failed, unknown = unknown, failed
		}
		hub.hub.Mu.Lock()
		hub.hub.Nodes[failed].NodeStatusReport.Summary.ExitCode = 1
		hub.hub.Nodes[unknown].NodeStatusReport.Summary.ExitCode = 3
		hub.hub.Mu.Unlock()
		if fleet := hub.hub.Fleet(now); fleet.ExitCode != 1 {
			t.Fatal(fleet)
		}
	}
}

// The hub keeps the events of all its nodes within one size limit. When
// they pass it, the hub removes the oldest events of the node that has the
// most, only until that node is level with the next one, so a noisy node
// cannot push out the events of the others. Events older than the time
// limit go at the retention tick, forgetting a node frees its share, and
// the counts the hub keeps in memory equal what is stored.
func TestHubEventHistoryLimits(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	h := hub.hub
	now := time.Now().UTC()
	for _, name := range []string{"quiet", "noisy"} {
		if _, status, e := h.Join(name, code.Secret, nodeSettings("warn"), "10.0.0.5", now); status != 200 {
			t.Fatal(status, e)
		}
	}
	// Each event is a little over 1 KiB.
	save := func(name string, n int, at time.Time) string {
		t.Helper()
		events := []checksandalerts.Event{}
		for i := 0; i < n; i++ {
			events = append(events, checksandalerts.Event{ID: incidents.ID(), Type: "failed", AlertSeverity: "warn", At: at, Message: strings.Repeat("x", 1000)})
		}
		h.Mu.Lock()
		defer h.Mu.Unlock()
		if e := h.Save(name, h.Nodes[name], events, h.OutageAlerters, at); e != nil {
			t.Fatal(e)
		}
		return events[n-1].ID
	}
	stored := func() (map[string]int64, map[string]int) {
		t.Helper()
		rows, e := h.DB.Query("SELECT node, SUM(length(data)), COUNT(*) FROM events GROUP BY node")
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		bytes, counts := map[string]int64{}, map[string]int{}
		var total int64
		for rows.Next() {
			var node string
			var size int64
			var count int
			if e = rows.Scan(&node, &size, &count); e != nil {
				t.Fatal(e)
			}
			bytes[node], counts[node], total = size, count, total+size
		}
		h.Mu.Lock()
		defer h.Mu.Unlock()
		if !reflect.DeepEqual(bytes, h.EventBytes) || total != h.EventBytesTotal {
			t.Fatalf("the hub counts %v (%d), and stores %v (%d)", h.EventBytes, h.EventBytesTotal, bytes, total)
		}
		return bytes, counts
	}
	const limit = 100 * 1024
	h.SetLimits(config.HubLimits{KeepEventHistoryOfNodesFor: 7 * 24 * time.Hour, EventHistoryOfNodesBytes: limit, KeepMeasurementHistoryOfNodesFor: "7d", MeasurementHistoryOfNodesSizeLimit: "20GiB", JoinedNodeLimit: config.DefaultJoinedNodeLimit})
	// Joining left one event for each node.
	_, joined := stored()
	save("quiet", 20, now)
	save("noisy", 60, now)
	if _, counts := stored(); counts["quiet"] != joined["quiet"]+20 || counts["noisy"] != joined["noisy"]+60 {
		t.Fatal("events within the limit were removed", counts)
	}
	// The noisy node passes the limit: it loses its own oldest events.
	newest := save("noisy", 60, now)
	bytes, counts := stored()
	if total := bytes["quiet"] + bytes["noisy"]; counts["quiet"] != joined["quiet"]+20 || total > limit || total < limit-2*1100 {
		t.Fatal("the noisy node's surplus was not taken from the noisy node alone", bytes, counts)
	}
	var id string
	if e := h.DB.QueryRow("SELECT json_extract(data,'$.id') FROM events WHERE node='noisy' ORDER BY id DESC LIMIT 1").Scan(&id); e != nil || id != newest {
		t.Fatal("the newest event was not kept", id, e)
	}
	// When the other node grows past it, the two end level.
	save("quiet", 100, now)
	bytes, _ = stored()
	if difference := bytes["quiet"] - bytes["noisy"]; bytes["quiet"]+bytes["noisy"] > limit || difference > 2*1100 || difference < -2*1100 {
		t.Fatal("the nodes are not level after the limit took from the larger one", bytes)
	}
	// Events received longer ago than the time limit go at the retention tick.
	save("quiet", 3, now.Add(-8*24*time.Hour))
	before, _ := stored()
	h.Prune(now)
	after, counts := stored()
	if after["quiet"] >= before["quiet"] || after["noisy"] != before["noisy"] {
		t.Fatal("the retention tick did not remove the old events of one node only", before, after)
	}
	var old int
	if e := h.DB.QueryRow("SELECT COUNT(*) FROM events WHERE at < ?", now.Add(-7*24*time.Hour).UnixNano()).Scan(&old); e != nil || old != 0 {
		t.Fatal(old, e)
	}
	// A hub that starts counts what is stored, and forgetting a node frees its share.
	reopened, e := fleet.OpenHub(hub.config.StateDir, hub.config.FleetHubRole.AddressForNodes, hub.store.Node, hub.config.FleetHubRole.ReportEvery.Value(), config.DefaultHubLimits, now)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(reopened.EventBytes, after) || reopened.EventBytesTotal != after["quiet"]+after["noisy"] {
		t.Fatal("a started hub does not count its stored events", reopened.EventBytes, after)
	}
	reopened.Close()
	if _, status, e := h.Forget("noisy"); status != 200 {
		t.Fatal(status, e)
	}
	if bytes, _ = stored(); len(bytes) != 1 || bytes["quiet"] != after["quiet"] || counts["quiet"] == 0 {
		t.Fatal("forgetting a node did not free its events", bytes)
	}
}

// The hub holds the alerts of nodes that it has accepted and not delivered
// yet up to a size limit. An alert beyond it is refused, so the node
// delivers it without the hub.
func TestHubAlertIntakeSizeLimit(t *testing.T) {
	t.Parallel()
	limits := config.DefaultStorageLimits
	limits.UndeliveredAlertsOfNodesBytes = 8 * 1024
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), limits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	alert := func() checksandalerts.Event {
		return checksandalerts.Event{ID: incidents.ID(), CheckGroup: "web", Check: "http", Type: "failed", AlertSeverity: "warn", At: time.Now().UTC(), Message: strings.Repeat("x", 1000), Notify: true}
	}
	// Each node holds at most a quarter of the limit, so eight nodes with
	// one alert each fill it.
	accepted := 0
	for ; accepted < 20; accepted++ {
		if e = s.Accept(fmt.Sprintf("app-%d", accepted), alert(), time.Now().UTC()); e != nil {
			break
		}
	}
	if !errors.Is(e, store.ErrIntakeFull) || accepted < 5 || accepted > 7 {
		t.Fatal("the intake did not stop at its size limit of 8 KiB", accepted, e)
	}
	// A reload that states a larger limit accepts alerts again.
	limits.UndeliveredAlertsOfNodesBytes = 64 * 1024
	s.SetLimits(limits)
	if e = s.Accept("app-new", alert(), time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
}

// One node's alerts hold at most a quarter of the hub's intake, so a node,
// also a hostile one, cannot fill it for the other nodes. The node's next
// alert is refused, and the node delivers it without the hub.
func TestOneNodeHoldsAQuarterOfTheHubAlertIntake(t *testing.T) {
	t.Parallel()
	limits := config.DefaultStorageLimits
	limits.UndeliveredAlertsOfNodesBytes = 16 * 1024
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), limits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	alert := func() checksandalerts.Event {
		return checksandalerts.Event{ID: incidents.ID(), CheckGroup: "web", Check: "http", Type: "failed", AlertSeverity: "warn", At: time.Now().UTC(), Message: strings.Repeat("x", 1000), Notify: true}
	}
	accepted := 0
	for ; accepted < 20; accepted++ {
		if e = s.Accept("noisy", alert(), time.Now().UTC()); e != nil {
			break
		}
	}
	if !errors.Is(e, store.ErrIntakeFull) || !strings.Contains(e.Error(), "noisy") || accepted < 2 || accepted > 4 {
		t.Fatal("one node held more than a quarter of the intake", accepted, e)
	}
	if e = s.Accept("app-2", alert(), time.Now().UTC()); e != nil {
		t.Fatal("another node was refused while the intake had room", e)
	}
}

// A reload applies the times and sizes of the stores. The two settings that
// size a database file need a daemon restart.
func TestReloadAppliesStorageLimits(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.FleetHubRole = &config.FleetHubRole{ListenOnAddress: "0.0.0.0:8445", AddressForNodes: "hub.test:8445"}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	change := func(change func(c *config.Config)) error {
		t.Helper()
		edited := *c
		role := *c.FleetHubRole
		edited.FleetHubRole = &role
		change(&edited)
		writeConfig(t, p, &edited)
		_, e := d.reload()
		return e
	}
	if e := change(func(c *config.Config) {
		c.KeepEventHistoryFor, c.EventHistoryContentSizeLimit, c.KeepUndeliveredAlertsFor = "7d", "20MiB", "2d"
		c.FleetHubRole.KeepEventHistoryOfNodesFor, c.FleetHubRole.UndeliveredAlertsOfNodesContentSizeLimit = "30d", "16MiB"
	}); e != nil {
		t.Fatal(e)
	}
	limits := d.store.Limits.Load()
	if limits.KeepEventHistoryFor != 7*24*time.Hour || limits.EventHistoryBytes != 20*1024*1024 || limits.KeepUndeliveredAlertsFor != 48*time.Hour || limits.UndeliveredAlertsOfNodesBytes != 16*1024*1024 || d.hub.Limits.Load().KeepEventHistoryOfNodesFor != 30*24*time.Hour {
		t.Fatalf("%+v %+v", limits, d.hub.Limits.Load())
	}
	for _, edit := range []func(c *config.Config){
		func(c *config.Config) { c.StateDatabaseSizeLimit = "1GiB" },
		func(c *config.Config) { c.FleetHubRole.EventHistoryOfNodesContentSizeLimit = "8GiB" },
	} {
		if e := change(edit); e == nil || !strings.Contains(e.Error(), "changing state_database_size_limit or fleet_hub_role.event_history_of_nodes_content_size_limit requires restarting MMWatchdog") {
			t.Fatal(e)
		}
	}
}

// The hub removes the old events of its nodes once a minute, under its own
// lock, and its event history can hold gigabytes. So the statement finds the
// old events through the index of their receipt time and reads no other
// event.
func TestTheHubFindsOldEventsThroughAnIndex(t *testing.T) {
	t.Parallel()
	hub, _ := startHub(t)
	rows, err := hub.hub.DB.Query("EXPLAIN QUERY PLAN "+fleet.HubRemoveEventsReceivedBefore, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := []string{}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if got := strings.Join(plan, "\n"); !strings.HasPrefix(got, "SEARCH events USING ") || !strings.Contains(got, "INDEX events_received (at<?)") || strings.Contains(got, "SCAN") {
		t.Fatal(got)
	}
}

// A hub takes at most joined_node_limit nodes, so a leaked join code cannot
// fill its database with nodes. The refusal says how to raise the limit, and
// a node that has joined can join again.
func TestAHubTakesAtMostItsJoinedNodeLimit(t *testing.T) {
	t.Parallel()
	d := newHubDaemon(t, nil)
	h := d.hub
	limits := *h.Limits.Load()
	limits.JoinedNodeLimit = 2
	h.SetLimits(limits)
	at := time.Now().UTC()
	for _, name := range []string{"app-1", "app-2"} {
		if _, status, err := h.Join(name, h.Code.Secret, nodeSettings("warn"), "192.0.2.1", at); err != nil || status != 200 {
			t.Fatal(name, status, err)
		}
	}
	_, status, err := h.Join("app-3", h.Code.Secret, nodeSettings("warn"), "192.0.2.3", at)
	if status != 403 || err == nil || err.Error() != "the fleet hub has 2 joined nodes, its joined_node_limit; to let more nodes join, raise joined_node_limit under fleet_hub_role in the hub's mmwatchdog_node_settings.yml and reload MMWatchdog on the hub" {
		t.Fatal(status, err)
	}
	if _, status, _ := h.Join("app-1", h.Code.Secret, nodeSettings("warn"), "192.0.2.1", at); status == 403 {
		t.Fatal("a joined node's name was refused for the limit")
	}
}
