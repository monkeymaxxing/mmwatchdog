package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

func TestNotificationStorageFailureIsVisibleUntilDeliveryRecovers(t *testing.T) {
	t.Parallel()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer node.Close()
	path, _ := daemonConfig(t)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.config.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: node.URL}, TimeLimitPerDeliveryAttempt: "1m"}}
	if err := d.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), Type: "failed", At: time.Now(), Notify: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.DB.Exec("CREATE TRIGGER reject_ack BEFORE DELETE ON outbox BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	status := func() (string, int) {
		t.Helper()
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status", nil))
		var s struct {
			StorageError string `json:"storage_error"`
			Delivery     struct {
				Pending int `json:"confirming_failure"`
			} `json:"delivery"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s.StorageError, s.Delivery.Pending
	}
	for {
		problem, _ := status()
		if strings.Contains(problem, "fixture storage failure") {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("delivery bookkeeping failure is missing from status")
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.mu.Lock()
	err = d.record(nil)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if problem, _ := status(); !strings.Contains(problem, "fixture storage failure") {
		t.Fatal("successful snapshot write hid the delivery failure")
	}
	if _, err := d.store.DB.Exec("DROP TRIGGER reject_ack"); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		if problem, pending := status(); problem == "" && pending == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delivery and visible storage status did not recover")
}

func TestWebhookSenderContract(t *testing.T) {
	t.Parallel()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("0123456789abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	var code atomic.Int32
	code.Store(204)
	var calls atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var event checksandalerts.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != event.ID || event.ID != "fixed-id" || r.Header.Get("Authorization") != "Bearer 0123456789abcdef" {
			t.Errorf("invalid webhook request: %v %v", r, event)
		}
		if code.Load() == 307 {
			w.Header().Set("Location", "/redirect")
		}
		w.WriteHeader(int(code.Load()))
	}))
	defer node.Close()
	var sender checksandalerts.NotificationSender = checksandalerts.WebhookSender{Delivery: checksandalerts.DeliveryOf{Destination: checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: node.URL, BearerTokenFile: token}, TimeLimitPerDeliveryAttempt: "1m"}}}
	event := checksandalerts.Event{AlertSeverity: "warn", ID: "fixed-id", Type: "failed"}
	for _, status := range []int32{204, 503, 307} {
		code.Store(status)
		err := sender.Send(context.Background(), event)
		if (err == nil) != (status == 204) {
			t.Fatalf("HTTP %d: %v", status, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("redirect followed: %d requests", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sender.Send(ctx, event) == nil {
		t.Fatal("ignored canceled delivery")
	}
}

func TestWebhookSlowDestinationDoesNotBlockAnother(t *testing.T) {
	t.Parallel()
	slowStarted := make(chan struct{})
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-slowStarted:
		default:
			close(slowStarted)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(204)
	}))
	defer slow.Close()
	fastEvents := make(chan string, 4)
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event checksandalerts.Event
		_ = json.NewDecoder(r.Body).Decode(&event)
		fastEvents <- event.Type
		w.WriteHeader(204)
	}))
	defer fast.Close()
	stateStore, err := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	now := time.Now()
	events := []checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), At: now, Type: "failed", Notify: true}, {AlertSeverity: "warn", ID: incidents.ID(), At: now, Type: "recovered", Notify: true}}
	// The slow destination holds its attempt for its whole time limit, a
	// minute. A fast destination that waited for it would get its alerts
	// that late.
	destinations := map[string]checksandalerts.Destination{"slow": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: slow.URL}, TimeLimitPerDeliveryAttempt: "1m"}, "fast": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: fast.URL}, TimeLimitPerDeliveryAttempt: "1m"}}
	if err := stateStore.Save(incidents.FreshSnapshot(), events, store.AlertRoute{Destinations: destinations}); err != nil {
		t.Fatal(err)
	}
	history, _ := openTestHistory(t, 16*mib)
	d := &Daemon{now: time.Now, started: time.Now(), store: stateStore, history: history, config: &config.Config{Alerts: config.Alerts{Destinations: destinations}, KeepMeasurementHistoryFor: "7d"}, storage: fleet.StorageHealth{}, deliveries: map[string]bool{}, deliveriesFinished: map[string]int{}, nextAttempts: map[string]queueHead{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	defer func() { close(release); cancel(); <-done }()
	// The attempt at the slow destination starts and holds on until the
	// test ends. The alerts of the fast destination arrive all the same.
	select {
	case <-slowStarted:
	case <-time.After(testhelpers.LongestWaitForAnEvent):
		t.Fatal("slow destination never started")
	}
	for _, want := range []string{"failed", "recovered"} {
		select {
		case got := <-fastEvents:
			if got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("slow destination blocked fast destination")
		}
	}
}

func TestDestinationChangesCancelQueuedNotifications(t *testing.T) {
	t.Parallel()
	for _, activation := range []string{"reload", "restart"} {
		for _, change := range []string{"remove", "url", "token file"} {
			t.Run(activation+"/"+change, func(t *testing.T) {
				oldCalls := atomic.Int32{}
				old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { oldCalls.Add(1); w.WriteHeader(204) }))
				defer old.Close()
				delivered := make(chan string, 8)
				current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var event checksandalerts.Event
					if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
						t.Error(err)
					}
					delivered <- event.ID
					w.WriteHeader(204)
				}))
				defer current.Close()
				p, c := daemonConfig(t)
				token := filepath.Join(filepath.Dir(p), "token")
				if err := os.WriteFile(token, []byte("original-test-token"), 0600); err != nil {
					t.Fatal(err)
				}
				c.Alerts.Destinations = map[string]checksandalerts.Destination{
					"ops":       installTokenModule(t, c, token),
					"unchanged": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: current.URL}, TimeLimitPerDeliveryAttempt: "1m"},
				}
				writeConfig(t, p, c)
				d, err := NewDaemon(p)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { d.Close() }()
				event := checksandalerts.Event{ID: incidents.ID(), Type: "failed", AlertSeverity: "warn", At: time.Now(), Notify: true}
				if err := d.record([]checksandalerts.Event{event}); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "remove":
					delete(c.Alerts.Destinations, "ops")
				case "url":
					c.Alerts.Destinations["ops"] = checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: current.URL, BearerTokenFile: token}, TimeLimitPerDeliveryAttempt: "1m"}
				case "token file":
					if err := os.WriteFile(token+"-replacement", []byte("a-long-replacement-token"), 0600); err != nil {
						t.Fatal(err)
					}
					c.Alerts.Destinations["ops"] = moduleDestination(t, "token_module", map[string]any{"api_token_file": token + "-replacement"})
				}
				writeConfig(t, p, c)
				if activation == "reload" {
					_, err = d.reload()
				} else {
					if err := d.Close(); err != nil {
						t.Fatal(err)
					}
					d, err = NewDaemon(p)
				}
				if err != nil {
					t.Fatal(err)
				}
				// Reusing the old token path must not send a replacement secret to an old URL.
				if err := os.WriteFile(token, []byte("replacement-test-token"), 0600); err != nil {
					t.Fatal(err)
				}
				pending, err := d.store.Pending()
				if err != nil || len(pending) != 1 || pending[0].Destination != "unchanged" || pending[0].Event.ID != event.ID {
					t.Fatal(pending, err)
				}
				events, err := d.store.Events(10)
				if err != nil {
					t.Fatal(err)
				}
				cancelled := false
				for _, e := range events {
					if e.Type == "delivery_cancelled" {
						cancelled = true
					}
				}
				if !cancelled {
					t.Fatal("cancelled delivery is missing from history")
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { d.notifications(ctx); close(done) }()
				defer func() { cancel(); <-done }()
				select {
				case id := <-delivered:
					if id != event.ID {
						t.Fatal(id)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("unchanged destination lost queued event")
				}
				if oldCalls.Load() != 0 {
					t.Fatal("old queued event reached changed destination")
				}
			})
		}
	}
}

// A reload that removes a destination cancels its queued deliveries in the
// same commit; if that commit fails, the reload changes nothing.
func TestReloadCancelsRemovedDestinationDurably(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "1s"}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.record([]checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), Type: "failed", At: time.Now(), Notify: true}}); err != nil {
		t.Fatal(err)
	}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{}
	writeConfig(t, p, c)
	if _, err := d.store.DB.Exec("CREATE TRIGGER reject_cancel BEFORE DELETE ON outbox BEGIN SELECT RAISE(FAIL, 'fixture cancellation failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.reload(); err == nil || len(d.config.Alerts.Destinations) != 1 || outboxCount(t, d, "ops") != 1 {
		t.Fatal("activation succeeded without durable cancellation", err)
	}
	if _, err := d.store.DB.Exec("DROP TRIGGER reject_cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.reload(); err != nil || outboxCount(t, d, "ops") != 0 {
		t.Fatal("removed destination still queued", err)
	}
}

func TestUnchangedDestinationRetriesAfterCredentialFileMoves(t *testing.T) {
	t.Parallel()
	type attempt struct{ id, auth string }
	attempts := make(chan attempt, 4)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event checksandalerts.Event
		_ = json.NewDecoder(r.Body).Decode(&event)
		auth := r.Header.Get("Authorization")
		attempts <- attempt{event.ID, auth}
		if auth == "Bearer original-test-token" {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer node.Close()
	p, c := daemonConfig(t)
	token := filepath.Join(filepath.Dir(p), "token")
	if err := os.WriteFile(token, []byte("original-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: node.URL, BearerTokenFile: token}, TimeLimitPerDeliveryAttempt: "1m"}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	event := checksandalerts.Event{ID: incidents.ID(), Type: "failed", AlertSeverity: "warn", At: time.Now(), Notify: true}
	if err := d.record([]checksandalerts.Event{event}); err != nil {
		t.Fatal(err)
	}
	// The worker runs until the test ends; the retry comes a second after
	// the failed attempt.
	ctx, cancel := context.WithTimeout(context.Background(), testhelpers.LongestWaitForAnEvent)
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case got := <-attempts:
		if got != (attempt{event.ID, "Bearer original-test-token"}) {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal("no initial attempt")
	}
	if err := os.WriteFile(token+"-moved", []byte("original-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Alerts.Destinations["ops"] = checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: node.URL, BearerTokenFile: token + "-moved"}, TimeLimitPerDeliveryAttempt: "2m"}
	writeConfig(t, p, c)
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-attempts:
		if got != (attempt{event.ID, "Bearer original-test-token"}) {
			t.Fatal("retry lost identity after moving an unchanged credential", got)
		}
	case <-ctx.Done():
		t.Fatal("unchanged destination lost retry")
	}
}

// queuedReminders lists the queued reminders in order, and checks that each
// is queued only for the destination it names.
func queuedReminders(t *testing.T, d *Daemon) []checksandalerts.Event {
	t.Helper()
	rows, e := d.store.DB.Query("SELECT destination, event FROM outbox WHERE json_extract(event,'$.type')='reminder' ORDER BY seq")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	events := []checksandalerts.Event{}
	for rows.Next() {
		var destination string
		var b []byte
		var event checksandalerts.Event
		if e = rows.Scan(&destination, &b); e == nil {
			e = json.Unmarshal(b, &event)
		}
		if e != nil {
			t.Fatal(e)
		}
		if destination != event.Destination {
			t.Fatalf("a reminder for %q was queued for %s", event.Destination, destination)
		}
		events = append(events, event)
	}
	return events
}

// A destination with remind_every receives a reminder for an announced
// incident still open, every interval after the failure was announced or its
// previous reminder, and a daemon restart neither repeats nor skips one.
// Maintenance holds reminders, which resume one interval after it ends, and
// they stop when the incident resolves.
func TestRemindersRepeatOpenIncidents(t *testing.T) {
	t.Parallel()
	p, c := heartbeatConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{
		"oncall": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/oncall"}, TimeLimitPerDeliveryAttempt: "5s", RemindEvery: "1h"},
		"ops":    {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "5s"},
	}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	// Reminder times lie in the past, so a window opened now outlasts them.
	t0 := time.Now().UTC().Add(-5 * time.Hour)
	schedule := func(at time.Time, want int) []checksandalerts.Event {
		t.Helper()
		d.schedule(context.Background(), at)
		got := queuedReminders(t, d)
		if len(got) != want {
			t.Fatalf("at %s: %d reminders, want %d: %+v", at, len(got), want, got)
		}
		return got
	}
	d.snapshot.Checks["backup/heartbeat"].Deadline = t0
	schedule(t0, 0)
	st := d.snapshot.Checks["backup/heartbeat"]
	failed := queuedEvents(t, d)[0]
	if failed.Type != "failed" || st.FailureIncident.Announced != failed.Incident {
		t.Fatal("the overdue heartbeat was not announced", failed)
	}
	schedule(t0.Add(59*time.Minute), 0)
	first := schedule(t0.Add(time.Hour), 1)[0]
	want := "backup/heartbeat still failing since " + t0.Format("15:04 UTC") + "; latest result: " + st.Last.Message
	if first.Destination != "oncall" || first.Incident != failed.Incident || first.Evidence == nil || first.Evidence.Incident != failed.Incident ||
		first.ID == failed.ID || first.Message != want || !first.At.Equal(t0.Add(time.Hour)) {
		t.Fatalf("reminder %+v", first)
	}
	schedule(t0.Add(90*time.Minute), 1)

	d.Close()
	if d, err = NewDaemon(p); err != nil {
		t.Fatal(err)
	}
	schedule(t0.Add(2*time.Hour-time.Second), 1)
	// The destination is unreachable, so the first reminder is still
	// queued: the second replaces it and does not pile up behind it.
	second := schedule(t0.Add(2*time.Hour), 1)[0]
	schedule(t0.Add(2*time.Hour), 1)
	if second.ID == first.ID || second.Incident != failed.Incident || !second.At.Equal(t0.Add(2*time.Hour)) {
		t.Fatal("each reminder is its own event, and the new one replaces the undelivered one", second)
	}

	setMaintenance(t, d, "backup", "10m")
	schedule(t0.Add(3*time.Hour), 1)
	ended := time.Now().UTC()
	setMaintenance(t, d, "backup", "off")
	if held := schedule(ended.Add(59*time.Minute), 1)[0]; held.ID != second.ID {
		t.Fatal("maintenance did not hold the reminders", held)
	}
	if third := schedule(ended.Add(time.Hour+time.Second), 1)[0]; third.ID == second.ID {
		t.Fatal("reminders did not resume one interval after the maintenance", third)
	}

	if code := postHeartbeat(d); code != http.StatusNoContent {
		t.Fatal(code)
	}
	st = d.snapshot.Checks["backup/heartbeat"]
	if st.FailureIncident.Incident != "" || st.FailureIncident.Reminded != nil || !st.FailureIncident.RemindFrom.IsZero() {
		t.Fatal("the resolved incident kept its reminders", st.FailureIncident)
	}
	schedule(ended.Add(5*time.Hour), 1)
}

// The status names each destination that is failing, with the error of the
// latest attempt: a node whose module of a destination is gone since MMWatchdog read
// its configuration says which module is missing. The mmhotline destination
// is not in the list, because the status says what is wrong with it in its
// own fields.
func TestStatusNamesFailingDestinations(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	testhelpers.InstallModule(t, c.ExternalModuleDir, "slack", testhelpers.TestModuleDescription("slack", checksandalerts.UsedAsAlertDestination, nil, nil), "true")
	c.Alerts.Destinations = mmhotlineDestinations(t)
	c.Alerts.Destinations["team_chat"] = moduleDestination(t, "slack", map[string]any{})
	c.Alerts.Destinations["quiet"] = checksandalerts.Destination{RemindEvery: "never", AlertSeverities: []string{}, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "1s"}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	program := filepath.Join(c.ExternalModuleDir, "slack", checksandalerts.ModuleExecutableFile)
	if err := os.Remove(program); err != nil {
		t.Fatal(err)
	}
	type status struct {
		Delivery              store.DeliverySummary   `json:"delivery"`
		MMHotlineDestinations []fleet.MMHotlineStatus `json:"mmhotline_destinations"`
	}
	read := func() (status, string) {
		t.Helper()
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status", nil))
		var s status
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s, w.Body.String()
	}
	if s, body := read(); s.Delivery.FailingDestinations == nil || len(s.Delivery.FailingDestinations) != 0 || !strings.Contains(body, `"failing_destinations":[]`) {
		t.Fatal("a node with nothing queued lists a failing destination, or no list:", body)
	}
	recordEvents(t, d, checksandalerts.Event{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", Message: "exit status 1", At: time.Now().UTC(), Notify: true},
		checksandalerts.Event{AlertSeverity: "warn", ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", Message: "exit status 2", At: time.Now().UTC(), Notify: true})
	// One attempt for each destination, as the delivery worker makes it: the
	// module is not installed, and MMHotline does not answer.
	pending, err := d.store.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	for _, delivery := range pending {
		attempt := errors.New("MMHotline gave no signed answer (HTTP 502).")
		if delivery.Destination == "team_chat" {
			d.mu.Lock()
			sender, ok := d.sender(delivery)
			d.mu.Unlock()
			if !ok {
				t.Fatal("no sender for", delivery.Destination)
			}
			if attempt = sender.Send(context.Background(), delivery.Event); attempt == nil {
				t.Fatal("a module that is not installed delivered")
			}
		}
		if _, err = d.store.Delivered(delivery, attempt); err != nil {
			t.Fatal(err)
		}
	}
	s, body := read()
	missing := "the module slack is not installed on this node: " + program + " is not an executable file"
	if len(s.Delivery.FailingDestinations) != 2 || s.Delivery.FailingDestinations[0].Destination != "mmhotline_on_call" {
		t.Fatal(body)
	}
	failing := s.Delivery.FailingDestinations[1]
	if failing.Destination != "team_chat" || failing.UndeliveredAlerts != 2 || failing.LatestAttemptError != missing || time.Until(failing.NextAttemptAt) > 5*time.Second || time.Until(failing.NextAttemptAt) < -5*time.Second {
		t.Fatalf("%+v", failing)
	}
	if failing.Describe() != "Destination team_chat: 2 alerts are not delivered. MMWatchdog tries again at "+failing.NextAttemptAt.UTC().Format("15:04 UTC")+". The latest attempt failed: "+missing {
		t.Fatal(failing.Describe())
	}
	if len(s.MMHotlineDestinations) != 1 || len(s.MMHotlineDestinations[0].DeliveryProblems) != 1 || !strings.HasPrefix(s.MMHotlineDestinations[0].DeliveryProblems[0], "MMHotline gave no signed answer (HTTP 502). Trying again at ") || s.Delivery.LastError == "" || s.Delivery.Pending != 4 {
		t.Fatal("the mmhotline destination's problem belongs in its own field:", body)
	}
	for _, name := range []string{`"destination":"team_chat"`, `"undelivered_alerts":2`, `"latest_attempt_error":"the module slack is not installed on this node`, `"next_attempt_at":"`} {
		if !strings.Contains(body, name) {
			t.Fatalf("the status lacks %s: %s", name, body)
		}
	}
}

// Each attempt to deliver an alert leaves one journal line that names the
// alert, where it went, and whether the destination accepted it.
func TestEachAlertDeliveryLeavesAJournalLine(t *testing.T) {
	t.Parallel()
	outageAlerter := checksandalerts.Event{Title: "db01 cannot reach the MMWatchdog fleet hub", TitleWithAlertSeverity: "SOS: db01 cannot reach the MMWatchdog fleet hub"}
	resolved := checksandalerts.Event{Title: "db01 reaches the MMWatchdog fleet hub again", TitleWithAlertSeverity: "Resolved SOS: db01 reaches the MMWatchdog fleet hub again"}
	for _, test := range []struct {
		destination string
		event       checksandalerts.Event
		err         error
		delay       time.Duration
		passedTo    []string
		want        string
	}{
		{"on_call_push_app", outageAlerter, nil, 0, nil, `delivered the alert "SOS: db01 cannot reach the MMWatchdog fleet hub" to alert destination on_call_push_app, which accepted it`},
		{"on_call_push_app", resolved, nil, 0, nil, `delivered the alert "Resolved SOS: db01 reaches the MMWatchdog fleet hub again" to alert destination on_call_push_app, which accepted it`},
		{"on_call_push_app", outageAlerter, errors.New("api.pushover.net answered HTTP 500"), 4 * time.Second, nil,
			`could not deliver the alert "SOS: db01 cannot reach the MMWatchdog fleet hub" to alert destination on_call_push_app. MMWatchdog will try again in 4s: api.pushover.net answered HTTP 500`},
		{config.HubDestination, outageAlerter, nil, 0, nil, `delivered the alert "SOS: db01 cannot reach the MMWatchdog fleet hub" to the MMWatchdog fleet hub, which accepted it`},
		{"team_mmhotline", outageAlerter, &checksandalerts.MMHotlineRefusal{Status: 422}, 0, []string{"on_call_push_app", "team_email"},
			`alert destination team_mmhotline permanently refused the alert "SOS: db01 cannot reach the MMWatchdog fleet hub", so MMWatchdog sends it to on_call_push_app, team_email: MMHotline permanently refused this alert (HTTP 422): `},
		{"team_mmhotline", outageAlerter, &checksandalerts.MMHotlineRefusal{Status: 422}, 0, nil,
			`alert destination team_mmhotline permanently refused the alert "SOS: db01 cannot reach the MMWatchdog fleet hub", and no other destination can take it, so MMWatchdog dropped it: MMHotline permanently refused this alert (HTTP 422): `},
		{"ops", checksandalerts.Event{Message: "an alert without a title"}, nil, 0, nil, `delivered the alert "an alert without a title" to alert destination ops, which accepted it`},
	} {
		if got := deliveryLine(test.destination, test.event, test.err, test.delay, test.passedTo); got != test.want {
			t.Errorf("deliveryLine(%s, %v):\n got %s\nwant %s", test.destination, test.err, got, test.want)
		}
	}
}

// A restart that comes while an alert is on its way to the destination must
// not make the next run send that alert again: the run that stops lets the
// attempt finish and records its result (a deploy turns maintenance on and
// restarts MMWatchdog a second later; the destination got "maintenance on"
// twice).
func TestStoppingARunLetsAnInFlightDeliveryFinishAndRecordsIt(t *testing.T) {
	t.Parallel()
	received := make(chan struct{}, 4)
	var requests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		received <- struct{}{}
		time.Sleep(500 * time.Millisecond) // the destination is slow to answer
		w.WriteHeader(204)
	}))
	defer destination.Close()
	stateStore, err := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	events := []checksandalerts.Event{{AlertSeverity: "warn", ID: incidents.ID(), At: time.Now(), Type: "maintenance_on", Notify: true}}
	destinations := map[string]checksandalerts.Destination{"team": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: destination.URL}, TimeLimitPerDeliveryAttempt: "1m"}}
	if err := stateStore.Save(incidents.FreshSnapshot(), events, store.AlertRoute{Destinations: destinations}); err != nil {
		t.Fatal(err)
	}
	history, _ := openTestHistory(t, 16*mib)
	d := &Daemon{now: time.Now, started: time.Now(), store: stateStore, history: history, config: &config.Config{Alerts: config.Alerts{Destinations: destinations}, KeepMeasurementHistoryFor: "7d"}, storage: fleet.StorageHealth{}, deliveries: map[string]bool{}, deliveriesFinished: map[string]int{}, nextAttempts: map[string]queueHead{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	select {
	case <-received:
	case <-time.After(testhelpers.LongestWaitForAnEvent):
		t.Fatal("the destination never got the alert")
	}
	cancel() // systemd stops the service while the destination is answering
	<-done
	left, e := stateStore.Pending()
	if e != nil {
		t.Fatal(e)
	}
	if len(left) != 0 {
		t.Fatalf("the destination got the alert (%d request) but %d alert still waits in the outbox, so the next run sends it again", requests.Load(), len(left))
	}
}
