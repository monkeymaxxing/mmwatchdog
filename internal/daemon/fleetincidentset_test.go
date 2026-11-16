package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// The tests of one incident per check across the fleet for a destination
// whose service keeps incidents of its own: docs/changing-mmwatchdog/design/fleet.md, "One
// incident per check across the fleet". The MMHotline destination keeps
// the same set; its tests are in mmhotline_daemon_test.go.

// ownPagerDescription describes a module of the project's own for a
// service that keeps incidents of its own, such as a pager.
const ownPagerDescription = `summary: Pages through a service that keeps incidents of its own, in a test.
used_as: alert_destination
service_keeps_incidents: true
examples:
  - description: Page for every alert.
    config: |
      own_pager: {}
      remind_every: never
      alert_severities: [info, warn, sos]
`

// ownChatDescription describes a module of the project's own for a chat,
// which keeps no incidents.
const ownChatDescription = `summary: Posts to a chat, in a test.
used_as: alert_destination
examples:
  - description: Post every alert.
    config: |
      own_chat: {}
      remind_every: never
      alert_severities: [info, warn, sos]
`

// recordingModule is the script of both modules: it adds each request to
// the file requests in its directory, which is the destination's
// run_from_directory, and delivers.
const recordingModule = `cat >> requests
echo >> requests
printf '{"protocol":1,"status":"delivered","message":"recorded"}'`

// installRecordingModules writes the modules own_pager and own_chat into a
// node's external_module_dir.
func installRecordingModules(t *testing.T, externalModuleDir string) {
	t.Helper()
	testhelpers.InstallModule(t, externalModuleDir, "own_pager", ownPagerDescription, recordingModule)
	testhelpers.InstallModule(t, externalModuleDir, "own_chat", ownChatDescription, recordingModule)
}

// recordingDestination delivers through the module, own_pager or own_chat,
// and takes these severities.
func recordingDestination(t *testing.T, module string, severities []string) checksandalerts.Destination {
	t.Helper()
	d := moduleDestination(t, module, map[string]any{})
	d.AlertSeverities = severities
	return d
}

// recordedAlerts are the alerts that the module of the destination got, in
// order. A module may be writing a request while the test reads the file:
// a request counts once the line break after it is written.
func recordedAlerts(t *testing.T, d checksandalerts.Destination) []checksandalerts.Event {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(d.Inputs.(*checksandalerts.ModuleCall).RunFromDirectory, "requests"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		t.Fatal(e)
	}
	alerts := []checksandalerts.Event{}
	lines := strings.Split(string(b), "\n")
	for _, line := range lines[:len(lines)-1] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var request struct {
			Alert checksandalerts.Event `json:"alert"`
		}
		if e = json.Unmarshal([]byte(line), &request); e != nil {
			t.Fatal(e, line)
		}
		alerts = append(alerts, request.Alert)
	}
	return alerts
}

// waitForRecordedAlerts returns the alerts of the destination once its
// module has got n.
func waitForRecordedAlerts(t *testing.T, d checksandalerts.Destination, n int) []checksandalerts.Event {
	t.Helper()
	testhelpers.WaitFor(t, "the module's alerts", func() bool { return len(recordedAlerts(t, d)) >= n })
	return recordedAlerts(t, d)
}

// queuedFor lists the alerts queued for one destination, in order.
func queuedFor(t *testing.T, d *Daemon, destination string) []checksandalerts.Event {
	t.Helper()
	rows, e := d.store.DB.Query("SELECT event FROM outbox WHERE destination=? ORDER BY seq", destination)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	events := []checksandalerts.Event{}
	for rows.Next() {
		var b []byte
		var ev checksandalerts.Event
		if e = rows.Scan(&b); e == nil {
			e = json.Unmarshal(b, &ev)
		}
		if e != nil {
			t.Fatal(e)
		}
		events = append(events, ev)
	}
	return events
}

// typesOf lists the type of each alert.
func typesOf(alerts []checksandalerts.Event) []string {
	out := []string{}
	for _, a := range alerts {
		out = append(out, a.Type)
	}
	return out
}

// appOnNode is the check app/shell of one node, run by the node's own
// incident state: an incident opens at the first failed or unknown run and
// closes at the first ok run, and its alerts carry their fleet incident key
// as every node's do.
type appOnNode struct {
	node string
	st   *incidents.CheckState
}

func newAppOnNode(node, severity string) *appOnNode {
	return &appOnNode{node, &incidents.CheckState{CheckGroup: "app", Check: "shell", Timing: checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: severity}}}
}

// alert is the one alert of events.
func (a *appOnNode) alert(t *testing.T, events []checksandalerts.Event) checksandalerts.Event {
	t.Helper()
	notifying := []checksandalerts.Event{}
	for _, e := range events {
		if e.Notify {
			e.Node = a.node
			notifying = append(notifying, e)
		}
	}
	if len(notifying) != 1 {
		t.Fatalf("expected one alert of %s: %+v", a.node, events)
	}
	return notifying[0]
}

func (a *appOnNode) observe(t *testing.T, status string, at time.Time) checksandalerts.Event {
	t.Helper()
	return a.alert(t, a.st.Observe(checksandalerts.Observation{Status: status, Message: status + " on " + a.node, At: at}, a.st.Timing, false))
}

