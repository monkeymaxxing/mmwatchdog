package checksandalerts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// Every alert has a title: one short line that names the node, the check
// group and the check, and what changed. A hub's grouped message counts
// its nodes. The table is the one in docs/changing-mmwatchdog/design/alerts.md.
func TestAlertTitles(t *testing.T) {
	t.Parallel()
	check := func(kind string) checksandalerts.Event {
		return checksandalerts.Event{Node: "web-3", CheckGroup: "myapp", Check: "http", Type: kind, AlertSeverity: "sos"}
	}
	grouped := func(e checksandalerts.Event, count int) checksandalerts.Event {
		e.Node, e.NodeCount = "", count
		return e
	}
	// The hub, util, watches its nodes with checks named after them.
	silent := func(kind string) checksandalerts.Event {
		return checksandalerts.Event{Node: "util", CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: "app-2", Type: kind, AlertSeverity: "sos"}
	}
	silentNodes := func(kind string, count int) checksandalerts.Event {
		e := silent(kind)
		e.Check, e.NodeCount = "", count
		return e
	}
	outageAlerter := func(kind string) checksandalerts.Event {
		return checksandalerts.Event{Node: "app-5", CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: checksandalerts.HubCheck, Type: kind, AlertSeverity: "warn"}
	}
	reminder := func(e checksandalerts.Event, kinds checksandalerts.TrackEvents) checksandalerts.Event {
		e.Type, e.Message = "reminder", checksandalerts.ReminderMessage(kinds, e.AboutCheck(), "14:02 UTC", "timeout")
		return e
	}
	maintenance := func(kind string, checkGroups ...string) checksandalerts.Event {
		e := incidents.MaintenanceEvent(kind, checkGroups, checksandalerts.Window{Until: time.Now().Add(time.Hour)}, time.Now(), "maintenance")
		e.Node = "web-3"
		return e
	}
	for _, tc := range []struct {
		event checksandalerts.Event
		hub   string
		title string
	}{
		{check("failed"), "", "web-3: myapp/http failed"},
		{grouped(check("failed"), 7), "util", "myapp/http failed on 7 nodes"},
		{check("cannot_check"), "", "web-3: MMWatchdog cannot check myapp/http"},
		{grouped(check("cannot_check"), 2), "util", "MMWatchdog cannot check myapp/http on 2 nodes"},
		{check(checksandalerts.FailureIncidentEvents.Raised), "", "web-3: myapp/http still failing, now SOS"},
		{grouped(check(checksandalerts.FailureIncidentEvents.Raised), 3), "util", "myapp/http still failing on 3 nodes, now SOS"},
		{check(checksandalerts.CannotCheckIncidentEvents.Raised), "", "web-3: MMWatchdog still cannot check myapp/http, now SOS"},
		{checksandalerts.Event{Node: "web-3", CheckGroup: "security", Check: "flaws", Finding: "CVE-2026-123158 openssh", Type: checksandalerts.ExactFindingChanged, AlertSeverity: "warn"}, "", "web-3: security/flaws: CVE-2026-123158 openssh still failing, changed"},
		{check("recovered"), "", "web-3: myapp/http is ok again"},
		{grouped(check("recovered"), 3), "util", "myapp/http is ok again on 3 nodes"},
		{check("can_check_again"), "", "web-3: MMWatchdog can check myapp/http again"},
		{grouped(check("can_check_again"), 3), "util", "MMWatchdog can check myapp/http again on 3 nodes"},
		{check("closed_without_recovery"), "", "web-3: myapp/http incident closed, maybe not fixed"},
		{grouped(check("closed_without_recovery"), 3), "util", "myapp/http incidents closed on 3 nodes, maybe not fixed"},
		{reminder(check(""), checksandalerts.FailureIncidentEvents), "", "web-3: myapp/http still failing"},
		{reminder(check(""), checksandalerts.CannotCheckIncidentEvents), "", "web-3: MMWatchdog still cannot check myapp/http"},
		{grouped(reminder(check(""), checksandalerts.FailureIncidentEvents), 4), "util", "myapp/http still failing on 4 nodes"},
		{grouped(reminder(check(""), checksandalerts.CannotCheckIncidentEvents), 2), "util", "MMWatchdog still cannot check myapp/http on 2 nodes"},
		// A direct check is named after its kind.
		{checksandalerts.Event{Node: "web-3", CheckGroup: "backup", Check: "heartbeat", Type: "failed"}, "", "web-3: backup/heartbeat failed"},
		// The hub's alerts about nodes that stopped reporting.
		{silent("failed"), "util", "app-2 stopped reporting to the MMWatchdog fleet hub"},
		{silentNodes("failed", 3), "util", "3 nodes stopped reporting to the MMWatchdog fleet hub"},
		{silent(checksandalerts.FailureIncidentEvents.Raised), "util", "app-2 still not reporting to the MMWatchdog fleet hub, now SOS"},
		{silentNodes(checksandalerts.FailureIncidentEvents.Raised, 3), "util", "3 nodes still not reporting to the MMWatchdog fleet hub, now SOS"},
		{silent("recovered"), "util", "app-2 reports to the MMWatchdog fleet hub again"},
		{silentNodes("recovered", 3), "util", "3 nodes report to the MMWatchdog fleet hub again"},
		{reminder(silent(""), checksandalerts.FailureIncidentEvents), "util", "app-2 still not reporting to the MMWatchdog fleet hub"},
		{silent("closed_without_recovery"), "util", "app-2 stopped reporting to the MMWatchdog fleet hub; alert closed, maybe not fixed"},
		{silentNodes("closed_without_recovery", 3), "util", "3 nodes stopped reporting to the MMWatchdog fleet hub; alerts closed, maybe not fixed"},
		// The hub-outage alerter node's alerts about the hub.
		{outageAlerter("failed"), "", "app-5 cannot reach the MMWatchdog fleet hub"},
		{outageAlerter("failed"), "util", "app-5 cannot reach the MMWatchdog fleet hub"},
		{outageAlerter(checksandalerts.FailureIncidentEvents.Raised), "", "app-5 still cannot reach the MMWatchdog fleet hub, now Warning"},
		{outageAlerter("recovered"), "", "app-5 reaches the MMWatchdog fleet hub again"},
		{reminder(outageAlerter(""), checksandalerts.FailureIncidentEvents), "", "app-5 still cannot reach the MMWatchdog fleet hub"},
		{outageAlerter("closed_without_recovery"), "", "app-5 no longer watches the MMWatchdog fleet hub; its alert about the hub is closed"},
		// Notices.
		{maintenance("maintenance_on", checksandalerts.EveryCheckGroup), "", "web-3: maintenance on for the whole node"},
		{maintenance("maintenance_on", "myapp"), "", "web-3: maintenance on for myapp"},
		{maintenance("maintenance_off", "worker_1", "worker_2"), "", "web-3: maintenance off for worker_1, worker_2"},
		{maintenance("maintenance_on", "worker_1", "worker_2", "worker_3", "worker_4"), "", "web-3: maintenance on for 4 check groups"},
		{grouped(maintenance("maintenance_on", checksandalerts.EveryCheckGroup), 5), "util", "maintenance on for 5 nodes"},
		{grouped(maintenance("maintenance_off", "myapp"), 5), "util", "maintenance off for 5 nodes"},
		{checksandalerts.Event{Node: "web-3", Type: "rebooted"}, "", "web-3 rebooted"},
		{checksandalerts.Event{Type: "rebooted", NodeCount: 3}, "util", "3 nodes rebooted"},
		{checksandalerts.Event{Node: "app-1", Type: "notice", Message: "Test of the alert destinations. Nothing is wrong."}, "", "app-1: Test of the alert destinations. Nothing is wrong."},
	} {
		title, shortened := checksandalerts.FitTitle(checksandalerts.AlertTitleParts(tc.event, tc.hub))
		if title != tc.title || shortened {
			t.Errorf("%s %s/%s: title %q (shortened %v), want %q", tc.event.Type, tc.event.CheckGroup, tc.event.Check, title, shortened, tc.title)
		}
	}
}

