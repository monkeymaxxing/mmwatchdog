package incidents_test

import (
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
)

func TestAlternatingFailedAndUnknownOpensFailureIncident(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{AlertAfterFailingFor: "4m", RecoveredAfterHealthyFor: "2m"}
	state := incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	for minute, status := range []string{"failed", "unknown", "failed", "unknown", "failed"} {
		events := state.Observe(checksandalerts.Observation{Status: status, At: start.Add(time.Duration(minute) * time.Minute)}, timing, false)
		if minute < 4 && len(events) != 0 {
			t.Fatalf("opened early at minute %d: %+v", minute, events)
		}
		if minute == 4 && (len(events) != 1 || events[0].Type != "failed" || !events[0].Notify) {
			t.Fatalf("no failure alert after four minutes: %+v", events)
		}
		if !state.FailingSince.Equal(start) {
			t.Fatal("lost failing time", state.FailingSince)
		}
	}
}

func TestAnObservationGapDoesNotProveHealthOrClearCannotCheckTime(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "2m"}
	state := incidents.CheckState{AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	state.Observe(checksandalerts.Observation{Status: "failed", At: start}, timing, false)
	state.Observe(checksandalerts.Observation{Status: "ok", At: start.Add(time.Minute)}, timing, false)
	state.BreakHealthyStreak()
	state.Observe(checksandalerts.Observation{Status: "ok", At: start.Add(3 * time.Minute)}, timing, false)
	if state.FailingSince.IsZero() {
		t.Fatal("the observation gap counted as healthy time")
	}
	state.Observe(checksandalerts.Observation{Status: "unknown", At: start.Add(4 * time.Minute)}, timing, false)
	state.BreakHealthyStreak()
	events := state.Observe(checksandalerts.Observation{Status: "unknown", At: start.Add(6 * time.Minute)}, timing, false)
	if len(events) != 1 || events[0].Type != "cannot_check" || !state.UnknownSince.Equal(start.Add(4*time.Minute)) {
		t.Fatal("action cleared cannot-check timer", events, state.UnknownSince)
	}
}

func TestRemovingAlertSeverityClosesOpenIncidents(t *testing.T) {
	t.Parallel()
	now := time.Now()
	definition := config.Definition{CheckGroup: "app", Name: "memory", Check: checksandalerts.Check{Inputs: new(checksandalerts.NodeFilesystem("/"))}, Severity: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	cfg := &config.Config{}
	snapshot := incidents.FreshSnapshot()
	snapshot.ReconcileOne(cfg, "app/memory", &definition, now)
	state := snapshot.Checks["app/memory"]
	state.Observe(checksandalerts.Observation{Status: "failed", At: now}, checksandalerts.Timing{}, false)
	state.Observe(checksandalerts.Observation{Status: "unknown", At: now.Add(time.Second)}, checksandalerts.Timing{}, false)
	definition.Severity = checksandalerts.AlertSeveritySettings{}
	definition.Check.OnlyRecordReadings = true
	events := snapshot.ReconcileOne(cfg, "app/memory", &definition, now.Add(2*time.Second))
	if len(events) != 2 {
		t.Fatalf("expected both incidents closed, got %+v", events)
	}
	for _, event := range events {
		if event.Type != "closed_without_recovery" || !event.Notify {
			t.Fatal(event)
		}
	}
	state = snapshot.Checks["app/memory"]
	if state.FailureIncident.Incident != "" || state.CannotCheckIncident.Incident != "" {
		t.Fatal("incident left open")
	}
}
