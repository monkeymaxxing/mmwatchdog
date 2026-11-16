package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// destination is a webhook that records the events it receives and the
// Authorization header each came with.
type destination struct {
	*httptest.Server
	mu     sync.Mutex
	events []checksandalerts.Event
	auth   []string
}

func captureAlerts(t *testing.T) *destination {
	t.Helper()
	d := &destination{}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event checksandalerts.Event
		if e := json.NewDecoder(r.Body).Decode(&event); e != nil {
			t.Error(e)
		}
		d.mu.Lock()
		d.events = append(d.events, event)
		d.auth = append(d.auth, r.Header.Get("Authorization"))
		d.mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(d.Close)
	return d
}

// wait returns the first n events once they have arrived.
func (d *destination) wait(t *testing.T, n int) []checksandalerts.Event {
	t.Helper()
	for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		d.mu.Lock()
		if len(d.events) >= n {
			events := append([]checksandalerts.Event{}, d.events[:n]...)
			d.mu.Unlock()
			return events
		}
		d.mu.Unlock()
	}
	t.Fatalf("expected %d alerts, got %v", n, d.received())
	return nil
}
func (d *destination) received() []checksandalerts.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]checksandalerts.Event{}, d.events...)
}

// runWorker runs a daemon's delivery worker until the test ends.
func runWorker(t *testing.T, d *Daemon) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func outboxCount(t *testing.T, d *Daemon, destination string) int {
	t.Helper()
	var n int
	if e := d.store.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE destination=?", destination).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// A joined node's alerts reach the destinations through the hub, which
// saves each under its event ID before accepting it. The node keeps a
// private copy of the fleet settings, with their tokens.
func TestAlertsGoThroughTheHub(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	token := testhelpers.WriteToken(t, "hub-webhook-token-1")
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL, BearerTokenFile: token}, TimeLimitPerDeliveryAttempt: "1m"}})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	info, e := os.Stat(filepath.Join(node.config.StateDir, fleet.FleetSettingsFile))
	if e != nil || info.Mode().Perm() != 0600 || node.fleetSettings == nil || node.fleetSettings.FileContentsByDestination["ops"]["bearer_token_file"] != "hub-webhook-token-1" || node.link.LastError != "" {
		t.Fatal("settings copy", info, e, node.fleetSettings, node.link)
	}
	runWorker(t, node)
	runWorker(t, hub)
	setMaintenance(t, node, "app", "10m")
	got := dest.wait(t, 1)[0]
	if got.Type != "maintenance_on" || got.Node != "app-1" || dest.auth[0] != "Bearer hub-webhook-token-1" {
		t.Fatal(got, dest.auth)
	}
	intake := func() int {
		var n int
		hub.store.DB.QueryRow("SELECT COUNT(*) FROM intake WHERE node='app-1' AND id=?", got.ID).Scan(&n)
		return n
	}
	if intake() != 1 {
		t.Fatal("the alert did not come through the hub")
	}
	// A retry after a lost reply is acknowledged without a second entry.
	sender := fleet.HubSender{Code: code, Path: filepath.Join(node.config.StateDir, fleet.CredentialFile), Node: "app-1"}
	if e := sender.Send(context.Background(), got); e != nil || intake() != 1 {
		t.Fatal("repeated alert", e, intake())
	}
	time.Sleep(500 * time.Millisecond)
	if len(dest.received()) != 1 {
		t.Fatal("a repeated alert was delivered twice", dest.received())
	}
}

// The hub's own alerts pass through the same alert grouping step.
func TestHubAlertsPassThroughRollups(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	hub, _, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1m"}})
	runWorker(t, hub)
	setMaintenance(t, hub, "app", "10m")
	got := dest.wait(t, 1)[0]
	var n int
	hub.store.DB.QueryRow("SELECT COUNT(*) FROM intake WHERE node=? AND id=?", hub.store.Node, got.ID).Scan(&n)
	if got.Type != "maintenance_on" || got.Node != hub.store.Node || n != 1 {
		t.Fatal(got, n)
	}
}

// An alert the hub does not accept goes directly to the destinations in the
// node's copy of the settings, with the copy's token. A node without the
// copy can deliver only through the hub.
func TestAlertsFallBackToDirectDelivery(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	token := testhelpers.WriteToken(t, "hub-webhook-token-1")
	_, code, srv := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL, BearerTokenFile: token}, TimeLimitPerDeliveryAttempt: "1m"}})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	if node.fleetSettings == nil || node.fleetSettings.Fleet != config.DefaultFleetTimings {
		t.Fatal("the settings copy lacks the hub's fleet timings", node.fleetSettings)
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms" // below what configuration allows
	newcomer := joinedNode(t, code, "app-2")
	srv.Close()
	os.Remove(token)
	setMaintenance(t, node, "app", "10m")
	setMaintenance(t, newcomer, "app", "10m")
	queued := queuedEvents(t, node)
	runWorker(t, node)
	runWorker(t, newcomer)
	got := dest.wait(t, 1)[0]
	if got.ID != queued[0].ID || got.Node != "app-1" || dest.auth[0] != "Bearer hub-webhook-token-1" {
		t.Fatal("direct delivery", got, dest.auth)
	}
	if outboxCount(t, node, config.HubDestination) != 0 {
		t.Fatal("the alert stayed queued for the hub")
	}
	time.Sleep(time.Second)
	if len(dest.received()) != 1 || outboxCount(t, newcomer, config.HubDestination) != 1 {
		t.Fatal("a node without the fleet settings delivered directly", dest.received())
	}
}