// A person who reads an alert in an email or a chat sees how urgent it is
// from its first words: title_with_alert_severity is the title with the
// severity in front, in the words that MMHotline shows. An alert that closes
// an incident says "Resolved", or "Closed" when the incident closed without
// recovery, and the highest severity the incident reached.
func TestTitleWithAlertSeverity(t *testing.T) {
	t.Parallel()
	check := func(kind, severity string) checksandalerts.Event {
		return checksandalerts.Event{Node: "app-1", CheckGroup: "myapp", Check: "http", Type: kind, AlertSeverity: severity}
	}
	maintenance := incidents.MaintenanceEvent("maintenance_off", []string{checksandalerts.EveryCheckGroup}, checksandalerts.Window{Until: time.Now().Add(time.Hour)}, time.Now(), "maintenance")
	maintenance.Node = "app-1"
	for _, tc := range []struct {
		event checksandalerts.Event
		hub   string
		want  string
	}{
		{check("failed", "info"), "", "Info: app-1: myapp/http failed"},
		{check("failed", "warn"), "", "Warning: app-1: myapp/http failed"},
		{check("failed", "sos"), "", "SOS: app-1: myapp/http failed"},
		{check("cannot_check", "warn"), "", "Warning: app-1: MMWatchdog cannot check myapp/http"},
		{check(checksandalerts.FailureIncidentEvents.Raised, "sos"), "", "SOS: app-1: myapp/http still failing, now SOS"},
		{check("reminder", "sos"), "", "SOS: app-1: myapp/http still failing"},
		{check("recovered", "info"), "", "Resolved info: app-1: myapp/http is ok again"},
		{check("recovered", "warn"), "", "Resolved warning: app-1: myapp/http is ok again"},
		{check("recovered", "sos"), "", "Resolved SOS: app-1: myapp/http is ok again"},
		{check("can_check_again", "warn"), "", "Resolved warning: app-1: MMWatchdog can check myapp/http again"},
		{check("closed_without_recovery", "sos"), "", "Closed SOS: app-1: myapp/http incident closed, maybe not fixed"},
		{check("closed_without_recovery", "warn"), "", "Closed warning: app-1: myapp/http incident closed, maybe not fixed"},
		{check("closed_without_recovery", "info"), "", "Closed info: app-1: myapp/http incident closed, maybe not fixed"},
		// A hub's grouped message has the severity that its nodes share.
		{checksandalerts.Event{CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "sos", NodeCount: 7}, "util", "SOS: myapp/http failed on 7 nodes"},
		{checksandalerts.Event{Node: "util", CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: "app-2", Type: "recovered", AlertSeverity: "sos"}, "util", "Resolved SOS: app-2 reports to the MMWatchdog fleet hub again"},
		// Notices: the end of a maintenance closes no incident.
		{maintenance, "", "Info: app-1: maintenance off for the whole node"},
		{checksandalerts.Event{Node: "app-1", Type: "rebooted", AlertSeverity: "warn"}, "", "Warning: app-1 rebooted"},
		{checksandalerts.Event{Node: "app-1", Type: "notice", AlertSeverity: "sos", Message: "Failover to db-2 started"}, "", "SOS: app-1: Failover to db-2 started"},
	} {
		event := tc.event
		event.WriteAlertTexts([]checksandalerts.Event{event}, tc.hub)
		if event.TitleWithAlertSeverity != tc.want || !strings.HasSuffix(tc.want, ": "+event.Title) {
			t.Errorf("%s %s: got %q with the title %q, want %q", tc.event.Type, tc.event.AlertSeverity, event.TitleWithAlertSeverity, event.Title, tc.want)
		}
	}
	// The label is in front of a title of the full length, and is not
	// counted in the 120 characters.
	long := check("recovered", "warn")
	long.CheckGroup = strings.Repeat("a", 200)
	long.WriteAlertTexts([]checksandalerts.Event{long}, "")
	if utf8.RuneCountInString(long.Title) != checksandalerts.AlertTitleLimit || long.TitleWithAlertSeverity != "Resolved warning: "+long.Title {
		t.Fatalf("%q %q", long.Title, long.TitleWithAlertSeverity)
	}
}

