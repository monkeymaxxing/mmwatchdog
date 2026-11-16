package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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

func TestEventTimeSpansIncludeEveryEventAndBothEnds(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background()) // establish the history cursor
	start := time.Now().UTC().Add(-time.Hour)
	events := make([]checksandalerts.Event, 125)
	for i := range events {
		events[i] = checksandalerts.Event{ID: incidents.ID(), CheckGroup: "app", Type: "notice", At: start.Add(time.Duration(i) * time.Second), Message: fmt.Sprint(i)}
	}
	node.mu.Lock()
	err := node.record(events)
	node.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	node.report(context.Background())
	end := events[len(events)-1].At
	span := url.Values{"since": {start.Format(time.RFC3339Nano)}, "until": {end.Format(time.RFC3339Nano)}}.Encode()
	for _, target := range []struct {
		d     *Daemon
		path  string
		fleet bool
	}{
		{node, "/events?check_group=app", false}, {hub, "/fleet?node=app-1", true},
	} {
		read := func(query string) []checksandalerts.Event {
			t.Helper()
			response := httptest.NewRecorder()
			target.d.routes().ServeHTTP(response, httptest.NewRequest("GET", target.path+query, nil))
			if response.Code != http.StatusOK {
				t.Fatal(response.Code, response.Body.String())
			}
			var got []checksandalerts.Event
			if target.fleet {
				var detail fleet.FleetNode
				if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
					t.Fatal(err)
				}
				got = detail.Events
			} else if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			return got
		}
		if got := read(""); len(got) != 100 {
			t.Fatalf("%s: latest events: %d", target.path, len(got))
		}
		wide := url.Values{"since": {"0001-01-01T00:00:00Z"}, "until": {"9999-12-31T23:59:59Z"}}.Encode()
		wideEvents := read("&" + wide)
		seen := map[string]bool{}
		for _, event := range wideEvents {
			seen[event.ID] = true
		}
		for _, event := range events {
			if !seen[event.ID] {
				t.Fatal("wide valid time span lost an event", event.ID)
			}
		}
		future := url.Values{"since": {"9999-01-01T00:00:00Z"}, "until": {"9999-12-31T23:59:59Z"}}.Encode()
		if got := read("&" + future); len(got) != 0 {
			t.Fatal("future time span", got)
		}
		got := read("&" + span)
		if len(got) != 125 || got[0].ID != events[124].ID || got[124].ID != events[0].ID {
			t.Fatalf("%s: span: %+v", target.path, got)
		}
		exact := url.Values{"since": {events[15].At.Format(time.RFC3339Nano)}, "until": {events[15].At.Format(time.RFC3339Nano)}}.Encode()
		if got := read("&" + exact); len(got) != 1 || got[0].ID != events[15].ID {
			t.Fatalf("single instant: %+v", got)
		}
	}
	for _, query := range []string{"since=wrong&until=wrong", "until=2026-10-07T15:00:00Z", "since=2026-10-07T15:00:00Z&until=2026-10-07T14:00:00Z"} {
		for _, path := range []string{"/events?", "/fleet?node=app-1&"} {
			response := httptest.NewRecorder()
			hub.routes().ServeHTTP(response, httptest.NewRequest("GET", path+query, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatal(path, query, response.Code)
			}
		}
	}
}

func TestJoinedConfigShowsFleetSettingsWithoutCredentialContents(t *testing.T) {
	t.Parallel()
	secret := "do-not-expose-this-credential-content"
	token := testhelpers.WriteToken(t, secret)
	destinations := outageAlerterDestinations()
	destination := destinations["ops"]
	destination.Inputs = &checksandalerts.Webhook{URL: "https://example.com/alerts", BearerTokenFile: token}
	destinations["ops"] = destination
	hub, code, _ := startHubWith(t, destinations)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	response := httptest.NewRecorder()
	node.routes().ServeHTTP(response, httptest.NewRequest("GET", "/config", nil))
	var current struct {
		config.Config
		FleetTimingsFromHub config.FleetTimings `json:"fleet_timings_from_hub"`
	}
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Alerts.Destinations["ops"].Inputs == nil || current.FleetTimingsFromHub != hub.config.FleetHubRole.FleetTimings {
		t.Fatal(response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), "file_contents_by_destination") {
		t.Fatal("config exposed credential contents", response.Body.String())
	}
	if len(node.config.Alerts.Destinations) != 0 {
		t.Fatal("config inspection changed local settings")
	}
}

