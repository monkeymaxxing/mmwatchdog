package checksandalerts_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

func TestEvidenceSurvivesRecoveryRestartAndRetirement(t *testing.T) {
	t.Parallel()
	stateStore, err := store.OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	c, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {app: {alert_after_failing_for: 0s, recovered_after_healthy_for: 0s, shell: 'false'}}"))
	if err != nil {
		t.Fatal(err)
	}
	snap := incidents.FreshSnapshot()
	now := time.Now().UTC()
	snap.Reconcile(c, config.CheckDefinitions(c), now)
	st := snap.Checks["app/shell"]
	code := 7
	original := checksandalerts.Observation{Protocol: 1, Status: "failed", Message: "queue is full", Stdout: "780 queued\n", Stderr: "upstream unavailable\n", ExitCode: &code, At: now, Measurements: map[string]checksandalerts.Measurement{"pending": {Value: 780, Unit: "count"}}}
	ev := st.Observe(original, c.CheckGroups["app"].Timing, false)
	id := st.FailureIncident.Incident
	original.Measurements["pending"] = checksandalerts.Measurement{Value: 0, Unit: "count"}
	code = 0
	destinations := map[string]checksandalerts.Destination{"capture": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://example.com"}, TimeLimitPerDeliveryAttempt: "1s"}}
	if err = stateStore.Save(snap, ev, store.AlertRoute{Destinations: destinations}); err != nil {
		t.Fatal(err)
	}
	snap, err = stateStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	st = snap.Checks["app/shell"]
	if st.FailureIncident.Evidence.Incident != id || *st.FailureIncident.Evidence.Observation.ExitCode != 7 {
		t.Fatal(st.FailureIncident.Evidence)
	}
	ev = st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(time.Second), Message: "healthy"}, c.CheckGroups["app"].Timing, false)
	if st.FailureIncident.Incident != "" || st.FailureIncident.Evidence != nil {
		t.Fatal(st)
	}
	if err = stateStore.Save(snap, ev, store.AlertRoute{Destinations: destinations}); err != nil {
		t.Fatal(err)
	}
	events, err := stateStore.CheckGroupEvents("app", 100)
	if err != nil || len(events) != 2 {
		t.Fatal(events, err)
	}
	for _, event := range events {
		e := event.Evidence
		if e == nil || e.Incident != id || e.Observation.Stdout != "780 queued\n" || e.Observation.Measurements["pending"].Value != 780 || e.Source.File == "" || e.Timing.AlertAfterFailingFor != "0s" {
			t.Fatalf("%+v", e)
		}
	}
	pending, err := stateStore.Pending()
	if err != nil || len(pending) != 1 || pending[0].Event.Evidence.Observation.Stderr != "upstream unavailable\n" {
		t.Fatal(pending, err)
	}
	// A health incident and a cannot-check incident each retain their own evidence.
	st.Observe(checksandalerts.Observation{Status: "failed", At: now.Add(2 * time.Second), Message: "bad"}, c.CheckGroups["app"].Timing, false)
	st.Observe(checksandalerts.Observation{Status: "unknown", At: now.Add(3 * time.Second), Message: "cannot execute"}, c.CheckGroups["app"].Timing, false)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{}
	retired := snap.Reconcile(c, config.CheckDefinitions(c), now.Add(4*time.Second))
	if len(retired) != 2 || retired[0].Evidence == nil || retired[1].Evidence == nil || retired[0].Evidence.Incident == retired[1].Evidence.Incident {
		t.Fatal(retired)
	}
}

func TestEvidenceBoundsEncodedPayload(t *testing.T) {
	t.Parallel()
	e := checksandalerts.CaptureEvidence("incident", checksandalerts.Observation{Status: "failed", Stdout: strings.Repeat("\x00\"🐒", primitives.ResultLimit/4), Stderr: strings.Repeat("\x00", primitives.DiagnosticLimit), Message: strings.Repeat("text", primitives.ResultLimit)}, checksandalerts.SourceLocation{File: "test.yaml", Line: 3}, checksandalerts.Timing{})
	b, err := json.Marshal(e)
	if err != nil || len(b) > checksandalerts.EvidenceLimit || !e.Truncated || !utf8.ValidString(e.Observation.Stdout) {
		t.Fatal(len(b), err, e.Truncated)
	}
}

// The details of an observation are part of the evidence, as a copy, and
// give way before the measurements and the message do when the evidence is
// too large. A line of the details is one line of plain text.
func TestEvidenceKeepsTheDetailsOfAnObservation(t *testing.T) {
	t.Parallel()
	details := []string{"the commands that use the most memory:", "  1 MiB: a (process 1)"}
	e := checksandalerts.CaptureEvidence("incident", checksandalerts.Observation{Status: "failed", Message: "memory_used_percent is 94 (fails above 90)", Details: details}, checksandalerts.SourceLocation{File: "test.yaml", Line: 3}, checksandalerts.Timing{})
	details[1] = "changed after the incident opened"
	if e.Truncated || len(e.Observation.Details) != 2 || e.Observation.Details[1] != "  1 MiB: a (process 1)" {
		t.Fatal(e)
	}
	long := []string{}
	for range 200 {
		long = append(long, strings.Repeat("x", primitives.DetailLineLimit))
	}
	e = checksandalerts.CaptureEvidence("incident", checksandalerts.Observation{Status: "failed", Message: "the message stays", Details: long, Measurements: map[string]checksandalerts.Measurement{"memory_used_percent": {Value: 94, Unit: "percent"}}}, checksandalerts.SourceLocation{File: "test.yaml", Line: 3}, checksandalerts.Timing{})
	b, err := json.Marshal(e)
	if err != nil || len(b) > checksandalerts.EvidenceLimit || !e.Truncated || len(e.Observation.Details) == 0 || len(e.Observation.Details) >= 200 || e.Observation.Message != "the message stays" || len(e.Observation.Measurements) != 1 {
		t.Fatal(len(b), err, e.Truncated, len(e.Observation.Details))
	}
	if got := primitives.DetailLine("  FATAL:\tcannot open\x1b[2J database\r\n next \x7f line \xff end  "); got != "FATAL: cannot open [2J database   next   line   end" {
		t.Fatalf("%q", got)
	}
	if got := primitives.DetailLine(strings.Repeat("é", primitives.DetailLineLimit)); len(got) > primitives.DetailLineLimit || !utf8.ValidString(got) {
		t.Fatal(len(got))
	}
}

func TestMaintenanceDoesNotAttachAnotherIncidentsEvidence(t *testing.T) {
	t.Parallel()
	st := incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	timing := checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	now := time.Now()
	st.Observe(checksandalerts.Observation{Status: "failed", Message: "first", At: now}, timing, false)
	old := st.FailureIncident.Incident
	st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(time.Second)}, timing, true)
	st.Observe(checksandalerts.Observation{Status: "failed", Message: "second", At: now.Add(2 * time.Second)}, timing, true)
	events := st.Observe(checksandalerts.Observation{Status: "failed", At: now.Add(3 * time.Second)}, timing, false)
	if len(events) != 2 || events[0].Incident != old || events[1].Evidence.Observation.Message != "second" {
		t.Fatal(events)
	}
	for _, e := range events {
		if e.Evidence != nil && e.Evidence.Incident != e.Incident {
			t.Fatal("wrong incident evidence", e)
		}
	}
}