// A title has at most 120 characters. MMWatchdog cuts a longer one inside its
// longest name, then inside the name that is then the longest, and never
// cuts the last words, which say what changed.
func TestALongTitleIsCutInsideItsLongestName(t *testing.T) {
	t.Parallel()
	// The example of docs/changing-mmwatchdog/design/alerts.md: 147 characters.
	long := checksandalerts.Event{Node: "db-replica-03.frankfurt.example.com", CheckGroup: "nightly_backup_of_the_customer_database", Check: "the_restored_copy_of_last_night_has_as_many_rows_as_the_primary", Type: "failed"}
	if full := "db-replica-03.frankfurt.example.com: nightly_backup_of_the_customer_database/the_restored_copy_of_last_night_has_as_many_rows_as_the_primary failed"; len(full) != 147 {
		t.Fatal(len(full))
	}
	long.WriteAlertTexts([]checksandalerts.Event{long}, "")
	want := "db-replica-03.frankfurt.example.com: nightly_backup_of_the_customer_database/the_restored_copy_of_last_night_has… failed"
	if long.Title != want || !long.TitleWasShortened || utf8.RuneCountInString(long.Title) != checksandalerts.AlertTitleLimit {
		t.Fatalf("%q (%d characters, shortened %v)", long.Title, utf8.RuneCountInString(long.Title), long.TitleWasShortened)
	}
	// The event's own fields keep every name in full.
	if long.Check != "the_restored_copy_of_last_night_has_as_many_rows_as_the_primary" {
		t.Fatal(long.Check)
	}
	// A title of exactly 120 characters is not cut.
	exact := checksandalerts.Event{Node: strings.Repeat("n", 120-len(": myapp/http failed")), CheckGroup: "myapp", Check: "http", Type: "failed"}
	exact.WriteAlertTexts([]checksandalerts.Event{exact}, "")
	if exact.TitleWasShortened || utf8.RuneCountInString(exact.Title) != checksandalerts.AlertTitleLimit {
		t.Fatal(exact.Title, exact.TitleWasShortened)
	}
	// Three names of 64 characters and the longest title MMWatchdog writes: the
	// longest name is cut as far as it goes, then the next, and the words
	// around the names stay whole.
	names := checksandalerts.Event{Node: strings.Repeat("n", 64), CheckGroup: strings.Repeat("g", 64), Check: strings.Repeat("c", 64), Type: "closed_without_recovery"}
	names.WriteAlertTexts([]checksandalerts.Event{names}, "")
	if !names.TitleWasShortened || utf8.RuneCountInString(names.Title) > checksandalerts.AlertTitleLimit ||
		!strings.HasSuffix(names.Title, "… incident closed, maybe not fixed") ||
		!strings.HasPrefix(names.Title, strings.Repeat("n", checksandalerts.ShortestCutName-1)+"…: ") {
		t.Fatalf("%q (%d characters)", names.Title, utf8.RuneCountInString(names.Title))
	}
	// A name is counted and cut in characters, not in bytes.
	accents := checksandalerts.Event{Node: "web-3", CheckGroup: "myapp", Check: strings.Repeat("é", 150), Type: "failed"}
	accents.WriteAlertTexts([]checksandalerts.Event{accents}, "")
	if !accents.TitleWasShortened || utf8.RuneCountInString(accents.Title) != checksandalerts.AlertTitleLimit || !utf8.ValidString(accents.Title) || !strings.HasSuffix(accents.Title, "é… failed") {
		t.Fatalf("%q", accents.Title)
	}
}

