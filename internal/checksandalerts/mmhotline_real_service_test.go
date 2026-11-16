package checksandalerts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

// TestRealServiceMMHotline sends an alert, the same alert again, check-ins, an SOS and its resolved-sos two minutes later, an alert that
// MMHotline can never take, and a request with a wrong signing secret to a
// real MMHotline, and checks each answer as a delivery does. The SOS wakes
// the person on call. It skips without its three environment variables;
// see docs/changing-mmwatchdog/testing.md, section "Real services before a release".
func TestRealServiceMMHotline(t *testing.T) {
	names := []string{"MMWATCHDOG_REAL_SERVICE_TEST_MMHOTLINE_URL", "MMWATCHDOG_REAL_SERVICE_TEST_MMHOTLINE_ALERT_SOURCE_ID", "MMWATCHDOG_REAL_SERVICE_TEST_MMHOTLINE_SIGNING_SECRET"}
	values := map[string]string{}
	for _, name := range names {
		if values[name] = os.Getenv(name); values[name] == "" {
			t.Skipf("%s is empty; see docs/changing-mmwatchdog/testing.md, section \"Real services before a release\"", name)
		}
	}
	secret, err := checksandalerts.DecodeSigningSecret(values[names[2]])
	if err != nil {
		t.Fatalf("%s: %v", names[2], err)
	}
	destination := checksandalerts.MMHotlineDestination{URL: values[names[0]], AlertSourceID: values[names[1]], SigningSecretFile: "/etc/mmwatchdog/secrets/mmhotline-signing-secret"}
	if problem := destination.Validate(checksandalerts.InputValidation{}); problem != nil {
		t.Fatalf("%s: %s", problem.Path, problem.Message)
	}
	connection := &checksandalerts.MMHotlineConnection{}
	send := func(route string, body, key []byte) ([]byte, error) {
		return connection.Send(context.Background(), route, destination, key, body, 10*time.Second)
	}
	type receipt struct {
		AlertID         string `json:"mmhotline_alert_id"`
		IncidentID      string `json:"mmhotline_incident_id"`
		AlreadyReceived bool   `json:"already_received"`
	}
	readReceipt := func(answer []byte) receipt {
		var r receipt
		if err := json.Unmarshal(answer, &r); err != nil || r.AlertID == "" {
			t.Fatalf("receipt %s: %v", answer, err)
		}
		return r
	}

	// An info notice, built as MMWatchdog queues it, is one alert with a
	// signed receipt; MMHotline opens no incident for it.
	node := "mmwatchdog-real-service-test"
	notice := checksandalerts.Event{ID: incidents.ID(), Node: node, Type: "notice", AlertSeverity: "info", At: time.Now().UTC(), Message: "Test of the mmhotline destination. Nothing is wrong."}
	alert := store.MMHotlineNotice(notice, store.QueuedBy{Role: store.StandaloneSender, Node: node}).MMHotlineAlert
	answer, err := send("alert-intake", alert.Encode(), secret)
	if err != nil {
		t.Fatal(err)
	}
	first := readReceipt(answer)
	if first.AlreadyReceived {
		t.Fatalf("a new alert was already received: %s", answer)
	}
	t.Logf("MMHotline accepted the alert %q as %s", alert.Title, first.AlertID)

	// The same alert again, as a retried delivery sends it, is the same alert.
	if answer, err = send("alert-intake", alert.Encode(), secret); err != nil {
		t.Fatal(err)
	}
	if again := readReceipt(answer); !again.AlreadyReceived || again.AlertID != first.AlertID {
		t.Fatalf("the retried alert: %s", answer)
	}

	// A check-in, as the standalone node sends it every 30 seconds.
	sendCheckIn := func() {
		checkIn, _ := fleet.FitCheckIn(fleet.CheckIn{SentAt: time.Now().UTC(), SentByNodeName: node}, fleet.CheckInBytes, fleet.MaxNodesInWholeNodeMaintenance)
		answer, err := send("check-in", checkIn, secret)
		if err != nil {
			t.Fatal(err)
		}
		var received struct {
			ReceivedAt string `json:"check_in_received_at"`
		}
		if err := json.Unmarshal(answer, &received); err != nil || received.ReceivedAt == "" {
			t.Fatalf("check-in receipt %s: %v", answer, err)
		}
	}
	sendCheckIn()

	// An SOS, built as a standalone node queues it, opens an MMHotline
	// incident that wakes the person on call. Its resolved-sos, under the
	// same key, resolves that incident and stops the repeats.
	from := store.QueuedBy{Role: store.StandaloneSender, Node: node}
	failure := checksandalerts.Event{ID: incidents.ID(), Incident: incidents.ID(), Node: node, CheckGroup: "real_service_test", Check: "mmhotline_sos", Type: "failed", AlertSeverity: "sos", At: time.Now().UTC(), Message: "Test of the mmhotline destination: this SOS is a test, and MMWatchdog resolves it in two minutes."}
	sos := store.MMHotlineFailure(failure, []checksandalerts.Event{failure}, from, "sos").MMHotlineAlert
	if answer, err = send("alert-intake", sos.Encode(), secret); err != nil {
		t.Fatal(err)
	}
	opened := readReceipt(answer)
	if opened.IncidentID == "" {
		t.Fatalf("the SOS opened no incident: %s", answer)
	}
	t.Logf("MMHotline opened the incident %s for the SOS %q. It rings the person on call now: acknowledge it in Pushover to claim the incident. The resolved-sos follows in two minutes.", opened.IncidentID, sos.Title)
	// The two minutes of waiting keep the check-ins coming, so
	// MMHotline does not warn that they stopped.
	for range 4 {
		time.Sleep(30 * time.Second)
		sendCheckIn()
	}
	recovered := checksandalerts.Event{ID: incidents.ID(), Node: node, CheckGroup: failure.CheckGroup, Check: failure.Check, Type: "recovered", At: time.Now().UTC()}
	title := node + ": " + failure.AboutCheck() + " is ok again"
	resolution := store.MMHotlineEvent(recovered, recovered.ID, sos.SenderIncidentKey, "resolved-sos", title, []string{title + ".", from.SentBy()}).MMHotlineAlert
	if answer, err = send("alert-intake", resolution.Encode(), secret); err != nil {
		t.Fatal(err)
	}
	if resolved := readReceipt(answer); resolved.IncidentID != opened.IncidentID {
		t.Fatalf("the resolved-sos joined another incident: %s", answer)
	}

	// An alert without a title is permanently refused, with a signed 400.
	invalid := checksandalerts.MMHotlineAlert{SenderAlertID: incidents.ID(), Severity: "info", Title: "", Message: ""}
	var refusal *checksandalerts.MMHotlineRefusal
	if _, err = send("alert-intake", invalid.Encode(), secret); !errors.As(err, &refusal) {
		t.Fatalf("an alert without a title: %v", err)
	}

	// A wrong signing secret gets the unsigned 401 and its message.
	if _, err = send("alert-intake", alert.Encode(), bytes.Repeat([]byte{0x01}, 32)); err == nil || !strings.HasPrefix(err.Error(), "MMHotline rejected the signature (HTTP 401).") {
		t.Fatalf("a wrong signing secret: %v", err)
	}

	if connection.LastSignedAnswerAt.IsZero() {
		t.Fatal("no signed answer was recorded")
	}
	if clock := connection.ClockProblem(checksandalerts.ClockShownAfter); clock != "" {
		t.Fatal(clock)
	}
}
