package incidents_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
)

func TestConfirmationAndUnknown(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "1m", RecoveredAfterHealthyFor: "30s"}
	s := incidents.CheckState{CheckGroup: "app", Check: "health", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	observe := func(sec int, status string) []checksandalerts.Event {
		return s.Observe(checksandalerts.Observation{At: start.Add(time.Duration(sec) * time.Second), Status: status, Message: status}, timing, false)
	}
	if len(observe(0, "failed")) != 0 || len(observe(30, "failed")) != 0 {
		t.Fatal("early failure")
	}
	ev := observe(60, "failed")
	if len(ev) != 1 || ev[0].Type != "failed" || !ev[0].Notify {
		t.Fatal(ev)
	}
	incident := s.FailureIncident.Incident
	if len(observe(90, "ok")) != 0 {
		t.Fatal("early recovery")
	}
	observe(120, "unknown")
	if s.FailureIncident.Incident != incident || !s.HealthySince.IsZero() {
		t.Fatal("unknown cleared failure or counted as recovery")
	}
	observe(150, "ok")
	ev = observe(180, "ok")
	if len(ev) != 1 || ev[0].Type != "recovered" || ev[0].Incident != incident || s.FailureIncident.Incident != "" {
		t.Fatal(ev)
	}
}
func TestRestartResetsStreakPreservesIncident(t *testing.T) {
	t.Parallel()
	timing := checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "2s", RecoveredAfterHealthyFor: "2s"}
	s := incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	start := time.Now()
	for i := 0; i < 3; i++ {
		s.Observe(checksandalerts.Observation{Status: "failed", At: start.Add(time.Duration(i) * time.Second)}, timing, false)
	}
	id := s.FailureIncident.Incident
	if id == "" {
		t.Fatal("no incident")
	}
	s.Interrupt()
	ev := s.Observe(checksandalerts.Observation{Status: "failed", At: start.Add(100 * time.Second)}, timing, false)
	if len(ev) != 0 || s.FailureIncident.Incident != id {
		t.Fatal("duplicate failure on restart")
	}
	s.Observe(checksandalerts.Observation{Status: "ok", At: start.Add(101 * time.Second)}, timing, false)
	s.Interrupt()
	ev = s.Observe(checksandalerts.Observation{Status: "ok", At: start.Add(200 * time.Second)}, timing, false)
	if len(ev) != 0 || s.FailureIncident.Incident == "" {
		t.Fatal("unobserved time counted toward recovery")
	}
}
func TestMaintenanceReconcilesCurrentState(t *testing.T) {
	t.Parallel()
	zero := checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	s := incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	now := time.Now()
	obs := func(status string, suppressed bool) []checksandalerts.Event {
		now = now.Add(time.Second)
		return s.Observe(checksandalerts.Observation{Status: status, At: now, Message: status}, zero, suppressed)
	}
	obs("failed", false)
	id := s.FailureIncident.Incident
	ev := obs("ok", true)
	if len(ev) != 1 || ev[0].Notify {
		t.Fatal(ev)
	}
	ev = obs("ok", false)
	if len(ev) != 1 || ev[0].Type != "recovered" || ev[0].Incident != id || !ev[0].Notify {
		t.Fatal(ev)
	}
	s = incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	obs("failed", true)
	obs("ok", true)
	if ev = obs("ok", false); len(ev) != 0 {
		t.Fatal("replayed maintenance transitions", ev)
	}
}

// Each row of the lifecycle table in docs/changing-mmwatchdog/implementation-rules.md, on both tracks.
func TestLifecycleTableAppliesToBothTracks(t *testing.T) {
	t.Parallel()
	type step struct {
		failing, suppressed bool
		want                []string // event type and notify, such as "failed!" or "recovered"
	}
	for _, track := range []struct{ failing, failed, recovered string }{
		{"failed", "failed", "recovered"},
		{"unknown", "cannot_check", "can_check_again"},
	} {
		for _, tc := range []struct {
			name  string
			steps []step
		}{
			{"open and resolve", []step{
				{true, false, []string{track.failed + "!"}},
				{false, false, []string{track.recovered + "!"}},
			}},
			{"announce after maintenance", []step{
				{true, true, []string{track.failed}},
				{true, false, []string{track.failed + "!"}},
			}},
			{"close announced after maintenance", []step{
				{true, false, []string{track.failed + "!"}},
				{false, true, []string{track.recovered}},
				{false, false, []string{track.recovered + "!"}},
			}},
			{"unannounced incident resolves locally", []step{
				{true, true, []string{track.failed}},
				{false, false, []string{track.recovered}},
			}},
			{"deferred recovery followed by a new failure", []step{
				{true, false, []string{track.failed + "!"}},
				{false, true, []string{track.recovered}},
				{true, true, []string{track.failed}},
				{true, false, []string{track.recovered + "!", track.failed + "!"}},
			}},
		} {
			t.Run(track.failing+"/"+tc.name, func(t *testing.T) {
				s := incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
				now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
				for i, st := range tc.steps {
					status := "ok"
					if st.failing {
						status = track.failing
					}
					events := s.Observe(checksandalerts.Observation{Status: status, At: now.Add(time.Duration(i) * time.Second)}, checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, st.suppressed)
					got := []string{}
					for _, e := range events {
						label := e.Type
						if e.Notify {
							label += "!"
						}
						got = append(got, label)
					}
					if strings.Join(got, ",") != strings.Join(st.want, ",") {
						t.Fatalf("step %d: got %v, want %v", i, got, st.want)
					}
				}
			})
		}
	}
}

