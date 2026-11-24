package incidents_test

import (
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/incidents"
)

// exactFindingCheck is a check whose module reports findings with exact
// findings, and run reports, at each call one minute after the previous
// one, the finding CVE-2026-123158 openssh with the exact finding given, or
// no finding for "".
func exactFindingCheck() (*incidents.CheckState, func(exact string, suppressed bool) []checksandalerts.Event) {
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "10s", AlertAfterFailingFor: "1m", RecoveredAfterHealthyFor: "0s"}
	s := &incidents.CheckState{CheckGroup: "security", Check: "no_known_exploited_flaws", OneIncidentForEachFinding: true, Timing: timing, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	at := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	run := func(exact string, suppressed bool) []checksandalerts.Event {
		at = at.Add(time.Minute)
		o := checksandalerts.Observation{Protocol: 1, Status: "ok", Message: "no flaw", At: at}
		if exact != "" {
			o.Status, o.Message = "failed", "1 flaw"
			o.Findings = []checksandalerts.Finding{{ID: "CVE-2026-123158 openssh", Message: "openssh has CVE-2026-123158", ExactFinding: exact}}
		}
		return s.Observe(o, timing, suppressed)
	}
	return s, run
}

func notifying(events []checksandalerts.Event) []checksandalerts.Event {
	sent := []checksandalerts.Event{}
	for _, e := range events {
		if e.Notify {
			sent = append(sent, e)
		}
	}
	return sent
}

const (
	versionA = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z"
	versionB = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u5; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z"
	versionC = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u6; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z"
)

// The alert that announces a finding names the exact finding of the latest
// run. A later change of the exact finding sends exact_finding_changed once,
// about the same incident, with the same fleet incident key, and the
// incident stays open; an unchanged exact finding sends nothing.
func TestAChangedExactFindingIsAnnouncedOnceAndKeepsTheIncident(t *testing.T) {
	t.Parallel()
	s, run := exactFindingCheck()
	// The exact finding changes before the incident opens: the first alert
	// names the latest one, and nothing else goes out.
	if sent := notifying(run(versionA, false)); len(sent) != 0 {
		t.Fatal("an alert before alert_after_failing_for:", sent)
	}
	sent := notifying(run(versionB, false))
	if len(sent) != 1 || sent[0].Type != "failed" || sent[0].ExactFinding != versionB || !checksandalerts.HasExactFinding(sent[0]) {
		t.Fatalf("first alert: %+v", sent)
	}
	failed := sent[0]
	if sent := notifying(run(versionB, false)); len(sent) != 0 {
		t.Fatal("an unchanged exact finding sent:", sent)
	}
	sent = notifying(run(versionC, false))
	if len(sent) != 1 {
		t.Fatalf("a changed exact finding: %+v", sent)
	}
	changed := sent[0]
	if changed.Type != checksandalerts.ExactFindingChanged || changed.Incident != failed.Incident || changed.FleetIncidentKey != failed.FleetIncidentKey || changed.Finding != "CVE-2026-123158 openssh" ||
		changed.ExactFinding != versionC || changed.AlertSeverity != "warn" || !checksandalerts.BelongsToAnIncident(changed.Type) || !checksandalerts.HasExactFinding(changed) {
		t.Fatalf("got %+v", changed)
	}
	if changed.ExactFindingSequence <= failed.ExactFindingSequence {
		t.Fatalf("the sequence of a later exact finding is %d, after %d", changed.ExactFindingSequence, failed.ExactFindingSequence)
	}
	if !strings.Contains(changed.Message, versionC) || !strings.Contains(changed.Message, versionB) {
		t.Fatalf("the message names the exact findings before and now: %q", changed.Message)
	}
	if sent := notifying(run(versionC, false)); len(sent) != 0 {
		t.Fatal("an announced change sent again:", sent)
	}
	// The recovery closes the one incident and names its latest exact finding.
	sent = notifying(run("", false))
	if len(sent) != 1 || sent[0].Type != "recovered" || sent[0].Incident != failed.Incident || sent[0].ExactFinding != versionC {
		t.Fatalf("recovery: %+v", sent)
	}
	if len(s.Findings) != 0 {
		t.Fatal("a recovered finding was kept")
	}
}

// Maintenance and quiet time hold a change of the exact finding back. The
// first run after that time sends it when the exact finding still differs
// from the announced one, and nothing when it changed back.
func TestAnExactFindingChangeWaitsForMaintenanceAndQuietTime(t *testing.T) {
	t.Parallel()
	s, run := exactFindingCheck()
	run(versionA, false)
	if sent := notifying(run(versionA, false)); len(sent) != 1 || sent[0].Type != "failed" {
		t.Fatalf("first alert: %+v", sent)
	}
	if sent := notifying(run(versionB, true)); len(sent) != 0 {
		t.Fatal("a change sent while held back:", sent)
	}
	before := s.Findings["CVE-2026-123158 openssh"].FailureIncident.ExactFindingSequence
	if sent := notifying(run(versionA, true)); len(sent) != 0 {
		t.Fatal("a change sent while held back:", sent)
	}
	if after := s.Findings["CVE-2026-123158 openssh"].FailureIncident.ExactFindingSequence; after <= before {
		t.Fatal("a change while held back was not counted")
	}
	if sent := notifying(run(versionA, false)); len(sent) != 0 {
		t.Fatal("a change back to the announced exact finding sent:", sent)
	}
	run(versionC, true)
	sent := notifying(run(versionC, false))
	if len(sent) != 1 || sent[0].Type != checksandalerts.ExactFindingChanged || sent[0].ExactFinding != versionC {
		t.Fatalf("after the held time: %+v", sent)
	}
}

// A reminder of a finding names its latest exact finding, like every alert
// of the incident.
func TestAReminderNamesTheExactFinding(t *testing.T) {
	t.Parallel()
	s, run := exactFindingCheck()
	run(versionA, false)
	run(versionA, false)
	destinations := map[string]checksandalerts.Destination{"team": {RemindEvery: "1h", AlertSeverities: []string{"warn"}}}
	reminders := s.Reminders(destinations, time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC))
	if len(reminders) != 1 || reminders[0].ExactFinding != versionA || reminders[0].Finding != "CVE-2026-123158 openssh" {
		t.Fatalf("got %+v", reminders)
	}
}

// Status marks an incident eligible for reminders while it is open and
// announced and nothing holds its alerts back.
func TestReminderEligibleIsAnOpenAnnouncedIncidentThatNothingHoldsBack(t *testing.T) {
	t.Parallel()
	s, run := exactFindingCheck()
	run(versionA, false)
	eligible := func(heldBack bool) bool {
		status := incidents.CheckStatus{CheckState: *s, FindingStatuses: map[string]incidents.CheckStatus{}}
		for _, finding := range s.FindingStates() {
			status.FindingStatuses[finding.Finding] = incidents.CheckStatus{CheckState: *finding}
		}
		status.MarkReminderEligible(heldBack)
		return status.FindingStatuses["CVE-2026-123158 openssh"].FailureIncident.ReminderEligible
	}
	if eligible(false) {
		t.Fatal("a finding in its failing time, without an incident, is eligible")
	}
	run(versionA, true)
	if eligible(false) {
		t.Fatal("an incident that is open but not announced is eligible")
	}
	run(versionA, false)
	if !eligible(false) {
		t.Fatal("an open, announced incident is not eligible")
	}
	if eligible(true) {
		t.Fatal("an incident held back by maintenance or quiet time is eligible")
	}
}
