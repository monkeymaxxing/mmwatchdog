package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// The tests of the every_systemd_unit check: one incident for each
// failed systemd unit. The rules are in docs/changing-mmwatchdog/design/checks.md under "Failed
// systemd units".

// failedUnitsCheck is the state of the design's example check.
func failedUnitsCheck(severity checksandalerts.AlertSeveritySettings) *incidents.CheckState {
	return &incidents.CheckState{CheckGroup: "failed_systemd_units", Check: "machine_has_no_failed_systemd_unit", AlertSeveritySettings: severity, OneIncidentForEachFinding: true}
}

// unitsObservation is what the check observes when systemd lists units as
// failed, each with the result exit-code, or lists none.
// failedUnit is the finding of one unit that systemd reports as failed, as
// an every_systemd_unit check reports it.
func failedUnit(unit, systemdResult string) checksandalerts.Finding {
	return checksandalerts.Finding{ID: unit, Message: primitives.FailedUnitMessage(unit, systemdResult)}
}

func unitsObservation(at time.Time, units ...string) checksandalerts.Observation {
	o := checksandalerts.Result("ok", "no failed systemd units")
	if len(units) > 0 {
		o = checksandalerts.Result("failed", "systemd units failed: "+strings.Join(units, ", "))
	}
	o.FindingIsGoneMessage = primitives.UnitIsNoLongerFailed
	for _, unit := range units {
		o.Findings = append(o.Findings, failedUnit(unit, "exit-code"))
	}
	o.At = at
	return o
}