func TestMonitoringErrorIsSeparate(t *testing.T) {
	t.Parallel()
	s := incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	timing := checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	now := time.Now()
	ev := s.Observe(checksandalerts.Observation{Status: "unknown", At: now, Message: "cannot execute"}, timing, false)
	if len(ev) != 1 || ev[0].Type != "cannot_check" || s.FailureIncident.Incident != "" {
		t.Fatal(ev)
	}
	ev = s.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(time.Second), Message: "works"}, timing, false)
	if len(ev) != 1 || ev[0].Type != "can_check_again" {
		t.Fatal(ev)
	}
}

func TestSummarySeparatesIncidentAndObservation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	open := incidents.Track{Incident: "open"}
	for _, tc := range []struct {
		name       string
		state      incidents.CheckState
		needsFresh bool
		want       string
	}{
		{"open incident with unknown observation", incidents.CheckState{FailureIncident: open, Last: checksandalerts.Observation{Status: "unknown"}}, false, "failed"},
		{"open incident awaiting fresh observation", incidents.CheckState{FailureIncident: open, Last: checksandalerts.Observation{Status: "ok"}}, true, "failed"},
		{"observed recovery still confirming", incidents.CheckState{FailureIncident: open, Last: checksandalerts.Observation{Status: "ok"}}, false, "confirming_recovery"},
		{"awaiting fresh observation without incident", incidents.CheckState{Last: checksandalerts.Observation{Status: "ok"}}, true, "unknown"},
		{"unknown without incident", incidents.CheckState{Last: checksandalerts.Observation{Status: "unknown"}}, false, "unknown"},
		{"unconfirmed failure", incidents.CheckState{Last: checksandalerts.Observation{Status: "failed"}}, false, "confirming_failure"},
		{"initial heartbeat grace", incidents.CheckState{Deadline: now.Add(time.Hour), Last: checksandalerts.Observation{Status: "unknown"}}, false, "waiting_for_first_heartbeat"},
		{"expired grace awaits deadline evaluation", incidents.CheckState{Deadline: now.Add(-time.Second), Last: checksandalerts.Observation{Status: "unknown"}}, false, "unknown"},
		{"received heartbeat is healthy", incidents.CheckState{Receipt: now, Deadline: now.Add(time.Hour), Last: checksandalerts.Observation{Status: "ok"}}, false, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.Summary(now, tc.needsFresh); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSummarizeVerdict(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	opened := now.Add(-time.Hour)
	open := incidents.Track{Incident: "open", Opened: opened}
	for _, tc := range []struct {
		name       string
		state      incidents.CheckState
		needsFresh bool
		storage    string
		code       int
		attention  string // state, since, and message of the one attention entry; "" for none
	}{
		{"healthy", incidents.CheckState{Last: checksandalerts.Observation{Status: "ok", Message: "fine"}}, false, "", 0, ""},
		{"unconfirmed failure", incidents.CheckState{Last: checksandalerts.Observation{Status: "failed", Message: "503"}}, false, "", 0, ""},
		{"open incident", incidents.CheckState{FailureIncident: open, Last: checksandalerts.Observation{Status: "failed", Message: "503"}}, false, "", 1, "failed|opened|503"},
		{"recovery still confirming", incidents.CheckState{FailureIncident: open, Timing: checksandalerts.Timing{RecoveredAfterHealthyFor: "1m"}, Last: checksandalerts.Observation{Status: "ok", Message: "200"}}, false, "", 1, "confirming_recovery|opened|the latest run is healthy; the alert closes once the check has been healthy for 1m (recovered_after_healthy_for): 200"},
		{"open incident with unknown observation", incidents.CheckState{FailureIncident: open, Last: checksandalerts.Observation{Status: "unknown", Message: "timeout"}}, false, "", 1, "failed|opened|no current result; the failure alert stays open: timeout"},
		{"open incident awaiting fresh observation", incidents.CheckState{FailureIncident: open, Last: checksandalerts.Observation{Status: "ok", Message: "200"}}, true, "", 1, "failed|opened|no current result; the failure alert stays open: 200"},
		{"healthy before the latest result stopped counting", incidents.CheckState{Last: checksandalerts.Observation{Status: "ok", Message: "fine"}}, true, "", 3, "unknown|0001-01-01 00:00:00 +0000 UTC|waiting for a new run; the earlier result no longer counts: fine"},
		{"unknown with cannot-check incident", incidents.CheckState{CannotCheckIncident: open, UnknownSince: now, Last: checksandalerts.Observation{Status: "unknown", Message: "cannot run"}}, false, "", 3, "unknown|opened|cannot run"},
		{"unknown before confirmation", incidents.CheckState{UnknownSince: opened, Last: checksandalerts.Observation{Status: "unknown", Message: "cannot run"}}, false, "", 3, "unknown|opened|cannot run"},
		{"initial heartbeat grace", incidents.CheckState{Deadline: now.Add(time.Hour), Last: checksandalerts.Observation{Status: "unknown", Message: "no heartbeat has arrived yet"}}, false, "", 0, ""},
		{"storage error during grace", incidents.CheckState{Deadline: now.Add(time.Hour), Last: checksandalerts.Observation{Status: "unknown"}}, false, "disk full", 3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := incidents.CheckStatus{CheckState: tc.state, State: tc.state.Summary(now, tc.needsFresh), NeedsFresh: tc.needsFresh}
			got := incidents.Summarize(map[string]incidents.CheckStatus{"app/shell": st}, tc.storage)
			attention := ""
			if len(got.Attention) > 1 {
				t.Fatal(got)
			} else if len(got.Attention) == 1 {
				a := got.Attention[0]
				since := a.Since.String()
				if a.Since.Equal(opened) {
					since = "opened"
				}
				attention = a.State + "|" + since + "|" + a.Message
				if a.Check != "app/shell" {
					t.Fatal(a)
				}
			}
			if got.ExitCode != tc.code || attention != tc.attention {
				t.Fatalf("got exit %d attention %q, want %d %q", got.ExitCode, attention, tc.code, tc.attention)
			}
		})
	}
}

func TestRetirementClosesAnnouncedIncidents(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"active", "recovered during maintenance", "replaced during maintenance", "never announced"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			timing := checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "800ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
			st := &incidents.CheckState{CheckGroup: "app", Check: "health", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
			st.Observe(checksandalerts.Observation{Status: "failed", At: now}, timing, mode == "never announced")
			st.Observe(checksandalerts.Observation{Status: "unknown", At: now.Add(time.Second)}, timing, mode == "never announced")
			announced := map[string]bool{}
			if mode != "never announced" {
				announced[st.FailureIncident.Announced] = true
				announced[st.CannotCheckIncident.Announced] = true
			}
			if mode == "recovered during maintenance" || mode == "replaced during maintenance" {
				st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(2 * time.Second)}, timing, true)
			}
			if mode == "replaced during maintenance" {
				st.Observe(checksandalerts.Observation{Status: "failed", At: now.Add(3 * time.Second)}, timing, true)
				st.Observe(checksandalerts.Observation{Status: "unknown", At: now.Add(4 * time.Second)}, timing, true)
			}
			snap := incidents.FreshSnapshot()
			snap.Checks["app/health"] = st
			snap.Maintenance["app"] = checksandalerts.Window{Until: now.Add(time.Hour)}
			events := snap.Reconcile(&config.Config{}, nil, now.Add(5*time.Second))
			// The removed check's window ends after its incidents retire.
			if n := len(events); n == 0 || events[n-1].Type != "maintenance_off" || events[n-1].CheckGroup != "app" || len(snap.Maintenance) != 0 {
				t.Fatalf("the removed check's window did not end last: %+v", events)
			}
			events = events[:len(events)-1]
			seen := map[string]bool{}
			for _, event := range events {
				if event.Type != "closed_without_recovery" || seen[event.Incident] {
					t.Fatalf("invalid retirement: %+v", event)
				}
				seen[event.Incident] = true
				if event.Notify != announced[event.Incident] {
					t.Fatalf("wrong notification for %+v", event)
				}
				if event.Evidence != nil && event.Evidence.Incident != event.Incident {
					t.Fatal("evidence from another incident")
				}
			}
			for id := range announced {
				if !seen[id] {
					t.Fatalf("announced incident %s was left open", id)
				}
			}
			if len(snap.Checks) != 0 {
				t.Fatal("removed check remains active")
			}
		})
	}
}