// The body is everything else a person needs to act: the message, since
// when the check has been failing, and a short excerpt of the evidence.
func TestAlertBody(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 30, 14, 7, 0, 0, time.UTC)
	since := time.Date(2026, 9, 30, 14, 2, 0, 0, time.UTC)
	code := 7
	failed := checksandalerts.Event{Node: "app-1", CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "warn", At: at, Message: "expected 200, received 503",
		Evidence: &checksandalerts.Evidence{Observation: checksandalerts.Observation{Status: "failed", Message: "expected 200, received 503"}, FailingSince: since}}
	body := func(e checksandalerts.Event, members ...checksandalerts.Event) string {
		if len(members) == 0 {
			members = []checksandalerts.Event{e}
		}
		return checksandalerts.AlertBody(e, members, "util")
	}
	if got := body(failed); got != "expected 200, received 503\nfailing since 14:02 UTC" {
		t.Fatalf("%q", got)
	}
	// A failing time that began on another day carries its date.
	yesterday := failed
	evidence := *failed.Evidence
	evidence.FailingSince = since.Add(-24 * time.Hour)
	yesterday.Evidence = &evidence
	if got := body(yesterday); got != "expected 200, received 503\nfailing since 2026-09-29 14:02 UTC" {
		t.Fatalf("%q", got)
	}
	// A check that failed on and off, a raise, and the output of a command.
	shell := failed
	shell.Type, shell.Message = checksandalerts.FailureIncidentEvents.Raised, "myapp/http has been failing for 30m, so this alert is now SOS (raise_to_sos_after_failing_for: 30m)"
	shell.Evidence = &checksandalerts.Evidence{FailingSince: since, LastRunsFailed: 5, LastRunsCounted: 10, Observation: checksandalerts.Observation{ExitCode: &code,
		Stdout: "one\ntwo\r\nthree\nfour\nfive\nsix\nseven\n", Stderr: "  refused  \n"}}
	want := shell.Message + "\nfailing on and off since 14:02 UTC; 5 of the last 10 runs failed\nexit code: 7\nstdout:\n  one\n  two\n  three\n  four\n  five\n  …\nstderr:\n  refused"
	if got := body(shell); got != want {
		t.Fatalf("%q", got)
	}
	// The details of the confirming observation come before its output.
	withDetails := failed
	withDetails.Evidence = &checksandalerts.Evidence{FailingSince: since, Observation: checksandalerts.Observation{ExitCode: &code, Stdout: "out\n",
		Details: []string{"the commands that use the most memory:", "  3051 MiB: /usr/sbin/mysqld (process 812)"}}}
	if got := body(withDetails); got != "expected 200, received 503\nfailing since 14:02 UTC\nthe commands that use the most memory:\n  3051 MiB: /usr/sbin/mysqld (process 812)\nexit code: 7\nstdout:\n  out" {
		t.Fatalf("%q", got)
	}
	cannotCheck := checksandalerts.Event{Node: "app-1", CheckGroup: "myapp", Check: "http", Type: "cannot_check", At: at, Message: "the check ran past its time limit",
		Evidence: &checksandalerts.Evidence{CannotCheckSince: since}}
	if got := body(cannotCheck); got != "the check ran past its time limit\nMMWatchdog has not been able to run the check since 14:02 UTC" {
		t.Fatalf("%q", got)
	}
	// A reminder's message says since when; its body adds the evidence.
	reminder := failed
	reminder.Type, reminder.Message = "reminder", "myapp/http still failing since 14:02 UTC; latest result: expected 200, received 500"
	reminder.Evidence = &checksandalerts.Evidence{FailingSince: since, Observation: checksandalerts.Observation{ExitCode: &code}}
	if got := body(reminder); got != reminder.Message+"\nexit code: 7" {
		t.Fatalf("%q", got)
	}
	// A recovery, one of MMWatchdog's own notices, and an alert without evidence are
	// their message.
	for _, e := range []checksandalerts.Event{
		{Type: "recovered", Message: "HTTP 200", Evidence: failed.Evidence},
		{Type: "closed_without_recovery", Message: "the check was removed from the configuration; MMWatchdog does not know whether the problem is fixed", Evidence: failed.Evidence},
		{Type: "rebooted", Message: "app-1 rebooted at 14:03 UTC; MMWatchdog last ran at 14:01 UTC"},
		{Type: "failed", Message: "app-2 has sent no report to the hub since 14:02 UTC"},
	} {
		if got := body(e); got != e.Message {
			t.Fatalf("%s: %q", e.Type, got)
		}
	}
	// The body of a notice that a person or a program sent is the text they
	// gave, or nothing: its message is its title.
	sent := checksandalerts.Event{Node: "app-1", Type: "notice", Message: "deploy finished", Body: "version 1.4\nby the deploy script"}
	sent.WriteAlertTexts([]checksandalerts.Event{sent}, "")
	if sent.Title != "app-1: deploy finished" || sent.Body != "version 1.4\nby the deploy script" {
		t.Fatalf("%q %q", sent.Title, sent.Body)
	}
	sent.Body = ""
	if sent.WriteAlertTexts([]checksandalerts.Event{sent}, ""); sent.Body != "" {
		t.Fatalf("%q", sent.Body)
	}
	// A body keeps its line breaks and tabs. Every other control character,
	// such as a NUL from /proc/PID/cmdline, and each character that turns
	// the direction of text, becomes a space: a receiver whose database
	// cannot hold a NUL would refuse the alert, and every later alert to it
	// would wait behind that one.
	odd := shell
	odd.Evidence = &checksandalerts.Evidence{FailingSince: since, Observation: checksandalerts.Observation{Stdout: "/usr/bin/app\x00--port\x008080\n\tready\x07 \x1b[31mred\x1b[0m \u202eevil\n"}}
	odd.WriteAlertTexts([]checksandalerts.Event{odd}, "")
	if want := odd.Message + "\nfailing since 14:02 UTC\nstdout:\n  /usr/bin/app --port 8080\n  \tready   [31mred [0m  evil"; odd.Body != want {
		t.Fatalf("got  %q\nwant %q", odd.Body, want)
	}
	sent.Body = "line\x00one\n\tline two\r"
	if sent.WriteAlertTexts([]checksandalerts.Event{sent}, ""); sent.Body != "line one\n\tline two " {
		t.Fatalf("%q", sent.Body)
	}
	// A hub's grouped message names its nodes in its message; the lines
	// below are from the first of them.
	second := failed
	second.Node, second.Message = "app-2", "expected 200, received 502"
	message := store.Combine([]checksandalerts.Event{failed, second}, "util")
	message.WriteAlertTexts([]checksandalerts.Event{failed, second}, "util")
	if message.Title != "myapp/http failed on 2 nodes" || message.Body != "myapp/http failed on 2 nodes: app-1, app-2\nfrom app-1:\n  expected 200, received 503\n  failing since 14:02 UTC" {
		t.Fatalf("%q %q", message.Title, message.Body)
	}
}