// happenedAtTheOutboxTime is, for a test of Store.FallBack, when an alert
// happened: at the time that the outbox keeps of it, on the wall clock.
func happenedAtTheOutboxTime(_ int64, _ string, at time.Time) time.Time { return at }

// Fallback waits for a failed attempt to reach the hub and send_alert_without_hub_after_waiting_for from
// when the alert happened, and a check's later alerts follow an earlier
// one directly while it is queued there, so they never pass it.
func TestFallBackKeepsEachConditionInOrder(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	dests := map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://a.example.com"}, TimeLimitPerDeliveryAttempt: "1s"}, "b": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://b.example.com"}, TimeLimitPerDeliveryAttempt: "1s"}}
	now := time.Now()
	first := checksandalerts.Event{ID: incidents.ID(), CheckGroup: "web", Check: "http", Type: "failed", AlertSeverity: "warn", At: now.Add(-time.Minute), Notify: true}
	second := checksandalerts.Event{ID: incidents.ID(), CheckGroup: "web", Check: "http", Type: "recovered", AlertSeverity: "warn", At: now, Notify: true}
	other := checksandalerts.Event{ID: incidents.ID(), CheckGroup: "db", Check: "tcp", Type: "failed", AlertSeverity: "warn", At: now, Notify: true}
	if e = s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{first, second, other}, store.AlertRoute{Joined: true, Destinations: dests}); e != nil {
		t.Fatal(e)
	}
	fall := func(hubFailed bool) *store.FallenBackAlert {
		t.Helper()
		moved, e := s.FallBack(dests, nil, 30*time.Second, hubFailed, now, happenedAtTheOutboxTime)
		if e != nil {
			t.Fatal(e)
		}
		return moved
	}
	if fall(false) != nil {
		t.Fatal("fell back before an attempt to reach the hub failed")
	}
	old, next, none := fall(true), fall(true), fall(true)
	if old == nil || old.Alert.ID != first.ID || old.FollowsEarlierAlertOfTheSameCheck || next == nil || next.Alert.ID != second.ID || !next.FollowsEarlierAlertOfTheSameCheck || none != nil {
		t.Fatal("expected the old alert, then its check's next one behind it, to fall back, and nothing else", old, next, none)
	}
	rows, _ := s.DB.Query("SELECT destination, json_extract(event,'$.id') FROM outbox ORDER BY seq")
	defer rows.Close()
	order := []string{}
	for rows.Next() {
		var dest, id string
		rows.Scan(&dest, &id)
		order = append(order, dest+" "+map[string]string{first.ID: "first", second.ID: "second", other.ID: "other"}[id])
	}
	if strings.Join(order, ", ") != "hub other, a first, b first, a second, b second" {
		t.Fatal(order)
	}
}

// Once an attempt to reach the hub has failed, every queued alert older than
// send_alert_without_hub_after_waiting_for falls back at once, rather than after an attempt of its own:
// against a hub that drops packets each attempt takes its whole timeout.
//
// The test reads the standard logger, which the whole test process shares,
// so it does not run in parallel with tests that log.
func TestFallBackDoesNotWaitForEachAttempt(t *testing.T) {
	dest := captureAlerts(t)
	_, code, srv := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1m"}})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms" // below what configuration allows
	srv.Close()
	for _, check := range []string{"a", "b", "c"} {
		node.mu.Lock()
		e := node.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: check, Check: "http", Type: "failed", At: time.Now().Add(-time.Minute), Notify: true}})
		node.mu.Unlock()
		if e != nil {
			t.Fatal(e)
		}
	}
	node.mu.Lock()
	node.latestHubAttemptError = errors.New("dial tcp 192.0.2.10:18445: connect: connection refused")
	node.mu.Unlock()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	now := time.Now()
	for range 3 {
		node.fallBack(now)
	}
	if outboxCount(t, node, config.HubDestination) != 0 || outboxCount(t, node, "ops") != 3 {
		t.Fatal("queued alerts waited for attempts of their own", outboxCount(t, node, config.HubDestination))
	}
	// Each alert's log line names the alert and the error of the hub's
	// latest attempt, which is gone from the queue once the alert moved.
	if !strings.Contains(logged.String(), "the failed alert of a/http goes directly to the alert destinations, because the latest attempt to deliver an alert to the hub failed: dial tcp 192.0.2.10:18445: connect: connection refused\n") ||
		strings.Count(logged.String(), "goes directly") != 3 {
		t.Fatal(logged.String())
	}
}