func (a *appOnNode) raise(t *testing.T, at time.Time) checksandalerts.Event {
	t.Helper()
	return a.alert(t, a.st.Raise(&a.st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", "the test raised it", at, false))
}

// withFleetIncidentKey gives an alert about an incident, written by hand
// in a test, the key that a node's own state gives it, as the hub requires.
func withFleetIncidentKey(e checksandalerts.Event) checksandalerts.Event {
	e.FleetIncidentKey = checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(e, checksandalerts.IncidentKind(e.Type), ""))
	return e
}

// appFailedKey is the fleet_incident_key of every alert about the failure
// incident of app/shell, on every node.
func incidentKeyFor(d *Daemon, readable string) string {
	return checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(readable), d.store.Node, d.store.FleetIdentity)
}

// Two nodes and more fail one check: a destination whose service keeps
// incidents gets each failure, the grouped one too, and the raise, all with
// the same key, and no recovery until the last node recovers. Then it gets
// one closing alert for the key, with the severities it got and the nodes
// that failed. Its module gets the key in each request. A destination
// whose module keeps no incidents still gets every recovery.
func TestOneClosingAlertWhenTheLastNodeRecovers(t *testing.T) {
	t.Parallel()
	pager, chat := recordingDestination(t, "own_pager", testhelpers.AllSeverities), recordingDestination(t, "own_chat", testhelpers.AllSeverities)
	hub, _, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"on_call_pager": pager, "team_chat": chat}, func(dir string) { installRecordingModules(t, dir) })
	now := time.Now().UTC()
	app1, app2, app3 := newAppOnNode("app-1", "warn"), newAppOnNode("app-2", "warn"), newAppOnNode("app-3", "warn")
	acceptAlert(t, hub, "app-1", app1.observe(t, "failed", now))
	groupAt(t, hub, now)
	acceptAlert(t, hub, "app-2", app2.observe(t, "failed", now))
	acceptAlert(t, hub, "app-3", app3.observe(t, "failed", now))
	groupAt(t, hub, now.Add(time.Second))
	groupAt(t, hub, now.Add(2*time.Minute))
	acceptAlert(t, hub, "app-3", app3.raise(t, now.Add(3*time.Minute)))
	groupAt(t, hub, now.Add(4*time.Minute))
	queued := queuedFor(t, hub, "on_call_pager")
	if got := typesOf(queued); !reflect.DeepEqual(got, []string{"failed", "failed", checksandalerts.FailureIncidentEvents.Raised}) || queued[1].NodeCount != 2 || queued[1].Title != "app/shell failed on 2 nodes" || queued[2].AlertSeverity != "sos" {
		t.Fatalf("the failures: %+v", queued)
	}
	for _, alert := range queued {
		if alert.FleetIncidentKey != incidentKeyFor(hub, "app/shell failed") {
			t.Fatalf("an alert without the key of app/shell: %+v", alert)
		}
	}
	// The first two recoveries close their incidents and send nothing.
	acceptAlert(t, hub, "app-1", app1.observe(t, "ok", now.Add(5*time.Minute)))
	groupAt(t, hub, now.Add(6*time.Minute))
	if n := len(queuedFor(t, hub, "on_call_pager")); n != 3 {
		t.Fatal("a recovery reached the pager while other nodes still fail", n)
	}
	acceptAlert(t, hub, "app-2", app2.observe(t, "ok", now.Add(7*time.Minute)))
	acceptAlert(t, hub, "app-3", app3.observe(t, "ok", now.Add(7*time.Minute)))
	groupAt(t, hub, now.Add(8*time.Minute))
	queued = queuedFor(t, hub, "on_call_pager")
	if len(queued) != 4 {
		t.Fatalf("expected one closing alert: %+v", typesOf(queued))
	}
	closing := queued[3]
	if closing.Type != "recovered" || closing.FleetIncidentKey != incidentKeyFor(hub, "app/shell failed") || closing.Incident != "" || closing.AlertSeverity != "sos" || !reflect.DeepEqual(closing.AlertSeveritiesSentForIncident, []string{"warn", "sos"}) ||
		!reflect.DeepEqual(closing.Nodes, []string{"app-1", "app-2", "app-3"}) || closing.NodeCount != 3 || closing.CheckGroup != "app" || closing.Check != "shell" ||
		closing.Title != "app/shell is ok on every node again" || closing.TitleWithAlertSeverity != "Resolved SOS: app/shell is ok on every node again" ||
		closing.Message != "app/shell is ok on every node again (was failing on app-1, app-2, app-3)" || !strings.HasPrefix(closing.Body, closing.Message+"\nlatest: ok on app-3") {
		t.Fatalf("the closing alert: %+v", closing)
	}
	// The chat keeps no incidents: it gets every recovery as it is.
	if got := typesOf(queuedFor(t, hub, "team_chat")); !reflect.DeepEqual(got, []string{"failed", "failed", checksandalerts.FailureIncidentEvents.Raised, "recovered", "recovered", "recovered"}) {
		t.Fatal("the chat's alerts", got)
	}
	// The modules get what was queued, the key in each request.
	runWorker(t, hub)
	for _, alert := range waitForRecordedAlerts(t, pager, 4) {
		if alert.FleetIncidentKey != incidentKeyFor(hub, "app/shell failed") {
			t.Fatalf("the module got an alert without the key: %+v", alert)
		}
	}
	waitForRecordedAlerts(t, chat, 6)
	testhelpers.WaitFor(t, "an empty queue", func() bool { return len(queuedEvents(t, hub)) == 0 })
	if n := len(recordedAlerts(t, pager)); n != 4 {
		t.Fatal("the pager got more than was queued", n)
	}
}

