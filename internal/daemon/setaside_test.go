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
	status, body := fleetCommand(t, hub, "GET", "/fleet/set-aside-findings")
	if status != 200 || !strings.Contains(body, `"reason":"Needs musl without PAM"`) || !strings.Contains(body, `"matches_no_node_now":true`) {
		t.Fatal(status, body)
	}
	if status, body = fleetCommand(t, hub, "GET", "/fleet/findings?check=packages%2Fflaws"); status != 200 || !strings.Contains(body, `"check":"packages/flaws","findings":[]`) {
		t.Fatal(status, body)
	}
}

// Only the hub answers the two commands of set-aside findings.
func TestSetAsideFindingCommandsRunOnTheHub(t *testing.T) {
	t.Parallel()
	p, _ := daemonConfig(t)
	node, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { node.Close() })
	for path, want := range map[string]string{
		"/fleet/findings?check=packages%2Fflaws": "fleet findings runs on the hub; this node has no fleet_hub_role setting",
		"/fleet/set-aside-findings":              "fleet set-aside-findings runs on the hub; this node has no fleet_hub_role setting",
	} {
		if status, body := fleetCommand(t, node, "GET", path); status != 400 || !strings.Contains(body, want) {
			t.Error(path, status, body)
		}
	}
}

// The hub never reminds of its own finding that a set-aside entry covers:
// its own scheduler makes the reminders, as for any check of a node, and
// they stop where the hub forwards them, through two remind_every
// intervals.
func TestTheHubNeverRemindsOfItsOwnSetAsideFinding(t *testing.T) {
	t.Parallel()
	chat := checksandalerts.Destination{RemindEvery: "1h", AlertSeverities: []string{"warn"}, Inputs: &checksandalerts.Webhook{URL: "https://chat.example.com"}, TimeLimitPerDeliveryAttempt: "1s"}
	hub := newHubDaemonOnClock(t, map[string]checksandalerts.Destination{"chat": chat}, nil, time.Now)
	opened := time.Now().UTC().Add(-3 * time.Hour)
	hub.mu.Lock()
	st := hub.snapshot.Checks["app/shell"]
	st.OneIncidentForEachFinding = true
	o := checksandalerts.Observation{Protocol: 1, Status: "failed", Message: "1 flaw", At: opened, Findings: []checksandalerts.Finding{{ID: setAsideFlaw, Message: "openssh has CVE-2026-123158", ExactFinding: setAsideExact}}}
	e := hub.record(st.Observe(o, checksandalerts.Timing{}, false))
	hub.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	groupAt(t, hub, opened)
	if got := typesOf(queuedFor(t, hub, "chat")); !reflect.DeepEqual(got, []string{"failed"}) {
		t.Fatal(got)
	}
	// As a start or a reload settles the file. A reload would also retire
	// the findings of this shell check, whose module reports none.
	hub.mu.Lock()
	hub.config.SetAsideFindings = config.SetAsideFindings{{Check: "app/shell", Finding: setAsideFlaw, ExactFinding: setAsideExact, Reason: "Needs musl without PAM",
		InvestigationNote: "triage_agent_notes/openssh.md", SetAsideBy: "triage agent", SetAsideOn: "2026-11-23"}}
	hub.settleSetAsideFindings(hub.config, opened)
	hub.mu.Unlock()
	if hub.setAsideUnsettled {
		t.Fatal("the entry was not settled")
	}
	if got := typesOf(queuedFor(t, hub, "chat")); !reflect.DeepEqual(got, []string{"failed", "closed_without_recovery"}) {
		t.Fatal(got)
	}
	for interval := 1; interval <= 2; interval++ {
		at := opened.Add(time.Duration(interval) * time.Hour)
		hub.mu.Lock()
		reminders := hub.remind(at)
		e = hub.record(reminders)
		hub.mu.Unlock()
		if e != nil {
			t.Fatal(e)
		}
		if len(reminders) != 1 {
			t.Fatalf("interval %d: the hub's scheduler made %d reminders, want 1", interval, len(reminders))
		}
		groupAt(t, hub, at)
		if e = hub.store.SendDueUpdatesAndReminders(hub.config.Alerts.Destinations, at); e != nil {
			t.Fatal(e)
		}
		if got := typesOf(queuedFor(t, hub, "chat")); !reflect.DeepEqual(got, []string{"failed", "closed_without_recovery"}) {
			t.Fatalf("interval %d: %v", interval, got)
		}
	}
}
