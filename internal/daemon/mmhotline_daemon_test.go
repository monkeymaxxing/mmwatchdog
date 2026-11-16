package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

const mmhotlineDestinationName = "mmhotline_on_call"

// mmhotlineDestinations is a configuration's one mmhotline destination.
func mmhotlineDestinations(t *testing.T, address ...string) map[string]checksandalerts.Destination {
	t.Helper()
	url := "https://hotline.example.com"
	if len(address) > 0 {
		url = address[0]
	}
	return map[string]checksandalerts.Destination{mmhotlineDestinationName: {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.MMHotlineDestination{URL: url, AlertSourceID: testhelpers.TestSourceID, SigningSecretFile: testhelpers.WriteSigningSecret(t)}, TimeLimitPerDeliveryAttempt: "1s"}}
}

// mmhotlineQueued lists the MMHotline alerts queued for delivery, in order,
// decoded from the bodies MMWatchdog will send as MMHotline decodes them.
func mmhotlineQueued(t *testing.T, d *Daemon) []testhelpers.MMHotlineAlert {
	t.Helper()
	alerts := []testhelpers.MMHotlineAlert{}
	for _, e := range queuedEvents(t, d) {
		if e.MMHotlineAlert == nil {
			t.Fatal("a MMHotline delivery without its alert", e)
		}
		a, err := testhelpers.DecodeMMHotlineAlert(e.MMHotlineAlert.Encode())
		if err != nil {
			t.Fatal(err)
		}
		alerts = append(alerts, a)
	}
	return alerts
}

func statusJSON(t *testing.T, d *Daemon) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status", nil))
	var status map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &status); e != nil {
		t.Fatal(e)
	}
	return status
}

func mmhotlineNode(t *testing.T, address ...string) *Daemon {
	t.Helper()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = mmhotlineDestinations(t, address...)
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	d.store.Node = "web-3"
	return d
}

func recordEvents(t *testing.T, d *Daemon, events ...checksandalerts.Event) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.record(events); e != nil {
		t.Fatal(e)
	}
}

