package incidents_test

import (
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
)

// MMWatchdog 0.2.0 makes apt_security_updates report its waiting updates
// as the one finding package security. A check of it that a 0.1.x node
// kept, with its one incident for the whole check, is a new check at the
// upgrade: its announced incident closes once, with a message that says
// why, and the finding then opens as an incident of its own. A healthy
// check changes without an alert.
func TestACheckWhoseModuleNowReportsFindingsRetiresItsIncidentOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1h", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	check := checksandalerts.Check{Inputs: &checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	defs := map[string]config.Definition{"software/no_security_update_waits": {CheckGroup: "software", Name: "no_security_update_waits", Check: check, Timing: timing, Severity: check.AlertSeveritySettings}}
	for _, failing := range []bool{true, false} {
		snap := incidents.FreshSnapshot()
		snap.Reconcile(&config.Config{}, defs, now)
		st := snap.Checks["software/no_security_update_waits"]
		if !st.OneIncidentForEachFinding {
			t.Fatal("a new apt_security_updates check has no incident for each finding")
		}
		// The state as MMWatchdog 0.1.x kept it: one incident for the check.
		st.OneIncidentForEachFinding = false
		status := "ok"
		if failing {
			status = "failed"
		}
		st.Observe(checksandalerts.Observation{Status: status, Message: "2 security updates wait to be installed: curl, openssl", At: now}, timing, false)
		announced := st.FailureIncident.Announced
		if failing == (announced == "") {
			t.Fatalf("failing %t, announced %q", failing, announced)
		}

		events := snap.Reconcile(&config.Config{}, defs, now.Add(time.Minute))
		switch {
		case failing && (len(events) != 1 || events[0].Type != "closed_without_recovery" || events[0].Incident != announced || !events[0].Notify ||
			!strings.HasPrefix(events[0].Message, "the check's module now reports each finding as an incident of its own")):
			t.Fatalf("upgrade of a failing check: %+v", events)
		case !failing && len(events) != 0:
			t.Fatalf("upgrade of a healthy check: %+v", events)
		}
		st = snap.Checks["software/no_security_update_waits"]
		if !st.OneIncidentForEachFinding || st.FailureIncident.Incident != "" || st.FailureIncident.Announced != "" {
			t.Fatalf("the check after the upgrade: %+v", st)
		}
		if events := snap.Reconcile(&config.Config{}, defs, now.Add(2*time.Minute)); len(events) != 0 {
			t.Fatalf("a second start retired the check again: %+v", events)
		}
	}
}