// When an alert falls back to the destinations, a later alert of the same
// check that waits for the hub falls back in the same step. The delivery
// worker reads the queue right after the fallback step: a hub that has
// just come back would otherwise get the later alert, such as a recovery,
// and send it on before the destinations have the failure that fell back.
func TestALaterAlertOfACheckFallsBackWithItsEarlierOne(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1m"}})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	// The check failed a minute ago, longer than
	// send_alert_without_hub_after_waiting_for, and has just recovered.
	st := &incidents.CheckState{CheckGroup: "app", Check: "shell", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	timing := checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	failed := st.Observe(checksandalerts.Observation{Status: "failed", Message: "down", At: time.Now().Add(-time.Minute)}, timing, false)
	recovered := st.Observe(checksandalerts.Observation{Status: "ok", Message: "up", At: time.Now()}, timing, false)
	if len(failed) != 1 || len(recovered) != 1 {
		t.Fatal(failed, recovered)
	}
	node.mu.Lock()
	e := node.record(append(failed, recovered...))
	// The node's latest attempt to reach the hub failed, and the hub is
	// back now.
	node.latestHubAttemptError = errors.New("the hub did not answer")
	node.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	runWorker(t, node)
	testhelpers.WaitFor(t, "an empty queue on the node", func() bool { return len(queuedEvents(t, node)) == 0 })
	var atTheHub int
	if e = hub.store.DB.QueryRow("SELECT COUNT(*) FROM intake WHERE id=?", recovered[0].ID).Scan(&atTheHub); e != nil || atTheHub != 0 {
		t.Fatal("the hub got the recovery while its failure went to the destinations directly", atTheHub, e)
	}
	if got := dest.wait(t, 2); got[0].ID != failed[0].ID || got[1].ID != recovered[0].ID {
		t.Fatalf("the destination got %s and %s, not the failure and then the recovery", got[0].Type, got[1].Type)
	}
}

// A reminder falls back only to the destination it names.
func TestReminderFallsBackToItsDestination(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	dests := map[string]checksandalerts.Destination{"a": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://a.example.com"}, TimeLimitPerDeliveryAttempt: "1s"}, "b": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://b.example.com"}, TimeLimitPerDeliveryAttempt: "1s", RemindEvery: "1h"}}
	reminder := checksandalerts.Event{ID: incidents.ID(), CheckGroup: "web", Check: "http", Type: "reminder", AlertSeverity: "warn", Destination: "b", At: time.Now().Add(-time.Minute), Notify: true}
	if e = s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{reminder}, store.AlertRoute{Joined: true, Destinations: dests}); e != nil {
		t.Fatal(e)
	}
	if moved, e := s.FallBack(dests, nil, 30*time.Second, true, time.Now(), happenedAtTheOutboxTime); moved == nil || e != nil {
		t.Fatal("the reminder did not fall back", e)
	}
	var destinations string
	s.DB.QueryRow("SELECT group_concat(destination) FROM outbox").Scan(&destinations)
	if destinations != "b" {
		t.Fatal("the reminder fell back to", destinations)
	}
}

// A joined node sends its reminders through the hub, with remind_every from
// its copy of the fleet settings. The hub delivers each only to the
// destination it names and rolls up matching reminders from other nodes.
func TestRemindersGoThroughTheHub(t *testing.T) {
	t.Parallel()
	oncall, ops := captureAlerts(t), captureAlerts(t)
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{
		"oncall": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: oncall.URL}, TimeLimitPerDeliveryAttempt: "1s", RemindEvery: "1h"},
		"ops":    {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: ops.URL}, TimeLimitPerDeliveryAttempt: "1s"},
	})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	opened := time.Now().UTC().Add(-time.Hour) // the reminder is not late
	node.mu.Lock()
	st := node.snapshot.Checks["app/shell"]
	events := st.Observe(checksandalerts.Observation{Status: "failed", Message: "expected 200, received 503", At: opened}, checksandalerts.Timing{}, false)
	if e := node.record(events); e != nil {
		t.Fatal(e)
	}
	if e := node.record(node.remind(opened.Add(time.Hour))); e != nil {
		t.Fatal(e)
	}
	node.mu.Unlock()
	if outboxCount(t, node, config.HubDestination) != 2 {
		t.Fatal("the failure and its reminder are not queued for the hub", queued(t, node))
	}
	runWorker(t, node)
	// The hub's grouping window stays open until the test ends it below, so
	// that the reminders of two nodes become one message however long their
	// joins and connections take.
	hub.alertGroups.Window = time.Hour
	runWorker(t, hub)
	got := oncall.wait(t, 2)
	want := "app/shell still failing since " + opened.Format("15:04 UTC") + "; latest result: expected 200, received 503"
	if got[0].Type != "failed" || got[1].Type != "reminder" || got[1].Destination != "oncall" || got[1].Node != "app-1" || got[1].Message != want || got[1].Incident != got[0].Incident {
		t.Fatalf("delivered %+v", got)
	}
	senders := map[string]fleet.HubSender{}
	for _, name := range []string{"app-2", "app-3"} {
		s := joinedNode(t, code, name)
		s.report(context.Background())
		senders[name] = fleet.HubSender{Code: code, Path: filepath.Join(s.config.StateDir, fleet.CredentialFile), Node: name}
	}
	// Both reminders wait in the hub, behind app-1's reminder in the same
	// window, until the window ends.
	for _, name := range []string{"app-2", "app-3"} {
		if e := senders[name].Send(context.Background(), withFleetIncidentKey(checksandalerts.Event{ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "reminder", AlertSeverity: "warn", Destination: "oncall", At: time.Now().UTC(), Message: "m"})); e != nil {
			t.Fatal(e)
		}
	}
	hub.mu.Lock()
	hub.alertGroups.Window = 0
	hub.mu.Unlock()
	// The hub names the nodes in the order in which their reminders came.
	combined := oncall.wait(t, 3)[2]
	if combined.Message != "app/shell still failing on 2 nodes: app-2, app-3" || combined.Destination != "oncall" || combined.NodeCount != 2 {
		t.Fatalf("combined %+v", combined)
	}
	time.Sleep(time.Second)
	if len(oncall.received()) != 3 || len(ops.received()) != 1 || ops.received()[0].Type != "failed" {
		t.Fatal("a reminder reached another destination", oncall.received(), ops.received())
	}
}