// A standalone node's set holds its own incidents only, so its own
// recovery is the closing alert, with the key, as it is. The event history
// keeps the key, so mmwatchdog events --json shows it, and an alert without an
// incident has none.
func TestAStandaloneNodesOwnRecoveryClosesTheIncident(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	installRecordingModules(t, c.ExternalModuleDir)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"on_call_pager": recordingDestination(t, "own_pager", testhelpers.AllSeverities)}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	app := newAppOnNode("", "warn")
	now := time.Now().UTC()
	failed := app.observe(t, "failed", now)
	window := checksandalerts.Window{Until: now.Add(time.Hour), Reason: "deploy"}
	recordEvents(t, d, failed, incidents.MaintenanceEvent("maintenance_on", []string{"other"}, window, now, "maintenance "+window.Describe()))
	recovered := app.observe(t, "ok", now.Add(time.Minute))
	recordEvents(t, d, recovered)
	queued := queuedFor(t, d, "on_call_pager")
	if got := typesOf(queued); !reflect.DeepEqual(got, []string{"failed", "maintenance_on", "recovered"}) || queued[2].ID != recovered.ID || queued[2].Title != d.store.Node+": app/shell is ok again" ||
		queued[0].FleetIncidentKey != incidentKeyFor(d, "app/shell failed") || queued[2].FleetIncidentKey != incidentKeyFor(d, "app/shell failed") || queued[1].FleetIncidentKey != "" || !reflect.DeepEqual(queued[2].AlertSeveritiesSentForIncident, []string{"warn"}) {
		t.Fatalf("%+v", queued)
	}
	stored, e := d.store.Events(10)
	if e != nil {
		t.Fatal(e)
	}
	keys := map[string]string{}
	for _, event := range stored {
		keys[event.Type] = event.FleetIncidentKey
	}
	if keys["failed"] != incidentKeyFor(d, "app/shell failed") || keys["recovered"] != incidentKeyFor(d, "app/shell failed") || keys["maintenance_on"] != "" {
		t.Fatal("the event history's keys", keys)
	}
}

// Every silent node's alert on the hub carries one key, and the hub-outage alerter
// node's alert about the hub has its own.
func TestFleetIncidentKeysOfSilenceAndOfTheOutageAlerter(t *testing.T) {
	t.Parallel()
	hub, _ := startHub(t)
	outageAlerter, _, _ := severityDaemon(t, nil)
	now := time.Now().UTC()
	silent := func(node string) checksandalerts.Event {
		st := &incidents.CheckState{CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: node, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "sos"}, Timing: checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}}
		return (&appOnNode{"", st}).observe(t, "failed", now)
	}
	recordEvents(t, hub, silent("app-5"), silent("app-6"))
	recordEvents(t, outageAlerter, silent(checksandalerts.HubCheck))
	for _, d := range []*Daemon{hub, outageAlerter} {
		stored, e := d.store.Events(10)
		if e != nil {
			t.Fatal(e)
		}
		want := incidentKeyFor(d, checksandalerts.SilenceIncidentKey)
		if d == outageAlerter {
			want = incidentKeyFor(d, "node_reports/to_hub failed")
		}
		for _, event := range stored {
			if event.Type == "failed" && event.FleetIncidentKey != want {
				t.Fatalf("%s: %+v", event.AboutCheck(), event)
			}
		}
	}
}

