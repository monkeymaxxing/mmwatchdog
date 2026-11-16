package official_extra_modules

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// The two secrets of a pushover destination, in the form that Pushover
// gives them: 30 letters and digits each.
const (
	pushoverApplicationToken = "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5"
	pushoverUserOrGroupKey   = "u9V8w7X6y5Z4a3B2c1D0e9F8g7H6i5"
)

// pushoverFleetIncidentKey is the fleet incident key of the failures of the
// check myapp/http: every alert about them carries it, on every node of a
// fleet and on a standalone node.
const pushoverFleetIncidentKey = "5925e47138c3e039dd7d115ca91fefaf"

// pushoverTag is the tag of the emergency messages about the failures of the
// check myapp/http, on every node.
const pushoverTag = "mmwatchdog_fleet_" + pushoverFleetIncidentKey

// pushoverFailure is the alert failure with the fleet incident key of its
// check, as MMWatchdog sends it.
var pushoverFailure = func() checksandalerts.Event {
	e := failure
	e.FleetIncidentKey = pushoverFleetIncidentKey
	return e
}()

var pushoverFileContents = map[string]string{"application_token_file": pushoverApplicationToken, "user_or_group_key_file": pushoverUserOrGroupKey}

func pushoverInputs(server string) map[string]any {
	return map[string]any{
		"application_token_file":          "/etc/mmwatchdog/secrets/pushover-application-token",
		"user_or_group_key_file":          "/etc/mmwatchdog/secrets/pushover-on-call-group-key",
		"repeat_sos_alerts_every":         "5m",
		"stop_repeating_sos_alerts_after": "1h",
		"api_server_url":                  server,
	}
}

// pushover answers as Pushover's API server does: it takes a message of at
// most 1,024 characters with a title of at most 250, answers an emergency
// message with a receipt, and stops the repeats of a tag.
func pushover(t *testing.T) *service {
	return startService(t, func(request received) (int, string) {
		if strings.HasPrefix(request.path, "/1/receipts/cancel_by_tag/") {
			return 200, `{"status":1,"request":"8b7e3c43-5d76-4b2e-9b1d-0f5c4b6a1e01"}`
		}
		var message struct {
			Title, Message string
			Priority       int
		}
		json.Unmarshal(request.body, &message)
		if utf8.RuneCountInString(message.Message) > 1024 {
			return 400, `{"message":"invalid","errors":["message cannot be longer than 1024 characters"],"status":0,"request":"5042853c-402d-4a18-abcb-168734a801de"}`
		}
		if utf8.RuneCountInString(message.Title) > 250 {
			return 400, `{"title":"invalid","errors":["title cannot be longer than 250 characters"],"status":0,"request":"5042853c-402d-4a18-abcb-168734a801de"}`
		}
		if message.Priority == 2 {
			return 200, `{"status":1,"request":"647d2300-702c-4b38-8b2f-d56326ae460b","receipt":"rLqVNmUXW6nKXSf3WZibBoTL2ZsMa1"}`
		}
		return 200, `{"status":1,"request":"647d2300-702c-4b38-8b2f-d56326ae460b"}`
	})
}

// pushoverMessage reads a request to Pushover's Message API.
func pushoverMessage(t *testing.T, request received) map[string]any {
	t.Helper()
	if request.method != "POST" || request.contentType != "application/json" || request.path != "/1/messages.json" {
		t.Fatalf("not a JSON POST to the Message API: %+v", request)
	}
	var message map[string]any
	decoder := json.NewDecoder(bytes.NewReader(request.body))
	decoder.UseNumber()
	if e := decoder.Decode(&message); e != nil {
		t.Fatal(e, string(request.body))
	}
	return message
}

// isPushoverStop reports a request that stops the repeats of the emergency
// messages with a tag: a form with the application's token, and the tag in
// the address.
func isPushoverStop(request received, tag string) bool {
	return request.method == "POST" && request.contentType == "application/x-www-form-urlencoded" && request.path == "/1/receipts/cancel_by_tag/"+tag+".json" && string(request.body) == "token="+pushoverApplicationToken
}