// systemdFails makes the fake systemctl report the units as failed, each
// with the result exit-code.
func systemdFails(t *testing.T, listed, shown string, units ...string) {
	t.Helper()
	list, show := "", ""
	for _, unit := range units {
		list += unit + " loaded failed failed A unit of the test\n"
		show += "Id=" + unit + "\nActiveState=failed\nResult=exit-code\n\n"
	}
	for path, text := range map[string]string{listed: list, shown: show} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// The check asks systemd for every failed unit, of every kind, and leaves
// out the units it ignores. It is unknown when systemd cannot be asked.
func TestFailedSystemdUnitsCheckAsksSystemd(t *testing.T) {
	t.Parallel()
	listed, shown, broken := testhelpers.FakeSystemctl(t)
	check := checksandalerts.Check{Inputs: &checksandalerts.EverySystemdUnit{FailWhen: "a_systemd_unit_has_failed", SystemdUnitsToIgnore: []string{"myapp.service"}}}
	observe := func() checksandalerts.Observation {
		return checksandalerts.ObserveCheck(testhelpers.TestContext(t), check)
	}
	if o := observe(); o.Status != "ok" || o.Message != "no failed systemd units" || len(o.Findings) != 0 {
		t.Fatalf("%+v", o)
	}
	// A systemd that marks each line with a dot writes the unit second. The
	// mount left the failed state between the two questions.
	os.WriteFile(listed, []byte("certbot.service loaded failed failed Certbot\n● backup.service loaded failed failed Nightly backup\nmyapp.service loaded failed failed My app\nvar-lib-data.mount loaded failed failed /var/lib/data\nlogrotate.timer loaded failed failed Rotate logs\n"), 0600)
	os.WriteFile(shown, []byte("Id=backup.service\nActiveState=failed\nResult=exit-code\n\nId=certbot.service\nResult=signal\nActiveState=failed\n\nId=myapp.service\nActiveState=failed\nResult=exit-code\n\nId=var-lib-data.mount\nActiveState=active\nResult=success\n"), 0600)
	o := observe()
	want := []checksandalerts.Finding{failedUnit("backup.service", "exit-code"), failedUnit("certbot.service", "signal"), failedUnit("logrotate.timer", "")}
	if o.Status != "failed" || o.Message != "3 systemd units failed: backup.service, certbot.service, logrotate.timer (not counting 1 in systemd_units_to_ignore: myapp.service)" || !reflect.DeepEqual(o.Findings, want) {
		t.Fatalf("%+v", o)
	}
	if want[0].Message != "systemd unit backup.service failed (systemd result: exit-code)" || want[2].Message != "systemd unit logrotate.timer failed" {
		t.Fatal(want[0].Message, want[2].Message)
	}
	// Only an ignored unit is failed: the check is healthy and says so.
	os.WriteFile(listed, []byte("myapp.service loaded failed failed My app\n"), 0600)
	if o = observe(); o.Status != "ok" || o.Message != "no failed systemd units (not counting 1 in systemd_units_to_ignore: myapp.service)" {
		t.Fatalf("%+v", o)
	}
	os.WriteFile(listed, []byte("backup.service loaded failed failed Nightly backup\n"), 0600)
	if o = observe(); o.Status != "failed" || o.Message != "1 systemd unit failed: backup.service" {
		t.Fatalf("%+v", o)
	}
	os.WriteFile(broken, nil, 0600)
	if o = observe(); o.Status != "unknown" || !strings.Contains(o.Message, "MMWatchdog could not ask systemd for its failed units") || !strings.Contains(o.Message, "Failed to connect to bus") {
		t.Fatalf("%+v", o)
	}
}

// The design's example, minute by minute: each failed unit is its own
// incident with its own alert and its own recovery, and the check itself
// opens none.
func TestEachFailedSystemdUnitIsItsOwnIncident(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	at := func(hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "1m"}
	s := failedUnitsCheck(checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"})
	observe := func(hour, minute int, units ...string) []checksandalerts.Event {
		t.Helper()
		events := s.Observe(unitsObservation(at(hour, minute), units...), timing, false)
		if s.FailureIncident.Incident != "" {
			t.Fatal("the check opened an incident of its own")
		}
		return events
	}
	texts := func(event checksandalerts.Event) (string, string) {
		event.Node = "app-1"
		event.WriteAlertTexts([]checksandalerts.Event{event}, "")
		return event.Title, event.Body
	}
	// At 02:00 backup.service fails; at 02:02 MMWatchdog sends its alert.
	if len(observe(2, 0, "backup.service")) != 0 || len(observe(2, 1, "backup.service")) != 0 {
		t.Fatal("alerted before alert_after_failing_for")
	}
	if s.Summary(at(2, 1), false) != "confirming_failure" {
		t.Fatal(s.Summary(at(2, 1), false))
	}
	events := observe(2, 2, "backup.service")
	if len(events) != 1 || events[0].Type != "failed" || !events[0].Notify || events[0].Finding != "backup.service" || events[0].AlertSeverity != "warn" ||
		events[0].Message != "systemd unit backup.service failed (systemd result: exit-code)" {
		t.Fatalf("%+v", events)
	}
	backup := events[0].Incident
	if title, body := texts(events[0]); title != "app-1: failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service failed" ||
		body != "systemd unit backup.service failed (systemd result: exit-code)\nfailing since 02:00 UTC" {
		t.Fatalf("%q\n%q", title, body)
	}
	if s.Summary(at(2, 2), false) != "failed" {
		t.Fatal(s.Summary(at(2, 2), false))
	}
	// At 03:00 certbot.service fails too; at 03:02 it gets its own alert.
	for minute := 3; minute < 60; minute++ {
		if events = observe(2, minute, "backup.service"); len(events) != 0 {
			t.Fatalf("%+v", events)
		}
	}
	if len(observe(3, 0, "backup.service", "certbot.service")) != 0 || len(observe(3, 1, "backup.service", "certbot.service")) != 0 {
		t.Fatal("the second unit alerted before its own alert_after_failing_for")
	}
	events = observe(3, 2, "backup.service", "certbot.service")
	if len(events) != 1 || events[0].Type != "failed" || events[0].Finding != "certbot.service" || events[0].Incident == backup || events[0].Evidence.FailingSince != at(3, 0) {
		t.Fatalf("%+v", events)
	}
	certbot := events[0].Incident
	// At 09:10 the backup is repaired; at 09:11 MMWatchdog sends its recovery. The
	// incident of certbot.service stays open.
	if events = observe(9, 10, "certbot.service"); len(events) != 0 {
		t.Fatalf("%+v", events)
	}
	events = observe(9, 11, "certbot.service")
	if len(events) != 1 || events[0].Type != "recovered" || !events[0].Notify || events[0].Incident != backup || events[0].Finding != "backup.service" ||
		events[0].Message != "systemd unit backup.service is no longer failed" {
		t.Fatalf("%+v", events)
	}
	if title, _ := texts(events[0]); title != "app-1: failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service is ok again" {
		t.Fatal(title)
	}
	if len(s.Findings) != 1 || s.Findings["certbot.service"].FailureIncident.Incident != certbot || s.Summary(at(9, 11), false) != "failed" {
		t.Fatalf("%+v", s.Findings)
	}
	// The last unit recovers: the check is healthy and holds no unit.
	observe(9, 12)
	if s.Summary(at(9, 12), false) != "confirming_recovery" {
		t.Fatal(s.Summary(at(9, 12), false))
	}
	events = observe(9, 13)
	if len(events) != 1 || events[0].Type != "recovered" || events[0].Incident != certbot || len(s.Findings) != 0 || !s.FailingSince.IsZero() || s.Summary(at(9, 13), false) != "ok" {
		t.Fatalf("%+v %+v", events, s)
	}
}

// A unit that fails on and off stays in one failing time, and a unit that
// stops failing before its incident opens is forgotten without an alert.
func TestAFailedUnitFollowsTheChecksTimingsByItself(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "2m"}
	s := failedUnitsCheck(checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"})
	observe := func(minute int, units ...string) []checksandalerts.Event {
		return s.Observe(unitsObservation(start.Add(time.Duration(minute)*time.Minute), units...), timing, false)
	}
	observe(0, "flaky.service")
	if events := observe(1); len(events) != 0 || s.Findings["flaky.service"] == nil {
		t.Fatal("one healthy minute ended the unit's failing time", events)
	}
	events := observe(2, "flaky.service")
	if len(events) != 1 || events[0].Evidence.FailingSince != start || events[0].Evidence.LastRunsFailed != 2 || events[0].Evidence.LastRunsCounted != 3 {
		t.Fatalf("%+v", events)
	}
	s = failedUnitsCheck(checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"})
	observe(0, "brief.service")
	observe(1)
	observe(2)
	if events = observe(3); len(events) != 0 || len(s.Findings) != 0 || !s.FailingSince.IsZero() {
		t.Fatalf("%+v %+v", events, s.Findings)
	}
}

// While MMWatchdog cannot ask systemd, the check has a cannot-check incident like
// every check, an open incident of a unit stays open, and unobserved time
// earns no credit.
func TestFailedUnitsWhileSystemdCannotBeAsked(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "2m"}
	s := failedUnitsCheck(checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"})
	minute := 0
	observe := func(units ...string) []checksandalerts.Event {
		minute++
		return s.Observe(unitsObservation(start.Add(time.Duration(minute)*time.Minute), units...), timing, false)
	}
	unknown := func() []checksandalerts.Event {
		minute++
		o := checksandalerts.Result("unknown", "systemd inspection: MMWatchdog could not ask systemd for its failed units: exit status 1")
		o.At = start.Add(time.Duration(minute) * time.Minute)
		return s.Observe(o, timing, false)
	}
	observe("backup.service")
	observe("backup.service")
	events := observe("backup.service", "certbot.service")
	if len(events) != 1 || events[0].Finding != "backup.service" {
		t.Fatalf("%+v", events)
	}
	backup := events[0].Incident
	// The open incident of backup.service was raised to sos; certbot.service
	// has no incident yet.
	s.Findings["backup.service"].FailureIncident.AlertSeverity = "sos"
	if events = unknown(); len(events) != 0 || s.Findings["certbot.service"] == nil || s.Findings["backup.service"].FailureIncident.Incident != backup || s.FailingSince.IsZero() {
		t.Fatalf("%+v %+v", events, s.Findings)
	}
	if s.Summary(start.Add(time.Hour), false) != "failed" {
		t.Fatal(s.Summary(start.Add(time.Hour), false))
	}
	unknown()
	// The cannot-check incident is the check's, names no unit, and takes
	// the severity of the open incident.
	events = unknown()
	if len(events) != 1 || events[0].Type != "cannot_check" || events[0].Finding != "" || events[0].AlertSeverity != "sos" {
		t.Fatalf("%+v", events)
	}
	// systemd answers again and no longer lists the unit. The unit must be
	// healthy for recovered_after_healthy_for from here.
	events = observe()
	if len(events) != 1 || events[0].Type != "can_check_again" {
		t.Fatalf("%+v", events)
	}
	observe()
	if events = unknown(); len(events) != 0 {
		t.Fatalf("%+v", events)
	}
	if events = observe(); len(events) != 0 {
		t.Fatal("time in which systemd could not be asked counted as healthy time", events)
	}
	observe()
	events = observe()
	if len(events) != 1 || events[0].Type != "recovered" || events[0].Incident != backup || len(s.Findings) != 0 {
		t.Fatalf("%+v", events)
	}
}

// A maintenance window holds the alert of a failed unit and the recovery of
// an announced one, and both go out when the window has ended.
func TestFailedUnitsInMaintenance(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	s := failedUnitsCheck(checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"})
	observe := func(suppressed bool, units ...string) []checksandalerts.Event {
		now = now.Add(time.Minute)
		return s.Observe(unitsObservation(now, units...), timing, suppressed)
	}
	events := observe(true, "backup.service")
	if len(events) != 1 || events[0].Type != "failed" || events[0].Notify {
		t.Fatalf("%+v", events)
	}
	incident := events[0].Incident
	events = observe(false, "backup.service")
	if len(events) != 1 || events[0].Type != "failed" || !events[0].Notify || events[0].Incident != incident || events[0].Finding != "backup.service" {
		t.Fatalf("%+v", events)
	}
	// The unit recovers during a window: the recovery is recorded, and the
	// unit is kept until its recovery has gone out.
	events = observe(true)
	if len(events) != 1 || events[0].Type != "recovered" || events[0].Notify || s.Findings["backup.service"] == nil {
		t.Fatalf("%+v %+v", events, s.Findings)
	}
	s.Interrupt()
	if s.Findings["backup.service"] == nil {
		t.Fatal("the end of the window forgot a unit whose recovery has not gone out")
	}
	events = observe(false)
	if len(events) != 1 || events[0].Type != "recovered" || !events[0].Notify || events[0].Incident != incident || len(s.Findings) != 0 {
		t.Fatalf("%+v %+v", events, s.Findings)
	}
}

// Each failed unit is raised and reminded of by itself, and its messages
// name it.
func TestFailedUnitsAreRaisedAndRemindedOfByThemselves(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "1m"}
	s := failedUnitsCheck(checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: "30m"})
	s.Observe(unitsObservation(start, "backup.service"), timing, false)
	s.Observe(unitsObservation(start.Add(20*time.Minute), "backup.service", "certbot.service"), timing, false)
	if !s.Reminding() || !s.FailureIncidentOpen() {
		t.Fatal("the check does not know of its units' incidents")
	}
	about := "failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service"
	events := s.RaiseAfterTime(start.Add(30*time.Minute), false)
	if len(events) != 1 || events[0].Type != checksandalerts.FailureIncidentEvents.Raised || events[0].Finding != "backup.service" || events[0].AlertSeverity != "sos" ||
		events[0].Message != about+" has been failing for 30m, so this alert is now SOS (raise_to_sos_after_failing_for: 30m)" {
		t.Fatalf("%+v", events)
	}
	events[0].Node = "app-1"
	events[0].WriteAlertTexts(events[:1], "")
	if events[0].Title != "app-1: "+about+" still failing, now SOS" {
		t.Fatal(events[0].Title)
	}
	if s.Findings["certbot.service"].FailureIncident.AlertSeverity != "warn" {
		t.Fatal("the raise of one unit raised another")
	}
	// A higher alert_severity on the check raises the unit that is still warn.
	s.AlertSeveritySettings.AlertSeverity = "sos"
	events = s.RaiseToPolicy(start.Add(31*time.Minute), false)
	if len(events) != 1 || events[0].Finding != "certbot.service" || events[0].AlertSeverity != "sos" {
		t.Fatalf("%+v", events)
	}
	// Each unit's reminders count from its own raise.
	ops := map[string]checksandalerts.Destination{"ops": {AlertSeverities: testhelpers.AllSeverities, RemindEvery: "1h"}}
	events = s.Reminders(ops, start.Add(90*time.Minute))
	if len(events) != 1 || events[0].Type != "reminder" || events[0].Destination != "ops" || events[0].Finding != "backup.service" ||
		events[0].Message != about+" still failing since 02:00 UTC; latest result: systemd unit backup.service failed (systemd result: exit-code)" {
		t.Fatalf("%+v", events)
	}
	events = s.Reminders(ops, start.Add(91*time.Minute))
	if len(events) != 1 || events[0].Finding != "certbot.service" {
		t.Fatalf("%+v", events)
	}
	// Reminders resume one interval after a maintenance window, for every unit.
	s.ResumeReminders(start.Add(100 * time.Minute))
	if events = s.Reminders(ops, start.Add(159*time.Minute)); len(events) != 0 {
		t.Fatalf("%+v", events)
	}
	if events = s.Reminders(ops, start.Add(160*time.Minute)); len(events) != 2 {
		t.Fatalf("%+v", events)
	}
}

// mmwatchdog validate names every unit that has its own systemd_unit_state check
// and is missing from systemd_units_to_ignore, and the list is required.
func TestSystemdUnitsToIgnore(t *testing.T) {
	t.Parallel()
	c, err := config.Load(testhelpers.ConfigFile(t, `check_groups:
  failed_systemd_units:
    checks:
      machine_has_no_failed_systemd_unit: {every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: [myapp.service, backup.timer]}}
  myapp:
    checks:
      myapp_service_is_running: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}}
      worker_service_is_running: {systemd_unit_state: {systemd_unit: worker.service, fail_when: the_unit_is_not_active}}
      worker_is_not_crash_looping: {systemd_unit_crash_loop: {systemd_unit: cron.service, automatic_restarts_above: 2, automatic_restarts_within: 1h}}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []checksandalerts.SystemdUnitMissingFromUnitsToIgnore{{CheckGroup: "failed_systemd_units", Check: "machine_has_no_failed_systemd_unit", SystemdUnit: "worker.service", SystemdUnitStateCheck: "myapp/worker_service_is_running"}}
	if missing := c.SystemdUnitsMissingFromUnitsToIgnore(); !reflect.DeepEqual(missing, want) {
		t.Fatalf("%+v", missing)
	}
	def := config.CheckDefinitions(c)["failed_systemd_units/machine_has_no_failed_systemd_unit"]
	if def.Check.ModuleName() != "every_systemd_unit" || !reflect.DeepEqual(def.Check.Inputs.(*checksandalerts.EverySystemdUnit).SystemdUnitsToIgnore, []string{"backup.timer", "myapp.service"}) {
		t.Fatalf("%+v", def)
	}
	// The ignored units are part of the check's definition; the order in
	// which they are written is not.
	other := def
	other.Check.Inputs = &checksandalerts.EverySystemdUnit{FailWhen: "a_systemd_unit_has_failed", SystemdUnitsToIgnore: []string{"myapp.service"}}
	if def.Identity() == other.Identity() {
		t.Fatal("a changed systemd_units_to_ignore kept the check's identity")
	}
	reordered := *c
	reordered.CheckGroups = map[string]checksandalerts.CheckGroup{"failed_systemd_units": c.CheckGroups["failed_systemd_units"]}
	group := reordered.CheckGroups["failed_systemd_units"]
	check := group.Checks["machine_has_no_failed_systemd_unit"]
	check.Inputs = &checksandalerts.EverySystemdUnit{FailWhen: "a_systemd_unit_has_failed", SystemdUnitsToIgnore: []string{"backup.timer", "myapp.service"}}
	group.Checks = map[string]checksandalerts.NamedCheck{"machine_has_no_failed_systemd_unit": check}
	reordered.CheckGroups["failed_systemd_units"] = group
	if again := config.CheckDefinitions(&reordered)["failed_systemd_units/machine_has_no_failed_systemd_unit"]; again.Identity() != def.Identity() {
		t.Fatal("writing the ignored units in another order changed the check's identity")
	}
	// The check can fail, so it is among the checks that trigger its check
	// group's recovery command by default.
	c, err = config.Load(testhelpers.ConfigFile(t, "check_groups: {a: {checks: {c: {every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: []}}}, recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix}}}\n"))
	if err != nil || !reflect.DeepEqual(c.CheckGroups["a"].RecoveryCommand.TriggeredByChecks, []string{"c"}) {
		t.Fatal(c, err)
	}
	for _, tc := range []struct{ check, want string }{
		{"{every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: []}}", ""},
		{"{every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: [proc-sys-fs-binfmt_misc.automount, 'dev-disk-by\\x2duuid-0a1b.swap', 'getty@tty1.service']}}", ""},
		{"{every_systemd_unit: {fail_when: a_systemd_unit_has_failed}}", "check_groups.a.checks.c.every_systemd_unit.systemd_units_to_ignore: is required: the systemd units whose failure this check leaves out, such as [myapp.service], or [] to leave none out"},
		{"{every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: }}", "expected a list"},
		{"{every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: [myapp]}}", "check_groups.a.checks.c.every_systemd_unit.systemd_units_to_ignore: expected distinct systemd unit names with their kind"},
		{"{every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: [a.service, a.service]}}", "expected distinct systemd unit names with their kind"},
		{"{every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: [], units: []}}", `unknown field "units"`},
		{"{every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: []}, fail_when_measurement_above: {restarts: 1}}", "the module every_systemd_unit reports no measurement, so its check takes no limit"},
	} {
		_, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {a: {checks: {c: "+tc.check+"}}}\n"))
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: got %v, want %q", tc.check, err, tc.want)
		}
	}
}

// unitsDaemon starts a daemon whose one check is the design's example and a
// fake systemctl, and returns a function that runs the check once at the
// next minute with the given units failed.
func unitsDaemon(t *testing.T, timing checksandalerts.Timing) (string, *config.Config, *Daemon, func(d *Daemon, units ...string) *incidents.CheckState) {
	t.Helper()
	listed, shown, _ := testhelpers.FakeSystemctl(t)
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"failed_systemd_units": {Timing: timing, Checks: map[string]checksandalerts.NamedCheck{
		"machine_has_no_failed_systemd_unit": {Check: checksandalerts.Check{Inputs: &checksandalerts.EverySystemdUnit{FailWhen: "a_systemd_unit_has_failed", SystemdUnitsToIgnore: []string{"myapp.service"}}}},
	}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	observe := func(d *Daemon, units ...string) *incidents.CheckState {
		t.Helper()
		systemdFails(t, listed, shown, units...)
		d.schedule(testhelpers.TestContext(t), now)
		d.workers.Wait()
		now = now.Add(time.Minute)
		return d.snapshot.Checks["failed_systemd_units/machine_has_no_failed_systemd_unit"]
	}
	return p, c, d, observe
}

// A scheduled run of the check through the daemon writes the recovery of a
// unit in the words of its module.
func TestTheRecoveryOfAFailedUnitSaysThatTheUnitIsNoLongerFailed(t *testing.T) {
	t.Parallel()
	_, _, d, observe := unitsDaemon(t, checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"})
	defer d.Close()
	if st := observe(d, "backup.service"); st.Findings["backup.service"] == nil || st.Findings["backup.service"].FailureIncident.Incident == "" {
		t.Fatalf("%+v", st)
	}
	observe(d)
	events, err := d.store.CheckGroupEvents("failed_systemd_units", 100)
	if err != nil {
		t.Fatal(err)
	}
	recovered := ""
	for _, event := range events {
		if event.Type == "recovered" && event.Finding == "backup.service" {
			recovered = event.Message
		}
	}
	if recovered != "systemd unit backup.service is no longer failed" {
		t.Fatalf("the recovery says %q: %s", recovered, eventSummary(events))
	}
}

// Inspection shows the check with one entry for each failed unit, a daemon
// restart keeps the units' incidents, and a changed check closes them
// without a recovery.
func TestFailedUnitsInStatusAcrossRestartAndReload(t *testing.T) {
	t.Parallel()
	p, c, d, observe := unitsDaemon(t, checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"})
	defer func() { d.Close() }()
	st := observe(d, "backup.service", "certbot.service", "myapp.service")
	if len(st.Findings) != 2 || st.FailureIncident.Incident != "" || st.Last.Message != "2 systemd units failed: backup.service, certbot.service (not counting 1 in systemd_units_to_ignore: myapp.service)" {
		t.Fatalf("%+v", st)
	}
	backup := st.Findings["backup.service"].FailureIncident.Incident
	recorder := httptest.NewRecorder()
	d.routes().ServeHTTP(recorder, httptest.NewRequest("GET", "http://localhost/status", nil))
	var status struct {
		Summary incidents.StatusSummary `json:"summary"`
		Checks  map[string]struct {
			State    string `json:"state"`
			Findings map[string]struct {
				State           string                      `json:"state"`
				Finding         string                      `json:"finding"`
				Last            checksandalerts.Observation `json:"last_observation"`
				FailureIncident incidents.Track             `json:"failure_incident"`
			} `json:"findings"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	check := status.Checks["failed_systemd_units/machine_has_no_failed_systemd_unit"]
	unit := check.Findings["backup.service"]
	if check.State != "failed" || len(check.Findings) != 2 || unit.State != "failed" || unit.Finding != "backup.service" || unit.FailureIncident.Incident != backup ||
		unit.Last.Message != "systemd unit backup.service failed (systemd result: exit-code)" {
		t.Fatalf("%s", recorder.Body.String())
	}
	attention := status.Summary.Attention
	if status.Summary.ExitCode != 1 || len(attention) != 2 || attention[0].Check != "failed_systemd_units/machine_has_no_failed_systemd_unit" || attention[0].Finding != "backup.service" ||
		attention[0].State != "failed" || attention[0].Message != "systemd unit backup.service failed (systemd result: exit-code)" || attention[1].Finding != "certbot.service" {
		t.Fatalf("%+v", status.Summary)
	}
	// mmwatchdog check-now lists the units and changes nothing.
	observations, err := d.checkNowGroup(testhelpers.TestContext(t), "failed_systemd_units")
	if o := observations["machine_has_no_failed_systemd_unit"]; err != nil || o.Status != "failed" || len(o.Findings) != 2 {
		t.Fatal(observations, err)
	}
	// A daemon restart keeps both incidents and sends nothing.
	d.Close()
	if d, err = NewDaemon(p); err != nil {
		t.Fatal(err)
	}
	st = d.snapshot.Checks["failed_systemd_units/machine_has_no_failed_systemd_unit"]
	if len(st.Findings) != 2 || st.Findings["backup.service"].FailureIncident.Incident != backup {
		t.Fatalf("%+v", st.Findings)
	}
	st = observe(d, "backup.service", "certbot.service")
	if st.Findings["backup.service"].FailureIncident.Incident != backup {
		t.Fatal("the restart opened a new incident")
	}
	// The unit joins systemd_units_to_ignore: the check changed, so the
	// incident of each unit closes without a recovery.
	check2 := c.CheckGroups["failed_systemd_units"].Checks["machine_has_no_failed_systemd_unit"]
	check2.Inputs = &checksandalerts.EverySystemdUnit{FailWhen: "a_systemd_unit_has_failed", SystemdUnitsToIgnore: []string{"myapp.service", "backup.service"}}
	c.CheckGroups["failed_systemd_units"].Checks["machine_has_no_failed_systemd_unit"] = check2
	writeConfig(t, p, c)
	if _, err = d.reload(); err != nil {
		t.Fatal(err)
	}
	events, err := d.store.Events(100)
	if err != nil {
		t.Fatal(err)
	}
	history := []string{}
	for i := len(events) - 1; i >= 0; i-- {
		history = append(history, events[i].Type+" "+events[i].Finding)
	}
	if !reflect.DeepEqual(history, []string{"failed backup.service", "failed certbot.service", "closed_without_recovery backup.service", "closed_without_recovery certbot.service"}) {
		t.Fatal(history)
	}
	if events[1].Incident != backup || events[1].Message != "the check was changed in the configuration; MMWatchdog does not know whether the problem is fixed" || len(d.snapshot.Checks["failed_systemd_units/machine_has_no_failed_systemd_unit"].Findings) != 0 {
		t.Fatalf("%+v", events[1])
	}
	// The unit that is still failed and not ignored gets a new incident.
	st = observe(d, "backup.service", "certbot.service")
	if len(st.Findings) != 1 || st.Findings["certbot.service"].FailureIncident.Incident == "" {
		t.Fatalf("%+v", st.Findings)
	}
}

// A recovery command that the check triggers runs while a unit is failed,
// and its attempts reset only when no unit has an open incident.
func TestFailedUnitsTriggerARecoveryCommand(t *testing.T) {
	t.Parallel()
	p, c, d, observe := unitsDaemon(t, checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "2m"})
	d.Close()
	marker := filepath.Join(t.TempDir(), "ran")
	group := c.CheckGroups["failed_systemd_units"]
	group.RecoveryCommand = &checksandalerts.RecoveryCommand{Script: "echo \"$MMWATCHDOG_CHECK\" > " + testhelpers.ShellQuote(marker), TimeLimitPerAttempt: "5s", ExecOptions: testhelpers.OwnOptions(t), RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"machine_has_no_failed_systemd_unit"}, Attempts: "1", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "1s"}}
	c.CheckGroups["failed_systemd_units"] = group
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	observe(d, "backup.service")
	d.workers.Wait()
	if b, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(b)) != "machine_has_no_failed_systemd_unit" {
		t.Fatal(string(b), err)
	}
	// The unit is no longer listed, but its incident is open until it has
	// been healthy for recovered_after_healthy_for: no healthy window yet.
	observe(d)
	d.mu.Lock()
	since := d.healthySince("failed_systemd_units")
	d.mu.Unlock()
	if !since.IsZero() {
		t.Fatal("a healthy window began while the incident of a unit is open")
	}
}