func TestFleetExpandsADestinationFailureWithoutAFailedCheck(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	report := node.buildReport(time.Now())
	report.Delivery.FailingDestinations = []store.FailingDestination{{Destination: "ops", UndeliveredAlerts: 1, LatestAttemptError: "receiver refused the alert"}}
	report.Summary.ExitCode = 0
	_, status, err := hub.hub.Accept("app-1", &report, time.Now())
	if err != nil || status != 200 {
		t.Fatal(status, err)
	}
	view := hub.hub.Fleet(time.Now())
	for _, entry := range view.Nodes {
		if entry.Name == "app-1" && entry.ExitCode != 3 {
			t.Fatalf("destination failure not reflected: %+v", entry)
		}
	}
	if view.ExitCode != 3 {
		t.Fatalf("fleet exit: %d", view.ExitCode)
	}
}

func TestHubBindsListenOnAddress(t *testing.T) {
	t.Parallel()
	hub, _ := startHub(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	hub.config.FleetHubRole.ListenOnAddress = occupied.Addr().String()
	err = hub.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), occupied.Addr().String()) {
		t.Fatalf("hub did not bind its configured address: %v", err)
	}
}

func TestFleetThresholdSettingsAreNumericAndExclusive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ settings, problem string }{
		{"raise_to_sos_when_silent_nodes_reach_count: 2, raise_to_sos_when_silent_nodes_reach_percent: 5", "keep only"},
		{"raise_to_sos_when_silent_nodes_reach_count: 0", "at least 1"},
		{"raise_to_sos_when_silent_nodes_reach_count: 1.5", "expected an integer"},
		{"raise_to_sos_when_silent_nodes_reach_count: '2'", "expected an integer"},
		{"raise_to_sos_when_silent_nodes_reach_percent: 101", "at most 100"},
		{"raise_to_sos_when_silent_nodes_reach_percent: 0", "above 0"},
		{"raise_to_sos_when_silent_nodes_reach_percent: 5%", "expected a number"},
		{"raise_to_sos_when_silent_nodes_reach_percent: .nan", "expected a finite number"},
	} {
		text := "fleet_hub_role: {listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445', " + tc.settings + "}\ncheck_groups: {app: {shell: 'true'}}\n"
		if _, err := config.Load(testhelpers.ConfigFile(t, text)); err == nil || !strings.Contains(err.Error(), tc.problem) {
			t.Fatalf("%s: %v", tc.settings, err)
		}
	}
}

func TestFleetListenAddressRefusesInvalidBindings(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"hub.test", "127.0.0.1:70000"} {
		text := fmt.Sprintf("fleet_hub_role: {listen_on_address: %q, address_for_nodes: 'hub.test:8445'}\ncheck_groups: {app: {shell: 'true'}}\n", address)
		_, err := config.Load(testhelpers.ConfigFile(t, text))
		if err == nil || !strings.Contains(err.Error(), "fleet_hub_role.listen_on_address: expected the local host and port where MMWatchdog listens") || !strings.Contains(err.Error(), "10.0.0.5:8445") || !strings.Contains(err.Error(), "0.0.0.0:8445") {
			t.Fatal(address, err)
		}
	}
}