// A standalone node sends each alert as MMHotline's generic signed alert:
// failures with the check's incident key and severity, its recovery as the
// resolution, notices as info, and never a reminder. A signed 400 leaves a
// visible gap without holding up the alerts behind it, a signed 429 waits
// for Retry-After, and mmwatchdog status names the address and problems.
func TestMMHotlineOnAStandaloneNode(t *testing.T) {
	f := testhelpers.NewFakeMMHotline(t, "ok")
	d := mmhotlineNode(t, f.URL)
	incident, code := incidents.ID(), 1
	// An hour ago: the outbox drops an alert older than Retention.
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	evidence := &checksandalerts.Evidence{Incident: incident, Observation: checksandalerts.Observation{Status: "failed", Message: "exit 1", ExitCode: &code, Stdout: "checking\nrefused", At: opened}, FailingSince: opened.Add(-time.Minute)}
	failed := checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", At: opened, Message: "exit 1", Evidence: evidence, Notify: true}
	reminder := checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Type: "reminder", AlertSeverity: "warn", At: opened, Message: "still failing", Destination: mmhotlineDestinationName, Notify: true}
	window := checksandalerts.Window{Until: opened.Add(time.Hour), Reason: "moving the database", User: "matt"}
	recordEvents(t, d, failed, reminder, incidents.MaintenanceEvent("maintenance_on", []string{"app"}, window, opened, "maintenance "+window.Describe()))
	queued := mmhotlineQueued(t, d)
	if len(queued) != 2 {
		t.Fatal("expected the failure and the maintenance notice, and no reminder", queued)
	}
	a := queued[0]
	if a["sender_alert_id"] != failed.ID || a["sender_incident_key"] != incidentKeyFor(d, "app/shell failed") || a["severity"] != "warn" || a["title"] != "web-3: app/shell failed" || a["happened_at"] != opened.Format(time.RFC3339) ||
		a["message"] != "exit 1\nfailing since "+checksandalerts.ClockTime(opened.Add(-time.Minute), opened)+"\nexit code: 1\nstdout:\n  checking\n  refused\nSent by the standalone MMWatchdog node web-3." {
		t.Fatalf("%+v", a)
	}
	if n := queued[1]; n["severity"] != "info" || n["title"] != "web-3: maintenance on for app" {
		t.Fatalf("%+v", n)
	}
	if _, keyed := queued[1]["sender_incident_key"]; keyed {
		t.Fatal("a notice has an incident key")
	}
	status := statusJSON(t, d)["mmhotline_destinations"].([]any)[0].(map[string]any)
	if status["mmhotline_url"] != f.URL || status["last_signed_answer_at"] != nil || status["node_clock_ahead_of_mmhotline_seconds"] != nil {
		t.Fatal("status before any answer", status)
	}
	runWorker(t, d)
	testhelpers.WaitFor(t, "two alerts", func() bool { alerts, _, _ := f.Received(); return len(alerts) == 2 })
	recordEvents(t, d, checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Type: "recovered", AlertSeverity: "warn", At: opened.Add(time.Minute), Message: "exit 0", Notify: true})
	testhelpers.WaitFor(t, "the resolution", func() bool { alerts, _, _ := f.Received(); return len(alerts) == 3 })
	alerts, _, _ := f.Received()
	if r := alerts[2]; r["severity"] != "resolved-warn" || r["sender_incident_key"] != incidentKeyFor(d, "app/shell failed") || r["title"] != "web-3: app/shell is ok again" {
		t.Fatalf("resolution %+v", r)
	}
	// A signed 400 is a delivery gap, and the next alert still goes out.
	f.SetMode("400")
	recordEvents(t, d, checksandalerts.Event{ID: incidents.ID(), Incident: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", At: time.Now(), Message: "exit 1", Notify: true})
	testhelpers.WaitFor(t, "the gap", func() bool { s, _ := d.store.DeliveryStats(); return s.Gaps == 1 && s.Pending == 0 })
	events, _ := d.store.Events(10)
	if events[0].Type != "delivery_gap" || !strings.Contains(events[0].Message, "MMHotline permanently refused this alert (HTTP 400)") {
		t.Fatal(events[0])
	}
	f.SetMode("ok")
	recordEvents(t, d, incidents.MaintenanceEvent("maintenance_off", []string{"app"}, window, time.Now(), "maintenance turned off"))
	testhelpers.WaitFor(t, "the alert after the gap", func() bool { alerts, _, _ := f.Received(); return len(alerts) == 4 })
	// A signed 429 waits for Retry-After, and status says so.
	f.SetMode("429")
	f.SetDate(time.Now().Add(-2 * time.Minute))
	beforeTheAttempt := time.Now()
	recordEvents(t, d, incidents.MaintenanceEvent("maintenance_on", []string{"app"}, window, time.Now(), "maintenance on"))
	var next time.Time
	var lastError string
	testhelpers.WaitFor(t, "the rate limit", func() bool { next, lastError, _ = d.store.Head(mmhotlineDestinationName); return lastError != "" })
	// The next attempt comes the 7 seconds of Retry-After after the
	// attempt, which came between beforeTheAttempt and now.
	if next.Before(beforeTheAttempt.Add(7*time.Second)) || next.After(time.Now().Add(7*time.Second)) {
		t.Fatal("Retry-After not honored", time.Until(next))
	}
	status = statusJSON(t, d)["mmhotline_destinations"].([]any)[0].(map[string]any)
	problems := fmt.Sprint(status["delivery_problems"])
	if status["mmhotline_url"] != f.URL || status["last_signed_answer_at"] == nil ||
		status["node_clock_ahead_of_mmhotline_seconds"].(float64) < 110 ||
		!strings.Contains(problems, "MMHotline asked MMWatchdog to wait 7s before sending again (HTTP 429). Trying again at ") || strings.Contains(problems, "clock") ||
		!strings.HasPrefix(fmt.Sprint(status["clock_warning"]), "This node's clock is 2 minutes ahead of MMHotline's.") || status["check_in_shortened"] != nil {
		t.Fatal(status)
	}
}

// One failed request is one attempt; the next attempt waits one second.
func TestMMHotlineBackoffAfterOneRequest(t *testing.T) {
	f := testhelpers.NewFakeMMHotline(t, "503")
	d := mmhotlineNode(t, f.URL)
	recordEvents(t, d, checksandalerts.Event{ID: incidents.ID(), Incident: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", At: time.Now(), Message: "exit 1", Notify: true})
	runWorker(t, d)
	var next time.Time
	var lastError string
	testhelpers.WaitFor(t, "the failed attempt", func() bool { next, lastError, _ = d.store.Head(mmhotlineDestinationName); return lastError != "" })
	_, _, firstRequests := f.Received()
	var attempts int
	d.store.DB.QueryRow("SELECT attempts FROM outbox WHERE destination=?", mmhotlineDestinationName).Scan(&attempts)
	if firstRequests != 1 || attempts != 1 || lastError != "MMHotline gave no signed answer (HTTP 503)." || time.Until(next) > time.Second {
		t.Fatal(firstRequests, attempts, lastError, time.Until(next))
	}
}

// acceptAlert saves a node's alert in the hub's alert grouping step.
func acceptAlert(t *testing.T, hub *Daemon, node string, e checksandalerts.Event) checksandalerts.Event {
	t.Helper()
	e.Node = node
	if checksandalerts.BelongsToAnIncident(e.Type) {
		key := e.FleetIncidentKey
		if key == "" || node == hub.store.Node && e.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			key = checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(e, checksandalerts.IncidentKind(e.Type), hub.store.Node))
		}
		e.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(key, hub.store.Node, hub.store.FleetIdentity)
	}
	if err := hub.store.Accept(node, e, time.Now()); err != nil {
		t.Fatal(err)
	}
	return e
}

func groupAt(t *testing.T, hub *Daemon, now time.Time) {
	t.Helper()
	// As the delivery worker does, before it takes the daemon's lock.
	awaiting := hub.hub.AwaitingFirstReports(time.Now())
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if e := hub.groupFleetAlerts(map[string]bool{}, awaiting, now); e != nil {
		t.Fatal(e)
	}
}

func checkAlert(kind, incident, group, check string, at time.Time) checksandalerts.Event {
	return checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: group, Check: check, Type: kind, AlertSeverity: "warn", At: at, Message: kind}
}

func titles(alerts []testhelpers.MMHotlineAlert) []string {
	out := []string{}
	for _, a := range alerts {
		out = append(out, a["title"])
	}
	return out
}