// asJSON writes a value as JSON with its keys in order, so that two
// messages compare as texts.
func asJSON(t *testing.T, value any) string {
	t.Helper()
	b, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

// pushoverKeepsTheSecrets fails the test when a secret of the destination
// is in what the module wrote.
func pushoverKeepsTheSecrets(t *testing.T, o outcome) {
	t.Helper()
	if o.holds(pushoverApplicationToken) || o.holds(pushoverUserOrGroupKey) {
		t.Fatalf("a secret of the destination is in what the module wrote: %+v", o)
	}
}

// The pushover module sends each alert as one message: the title with the
// alert's severity in front, as MMWatchdog wrote it, as the message's title, and the
// body as the message, in Pushover's monospace font. An sos alert is an
// emergency, which Pushover repeats until somebody acknowledges it, with the
// tag of its check's fleet incident key when it has one; warn is Pushover's
// normal priority, and info its low priority.
func TestPushoverSendsTheTitleAndTheBody(t *testing.T) {
	t.Parallel()
	server := pushover(t)
	warn := pushoverFailure
	warn.AlertSeverity, warn.TitleWithAlertSeverity = "warn", "Warning: app-1: myapp/http failed"
	raised := pushoverFailure
	raised.Type, raised.Title, raised.TitleWithAlertSeverity, raised.Body = "failure_alert_severity_raised", "app-1: myapp/http still failing, now SOS", "SOS: app-1: myapp/http still failing, now SOS", "myapp/http has been failing for 30m, so this alert is now SOS (raise_to_sos_after_failing_for: 30m)\nfailing since 14:02 UTC"
	cannotCheck := pushoverFailure
	cannotCheck.Type, cannotCheck.Incident, cannotCheck.FleetIncidentKey, cannotCheck.Title, cannotCheck.TitleWithAlertSeverity, cannotCheck.Body = "cannot_check", "00112233445566778899aabbccddeeff", "b9ee4efcc5ba8639466c127ff2f549c4", "app-1: MMWatchdog cannot check myapp/http", "SOS: app-1: MMWatchdog cannot check myapp/http", "MMWatchdog has not been able to run the check since 14:02 UTC"
	onlyTheTitle := pushoverFailure
	onlyTheTitle.Body, onlyTheTitle.Message = "", ""
	// A notice and a reboot notice have no fleet incident key, so their
	// emergency messages have no tag. A notice without --body has no body.
	notice := checksandalerts.Event{ID: "11111111111111111111111111111111", Node: "app-1", Type: "notice", AlertSeverity: "info", Title: "app-1: Test of Pushover. Nothing is wrong.", TitleWithAlertSeverity: "Info: app-1: Test of Pushover. Nothing is wrong.", Message: "Test of Pushover. Nothing is wrong."}
	sosNotice := notice
	sosNotice.AlertSeverity, sosNotice.TitleWithAlertSeverity = "sos", "SOS: app-1: Test of Pushover. Nothing is wrong."
	rebooted := checksandalerts.Event{ID: "22222222222222222222222222222222", Node: "app-1", Type: "rebooted", AlertSeverity: "sos", Title: "app-1 rebooted", TitleWithAlertSeverity: "SOS: app-1 rebooted", Body: "the node booted at 14:01 UTC; MMWatchdog last ran at 13:58 UTC"}
	maintenance := checksandalerts.Event{ID: "33333333333333333333333333333333", Node: "app-1", CheckGroups: []string{"every_check_group"}, Type: "maintenance_on", AlertSeverity: "info", Title: "app-1: maintenance on for the whole node", TitleWithAlertSeverity: "Info: app-1: maintenance on for the whole node", Body: "maintenance until 2026-09-29 16:00 UTC, turned on by barbara: kernel update"}
	// The hub's message about the same failure of several nodes has no
	// node and no incident, and the key of the check, so its emergency
	// message has the tag of the check.
	grouped := checksandalerts.Event{ID: "44444444444444444444444444444444", FleetIncidentKey: pushoverFleetIncidentKey, CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "sos", Nodes: []string{"app-2", "app-3"}, NodeCount: 2, Title: "myapp/http failed on 2 nodes", TitleWithAlertSeverity: "SOS: myapp/http failed on 2 nodes", Body: "myapp/http failed on 2 nodes: app-2, app-3\nfrom app-2:\n  expected HTTP 200, received 503\n  failing since 14:02 UTC"}
	warnReminder := warn
	warnReminder.Type, warnReminder.Title, warnReminder.TitleWithAlertSeverity, warnReminder.Body = "reminder", "app-1: myapp/http still failing", "Warning: app-1: myapp/http still failing", "myapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503"
	warnRecovery := warn
	warnRecovery.Type, warnRecovery.Title, warnRecovery.TitleWithAlertSeverity, warnRecovery.Body = "recovered", "app-1: myapp/http is ok again", "Resolved warning: app-1: myapp/http is ok again", "HTTP 200"
	infoRecovery := warnRecovery
	infoRecovery.AlertSeverity, infoRecovery.TitleWithAlertSeverity = "info", "Resolved info: app-1: myapp/http is ok again"
	odd := pushoverFailure
	odd.TitleWithAlertSeverity, odd.Body = "SOS: app-1: "+awkward[:22], awkward
	secrets := map[string]any{"token": pushoverApplicationToken, "user": pushoverUserOrGroupKey}
	with := func(fields map[string]any) map[string]any {
		message := map[string]any{}
		for _, part := range []map[string]any{secrets, fields} {
			for key, value := range part {
				message[key] = value
			}
		}
		return message
	}
	for _, tc := range []struct {
		name  string
		alert checksandalerts.Event
		want  map[string]any
	}{
		{"an sos failure", pushoverFailure, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "tags": pushoverTag, "title": "SOS: app-1: myapp/http failed", "message": "expected HTTP 200, received 503\nfailing since 14:02 UTC", "monospace": 1})},
		// MMWatchdog gives every alert of an incident the key. The module builds the
		// tag of a check from nothing else, so a failure without it has no
		// tag.
		{"an sos failure without a fleet incident key", failure, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "title": "SOS: app-1: myapp/http failed", "message": "expected HTTP 200, received 503\nfailing since 14:02 UTC", "monospace": 1})},
		{"a warn failure", warn, with(map[string]any{"priority": 0, "title": "Warning: app-1: myapp/http failed", "message": "expected HTTP 200, received 503\nfailing since 14:02 UTC", "monospace": 1})},
		{"a raise to sos", raised, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "tags": pushoverTag, "title": raised.TitleWithAlertSeverity, "message": raised.Body, "monospace": 1})},
		{"an sos alert that MMWatchdog cannot check", cannotCheck, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "tags": "mmwatchdog_fleet_b9ee4efcc5ba8639466c127ff2f549c4", "title": cannotCheck.TitleWithAlertSeverity, "message": cannotCheck.Body, "monospace": 1})},
		// Pushover takes no message without text: the title is the message,
		// and Pushover shows the application's name above it.
		{"only the title", onlyTheTitle, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "tags": pushoverTag, "message": "SOS: app-1: myapp/http failed"})},
		{"an info notice", notice, with(map[string]any{"priority": -1, "message": "Info: app-1: Test of Pushover. Nothing is wrong."})},
		{"an sos notice", sosNotice, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "message": "SOS: app-1: Test of Pushover. Nothing is wrong."})},
		{"an sos reboot notice", rebooted, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "title": "SOS: app-1 rebooted", "message": rebooted.Body, "monospace": 1})},
		{"a maintenance alert", maintenance, with(map[string]any{"priority": -1, "title": maintenance.TitleWithAlertSeverity, "message": maintenance.Body, "monospace": 1})},
		{"a grouped sos failure of the hub", grouped, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "tags": pushoverTag, "title": grouped.TitleWithAlertSeverity, "message": grouped.Body, "monospace": 1})},
		{"a warn reminder", warnReminder, with(map[string]any{"priority": 0, "title": warnReminder.TitleWithAlertSeverity, "message": warnReminder.Body, "monospace": 1})},
		// No sos alert of the incident went out, so nothing repeats.
		{"the recovery of a warn incident", warnRecovery, with(map[string]any{"priority": 0, "title": "Resolved warning: app-1: myapp/http is ok again", "message": "HTTP 200", "monospace": 1})},
		{"the recovery of an info incident", infoRecovery, with(map[string]any{"priority": -1, "title": "Resolved info: app-1: myapp/http is ok again", "message": "HTTP 200", "monospace": 1})},
		// Pushover reads no formatting in a message in its monospace font,
		// and in no title, so the text goes as it is.
		{"awkward text", odd, with(map[string]any{"priority": 2, "retry": 300, "expire": 3600, "tags": pushoverTag, "title": "SOS: app-1: it said \"no\" & 'never'", "message": awkward, "monospace": 1})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(server.got())
			o := run(t, "pushover", tc.alert, pushoverInputs(server.URL+"/"), pushoverFileContents)
			if o.exitStatus != 0 || o.status != "delivered" || o.message != "Pushover accepted the message" || o.stderr != "" {
				t.Fatalf("%+v", o)
			}
			requests := server.got()[before:]
			if len(requests) != 1 {
				t.Fatalf("%d requests: %+v", len(requests), requests)
			}
			if got, want := asJSON(t, pushoverMessage(t, requests[0])), asJSON(t, tc.want); got != want {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
			pushoverKeepsTheSecrets(t, o)
		})
	}
}