// On the hub, the alerts of each failed unit are grouped by themselves, and
// a grouped message names the unit.
func TestTheHubGroupsFailedUnitsByUnit(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 29, 2, 2, 0, 0, time.UTC)
	alert := func(node, unit string) checksandalerts.Event {
		return checksandalerts.Event{ID: incidents.ID(), Incident: incidents.ID(), Node: node, CheckGroup: "failed_systemd_units", Check: "machine_has_no_failed_systemd_unit", Finding: unit, Type: "failed", AlertSeverity: "warn", At: at,
			Message: "systemd unit " + unit + " failed (systemd result: exit-code)"}
	}
	backup1, backup2, certbot := alert("app-1", "backup.service"), alert("app-2", "backup.service"), alert("app-1", "certbot.service")
	if store.IntakeSubject(backup1) != store.IntakeSubject(backup2) || store.IntakeSubject(backup1) == store.IntakeSubject(certbot) {
		t.Fatal("alerts are not grouped by unit")
	}
	if e := store.IntakeSubjectEvent("app-1", store.IntakeSubject(certbot), "failed"); e.CheckGroup != certbot.CheckGroup || e.Check != certbot.Check || e.Finding != "certbot.service" {
		t.Fatalf("%+v", e)
	}
	if e := store.IntakeSubjectEvent("app-1", store.IntakeSubject(checksandalerts.Event{CheckGroup: "myapp", Check: "http"}), "failed"); e.CheckGroup != "myapp" || e.Check != "http" || e.Finding != "" {
		t.Fatalf("%+v", e)
	}
	message := store.Combine([]checksandalerts.Event{backup1, backup2}, "util")
	message.WriteAlertTexts([]checksandalerts.Event{backup1, backup2}, "util")
	if message.Finding != "backup.service" || message.Title != "failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service failed on 2 nodes" ||
		!strings.HasPrefix(message.Body, "failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service failed on 2 nodes: app-1, app-2\n") {
		t.Fatalf("%q\n%q", message.Title, message.Body)
	}
	// Each unit has its own MMHotline incident key.
	if key := checksandalerts.ReadableFleetIncidentKey(backup1, "failed", "util"); key != "failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service failed" || key == checksandalerts.ReadableFleetIncidentKey(certbot, "failed", "util") {
		t.Fatal(key)
	}
}