func TestIncidentKeysSeparateStandaloneNodesAndFleets(t *testing.T) {
	t.Parallel()
	first, second := mmhotlineNode(t), mmhotlineNode(t)
	first.store.Node, second.store.Node = "app-1", "app-2"
	now := time.Now().UTC()
	failure := func() checksandalerts.Event {
		return checksandalerts.Event{ID: incidents.ID(), Incident: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", Notify: true, At: now}
	}
	recordEvents(t, first, failure())
	recordEvents(t, second, failure())
	firstKey, secondKey := mmhotlineQueued(t, first)[0]["sender_incident_key"], mmhotlineQueued(t, second)[0]["sender_incident_key"]
	if !checksandalerts.FleetIncidentKeyPattern.MatchString(firstKey) || firstKey == secondKey {
		t.Fatal("standalone nodes shared an incident", firstKey, secondKey)
	}
	hub, code := startHub(t)
	one, two := joinedNode(t, code, "app-1"), joinedNode(t, code, "app-2")
	for _, d := range []*Daemon{hub, one, two} {
		recordEvents(t, d, failure())
	}
	key := func(d *Daemon) string {
		events, err := d.store.CheckGroupEvents("app", 1)
		if err != nil {
			t.Fatal(err)
		}
		return events[0].FleetIncidentKey
	}
	if key(hub) != key(one) || key(hub) != key(two) || key(hub) == firstKey {
		t.Fatal("fleet identities do not agree", key(hub), key(one), key(two))
	}
	otherHub, _ := startHub(t)
	recordEvents(t, otherHub, failure())
	if key(hub) == key(otherHub) {
		t.Fatal("independent fleets shared an incident")
	}
}

func TestScopedKeyKeepsTheCannotCheckKindOfRemindersAndClosings(t *testing.T) {
	t.Parallel()
	p, _ := daemonConfig(t)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := &incidents.CheckState{CheckGroup: "app", Check: "shell", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	now := time.Now().UTC()
	events := []checksandalerts.Event{}
	for _, kind := range []string{"cannot_check", "reminder", "closed_without_recovery"} {
		events = append(events, st.Event(&st.CannotCheckIncident, kind, "one-incident", "cannot check", now, false))
	}
	recordEvents(t, d, events...)
	stored, err := d.store.CheckGroupEvents("app", 10)
	if err != nil || len(stored) != 3 {
		t.Fatal(stored, err)
	}
	for _, event := range stored {
		if event.FleetIncidentKey != incidentKeyFor(d, "app/shell cannot check") {
			t.Fatal("incident kind changed", event)
		}
	}
}

func TestFallbackIncidentReportsFitTheRequestAndKeepTheRest(t *testing.T) {
	t.Parallel()
	pager := recordingDestination(t, "own_pager", testhelpers.AllSeverities)
	hub, code, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"ops": pager}, func(dir string) { installRecordingModules(t, dir) })
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	sample := store.IncidentSentWithoutTheHub{Destination: "ops", Incident: incidents.ID(), CheckGroup: strings.Repeat("g", 64), Check: strings.Repeat("c", 64), Type: "failed", AlertSeveritiesSentForIncident: []string{"warn"}}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	count := fleet.ReportLimit/len(encoded) + 100
	tx, err := node.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO incidents_sent_without_the_hub(destination,incident_id,check_group,check_name,finding,kind,alert_severities_sent_for_incident,at) VALUES('ops',?,?,?,'','failed','warn',?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	for i := 0; i < count; i++ {
		if _, err := statement.Exec(incidents.ID(), sample.CheckGroup, sample.Check, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	report := node.buildReport(time.Now())
	encoded, err = json.Marshal(report)
	if err != nil || len(encoded) > fleet.ReportLimit || len(report.IncidentsSentWithoutTheHub) == 0 || len(report.IncidentsSentWithoutTheHub) >= count {
		t.Fatal(len(encoded), len(report.IncidentsSentWithoutTheHub), count, err)
	}
	if err := report.Validate(); err != nil {
		t.Fatal("large batch refused", err)
	}
	carried := map[string]bool{}
	for _, incident := range report.IncidentsSentWithoutTheHub {
		carried[incident.Incident] = true
	}
	records, err := node.store.SentWithoutTheHub()
	if err != nil || len(records) != count {
		t.Fatal("report building lost records", len(records), err)
	}
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	node.report(context.Background())
	if node.link.LastError != "" || len(node.rep.IncidentsKnownToTheHub) != len(carried) {
		t.Fatal("full report batch was not confirmed", node.link.LastError, len(node.rep.IncidentsKnownToTheHub), len(carried))
	}
	t.Logf("report: %d bytes; confirmed %d incidents through HTTPS", len(encoded), len(carried))
	next := node.buildReport(time.Now())
	if len(next.IncidentsSentWithoutTheHub) == 0 {
		t.Fatal("remaining incidents did not advance")
	}
	for _, incident := range next.IncidentsSentWithoutTheHub {
		if carried[incident.Incident] {
			t.Fatal("confirmed incident sent again", incident)
		}
	}
}

func TestLargeFallbackBatchIsConfirmedThroughReportTransport(t *testing.T) {
	t.Parallel()
	pager := recordingDestination(t, "own_pager", testhelpers.AllSeverities)
	hub, code, _ := startHubWithModules(t, map[string]checksandalerts.Destination{"ops": pager}, func(dir string) { installRecordingModules(t, dir) })
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	tx, err := node.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO incidents_sent_without_the_hub(destination,incident_id,check_group,check_name,finding,kind,alert_severities_sent_for_incident,at) VALUES('ops',?,'app','shell','','failed','warn',?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	const count = 1201
	for i := 0; i < count; i++ {
		if _, err := statement.Exec(incidents.ID(), time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	node.report(context.Background())
	records, err := node.store.SentWithoutTheHub()
	if err != nil || len(records) != count {
		t.Fatal("unconfirmed records", len(records), err)
	}
	hub.reconcileReports(time.Now())
	node.report(context.Background())
	if node.link.LastError != "" {
		t.Fatal("report reply", node.link.LastError)
	}
	if len(node.rep.IncidentsKnownToTheHub) != count {
		t.Fatal("confirmed incidents", len(node.rep.IncidentsKnownToTheHub))
	}
	records, err = node.store.SentWithoutTheHub()
	if err != nil || len(records) != 0 {
		t.Fatal("confirmed records remain", len(records), err)
	}
	// A following report advances with a new incident, rather than resending the batch.
	if _, err := node.store.DB.Exec(`INSERT INTO incidents_sent_without_the_hub(destination,incident_id,check_group,check_name,finding,kind,alert_severities_sent_for_incident,at) VALUES('ops',?,'app','shell','','failed','warn',?)`, incidents.ID(), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	node.report(context.Background())
	records, err = node.store.SentWithoutTheHub()
	if err != nil || len(records) != 0 || node.link.LastError != "" {
		t.Fatal("following batch", len(records), node.link.LastError, err)
	}
}

func TestEveryFleetAlertUsesTheHubCertificateIdentity(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"hub check", "joined check", "silence", "hub unreachable"} {
		t.Run(path, func(t *testing.T) {
			hub, code, _ := startHubWith(t, mmhotlineDestinations(t))
			node := joinedNode(t, code, "app-1")
			node.report(context.Background())
			now := time.Now().UTC()
			readable := "app/shell failed"
			sender := hub
			switch path {
			case "hub check", "joined check":
				event := checkAlert("failed", incidents.ID(), "app", "shell", now)
				event.Notify = true
				if path == "hub check" {
					recordEvents(t, hub, event)
				} else {
					recordEvents(t, node, event)
					sender := fleet.HubSender{Code: code, Path: filepath.Join(node.config.StateDir, fleet.CredentialFile), Node: "app-1"}
					alerts := queuedFor(t, node, config.HubDestination)
					if len(alerts) != 1 {
						t.Fatal("node alerts", alerts)
					}
					if err := sender.Send(context.Background(), alerts[0]); err != nil {
						t.Fatal(err)
					}
				}
			case "silence":
				readable = "node_reports stopped reporting"
				tick(hub, fleetCheck(hub, "app-1").Deadline)
			case "hub unreachable":
				readable = "node_reports/to_hub failed"
				tick(node, fleetCheck(node, checksandalerts.HubCheck).Deadline)
				sender = node
			}
			if sender == hub {
				groupAt(t, hub, now.Add(time.Hour))
			}
			alerts := mmhotlineQueuedFor(t, sender)
			if len(alerts) != 1 {
				t.Fatal("MMHotline alerts", alerts)
			}
			// Derive the expectation from the certificate bytes and decision I,
			// independently of the production key helpers or the join-code parser.
			fingerprint := sha256.Sum256(hub.hub.Cert.Certificate[0])
			check := sha256.Sum256([]byte(readable))
			scoped := sha256.Sum256([]byte(fmt.Sprintf("fleet:%x\x00%x", fingerprint, check[:16])))
			if got, want := alerts[0]["sender_incident_key"], fmt.Sprintf("%x", scoped[:16]); got != want {
				t.Fatal("fleet identity", got, want)
			}
		})
	}
}