// MMWatchdog sends the destination one closing alert for each check, when the last
// node recovers. When the check was sos, that alert first stops the repeats
// of the check's emergency messages on every node, by the check's tag, and
// then sends its own message at normal priority, such as "Resolved SOS: ...".
// When several nodes failed, the closing alert is a new event without a node
// and an incident, with the key of the check. A reminder of an sos incident
// stops nothing: it goes to one destination, and the stop by tag would also
// silence the other destinations that share the application's token while
// the check fails. It starts repeats of its own, with the check's tag, which
// the closing alert stops with the earlier ones. The closing alert stops the
// repeats only when an sos alert of the check went to its destination, as
// alert_severities_sent_for_incident says, or, without that list, when its
// own severity is sos: a stop by tag also reaches the other destinations
// with the same application token, which can still repeat an sos alert of a
// node that fails.
func TestPushoverStopsTheRepeatsOfAnSosIncident(t *testing.T) {
	t.Parallel()
	server := pushover(t)
	recovery := pushoverFailure
	recovery.Type, recovery.Title, recovery.TitleWithAlertSeverity, recovery.Body = "recovered", "app-1: myapp/http is ok again", "Resolved SOS: app-1: myapp/http is ok again", "HTTP 200"
	recovery.AlertSeveritiesSentForIncident = []string{"warn", "sos"}
	canCheckAgain := pushoverFailure
	canCheckAgain.Type, canCheckAgain.Title, canCheckAgain.TitleWithAlertSeverity, canCheckAgain.Body = "can_check_again", "app-1: MMWatchdog can check myapp/http again", "Resolved SOS: app-1: MMWatchdog can check myapp/http again", "MMWatchdog can run the check again"
	retired := pushoverFailure
	retired.Type, retired.Title, retired.TitleWithAlertSeverity, retired.Body = "closed_without_recovery", "app-1: myapp/http incident closed, maybe not fixed", "Closed SOS: app-1: myapp/http incident closed, maybe not fixed", "the check was removed from the configuration; MMWatchdog does not know whether the problem is fixed"
	recoveryOnlyTheTitle := recovery
	recoveryOnlyTheTitle.Body, recoveryOnlyTheTitle.Message = "", ""
	reminder := pushoverFailure
	reminder.Type, reminder.Title, reminder.TitleWithAlertSeverity, reminder.Body = "reminder", "app-1: myapp/http still failing", "SOS: app-1: myapp/http still failing", "myapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503"
	reminder.Destination = "team_lead_phone"
	severalNodes := checksandalerts.Event{ID: "55555555555555555555555555555555", FleetIncidentKey: pushoverFleetIncidentKey, CheckGroup: "myapp", Check: "http", Type: "recovered",
		AlertSeverity: "sos", AlertSeveritiesSentForIncident: []string{"warn", "sos"}, Nodes: []string{"app-2", "app-3"}, NodeCount: 2,
		Title: "myapp/http is ok on every node again", TitleWithAlertSeverity: "Resolved SOS: myapp/http is ok on every node again",
		Body: "myapp/http is ok on every node again (was failing on app-2, app-3)"}
	// Two destinations share the application token: on_call_warnings takes
	// [warn], and on_call_phones [sos]. On app-1 the check failed at warn and
	// was raised to sos, which went to on_call_phones alone; on app-2 it
	// fails at sos. app-1 recovers, and on_call_warnings, which got only the
	// warn failure, gets the closing alert of the check with the severities
	// sent to it. It stops nothing, so the emergency message of app-2 on
	// on_call_phones goes on.
	onlyWarnSentHere := pushoverFailure
	onlyWarnSentHere.Type, onlyWarnSentHere.Title, onlyWarnSentHere.TitleWithAlertSeverity, onlyWarnSentHere.Body = "recovered", "app-1: myapp/http is ok again", "Resolved SOS: app-1: myapp/http is ok again", "HTTP 200"
	onlyWarnSentHere.AlertSeveritiesSentForIncident = []string{"warn"}
	onlyWarnAtWarn := onlyWarnSentHere
	onlyWarnAtWarn.AlertSeverity, onlyWarnAtWarn.TitleWithAlertSeverity = "warn", "Resolved warning: app-1: myapp/http is ok again"
	secrets := `"token":"` + pushoverApplicationToken + `","user":"` + pushoverUserOrGroupKey + `"`
	for _, tc := range []struct {
		name  string
		alert checksandalerts.Event
		stops bool
		want  string
	}{
		{"a recovery", recovery, true, `{"message":"HTTP 200","monospace":1,"priority":0,"title":"Resolved SOS: app-1: myapp/http is ok again",` + secrets + `}`},
		{"MMWatchdog can check again", canCheckAgain, true, `{"message":"MMWatchdog can run the check again","monospace":1,"priority":0,"title":"Resolved SOS: app-1: MMWatchdog can check myapp/http again",` + secrets + `}`},
		{"an incident closed without recovery", retired, true, `{"message":"the check was removed from the configuration; MMWatchdog does not know whether the problem is fixed","monospace":1,"priority":0,"title":"Closed SOS: app-1: myapp/http incident closed, maybe not fixed",` + secrets + `}`},
		{"a recovery, only the title", recoveryOnlyTheTitle, true, `{"message":"Resolved SOS: app-1: myapp/http is ok again","priority":0,` + secrets + `}`},
		{"the closing alert of several nodes", severalNodes, true, `{"message":"myapp/http is ok on every node again (was failing on app-2, app-3)","monospace":1,"priority":0,"title":"Resolved SOS: myapp/http is ok on every node again",` + secrets + `}`},
		{"the closing alert to a destination that got only warn", onlyWarnAtWarn, false, `{"message":"HTTP 200","monospace":1,"priority":0,"title":"Resolved warning: app-1: myapp/http is ok again",` + secrets + `}`},
		{"a node's own recovery that reached sos, to a destination that got only warn", onlyWarnSentHere, false, `{"message":"HTTP 200","monospace":1,"priority":0,"title":"Resolved SOS: app-1: myapp/http is ok again",` + secrets + `}`},
		{"a reminder for one destination", reminder, false, `{"expire":3600,"message":"myapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503","monospace":1,"priority":2,"retry":300,"tags":"` + pushoverTag + `","title":"SOS: app-1: myapp/http still failing",` + secrets + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(server.got())
			o := run(t, "pushover", tc.alert, pushoverInputs(server.URL), pushoverFileContents)
			if o.exitStatus != 0 || o.status != "delivered" || o.message != "Pushover accepted the message" || o.stderr != "" {
				t.Fatalf("%+v", o)
			}
			requests := server.got()[before:]
			if tc.stops {
				if len(requests) != 2 || !isPushoverStop(requests[0], pushoverTag) {
					t.Fatalf("%d requests: %+v", len(requests), requests)
				}
				requests = requests[1:]
			}
			if len(requests) != 1 {
				t.Fatalf("%d requests: %+v", len(requests), requests)
			}
			if got := asJSON(t, pushoverMessage(t, requests[0])); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
			pushoverKeepsTheSecrets(t, o)
		})
	}
}

