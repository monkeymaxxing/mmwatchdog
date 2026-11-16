package checksandalerts_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

var testAlertBody = []byte(`{"sender_alert_id":"a1","severity":"warn","title":"web-3: myapp/http failed","message":""}`)

// The destination takes every alert source ID that MMHotline's settings
// file takes: 1 to 64 letters, digits, _ and -. The team names its alert
// sources there, such as mmwatchdog_fleet_hub, and an ID that MMHotline
// made earlier, 32 hexadecimal characters, still fits the rule.
func TestMMHotlineDestinationTakesTheAlertSourceIDsOfMMHotlinesSettingsFile(t *testing.T) {
	t.Parallel()
	for id, valid := range map[string]bool{
		"monitoring": true, "mmwatchdog_fleet_hub": true, "Web-Team_2": true, "2b84d6f1a3c5e7091b2d3f4a5c6e7f80": true, strings.Repeat("a", 64): true,
		"": false, strings.Repeat("a", 65): false, "web team": false, "web/team": false, "web.team": false, "équipe": false,
	} {
		err := (&checksandalerts.MMHotlineDestination{URL: "http://10.20.0.1:8484", AlertSourceID: id, SigningSecretFile: "/etc/mmwatchdog/secrets/mmhotline-signing-secret"}).Validate(checksandalerts.InputValidation{})
		want := "<nil>"
		if !valid {
			want = "mmhotline_alert_source_id: expected the alert_source_id of an alert source in MMHotline's settings file: 1 to 64 letters, digits, _ and -, such as monitoring"
		}
		got := "<nil>"
		if err != nil {
			got = err.Path + ": " + err.Message
		}
		if got != want {
			t.Errorf("%q: got %s, want %s", id, got, want)
		}
	}
}

