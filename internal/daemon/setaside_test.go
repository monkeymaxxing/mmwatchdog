package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
)

const (
	setAsideFlaw  = "CVE-2026-123158 openssh"
	setAsideExact = "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none"
)

func setAsideFile(reason string) string {
	return `set_aside_findings:
  - check: packages/flaws
    finding: ` + setAsideFlaw + `
    exact_finding: "` + setAsideExact + `"
    reason: ` + reason + `
    investigation_note: triage_agent_notes/openssh.md
    set_aside_by: triage agent
    set_aside_on: 2026-11-23
`
}

// A reload applies a changed set_aside_findings.yml at once: the announced
// finding closes with closed_without_recovery. An invalid file is refused,
// and the configuration in force stays.
func TestAReloadAppliesSetAsideFindings(t *testing.T) {
	t.Parallel()
	chat := checksandalerts.Destination{RemindEvery: "never", AlertSeverities: []string{"warn"}, Inputs: &checksandalerts.Webhook{URL: "https://chat.example.com"}, TimeLimitPerDeliveryAttempt: "1s"}
	hub := newHubDaemonOnClock(t, map[string]checksandalerts.Destination{"chat": chat}, nil, time.Now)
	now := time.Now().UTC()
	failure := checksandalerts.Event{ID: incidents.ID(), Incident: incidents.ID(), CheckGroup: "packages", Check: "flaws", Finding: setAsideFlaw, ExactFinding: setAsideExact, Type: "failed", AlertSeverity: "warn", At: now, Message: setAsideExact}
	acceptAlert(t, hub, "app-1", failure)
	groupAt(t, hub, now)
	if got := typesOf(queuedFor(t, hub, "chat")); !reflect.DeepEqual(got, []string{"failed"}) {
		t.Fatal(got)
	}
	path := filepath.Join(hub.path, config.SetAsideFindingsFile)
	if e := os.WriteFile(path, []byte(strings.Replace(setAsideFile("x"), "    reason: x\n", "", 1)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := hub.reload(); e == nil || !strings.Contains(e.Error(), "set_aside_findings.0.reason: is required") {
		t.Fatal("an invalid file is not refused:", e)
	}
	if len(hub.config.SetAsideFindings) != 0 || len(queuedFor(t, hub, "chat")) != 1 {
		t.Fatal("a refused reload changed the configuration in force")
	}
	if e := os.WriteFile(path, []byte(setAsideFile("Needs musl without PAM")), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := hub.reload(); e != nil {
		t.Fatal(e)
	}
	queued := queuedFor(t, hub, "chat")
	if got := typesOf(queued); !reflect.DeepEqual(got, []string{"failed", "closed_without_recovery"}) || queued[1].Message != "Set aside after investigation: Needs musl without PAM" {
		t.Fatalf("%v %q", got, queued[len(queued)-1].Message)
	}
	if n := hub.hub.SetAsideFindings.Load(); n == nil || len(*n) != 1 {
		t.Fatal("the fleet views do not have the entry")
	}
}