// What Pushover answers to the stop of the repeats never decides the
// delivery: the message that follows decides it. So a recovery arrives when
// Pushover does not stop the repeats, and MMWatchdog tries the stop and the message
// again when the message fails. MMWatchdog shows nothing of an answer delivered, so
// a stop that failed is not reported, and a person acknowledges the earlier
// message in the app.
func TestPushoverDeliversTheRecoveryWhenTheRepeatsDoNotStop(t *testing.T) {
	t.Parallel()
	recovery := pushoverFailure
	recovery.Type, recovery.TitleWithAlertSeverity, recovery.Body = "recovered", "Resolved SOS: app-1: myapp/http is ok again", "HTTP 200"
	for _, tc := range []struct {
		name                      string
		stopStatus, messageStatus int
		stopAnswer, messageAnswer string
		wantStatus, wantMessage   string
	}{
		// Pushover documents no answer of a stop by tag but success. This
		// refusal stands for any other answer, such as one for a tag that no
		// message has any more.
		{"Pushover refuses the stop", 400, 200, `{"errors":["no active receipts found with tag"],"status":0,"request":"5042853c-402d-4a18-abcb-168734a801de"}`, `{"status":1,"request":"647d2300-702c-4b38-8b2f-d56326ae460b"}`,
			"delivered", "Pushover accepted the message"},
		{"an error of Pushover at the stop", 500, 200, "<html><body>Internal Server Error</body></html>", `{"status":1,"request":"647d2300-702c-4b38-8b2f-d56326ae460b"}`,
			"delivered", "Pushover accepted the message"},
		// The repeats have stopped, and the recovery's message comes when MMWatchdog
		// tries again.
		{"the stop works and the message fails", 200, 500, `{"status":1,"request":"8b7e3c43-5d76-4b2e-9b1d-0f5c4b6a1e01"}`, `{"errors":["internal error"],"status":0}`,
			"failed", "Pushover answered 500: internal error."},
		{"both fail", 503, 503, "Service Unavailable", "Service Unavailable",
			"failed", "the API server answered 503 without Pushover's JSON: Service Unavailable. Pushover may have a temporary problem; if it lasts, check api_server_url and what stands between this node and that server."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := startService(t, func(request received) (int, string) {
				if strings.HasPrefix(request.path, "/1/receipts/") {
					return tc.stopStatus, tc.stopAnswer
				}
				return tc.messageStatus, tc.messageAnswer
			})
			o := run(t, "pushover", recovery, pushoverInputs(server.URL), pushoverFileContents)
			if requests := server.got(); o.status != tc.wantStatus || o.message != tc.wantMessage || len(requests) != 2 || !isPushoverStop(requests[0], pushoverTag) || pushoverMessage(t, requests[1])["title"] != recovery.TitleWithAlertSeverity {
				t.Fatalf("got  %s %q\nwant %s %q\n%+v", o.status, o.message, tc.wantStatus, tc.wantMessage, requests)
			}
			pushoverKeepsTheSecrets(t, o)
		})
	}
}