// A joined node that delivers without the hub sends the failure to a
// destination that keeps incidents, with the key, and never its recovery,
// which goes only to a destination that keeps none. Its reports carry the
// incident until the hub holds it, and the hub, back, sends the one
// closing alert. The node learns whether the destination keeps incidents
// from its own module, which it installed after it got the fleet settings.
func TestAJoinedNodeLeavesTheClosingAlertToTheHub(t *testing.T) {
	t.Parallel()
	pager, chat := recordingDestination(t, "own_pager", testhelpers.AllSeverities), recordingDestination(t, "own_chat", testhelpers.AllSeverities)
	hub, code, srv := startHubWithModules(t, map[string]checksandalerts.Destination{"on_call_pager": pager, "team_chat": chat}, func(dir string) { installRecordingModules(t, dir) })
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	installRecordingModules(t, node.config.ExternalModuleDir)
	if _, e := node.reload(); e != nil {
		t.Fatal(e)
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms"
	srv.Close()
	app := newAppOnNode("app-1", "warn")
	failed := app.observe(t, "failed", time.Now().UTC())
	recordEvents(t, node, failed, app.observe(t, "ok", time.Now().UTC()))
	runWorker(t, node)
	testhelpers.WaitFor(t, "the alerts to leave the node's queues", func() bool { return len(queuedEvents(t, node)) == 0 })
	if got := recordedAlerts(t, pager); len(got) != 1 || got[0].ID != failed.ID || got[0].FleetIncidentKey != incidentKeyFor(hub, "app/shell failed") {
		t.Fatalf("expected the failure alone: %+v", got)
	}
	if got := typesOf(waitForRecordedAlerts(t, chat, 2)); !reflect.DeepEqual(got, []string{"failed", "recovered"}) {
		t.Fatal("the chat's alerts", got)
	}
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 1 || sent[0].Destination != "on_call_pager" || sent[0].Incident != failed.Incident {
		t.Fatal("the node does not carry the incident it sent to the pager", sent)
	}
	// The hub is back at another address.
	back := httptest.NewUnstartedServer(hub.hubRoutes())
	back.TLS = &tls.Config{Certificates: []tls.Certificate{hub.hub.Cert}}
	back.StartTLS()
	t.Cleanup(back.Close)
	moved := code
	moved.Address = back.Listener.Addr().String()
	node.mu.Lock()
	node.setJoinCode(&moved)
	node.mu.Unlock()
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	queued := queuedFor(t, hub, "on_call_pager")
	if len(queued) != 1 || queued[0].Type != "recovered" || queued[0].FleetIncidentKey != incidentKeyFor(hub, "app/shell failed") || !reflect.DeepEqual(queued[0].Nodes, []string{"app-1"}) ||
		queued[0].Title != "app-1: app/shell is ok again" || queued[0].Message != "app-1: app/shell is ok again" || queued[0].AlertSeverity != "warn" {
		t.Fatalf("the hub's closing alert: %+v", queued)
	}
	if n := len(queuedFor(t, hub, "team_chat")); n != 0 {
		t.Fatal("the chat got the closing alert of a set it does not keep", n)
	}
	node.report(context.Background())
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 0 {
		t.Fatal("the hub's reply did not let the node forget the incident", sent)
	}
}

// Forgetting a node retires its incidents in the set, also when the hub
// restarts before it applied the forget, and the closing alert says so.
func TestForgetRetiresTheIncidentOfADestinationThatKeepsIncidents(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		pager := recordingDestination(t, "own_pager", testhelpers.AllSeverities)
		hub, code, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"on_call_pager": pager}, func(dir string) { installRecordingModules(t, dir) })
		node := joinedNode(t, code, "app-2")
		incident := incidents.ID()
		node.mu.Lock()
		node.snapshot.Checks["app/shell"].FailureIncident = incidents.Track{Incident: incident, Announced: incident, Opened: time.Now(), AlertSeverity: "warn", AnnouncedAlertSeverities: []string{"warn"}}
		node.mu.Unlock()
		node.report(context.Background())
		hub.reconcileReports(time.Now())
		if status, body := fleetCommand(t, hub, "POST", "/fleet/forget/app-2"); status != 200 {
			t.Fatal(status, body)
		}
		if restart {
			hub.hub.TakeReported() // the forget was not applied before the hub stopped
			hub.Close()
			restarted, e := NewDaemon(hub.path)
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { restarted.Close() })
			hub = restarted
		}
		hub.reconcileReports(time.Now())
		queued := queuedFor(t, hub, "on_call_pager")
		if len(queued) != 1 || queued[0].Type != "closed_without_recovery" || queued[0].FleetIncidentKey != incidentKeyFor(hub, "app/shell failed") || queued[0].Title != "app-2: app/shell incident closed, maybe not fixed" ||
			queued[0].TitleWithAlertSeverity != "Closed warning: app-2: app/shell incident closed, maybe not fixed" {
			t.Fatalf("restart %v: %+v", restart, queued)
		}
	}
}

// The set lives in state.db: a hub that restarts between two recoveries
// still sends the one closing alert at the last. A cannot-check key closes
// with can_check_again.
func TestTheIncidentSetSurvivesAHubRestart(t *testing.T) {
	t.Parallel()
	pager := recordingDestination(t, "own_pager", testhelpers.AllSeverities)
	hub, _, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"on_call_pager": pager}, func(dir string) { installRecordingModules(t, dir) })
	now := time.Now().UTC()
	app2, app3 := newAppOnNode("app-2", "warn"), newAppOnNode("app-3", "warn")
	acceptAlert(t, hub, "app-2", app2.observe(t, "unknown", now))
	groupAt(t, hub, now)
	acceptAlert(t, hub, "app-3", app3.observe(t, "unknown", now))
	groupAt(t, hub, now.Add(2*time.Minute))
	acceptAlert(t, hub, "app-2", app2.observe(t, "ok", now.Add(3*time.Minute)))
	groupAt(t, hub, now.Add(4*time.Minute))
	hub.Close()
	restarted, e := NewDaemon(hub.path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { restarted.Close() })
	acceptAlert(t, restarted, "app-3", app3.observe(t, "ok", now.Add(5*time.Minute)))
	groupAt(t, restarted, now.Add(6*time.Minute))
	queued := queuedFor(t, restarted, "on_call_pager")
	key := incidentKeyFor(hub, "app/shell cannot check")
	if got := typesOf(queued); !reflect.DeepEqual(got, []string{"cannot_check", "cannot_check", "can_check_again"}) || queued[0].FleetIncidentKey != key || queued[2].FleetIncidentKey != key ||
		queued[2].Title != "MMWatchdog can check app/shell on every node again" || queued[2].Message != "MMWatchdog can check app/shell on every node again (could not be checked on app-2, app-3)" {
		t.Fatalf("%+v", queued)
	}
}

// The incident of a check that names only another destination with
// send_alerts_only_to_destinations stays out of the pager's set: it holds
// the key of the same check on other nodes not open, and its end sends the
// pager nothing.
func TestTheSetTakesOnlyIncidentsWhoseAlertsGoToItsDestination(t *testing.T) {
	t.Parallel()
	pager, ops := recordingDestination(t, "own_pager", testhelpers.AllSeverities), recordingDestination(t, "own_chat", []string{})
	hub, code, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"on_call_pager": pager, "ops": ops}, func(dir string) { installRecordingModules(t, dir) })
	node := joinedNode(t, code, "app-2")
	setTrack := func(id string) {
		node.mu.Lock()
		defer node.mu.Unlock()
		st := node.snapshot.Checks["app/shell"]
		st.SendAlertsOnlyToDestinations = &[]string{"ops"}
		st.FailureIncident = incidents.Track{Incident: id, Announced: id, Opened: time.Now(), AlertSeverity: "warn", AnnouncedAlertSeverities: []string{"warn"}}
	}
	setTrack(incidents.ID())
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	now := time.Now().UTC()
	app3 := newAppOnNode("app-3", "warn")
	acceptAlert(t, hub, "app-3", app3.observe(t, "failed", now))
	groupAt(t, hub, now)
	recovered := app3.observe(t, "ok", now)
	acceptAlert(t, hub, "app-3", recovered)
	groupAt(t, hub, now.Add(2*time.Minute))
	if queued := queuedFor(t, hub, "on_call_pager"); len(queued) != 2 || queued[1].ID != recovered.ID {
		t.Fatalf("the incident of a check that names only ops held the key open: %+v", queued)
	}
	setTrack("")
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	if n := len(queuedFor(t, hub, "on_call_pager")); n != 2 {
		t.Fatal("the pager got the end of an incident that it never got", n)
	}
}

