package incidents_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

// MMWatchdog 0.2.0 makes apt_security_updates report its waiting updates
// as the one finding package security. A check of it that a 0.1.3 node
// kept in state.db, with its one incident for the whole check, is a new
// check at the upgrade: its announced incident closes once, with a message
// that says why, and the waiting updates that the upgraded check then
// observes, twice, open the finding as one incident of its own. A healthy
// check changes without an alert.
func TestACheckWhoseModuleNowReportsFindingsRetiresItsIncidentOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1h", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	check := checksandalerts.Check{Inputs: &checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	const key = "software/no_security_update_waits"
	defs := map[string]config.Definition{key: {CheckGroup: "software", Name: "no_security_update_waits", Check: check, Timing: timing, Severity: check.AlertSeveritySettings}}
	const waiting = "2 security updates wait to be installed: curl, openssl"
	for _, failing := range []bool{true, false} {
		snap := incidents.FreshSnapshot()
		snap.Reconcile(&config.Config{}, defs, now)
		st := snap.Checks[key]
		if !st.OneIncidentForEachFinding {
			t.Fatal("a new apt_security_updates check has no incident for each finding")
		}
		// The state as MMWatchdog 0.1.3 kept it: one incident for the check.
		st.OneIncidentForEachFinding = false
		status := "ok"
		if failing {
			status = "failed"
		}
		st.Observe(checksandalerts.Observation{Status: status, Message: waiting, At: now}, timing, false)
		announced := st.FailureIncident.Announced
		if failing == (announced == "") {
			t.Fatalf("failing %t, announced %q", failing, announced)
		}
		// 0.1.3 saved it in state.db, and 0.2.0 starts from what it loads.
		dir := filepath.Join(t.TempDir(), "state")
		s, e := store.OpenStore(dir, config.DefaultStorageLimits)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Save(snap, nil, store.AlertRoute{}); e != nil {
			t.Fatal(e)
		}
		s.Close()
		if s, e = store.OpenStore(dir, config.DefaultStorageLimits); e != nil {
			t.Fatal(e)
		}
		snap, e = s.Load()
		s.Close()
		if e != nil {
			t.Fatal(e)
		}
		if st = snap.Checks[key]; st == nil || st.OneIncidentForEachFinding || st.FailureIncident.Announced != announced {
			t.Fatalf("the loaded 0.1.3 state: %+v", st)
		}

		events := snap.Reconcile(&config.Config{}, defs, now.Add(time.Minute))
		events = append(events, snap.Reconcile(&config.Config{}, defs, now.Add(2*time.Minute))...)
		st = snap.Checks[key]
		if !st.OneIncidentForEachFinding || st.FailureIncident.Incident != "" || st.FailureIncident.Announced != "" {
			t.Fatalf("the check after the upgrade: %+v", st)
		}
		// The upgraded check observes the waiting updates twice.
		for run := range 2 {
			events = append(events, st.Observe(checksandalerts.Observation{Protocol: 1, Status: status, Message: waiting, At: now.Add(time.Duration(run+1) * time.Hour),
				Findings: findingsIf(failing, checksandalerts.Finding{ID: checksandalerts.PackageSecurity, Message: waiting, ExactFinding: "debian 13: security updates wait"})}, timing, false)...)
		}
		retired, opened := 0, 0
		for _, e := range events {
			switch {
			case e.Type == "closed_without_recovery" && e.Incident == announced && e.Notify && strings.HasPrefix(e.Message, "the check's module now reports each finding as an incident of its own"):
				retired++
			case e.Type == "failed" && e.Finding == checksandalerts.PackageSecurity && e.Notify && e.Incident != announced:
				opened++
			default:
				t.Fatalf("an alert other than the retirement and the opening: %+v", e)
			}
		}
		if want := map[bool]int{true: 1, false: 0}[failing]; retired != want || opened != want {
			t.Fatalf("failing %t: %d retirements of the 0.1.3 incident and %d openings of %s, want %d of each", failing, retired, opened, checksandalerts.PackageSecurity, want)
		}
	}
}

// findingsIf is findings when failing, otherwise none.
func findingsIf(failing bool, findings ...checksandalerts.Finding) []checksandalerts.Finding {
	if failing {
		return findings
	}
	return nil
}