// A message of more than 1,024 characters, and a title of more than 250,
// are cut between two characters and marked. A text that fits is not cut.
func TestPushoverCutsALongText(t *testing.T) {
	t.Parallel()
	server := pushover(t)
	for _, tc := range []struct {
		name, title, body string
	}{
		{"a long body", failure.TitleWithAlertSeverity, strings.Repeat("ü🐒& ", 1000)},
		{"a body of exactly the limit", failure.TitleWithAlertSeverity, strings.Repeat("🐒", 1024)},
		{"a long title", "SOS: app-1: " + strings.Repeat("ș", 300), "HTTP 503"},
		{"a long title without a body", "SOS: app-1: " + strings.Repeat("ș", 1100), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			long := failure
			long.TitleWithAlertSeverity, long.Body = tc.title, tc.body
			before := len(server.got())
			o := run(t, "pushover", long, pushoverInputs(server.URL), pushoverFileContents)
			requests := server.got()[before:]
			if o.status != "delivered" || len(requests) != 1 {
				t.Fatalf("%d requests; %+v", len(requests), o)
			}
			message := pushoverMessage(t, requests[0])
			// Without a body, the title is the message.
			if tc.body == "" {
				sent, _ := message["message"].(string)
				if _, hasTitle := message["title"]; hasTitle || utf8.RuneCountInString(sent) != 1024 || !strings.HasSuffix(sent, "…") || !strings.HasPrefix(tc.title, strings.TrimSuffix(sent, "…")) {
					t.Fatalf("%+v", message)
				}
				return
			}
			for _, part := range []struct {
				field, whole string
				most         int
			}{{"title", tc.title, 250}, {"message", tc.body, 1024}} {
				sent, _ := message[part.field].(string)
				cut := utf8.RuneCountInString(part.whole) > part.most
				if !utf8.ValidString(sent) || cut && utf8.RuneCountInString(sent) != part.most || !cut && sent != part.whole || cut != strings.HasSuffix(sent, "…") || !strings.HasPrefix(part.whole, strings.TrimSuffix(sent, "…")) {
					t.Fatalf("%s: %d characters, cut %v, ends with %q", part.field, utf8.RuneCountInString(sent), cut, sent[max(0, len(sent)-12):])
				}
			}
		})
	}
}