// A changed version in a report reply makes the node fetch the settings
// again. Destination identity rules apply to the copy as to alert_destinations.yml.
func TestSettingsCopyFollowsTheHub(t *testing.T) {
	t.Parallel()
	first, second := captureAlerts(t), captureAlerts(t)
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: first.URL}, TimeLimitPerDeliveryAttempt: "1m"}})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	version := node.fleetSettings.Version
	node.mu.Lock()
	e := node.store.Save(node.snapshot, []checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), Type: "failed", At: time.Now(), Notify: true}}, store.AlertRoute{Destinations: node.fleetSettings.Destinations})
	node.mu.Unlock()
	if e != nil || outboxCount(t, node, "ops") != 1 {
		t.Fatal("queued direct delivery", e)
	}
	b, _ := json.Marshal(config.Alerts{Destinations: map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: second.URL}, TimeLimitPerDeliveryAttempt: "1m"}}})
	os.WriteFile(filepath.Join(hub.path, "alert_destinations.yml"), b, 0600)
	if _, e = hub.reload(); e != nil {
		t.Fatal(e)
	}
	node.report(context.Background())
	if node.link.LastError != "" || node.fleetSettings.Version == version || node.fleetSettings.Destinations["ops"].Inputs.(*checksandalerts.Webhook).URL != second.URL {
		t.Fatal("settings did not follow the hub", node.link, node.fleetSettings)
	}
	stored := fleet.ReadFleetSettings(filepath.Join(node.config.StateDir, fleet.FleetSettingsFile), node.config.Modules)
	if stored == nil || stored.Version != node.fleetSettings.Version || outboxCount(t, node, "ops") != 0 {
		t.Fatal("stored copy or cancellation", stored)
	}
	events, _ := node.store.Events(5)
	if events[0].Type != "delivery_cancelled" {
		t.Fatal(events)
	}
}