func TestStatusFailureBeforeUnknown(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, reverse := range []bool{false, true} {
		failed, unknown := "a/shell", "z/shell"
		if reverse {
			failed, unknown = unknown, failed
		}
		checks := map[string]incidents.CheckStatus{
			failed:  {CheckState: incidents.CheckState{FailureIncident: incidents.Track{Incident: "incident"}, Last: checksandalerts.Observation{Status: "failed"}}, State: "failed"},
			unknown: {CheckState: incidents.CheckState{Last: checksandalerts.Observation{Status: "unknown"}}, State: "unknown"},
		}
		summary := incidents.Summarize(checks, "storage failure")
		if summary.ExitCode != 1 || len(summary.Attention) != 2 {
			t.Fatal(summary)
		}
		delete(checks, failed)
		if summary = incidents.Summarize(checks, ""); summary.ExitCode != 3 {
			t.Fatal(summary)
		}
	}
	for _, group := range []string{"backup", checksandalerts.NodeReportsCheckGroup} {
		state := incidents.CheckState{CheckGroup: group, Deadline: now.Add(time.Minute), Last: checksandalerts.Observation{Status: "unknown"}}
		summary := incidents.Summarize(map[string]incidents.CheckStatus{"waiting": {CheckState: state, State: state.Summary(now, false)}}, "")
		if summary.ExitCode != 0 || len(summary.Attention) != 0 {
			t.Fatal(summary)
		}
	}
}