// Every answer of Pushover other than a message that it took is a failed
// attempt with a message that a person can act on, and no failure shows a
// secret of the destination. MMWatchdog shows a module's message as one line of at
// most 300 bytes, so every message is one, and Pushover's words get the
// bytes that the module's own words leave.
func TestPushoverFailures(t *testing.T) {
	t.Parallel()
	noJSON := func(status, words string) string {
		if words != "" {
			words = ": " + words
		}
		return "the API server answered " + status + " without Pushover's JSON" + words + ". Pushover may have a temporary problem; if it lasts, check api_server_url and what stands between this node and that server."
	}
	const tokenAdvice = " Pushover does not know this application token. Copy it from pushover.net into the file that application_token_file names."
	const keyAdvice = " Pushover does not know this user or group key, or its user has no active device. Copy the key from pushover.net into the file that user_or_group_key_file names."
	const monthlyAdvice = ". The account has used up its messages of this month. Pushover takes messages again on the 1st of next month, or after more are bought."
	const blockAdvice = ". Pushover takes no messages for now: the account used up its messages of this month, or Pushover blocks this node after many refused requests."
	longErrors := `{"token":"invalid","errors":["application token is invalid, ` + strings.Repeat("x", 150) + `","` + strings.Repeat("y", 150) + `"],"status":0}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		// The answers that users got from Pushover, in the issue trackers of
		// Alertmanager and of other programs that send to Pushover.
		{"a wrong application token", 400, `{"token":"invalid","errors":["application token is invalid, see https://pushover.net/api"],"status":0,"request":"5042853c-402d-4a18-abcb-168734a801de"}`,
			"Pushover answered 400: application token is invalid, see https://pushover.net/api." + tokenAdvice},
		{"a wrong user key", 400, `{"user":"invalid","errors":["user identifier is invalid"],"status":0,"request":"5042853c-402d-4a18-abcb-168734a801de"}`,
			"Pushover answered 400: user identifier is invalid." + keyAdvice},
		{"a wrong group key, in the older words", 400, `{"user":"invalid","errors":["user identifier is not a valid user, group, or subscribed user key"],"status":0,"request":"5042853c-402d-4a18-abcb-168734a801de"}`,
			"Pushover answered 400: user identifier is not a valid user, group, or subscribed user key." + keyAdvice},
		{"another refusal", 400, `{"message":"cannot be blank","errors":["message cannot be blank"],"status":0,"request":"5042853c-402d-4a18-abcb-168734a801de"}`,
			"Pushover answered 400: message cannot be blank."},
		{"several errors", 400, `{"token":"invalid","user":"invalid","errors":["application token is invalid","user identifier is invalid"],"status":0}`,
			"Pushover answered 400: application token is invalid; user identifier is invalid." + tokenAdvice},
		// The errors come as MMWatchdog shows them, on one line, and a long list of
		// them gets the bytes that the advice leaves.
		{"errors on several lines", 400, `{"errors":["message is\ninvalid\t\u0001 here ", " ", "and  so is the title"],"status":0}`,
			"Pushover answered 400: message is invalid here; and so is the title."},
		{"errors longer than 200 characters", 400, `{"errors":["` + strings.Repeat("e", 250) + `"],"status":0}`,
			"Pushover answered 400: " + strings.Repeat("e", 199) + "…."},
		{"errors longer than the advice leaves room for", 400, longErrors,
			"Pushover answered 400: application token is invalid, " + strings.Repeat("x", 300-len("Pushover answered 400: application token is invalid, .…")-len(tokenAdvice)) + "…." + tokenAdvice},
		// At the monthly limit, Pushover names the token as invalid, and the
		// token is not wrong.
		{"the monthly limit", 429, `{"token":"invalid","errors":["application has exceeded its monthly limit of 10000 messages, see https://support.pushover.net/i13"],"status":0,"request":"5042853c-402d-4a18-abcb-168734a801de"}`,
			"Pushover answered 429: application has exceeded its monthly limit of 10000 messages, see https://support.pushover.net/i13" + monthlyAdvice},
		{"a node that Pushover blocks for a while", 429, "Your IP has been temporarily blocked because of too many invalid requests\n",
			"Pushover answered 429: Your IP has been temporarily blocked because of too many invalid requests" + blockAdvice},
		{"a node that Pushover is about to block", 200, "Your IP is about to be blocked because of too many invalid requests",
			noJSON("200", "Your IP is about to be blocked because of too many invalid requests")},
		{"an answer of 200 that is no success", 200, `{"errors":["something went wrong"],"status":0}`, "Pushover answered 200: something went wrong."},
		{"an error of Pushover", 500, `{"errors":["internal error"],"status":0}`, "Pushover answered 500: internal error."},
		{"a page of another server", 502, "<html><body>Bad Gateway</body></html>", noJSON("502", "<html><body>Bad Gateway</body></html>")},
		// A first line of 300 characters of two bytes each: 199 of them would
		// leave no room for the module's own words.
		{"a page whose first line is longer than 200 characters", 502, "<html>" + strings.Repeat("ș", 300) + "\n</html>",
			noJSON("502", "<html>"+strings.Repeat("ș", (300-len(noJSON("502", "…")))/2-3)+"…")},
		{"a redirect, which the module does not follow", 302, "", noJSON("302", "")},
		{"an answer of white space", 503, " \n", noJSON("503", "")},
		{"JSON that is no object", 400, `["application token is invalid"]`, noJSON("400", `["application token is invalid"]`)},
		{"a success without Pushover's status", 200, `{"ok":true}`, noJSON("200", `{"ok":true}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := startService(t, func(received) (int, string) { return tc.status, tc.body })
			o := run(t, "pushover", failure, pushoverInputs(server.URL), pushoverFileContents)
			if o.exitStatus != 0 || o.status != "failed" || o.message != tc.want {
				t.Fatalf("got  %q\nwant %q\n%+v", o.message, tc.want, o)
			}
			if checksandalerts.OneLine(o.message) != o.message || len(o.message) > 300 {
				t.Fatalf("MMWatchdog would shorten the message of %d bytes: %q", len(o.message), o.message)
			}
			if len(server.got()) != 1 {
				t.Fatalf("%d requests", len(server.got()))
			}
			pushoverKeepsTheSecrets(t, o)
		})
	}
	t.Run("the server closes the connection without an answer", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			connection, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			connection.Close()
		}))
		t.Cleanup(server.Close)
		const prefix = "MMWatchdog could not send the message to Pushover: "
		o := run(t, "pushover", failure, pushoverInputs(server.URL), pushoverFileContents)
		if o.exitStatus != 0 || o.status != "failed" || !strings.HasPrefix(o.message, prefix) || strings.TrimPrefix(o.message, prefix) == "" || checksandalerts.OneLine(o.message) != o.message {
			t.Fatalf("%+v", o)
		}
		pushoverKeepsTheSecrets(t, o)
	})
	// Invalid secret file contents fail before a request is sent.
	server := pushover(t)

	files := func(name, content string) map[string]string {
		return map[string]string{"application_token_file": pushoverApplicationToken, "user_or_group_key_file": pushoverUserOrGroupKey, name: content}
	}
	const wrongToken = "the file that application_token_file names must hold one API token of a Pushover application, 30 letters and digits such as azGDORePK8gMaC0QOYAMyEEuzJnyUi, and nothing else"
	const wrongKey = "the file that user_or_group_key_file names must hold one Pushover user key or group key, 30 letters and digits such as uQiRzpo4DXghDmr9QzzfQu27cmVRsG, and nothing else"

	info := failure
	info.AlertSeverity = "info"
	for _, tc := range []struct {
		name         string
		inputs       map[string]any
		fileContents map[string]string
		want         string
	}{
		{"a token that is too short", pushoverInputs(server.URL), files("application_token_file", pushoverApplicationToken[1:]), wrongToken},
		{"a token with a character that is no letter or digit", pushoverInputs(server.URL), files("application_token_file", pushoverApplicationToken[1:]+"-"), wrongToken},
		{"a token of letters outside ASCII", pushoverInputs(server.URL), files("application_token_file", strings.Repeat("ș", 15)), wrongToken},
		{"a file that holds two lines", pushoverInputs(server.URL), files("application_token_file", pushoverApplicationToken+"\n"+pushoverApplicationToken), wrongToken},
		{"an empty file", pushoverInputs(server.URL), files("application_token_file", ""), wrongToken},
		{"a key that holds more", pushoverInputs(server.URL), files("user_or_group_key_file", pushoverUserOrGroupKey+" "), wrongKey},
		{"a key that is the address of the dashboard", pushoverInputs(server.URL), files("user_or_group_key_file", "https://pushover.net/dashboard"), wrongKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, alert := range []checksandalerts.Event{failure, info} {
				o := run(t, "pushover", alert, tc.inputs, tc.fileContents)
				if o.exitStatus != 0 || o.status != "failed" || o.message != tc.want {
					t.Fatalf("%s:\ngot  %q\nwant %q", alert.AlertSeverity, o.message, tc.want)
				}
				pushoverKeepsTheSecrets(t, o)
			}
		})
	}
	if requests := server.got(); len(requests) != 0 {
		t.Fatalf("the module sent a request with a wrong file or input: %+v", requests)
	}
}