// Alerts are untrusted input: authenticated, strictly decoded, and always in
// the sender's own name.
func TestAlertIntakeIsAuthenticatedAndStrict(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	cred := fleet.ReadCredential(filepath.Join(node.config.StateDir, fleet.CredentialFile)).Credential
	post := func(credential, body string) int {
		r := httptest.NewRequest("POST", "https://hub.test/v1/alerts", bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		hub.hubRoutes().ServeHTTP(w, r)
		return w.Code
	}
	id := incidents.ID()
	key := checksandalerts.FleetIncidentKey("web/http failed")
	valid := `{"id":"` + id + `","fleet_incident_key":"` + key + `","node":"app-2","check_group":"web","check":"http","type":"failed","alert_severity":"warn","at":"2026-01-02T03:04:05Z","message":"down"`
	for body, want := range map[string]int{
		valid + `,"extra":1}`: 400,
		strings.Replace(valid, `,"alert_severity":"warn"`, ``, 1) + `}`:                                                 400,
		strings.Replace(valid, `"warn"`, `"loud"`, 1) + `}`:                                                             400,
		strings.Replace(valid, `"failed"`, `"recovered"`, 1) + `,"alert_severities_sent_for_incident":["warn","warn"]}`: 400,
		strings.Replace(valid, `"failed"`, `"node_joined"`, 1) + `}`:                                                    400,
		strings.Replace(valid, `"failed"`, `"rebooted"`, 1) + `}`:                                                       400,
		valid + `,"nodes":["app-3"]}`:                                                                                   400,
		valid + `,"maintenance":{"until":"2026-01-02T03:04:05Z"}}`:                                                      400,
		valid + `,"destination":"ops"}`:                                                                                 400,
		strings.Replace(valid, key, "web/http failed", 1) + `}`:                                                         400,
		strings.Replace(valid, `"fleet_incident_key":"`+key+`",`, ``, 1) + `}`:                                          400,
		strings.Replace(valid, `"failed"`, `"reminder"`, 1) + `}`:                                                       400,
		`{"id":"` + strings.Repeat("x", fleet.AlertLimit) + `"}`:                                                        400,
	} {
		if status := post(cred, body); status != want {
			t.Errorf("%.80s: got %d, want %d", body, status, want)
		}
	}
	if status := post(strings.Repeat("a", 64), valid+`}`); status != 401 {
		t.Fatal(status)
	}
	if status := post(cred, valid+`}`); status != 200 {
		t.Fatal(status)
	}
	var owner, eventNode string
	hub.store.DB.QueryRow("SELECT node, json_extract(event,'$.node') FROM intake WHERE id=?", id).Scan(&owner, &eventNode)
	if owner != "app-1" || eventNode != "app-1" {
		t.Fatal("an alert was saved in another node's name", owner, eventNode)
	}
}

// An alert the hub cannot save is not accepted, so the node falls back.
func TestHubRefusesAlertsItCannotSave(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	if _, e := hub.store.DB.Exec("CREATE TRIGGER reject_intake BEFORE INSERT ON intake BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); e != nil {
		t.Fatal(e)
	}
	sender := fleet.HubSender{Code: code, Path: filepath.Join(node.config.StateDir, fleet.CredentialFile), Node: "app-1"}
	event := withFleetIncidentKey(checksandalerts.Event{ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", At: time.Now()})
	if e := sender.Send(context.Background(), event); e == nil || !strings.Contains(e.Error(), "503") {
		t.Fatal(e)
	}
	hub.mu.Lock()
	problems := hub.storage.Problems()
	hub.mu.Unlock()
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "alert intake: ") || !strings.Contains(problems[0], "fixture storage failure") {
		t.Fatal(problems)
	}
	hub.store.DB.Exec("DROP TRIGGER reject_intake")
	if e := sender.Send(context.Background(), event); e != nil {
		t.Fatal(e)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if problems := hub.storage.Problems(); len(problems) != 0 {
		t.Fatal("storage problem stayed after a saved alert", problems)
	}
}

// Matching alerts from several nodes after the first become one message;
// a check alert delivered late carries the node's current state.
func TestRolledUpAndLateAlertsThroughTheHub(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1s"}})
	// The hub's grouping window stays open until the test ends it below, so
	// that the alerts of two nodes become one message however long their
	// connections take.
	hub.alertGroups.Window = time.Hour
	runWorker(t, hub)
	senders := map[string]fleet.HubSender{}
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		s := joinedNode(t, code, name)
		s.report(context.Background())
		senders[name] = fleet.HubSender{Code: code, Path: filepath.Join(s.config.StateDir, fleet.CredentialFile), Node: name}
	}
	send := func(name, kind string, at time.Time) {
		t.Helper()
		if e := senders[name].Send(context.Background(), withFleetIncidentKey(checksandalerts.Event{ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: kind, AlertSeverity: "warn", At: at, Message: "m"})); e != nil {
			t.Fatal(e)
		}
	}
	now := time.Now().UTC()
	send("app-1", "failed", now)
	if got := dest.wait(t, 1)[0]; got.Node != "app-1" || got.Nodes != nil || got.Title != "app-1: app/shell failed" || got.TitleWithAlertSeverity != "Warning: app-1: app/shell failed" || got.Body != "m" {
		t.Fatal("the first alert did not go out alone, with its title and body", got)
	}
	send("app-2", "failed", now)
	send("app-3", "failed", now)
	send("app-1", "recovered", now.Add(-5*time.Minute))
	hub.mu.Lock()
	hub.alertGroups.Window = 0
	hub.mu.Unlock()
	got := dest.wait(t, 3)
	var combined, late checksandalerts.Event
	for _, e := range got[1:] {
		if e.Type == "failed" {
			combined = e
		} else {
			late = e
		}
	}
	if !reflect.DeepEqual(combined.Nodes, []string{"app-2", "app-3"}) || combined.NodeCount != 2 || combined.Message != "app/shell failed on 2 nodes: app-2, app-3" {
		t.Fatalf("combined %+v", combined)
	}
	// A grouped message has a title and a body of its own: the title counts
	// the nodes, and the body names them and adds the first node's alert.
	if combined.Title != "app/shell failed on 2 nodes" || combined.TitleWithAlertSeverity != "Warning: app/shell failed on 2 nodes" || combined.Body != "app/shell failed on 2 nodes: app-2, app-3\nfrom app-2:\n  m" {
		t.Fatalf("combined %q %q", combined.Title, combined.Body)
	}
	if !strings.HasSuffix(late.Message, ". Now app-1 shows app/shell as ok)") || !strings.Contains(late.Message, "(alert delivered late; the event happened at ") {
		t.Fatal("late alert", late.Message)
	}
	// The body says the same, on a line of its own; the title stays.
	if late.Title != "app-1: app/shell is ok again" || late.TitleWithAlertSeverity != "Resolved warning: app-1: app/shell is ok again" || !strings.HasPrefix(late.Body, "m\nalert delivered late; the event happened at ") || !strings.HasSuffix(late.Body, ". Now app-1 shows app/shell as ok") {
		t.Fatalf("late %q %q", late.Title, late.Body)
	}
	time.Sleep(time.Second)
	if n := len(dest.received()); n != 3 {
		t.Fatal("expected three messages", dest.received())
	}
}

// The hub announces a maintenance end ten minutes after the maintenance
// ended, so the announcement says when it ended, that alerts resumed then,
// and why it comes later. A group's end is its last node's end.
func TestAMaintenanceEndSaysWhenItEndedAndWhyTheHubWaited(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1s"}})
	runWorker(t, hub)
	senders := map[string]fleet.HubSender{}
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		s := joinedNode(t, code, name)
		s.report(context.Background())
		senders[name] = fleet.HubSender{Code: code, Path: filepath.Join(s.config.StateDir, fleet.CredentialFile), Node: name}
	}
	now := time.Now().UTC()
	every := []string{checksandalerts.EveryCheckGroup}
	send := func(name string, event checksandalerts.Event) {
		t.Helper()
		if e := senders[name].Send(context.Background(), event); e != nil {
			t.Fatal(e)
		}
	}
	// endHeldGroup waits until the hub holds the group's end, then moves the
	// start of its ten quiet minutes into the past.
	endHeldGroup := func(w checksandalerts.Window) {
		t.Helper()
		g := w.Reason + "\x00" + w.User
		for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); ; time.Sleep(20 * time.Millisecond) {
			hub.mu.Lock()
			_, held := hub.alertGroups.Quiet[g]
			if held {
				hub.alertGroups.Quiet[g] = now.Add(-time.Hour)
			}
			hub.mu.Unlock()
			if held {
				return
			}
			if time.Now().After(end) {
				t.Fatal("the hub never held the maintenance end")
			}
		}
	}
	wait := " The MMWatchdog fleet hub waits 10 minutes after maintenance ends on every affected node before sending the end alert. The wait combines maintenance alerts from a deployment across several nodes into one start alert and one end alert"

	deploy := checksandalerts.Window{Until: now.Add(time.Hour), Reason: "deploy 42", User: "alice"}
	for _, name := range []string{"app-1", "app-2"} {
		send(name, incidents.MaintenanceEvent("maintenance_on", every, deploy, now.Add(-20*time.Minute), "maintenance "+deploy.Describe()))
	}
	dest.wait(t, 1)
	send("app-1", incidents.MaintenanceEvent("maintenance_off", every, deploy, now.Add(-15*time.Minute), "maintenance turned off by alice: deploy 42"))
	send("app-2", incidents.MaintenanceEvent("maintenance_off", every, deploy, now.Add(-12*time.Minute), "maintenance turned off by alice: deploy 42"))
	endHeldGroup(deploy)
	last := checksandalerts.ClockTime(now.Add(-12*time.Minute), time.Now())
	got := dest.wait(t, 2)[1]
	if !got.At.Equal(now.Add(-12*time.Minute)) || got.Message != "maintenance turned off on 2 nodes: app-1, app-2 (by alice: deploy 42) (maintenance ended on the last node in the alert at "+last+". Alerts on each node resumed when the node's maintenance ended."+wait+")" {
		t.Fatalf("group end %v %q", got.At, got.Message)
	}
	if !strings.HasSuffix(got.Body, "\nmaintenance ended on the last node in the alert at "+last+". Alerts on each node resumed when the node's maintenance ended."+wait) {
		t.Fatalf("group end body %q", got.Body)
	}

	reboot := checksandalerts.Window{Until: now.Add(time.Hour), Reason: "reboot", User: "bob"}
	send("app-3", incidents.MaintenanceEvent("maintenance_on", every, reboot, now.Add(-14*time.Minute), "maintenance "+reboot.Describe()))
	dest.wait(t, 3)
	send("app-3", incidents.MaintenanceEvent("maintenance_off", every, reboot, now.Add(-11*time.Minute), "maintenance turned off by bob: reboot"))
	endHeldGroup(reboot)
	ended := checksandalerts.ClockTime(now.Add(-11*time.Minute), time.Now())
	got = dest.wait(t, 4)[3]
	if got.Title != "app-3: maintenance off for the whole node" || got.Message != "maintenance turned off by bob: reboot (maintenance ended at "+ended+", and alerts resumed then."+wait+")" {
		t.Fatalf("one node's end %q %q", got.Title, got.Message)
	}
}