// A check counts as failing from its first failed observation until it has
// been healthy for recovered_after_healthy_for. A check that fails on and
// off is failing the whole time: its incident opens at the first failed
// observation after alert_after_failing_for, with a note of how many of its
// last runs failed, and resolves only after a lasting recovery.
func TestFailingTimeOfACheckThatFailsOnAndOff(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "5m", RecoveredAfterHealthyFor: "1m"}
	s := incidents.CheckState{CheckGroup: "myapp", Check: "myapp_answers_health_request", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "sos"}}
	observe := func(sec int, status string) []checksandalerts.Event {
		return s.Observe(checksandalerts.Observation{At: start.Add(time.Duration(sec) * time.Second), Status: status, Message: "expected HTTP 200, received 503"}, timing, false)
	}
	// 12:00:00 to 12:04:30: failed and healthy in turn.
	for sec := 0; sec < 300; sec += 60 {
		if events := append(observe(sec, "failed"), observe(sec+30, "ok")...); len(events) != 0 {
			t.Fatal("alerted before the failing time lasted alert_after_failing_for", sec, events)
		}
		if !s.FailingSince.Equal(start) || s.Summary(start, false) != "confirming_failure" {
			t.Fatal("a healthy run shorter than recovered_after_healthy_for ended the failing time", sec, s.FailingSince, s.Summary(start, false))
		}
	}
	// 12:05:00: the failing time has lasted 5 minutes, and this run failed.
	events := observe(300, "failed")
	if len(events) != 1 || events[0].Type != "failed" || events[0].AlertSeverity != "sos" || events[0].Message != "expected HTTP 200, received 503" {
		t.Fatalf("%+v", events)
	}
	// The evidence keeps since when, and that the check failed on and off;
	// the body of the alert says both.
	if evidence := events[0].Evidence; evidence == nil || !evidence.FailingSince.Equal(start) || evidence.LastRunsFailed != 5 || evidence.LastRunsCounted != 10 {
		t.Fatalf("%+v", events[0].Evidence)
	}
	alert := events[0]
	alert.Node = "app-1"
	alert.WriteAlertTexts([]checksandalerts.Event{alert}, "")
	if alert.Title != "app-1: myapp/myapp_answers_health_request failed" || alert.Body != "expected HTTP 200, received 503\nfailing on and off since 12:00 UTC; 5 of the last 10 runs failed" {
		t.Fatalf("%q %q", alert.Title, alert.Body)
	}
	incident := s.FailureIncident.Incident
	// On and off again until 12:09:00; nothing more is sent.
	for sec := 330; sec < 540; sec += 60 {
		if events := append(observe(sec, "ok"), observe(sec+30, "failed")...); len(events) != 0 || s.FailureIncident.Incident != incident {
			t.Fatal(sec, events)
		}
	}
	// 12:09:30 is the first observation of a lasting recovery, and at
	// 12:10:30 the check has been healthy for 1 minute.
	if events := append(observe(570, "ok"), observe(600, "ok")...); len(events) != 0 || s.Summary(start, false) != "confirming_recovery" {
		t.Fatal(events, s.Summary(start, false))
	}
	events = observe(630, "ok")
	if len(events) != 1 || events[0].Type != "recovered" || events[0].Incident != incident || !s.FailingSince.IsZero() || s.FailingTimeRunsFailed != nil || s.Summary(start, false) != "ok" {
		t.Fatalf("%+v %+v", events, s)
	}
	// A check that fails on every run gets no note.
	for sec := 700; sec <= 1000; sec += 30 {
		events = observe(sec, "failed")
	}
	if len(events) != 1 || events[0].Message != "expected HTTP 200, received 503" {
		t.Fatalf("%+v", events)
	}
}