// MMWatchdog writes the title and the body once, when the alert is queued, and
// every destination gets both. A destination that states
// send_only_the_alert_title gets the title, with and without the severity
// in front, and the fields that identify the alert, with no body, message,
// or evidence.
func TestTitleAndBodyAreQueuedForEveryDestination(t *testing.T) {
	t.Parallel()
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.Node = "app-1"
	destinations := map[string]checksandalerts.Destination{
		"team_chat":   {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/chat"}, TimeLimitPerDeliveryAttempt: "1s"},
		"on_call_sms": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/sms"}, TimeLimitPerDeliveryAttempt: "1s", SendOnlyTheAlertTitle: true},
	}
	now := time.Now().UTC()
	evidence := &checksandalerts.Evidence{Incident: "incident-1", Observation: checksandalerts.Observation{Status: "failed", Message: "expected 200, received 503"}, FailingSince: now}
	failed := checksandalerts.Event{ID: incidents.ID(), Incident: "incident-1", CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "sos", At: now, Message: "expected 200, received 503", Evidence: evidence, Notify: true}
	recorded := checksandalerts.Event{ID: incidents.ID(), CheckGroup: "myapp", Check: "http", Type: "systemd_unit_restart_requested", At: now, Message: "restart systemd unit myapp.service, attempt 1"}
	if e = s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{failed, recorded}, store.AlertRoute{Destinations: destinations}); e != nil {
		t.Fatal(e)
	}
	queued := func(destination string) map[string]any {
		t.Helper()
		var b []byte
		if e := s.DB.QueryRow("SELECT event FROM outbox WHERE destination=?", destination).Scan(&b); e != nil {
			t.Fatal(e)
		}
		event := map[string]any{}
		if e := json.Unmarshal(b, &event); e != nil {
			t.Fatal(e)
		}
		return event
	}
	title, body := "app-1: myapp/http failed", "expected 200, received 503\nfailing since "+now.Format("15:04 UTC")
	chat := queued("team_chat")
	if chat["title"] != title || chat["title_with_alert_severity"] != "SOS: "+title || chat["body"] != body || chat["message"] != "expected 200, received 503" || chat["evidence"] == nil || chat["title_was_shortened"] != nil {
		t.Fatalf("%+v", chat)
	}
	sms := queued("on_call_sms")
	if sms["title"] != title || sms["title_with_alert_severity"] != "SOS: "+title || sms["id"] != failed.ID || sms["incident_id"] != "incident-1" || sms["node"] != "app-1" || sms["check_group"] != "myapp" || sms["check"] != "http" || sms["alert_severity"] != "sos" {
		t.Fatalf("%+v", sms)
	}
	for _, field := range []string{"body", "message", "evidence"} {
		if _, present := sms[field]; present {
			t.Fatalf("a destination that takes only the title got %s: %+v", field, sms)
		}
	}
	// The event history has the alert with both texts; an event that is
	// only recorded has neither.
	events, e := s.Events(10)
	if e != nil || len(events) != 2 {
		t.Fatal(events, e)
	}
	for _, event := range events {
		if alert := event.Type == "failed"; (event.Title == title && event.Body == body) != alert || (event.Title == "" && event.Body == "") == alert {
			t.Fatalf("%+v", event)
		}
	}
	// On the hub the alert waits in the alert intake with its texts.
	if e = s.Save(incidents.FreshSnapshot(), []checksandalerts.Event{{ID: incidents.ID(), CheckGroup: checksandalerts.NodeReportsCheckGroup, Check: "app-2", Type: "failed", AlertSeverity: "warn", At: now, Message: "app-2 has sent no report to the hub since 14:02 UTC", Notify: true}}, store.AlertRoute{Destinations: destinations, Hub: true}); e != nil {
		t.Fatal(e)
	}
	var b []byte
	var silent checksandalerts.Event
	if e = s.DB.QueryRow("SELECT event FROM intake").Scan(&b); e == nil {
		e = json.Unmarshal(b, &silent)
	}
	if e != nil || silent.Title != "app-2 stopped reporting to the MMWatchdog fleet hub" || silent.TitleWithAlertSeverity != "Warning: app-2 stopped reporting to the MMWatchdog fleet hub" || silent.Body != "app-2 has sent no report to the hub since 14:02 UTC" {
		t.Fatal(silent, e)
	}
}