// Two destinations of one module that take different severities each have
// their own set, and the closing alert that each gets carries the
// severities that it got. app-a fails at warn, which only the first gets,
// and is raised to sos, which only the second gets; app-b fails at sos.
// app-a's report, which shows both severities, adds to each set only the
// severity that its destination takes. When app-a recovers, the first gets
// app-a's own closing alert with its own severities alone, so a module that
// shares one credential of a service between the two destinations cancels
// nothing that the second opened; the second waits for app-b.
func TestEachDestinationClosesWithTheSeveritiesItGot(t *testing.T) {
	t.Parallel()
	warnings, emergencies := recordingDestination(t, "own_pager", []string{"warn"}), recordingDestination(t, "own_pager", []string{"sos"})
	hub, _, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"warnings": warnings, "emergencies": emergencies}, func(dir string) { installRecordingModules(t, dir) })
	now := time.Now().UTC()
	appA, appB := newAppOnNode("app-a", "warn"), newAppOnNode("app-b", "sos")
	acceptAlert(t, hub, "app-a", appA.observe(t, "failed", now))
	acceptAlert(t, hub, "app-b", appB.observe(t, "failed", now))
	groupAt(t, hub, now)
	acceptAlert(t, hub, "app-a", appA.raise(t, now.Add(time.Minute)))
	groupAt(t, hub, now.Add(2*time.Minute))
	id := appA.st.FailureIncident.Incident
	reconcileAt(t, hub, "app-a", store.ReportedIncidents{Sent: now.Add(150 * time.Second), Incidents: map[string]checksandalerts.Event{id: {Node: "app-a", CheckGroup: "app", Check: "shell", Type: "failed", Incident: id, AlertSeverity: "sos", AlertSeveritiesSentForIncident: []string{"warn", "sos"}, At: now}}})
	recoveredA := appA.observe(t, "ok", now.Add(3*time.Minute))
	acceptAlert(t, hub, "app-a", recoveredA)
	groupAt(t, hub, now.Add(4*time.Minute))
	got := queuedFor(t, hub, "warnings")
	if len(got) != 2 || got[1].ID != recoveredA.ID || got[1].AlertSeverity != "warn" || !reflect.DeepEqual(got[1].AlertSeveritiesSentForIncident, []string{"warn"}) ||
		got[1].TitleWithAlertSeverity != "Resolved warning: app-a: app/shell is ok again" {
		t.Fatalf("the first destination: %+v", got)
	}
	if got := typesOf(queuedFor(t, hub, "emergencies")); !reflect.DeepEqual(got, []string{"failed", checksandalerts.FailureIncidentEvents.Raised}) {
		t.Fatal("the second destination closed while app-b still fails", got)
	}
	acceptAlert(t, hub, "app-b", appB.observe(t, "ok", now.Add(5*time.Minute)))
	groupAt(t, hub, now.Add(6*time.Minute))
	if got := queuedFor(t, hub, "warnings"); len(got) != 2 {
		t.Fatal("the first destination got the end of an incident that it never got", typesOf(got))
	}
	got = queuedFor(t, hub, "emergencies")
	if len(got) != 3 || got[2].Type != "recovered" || got[2].AlertSeverity != "sos" || !reflect.DeepEqual(got[2].AlertSeveritiesSentForIncident, []string{"sos"}) || !reflect.DeepEqual(got[2].Nodes, []string{"app-a", "app-b"}) {
		t.Fatalf("the second destination: %+v", got)
	}
}

// A set belongs to its destination while the destination keeps incidents:
// a destination that is gone, or whose module keeps none, loses its set.
func TestADestinationThatNoLongerKeepsIncidentsLosesItsSet(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	installRecordingModules(t, c.ExternalModuleDir)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"pager_a": recordingDestination(t, "own_pager", testhelpers.AllSeverities), "pager_b": recordingDestination(t, "own_pager", testhelpers.AllSeverities), "chat": recordingDestination(t, "own_chat", testhelpers.AllSeverities)}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	recordEvents(t, d, newAppOnNode("", "warn").observe(t, "failed", time.Now().UTC()))
	rows := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, name := range []string{"pager_a", "pager_b", "chat"} {
			var n int
			if e := d.store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM fleet_incident_set WHERE destination=?) + (SELECT COUNT(*) FROM fleet_incident_keys WHERE destination=?)", name, name).Scan(&n); e != nil {
				t.Fatal(e)
			}
			out[name] = n
		}
		return out
	}
	if got := rows(); !reflect.DeepEqual(got, map[string]int{"pager_a": 2, "pager_b": 2, "chat": 0}) {
		t.Fatal("the sets", got)
	}
	configured := d.config.Alerts.Destinations
	commitWith := func(destinations map[string]checksandalerts.Destination) {
		t.Helper()
		setDestinations(d, destinations)
		recordEvents(t, d)
	}
	commitWith(map[string]checksandalerts.Destination{"pager_b": configured["pager_b"], "chat": configured["chat"]})
	if got := rows(); !reflect.DeepEqual(got, map[string]int{"pager_a": 0, "pager_b": 2, "chat": 0}) {
		t.Fatal("the set of a destination that is gone", got)
	}
	commitWith(map[string]checksandalerts.Destination{"pager_b": configured["chat"]})
	if got := rows(); !reflect.DeepEqual(got, map[string]int{"pager_a": 0, "pager_b": 0, "chat": 0}) {
		t.Fatal("the set of a destination whose module keeps no incidents", got)
	}
}

