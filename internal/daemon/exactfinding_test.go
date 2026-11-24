package daemon

import (
	"context"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/testhelpers"
)

// A joined node whose reports reach its hub makes no reminder of a finding
// with an exact finding: the hub reminds once for the fleet incident key.
// A finding without an exact finding keeps the node's own reminders. Once a
// report fails, the node delivers without the hub and reminds of both.
// Status, and so the node status report, marks each open, announced
// incident eligible for reminders, and none while maintenance holds the
// check's alerts back.
func TestAJoinedNodeLeavesRemindersOfExactFindingsToTheHub(t *testing.T) {
	t.Parallel()
	ops := captureAlerts(t)
	_, code, _ := startHubWith(t, map[string]checksandalerts.Destination{
		"ops": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: ops.URL}, TimeLimitPerDeliveryAttempt: "1s", RemindEvery: "1h"},
	})
	node := joinedNode(t, code, "app-1")
	if e := node.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	opened := time.Now().UTC().Add(-time.Hour)
	node.mu.Lock()
	defer node.mu.Unlock()
	st := node.snapshot.Checks["app/shell"]
	st.OneIncidentForEachFinding = true
	o := checksandalerts.Observation{Protocol: 1, Status: "failed", Message: "2 flaws", At: opened, Findings: []checksandalerts.Finding{
		{ID: "CVE-2026-123158 openssh", Message: "openssh has CVE-2026-123158", ExactFinding: "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none"},
		{ID: "old-flaw", Message: "a finding without an exact finding"},
	}}
	if e := node.record(st.Observe(o, checksandalerts.Timing{}, false)); e != nil {
		t.Fatal(e)
	}
	reminded := func(at time.Time) map[string]bool {
		findings := map[string]bool{}
		for _, reminder := range node.remind(at) {
			findings[reminder.Finding] = true
		}
		return findings
	}
	if got := reminded(opened.Add(time.Hour)); len(got) != 1 || !got["old-flaw"] {
		t.Fatalf("reminders while the hub is reached: %v", got)
	}
	eligible := func() map[string]bool {
		checks, _, _ := node.status("", opened.Add(time.Hour), nil)
		found := map[string]bool{}
		for id, finding := range checks["app/shell"].FindingStatuses {
			found[id] = finding.FailureIncident.ReminderEligible
		}
		return found
	}
	if got := eligible(); !got["CVE-2026-123158 openssh"] || !got["old-flaw"] {
		t.Fatalf("reminder_eligible: %v", got)
	}
	node.link.LastError = "the fleet hub did not answer"
	if got := reminded(opened.Add(2 * time.Hour)); len(got) != 2 {
		t.Fatalf("reminders while the node delivers without the hub: %v", got)
	}
	node.snapshot.Maintenance["app"] = checksandalerts.Window{Until: opened.Add(3 * time.Hour)}
	if got := eligible(); got["CVE-2026-123158 openssh"] || got["old-flaw"] {
		t.Fatalf("reminder_eligible in maintenance: %v", got)
	}
}