// Beside a healthy latest run, a confirming state says what it waits for:
// PgBouncer's first run showed "confirming_failure" next to "command
// exited 0", which read as a contradiction.
func TestConfirmingStatesSayWhatTheyWaitFor(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 28, 10, 41, 7, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "10s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "1m"}
	s := &incidents.CheckState{CheckGroup: "pgbouncer", Check: "pgbouncer_answers", Timing: timing, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	explain := func(minutes int, status, message string) string {
		at := start.Add(time.Duration(minutes) * time.Minute)
		s.Observe(checksandalerts.Observation{At: at, Status: status, Message: message}, timing, false)
		st := incidents.CheckStatus{CheckState: *s, State: s.Summary(at, false)}
		return st.State + " " + st.Explanation()
	}
	for _, step := range []struct {
		minutes         int
		status, message string
		want            string
	}{
		{0, "failed", "command exited 2", "confirming_failure command exited 2"},
		{1, "ok", "command exited 0", "confirming_failure the latest run is healthy, but runs have failed since 10:41:07 UTC; the check counts as failing until it has been healthy for 1m (recovered_after_healthy_for), so that a check that fails on and off still alerts: command exited 0"},
		{2, "failed", "command exited 2", "failed command exited 2"},
		{3, "ok", "command exited 0", "confirming_recovery the latest run is healthy; the alert closes once the check has been healthy for 1m (recovered_after_healthy_for): command exited 0"},
		{4, "ok", "command exited 0", "ok command exited 0"},
	} {
		if got := explain(step.minutes, step.status, step.message); got != step.want {
			t.Fatalf("after %d minutes got %q, want %q", step.minutes, got, step.want)
		}
	}
}

// With a nonzero alert delay, one failed run followed by health does not
// alert. Only a daemon start or maintenance boundary resets an unopened
// failing time. Unknown runs and finished recovery actions keep it running.
func TestFailingTimeEndsOnlyThroughHealthOrBeforeItsIncident(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "5m", RecoveredAfterHealthyFor: "10m"}
	fresh := func() *incidents.CheckState {
		return &incidents.CheckState{CheckGroup: "app", Check: "health", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	}
	at := func(sec int) time.Time { return start.Add(time.Duration(sec) * time.Second) }
	s := fresh()
	s.Observe(checksandalerts.Observation{At: at(0), Status: "failed", Message: "down"}, timing, false)
	for sec := 30; sec < 600; sec += 30 {
		if events := s.Observe(checksandalerts.Observation{At: at(sec), Status: "ok", Message: "up"}, timing, false); len(events) != 0 || s.FailingSince.IsZero() || s.Summary(at(sec), false) != "confirming_failure" {
			t.Fatal("one failed run alerted, or its failing time ended early", sec, events)
		}
	}
	// Healthy for 10 minutes at 12:10:30: the failing time ends without an incident.
	if events := s.Observe(checksandalerts.Observation{At: at(630), Status: "ok", Message: "up"}, timing, false); len(events) != 0 || !s.FailingSince.IsZero() || s.Summary(at(630), false) != "ok" {
		t.Fatal(events, s.FailingSince)
	}
	// Before the incident opens, a daemon start or maintenance boundary
	// ends the failing time, so the waiting times count anew.
	for name, end := range map[string]func(s *incidents.CheckState){
		"daemon start or maintenance boundary": func(s *incidents.CheckState) { s.Interrupt() },
	} {
		s = fresh()
		s.Observe(checksandalerts.Observation{At: at(0), Status: "failed", Message: "down"}, timing, false)
		end(s)
		if !s.FailingSince.IsZero() {
			t.Fatal(name, "left the unopened failing time running")
		}
		if events := s.Observe(checksandalerts.Observation{At: at(300), Status: "failed", Message: "down"}, timing, false); len(events) != 0 || !s.FailingSince.Equal(at(300)) {
			t.Fatal(name, "counted the time before it", events)
		}
	}
	// With an open incident, neither ends the failing time or the incident.
	s = fresh()
	s.Observe(checksandalerts.Observation{At: at(0), Status: "failed", Message: "down"}, timing, false)
	s.Observe(checksandalerts.Observation{At: at(300), Status: "failed", Message: "down"}, timing, false)
	incident := s.FailureIncident.Incident
	s.Observe(checksandalerts.Observation{At: at(330), Status: "unknown", Message: "cannot run"}, timing, false)
	s.Interrupt()
	if incident == "" || s.FailureIncident.Incident != incident || !s.FailingSince.Equal(at(0)) {
		t.Fatal("unknown or unobserved time ended a confirmed failure", s.FailureIncident, s.FailingSince)
	}
	// A finished restart or recovery command asks for fresh observations
	// and starts the healthy count over; the failing time goes on, so the
	// alert still goes out 5 minutes after the first failed run.
	s = fresh()
	s.Observe(checksandalerts.Observation{At: at(0), Status: "failed", Message: "down"}, timing, false)
	s.Observe(checksandalerts.Observation{At: at(30), Status: "ok", Message: "up"}, timing, false)
	s.BreakHealthyStreak()
	if !s.FailingSince.Equal(at(0)) || !s.HealthySince.IsZero() {
		t.Fatal("a finished action ended the failing time or kept the healthy count", s.FailingSince, s.HealthySince)
	}
	if events := s.Observe(checksandalerts.Observation{At: at(300), Status: "failed", Message: "down"}, timing, false); len(events) != 1 || events[0].Type != "failed" {
		t.Fatal(events)
	}
}

// raise_to_sos_after_failing_for counts from the start of the failing time,
// like every waiting time: an incident that opened later is raised when the
// failing time has lasted that long, and a value at or below
// alert_after_failing_for makes the first alert sos. A cannot-check
// incident counts from the first unknown result.
func TestRaiseCountsFromTheStartOfTheFailingTime(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return start.Add(time.Duration(minutes) * time.Minute) }
	check := func(raise checksandalerts.Duration) *incidents.CheckState {
		return &incidents.CheckState{CheckGroup: "app", Check: "health", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: raise}}
	}
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "20m", RecoveredAfterHealthyFor: "1m"}
	s := check("30m")
	s.Observe(checksandalerts.Observation{At: at(0), Status: "failed", Message: "down"}, timing, false)
	events := s.Observe(checksandalerts.Observation{At: at(20), Status: "failed", Message: "down"}, timing, false)
	if len(events) != 1 || events[0].AlertSeverity != "warn" {
		t.Fatal(events)
	}
	if events = s.RaiseAfterTime(at(29), false); len(events) != 0 {
		t.Fatal("raised before the failing time lasted 30 minutes", events)
	}
	// 10 minutes after the incident opened, 30 minutes into the failing time.
	events = s.RaiseAfterTime(at(30), false)
	if len(events) != 1 || events[0].Type != "failure_alert_severity_raised" || events[0].AlertSeverity != "sos" || events[0].Message != "app/health has been failing for 30m, so this alert is now SOS (raise_to_sos_after_failing_for: 30m)" {
		t.Fatalf("%+v", events)
	}
	// At or below alert_after_failing_for, the failure alert itself is sos.
	s = check("10m")
	s.Observe(checksandalerts.Observation{At: at(0), Status: "failed", Message: "down"}, timing, false)
	if events = s.Observe(checksandalerts.Observation{At: at(20), Status: "failed", Message: "down"}, timing, false); len(events) != 1 || events[0].Type != "failed" || events[0].AlertSeverity != "sos" {
		t.Fatalf("%+v", events)
	}
	// Unable to observe the check for 20 minutes, more than the 10 minutes
	// of the raise: the cannot-check alert goes out as sos.
	s = check("10m")
	s.Observe(checksandalerts.Observation{At: at(0), Status: "unknown", Message: "cannot run"}, timing, false)
	if events = s.Observe(checksandalerts.Observation{At: at(20), Status: "unknown", Message: "cannot run"}, timing, false); len(events) != 1 || events[0].Type != "cannot_check" || events[0].AlertSeverity != "sos" {
		t.Fatalf("%+v", events)
	}
	s = check("30m")
	s.Observe(checksandalerts.Observation{At: at(0), Status: "unknown", Message: "cannot run"}, timing, false)
	s.Observe(checksandalerts.Observation{At: at(20), Status: "unknown", Message: "cannot run"}, timing, false)
	if events = s.RaiseAfterTime(at(30), false); len(events) != 1 || events[0].Message != "MMWatchdog has not been able to run app/health for 30m, so this alert is now SOS (raise_to_sos_after_failing_for: 30m)" {
		t.Fatalf("%+v", events)
	}
}