// mmhotlineQueuedFor lists the MMHotline alerts queued for the mmhotline
// destination, beside a destination of another module.
func mmhotlineQueuedFor(t *testing.T, d *Daemon) []testhelpers.MMHotlineAlert {
	t.Helper()
	alerts := []testhelpers.MMHotlineAlert{}
	for _, e := range queuedFor(t, d, mmhotlineDestinationName) {
		a, err := testhelpers.DecodeMMHotlineAlert(e.MMHotlineAlert.Encode())
		if err != nil {
			t.Fatal(err)
		}
		alerts = append(alerts, a)
	}
	return alerts
}

// reconcileAt applies one node's report to the hub's fleet incident sets.
func reconcileAt(t *testing.T, hub *Daemon, reporter string, r store.ReportedIncidents) {
	t.Helper()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if e := hub.store.ReconcileIncidentSets(map[string]store.ReportedIncidents{reporter: r}, hub.config.Alerts.Destinations, false, time.Now()); e != nil {
		t.Fatal(e)
	}
}

// An incident that only a report shows joins each set with only the
// severities that the set's destination takes, so each closing alert lists
// what its destination got.
func TestAReportAddsOnlyTheSeveritiesADestinationTakes(t *testing.T) {
	t.Parallel()
	warnings, emergencies := recordingDestination(t, "own_pager", []string{"warn"}), recordingDestination(t, "own_pager", []string{"sos"})
	hub, _, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"warnings": warnings, "emergencies": emergencies}, func(dir string) { installRecordingModules(t, dir) })
	now := time.Now().UTC()
	id := incidents.ID()
	shown := map[string]checksandalerts.Event{id: {Node: "app-a", CheckGroup: "app", Check: "shell", Type: "failed", Incident: id, AlertSeverity: "sos", AlertSeveritiesSentForIncident: []string{"warn", "sos"}, At: now}}
	reconcileAt(t, hub, "app-a", store.ReportedIncidents{Sent: now.Add(time.Second), Incidents: shown})
	reconcileAt(t, hub, "app-a", store.ReportedIncidents{Sent: now.Add(time.Minute), Incidents: map[string]checksandalerts.Event{}})
	for name, want := range map[string][]string{"warnings": {"warn"}, "emergencies": {"sos"}} {
		got := queuedFor(t, hub, name)
		if len(got) != 1 || got[0].Type != "recovered" || !reflect.DeepEqual(got[0].AlertSeveritiesSentForIncident, want) || got[0].AlertSeverity != want[0] {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

// A reminder that waits in fleet alert grouping while the key closes does
// not go out after the closing alert, which would open the service's
// incident again with nothing to close it.
func TestAReminderAfterTheClosingAlertIsNotSent(t *testing.T) {
	t.Parallel()
	pager := recordingDestination(t, "own_pager", testhelpers.AllSeverities)
	hub, _, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"on_call_pager": pager}, func(dir string) { installRecordingModules(t, dir) })
	now := time.Now().UTC()
	app2, app3 := newAppOnNode("app-2", "warn"), newAppOnNode("app-3", "warn")
	acceptAlert(t, hub, "app-2", app2.observe(t, "failed", now))
	acceptAlert(t, hub, "app-3", app3.observe(t, "failed", now))
	groupAt(t, hub, now)
	reminding := map[string]checksandalerts.Destination{"on_call_pager": {AlertSeverities: testhelpers.AllSeverities, RemindEvery: "1h"}}
	acceptAlert(t, hub, "app-2", app2.st.Reminders(reminding, now.Add(time.Hour))[0])
	groupAt(t, hub, now.Add(time.Hour))
	acceptAlert(t, hub, "app-3", app3.st.Reminders(reminding, now.Add(time.Hour+10*time.Second))[0])
	groupAt(t, hub, now.Add(time.Hour+10*time.Second)) // app-3's reminder waits in the window
	acceptAlert(t, hub, "app-2", app2.observe(t, "ok", now.Add(time.Hour+15*time.Second)))
	acceptAlert(t, hub, "app-3", app3.observe(t, "ok", now.Add(time.Hour+15*time.Second))) // waits behind app-3's reminder
	groupAt(t, hub, now.Add(time.Hour+20*time.Second))
	// app-3's report after its recovery no longer shows the incident, and
	// the key closes.
	reconcileAt(t, hub, "app-3", store.ReportedIncidents{Sent: now.Add(time.Hour + 25*time.Second), Incidents: map[string]checksandalerts.Event{}})
	groupAt(t, hub, now.Add(time.Hour+2*time.Minute))
	groupAt(t, hub, now.Add(time.Hour+4*time.Minute))
	if waiting, e := hub.store.Intake(); e != nil || len(waiting) != 0 {
		t.Fatal("alerts still wait in fleet alert grouping", waiting, e)
	}
	// The failures of both nodes went out as one message, then app-2's
	// reminder, then the one closing alert; app-3's reminder did not.
	if got := typesOf(queuedFor(t, hub, "on_call_pager")); !reflect.DeepEqual(got, []string{"failed", "reminder", "recovered"}) {
		t.Fatal("the pager's alerts", got)
	}
}