// A joined node's rebooted alert goes through the hub, where matching
// alerts from other nodes roll up into one message that is never marked
// late.
func TestRebootedAlertsThroughTheHub(t *testing.T) {
	boot := "boot-1"
	setBoot(t, &boot, time.Now().UTC())
	dest := captureAlerts(t)
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1s"}})
	// The hub's grouping window stays open until the test ends it below.
	hub.alertGroups.Window = time.Hour
	runWorker(t, hub)
	senders := map[string]fleet.HubSender{}
	var node *Daemon
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		node = joinedNode(t, code, name)
		node.report(context.Background())
		senders[name] = fleet.HubSender{Code: code, Path: filepath.Join(node.config.StateDir, fleet.CredentialFile), Node: name}
	}
	node.Close()
	boot = "boot-2"
	node, e := NewDaemon(node.path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { node.Close() })
	node.store.Node, node.link.Node = "app-3", "app-3"
	if got := queued(t, node); !reflect.DeepEqual(got, []string{"rebooted"}) || outboxCount(t, node, config.HubDestination) != 1 {
		t.Fatal("the reboot alert is not queued for the hub", got)
	}
	runWorker(t, node)
	if got := dest.wait(t, 1)[0]; got.Type != "rebooted" || got.Node != "app-3" || got.CheckGroup != "" || !strings.Contains(got.Message, " rebooted at ") {
		t.Fatal("the reboot alert did not come through the hub", got)
	}
	for i, name := range []string{"app-1", "app-2"} {
		// app-1's alert happened long ago; a reboot alert names no check
		// whose present state could be added.
		at := time.Now().UTC().Add(time.Duration(i-1) * 5 * time.Minute)
		if e := senders[name].Send(context.Background(), checksandalerts.Event{ID: incidents.ID(), Type: "rebooted", AlertSeverity: "info", At: at, Message: name + " rebooted"}); e != nil {
			t.Fatal(e)
		}
	}
	hub.mu.Lock()
	hub.alertGroups.Window = 0
	hub.mu.Unlock()
	combined := dest.wait(t, 2)[1]
	if combined.Message != "2 nodes rebooted: app-1, app-2" || combined.NodeCount != 2 || combined.CheckGroup != "" || combined.Type != "rebooted" {
		t.Fatalf("combined %+v", combined)
	}
}