// mmwatchdog validate names every check whose alert title would be cut on this
// node, with the title as MMWatchdog would send it.
func TestValidateNamesTitlesThatWillBeShortened(t *testing.T) {
	t.Parallel()
	long := "the_restored_copy_of_last_night_has_as_many_rows_as_the_primary"
	c := &config.Config{CheckGroups: map[string]checksandalerts.CheckGroup{
		"nightly_backup_of_the_customer_database": {Checks: map[string]checksandalerts.NamedCheck{
			long:     {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "myapp.service", FailWhen: "the_unit_is_not_active"}}},
			"is_new": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "myapp.service", FailWhen: "the_unit_is_not_active"}}},
		}},
		"myapp": {Checks: map[string]checksandalerts.NamedCheck{"http": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "myapp.service", FailWhen: "the_unit_is_not_active"}}}}},
	}}
	// The short names of the same node fit, whatever the alert.
	got := c.AlertTitlesThatWillBeShortened("web-3")
	if len(got) != 1 || got[0].CheckGroup != "nightly_backup_of_the_customer_database" || got[0].Check != long {
		t.Fatalf("%+v", got)
	}
	// The longest title of a check is the one of a raised incident of a
	// check that MMWatchdog cannot run, so that one is counted and shown.
	full := "web-3: MMWatchdog still cannot check nightly_backup_of_the_customer_database/" + long + ", now SOS"
	if got[0].Characters != len(full) || utf8.RuneCountInString(got[0].Title) != checksandalerts.AlertTitleLimit || !strings.HasSuffix(got[0].Title, "…, now SOS") {
		t.Fatalf("%+v, want %d characters", got[0], len(full))
	}
	// On a node with a long name, a check with ordinary names is cut too.
	if got = c.AlertTitlesThatWillBeShortened(strings.Repeat("n", 100)); len(got) != 3 {
		t.Fatalf("%+v", got)
	}
}