// Clone copies every field of a snapshot, and the copy shares no map, slice,
// or pointer with the snapshot: a control changes its clone and drops it
// when its commit fails. Every time keeps its monotonic reading, so the
// waits of the clone count on the monotonic clock as those of the snapshot
// do. The test fills every field by reflection, so a field added later
// that Clone does not copy fails it.
func TestCloneCopiesEveryFieldAndSharesNothing(t *testing.T) {
	t.Parallel()
	// A time of the daemon's clock carries a monotonic reading.
	at := time.Now()
	var original, unchanged incidents.Snapshot
	fillEveryField(reflect.ValueOf(&original).Elem(), at, map[reflect.Type]int{})
	fillEveryField(reflect.ValueOf(&unchanged).Elem(), at, map[reflect.Type]int{})
	clone := original.Clone()
	if lost := differences("snapshot", reflect.ValueOf(clone), reflect.ValueOf(unchanged)); len(lost) > 0 {
		t.Fatal("the clone differs from the snapshot in:", strings.Join(lost, ", "))
	}
	changeEverything(reflect.ValueOf(&clone).Elem())
	if shared := differences("snapshot", reflect.ValueOf(original), reflect.ValueOf(unchanged)); len(shared) > 0 {
		t.Fatal("a change of the clone changed the snapshot, which shares these with the clone:", strings.Join(shared, ", "))
	}
}