func TestRollupPlan(t *testing.T) {
	t.Parallel()
	r := fleet.AlertGroupingState{Sent: map[string]time.Time{}, Quiet: map[string]time.Time{}, Window: fleet.AlertGroupingWindow}
	row := func(seq int64, node, kind, subject, group string) store.IntakeRow {
		return store.IntakeRow{Seq: seq, Node: node, Kind: kind, Subject: subject, Group: group}
	}
	messages := func(p store.AlertGroupingPlan) [][]int64 {
		out := [][]int64{}
		for _, m := range p.Messages {
			out = append(out, m.Members)
		}
		return out
	}
	t0 := time.Unix(1_000_000, 0)
	p := r.Plan([]store.IntakeRow{row(1, "app-1", "failed", "web\x00http", "")}, nil, t0)
	if !reflect.DeepEqual(messages(p), [][]int64{{1}}) {
		t.Fatal("the first alert of a kind goes out at once", p)
	}
	r.Sent[p.Sent[0]] = t0
	rows := []store.IntakeRow{
		row(2, "app-2", "failed", "web\x00http", ""),
		row(3, "app-3", "failed", "web\x00http", ""),
		row(4, "app-2", "recovered", "web\x00http", ""), // after app-2's failure
		row(5, "app-4", "failed", "db\x00tcp", ""),
	}
	if p = r.Plan(rows, nil, t0.Add(30*time.Second)); !reflect.DeepEqual(messages(p), [][]int64{{5}}) {
		t.Fatal("matching alerts within the window wait; an alert that waits holds its check's later ones", messages(p))
	}
	if p = r.Plan(rows[:3], nil, t0.Add(time.Minute)); !reflect.DeepEqual(messages(p), [][]int64{{2, 3}, {4}}) {
		t.Fatal("combined message, then the next alert for its check", messages(p))
	}
	// Reminders roll up only with reminders for the same destination.
	reminder := func(seq int64, node, destination string) store.IntakeRow {
		return store.IntakeRow{Seq: seq, Node: node, Kind: "reminder", Subject: "web\x00http", Destination: destination}
	}
	rows = []store.IntakeRow{reminder(6, "app-1", "oncall"), reminder(7, "app-2", "team"), reminder(8, "app-3", "oncall")}
	if p = r.Plan(rows, nil, t0.Add(2*time.Minute)); !reflect.DeepEqual(messages(p), [][]int64{{6, 8}, {7}}) {
		t.Fatal("reminders for different destinations rolled up together", messages(p))
	}
	// A maintenance group opens with one message; later starts join it
	// quietly; its end is announced once no node has been in maintenance
	// for groupQuiet.
	g := "deploy 42\x00alice"
	r = fleet.AlertGroupingState{Sent: map[string]time.Time{}, Quiet: map[string]time.Time{}, Window: fleet.AlertGroupingWindow}
	p = r.Plan([]store.IntakeRow{row(10, "app-1", "maintenance_on", checksandalerts.EveryCheckGroup+"\x00", g), row(11, "app-2", "maintenance_on", checksandalerts.EveryCheckGroup+"\x00", g)}, map[string]bool{g: true}, t0)
	if len(p.Messages) != 1 || !p.Messages[0].Hold || !reflect.DeepEqual(p.Messages[0].Members, []int64{10, 11}) {
		t.Fatal("group start", p)
	}
	held := func(seqs ...int64) []store.IntakeRow {
		out := []store.IntakeRow{}
		for _, s := range seqs {
			out = append(out, store.IntakeRow{Seq: s, Node: "app", Kind: "maintenance_on", Subject: checksandalerts.EveryCheckGroup + "\x00", Group: g, Held: true})
		}
		return out
	}
	rows = append(held(10, 11), row(12, "app-3", "maintenance_on", checksandalerts.EveryCheckGroup+"\x00", g), row(13, "app-1", "maintenance_off", checksandalerts.EveryCheckGroup+"\x00", g))
	if p = r.Plan(rows, map[string]bool{g: true}, t0.Add(2*time.Minute)); len(p.Messages) != 0 || !reflect.DeepEqual(p.Done, []int64{12}) || !reflect.DeepEqual(p.Hold, []int64{13}) {
		t.Fatal("a start in an open group, and an end, wait quietly", p)
	}
	ended := store.IntakeRow{Seq: 13, Node: "app-1", Kind: "maintenance_off", Subject: checksandalerts.EveryCheckGroup + "\x00", Group: g, Held: true}
	rows = append(held(10, 11), ended)
	for _, at := range []time.Duration{5, 14} {
		if p = r.Plan(rows, nil, t0.Add(at*time.Minute)); len(p.Messages) != 0 {
			t.Fatal("the group ended before it was quiet for groupQuiet", at, p)
		}
	}
	if p = r.Plan(rows, map[string]bool{g: true}, t0.Add(14*time.Minute)); len(p.Messages) != 0 || len(r.Quiet) != 0 {
		t.Fatal("a node back in maintenance keeps the group open", p)
	}
	r.Plan(rows, nil, t0.Add(20*time.Minute))
	p = r.Plan(rows, nil, t0.Add(30*time.Minute))
	if !reflect.DeepEqual(messages(p), [][]int64{{13}}) || !reflect.DeepEqual(p.Done, []int64{10, 11, 13}) || !reflect.DeepEqual(p.Closed, []string{g}) {
		t.Fatal("group end", p)
	}
}