// The MMWatchdog fleet hub keys MMHotline incidents per check across the fleet,
// keeps the set of MMWatchdog incidents open for each key, and sends one resolution
// when it empties. Silence alerts are sos.
func TestMMHotlineFleetIncidentKeys(t *testing.T) {
	t.Parallel()
	hub, _, _ := startHubWith(t, mmhotlineDestinations(t))
	now := time.Now().UTC()
	a2, a3, a4 := incidents.ID(), incidents.ID(), incidents.ID()
	acceptAlert(t, hub, "app-2", checkAlert("failed", a2, "myapp", "http", now))
	groupAt(t, hub, now)
	acceptAlert(t, hub, "app-3", checkAlert("failed", a3, "myapp", "http", now))
	acceptAlert(t, hub, "app-4", checkAlert("failed", a4, "myapp", "http", now))
	groupAt(t, hub, now.Add(time.Second))
	groupAt(t, hub, now.Add(2*time.Minute))
	queued := mmhotlineQueued(t, hub)
	if len(queued) != 2 || queued[0]["title"] != "app-2: myapp/http failed" || queued[1]["title"] != "myapp/http failed on 2 nodes" ||
		queued[0]["sender_incident_key"] != incidentKeyFor(hub, "myapp/http failed") || queued[1]["sender_incident_key"] != incidentKeyFor(hub, "myapp/http failed") || queued[1]["severity"] != "warn" ||
		!strings.Contains(queued[1]["message"], "Sent by the MMWatchdog fleet hub on "+hub.store.Node+".") {
		t.Fatalf("%+v", queued)
	}
	// Recoveries remove incidents from the set; only the last sends.
	acceptAlert(t, hub, "app-2", checkAlert("recovered", a2, "myapp", "http", now))
	groupAt(t, hub, now.Add(4*time.Minute))
	if n := len(mmhotlineQueued(t, hub)); n != 2 {
		t.Fatal("a single node's recovery reached MMHotline", n)
	}
	acceptAlert(t, hub, "app-3", checkAlert("recovered", a3, "myapp", "http", now))
	acceptAlert(t, hub, "app-4", checkAlert("closed_without_recovery", a4, "myapp", "http", now))
	groupAt(t, hub, now.Add(6*time.Minute))
	groupAt(t, hub, now.Add(8*time.Minute))
	queued = mmhotlineQueued(t, hub)
	if len(queued) != 3 {
		t.Fatalf("expected one resolution %+v", queued)
	}
	if r := queued[2]; r["sender_incident_key"] != incidentKeyFor(hub, "myapp/http failed") || r["severity"] != "resolved-warn" || !strings.HasPrefix(r["message"], "myapp/http incidents closed on every node, maybe not fixed (was open on app-2, app-3, app-4).") {
		t.Fatalf("%+v", r)
	}
	// Silence alerts share one key, and the hub-outage alerter's has its own. Each
	// carries the severity MMWatchdog gave it.
	silent, outageAlerter, cannot := incidents.ID(), incidents.ID(), incidents.ID()
	sos := func(e checksandalerts.Event) checksandalerts.Event { e.AlertSeverity = "sos"; return e }
	acceptAlert(t, hub, hub.store.Node, sos(checkAlert("failed", silent, checksandalerts.NodeReportsCheckGroup, "app-5", now)))
	acceptAlert(t, hub, "app-6", sos(checkAlert("failed", outageAlerter, checksandalerts.NodeReportsCheckGroup, checksandalerts.HubCheck, now)))
	acceptAlert(t, hub, "app-7", checkAlert("cannot_check", cannot, "myapp", "http", now))
	groupAt(t, hub, now.Add(10*time.Minute))
	acceptAlert(t, hub, hub.store.Node, checkAlert("recovered", silent, checksandalerts.NodeReportsCheckGroup, "app-5", now))
	acceptAlert(t, hub, "app-6", checkAlert("recovered", outageAlerter, checksandalerts.NodeReportsCheckGroup, checksandalerts.HubCheck, now))
	acceptAlert(t, hub, "app-7", checkAlert("can_check_again", cannot, "myapp", "http", now))
	groupAt(t, hub, now.Add(12*time.Minute))
	got := map[string]testhelpers.MMHotlineAlert{}
	for _, a := range mmhotlineQueued(t, hub)[3:] {
		got[a["title"]] = a
	}
	for title, want := range map[string][2]string{
		"app-5 stopped reporting to the MMWatchdog fleet hub":     {"node_reports stopped reporting", "sos"},
		"app-6 cannot reach the MMWatchdog fleet hub":             {"node_reports/to_hub failed", "sos"},
		"app-7: MMWatchdog cannot check myapp/http":               {"myapp/http cannot check", "warn"},
		"app-5 reports to the MMWatchdog fleet hub again":         {"node_reports stopped reporting", "resolved-sos"},
		"Reports from app-6 reach the MMWatchdog fleet hub again": {"node_reports/to_hub failed", "resolved-sos"},
		"app-7: MMWatchdog can check myapp/http again":            {"myapp/http cannot check", "resolved-warn"},
	} {
		if a, ok := got[title]; !ok || a["sender_incident_key"] != incidentKeyFor(hub, want[0]) || a["severity"] != want[1] {
			t.Errorf("%q: %+v, want %v", title, a, want)
		}
	}
	if len(got) != 6 {
		t.Fatal(titles(mmhotlineQueued(t, hub)))
	}
}