// While some joined node has not reported since the hub started, no key
// closes, neither through a recovery alert nor through a report: that node
// may have failed and sent its failure without the hub. The MMHotline
// destination waits too. Once every node has reported, the key closes as
// usual.
func TestNoKeyClosesBeforeEveryNodeHasReportedAfterAHubStart(t *testing.T) {
	t.Parallel()
	destinations := mmhotlineDestinations(t)
	destinations["on_call_pager"] = recordingDestination(t, "own_pager", testhelpers.AllSeverities)
	hub, code, _ := startHubWithModules(t, destinations, func(dir string) { installRecordingModules(t, dir) })
	a, b := joinedNode(t, code, "app-a"), joinedNode(t, code, "app-b")
	a.report(context.Background())
	b.report(context.Background())
	now := time.Now().UTC()
	appA := newAppOnNode("app-a", "warn")
	acceptAlert(t, hub, "app-a", appA.observe(t, "failed", now))
	groupAt(t, hub, now)
	// The hub stops. app-b fails and sends its failure without the hub.
	incidentB := incidents.ID()
	for _, name := range []string{"on_call_pager", mmhotlineDestinationName} {
		if _, e := b.store.DB.Exec("INSERT INTO incidents_sent_without_the_hub(destination,incident_id,check_group,check_name,finding,kind,alert_severities_sent_for_incident,at) VALUES(?,?,?,?,?,?,?,?)", name, incidentB, "app", "shell", "", "failed", "warn", time.Now().UnixNano()); e != nil {
			t.Fatal(e)
		}
	}
	setTrack := func(id string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.snapshot.Checks["app/shell"].FailureIncident = incidents.Track{Incident: id, Announced: id, Opened: time.Now(), AlertSeverity: "warn", AnnouncedAlertSeverities: []string{"warn"}}
	}
	setTrack(incidentB)
	hub.Close()
	restarted, e := NewDaemon(hub.path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { restarted.Close() })
	back := httptest.NewUnstartedServer(restarted.hubRoutes())
	back.TLS = &tls.Config{Certificates: []tls.Certificate{restarted.hub.Cert}}
	back.StartTLS()
	t.Cleanup(back.Close)
	moved := code
	moved.Address = back.Listener.Addr().String()
	for _, node := range []*Daemon{a, b} {
		node.mu.Lock()
		node.setJoinCode(&moved)
		node.mu.Unlock()
	}
	// app-a recovers, and its alert and its report reach the hub before
	// app-b has reported.
	acceptAlert(t, restarted, "app-a", appA.observe(t, "ok", now.Add(time.Minute)))
	groupAt(t, restarted, now.Add(2*time.Minute))
	a.report(context.Background())
	restarted.reconcileReports(time.Now())
	b.report(context.Background())
	restarted.reconcileReports(time.Now())
	// Only app-a's failure, queued before the hub stopped, waits.
	if pager, mmhotline := queuedFor(t, restarted, "on_call_pager"), mmhotlineQueuedFor(t, restarted); len(pager) != 1 || len(mmhotline) != 1 {
		t.Fatalf("a key closed while app-b still fails: %v %v", typesOf(pager), titles(mmhotline))
	}
	setTrack("")
	b.report(context.Background())
	restarted.reconcileReports(time.Now())
	pager, mmhotline := queuedFor(t, restarted, "on_call_pager"), mmhotlineQueuedFor(t, restarted)
	if len(pager) != 2 || pager[1].Message != "app/shell is ok on every node again (was failing on app-a, app-b)" || len(mmhotline) != 2 || mmhotline[1]["title"] != "app/shell is ok on every node again" {
		t.Fatalf("%+v %v", pager, titles(mmhotline))
	}
}