// send_only_the_alert_title is a setting of a destination. Changing it
// keeps the destination's queue, because it is no part of the destination's
// identity; it applies to alerts queued after the change.
func TestSendOnlyTheAlertTitleSetting(t *testing.T) {
	t.Parallel()
	path := testhelpers.ConfigFile(t, "check_groups: {a: {http: {url: 'https://example.com'}}}\n")
	alerts := filepath.Join(path, "alert_destinations.yml")
	write := func(extra string) *config.Config {
		t.Helper()
		if e := os.WriteFile(alerts, []byte("destinations:\n  on_call_sms: {webhook: {url: 'https://example.com/sms'}, remind_every: never, alert_severities: [sos]"+extra+"}\n"), 0600); e != nil {
			t.Fatal(e)
		}
		c, e := config.Load(path)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	with, without := write(", send_only_the_alert_title: true").Alerts.Destinations["on_call_sms"], write("").Alerts.Destinations["on_call_sms"]
	if !with.SendOnlyTheAlertTitle || without.SendOnlyTheAlertTitle || !checksandalerts.SameDestination(with, without) {
		t.Fatalf("%+v %+v", with, without)
	}
	if e := os.WriteFile(alerts, []byte("destinations:\n  on_call_sms: {remind_every: never, webhook: {url: 'https://example.com/sms'}, alert_severities: [sos], send_only_the_alert_title: 120}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := config.Load(path); e == nil || !strings.Contains(e.Error(), "expected true or false") {
		t.Fatal("a number of characters was accepted", e)
	}
}