// OnThisClock gives every time that MMWatchdog loaded from state.db a monotonic
// reading and keeps its wall reading, so that each wait that counts from it
// counts on the monotonic clock after a restart. The test fills every field
// by reflection, so a time added later that OnThisClock leaves out fails it.
func TestOnThisClockPutsEveryLoadedTimeOnTheMonotonicClock(t *testing.T) {
	t.Parallel()
	// A time decoded from state.db has a wall reading only.
	loaded := time.Now().Round(0).Add(-time.Hour)
	var snap incidents.Snapshot
	fillEveryField(reflect.ValueOf(&snap).Elem(), loaded, map[reflect.Type]int{})
	now := time.Now()
	snap.OnThisClock(now)
	wrong := []string{}
	for path, at := range timesIn("snapshot", reflect.ValueOf(snap)) {
		if at == at.Round(0) || !at.Round(0).Equal(loaded) || at.Sub(now) != loaded.Sub(now.Round(0)) {
			wrong = append(wrong, path)
		}
	}
	if len(wrong) > 0 {
		t.Fatal("these times are not on the monotonic clock with their wall reading after OnThisClock:", strings.Join(wrong, ", "))
	}
	unset := incidents.Snapshot{Checks: map[string]*incidents.CheckState{"app/http": {}}}
	unset.OnThisClock(now)
	if !unset.Checks["app/http"].FailingSince.IsZero() {
		t.Fatal("OnThisClock gave a time that was not set a value")
	}
}

var timeType = reflect.TypeFor[time.Time]()

// fillEveryField gives every exported field in v, recursively, a value that
// is not zero: a map and a slice get one element, a pointer a filled value,
// a time the time at, and the state of a module the JSON value 1. A
// function stays nil, since it holds no state. A type that holds itself, as
// a check holds the state of its findings, is filled two levels deep.
func fillEveryField(v reflect.Value, at time.Time, inside map[reflect.Type]int) {
	switch v.Type() {
	case timeType:
		v.Set(reflect.ValueOf(at))
		return
	case reflect.TypeFor[json.RawMessage]():
		v.Set(reflect.ValueOf(json.RawMessage("1")))
		return
	}
	// Only supported snapshot field types are filled; functions deliberately stay nil.
	//exhaustive:ignore
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillEveryField(p.Elem(), at, inside)
		v.Set(p)
	case reflect.Struct:
		if inside[v.Type()] == 2 {
			return
		}
		inside[v.Type()]++
		defer func() { inside[v.Type()]-- }()
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillEveryField(v.Field(i), at, inside)
			}
		}
	case reflect.Map:
		key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillEveryField(key, at, inside)
		fillEveryField(value, at, inside)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, value)
		v.Set(m)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillEveryField(s.Index(0), at, inside)
		v.Set(s)
	}
}

// changeEverything changes every value in v, recursively and in place:
// through every pointer, every element of a slice, and every entry of a
// map, which also gets a new entry.
func changeEverything(v reflect.Value) {
	if v.Type() == timeType {
		v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Hour)))
		return
	}
	// Only supported snapshot field types have mutable test values; other kinds hold no snapshot state.
	//exhaustive:ignore
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + " changed")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(v.Uint() + 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 1)
	case reflect.Pointer:
		if !v.IsNil() {
			changeEverything(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				changeEverything(v.Field(i))
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		for _, key := range v.MapKeys() {
			value := reflect.New(v.Type().Elem()).Elem()
			value.Set(v.MapIndex(key))
			changeEverything(value)
			v.SetMapIndex(key, value)
		}
		if v.Type().Key().Kind() == reflect.String {
			v.SetMapIndex(reflect.ValueOf("added").Convert(v.Type().Key()), reflect.New(v.Type().Elem()).Elem())
		}
	case reflect.Slice:
		for i := range v.Len() {
			changeEverything(v.Index(i))
		}
	}
}

// differences names, below path, every value in which a and b differ. Two
// times differ also when only their monotonic readings or their locations
// do.
func differences(path string, a, b reflect.Value) []string {
	if a.Type() == timeType || a.Kind() == reflect.Func {
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			return []string{path}
		}
		return nil
	}
	found := []string{}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				found = append(found, path)
			}
			return found
		}
		return differences(path, a.Elem(), b.Elem())
	case reflect.Struct:
		for i := range a.NumField() {
			if a.Type().Field(i).IsExported() {
				found = append(found, differences(path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))...)
			}
		}
	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return []string{path}
		}
		for _, key := range a.MapKeys() {
			if !b.MapIndex(key).IsValid() {
				found = append(found, fmt.Sprintf("%s[%v]", path, key))
				continue
			}
			found = append(found, differences(fmt.Sprintf("%s[%v]", path, key), a.MapIndex(key), b.MapIndex(key))...)
		}
	case reflect.Slice:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return []string{path}
		}
		for i := range a.Len() {
			found = append(found, differences(fmt.Sprintf("%s[%d]", path, i), a.Index(i), b.Index(i))...)
		}
	default:
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			found = append(found, path)
		}
	}
	return found
}

// timesIn returns, by path below path, every time in v that is not zero.
func timesIn(path string, v reflect.Value) map[string]time.Time {
	found := map[string]time.Time{}
	if v.Type() == timeType {
		if at := v.Interface().(time.Time); !at.IsZero() {
			found[path] = at
		}
		return found
	}
	add := func(more map[string]time.Time) {
		for p, at := range more {
			found[p] = at
		}
	}
	// Only containers can hold time values; scalar values need no traversal.
	//exhaustive:ignore
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			add(timesIn(path, v.Elem()))
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				add(timesIn(path+"."+v.Type().Field(i).Name, v.Field(i)))
			}
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			add(timesIn(fmt.Sprintf("%s[%v]", path, key), v.MapIndex(key)))
		}
	case reflect.Slice:
		for i := range v.Len() {
			add(timesIn(fmt.Sprintf("%s[%d]", path, i), v.Index(i)))
		}
	}
	return found
}