func TestCombinedMessages(t *testing.T) {
	t.Parallel()
	events := []checksandalerts.Event{}
	for i := 0; i < 25; i++ {
		events = append(events, checksandalerts.Event{ID: incidents.ID(), Node: "app-" + string(rune('a'+i)), CheckGroup: "web", Check: "http", Type: "cannot_check", AlertSeverity: "warn", At: time.Unix(int64(100-i), 0)})
	}
	m := store.Combine(events, "util")
	if m.NodeCount != 25 || len(m.Nodes) != 20 || m.At != time.Unix(76, 0) || !strings.HasPrefix(m.Message, "MMWatchdog cannot check web/http on 25 nodes: app-a, app-b") || !strings.HasSuffix(m.Message, "app-t, and 5 more") {
		t.Fatal(m.Message, m)
	}
	w := &checksandalerts.Window{Reason: "deploy 42", User: "alice"}
	m = store.Combine([]checksandalerts.Event{{Node: "app-1", CheckGroup: checksandalerts.EveryCheckGroup, Type: "maintenance_off", Maintenance: w}, {Node: "app-2", CheckGroup: checksandalerts.EveryCheckGroup, Type: "maintenance_off", Maintenance: w}}, "util")
	if m.Message != "maintenance turned off on 2 nodes: app-1, app-2 (by alice: deploy 42)" || m.CheckGroup != checksandalerts.EveryCheckGroup {
		t.Fatal(m.Message)
	}
	m = store.Combine([]checksandalerts.Event{{Node: "app-1", CheckGroup: checksandalerts.EveryCheckGroup, Type: "maintenance_on", Maintenance: w}, {Node: "app-2", CheckGroup: checksandalerts.EveryCheckGroup, Type: "maintenance_on", Maintenance: w}}, "util")
	if m.Message != "maintenance started on 2 nodes: app-1, app-2 (by alice: deploy 42)" {
		t.Fatal(m.Message)
	}
	// An operator's reason that mentions "expired" does not change the cause.
	w = &checksandalerts.Window{Reason: "renew expired certificate", User: "alice"}
	ended := []checksandalerts.Event{{Node: "app-1", CheckGroup: checksandalerts.EveryCheckGroup, Type: "maintenance_off", Maintenance: w, Message: "maintenance turned off by alice: renew expired certificate"}, {Node: "app-2", CheckGroup: checksandalerts.EveryCheckGroup, Type: "maintenance_off", Maintenance: w, Message: incidents.MaintenanceExpiredMessage + ": renew expired certificate"}}
	if m = store.Combine(ended, "util"); !strings.HasPrefix(m.Message, "maintenance expired or turned off on 2 nodes") {
		t.Fatal(m.Message)
	}
	unknown := []checksandalerts.Event{{Node: "app-1", CheckGroup: "web", Check: "http", Type: "reminder", Destination: "oncall", Message: checksandalerts.ReminderMessage(checksandalerts.CannotCheckIncidentEvents, "web/http", "14:02 UTC", "token file missing")}, {Node: "app-2", CheckGroup: "web", Check: "http", Type: "reminder", Destination: "oncall", Message: checksandalerts.ReminderMessage(checksandalerts.CannotCheckIncidentEvents, "web/http", "14:03 UTC", "token file missing")}}
	if m = store.Combine(unknown, "util"); m.Message != "MMWatchdog has not been able to run web/http on 2 nodes: app-1, app-2" {
		t.Fatal(m.Message)
	}
	// The grouped reminder is still known as one of a cannot-check incident.
	if m.WriteAlertTexts(unknown, "util"); m.Title != "MMWatchdog still cannot check web/http on 2 nodes" {
		t.Fatal(m.Title)
	}
	failing := store.IntakeRow{Kind: "reminder", Subject: "web\x00http", Destination: "oncall"}
	cannotCheck := failing
	cannotCheck.Group = checksandalerts.CannotCheckIncidentEvents.Failed
	if failing.AlertGroupKey() == cannotCheck.AlertGroupKey() {
		t.Fatal("failure and cannot-check reminders share a grouping key")
	}
	silent := []checksandalerts.Event{{Node: "util", CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: "app-1", Type: "reminder", Destination: "oncall"}, {Node: "util", CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: "app-2", Type: "reminder", Destination: "oncall"}}
	if m = store.Combine(silent, "util"); m.Message != "2 nodes still not reporting: app-1, app-2" || m.Destination != "oncall" {
		t.Fatal(m.Message, m.Destination)
	}
}