// unitAlert is an alert of the design's example check about one unit.
func unitAlert(kind, incident, unit string, at time.Time) checksandalerts.Event {
	return checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "failed_systemd_units", Check: "machine_has_no_failed_systemd_unit", Finding: unit, Type: kind, AlertSeverity: "sos", At: at,
		Message: "systemd unit " + unit + " failed (systemd result: exit-code)", Notify: true}
}

// Each failed unit is its own MMHotline incident: two failed units of one
// check have two keys, and the recovery of one resolves only its own.
func TestFailedUnitsHaveTheirOwnMMHotlineIncidents(t *testing.T) {
	t.Parallel()
	d := mmhotlineNode(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	backup, certbot := incidents.ID(), incidents.ID()
	recordEvents(t, d, unitAlert("failed", backup, "backup.service", at), unitAlert("failed", certbot, "certbot.service", at))
	queued := mmhotlineQueued(t, d)
	about := "failed_systemd_units/machine_has_no_failed_systemd_unit: "
	if len(queued) != 2 || queued[0]["sender_incident_key"] != incidentKeyFor(d, about+"backup.service failed") || queued[1]["sender_incident_key"] != incidentKeyFor(d, about+"certbot.service failed") ||
		queued[0]["title"] != "web-3: "+about+"backup.service failed" || queued[0]["severity"] != "sos" {
		t.Fatalf("%+v", queued)
	}
	recovered := unitAlert("recovered", backup, "backup.service", at.Add(time.Minute))
	recovered.Message = "systemd unit backup.service is no longer failed"
	recordEvents(t, d, recovered)
	queued = mmhotlineQueued(t, d)
	if len(queued) != 3 || queued[2]["sender_incident_key"] != incidentKeyFor(d, about+"backup.service failed") || queued[2]["severity"] != "resolved-sos" || queued[2]["title"] != "web-3: "+about+"backup.service is ok again" {
		t.Fatalf("%+v", queued)
	}
	var open int
	if e := d.store.DB.QueryRow("SELECT COUNT(*) FROM fleet_incident_set WHERE state=? AND incident_key=?", store.FleetIncidentOpen, about+"certbot.service failed").Scan(&open); e != nil || open != 1 {
		t.Fatal("the recovery of one unit closed the MMHotline incident of another", open, e)
	}
}

// A joined node's alert about a failed unit goes through the hub with its
// unit, its report shows each failed unit, and the hub resolves the unit's
// MMHotline incident when the report no longer shows it.
func TestFailedUnitsThroughTheHub(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, mmhotlineDestinations(t))
	node := joinedNode(t, code, "app-2")
	incident := incidents.ID()
	now := time.Now().UTC()
	setUnits := func(units map[string]*incidents.CheckState) {
		node.mu.Lock()
		defer node.mu.Unlock()
		st := node.snapshot.Checks["app/shell"]
		st.OneIncidentForEachFinding, st.Findings = true, units
		st.Last = checksandalerts.Observation{Status: "ok", Message: "no failed systemd units", At: time.Now()}
		if len(units) > 0 {
			st.Last = checksandalerts.Observation{Status: "failed", Message: "1 systemd unit failed: backup.service", At: time.Now()}
		}
	}
	setUnits(map[string]*incidents.CheckState{"backup.service": {
		Last:            checksandalerts.Observation{Status: "failed", Message: "systemd unit backup.service failed (systemd result: exit-code)", At: now},
		FailureIncident: incidents.Track{Incident: incident, Announced: incident, Opened: now, AlertSeverity: "warn", AnnouncedAlertSeverities: []string{"warn"}},
		FailingSince:    now,
	}})
	node.report(context.Background())
	view, err := hub.hub.Node("app-2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reported := view.NodeStatusReport
	check := reported.Checks["app/shell"]
	if unit := check.Findings["backup.service"]; check.State != "failed" || unit.State != "failed" || unit.FailureIncident == nil || unit.FailureIncident.Incident != incident ||
		unit.Message != "systemd unit backup.service failed (systemd result: exit-code)" {
		t.Fatalf("%+v", check)
	}
	if a := reported.Summary.Attention; reported.Summary.ExitCode != 1 || len(a) != 1 || a[0].Finding != "backup.service" {
		t.Fatalf("%+v", reported.Summary)
	}
	// The node's alert names the unit as its finding; the hub refuses an ID
	// that no finding can have, such as a text of two lines.
	sender := fleet.HubSender{Code: code, Path: filepath.Join(node.config.StateDir, fleet.CredentialFile), Node: "app-2"}
	failed := withFleetIncidentKey(checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Finding: "backup.service", Type: "failed", AlertSeverity: "warn", At: now, Message: "systemd unit backup.service failed (systemd result: exit-code)"})
	if e := sender.Send(context.Background(), failed); e != nil {
		t.Fatal(e)
	}
	invalid := failed
	invalid.ID, invalid.Finding = incidents.ID(), "backup.service\nsecond line"
	if e := sender.Send(context.Background(), invalid); e == nil || !strings.Contains(e.Error(), "invalid alert") {
		t.Fatal("the hub accepted an alert with an invalid finding", e)
	}
	groupAt(t, hub, now)
	hub.reconcileReports(time.Now())
	queued := mmhotlineQueued(t, hub)
	if len(queued) != 1 || queued[0]["sender_incident_key"] != incidentKeyFor(hub, "app/shell: backup.service failed") || queued[0]["title"] != "app-2: app/shell: backup.service failed" {
		t.Fatalf("%+v", queued)
	}
	// Delivered late, the alert says what the node reports for the unit now.
	old := failed
	old.Node, old.At, old.Body = "app-2", now.Add(-10*time.Minute), "body"
	if late := hub.hub.Late(old, time.Now()); !strings.HasSuffix(late.Message, ". Now app-2 shows app/shell: backup.service as failing)") {
		t.Fatal(late.Message)
	}
	// The unit recovers while the hub hears no recovery alert: the next
	// report no longer shows it, and the hub resolves its key.
	setUnits(nil)
	node.report(context.Background())
	if late := hub.hub.Late(old, time.Now()); !strings.HasSuffix(late.Message, ". Now app-2 shows app/shell: backup.service as ok)") {
		t.Fatal(late.Message)
	}
	hub.reconcileReports(time.Now())
	queued = mmhotlineQueued(t, hub)
	if len(queued) != 2 || queued[1]["sender_incident_key"] != incidentKeyFor(hub, "app/shell: backup.service failed") || queued[1]["severity"] != "resolved-warn" || queued[1]["title"] != "app-2: app/shell: backup.service is ok again" {
		t.Fatalf("%+v", queued)
	}
	// A report with an ID that no finding can have is refused: an empty
	// one, one of two lines, and one that is too long.
	for _, id := range []string{"", "backup.service\nsecond line", strings.Repeat("u", checksandalerts.FindingIDLimit+1)} {
		bad := fleet.NodeStatusReport{StartID: incidents.ID(), History: incidents.ID(), MeasurementHistoryID: incidents.ID(), Seq: 1, AlertSeverityWhenThisNodeGoesSilent: "sos", Checks: map[string]fleet.ReportedCheck{"app/shell": {Findings: map[string]fleet.ReportedCheck{id: {}}}}}
		if e := bad.Validate(); e == nil || !strings.Contains(e.Error(), "finding with an invalid ID") {
			t.Fatal(strconv.Quote(id), e)
		}
	}
}