// A failure waiting in the one-minute grouping window holds its key open:
// no resolution goes out before it. A failure whose incident the set
// already knows is over is not sent, and its key still resolves.
func TestMMHotlineResolutionWaitsForGroupedFailures(t *testing.T) {
	t.Parallel()
	hub, _, _ := startHubWith(t, mmhotlineDestinations(t))
	now := time.Now().UTC()
	a2, a3 := incidents.ID(), incidents.ID()
	acceptAlert(t, hub, "app-2", checkAlert("failed", a2, "myapp", "http", now))
	groupAt(t, hub, now)
	acceptAlert(t, hub, "app-3", checkAlert("failed", a3, "myapp", "http", now.Add(10*time.Second)))
	acceptAlert(t, hub, "app-2", checkAlert("recovered", a2, "myapp", "http", now.Add(20*time.Second)))
	groupAt(t, hub, now.Add(20*time.Second))
	if got := titles(mmhotlineQueued(t, hub)); len(got) != 1 {
		t.Fatal("a resolution went out while app-3's failure waited in grouping", got)
	}
	groupAt(t, hub, now.Add(time.Minute+time.Second))
	acceptAlert(t, hub, "app-3", checkAlert("recovered", a3, "myapp", "http", now.Add(2*time.Minute)))
	groupAt(t, hub, now.Add(3*time.Minute))
	got := mmhotlineQueued(t, hub)
	if len(got) != 3 || got[1]["title"] != "app-3: myapp/http failed" || got[2]["title"] != "myapp/http is ok on every node again" ||
		!strings.HasPrefix(got[2]["message"], "myapp/http is ok on every node again (was failing on app-2, app-3).") {
		t.Fatal(titles(got))
	}
	// Reports open and close app-4's incident while its failure alert still
	// waits: the key waits for it, and the stale failure is not sent.
	reconcile := func(changes map[string]store.ReportedIncidents) {
		t.Helper()
		hub.mu.Lock()
		defer hub.mu.Unlock()
		if e := hub.store.ReconcileIncidentSets(changes, hub.config.Alerts.Destinations, false, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	a4, later := incidents.ID(), now.Add(time.Hour)
	acceptAlert(t, hub, "app-4", checkAlert("failed", a4, "myapp", "http", later))
	shown := map[string]checksandalerts.Event{a4: {Node: "app-4", CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "warn", Incident: a4}}
	reconcile(map[string]store.ReportedIncidents{"app-4": {Sent: later.Add(time.Second), Incidents: shown}})
	reconcile(map[string]store.ReportedIncidents{"app-4": {Sent: later.Add(time.Minute), Incidents: map[string]checksandalerts.Event{}}})
	if n := len(mmhotlineQueued(t, hub)); n != 3 {
		t.Fatal("resolved while app-4's failure waited in grouping", n)
	}
	groupAt(t, hub, later.Add(2*time.Minute))
	got = mmhotlineQueued(t, hub)
	if len(got) != 4 || got[3]["title"] != "app-4: myapp/http is ok again" || !strings.HasPrefix(got[3]["message"], "app-4: myapp/http is ok again.") {
		t.Fatal("the stale failure went out, or its key did not resolve", titles(got))
	}
}

// The hub also learns incidents from each node's latest report: one only a
// report shows joins the set, and one a newer report no longer shows closes
// it. A report sent before an alert never closes its incident, and a
// resolved incident never comes back from an older report.
func TestMMHotlineIncidentsFollowReports(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, mmhotlineDestinations(t))
	node := joinedNode(t, code, "app-2")
	incident := incidents.ID()
	setTrack := func(id string) {
		node.mu.Lock()
		defer node.mu.Unlock()
		node.snapshot.Checks["app/shell"].FailureIncident = incidents.Track{Incident: id, Announced: id, Opened: time.Now(), AlertSeverity: "warn", AnnouncedAlertSeverities: []string{"warn"}}
	}
	setTrack(incident)
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	now := time.Now().UTC()
	a3 := incidents.ID()
	acceptAlert(t, hub, "app-3", checkAlert("failed", a3, "app", "shell", now))
	groupAt(t, hub, now)
	acceptAlert(t, hub, "app-3", checkAlert("recovered", a3, "app", "shell", now))
	groupAt(t, hub, now.Add(2*time.Minute))
	if n := len(mmhotlineQueued(t, hub)); n != 1 {
		t.Fatal("resolved while app-2's report still shows its incident", n)
	}
	setTrack("")
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	queued := mmhotlineQueued(t, hub)
	if len(queued) != 2 || queued[1]["title"] != "app/shell is ok on every node again" || !strings.HasPrefix(queued[1]["message"], "app/shell is ok on every node again (was failing on app-2, app-3).") {
		t.Fatalf("%+v", queued)
	}
	reconcile := func(changes map[string]store.ReportedIncidents) {
		t.Helper()
		hub.mu.Lock()
		defer hub.mu.Unlock()
		if e := hub.store.ReconcileIncidentSets(changes, hub.config.Alerts.Destinations, false, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	openRows := func() int {
		var n int
		hub.store.DB.QueryRow("SELECT COUNT(*) FROM fleet_incident_set WHERE state=?", store.FleetIncidentOpen).Scan(&n)
		return n
	}
	// A report older than the resolution cannot open the incident again.
	reconcile(map[string]store.ReportedIncidents{"app-2": {Sent: now.Add(-time.Minute), Incidents: map[string]checksandalerts.Event{incident: {Node: "app-2", CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", Incident: incident}}}})
	if openRows() != 0 {
		t.Fatal("an older report opened a resolved incident again")
	}
	// A report sent before a failure alert does not close its incident.
	later := now.Add(time.Hour)
	a4 := incidents.ID()
	acceptAlert(t, hub, "app-4", checkAlert("failed", a4, "app", "shell", later))
	groupAt(t, hub, later)
	reconcile(map[string]store.ReportedIncidents{"app-4": {Sent: later.Add(-time.Second), Incidents: map[string]checksandalerts.Event{}}})
	if openRows() != 1 {
		t.Fatal("a report sent before the alert closed its incident")
	}
}

// A report's incident joins the MMHotline incident set only when its alerts
// go to the mmhotline destination. A check that names only another
// destination neither holds open the key of the same check on other nodes
// nor gets a resolution of its own.
func TestMMHotlineIncidentsOfReportsFollowTheNamedDestinations(t *testing.T) {
	t.Parallel()
	destinations := mmhotlineDestinations(t)
	destinations["ops"] = checksandalerts.Destination{RemindEvery: "never", AlertSeverities: []string{}, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "5s"}
	hub, code, _ := startHubWith(t, destinations)
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
	a3 := incidents.ID()
	acceptAlert(t, hub, "app-3", checkAlert("failed", a3, "app", "shell", now))
	groupAt(t, hub, now)
	acceptAlert(t, hub, "app-3", checkAlert("recovered", a3, "app", "shell", now))
	groupAt(t, hub, now.Add(2*time.Minute))
	queued := mmhotlineQueued(t, hub)
	if len(queued) != 2 || queued[1]["title"] != "app-3: app/shell is ok again" || !strings.HasPrefix(queued[1]["message"], "app-3: app/shell is ok again.") {
		t.Fatal("the incident of a check that names only ops held the MMHotline key open", titles(queued))
	}
	setTrack("")
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	if n := len(mmhotlineQueued(t, hub)); n != 2 {
		t.Fatal("MMHotline got a resolution of an incident that it never got", n)
	}
}

// Forgetting a node closes its incidents through the fleet's own forget,
// also when the hub restarts before it applied the forget.
func TestMMHotlineForgetClosesIncidents(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		hub, code, _ := startHubWith(t, mmhotlineDestinations(t))
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
		queued := mmhotlineQueued(t, hub)
		if len(queued) != 1 || queued[0]["title"] != "app-2: app/shell incident closed, maybe not fixed" || queued[0]["severity"] != "resolved-warn" {
			t.Fatal(restart, titles(queued))
		}
	}
}

// A joined node holds the hub's signing secret and address, sends an
// alert the hub did not accept straight to MMHotline with the same key, and
// never sends a resolution or a check-in. Its reports carry that
// incident until the hub takes one, so the hub, back, resolves its key even
// though the node recovered while it was down.
func TestMMHotlineDirectFallback(t *testing.T) {
	f := testhelpers.NewFakeMMHotline(t, "ok")
	hub, code, srv := startHubWith(t, mmhotlineDestinations(t, f.URL))
	hub.sendCheckIn(context.Background(), time.Now().UTC())
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	held, _ := os.ReadFile(filepath.Join(node.config.StateDir, fleet.FleetSettingsFile))
	if node.fleetSettings.FileContentsByDestination[mmhotlineDestinationName]["mmhotline_signing_secret_file"] != hex.EncodeToString(testhelpers.TestSigningSecret) ||
		!strings.Contains(string(held), `"file_contents_by_destination"`) {
		t.Fatal("the joined node lacks the signing secret or address", string(held))
	}
	// The joined node's copy holds the destination, but it sends no check-in.
	_, checkIns, _ := f.Received()
	node.sendCheckIn(context.Background(), time.Now())
	if _, again, _ := f.Received(); len(again) != len(checkIns) {
		t.Fatal("a joined node sent a check-in")
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms"
	srv.Close()
	incident := incidents.ID()
	recordEvents(t, node, checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", At: time.Now(), Message: "exit 1", Notify: true},
		checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Type: "recovered", AlertSeverity: "warn", At: time.Now(), Message: "exit 0", Notify: true})
	runWorker(t, node)
	testhelpers.WaitFor(t, "both alerts to leave the hub's queue and the MMHotline queue", func() bool {
		return outboxCount(t, node, config.HubDestination) == 0 && outboxCount(t, node, mmhotlineDestinationName) == 0
	})
	alerts, _, _ := f.Received()
	if len(alerts) != 1 || alerts[0]["sender_incident_key"] != incidentKeyFor(hub, "app/shell failed") || !strings.HasSuffix(alerts[0]["message"], "Sent directly by the MMWatchdog node app-1 because the MMWatchdog fleet hub did not accept the alert in time.") {
		t.Fatalf("expected the failure alone %+v", alerts)
	}
	// The hub returns at another address; the node's next report carries
	// the incident it sent directly, and the hub resolves the key.
	back := httptest.NewUnstartedServer(hub.hubRoutes())
	back.TLS = &tls.Config{Certificates: []tls.Certificate{hub.hub.Cert}}
	back.StartTLS()
	t.Cleanup(back.Close)
	moved := code
	moved.Address = back.Listener.Addr().String()
	node.mu.Lock()
	node.setJoinCode(&moved)
	node.mu.Unlock()
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 1 || sent[0].Incident != incident {
		t.Fatal("the node does not remember the incident it sent directly", sent)
	}
	// The node keeps carrying it until a reply says the hub holds it, even
	// when the hub accepted the report but has not applied it yet.
	node.report(context.Background())
	node.report(context.Background())
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 1 || node.link.LastError != "" {
		t.Fatal("the node forgot the incident before the hub held it", sent, node.link.LastError)
	}
	hub.reconcileReports(time.Now())
	node.report(context.Background())
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 0 {
		t.Fatal("the hub's reply did not let the node forget the incident", sent)
	}
	hub.reconcileReports(time.Now())
	queued := mmhotlineQueued(t, hub)
	if len(queued) != 1 || queued[0]["title"] != "app-1: app/shell is ok again" || !strings.HasPrefix(queued[0]["message"], "app-1: app/shell is ok again.") {
		t.Fatal(titles(queued))
	}
}

// The check-in names its sender and lists every node in a whole-node
// window, from the nodes' reports and the hub's own, but no check-group
// window, and leaves the list out when it is empty. A reload sends one to
// every mmhotline destination at once.
func TestCheckIns(t *testing.T) {
	f := testhelpers.NewFakeMMHotline(t, "ok")
	hub, code, _ := startHubWith(t, mmhotlineDestinations(t, f.URL))
	destination := hub.config.Alerts.Destinations[mmhotlineDestinationName]
	secret := destination.MMHotline().SigningSecretFile
	if err := os.Chmod(secret, 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(secret, 0600)
	// A live node keeps sending check-ins after a permission change.
	hub.sendCheckIn(context.Background(), time.Now().UTC())
	_, checkIns, _ := f.Received()
	if len(checkIns) != 1 || checkIns[0]["sent_by_node_name"] != hub.store.Node || checkIns[0]["sent_at"] == nil {
		t.Fatal(checkIns)
	}
	if _, listed := checkIns[0]["nodes_in_whole_node_maintenance"]; listed {
		t.Fatal("an empty list was sent", checkIns[0])
	}
	whole, groupOnly := joinedNode(t, code, "app-2"), joinedNode(t, code, "app-3")
	w := httptest.NewRecorder()
	whole.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/maintenance/on/"+checksandalerts.EveryCheckGroup, strings.NewReader(`{"for":"1h","reason":"moving the database","user":"matt"}`)))
	setMaintenance(t, groupOnly, "app", "1h")
	setMaintenance(t, hub, checksandalerts.EveryCheckGroup, "30m")
	whole.report(context.Background())
	groupOnly.report(context.Background())
	hub.sendCheckIn(context.Background(), time.Now().UTC())
	_, checkIns, _ = f.Received()
	nodes := checkIns[1]["nodes_in_whole_node_maintenance"].([]any)
	own, other := nodes[0].(map[string]any), nodes[len(nodes)-1].(map[string]any)
	if w.Code != http.StatusOK || len(nodes) != 2 || other["node_name"] != "app-2" || other["reason"] != "moving the database" || other["started_by"] != "matt" ||
		own["node_name"] != hub.store.Node || own["maintenance_until"] == nil {
		t.Fatal(nodes)
	}
	// The running sender sends one at start, and at once after a reload.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hub.checkIns(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	testhelpers.WaitFor(t, "the check-in at start", func() bool { _, checkIns, _ := f.Received(); return len(checkIns) == 3 })
	if err := os.Chmod(secret, 0600); err != nil {
		t.Fatal(err)
	}
	destination = hub.config.Alerts.Destinations[mmhotlineDestinationName]
	destinations := map[string]checksandalerts.Destination{mmhotlineDestinationName: destination, "second_team": destination}
	encoded, _ := json.Marshal(config.Alerts{Destinations: destinations})
	if err := os.WriteFile(filepath.Join(hub.path, "alert_destinations.yml"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, e := hub.reload(); e != nil {
		t.Fatal(e)
	}
	// Both destinations, the one that was there and the one the reload added,
	// get one at once.
	testhelpers.WaitFor(t, "the check-ins after the reload", func() bool { _, checkIns, _ := f.Received(); return len(checkIns) == 5 })
}

// After the hub starts, an incident a node sent without the hub resolves
// nothing until every node has reported once: another node that also sent
// one may still be failing.
func TestMMHotlineResolutionWaitsForEveryNodeAfterHubStart(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, mmhotlineDestinations(t))
	a, b := joinedNode(t, code, "app-a"), joinedNode(t, code, "app-b")
	a.report(context.Background())
	b.report(context.Background())
	incidentA, incidentB := incidents.ID(), incidents.ID()
	for node, incident := range map[*Daemon]string{a: incidentA, b: incidentB} {
		if _, e := node.store.DB.Exec("INSERT INTO incidents_sent_without_the_hub(destination,incident_id,check_group,check_name,finding,kind,alert_severities_sent_for_incident,at) VALUES(?,?,?,?,?,?,?,?)", mmhotlineDestinationName, incident, "app", "shell", "", "failed", "warn", time.Now().UnixNano()); e != nil {
			t.Fatal(e)
		}
	}
	setTrack := func(node *Daemon, id string) {
		node.mu.Lock()
		defer node.mu.Unlock()
		node.snapshot.Checks["app/shell"].FailureIncident = incidents.Track{Incident: id, Announced: id, Opened: time.Now(), AlertSeverity: "warn", AnnouncedAlertSeverities: []string{"warn"}}
	}
	setTrack(b, incidentB) // app-b is still failing; app-a recovered
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
	a.report(context.Background())
	restarted.reconcileReports(time.Now())
	if got := mmhotlineQueued(t, restarted); len(got) != 0 {
		t.Fatal("resolved before app-b reported after the hub started", titles(got))
	}
	b.report(context.Background())
	restarted.reconcileReports(time.Now())
	if got := mmhotlineQueued(t, restarted); len(got) != 0 {
		t.Fatal("resolved while app-b still fails", titles(got))
	}
	setTrack(b, "")
	b.report(context.Background())
	restarted.reconcileReports(time.Now())
	got := mmhotlineQueued(t, restarted)
	if len(got) != 1 || !strings.HasPrefix(got[0]["message"], "app/shell is ok on every node again (was failing on app-a, app-b).") {
		t.Fatal(titles(got))
	}
}

func TestMMHotlineDestinationsSendCheckInsConcurrentlyAndShowTheirOwnStatus(t *testing.T) {
	slow := testhelpers.NewFakeMMHotline(t, "401")
	slow.SetDate(time.Now().Add(7 * time.Minute))
	fast := testhelpers.NewFakeMMHotline(t, "ok")
	fast.SetDate(time.Now().Add(-2 * time.Minute))
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-release:
			slow.Config.Handler.ServeHTTP(w, request)
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	var once sync.Once
	defer once.Do(func() { close(release) })
	path, settings := daemonConfig(t)
	settings.Alerts.Destinations = mmhotlineDestinations(t, server.URL)
	slowDestination := settings.Alerts.Destinations[mmhotlineDestinationName]
	slowDestination.TimeLimitPerDeliveryAttempt = "1m"
	settings.Alerts.Destinations[mmhotlineDestinationName] = slowDestination
	destination := mmhotlineDestinations(t, fast.URL)[mmhotlineDestinationName]
	settings.Alerts.Destinations["another_team"] = destination
	writeConfig(t, path, settings)
	node, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	before := statusJSON(t, node)["mmhotline_destinations"].([]any)
	if len(before) != 2 || before[0].(map[string]any)["destination"] != "another_team" {
		t.Fatal(before)
	}
	done := make(chan struct{})
	go func() { node.sendCheckIn(context.Background(), time.Now()); close(done) }()
	select {
	case <-started:
	case <-time.After(testhelpers.LongestWaitForAnEvent):
		t.Fatal("the first check-in never started")
	}
	// The second request must arrive while the first is blocked.
	testhelpers.WaitFor(t, "the other team's check-in while the first is blocked", func() bool { _, checkIns, _ := fast.Received(); return len(checkIns) == 1 })
	once.Do(func() { close(release) })
	<-done
	statuses := statusJSON(t, node)["mmhotline_destinations"].([]any)
	fastStatus, slowStatus := statuses[0].(map[string]any), statuses[1].(map[string]any)
	if fastStatus["mmhotline_url"] != fast.URL || fastStatus["last_signed_answer_at"] == nil || fastStatus["node_clock_ahead_of_mmhotline_seconds"].(float64) < 110 || len(fastStatus["delivery_problems"].([]any)) != 0 {
		t.Fatal(fastStatus)
	}
	if slowStatus["mmhotline_url"] != server.URL || slowStatus["last_signed_answer_at"] != nil || slowStatus["node_clock_ahead_of_mmhotline_seconds"] != nil || !strings.Contains(fmt.Sprint(slowStatus["delivery_problems"]), "MMHotline rejected the signature (HTTP 401).") {
		t.Fatal(slowStatus)
	}
}

func TestMMHotlineSigningSecretRotationKeepsQueuedAlerts(t *testing.T) {
	server := testhelpers.NewFakeMMHotline(t, "unsigned_503")
	node := mmhotlineNode(t, server.URL)
	event := checksandalerts.Event{ID: incidents.ID(), Incident: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", At: time.Now(), Message: "exit 1", Notify: true}
	recordEvents(t, node, event)
	runWorker(t, node)
	testhelpers.WaitFor(t, "the queued alert's failed attempt", func() bool { _, problem, _ := node.store.Head(mmhotlineDestinationName); return problem != "" })
	secret := bytes.Repeat([]byte{0x73}, 32)
	destination := node.config.Alerts.Destinations[mmhotlineDestinationName]
	if err := os.WriteFile(destination.MMHotline().SigningSecretFile, []byte(hex.EncodeToString(secret)), 0600); err != nil {
		t.Fatal(err)
	}
	server.SetSigningSecret(secret)
	server.SetMode("ok")
	if _, err := node.reload(); err != nil {
		t.Fatal(err)
	}
	testhelpers.WaitFor(t, "the original queued alert signed with the new secret", func() bool { return len(server.AlertIDs()) == 1 })
	if ids := server.AlertIDs(); ids[0] != event.ID {
		t.Fatal(ids)
	}
	// The server has the alert before the node has recorded the delivery:
	// wait for the record, never assume it is there at once.
	var stats store.DeliverySummary
	var err error
	testhelpers.WaitFor(t, "the delivery of the queued alert recorded", func() bool {
		stats, err = node.store.DeliveryStats()
		return err != nil || stats.Pending == 0
	})
	if err != nil || stats.Pending != 0 {
		t.Fatal(stats, err)
	}
}

func TestMMHotlineStatusIsAnEmptyListWithoutDestinations(t *testing.T) {
	path, settings := daemonConfig(t)
	writeConfig(t, path, settings)
	node, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	statuses, ok := statusJSON(t, node)["mmhotline_destinations"].([]any)
	if !ok || len(statuses) != 0 {
		t.Fatal(statuses, ok)
	}
}

func TestMMHotlineAddressOrSourceReloadStartsFreshConnectionStatus(t *testing.T) {
	for _, change := range []string{"address", "alert source"} {
		t.Run(change, func(t *testing.T) {
			first := testhelpers.NewFakeMMHotline(t, "ok")
			node := mmhotlineNode(t, first.URL)
			node.sendCheckIn(context.Background(), time.Now())
			before := statusJSON(t, node)["mmhotline_destinations"].([]any)[0].(map[string]any)
			if before["last_signed_answer_at"] == nil {
				t.Fatal(before)
			}
			destination := node.config.Alerts.Destinations[mmhotlineDestinationName]
			inputs := *destination.MMHotline()
			if change == "address" {
				second := testhelpers.NewFakeMMHotline(t, "401")
				inputs.URL = second.URL
			} else {
				inputs.AlertSourceID = strings.Repeat("a", 32)
			}
			destination.Inputs = &inputs
			body, _ := json.Marshal(config.Alerts{Destinations: map[string]checksandalerts.Destination{mmhotlineDestinationName: destination}})
			if err := os.WriteFile(filepath.Join(node.path, "alert_destinations.yml"), body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := node.reload(); err != nil {
				t.Fatal(err)
			}
			after := statusJSON(t, node)["mmhotline_destinations"].([]any)[0].(map[string]any)
			if after["mmhotline_url"] != inputs.URL || after["mmhotline_alert_source_id"] != inputs.AlertSourceID || after["last_signed_answer_at"] != nil || after["node_clock_ahead_of_mmhotline_seconds"] != nil {
				t.Fatal(after)
			}
		})
	}
}

// An alert that MMHotline permanently refuses goes at once, with its own
// severity, title, and body, to every other destination of the node that is
// not an mmhotline destination, also to one whose alert_severities do not
// take it and one that the check's alerts do not name: a refused SOS still
// reaches a person. Another team's MMHotline gets nothing.
func TestARefusedAlertGoesToTheOtherDestinations(t *testing.T) {
	t.Parallel()
	refusing, otherTeam := testhelpers.NewFakeMMHotline(t, "400"), testhelpers.NewFakeMMHotline(t, "ok")
	pager := captureAlerts(t)
	p, c := daemonConfig(t)
	c.Alerts.Destinations = mmhotlineDestinations(t, refusing.URL)
	c.Alerts.Destinations["other_team_mmhotline"] = mmhotlineDestinations(t, otherTeam.URL)[mmhotlineDestinationName]
	c.Alerts.Destinations["pager"] = checksandalerts.Destination{RemindEvery: "never", AlertSeverities: []string{"info"}, Inputs: &checksandalerts.Webhook{URL: pager.URL}, TimeLimitPerDeliveryAttempt: "1m"}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	d.store.Node = "web-3"
	only, incident := []string{mmhotlineDestinationName}, incidents.ID()
	recordEvents(t, d, checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "sos", At: time.Now(), Message: "exit 1", Notify: true, SendAlertsOnlyToDestinations: &only})
	runWorker(t, d)
	passed := pager.wait(t, 1)[0]
	if passed.Type != "notice" || passed.AlertSeverity != "sos" || passed.TitleWithAlertSeverity != "SOS: web-3: app/shell failed" ||
		!strings.HasPrefix(passed.Body, "MMHotline (destination mmhotline_on_call) permanently refused this alert, so MMWatchdog sends it here instead. MMHotline's reason (HTTP 400): ") ||
		!strings.Contains(passed.Body, "\n\nexit 1") {
		t.Fatalf("%+v", passed)
	}
	// A refused resolution comes as information: it calls nobody out.
	recordEvents(t, d, checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Type: "recovered", AlertSeverity: "sos", At: time.Now(), Message: "exit 0", Notify: true, SendAlertsOnlyToDestinations: &only})
	if resolved := pager.wait(t, 2)[1]; resolved.AlertSeverity != "info" || resolved.TitleWithAlertSeverity != "Info: web-3: app/shell is ok again" {
		t.Fatalf("%+v", resolved)
	}
	if alerts, _, _ := otherTeam.Received(); len(alerts) != 0 {
		t.Fatal("another team's MMHotline got the refused alert", alerts)
	}
	if s, _ := d.store.DeliveryStats(); s.Gaps != 2 {
		t.Fatal("the refusal is not counted as a dropped delivery", s)
	}
}

// The MMWatchdog fleet hub passes on an alert of a joined node that MMHotline
// permanently refuses to the hub's own other destinations, as it does its own.
func TestTheHubPassesOnARefusedAlertOfAJoinedNode(t *testing.T) {
	t.Parallel()
	refusing, pager := testhelpers.NewFakeMMHotline(t, "400"), captureAlerts(t)
	destinations := mmhotlineDestinations(t, refusing.URL)
	destinations["on_call_push_app"] = checksandalerts.Destination{RemindEvery: "never", AlertSeverities: []string{"info"}, Inputs: &checksandalerts.Webhook{URL: pager.URL}, TimeLimitPerDeliveryAttempt: "1m"}
	hub, _, _ := startHubWith(t, destinations)
	sos := checkAlert("failed", incidents.ID(), "app", "shell", time.Now().UTC())
	sos.AlertSeverity = "sos"
	acceptAlert(t, hub, "app-2", sos)
	groupAt(t, hub, time.Now().UTC())
	runWorker(t, hub)
	passed := pager.wait(t, 1)[0]
	if passed.AlertSeverity != "sos" || !strings.Contains(passed.TitleWithAlertSeverity, "app-2") || !strings.Contains(passed.TitleWithAlertSeverity, "app/shell failed") ||
		!strings.HasPrefix(passed.Body, "MMHotline (destination mmhotline_on_call) permanently refused this alert, so MMWatchdog sends it here instead.") {
		t.Fatalf("%+v", passed)
	}
}
