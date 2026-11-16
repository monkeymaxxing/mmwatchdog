package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

func TestDestinationHealthNoticesWaitTenMinutesAndRecoverOnce(t *testing.T) {
	t.Parallel()
	d := noticeDaemon(t, map[string]checksandalerts.Destination{
		"broken": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com/broken"}, AlertSeverities: []string{"warn"}},
		"other":  {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com/other"}, AlertSeverities: []string{}},
	})
	now := d.now()
	event := checksandalerts.Event{ID: incidents.ID(), Type: "notice", AlertSeverity: "warn", At: now, Message: "original", Notify: true}
	d.mu.Lock()
	second := event
	second.ID, second.Message = incidents.ID(), "second"
	err := d.record([]checksandalerts.Event{event, second})
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.DB.Exec("UPDATE outbox SET queued_at=?", now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	pending, err := d.store.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	if _, err = d.store.Delivered(pending[0], errors.New("address refused")); err != nil {
		t.Fatal(err)
	}
	d.checkDestinationHealth(now.Add(10*time.Minute - time.Nanosecond))
	if got := outboxCount(t, d, "other"); got != 0 {
		t.Fatal("warned too early", got)
	}
	d.checkDestinationHealth(now.Add(10 * time.Minute))
	// The store stamps newly queued notices with wall time. Put this notice
	// on the test clock before advancing it, so it is not an old backlog.
	if _, err := d.store.DB.Exec("UPDATE outbox SET queued_at=? WHERE destination='other'", now.Add(10*time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	d.checkDestinationHealth(now.Add(11 * time.Minute))
	if got := outboxCount(t, d, "other"); got != 1 {
		t.Fatal("missing or repeated warning", got)
	}
	queued, _ := d.store.Pending()
	for _, delivery := range queued {
		if delivery.Destination == "other" && (delivery.Event.AlertSeverity != "warn" || !strings.Contains(delivery.Event.Body, "address refused")) {
			t.Fatal(delivery)
		}
	}
	if _, err := d.store.Delivered(pending[0], nil); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.destinationSucceeded("broken", pending[0].Target, now.Add(12*time.Minute), "delivered an alert")
	d.mu.Unlock()
	if got := outboxCount(t, d, "other"); got != 1 {
		t.Fatal("partial delivery announced recovery with an old backlog", got)
	}
	remaining, err := d.store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range remaining {
		if delivery.Destination == "broken" {
			if _, err := d.store.Delivered(delivery, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if outboxCount(t, d, "broken") != 0 {
		t.Fatal("remaining alert was not delivered")
	}
	d.mu.Lock()
	d.destinationSucceeded("broken", pending[0].Target, now.Add(12*time.Minute), "delivered an alert")
	d.destinationSucceeded("broken", pending[0].Target, now.Add(12*time.Minute), "delivered an alert")
	d.mu.Unlock()
	if got := outboxCount(t, d, "other"); got != 2 {
		t.Fatal("missing or repeated recovery", got)
	}
	var severity, title string
	if err := d.store.DB.QueryRow("SELECT json_extract(event,'$.alert_severity'),json_extract(event,'$.title') FROM outbox WHERE destination='other' ORDER BY seq DESC LIMIT 1").Scan(&severity, &title); err != nil || severity != "warn" || !strings.Contains(title, "Destination broken works again.") {
		t.Fatal(severity, title, err)
	}
}

func TestDestinationVerificationDailyRetriesAndStatus(t *testing.T) {
	t.Parallel()
	d := noticeDaemon(t, map[string]checksandalerts.Destination{
		"checked": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, AlertSeverities: testhelpers.AllSeverities},
	})
	now := d.started
	var workers sync.WaitGroup
	d.scheduleDestinationVerifications(context.Background(), now, &workers)
	workers.Wait()
	if d.healthOf("checked").verifying || !d.healthOf("checked").NextVerificationAt.Equal(now.Add(24*time.Hour)) {
		t.Fatal("startup ran verification")
	}
	d.mu.Lock()
	delivery, health, ok := d.verificationDelivery("checked")
	d.mu.Unlock()
	if !ok {
		t.Fatal("not reserved")
	}
	d.now = func() time.Time { return now }
	d.finishVerification("checked", delivery, health, checksandalerts.DestinationVerification{CanVerifyWithoutMessage: true, VerificationMessage: "credential refused"})
	if !health.NextVerificationAt.Equal(now.Add(time.Minute)) {
		t.Fatal("no retry", health.NextVerificationAt)
	}
	if got := d.withFailingVerifications(nil, now.Add(10*time.Minute-time.Nanosecond)); len(got) != 0 {
		t.Fatal("failed too early", got)
	}
	d.now = func() time.Time { return now.Add(10 * time.Minute) }
	d.checkDestinationHealth(d.now())
	statusNow := func() (int, int, string) {
		recorder := httptest.NewRecorder()
		d.routes().ServeHTTP(recorder, httptest.NewRequest("GET", "/status", nil))
		var status struct {
			Summary  incidents.StatusSummary `json:"summary"`
			Delivery store.DeliverySummary   `json:"delivery"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil || len(status.Delivery.FailingDestinations) != 1 || status.Delivery.FailingDestinations[0].LatestAttemptError != "credential refused" {
			t.Fatal(recorder.Body.String(), err)
		}
		return status.Summary.ExitCode, len(status.Delivery.FailingDestinations), recorder.Body.String()
	}
	// A failing destination alone is health unknown: exit 3.
	if code, _, body := statusNow(); code != 3 {
		t.Fatal("failing destination without a failed check should exit 3:", body)
	}
	// A failed check takes precedence: exit 1, and the destination still shows.
	d.snapshot.Checks["app/shell"].FailureIncident.Incident = "open-incident"
	d.snapshot.Checks["app/shell"].FailingSince = now
	d.snapshot.Checks["app/shell"].Last = checksandalerts.Observation{Status: "failed", At: now}
	if code, _, body := statusNow(); code != 1 {
		t.Fatal("a failed check takes precedence over a failing destination (exit 1):", body)
	}
	d.finishVerification("checked", delivery, health, checksandalerts.DestinationVerification{CanVerifyWithoutMessage: true, AcceptsAddressAndCredential: true, VerificationMessage: "accepted"})
	if got := d.withFailingVerifications(nil, d.now()); len(got) != 0 {
		t.Fatal("success kept failure", got)
	}
	if !health.NextVerificationAt.Equal(d.now().Add(24 * time.Hour)) {
		t.Fatal("next daily verification", health.NextVerificationAt)
	}
}

func TestReloadIdentityUsesCredentialContents(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	first, second := filepath.Join(path, "token"), filepath.Join(path, "new-token-path")
	for _, file := range []string{first, second} {
		if err := os.WriteFile(file, []byte("a-valid-token-value"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"chat": installTokenModule(t, c, first)}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.mu.Lock()
	err = d.record([]checksandalerts.Event{{ID: incidents.ID(), Type: "notice", AlertSeverity: "warn", At: d.now(), Message: "queued", Notify: true}})
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	destination := c.Alerts.Destinations["chat"]
	destination.Inputs = moduleDestination(t, "token_module", map[string]any{"api_token_file": second}).Inputs
	c.Alerts.Destinations["chat"] = destination
	writeConfig(t, path, c)
	result, err := d.reload()
	if err != nil || len(result.NewOrChangedDestinations) != 0 || outboxCount(t, d, "chat") != 1 {
		t.Fatal("moving equal credential cancelled the queue", result, err)
	}
	if err := os.WriteFile(second, []byte("another-valid-token-value"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = d.reload()
	if err != nil || len(result.NewOrChangedDestinations) != 1 || result.NewOrChangedDestinations[0] != "chat" || outboxCount(t, d, "chat") != 0 {
		t.Fatal("credential rotation kept the old queue", result, err)
	}
}

func TestReloadVerifiesNewOrChangedDestinationsWithoutSendingAMessage(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Add(1) }))
	defer server.Close()
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"chat": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: server.URL}, AlertSeverities: testhelpers.AllSeverities}}
	writeConfig(t, path, c)
	api := httptest.NewServer(d.routes())
	defer api.Close()
	response, err := api.Client().Post(api.URL+"/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result ReloadResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || !result.Reloaded || len(result.NewOrChangedDestinations) != 1 || len(result.DestinationVerifications) != 1 {
		t.Fatal(result, err)
	}
	if received.Load() != 0 || outboxCount(t, d, "chat") != 0 {
		t.Fatal("reload sent a message")
	}
}

func TestRoutingWarningsAndSilentCauseRefusal(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	nobody := []string{}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"chat": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, AlertSeverities: []string{"warn"}}}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Checks: map[string]checksandalerts.NamedCheck{
		"cause": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}, SendAlertsOnlyToDestinations: &nobody}},
		"quiet": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}, NoAlertsOrRecoveryActionsWhileTheseChecksFail: []string{"app/cause"}}},
	}}}
	writeConfig(t, path, c)
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), `the cause "app/cause" sends alerts to no destination while keeping "app/quiet" quiet`) {
		t.Fatal(err)
	}
	quiet := c.CheckGroups["app"].Checks["quiet"]
	quiet.NoAlertsOrRecoveryActionsWhileTheseChecksFail = nil
	c.CheckGroups["app"].Checks["quiet"] = quiet
	writeConfig(t, path, c)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	warnings := loaded.ValidationWarnings("node", false)
	if len(warnings.ChecksWithoutAlertDestination) != 1 || warnings.ChecksWithoutAlertDestination[0].Check != "cause" {
		t.Fatal(warnings)
	}
	// Node validation checks against saved hub settings; config-only does not.
	loaded.JoinFleetHub = &config.JoinFleetHub{}
	named := []string{"chat", "misspelled"}
	cause := loaded.CheckGroups["app"].Checks["cause"]
	cause.SendAlertsOnlyToDestinations = &named
	loaded.CheckGroups["app"].Checks["cause"] = cause
	settings := fleet.FleetSettings{Destinations: loaded.Alerts.Destinations}
	warnings = config.ValidationWarnings{}
	loaded.RoutingWarnings(settings.Destinations, &warnings)
	if len(warnings.ChecksWithUnknownAlertDestinations) != 1 || warnings.ChecksWithUnknownAlertDestinations[0].UnknownDestinationNames[0] != "misspelled" {
		t.Fatal(warnings)
	}
}

func TestDailyDestinationVerificationRunsQuietlyAndRetriesWithCurrentCredential(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(path, "token")
	if err := os.WriteFile(token, []byte("rejected-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	script := `def deliver_alert(alert, inputs, file_contents):
    fail("verification must not deliver an alert")

def verify_destination(inputs, file_contents):
    return (file_contents["api_token_file"] == "accepted-credential", "server checked credential")
`
	testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "quiet_verification", testhelpers.TestModuleDescription("quiet_verification", checksandalerts.UsedAsAlertDestination, map[string]string{"api_token_file": "string"}, nil), script)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"chat": {RemindEvery: "never", TimeLimitPerDeliveryAttempt: "1m", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.ModuleCall{Name: "quiet_verification", Inputs: map[string]any{"api_token_file": token}, ExecOptions: primitives.ExecOptions{RunAsUserAccount: account.Username, RunFromDirectory: "/"}}}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := d.started
	d.now = func() time.Time { return now }
	run := func(at time.Time) {
		now = at
		var workers sync.WaitGroup
		d.scheduleDestinationVerifications(context.Background(), now, &workers)
		workers.Wait()
	}
	run(now.Add(24*time.Hour - time.Nanosecond))
	if health := d.healthOf("chat"); !health.VerificationFailingSince.IsZero() {
		t.Fatal("verified before a day elapsed")
	}
	run(d.started.Add(24 * time.Hour))
	health := d.healthOf("chat")
	if !health.VerificationFailingSince.Equal(now) || health.VerificationError != "server checked credential" || !health.NextVerificationAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("daily verification did not run: %+v", health)
	}
	if err := os.WriteFile(token, []byte("accepted-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	run(now.Add(time.Minute))
	if !health.VerificationFailingSince.IsZero() || !health.NextVerificationAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("retry did not accept the current credential: %+v", health)
	}
	if outboxCount(t, d, "chat") != 0 {
		t.Fatal("verification queued an alert")
	}
}

func TestJoinedRoutingWarningsUseTheSavedFleetSettings(t *testing.T) {
	t.Parallel()
	_, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"team": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, AlertSeverities: []string{"sos"}}})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	group := node.config.CheckGroups["app"]
	check := group.Checks["shell"]
	names := []string{"misspelled"}
	check.SendAlertsOnlyToDestinations = &names
	group.Checks["shell"] = check
	warnings := ValidationWarnings(node.config, node.store.Node, false)
	if len(warnings.ChecksWithUnknownAlertDestinations) != 0 || len(warnings.ChecksWithoutAlertDestination) != 0 {
		t.Fatalf("config-only read saved hub settings: %+v", warnings)
	}
	warnings = ValidationWarnings(node.config, node.store.Node, true)
	if len(warnings.ChecksWithUnknownAlertDestinations) != 1 || warnings.ChecksWithUnknownAlertDestinations[0].UnknownDestinationNames[0] != "misspelled" || len(warnings.ChecksWithoutAlertDestination) != 1 {
		t.Fatalf("saved destinations did not reach validation: %+v", warnings)
	}
	encoded, err := json.Marshal(warnings)
	if err != nil || !strings.Contains(string(encoded), `"checks_with_unknown_alert_destinations":[`) || !strings.Contains(string(encoded), `"unknown_destination_names":["misspelled"]`) {
		t.Fatal(string(encoded), err)
	}
}

func TestRestartWithUnavailableCredentialPreservesQueuedAlerts(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	token := filepath.Join(path, "credential")
	unavailable := filepath.Join(path, "credential-unavailable")
	if err := os.WriteFile(token, []byte("original-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"chat": installTokenModule(t, c, token)}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	event := checksandalerts.Event{ID: incidents.ID(), Type: "notice", At: d.now(), AlertSeverity: "warn", Message: "queued before credential became unavailable", Notify: true}
	d.mu.Lock()
	err = d.record([]checksandalerts.Event{event})
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(token, unavailable); err != nil {
		t.Fatal(err)
	}
	d, err = NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.config.Alerts.Destinations["chat"].CredentialContentFingerprint; got != "" {
		t.Fatal("unreadable credential got a fingerprint", got)
	}
	checkQueue := func() {
		t.Helper()
		pending, err := d.store.Pending()
		if err != nil || len(pending) != 1 || pending[0].Event.ID != event.ID {
			t.Fatal("restart discarded the queued alert", pending, err)
		}
	}
	checkQueue()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(unavailable, token); err != nil {
		t.Fatal(err)
	}
	d, err = NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	checkQueue()
}

func TestDestinationHealthSurvivesRestartAndAnnouncesRecovery(t *testing.T) {
	t.Parallel()
	for _, recovery := range []string{"delivery_then_verification", "verification"} {
		t.Run(recovery, func(t *testing.T) {
			path, c := daemonConfig(t)
			receiver, other := captureAlerts(t), captureAlerts(t)
			c.Alerts.Destinations = map[string]checksandalerts.Destination{
				"checked": {Inputs: &checksandalerts.Webhook{URL: receiver.URL}, RemindEvery: "never", AlertSeverities: []string{"warn"}},
				"other":   {Inputs: &checksandalerts.Webhook{URL: other.URL}, RemindEvery: "never", AlertSeverities: []string{}},
			}
			writeConfig(t, path, c)
			now := time.Now()
			clock := func() time.Time { return now }
			d, err := newDaemon(path, clock)
			if err != nil {
				t.Fatal(err)
			}
			d.mu.Lock()
			delivery, health, ok := d.verificationDelivery("checked")
			d.mu.Unlock()
			if !ok {
				t.Fatal("verification not reserved")
			}
			beganFailing := now
			d.finishVerification("checked", delivery, health, checksandalerts.DestinationVerification{CanVerifyWithoutMessage: true, VerificationMessage: "credential refused"})
			nextVerification := health.NextVerificationAt
			now = now.Add(10 * time.Minute)
			d.checkDestinationHealth(now)
			if outboxCount(t, d, "other") != 1 {
				t.Fatal("failure notice not queued")
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := newDaemon(path, clock)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { restarted.Close() })
			restored := restarted.healthOf("checked")
			if !restored.VerificationFailingSince.Equal(beganFailing) || !restored.NextVerificationAt.Equal(nextVerification) || !restored.FailingNoticeSent {
				t.Fatalf("lost health on restart: %+v", restored)
			}
			recorder := httptest.NewRecorder()
			restarted.routes().ServeHTTP(recorder, httptest.NewRequest("GET", "/status", nil))
			var status struct {
				Delivery store.DeliverySummary `json:"delivery"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil || len(status.Delivery.FailingDestinations) != 1 || status.Delivery.FailingDestinations[0].LatestAttemptError != "credential refused" {
				t.Fatal(recorder.Body.String(), err)
			}
			restarted.checkDestinationHealth(now)
			if outboxCount(t, restarted, "other") != 1 {
				t.Fatal("restart repeated failure notice")
			}
			if recovery == "verification" {
				restarted.mu.Lock()
				delivery, health, ok = restarted.verificationDelivery("checked")
				restarted.mu.Unlock()
				if !ok {
					t.Fatal("verification not reserved after restart")
				}
				restarted.finishVerification("checked", delivery, health, checksandalerts.DestinationVerification{CanVerifyWithoutMessage: true, AcceptsAddressAndCredential: true, VerificationMessage: "accepted"})
			} else {
				restarted.mu.Lock()
				err = restarted.record([]checksandalerts.Event{{ID: incidents.ID(), Type: "notice", At: now, AlertSeverity: "warn", Message: "delivery after restart", Notify: true}})
				restarted.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				pending, err := restarted.store.Pending()
				if err != nil {
					t.Fatal(err)
				}
				for _, queued := range pending {
					if queued.Destination != "checked" {
						continue
					}
					restarted.mu.Lock()
					sender, ok := restarted.sender(queued)
					restarted.mu.Unlock()
					if !ok {
						t.Fatal("no sender after restart")
					}
					if err := sender.Send(context.Background(), queued.Event); err != nil {
						t.Fatal(err)
					}
					if _, err := restarted.store.Delivered(queued, nil); err != nil {
						t.Fatal(err)
					}
					restarted.mu.Lock()
					restarted.destinationSucceeded("checked", queued.Target, now, "delivered an alert")
					restarted.mu.Unlock()
				}
				if restarted.healthOf("checked").VerificationFailingSince.IsZero() || outboxCount(t, restarted, "other") != 1 {
					t.Fatal("delivery ended the failed verification")
				}
				restarted.mu.Lock()
				delivery, health, ok = restarted.verificationDelivery("checked")
				restarted.mu.Unlock()
				if !ok {
					t.Fatal("verification not reserved after delivery")
				}
				restarted.finishVerification("checked", delivery, health, checksandalerts.DestinationVerification{CanVerifyWithoutMessage: true, AcceptsAddressAndCredential: true, VerificationMessage: "accepted"})
			}
			runWorker(t, restarted)
			got := other.wait(t, 2)
			if got[0].AlertSeverity != "warn" || got[0].Message != "Destination checked is failing." || got[1].AlertSeverity != "warn" || got[1].Message != "Destination checked works again." {
				t.Fatal(got)
			}
			restarted.mu.Lock()
			restored = restarted.healthOf("checked")
			failed, announced := restored.VerificationFailingSince, restored.FailingNoticeSent
			restarted.mu.Unlock()
			if !failed.IsZero() || announced {
				t.Fatal("recovery did not clear persisted failure")
			}
		})
	}
}

func TestPartialDeliveryDoesNotPostponeTheBacklogWarning(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{
		"blocked": {Inputs: &checksandalerts.Webhook{URL: "https://example.com/blocked"}, RemindEvery: "never", AlertSeverities: []string{"warn"}},
		"other":   {Inputs: &checksandalerts.Webhook{URL: "https://example.com/other"}, RemindEvery: "never", AlertSeverities: []string{}},
	}
	writeConfig(t, path, c)
	now := time.Now()
	d, err := newDaemon(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	queuedAt := now
	d.mu.Lock()
	err = d.record([]checksandalerts.Event{
		{ID: incidents.ID(), Type: "notice", At: now, AlertSeverity: "warn", Message: "first", Notify: true},
		{ID: incidents.ID(), Type: "notice", At: now, AlertSeverity: "warn", Message: "second", Notify: true},
	})
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.DB.Exec("UPDATE outbox SET queued_at=?", queuedAt.UnixNano()); err != nil {
		t.Fatal(err)
	}
	now = queuedAt.Add(9 * time.Minute)
	pending, err := d.store.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	if _, err := d.store.Delivered(pending[0], nil); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.destinationSucceeded("blocked", pending[0].Target, now, "delivered an alert")
	d.mu.Unlock()
	now = queuedAt.Add(10*time.Minute - time.Nanosecond)
	d.checkDestinationHealth(now)
	if outboxCount(t, d, "other") != 0 {
		t.Fatal("warning sent before ten minutes")
	}
	now = queuedAt.Add(10 * time.Minute)
	d.checkDestinationHealth(now)
	if outboxCount(t, d, "other") != 1 {
		t.Fatal("partial progress postponed warning for the ten-minute-old backlog")
	}
}

func TestDeliveriesDoNotPostponeDailyVerification(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	script := `def deliver_alert(alert, inputs, file_contents):
    return {"status": "delivered", "message": "accepted"}

def verify_destination(inputs, file_contents):
    return (False, "daily verification ran")
`
	testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "daily_verification", testhelpers.TestModuleDescription("daily_verification", checksandalerts.UsedAsAlertDestination, nil, nil), script)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"chat": {Inputs: &checksandalerts.ModuleCall{Name: "daily_verification", ExecOptions: primitives.ExecOptions{RunAsUserAccount: account.Username, RunFromDirectory: "/"}}, RemindEvery: "never", TimeLimitPerDeliveryAttempt: "1m", AlertSeverities: testhelpers.AllSeverities}}
	writeConfig(t, path, c)
	now := time.Now()
	d, err := newDaemon(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	due := d.healthOf("chat").NextVerificationAt
	for hour := 1; hour < 24; hour++ {
		now = d.started.Add(time.Duration(hour) * time.Hour)
		d.mu.Lock()
		event := checksandalerts.Event{ID: incidents.ID(), Type: "notice", At: now, AlertSeverity: "warn", Message: "hourly alert", Notify: true}
		err = d.record([]checksandalerts.Event{event})
		d.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		pending, err := d.store.Pending()
		if err != nil || len(pending) != 1 {
			t.Fatal(pending, err)
		}
		d.mu.Lock()
		sender, ok := d.sender(pending[0])
		d.mu.Unlock()
		if !ok {
			t.Fatal("no sender")
		}
		if err := sender.Send(context.Background(), pending[0].Event); err != nil {
			t.Fatal(err)
		}
		if _, err := d.store.Delivered(pending[0], nil); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		d.destinationSucceeded("chat", pending[0].Target, now, "delivered an alert")
		d.mu.Unlock()
	}
	if !d.healthOf("chat").NextVerificationAt.Equal(due) {
		t.Fatal("hourly deliveries postponed verification")
	}
	now = due
	var workers sync.WaitGroup
	d.scheduleDestinationVerifications(context.Background(), now, &workers)
	workers.Wait()
	health := d.healthOf("chat")
	if health.VerificationError != "daily verification ran" || !health.VerificationFailingSince.Equal(due) {
		t.Fatalf("daily verification did not run: %+v", health)
	}
}

func TestReloadCertificateAuthoritiesPreservesQueuedAlerts(t *testing.T) {
	t.Parallel()
	for _, module := range []string{"webhook", "email", "custom"} {
		t.Run(module, func(t *testing.T) {
			path, c := daemonConfig(t)
			authority, _ := testhelpers.TestCertificate(t, t.TempDir())
			replacement, _ := testhelpers.TestCertificate(t, t.TempDir())
			destination := checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities}
			switch module {
			case "webhook":
				destination.Inputs = &checksandalerts.Webhook{URL: "https://example.com", TrustedCertificateAuthorityFile: authority}
			case "email":
				destination.Inputs = &checksandalerts.Email{SMTPServer: "mail.example.com:465", SMTPConnectionSecurity: "implicit_tls", From: "mmwatchdog@example.com", To: []string{"person@example.com"}, TrustedCertificateAuthorityFile: authority}
			case "custom":
				account, err := user.Current()
				if err != nil {
					t.Fatal(err)
				}
				testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "public_authorities", testhelpers.TestModuleDescription("public_authorities", checksandalerts.UsedAsAlertDestination, map[string]string{"trusted_certificate_authority_file": "string"}, nil), `def deliver_alert(alert, inputs, file_contents):
    return {"status": "delivered", "message": "accepted"}

def verify_destination(inputs, file_contents):
    return (True, "accepted")
`)
				destination.TimeLimitPerDeliveryAttempt = "1m"
				destination.Inputs = &checksandalerts.ModuleCall{Name: "public_authorities", Inputs: map[string]any{"trusted_certificate_authority_file": authority}, ExecOptions: primitives.ExecOptions{RunAsUserAccount: account.Username, RunFromDirectory: "/"}}
			}
			c.Alerts.Destinations = map[string]checksandalerts.Destination{"chat": destination}
			writeConfig(t, path, c)
			d, err := NewDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			d.mu.Lock()
			err = d.record([]checksandalerts.Event{{ID: incidents.ID(), Type: "notice", AlertSeverity: "warn", At: d.now(), Message: "queued before certificate rotation", Notify: true}})
			d.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := os.ReadFile(replacement)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(authority, bundle, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := d.reload()
			if err != nil || len(result.NewOrChangedDestinations) != 0 || len(result.DestinationVerifications) != 0 || outboxCount(t, d, "chat") != 1 {
				t.Fatal("certificate rotation changed delivery identity or cancelled queued alerts", result, err)
			}
		})
	}
}

func TestWebhookVerificationDoesNotRecoverUndeliveredAlerts(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	d := noticeDaemon(t, map[string]checksandalerts.Destination{
		"broken": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: server.URL}, AlertSeverities: []string{"warn"}},
		"other":  {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, AlertSeverities: []string{}},
	})
	now := d.now()
	d.now = func() time.Time { return now }
	d.mu.Lock()
	err := d.record([]checksandalerts.Event{{ID: incidents.ID(), Type: "notice", AlertSeverity: "warn", At: now, Message: "queued", Notify: true}})
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.DB.Exec("UPDATE outbox SET queued_at=?", now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	pending, err := d.store.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	d.mu.Lock()
	delivery, health, ok := d.verificationDelivery("broken")
	d.mu.Unlock()
	if !ok {
		t.Fatal("verification not reserved")
	}
	if err := delivery.Destination.Inputs.Sender(delivery).Send(context.Background(), pending[0].Event); err == nil {
		t.Fatal("webhook delivery unexpectedly succeeded")
	} else if _, err := d.store.Delivered(pending[0], err); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	d.checkDestinationHealth(now)
	result := delivery.VerifyDestination(context.Background())
	if !result.AcceptsAddressAndCredential {
		t.Fatal(result)
	}
	d.finishVerification("broken", delivery, health, result)
	if !health.FailingNoticeSent || outboxCount(t, d, "other") != 1 || len(d.withFailingVerifications(nil, now)) != 1 {
		t.Fatal("connection-only verification ended a delivery failure")
	}
	// Delivery success must also wait for a previously failed verification.
	d.finishVerification("broken", delivery, health, checksandalerts.DestinationVerification{CanVerifyWithoutMessage: true, VerificationMessage: "verification refused"})
	if _, err := d.store.Delivered(pending[0], nil); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.destinationSucceeded("broken", pending[0].Target, now, "delivered an alert")
	d.mu.Unlock()
	if health.VerificationFailingSince.IsZero() || outboxCount(t, d, "other") != 1 {
		t.Fatal("delivery erased a failed verification")
	}
	d.finishVerification("broken", delivery, health, result)
	if health.FailingNoticeSent || outboxCount(t, d, "other") != 2 {
		t.Fatal("both conditions recovered without a recovery notice")
	}
}

func TestPublicDestinationJSONNames(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		value    any
		field    string
		expected any
	}{
		{"reload", ReloadResult{NewOrChangedDestinations: []string{"new_destination"}}, "new_or_changed_destinations", []any{"new_destination"}},
		{"routing warning", config.CheckWithUnknownAlertDestinations{UnknownDestinationNames: []string{"missing_destination"}}, "unknown_destination_names", []any{"missing_destination"}},
		{"destination queue", store.FailingDestination{OldestUndeliveredAlertQueuedAt: at}, "oldest_undelivered_alert_queued_at", at.Format(time.RFC3339)},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(object[test.field], test.expected) {
				t.Fatal(string(encoded))
			}
		})
	}
}