// A node whose reports did not reach a running hub, while its failure went
// to the destinations without the hub, can report after the key closed. The
// closing alert closed what its failure opened in the service, so the hub
// sends that failure again, built from the report, to the pager and to
// MMHotline, and closes the key again when the node recovers.
func TestAFailureGoesAgainWhenTheKeyClosedWhileTheNodeStillFailed(t *testing.T) {
	t.Parallel()
	destinations := mmhotlineDestinations(t)
	destinations["on_call_pager"] = recordingDestination(t, "own_pager", testhelpers.AllSeverities)
	hub, code, _ := startHubWithModules(t, destinations, func(dir string) { installRecordingModules(t, dir) })
	a, b := joinedNode(t, code, "app-a"), joinedNode(t, code, "app-b")
	a.report(context.Background())
	b.report(context.Background())
	now := time.Now().UTC()
	appA := newAppOnNode("app-a", "warn")
	acceptAlert(t, hub, "app-a", appA.observe(t, "failed", now))
	groupAt(t, hub, now)
	// app-b fails, but neither its alert nor its report reaches the hub:
	// its failure goes to the destinations without the hub.
	incidentB, openedB := incidents.ID(), time.Now().UTC()
	setTrack := func(id string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		st := b.snapshot.Checks["app/shell"]
		st.FailureIncident = incidents.Track{Incident: id, Announced: id, Opened: openedB, AlertSeverity: "warn", AnnouncedAlertSeverities: []string{"warn"}}
		st.Last = checksandalerts.Observation{Status: "failed", Message: "exit 1", At: openedB}
	}
	setTrack(incidentB)
	acceptAlert(t, hub, "app-a", appA.observe(t, "ok", now.Add(time.Minute)))
	groupAt(t, hub, now.Add(2*time.Minute))
	if got := typesOf(queuedFor(t, hub, "on_call_pager")); !reflect.DeepEqual(got, []string{"failed", "recovered"}) {
		t.Fatal("setup: the key closes with app-a's recovery", got)
	}
	b.report(context.Background())
	hub.reconcileReports(time.Now())
	pager := queuedFor(t, hub, "on_call_pager")
	if len(pager) != 3 {
		t.Fatalf("app-b's failure did not go to the pager again: %v", typesOf(pager))
	}
	again := pager[2]
	if again.Type != "failed" || again.Incident != incidentB || again.Node != "app-b" || again.FleetIncidentKey != incidentKeyFor(hub, "app/shell failed") || again.Title != "app-b: app/shell failed" ||
		!again.At.Equal(openedB) || again.AlertSeverity != "warn" || !strings.HasPrefix(again.Body, "exit 1") {
		t.Fatalf("the failure sent again: %+v", again)
	}
	if got := mmhotlineQueuedFor(t, hub); len(got) != 3 || got[2]["title"] != "app-b: app/shell failed" || got[2]["sender_incident_key"] != incidentKeyFor(hub, "app/shell failed") || got[2]["severity"] != "warn" {
		t.Fatalf("MMHotline: %v", titles(got))
	}
	setTrack("")
	b.report(context.Background())
	hub.reconcileReports(time.Now())
	pager = queuedFor(t, hub, "on_call_pager")
	if len(pager) != 4 || pager[3].Type != "recovered" || !reflect.DeepEqual(pager[3].Nodes, []string{"app-b"}) {
		t.Fatalf("the key did not close again: %+v", pager)
	}
}

// A joined node that sends a failure and then its raise without the hub
// carries both severities, and the hub's closing alert lists both.
func TestTheSeveritiesSentWithoutTheHubReachTheClosingAlert(t *testing.T) {
	t.Parallel()
	pager, chat := recordingDestination(t, "own_pager", []string{"warn", "sos"}), recordingDestination(t, "own_chat", testhelpers.AllSeverities)
	hub, code, srv := startHubWithModules(t, map[string]checksandalerts.Destination{"on_call_pager": pager, "team_chat": chat}, func(dir string) { installRecordingModules(t, dir) })
	node := joinedNode(t, code, "app-1")
	installRecordingModules(t, node.config.ExternalModuleDir)
	if _, e := node.reload(); e != nil {
		t.Fatal(e)
	}
	node.report(context.Background())
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms"
	srv.Close()
	app := newAppOnNode("app-1", "warn")
	recordEvents(t, node, app.observe(t, "failed", time.Now().UTC()), app.raise(t, time.Now().UTC()), app.observe(t, "ok", time.Now().UTC()))
	runWorker(t, node)
	waitForRecordedAlerts(t, pager, 2)
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 1 || !reflect.DeepEqual(sent[0].AlertSeveritiesSentForIncident, []string{"warn", "sos"}) {
		t.Fatalf("the node carries %+v", sent)
	}
	back := httptest.NewUnstartedServer(hub.hubRoutes())
	back.TLS = &tls.Config{Certificates: []tls.Certificate{hub.hub.Cert}}
	back.StartTLS()
	t.Cleanup(back.Close)
	moved := code
	moved.Address = back.Listener.Addr().String()
	node.mu.Lock()
	node.setJoinCode(&moved)
	node.mu.Unlock()
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	if queued := queuedFor(t, hub, "on_call_pager"); len(queued) != 1 || queued[0].AlertSeverity != "sos" || !reflect.DeepEqual(queued[0].AlertSeveritiesSentForIncident, []string{"warn", "sos"}) {
		t.Fatalf("the hub's closing alert: %+v", queued)
	}
}

// A joined node learns from the fleet settings which destinations keep
// incidents, so a node without the module, or with an older copy of it,
// still never sends such a destination a recovery without the hub, and
// records the failure for its reports.
func TestAJoinedNodeTakesTheDestinationsThatKeepIncidentsFromTheHub(t *testing.T) {
	t.Parallel()
	pager := recordingDestination(t, "own_pager", testhelpers.AllSeverities)
	_, code, srv := startHubWithModules(t, map[string]checksandalerts.Destination{"on_call_pager": pager}, func(dir string) { installRecordingModules(t, dir) })
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	if node.fleetSettings == nil || !reflect.DeepEqual(node.fleetSettings.DestinationsWhoseServiceKeepsIncidents, []string{"on_call_pager"}) {
		t.Fatal("the fleet settings do not say which destinations keep incidents", node.fleetSettings)
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms"
	srv.Close()
	app := newAppOnNode("app-1", "warn")
	failed := app.observe(t, "failed", time.Now().UTC())
	recordEvents(t, node, failed, app.observe(t, "ok", time.Now().UTC()))
	runWorker(t, node)
	testhelpers.WaitFor(t, "both alerts to leave the hub's queue", func() bool { return outboxCount(t, node, "hub") == 0 })
	if got := queuedFor(t, node, "on_call_pager"); len(got) != 1 || got[0].ID != failed.ID {
		t.Fatalf("the node without the module queued for the pager: %v", typesOf(got))
	}
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 1 || sent[0].Incident != failed.Incident {
		t.Fatal("the node does not carry the incident", sent)
	}
}