// Ending announced and quiet windows together must preserve each window's
// choice, even when their other fields match.
func TestMaintenanceEndKeepsAnnouncementChoicesSeparate(t *testing.T) {
	t.Parallel()
	now := time.Now()
	announced := checksandalerts.Window{Until: now, Reason: "deploy", User: "deployer"}
	quiet := announced
	quiet.WithoutMaintenanceOnAndOffAlerts = true
	snapshot := incidents.FreshSnapshot()
	snapshot.Maintenance["announced"] = announced
	snapshot.Maintenance["quiet"] = quiet
	events := snapshot.EndMaintenance([]string{"announced", "quiet"}, now, incidents.MaintenanceExpiredMessage)
	if len(events) != 2 || events[0].CheckGroup != "announced" || !events[0].Notify || events[1].CheckGroup != "quiet" || events[1].Notify {
		t.Fatal(events)
	}
	if !strings.Contains(events[1].Message, "maintenance start and end are not announced") {
		t.Fatal(events[1])
	}
}

func TestEventRecordedDuringMaintenanceSaysItSentNoAlert(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "800ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	for _, status := range []string{"failed", "unknown"} {
		st := &incidents.CheckState{CheckGroup: "app", Check: "health", AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
		events := st.Observe(checksandalerts.Observation{Status: status, At: now, Message: "boom"}, timing, true)
		if len(events) != 1 || events[0].Notify || events[0].Message != "boom (no alert sent: alerts were paused by a maintenance window)" {
			t.Fatal(status, events)
		}
		// The first run after the window sends the alert, and its message is the observation's.
		events = st.Observe(checksandalerts.Observation{Status: status, At: now.Add(time.Second), Message: "boom"}, timing, false)
		if len(events) != 1 || !events[0].Notify || events[0].Message != "boom" {
			t.Fatal(status, events)
		}
	}
}

// An event check opens its failure incident at the first event, but its
// cannot-check incident waits the check group's alert_after_failing_for, as
// any other check's does: a short moment in which the check cannot read its
// events, such as a log file that is being rotated, sends nothing, and a
// check that stays unknown alerts once the wait has passed.
func TestAnEventCheckWaitsBeforeItCannotCheck(t *testing.T) {
	t.Parallel()
	c := &config.Config{CheckGroups: map[string]checksandalerts.CheckGroup{"myapp": {
		Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "10s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "0s"},
		Checks: map[string]checksandalerts.NamedCheck{
			"log": {Check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: "/var/log/myapp.log", FailWhenANewLineContains: "FATAL"}}},
			"web": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}},
		},
	}}}
	definitions := config.CheckDefinitions(c)
	timing := definitions["myapp/log"].Timing
	if timing.AlertAfterFailingFor != "0s" || timing.CannotCheckAlertAfter() != 2*time.Minute {
		t.Fatalf("an event check's timing: %+v", timing)
	}
	if other := definitions["myapp/web"].Timing; other.CannotCheckAlertAfter() != 2*time.Minute || other.AlertAfterCannotCheckFor != "" {
		t.Fatalf("another check's timing: %+v", other)
	}
	s := incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	start := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	observe := func(minutes float64, status string) []string {
		t.Helper()
		types := []string{}
		for _, event := range s.Observe(checksandalerts.Observation{Status: status, At: start.Add(time.Duration(minutes * float64(time.Minute))), Message: status}, timing, false) {
			types = append(types, event.Type)
		}
		return types
	}
	// A short moment: unknown for a minute, then readable again.
	for _, step := range []struct {
		minutes float64
		status  string
	}{{0, "unknown"}, {1, "unknown"}, {1.5, "ok"}} {
		if got := observe(step.minutes, step.status); len(got) != 0 {
			t.Fatalf("at %v minutes, %s sent %v", step.minutes, step.status, got)
		}
	}
	// A lasting one: the cannot-check incident opens once the check has
	// been unknown for two minutes, and not before.
	for _, minutes := range []float64{2, 3} {
		if got := observe(minutes, "unknown"); len(got) != 0 {
			t.Fatalf("at %v minutes the cannot-check incident opened early: %v", minutes, got)
		}
	}
	if got := observe(4, "unknown"); !reflect.DeepEqual(got, []string{"cannot_check"}) {
		t.Fatalf("a check that stays unknown for the wait sent %v", got)
	}
	// An event still opens the failure incident at once.
	if got := observe(5, "failed"); !slices.Contains(got, "failed") {
		t.Fatalf("the first event sent %v", got)
	}
}