// A joined node whose hub cannot take the alert of a failed unit sends it
// straight to MMHotline, with the unit's own incident key, and sends no
// resolution when the unit recovers. Its reports carry the unit's incident
// until the hub holds it, so the hub, back, resolves the unit's key and no
// other.
func TestAJoinedNodeSendsAFailedUnitStraightToMMHotline(t *testing.T) {
	f := testhelpers.NewFakeMMHotline(t, "ok")
	hub, code, srv := startHubWith(t, mmhotlineDestinations(t, f.URL))
	hub.sendCheckIn(context.Background(), time.Now().UTC())
	listed, shown, _ := testhelpers.FakeSystemctl(t)
	p, c := daemonConfig(t)
	c.JoinFleetHub = &config.JoinFleetHub{JoinCodeFile: filepath.Join(p, "hub-join")}
	if e := os.WriteFile(c.JoinFleetHub.JoinCodeFile, []byte(code.String()+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"failed_systemd_units": {
		Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks: map[string]checksandalerts.NamedCheck{"machine_has_no_failed_systemd_unit": {Check: checksandalerts.Check{Inputs: &checksandalerts.EverySystemdUnit{FailWhen: "a_systemd_unit_has_failed", SystemdUnitsToIgnore: []string{}}}}},
	}}
	writeConfig(t, p, c)
	node, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { node.Close() })
	node.store.Node, node.link.Node = "app-1", "app-1"
	node.report(context.Background())
	if node.link.LastError != "" || node.fleetSettings == nil || node.fleetSettings.FileContentsByDestination[mmhotlineDestinationName]["mmhotline_signing_secret_file"] == "" {
		t.Fatal("the node did not join or holds no signing secret", node.link, node.fleetSettings)
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms" // below what configuration allows
	srv.Close()
	// The runs happened in the past, so the alerts are older than the wait
	// before the node sends without the hub.
	at := time.Now().UTC().Add(-time.Hour)
	observe := func(units ...string) *incidents.CheckState {
		t.Helper()
		systemdFails(t, listed, shown, units...)
		node.schedule(testhelpers.TestContext(t), at)
		node.workers.Wait()
		at = at.Add(time.Minute)
		return node.snapshot.Checks["failed_systemd_units/machine_has_no_failed_systemd_unit"]
	}
	st := observe("backup.service")
	unit := st.Findings["backup.service"]
	if unit == nil || unit.FailureIncident.Incident == "" || st.FailureIncident.Incident != "" {
		t.Fatalf("expected an incident of the unit and none of the check %+v", st)
	}
	incident := unit.FailureIncident.Incident
	runWorker(t, node)
	testhelpers.WaitFor(t, "the unit's alert to leave the hub's queue and the MMHotline queue", func() bool {
		return outboxCount(t, node, config.HubDestination) == 0 && outboxCount(t, node, mmhotlineDestinationName) == 0
	})
	about := "failed_systemd_units/machine_has_no_failed_systemd_unit: "
	alerts, checkIns, _ := f.Received()
	if len(alerts) != 1 || alerts[0]["sender_incident_key"] != incidentKeyFor(hub, about+"backup.service failed") || alerts[0]["severity"] != "warn" || alerts[0]["title"] != "app-1: "+about+"backup.service failed" ||
		!strings.HasSuffix(alerts[0]["message"], "Sent directly by the MMWatchdog node app-1 because the MMWatchdog fleet hub did not accept the alert in time.") {
		t.Fatalf("expected the unit's failure alone, with its own key %+v", alerts)
	}
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 1 || sent[0].Incident != incident || sent[0].Finding != "backup.service" {
		t.Fatal("the node does not remember the unit's incident that it sent directly", sent)
	}
	// The unit recovers while the hub is still away: the recovery leaves the
	// node's queues, and MMHotline gets no resolution and no check-in
	// from the joined node.
	if st = observe(); len(st.Findings) != 0 {
		t.Fatalf("the unit's incident stayed open %+v", st.Findings)
	}
	testhelpers.WaitFor(t, "the unit's recovery to leave the hub's queue and the MMHotline queue", func() bool {
		return outboxCount(t, node, config.HubDestination) == 0 && outboxCount(t, node, mmhotlineDestinationName) == 0
	})
	if again, checkInAgain, _ := f.Received(); len(again) != 1 || len(checkInAgain) != len(checkIns) {
		t.Fatalf("the joined node sent MMHotline more than the failure %+v, %d check-ins before and %d after", again, len(checkIns), len(checkInAgain))
	}
	// The hub returns at another address. The node's reports carry the
	// unit's incident, and the hub resolves the unit's key.
	back := httptest.NewUnstartedServer(hub.hubRoutes())
	back.TLS = &tls.Config{Certificates: []tls.Certificate{hub.hub.Cert}}
	back.StartTLS()
	t.Cleanup(back.Close)
	moved := code
	moved.Address = back.Listener.Addr().String()
	node.mu.Lock()
	node.setJoinCode(&moved)
	node.mu.Unlock()
	node.report(context.Background())
	hub.reconcileReports(time.Now())
	node.report(context.Background())
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 0 || node.link.LastError != "" {
		t.Fatal("the hub's reply did not let the node forget the unit's incident", sent, node.link.LastError)
	}
	hub.reconcileReports(time.Now())
	queued := mmhotlineQueued(t, hub)
	if len(queued) != 1 || queued[0]["sender_incident_key"] != incidentKeyFor(hub, about+"backup.service failed") || queued[0]["severity"] != "resolved-warn" ||
		queued[0]["title"] != "app-1: "+about+"backup.service is ok again" {
		t.Fatalf("expected one resolution of the unit's key %+v", queued)
	}
}

// A hub's combined reminder about one unit does not replace the queued
// reminder about another unit of the same check.
func TestRemindersOfTwoUnitsDoNotReplaceEachOther(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "1s", RemindEvery: "1h"}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	at := time.Now().UTC()
	reminder := func(unit string) checksandalerts.Event {
		// A combined reminder has no incident ID of its own.
		e := unitAlert("reminder", "", unit, at)
		e.AlertSeverity, e.Destination = "warn", "ops"
		return e
	}
	recordEvents(t, d, reminder("backup.service"), reminder("certbot.service"))
	if units := queuedUnits(t, d); !reflect.DeepEqual(units, []string{"backup.service", "certbot.service"}) {
		t.Fatal(units)
	}
	// The next reminder about the same unit replaces only that unit's.
	recordEvents(t, d, reminder("backup.service"))
	if units := queuedUnits(t, d); !reflect.DeepEqual(units, []string{"certbot.service", "backup.service"}) {
		t.Fatal(units)
	}
}