// The module reads every time that MMWatchdog takes, which is Go's notation with
// fractions and the units from ns to h, or a whole number of days, and
// sends Pushover whole seconds: a fraction of a second is dropped.
func TestPushoverReadsTheTimesInMMWatchdogsNotation(t *testing.T) {
	t.Parallel()
	server := pushover(t)
	for _, tc := range []struct {
		every, after  string
		retry, expire int
	}{
		{"5m", "1h", 300, 3600},
		{"30s", "30s", 30, 30},
		{"1m30s", "1h30m", 90, 5400},
		{"1.5m", "1.5h", 90, 5400},
		{"0.5m", "3h", 30, 10800},
		{"30.9s", "2h59m59.9s", 30, 10799},
		{"+2m", "180m", 120, 10800},
		{"30000ms", "10800000000us", 30, 10800},
		{"30000000000ns", "10799999999999ns", 30, 10799},
	} {
		t.Run(tc.every+" and "+tc.after, func(t *testing.T) {
			inputs := pushoverInputs(server.URL)
			inputs["repeat_sos_alerts_every"], inputs["stop_repeating_sos_alerts_after"] = tc.every, tc.after
			before := len(server.got())
			o := run(t, "pushover", failure, inputs, pushoverFileContents)
			requests := server.got()[before:]
			if o.status != "delivered" || len(requests) != 1 {
				t.Fatalf("%+v; %d requests", o, len(requests))
			}
			message := pushoverMessage(t, requests[0])
			if message["retry"] != json.Number(strconv.Itoa(tc.retry)) || message["expire"] != json.Number(strconv.Itoa(tc.expire)) {
				t.Fatalf("retry %v, expire %v", message["retry"], message["expire"])
			}
		})
	}
	// MMWatchdog takes every one of these times, and the module reads it as MMWatchdog does.
	for _, text := range []string{"1.5h", "7d", "100ms", "1h30m", "2h59m59.9s", "30000000000ns", "1µs", "1μs", "1us", ".5h", "5.h"} {
		want, e := checksandalerts.ParseDuration(text)
		if e != nil {
			t.Fatalf("%s: %v", text, e)
		}
		if want < 30*time.Second || want > 3*time.Hour {
			continue
		}
		inputs := pushoverInputs(server.URL)
		inputs["repeat_sos_alerts_every"], inputs["stop_repeating_sos_alerts_after"] = "30s", text
		before := len(server.got())
		if o := run(t, "pushover", failure, inputs, pushoverFileContents); o.status != "delivered" || pushoverMessage(t, server.got()[before])["expire"] != json.Number(strconv.Itoa(int(want/time.Second))) {
			t.Fatalf("%s: %+v", text, o)
		}
	}
}

// Without api_server_url the module sends to Pushover's own server,
// https://api.pushover.net. The test gives the module a proxy that stands on
// this machine and refuses every connection, so the request reaches nothing
// but the proxy, which sees where the module wanted to connect.
func TestPushoverUsesPushoversServerUnlessToldOtherwise(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := []string{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.Host)
		mu.Unlock()
		http.Error(w, "this proxy connects to nothing", http.StatusForbidden)
	}))
	defer proxy.Close()
	inputs := pushoverInputs("")
	delete(inputs, "api_server_url")
	o := run(t, "pushover", failure, inputs, pushoverFileContents, "HTTPS_PROXY="+proxy.URL)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "CONNECT api.pushover.net:443" {
		t.Fatalf("the module asked the proxy for %q: %+v", asked, o)
	}
	if o.exitStatus != 0 || o.status != "failed" || !strings.HasPrefix(o.message, "MMWatchdog could not send the message to Pushover: ") {
		t.Fatalf("%+v", o)
	}
	pushoverKeepsTheSecrets(t, o)
}