func TestMMHotlineReceiptsDecideDelivery(t *testing.T) {
	for _, mode := range []string{"ok", "unsigned", "wrongly_signed", "other_route", "401", "503", "unsigned_400", "unsigned_413", "unsigned_401", "unsigned_503", "unsigned_200", "missing_receipt", "201"} {
		t.Run(mode, func(t *testing.T) {
			server := testhelpers.NewFakeMMHotline(t, mode)
			connection := &checksandalerts.MMHotlineConnection{}
			destination := checksandalerts.MMHotlineDestination{URL: server.URL + "/", AlertSourceID: testhelpers.TestSourceID, SigningSecretFile: "/etc/mmwatchdog/secrets/mmhotline-signing-secret"}
			_, err := connection.Send(context.Background(), "alert-intake", destination, testhelpers.TestSigningSecret, testAlertBody, time.Second)
			if (err == nil) != (mode == "ok") {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if strings.HasPrefix(mode, "unsigned_") && mode != "unsigned_401" {
				want := "MMHotline gave no signed answer (HTTP " + strings.TrimPrefix(mode, "unsigned_") + ")."
				if err.Error() != want {
					t.Fatalf("got %v; want %s", err, want)
				}
			}
			_, _, requests := server.Received()
			if requests != 1 {
				t.Fatalf("one attempt made %d requests", requests)
			}
			if mode == "ok" {
				alerts, _, _ := server.Received()
				if len(alerts) != 1 || alerts[0]["sender_alert_id"] != "a1" {
					t.Fatal(alerts)
				}
			}
		})
	}
}

func TestMMHotlineRefusalsRateLimitsAndClock(t *testing.T) {
	server := testhelpers.NewFakeMMHotline(t, "400")
	connection := &checksandalerts.MMHotlineConnection{}
	destination := checksandalerts.MMHotlineDestination{URL: server.URL, AlertSourceID: testhelpers.TestSourceID}
	send := func() error {
		_, err := connection.Send(context.Background(), "alert-intake", destination, testhelpers.TestSigningSecret, testAlertBody, time.Second)
		return err
	}
	var refusal *checksandalerts.MMHotlineRefusal
	if err := send(); !errors.As(err, &refusal) || refusal.Status != 400 || !strings.HasPrefix(err.Error(), "MMHotline permanently refused this alert (HTTP 400):") {
		t.Fatal(err)
	}
	// A signed answer records the clock; a later unsigned answer cannot change it.
	server.SetMode("429")
	server.SetDate(time.Now().Add(7 * time.Minute))
	var later *checksandalerts.RetryLater
	if err := send(); !errors.As(err, &later) || later.After != 7*time.Second {
		t.Fatal(err)
	}
	if clock := connection.ClockProblem(checksandalerts.ClockShownAfter); !strings.Contains(clock, "7 minutes behind MMHotline's") {
		t.Fatal(clock)
	}
	if connection.LastSignedAnswerAt.IsZero() {
		t.Fatal("no signed-answer time")
	}
	server.SetMode("unsigned")
	server.SetDate(time.Now().Add(-time.Hour))
	send()
	if clock := connection.ClockProblem(checksandalerts.ClockShownAfter); !strings.Contains(clock, "7 minutes behind MMHotline's") {
		t.Fatal(clock)
	}
}

func TestMMHotlineClockHintFromRefusedSignatures(t *testing.T) {
	server := testhelpers.NewFakeMMHotline(t, "401")
	server.SetDate(time.Now().Add(7 * time.Minute))
	connection := &checksandalerts.MMHotlineConnection{}
	destination := checksandalerts.MMHotlineDestination{URL: server.URL, AlertSourceID: testhelpers.TestSourceID, SigningSecretFile: "/etc/mmwatchdog/secrets/mmhotline-signing-secret"}
	_, err := connection.Send(context.Background(), "alert-intake", destination, testhelpers.TestSigningSecret, testAlertBody, time.Second)
	want := "MMHotline rejected the signature (HTTP 401). MMHotline's settings file may no longer list this signing secret or this alert source. Write the signing secret that it lists for the alert source into /etc/mmwatchdog/secrets/mmhotline-signing-secret and run sudo mmwatchdog reload. This node's clock may be 7 minutes behind MMHotline's; MMHotline refuses requests more than 5 minutes off."
	if err == nil || err.Error() != want {
		t.Fatal(err)
	}
	if connection.ClockProblem(checksandalerts.ClockShownAfter) != "" || connection.OffsetKnown || !connection.LastSignedAnswerAt.IsZero() {
		t.Fatal("unsigned answer changed trusted status")
	}
}

// Titles, messages, and the whole encoded body are fitted to MMHotline's
// limits before queueing, so no alert can meet a 413 that would hold up
// every alert behind it.
func TestMMHotlineAlertFitsLimits(t *testing.T) {
	t.Parallel()
	for _, filler := range []string{"é", "<", "\x01", "\""} {
		message := checksandalerts.Event{ID: incidents.ID(), Incident: incidents.ID(), Node: "web-3", CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "warn", At: time.Now(), Message: strings.Repeat(filler, 20*1024)}
		queued := store.MMHotlineFailure(message, []checksandalerts.Event{message}, store.QueuedBy{Role: store.StandaloneSender, Node: strings.Repeat("n", 400)}, "warn")
		body := queued.MMHotlineAlert.Encode()
		a, e := testhelpers.DecodeMMHotlineAlert(body)
		if e != nil || len(body) > 64*1024 || a["title"] != "web-3: myapp/http failed" || a["sender_incident_key"] != queued.FleetIncidentKey || !checksandalerts.FleetIncidentKeyPattern.MatchString(a["sender_incident_key"]) || a["severity"] != "warn" || a["sender_alert_id"] != message.ID {
			t.Fatal(filler, len(body), e, a["title"])
		}
	}
	// Valid long names that differ after 256 bytes must keep different keys.
	message := checksandalerts.Event{ID: incidents.ID(), Type: "failed", CheckGroup: strings.Repeat("g", 64), Check: strings.Repeat("c", 64), Finding: strings.Repeat("f", 199) + "a"}
	first := store.MMHotlineFailure(message, []checksandalerts.Event{message}, store.QueuedBy{Role: store.StandaloneSender, Node: "web-3"}, "sos").MMHotlineAlert
	message.Finding = strings.Repeat("f", 199) + "b"
	second := store.MMHotlineFailure(message, []checksandalerts.Event{message}, store.QueuedBy{Role: store.StandaloneSender, Node: "web-3"}, "sos").MMHotlineAlert
	if len(first.SenderIncidentKey) != 32 || first.SenderIncidentKey == second.SenderIncidentKey {
		t.Fatal("long names lost their distinct keys", first.SenderIncidentKey, second.SenderIncidentKey)
	}
	long := store.MMHotlineEvent(checksandalerts.Event{}, "x", "", "info", strings.Repeat("ü", 300), nil).MMHotlineAlert
	if n := len([]rune(long.Title)); n != store.MMHotlineTitleLimit {
		t.Fatal("title not fitted", n)
	}
}

// A check-in stays within MMHotline's limits: at most 1,000 nodes,
// and under 60 KiB encoded, leaving out reasons and users before windows.
func TestCheckInFitsMMHotlineLimits(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	// The sender's own name sorts after every other node's, and it is in a
	// whole-node window too: its window must still be listed, first.
	message := func(n int, reason string) fleet.CheckIn {
		m := fleet.CheckIn{SentAt: until, SentByNodeName: "zz-util"}
		for i := 0; i < n; i++ {
			m.NodesInWholeNodeMaintenance = append(m.NodesInWholeNodeMaintenance, fleet.NodeInMaintenance{NodeName: fmt.Sprintf("n%04d", i), MaintenanceUntil: until, Reason: reason, StartedBy: "matt"})
		}
		m.NodesInWholeNodeMaintenance = append(m.NodesInWholeNodeMaintenance, fleet.NodeInMaintenance{NodeName: "zz-util", MaintenanceUntil: until, Reason: reason, StartedBy: "matt"})
		return m
	}
	first := func(listed []any) map[string]any { return listed[0].(map[string]any) }
	body, note := fleet.FitCheckIn(message(1300, strings.Repeat("r", 200)), fleet.CheckInBytes, fleet.MaxNodesInWholeNodeMaintenance)
	checkIn, e := testhelpers.DecodeMMHotlineCheckIn(body)
	listed := checkIn["nodes_in_whole_node_maintenance"].([]any)
	if e != nil || len(body) > 60*1024 || len(listed) < 900 || first(listed)["node_name"] != "zz-util" || first(listed)["reason"] != nil ||
		!strings.Contains(note, "of the 1301 nodes in whole-node maintenance, without reasons and users,") {
		t.Fatal(len(body), len(listed), e, note)
	}
	// Short windows stop at 1,000 nodes, with their reasons, unescaped.
	body, note = fleet.FitCheckIn(message(1300, "deploy <v2> & cache"), 1<<20, fleet.MaxNodesInWholeNodeMaintenance)
	checkIn = nil
	json.Unmarshal(body, &checkIn)
	listed = checkIn["nodes_in_whole_node_maintenance"].([]any)
	if len(listed) != 1000 || first(listed)["node_name"] != "zz-util" || !strings.Contains(string(body), `"reason":"deploy <v2> & cache"`) || !strings.HasPrefix(note, "The check-in listed 1000 of the 1301 nodes") {
		t.Fatal(len(listed), note)
	}
	if _, note = fleet.FitCheckIn(message(3, "deploy"), fleet.CheckInBytes, fleet.MaxNodesInWholeNodeMaintenance); note != "" {
		t.Fatal(note)
	}
}

// A reboot notice reaches MMHotline at the severity its node states for its
// own reboot, without an incident key. A maintenance notice is always info.
// A notice sent with mmwatchdog send-notice has the severity its command stated,
// and its body is the text its sender gave.
func TestMMHotlineNoticeSeverity(t *testing.T) {
	t.Parallel()
	from := store.QueuedBy{Role: store.StandaloneSender, Node: "web-3"}
	for _, tc := range []struct {
		event           checksandalerts.Event
		severity, title string
	}{
		{checksandalerts.Event{ID: incidents.ID(), Node: "web-3", Type: "rebooted", AlertSeverity: "warn", Message: "web-3 rebooted at 14:03 UTC; MMWatchdog last ran at 14:01 UTC"}, "warn", "web-3 rebooted"},
		{checksandalerts.Event{ID: incidents.ID(), Node: "db-1", Type: "rebooted", AlertSeverity: "sos", Message: "db-1 rebooted at 14:03 UTC; MMWatchdog last ran at 14:01 UTC"}, "sos", "db-1 rebooted"},
		{checksandalerts.Event{ID: incidents.ID(), Type: "rebooted", AlertSeverity: "info", NodeCount: 3, Message: "3 nodes rebooted: app-1, app-2, app-3"}, "info", "3 nodes rebooted"},
		{incidents.MaintenanceEvent("maintenance_on", []string{"app"}, checksandalerts.Window{Until: time.Now().Add(time.Hour)}, time.Now(), "maintenance on"), "info", ": maintenance on for app"},
		{checksandalerts.Event{ID: incidents.ID(), Node: "web-3", Type: "notice", AlertSeverity: "sos", Message: "Test of the alert destinations. Nothing is wrong."}, "sos", "web-3: Test of the alert destinations. Nothing is wrong."},
		{checksandalerts.Event{ID: incidents.ID(), Node: "web-3", Type: "notice", AlertSeverity: "info", Message: "backup finished"}, "info", "web-3: backup finished"},
	} {
		alert := store.MMHotlineNotice(tc.event, from).MMHotlineAlert
		if alert == nil || alert.Severity != tc.severity || alert.Title != tc.title || alert.SenderIncidentKey != "" {
			t.Fatalf("%s: %+v", tc.event.Type, alert)
		}
	}
	sent := checksandalerts.Event{ID: incidents.ID(), Node: "web-3", Type: "notice", AlertSeverity: "warn", Message: "deploy finished", Body: "version 1.4"}
	if alert := store.MMHotlineNotice(sent, from).MMHotlineAlert; alert.Message != "version 1.4\nSent by the standalone MMWatchdog node web-3." {
		t.Fatalf("%q", alert.Message)
	}
	// Without a body, MMHotline's message says only who sent the notice.
	sent.Body = ""
	if alert := store.MMHotlineNotice(sent, from).MMHotlineAlert; alert.Message != "Sent by the standalone MMWatchdog node web-3." {
		t.Fatalf("%q", alert.Message)
	}
}

func TestMMHotlineURLValidation(t *testing.T) {
	t.Parallel()
	addressError := "expected MMHotline's address: http:// or https://, a host, and an optional port and path, such as http://127.0.0.1:8484"
	routeError := "write MMHotline's address without the route, such as http://10.20.0.1:8484; MMWatchdog adds /alert-intake/{alert source ID} itself"
	for _, test := range []struct{ address, message string }{
		{"", addressError}, {"hotline.example.com", addressError}, {"ftp://hotline.example.com", addressError}, {"http:///prefix", addressError},
		{"http://user:password@hotline.example.com", addressError}, {"https://hotline.example.com?x=1", addressError}, {"https://hotline.example.com?", addressError}, {"https://hotline.example.com#fragment", addressError}, {"https://hotline.example.com#", addressError}, {"http://hotline.example.com:bad", addressError},
		{"https://hotline.example.com/alert-intake/source", routeError}, {"https://hotline.example.com/check-in/source", routeError}, {"https://hotline.example.com/prefix/alert-intake", routeError}, {"https://hotline.example.com/prefix/check-in/", routeError},
		{"http://127.0.0.1:8484", ""}, {"https://hotline.example.com/prefix/", ""}, {"http://[::1]:8484", ""},
	} {
		t.Run(test.address, func(t *testing.T) {
			destination := checksandalerts.MMHotlineDestination{URL: test.address, AlertSourceID: testhelpers.TestSourceID, SigningSecretFile: "/etc/mmwatchdog/secrets/mmhotline"}
			problem := destination.Validate(checksandalerts.InputValidation{})
			if test.message == "" {
				if problem != nil {
					t.Fatal(problem)
				}
				return
			}
			if problem == nil || problem.Path != "mmhotline_url" || problem.Message != test.message {
				t.Fatal(problem)
			}
		})
	}
}

func TestMMHotlineRequestsKeepTheAddressPathPrefix(t *testing.T) {
	t.Parallel()
	fake := testhelpers.NewFakeMMHotline(t, "ok")
	server := httptest.NewServer(http.StripPrefix("/prefix", fake.Config.Handler))
	defer server.Close()
	destination := checksandalerts.MMHotlineDestination{URL: server.URL + "/prefix/", AlertSourceID: testhelpers.TestSourceID}
	connection := &checksandalerts.MMHotlineConnection{}
	if _, err := connection.Send(context.Background(), "alert-intake", destination, testhelpers.TestSigningSecret, testAlertBody, time.Second); err != nil {
		t.Fatal(err)
	}
	checkIn := []byte(`{"sent_at":"2026-10-23T00:00:00Z","sent_by_node_name":"web-3"}`)
	if _, err := connection.Send(context.Background(), "check-in", destination, testhelpers.TestSigningSecret, checkIn, time.Second); err != nil {
		t.Fatal(err)
	}
	alerts, messages, requests := fake.Received()
	if len(alerts) != 1 || len(messages) != 1 || requests != 2 {
		t.Fatal(alerts, messages, requests)
	}
}