// queuedUnits lists the unit of each queued delivery in order.
func queuedUnits(t *testing.T, d *Daemon) []string {
	t.Helper()
	units := []string{}
	for _, event := range queuedEvents(t, d) {
		units = append(units, event.Finding)
	}
	return units
}

func TestLongFilesystemFindingRecoveryNamesPathAfterRestart(t *testing.T) {
	path := "/mnt/" + strings.Repeat("long-directory/", 30) + "data"
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": "36 25 8:1 / " + path + " rw,relatime - ext4 /dev/sda1 rw\n"})
	original := primitives.AskKernelAboutFilesystem
	full := true
	primitives.AskKernelAboutFilesystem = func(asked string) (primitives.FilesystemCounts, error) {
		if asked != path {
			t.Errorf("unexpected filesystem %q", asked)
		}
		available := uint64(50)
		if full {
			available = 1
		}
		return primitives.FilesystemCounts{Blocks: 100, BlocksAvailable: available, BlockBytes: 4096}, nil
	}
	t.Cleanup(func() { primitives.AskKernelAboutFilesystem = original })
	configPath, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"filesystems": {
		Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks: map[string]checksandalerts.NamedCheck{"storage": {Check: checksandalerts.Check{
			Inputs:                   &checksandalerts.EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: []string{}},
			FailWhenMeasurementAbove: map[string]float64{"space_used_percent": 90},
			AlertSeveritySettings:    checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"},
		}}},
	}}
	writeConfig(t, configPath, c)
	d, err := NewDaemon(configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	now := time.Now().UTC()
	d.schedule(testhelpers.TestContext(t), now)
	d.workers.Wait()
	state := d.snapshot.Checks["filesystems/storage"]
	if len(state.Findings) != 1 {
		t.Fatal(state)
	}
	findingID := state.Last.Findings[0].ID
	if !strings.HasPrefix(findingID, "filesystem_path_sha256:") || !strings.Contains(state.Findings[findingID].FindingRecoveryMessage, path) {
		t.Fatal(state)
	}
	d.Close()
	d, err = NewDaemon(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.snapshot.Checks["filesystems/storage"].Findings[findingID]; got == nil || !strings.Contains(got.FindingRecoveryMessage, path) {
		t.Fatal(got)
	}
	full = false
	d.schedule(testhelpers.TestContext(t), now.Add(2*time.Minute))
	d.workers.Wait()
	events, err := d.store.CheckGroupEvents("filesystems", 100)
	if err != nil {
		t.Fatal(err)
	}
	failed, recovered := false, false
	for _, event := range events {
		if event.Finding != findingID {
			continue
		}
		if event.Type == "failed" || event.Type == "recovered" {
			if !strings.Contains(event.Message, path) {
				t.Fatal(event)
			}
			failed = failed || event.Type == "failed"
			recovered = recovered || event.Type == "recovered"
		}
	}
	if !failed || !recovered {
		t.Fatal(eventSummary(events))
	}
}