// MMWatchdog checks what a destination states for pushover against the module's
// description, before the module runs: a required input that is missing,
// an input that the module does not have, and a value of another type are
// validation errors.
func TestTheInputsOfPushoverAreValidated(t *testing.T) {
	t.Parallel()
	modules := checksandalerts.ModuleDirs{OfficialExtra: officialExtraModules(t), External: filepath.Join(t.TempDir(), "external_modules")}
	if module, e := config.DescribeModule(modules, "pushover"); e != nil || module.Kind != "official_extra" || module.ShippedWithMMWatchdogVersion != checksandalerts.Version || module.Description.UsedAs != "alert_destination" || len(module.Description.Examples) == 0 {
		t.Fatalf("%+v %v", module, e)
	}
	directory := t.TempDir()
	if e := os.Mkdir(filepath.Join(directory, "check_groups"), 0700); e != nil {
		t.Fatal(e)
	}
	for file, content := range map[string]string{
		"mmwatchdog_node_settings.yml":              "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\n",
		filepath.Join("check_groups", "checks.yml"): "check_groups: {app: {check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s, systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: warn}}\n",
	} {
		if e := os.WriteFile(filepath.Join(directory, file), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	const files = "application_token_file: /etc/mmwatchdog/secrets/pushover-application-token, user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-on-call-group-key"
	for _, tc := range []struct{ destination, want string }{
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 1h}", ""},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 30s, stop_repeating_sos_alerts_after: 3h, api_server_url: 'https://pushover-stand-in.example.com'}", ""},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 29s, stop_repeating_sos_alerts_after: 1h}", "destinations.chat.pushover.repeat_sos_alerts_every: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 29.9s, stop_repeating_sos_alerts_after: 1h}", "destinations.chat.pushover.repeat_sos_alerts_every: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 100ms, stop_repeating_sos_alerts_after: 1h}", "destinations.chat.pushover.repeat_sos_alerts_every: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 3h1s}", "destinations.chat.pushover.stop_repeating_sos_alerts_after: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 7d}", "destinations.chat.pushover.stop_repeating_sos_alerts_after: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 3.5h}", "destinations.chat.pushover.stop_repeating_sos_alerts_after: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 4m59s}", "destinations.chat.pushover.stop_repeating_sos_alerts_after: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 100ms}", "destinations.chat.pushover.stop_repeating_sos_alerts_after: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 1m, stop_repeating_sos_alerts_after: 0d}", "destinations.chat.pushover.stop_repeating_sos_alerts_after: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 1h, api_server_url: 'api.pushover.net'}", "destinations.chat.pushover.api_server_url: "},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 1h, api_server_url: 'https://api.pushover.net https://other.example.com'}", "destinations.chat.pushover.api_server_url: "},
		{"pushover: {user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-on-call-group-key, repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 1h}",
			"destinations.chat.pushover.application_token_file: is required: The absolute path of a file that holds the API token of the Pushover application"},
		{"pushover: {application_token_file: /etc/mmwatchdog/secrets/pushover-application-token, repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 1h}",
			"destinations.chat.pushover.user_or_group_key_file: is required: The absolute path of a file that holds the key of the Pushover user or delivery group"},
		{"pushover: {" + files + ", stop_repeating_sos_alerts_after: 1h}", "destinations.chat.pushover.repeat_sos_alerts_every: is required: How often Pushover repeats an sos alert"},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m}", "destinations.chat.pushover.stop_repeating_sos_alerts_after: is required: How long Pushover repeats an sos alert"},
		// A time is written with its unit, so mmwatchdog validate refuses a bare
		// number and a text that is no time.
		{"pushover: {" + files + ", repeat_sos_alerts_every: 300, stop_repeating_sos_alerts_after: 1h}", "destinations.chat.pushover.repeat_sos_alerts_every: expected a duration such as 30s, 5m, 2h, or 7d"},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: an hour}", "destinations.chat.pushover.stop_repeating_sos_alerts_after: expected a duration such as 30s, 5m, 2h, or 7d"},
		{"pushover: {" + files + ", repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 1h, sound: siren}",
			`destinations.chat.pushover.sound: unknown field "sound": the inputs of the module pushover are application_token_file, user_or_group_key_file, repeat_sos_alerts_every, stop_repeating_sos_alerts_after, api_server_url`},
		{"pushover: {application_token_file: secrets/pushover, user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-on-call-group-key, repeat_sos_alerts_every: 5m, stop_repeating_sos_alerts_after: 1h}",
			"destinations.chat.pushover.application_token_file: an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set application_token_file to an absolute file path"},
	} {
		if e := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte("destinations:\n  chat:\n    "+tc.destination+"\n    remind_every: never\n    alert_severities: [sos]\n"), 0600); e != nil {
			t.Fatal(e)
		}
		_, e := config.LoadWithModuleDirs(directory, modules)
		if tc.want == "" && e != nil || tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
			t.Errorf("%s:\n got %v\nwant %q", tc.destination, e, tc.want)
		}
	}
}
