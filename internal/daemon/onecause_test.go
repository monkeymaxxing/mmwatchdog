package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// The three checks of the example in docs/changing-mmwatchdog/design/checks.md under "One
// cause, one alert": the health request of myapp names the application's
// own unit check and the database's unit check as its causes.
const (
	postgresCheck    = "postgres/postgres_service_is_running"
	myappUnitCheck   = "myapp/myapp_service_is_running"
	healthCheck      = "myapp/myapp_answers_health_request"
	quietBecausePost = "; no alert while " + postgresCheck + " fails"
	keptQuietLine    = "no alerts and no recovery actions for these checks while this check fails: " + healthCheck
)

// oneCause is a daemon with the example's two check groups, a destination
// that reminds every 10 minutes, and a stand-in for systemctl that counts
// the restarts of myapp.service. start is the example's 12:00.
type oneCause struct {
	t                *testing.T
	d                *Daemon
	path             string
	config           *config.Config
	start            time.Time
	state, restarted string
}

func newOneCause(t *testing.T) *oneCause {
	t.Helper()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"team_chat": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:1"}, RemindEvery: "10m", TimeLimitPerDeliveryAttempt: "1s"}}
	timing := checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "30s"}
	sos := checksandalerts.AlertSeveritySettings{AlertSeverity: "sos"}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"postgres": {Timing: timing, Checks: map[string]checksandalerts.NamedCheck{
			"postgres_service_is_running": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "postgresql.service", FailWhen: "the_unit_is_not_active"}, AlertSeveritySettings: sos}},
		}},
		"myapp": {Timing: timing, Checks: map[string]checksandalerts.NamedCheck{
			"myapp_service_is_running": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "myapp.service", FailWhen: "the_unit_is_not_active"}, AlertSeveritySettings: sos}},
			"myapp_answers_health_request": {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:1/health"},
				AlertSeveritySettings:                         checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: "10m"},
				NoAlertsOrRecoveryActionsWhileTheseChecksFail: []string{myappUnitCheck, postgresCheck}}},
		}, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "myapp.service", RestartAfterFailingFor: "1m",
			RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"myapp_service_is_running", "myapp_answers_health_request"}, Attempts: "3", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"}}},
	}
	writeConfig(t, p, c)
	dir := t.TempDir()
	o := &oneCause{t: t, path: p, config: c, start: time.Now().UTC().Add(-time.Hour), state: filepath.Join(dir, "state"), restarted: filepath.Join(dir, "restarted")}
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=%s\\nResult=success\\n' \"$(cat " + testhelpers.ShellQuote(o.state) + ")\"; else echo restart >> " + testhelpers.ShellQuote(o.restarted) + "; fi\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	os.WriteFile(o.state, []byte("active"), 0600)
	testhelpers.PutProgramsFirst(t, dir)
	o.startDaemon()
	t.Cleanup(func() { o.d.Close() })
	// Every check has run and is healthy before the example begins.
	for _, key := range []string{postgresCheck, myappUnitCheck, healthCheck} {
		acceptRun(t, o.d, key, "ok", o.start.Add(-time.Minute))
	}
	return o
}

// acceptRun applies one run of the check key, as the result of a scheduled
// run is applied, and commits its events.
func acceptRun(t *testing.T, d *Daemon, key, status string, at time.Time) []checksandalerts.Event {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	events := d.accept(testhelpers.TestContext(t), key, d.checks[key], checksandalerts.Observation{Status: status, Message: status, At: at})
	if err := d.record(events); err != nil {
		t.Fatal(err)
	}
	return events
}

func (o *oneCause) startDaemon() {
	o.t.Helper()
	d, err := NewDaemon(o.path)
	if err != nil {
		o.t.Fatal(err)
	}
	o.d = d
}

// run applies one run of the check key at after past the start, as the
// result of a scheduled run is applied (accept), and lets a restart that is
// due finish. A restart reads the clock for the age of the run that calls
// for it, so the run then counts as made now: by the clock every waiting
// time is long over, and what holds a restart back is only the rule under
// test.
func (o *oneCause) run(key, status string, after time.Duration) []checksandalerts.Event {
	o.t.Helper()
	d := o.d
	events := acceptRun(o.t, d, key, status, o.start.Add(after))
	d.workers.Wait()
	d.mu.Lock()
	now := time.Now().UTC()
	d.snapshot.Checks[key].Last.At = now
	d.startRecovery(testhelpers.TestContext(o.t), now)
	d.mu.Unlock()
	d.workers.Wait()
	return events
}

// restartDue asks which check calls for the restart of myapp.service when
// the latest run of every check counts as made now.
func (o *oneCause) restartDue() string {
	o.d.mu.Lock()
	defer o.d.mu.Unlock()
	now := time.Now().UTC()
	for _, st := range o.d.snapshot.Checks {
		st.Last.At = now
	}
	return o.d.recoveryDue("myapp.service", now, "")
}

func (o *oneCause) restarts() int {
	b, _ := os.ReadFile(o.restarted)
	return strings.Count(string(b), "restart")
}

func (o *oneCause) checkState(key string) *incidents.CheckState { return o.d.snapshot.Checks[key] }

// tick runs the raise and the reminder step of the scheduler tick at after
// past the start and commits what they made.
func (o *oneCause) tick(after time.Duration) []checksandalerts.Event {
	o.t.Helper()
	o.d.mu.Lock()
	defer o.d.mu.Unlock()
	events := append(o.d.raiseOpenIncidents(o.start.Add(after)), o.d.remind(o.start.Add(after))...)
	if err := o.d.record(events); err != nil {
		o.t.Fatal(err)
	}
	return events
}

// sent lists the alerts that were queued for the destination, as
// "CHECK_GROUP/CHECK type severity".
func (o *oneCause) sent() []string {
	o.t.Helper()
	out := []string{}
	for _, e := range queuedEvents(o.t, o.d) {
		out = append(out, e.CheckGroup+"/"+e.Check+" "+e.Type+" "+e.AlertSeverity)
	}
	return out
}

func (o *oneCause) status(key string) incidents.CheckStatus {
	o.t.Helper()
	o.d.mu.Lock()
	defer o.d.mu.Unlock()
	checks, _, _ := o.d.status("", time.Now().UTC(), nil)
	return checks[key]
}

// postgresRecovers runs the database check healthy at 12:20 and at 12:20:30,
// which ends its failing time, and returns the events of the second run.
func (o *oneCause) postgresRecovers() []checksandalerts.Event {
	o.t.Helper()
	if events := o.run(postgresCheck, "ok", 20*time.Minute); len(events) != 0 {
		o.t.Fatal(eventSummary(events))
	}
	return o.run(postgresCheck, "ok", 20*time.Minute+30*time.Second)
}

// The example of the design: the database stops, and the health request of
// the application fails with it. MMWatchdog sends one alert, for the database, and
// does not restart the application. When the database is healthy again, the
// next run of the health request decides.
// A cause whose alerts reach no destination cannot be the reason to keep a
// check quiet. The hub can change the destinations after the load-time check.
func TestACauseThatReachesNoDestinationKeepsNoCheckQuiet(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	o.run(healthCheck, "failed", 10*time.Second)
	o.d.mu.Lock()
	quiet := o.d.quietWhile(healthCheck)
	// The destination now takes only warn alerts: the database check's sos
	// alerts reach nobody.
	o.d.config.Alerts.Destinations = map[string]checksandalerts.Destination{"team_chat": {AlertSeverities: []string{"warn"}, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:1"}, RemindEvery: "10m", TimeLimitPerDeliveryAttempt: "1s"}}
	o.d.mu.Unlock()
	if quiet != postgresCheck {
		t.Fatalf("the health request is kept quiet by %q while the cause reaches a destination", quiet)
	}
	o.run(healthCheck, "failed", 20*time.Second)
	if got := o.checkState(healthCheck).QuietNowWhileCheckFails; got != "" {
		t.Fatalf("the health request is still kept quiet by %q after the cause's destinations are gone", got)
	}
}

func TestOneCauseOneAlert(t *testing.T) {
	t.Parallel()
	for _, stillFails := range []bool{true, false} {
		name := "the health request is healthy after the quiet time"
		if stillFails {
			name = "the health request still fails after the quiet time"
		}
		t.Run(name, func(t *testing.T) {
			o := newOneCause(t)
			// 12:00: the database crashes, and the health request starts to fail.
			if events := append(o.run(postgresCheck, "failed", 0), o.run(healthCheck, "failed", 10*time.Second)...); len(events) != 0 {
				t.Fatal(eventSummary(events))
			}
			if got := o.checkState(healthCheck).QuietNowWhileCheckFails; got != postgresCheck {
				t.Fatalf("the health request is kept quiet by %q", got)
			}
			// 12:01: the health request has failed for 1 minute, which would
			// make the restart due. MMWatchdog does not restart and uses no attempt.
			o.run(healthCheck, "failed", 70*time.Second)
			if attempts := o.d.snapshot.RecoveryAttemptsByTarget["myapp.service"]; o.restarts() != 0 || (attempts != nil && attempts.Attempts != 0) {
				t.Fatal("MMWatchdog restarted the application while the database was the cause", o.restarts(), attempts)
			}
			// 12:02: one alert, for the database. It names the quiet check.
			events := o.run(postgresCheck, "failed", 2*time.Minute)
			if len(events) != 1 || events[0].Type != "failed" || !events[0].Notify ||
				!reflect.DeepEqual(events[0].NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails, []string{healthCheck}) || events[0].NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails != 1 {
				t.Fatalf("%+v", events)
			}
			// The incident of the health request opens and is only recorded.
			events = o.run(healthCheck, "failed", 2*time.Minute+10*time.Second)
			if len(events) != 1 || events[0].Type != "failed" || events[0].Notify || events[0].Message != "failed"+quietBecausePost {
				t.Fatalf("%+v", events)
			}
			incident := o.checkState(healthCheck).FailureIncident.Incident
			queued := queuedEvents(t, o.d)
			if len(queued) != 1 || queued[0].Check != "postgres_service_is_running" || !strings.Contains(queued[0].Body, "\n"+keptQuietLine) {
				t.Fatalf("%+v", queued)
			}
			// Inspection keeps showing the state of the quiet check, and says
			// what MMWatchdog holds back.
			status := o.status(healthCheck)
			if want := healthCheck + " is failing; no alert and no recovery action while " + postgresCheck + " fails"; status.State != "failed" || status.KeptQuietSentence() != want {
				t.Fatalf("%s %q", status.State, status.KeptQuietSentence())
			}
			if got := fleet.ReportedCheckOf(status).Message; got != "failed; no alert and no recovery action while "+postgresCheck+" fails" {
				t.Fatalf("%q", got)
			}
			if got := o.status(postgresCheck); got.KeptQuietSentence() != "" || !reflect.DeepEqual(got.KeepsTheseChecksQuiet, []string{healthCheck}) {
				t.Fatalf("%+v", got)
			}
			// 12:13: the raise of the health request to sos is due, and so
			// is a reminder of the database's alert. The raise is not sent:
			// the incident that was never announced takes the severity. The
			// reminder of the cause names the quiet check again.
			events = o.tick(13 * time.Minute)
			if len(events) != 1 || events[0].Type != "reminder" || events[0].Check != "postgres_service_is_running" || o.checkState(healthCheck).FailureIncident.AlertSeverity != "sos" {
				t.Fatalf("%v %+v", eventSummary(events), o.checkState(healthCheck).FailureIncident)
			}
			if queued = queuedEvents(t, o.d); len(queued) != 2 || !strings.Contains(queued[1].Body, "\n"+keptQuietLine) {
				t.Fatalf("%+v", queued)
			}
			// 12:20:30: the database has been healthy for 30 seconds. Its
			// failing time ends. The health request stays quiet until its own
			// next run: its latest run is from the quiet time.
			if events = o.postgresRecovers(); len(events) != 1 || events[0].Type != "recovered" || !events[0].Notify {
				t.Fatalf("%+v", events)
			}
			if due := o.restartDue(); due != "" || o.restarts() != 0 {
				t.Fatal("a run from the quiet time decided after it", due, o.restarts())
			}
			// Status no longer says that the database fails.
			status = o.status(healthCheck)
			if want := healthCheck + " is failing; no alert and no recovery action until its next run, because " + postgresCheck + " was failing"; status.QuietNowWhileCheckFails != "" || status.QuietUntilNextRunAfterCheckFailed != postgresCheck || status.KeptQuietSentence() != want {
				t.Fatalf("%q %q %q", status.QuietNowWhileCheckFails, status.QuietUntilNextRunAfterCheckFailed, status.KeptQuietSentence())
			}
			if events = o.tick(20*time.Minute + 40*time.Second); len(events) != 0 {
				t.Fatal(eventSummary(events))
			}
			if !stillFails {
				// The next run is healthy: MMWatchdog sends nothing about the
				// application, and the incident that nobody heard of closes
				// in the records only.
				events = append(o.run(healthCheck, "ok", 20*time.Minute+45*time.Second), o.run(healthCheck, "ok", 21*time.Minute+15*time.Second)...)
				if len(events) != 1 || events[0].Type != "recovered" || events[0].Notify || events[0].Incident != incident || events[0].Message != "ok" {
					t.Fatalf("%+v", events)
				}
				want := []string{postgresCheck + " failed sos", postgresCheck + " reminder sos", postgresCheck + " recovered sos"}
				if got := o.sent(); !reflect.DeepEqual(got, want) || o.restarts() != 0 {
					t.Fatalf("%v, %d restarts", got, o.restarts())
				}
				return
			}
			// The next run still fails: the alert that became due in the
			// quiet time goes out now, at the severity that the incident
			// reached, and MMWatchdog restarts the application with every attempt
			// available.
			events = o.run(healthCheck, "failed", 20*time.Minute+45*time.Second)
			if len(events) != 1 || events[0].Type != "failed" || !events[0].Notify || events[0].Incident != incident || events[0].AlertSeverity != "sos" || events[0].Message != "failed" {
				t.Fatalf("%+v", events)
			}
			if attempts := o.d.snapshot.RecoveryAttemptsByTarget["myapp.service"]; o.restarts() != 1 || attempts == nil || attempts.Attempts != 1 {
				t.Fatal("the application was not restarted once after the quiet time", o.restarts(), attempts)
			}
			if status := o.status(healthCheck); status.QuietNowWhileCheckFails != "" || status.KeptQuietSentence() != "" {
				t.Fatalf("%+v", status)
			}
			want := []string{postgresCheck + " failed sos", postgresCheck + " reminder sos", postgresCheck + " recovered sos", healthCheck + " failed sos"}
			if got := o.sent(); !reflect.DeepEqual(got, want) {
				t.Fatal(got)
			}
		})
	}
}

// The setting covers only the check that has it: another check that
// triggers the same restart still does, also while the quiet check fails.
func TestAQuietCheckLeavesTheOtherChecksOfARestartAlone(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	o.run(healthCheck, "failed", 10*time.Second)
	if o.restarts() != 0 {
		t.Fatal("the quiet check restarted the unit")
	}
	// systemd reports the unit as failed, so its own check calls for the
	// restart. The health request names that check too, and stays quiet.
	os.WriteFile(o.state, []byte("failed"), 0600)
	o.run(myappUnitCheck, "failed", 20*time.Second)
	if o.restarts() != 1 {
		t.Fatal("the unit's own check did not restart it", o.restarts())
	}
	o.run(healthCheck, "failed", 40*time.Second)
	if got := o.checkState(healthCheck).QuietNowWhileCheckFails; got != myappUnitCheck {
		t.Fatalf("the health request is kept quiet by %q, and it names %s first", got, myappUnitCheck)
	}
}

// An incident that was announced before the quiet time is held like every
// alert of the quiet check: no reminder, no raise, and no recovery alert
// while the cause fails. Nothing is sent later for the quiet time. After it,
// the recovery of the announced incident goes out with the first healthy
// run, and the reminders of an incident that is still open count from the
// end of the quiet time.
func TestQuietTimeHoldsTheAlertsOfAnAnnouncedIncident(t *testing.T) {
	t.Parallel()
	for _, recoversInTheQuietTime := range []bool{true, false} {
		name := "the check keeps failing"
		if recoversInTheQuietTime {
			name = "the check recovers in the quiet time"
		}
		t.Run(name, func(t *testing.T) {
			o := newOneCause(t)
			// Nothing keeps the health request quiet at 12:00: MMWatchdog restarts
			// the unit, and observes the unit's checks again after it.
			o.run(healthCheck, "failed", 0)
			o.run(myappUnitCheck, "ok", 10*time.Second)
			if events := o.run(healthCheck, "failed", 2*time.Minute); len(events) != 1 || !events[0].Notify || o.restarts() != 1 {
				t.Fatalf("%+v, %d restarts", events, o.restarts())
			}
			// 12:05: the database starts to fail. From this moment MMWatchdog holds
			// the reminder and the raise of the health request, before the
			// health request has run again.
			o.run(postgresCheck, "failed", 5*time.Minute)
			o.run(postgresCheck, "failed", 7*time.Minute)
			if events := o.tick(13 * time.Minute); len(events) != 0 || o.checkState(healthCheck).FailureIncident.AlertSeverity != "warn" {
				t.Fatal("MMWatchdog sent a reminder or a raise for a quiet check", eventSummary(events))
			}
			after := 20*time.Minute + 45*time.Second
			if recoversInTheQuietTime {
				o.run(healthCheck, "ok", 14*time.Minute)
				events := o.run(healthCheck, "ok", 14*time.Minute+30*time.Second)
				if len(events) != 1 || events[0].Type != "recovered" || events[0].Notify || events[0].Message != "ok"+quietBecausePost {
					t.Fatalf("%+v", events)
				}
				// The destinations still show the incident as open, and
				// status says why.
				if got, want := o.status(healthCheck).KeptQuietSentence(), healthCheck+" is ok again; no recovery alert while "+postgresCheck+" fails"; got != want {
					t.Fatalf("%q", got)
				}
				o.postgresRecovers()
				// The incident was announced, so its recovery is too.
				if events = o.run(healthCheck, "ok", after); len(events) != 1 || events[0].Type != "recovered" || !events[0].Notify {
					t.Fatalf("%+v", events)
				}
				want := []string{healthCheck + " failed warn", postgresCheck + " failed sos", postgresCheck + " recovered sos", healthCheck + " recovered warn"}
				if got := o.sent(); !reflect.DeepEqual(got, want) {
					t.Fatal(got)
				}
				return
			}
			o.run(healthCheck, "failed", 14*time.Minute)
			o.postgresRecovers()
			// The first run after the quiet time still fails. The incident
			// is announced already, so the run sends nothing; the raise that
			// was held goes out at the next tick, and the reminders count
			// from this run.
			if events := o.run(healthCheck, "failed", after); len(events) != 0 || !o.checkState(healthCheck).FailureIncident.RemindFrom.Equal(o.start.Add(after)) {
				t.Fatal(eventSummary(events), o.checkState(healthCheck).FailureIncident.RemindFrom)
			}
			if events := o.tick(21 * time.Minute); len(events) != 1 || events[0].Type != checksandalerts.FailureIncidentEvents.Raised || events[0].AlertSeverity != "sos" {
				t.Fatal(eventSummary(events))
			}
			// The raise is the latest alert, so the reminders count from it.
			if events := o.tick(31*time.Minute - time.Second); len(events) != 0 {
				t.Fatal(eventSummary(events))
			}
			if events := o.tick(31 * time.Minute); len(events) != 1 || events[0].Type != "reminder" || events[0].Check != "myapp_answers_health_request" {
				t.Fatal(eventSummary(events))
			}
		})
	}
}

// A quiet check that MMWatchdog cannot run sends no cannot-check alert either. After
// the quiet time, the first run that MMWatchdog still cannot make sends it.
func TestQuietTimeHoldsACannotCheckAlert(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	o.run(healthCheck, "unknown", 10*time.Second)
	events := o.run(healthCheck, "unknown", 2*time.Minute+10*time.Second)
	if len(events) != 1 || events[0].Type != "cannot_check" || events[0].Notify || events[0].Message != "unknown"+quietBecausePost {
		t.Fatalf("%+v", events)
	}
	if got, want := o.status(healthCheck).KeptQuietSentence(), "MMWatchdog cannot check "+healthCheck+"; no alert while "+postgresCheck+" fails"; got != want {
		t.Fatalf("%q", got)
	}
	o.postgresRecovers()
	if events = o.run(healthCheck, "unknown", 20*time.Minute+45*time.Second); len(events) != 1 || events[0].Type != "cannot_check" || !events[0].Notify {
		t.Fatalf("%+v", events)
	}
	if got := o.sent(); !reflect.DeepEqual(got, []string{healthCheck + " cannot_check warn"}) {
		t.Fatal(got)
	}
}

// A daemon start ends the failing time of a check whose incident has not
// opened: MMWatchdog did not watch it for a time. A check that such a check kept
// quiet stays quiet until MMWatchdog has observed every check that it names again,
// so the start sends no alert for it and restarts nothing.
func TestQuietTimeLastsUntilTheCausesAreObservedAgain(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	o.run(healthCheck, "failed", 0)
	// The incident of the health request opens, quietly. The database check
	// has not run since 12:00, so its incident has not opened.
	if events := o.run(healthCheck, "failed", 2*time.Minute); len(events) != 1 || events[0].Notify {
		t.Fatalf("%+v", events)
	}
	if err := o.d.Close(); err != nil {
		t.Fatal(err)
	}
	o.startDaemon()
	if failing, quiet := o.checkState(postgresCheck).FailingSince, o.checkState(healthCheck).QuietNowWhileCheckFails; !failing.IsZero() || quiet != postgresCheck {
		t.Fatal("after the start", failing, quiet)
	}
	quiet := func(what string, events []checksandalerts.Event) {
		t.Helper()
		if len(events) != 0 || o.restarts() != 0 || o.checkState(healthCheck).QuietNowWhileCheckFails != postgresCheck {
			t.Fatal(what, eventSummary(events), o.restarts(), o.checkState(healthCheck).QuietNowWhileCheckFails)
		}
	}
	// The health request runs first after the start.
	quiet("the start ended the quiet time before MMWatchdog had observed a cause again", o.run(healthCheck, "failed", 3*time.Minute))
	// The database check runs and fails: the quiet time goes on.
	o.run(postgresCheck, "failed", 3*time.Minute+10*time.Second)
	quiet("the database check fails again", o.run(healthCheck, "failed", 3*time.Minute+30*time.Second))
	// The database is healthy again. The health request names the unit
	// check of the application too, and MMWatchdog has not observed that check
	// since the start.
	o.postgresRecovers()
	quiet("MMWatchdog has not observed the other named check since the start", o.run(healthCheck, "failed", 20*time.Minute+45*time.Second))
	// Now MMWatchdog has observed both: the next run of the health request decides.
	o.run(myappUnitCheck, "ok", 21*time.Minute)
	if events := o.run(healthCheck, "failed", 21*time.Minute+15*time.Second); len(events) != 1 || events[0].Type != "failed" || !events[0].Notify || o.restarts() != 1 {
		t.Fatalf("%+v, %d restarts", events, o.restarts())
	}
}

// lateRun makes the scheduler start the next run of the check key late, at
// after past the start, and leaves that run in flight: its result is not
// applied. previousStart is when the run before it started.
func (o *oneCause) lateRun(key string, previousStart, after time.Duration) {
	o.t.Helper()
	d, at := o.d, o.start.Add(after)
	// The run waits for its gate and ends without a result.
	ctx, cancel := context.WithCancel(testhelpers.TestContext(o.t))
	cancel()
	d.mu.Lock()
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	d.gates[key] = gate
	for _, rc := range d.checks {
		rc.next = at.Add(time.Hour)
	}
	d.checks[key].next, d.checks[key].scheduled = at.Add(-20*time.Second), o.start.Add(previousStart)
	d.mu.Unlock()
	d.schedule(ctx, at)
	d.workers.Wait()
	d.mu.Lock()
	delete(d.gates, key)
	d.mu.Unlock()
}

// A run of the named check that starts late ends its failing time while its
// incident has not opened: MMWatchdog did not watch the check for a time. The check
// still counts as failing until that run has a result, so the quiet check
// neither alerts nor restarts in between.
func TestALateRunOfTheCauseKeepsTheCheckQuiet(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	o.run(healthCheck, "failed", 10*time.Second)
	o.run(postgresCheck, "failed", 30*time.Second)
	o.run(postgresCheck, "failed", 60*time.Second)
	o.run(healthCheck, "failed", 70*time.Second)
	o.run(postgresCheck, "failed", 90*time.Second)
	// The run of 12:02:00 starts at 12:02:20, more than one and a half
	// intervals after the run before it, and is still running.
	o.lateRun(postgresCheck, 90*time.Second, 140*time.Second)
	if cause := o.checkState(postgresCheck); cause.FailingSince.IsZero() || !cause.CountsAsFailing() {
		t.Fatalf("the late start: %+v", cause)
	}
	if events := o.run(healthCheck, "failed", 145*time.Second); len(events) != 1 || events[0].Notify || o.restarts() != 0 {
		t.Fatalf("%+v, %d restarts", events, o.restarts())
	}
	// The late run fails: the failing time of the database check continues,
	// and the quiet time goes on.
	o.run(postgresCheck, "failed", 150*time.Second)
	if events := o.run(healthCheck, "failed", 175*time.Second); len(events) != 0 || o.restarts() != 0 {
		t.Fatal(eventSummary(events), o.restarts())
	}
}

// A run of the named check that MMWatchdog could not make is an unknown
// observation. It keeps the cause in its failing time, so dependent checks
// remain quiet until the cause has recovered.
func TestAMissedRunOfTheCauseKeepsTheQuietTime(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	o.run(healthCheck, "failed", 10*time.Second)
	o.run(healthCheck, "failed", 70*time.Second)
	if o.restarts() != 0 {
		t.Fatal("the quiet check restarted the unit")
	}
	at := o.start.Add(80 * time.Second)
	o.d.mu.Lock()
	// The database check last ran at 12:00, and that run has not finished.
	o.d.snapshot.Checks[postgresCheck].Last.At = o.start
	for key, rc := range o.d.checks {
		rc.next = at.Add(time.Hour)
		if key == postgresCheck {
			rc.running, rc.next = true, at.Add(-time.Minute)
		}
	}
	o.d.mu.Unlock()
	o.d.schedule(testhelpers.TestContext(t), at)
	o.d.workers.Wait()
	if cause := o.checkState(postgresCheck); cause.Last.Status != "unknown" || !cause.CountsAsFailing() {
		t.Fatalf("%+v", cause)
	}
	o.run(healthCheck, "failed", 85*time.Second)
	if o.restarts() != 0 || o.checkState(healthCheck).QuietNowWhileCheckFails == "" {
		t.Fatal(o.restarts(), o.checkState(healthCheck).QuietNowWhileCheckFails)
	}
}

// An incident that was announced before the quiet time resolves in it, and
// a second incident opens in it. After the quiet time the recovery of the
// first goes out, with the reason why it is late, and then the failure.
func TestTheRecoveryThatWaitedForTheQuietTimeSaysSo(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(healthCheck, "failed", 0)
	o.run(myappUnitCheck, "ok", 10*time.Second)
	o.run(healthCheck, "failed", 2*time.Minute)
	first := o.checkState(healthCheck).FailureIncident.Incident
	o.run(postgresCheck, "failed", 5*time.Minute)
	o.run(healthCheck, "ok", 6*time.Minute)
	o.run(healthCheck, "ok", 6*time.Minute+30*time.Second)
	o.run(healthCheck, "failed", 7*time.Minute)
	o.run(healthCheck, "failed", 9*time.Minute+10*time.Second)
	o.postgresRecovers()
	events := o.run(healthCheck, "failed", 20*time.Minute+45*time.Second)
	if len(events) != 2 || events[0].Type != "recovered" || events[0].Incident != first || !events[0].Notify || events[0].Message != "previous incident resolved while MMWatchdog sent no alerts for this check" ||
		events[1].Type != "failed" || !events[1].Notify || events[1].Incident == first {
		t.Fatalf("%+v", events)
	}
}

// Maintenance of either check group inside the quiet time changes nothing
// about it: no alert and no restart for the quiet check.
func TestMaintenanceInsideAQuietTime(t *testing.T) {
	t.Parallel()
	quiet := func(t *testing.T, o *oneCause, what string, events []checksandalerts.Event) {
		t.Helper()
		for _, e := range events {
			if e.Notify {
				t.Fatalf("%s: MMWatchdog sent %s for %s/%s", what, e.Type, e.CheckGroup, e.Check)
			}
		}
		if o.restarts() != 0 {
			t.Fatalf("%s: MMWatchdog restarted myapp.service", what)
		}
	}
	t.Run("of the named check's check group", func(t *testing.T) {
		o := newOneCause(t)
		o.run(postgresCheck, "failed", 0)
		o.run(healthCheck, "failed", 10*time.Second)
		o.run(healthCheck, "failed", 70*time.Second)
		// The window starts before the incident of the database check has
		// opened, which ends that check's failing time.
		setMaintenance(t, o.d, "postgres", "1h")
		quiet(t, o, "the first run after the window started", o.run(healthCheck, "failed", 100*time.Second))
		o.run(postgresCheck, "failed", 110*time.Second)
		quiet(t, o, "the incident opens in the quiet time", o.run(healthCheck, "failed", 140*time.Second))
		o.run(postgresCheck, "failed", 4*time.Minute)
		setMaintenance(t, o.d, "postgres", "off")
		quiet(t, o, "the first run after the window ended", o.run(healthCheck, "failed", 5*time.Minute))
		// The alert of the database check goes out after its maintenance
		// and names the quiet check.
		events := o.run(postgresCheck, "failed", 5*time.Minute+10*time.Second)
		if len(events) != 1 || events[0].Type != "failed" || !events[0].Notify || !reflect.DeepEqual(events[0].NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails, []string{healthCheck}) {
			t.Fatalf("%+v", events)
		}
	})
	t.Run("of the quiet check's check group", func(t *testing.T) {
		o := newOneCause(t)
		o.run(postgresCheck, "failed", 0)
		o.run(postgresCheck, "failed", 2*time.Minute)
		o.run(healthCheck, "failed", 10*time.Second)
		o.run(healthCheck, "failed", 130*time.Second)
		incident := o.checkState(healthCheck).FailureIncident.Incident
		setMaintenance(t, o.d, "myapp", "1h")
		quiet(t, o, "in the window", o.run(healthCheck, "failed", 3*time.Minute))
		setMaintenance(t, o.d, "myapp", "off")
		o.run(myappUnitCheck, "ok", 3*time.Minute+30*time.Second)
		quiet(t, o, "the first run after the window ended", o.run(healthCheck, "failed", 4*time.Minute))
		if got := o.checkState(healthCheck).FailureIncident; got.Incident != incident || got.Announced != "" {
			t.Fatalf("%+v", got)
		}
		o.postgresRecovers()
		events := o.run(healthCheck, "failed", 20*time.Minute+45*time.Second)
		if len(events) != 1 || !events[0].Notify || events[0].Incident != incident || o.restarts() != 1 {
			t.Fatalf("%+v, %d restarts", events, o.restarts())
		}
	})
}

// A reload that changes the named check makes it a new check, which MMWatchdog has
// not observed yet: the quiet time goes on until it has. A reload that
// changes the quiet check retires its incident, which nobody heard of,
// without an alert.
func TestAReloadInsideAQuietTime(t *testing.T) {
	t.Parallel()
	t.Run("changes the named check", func(t *testing.T) {
		o := newOneCause(t)
		o.run(postgresCheck, "failed", 0)
		o.run(healthCheck, "failed", 10*time.Second)
		o.run(healthCheck, "failed", 130*time.Second)
		postgres := o.config.CheckGroups["postgres"]
		check := postgres.Checks["postgres_service_is_running"]
		check.Inputs = &checksandalerts.SystemdUnitState{SystemdUnit: "postgres.service", FailWhen: "the_unit_is_not_active"}
		postgres.Checks["postgres_service_is_running"] = check
		writeConfig(t, o.path, o.config)
		if _, err := o.d.reload(); err != nil {
			t.Fatal(err)
		}
		if events := o.run(healthCheck, "failed", 3*time.Minute); len(events) != 0 || o.restarts() != 0 {
			t.Fatal("before MMWatchdog has observed the changed check", eventSummary(events), o.restarts())
		}
		o.run(postgresCheck, "failed", 3*time.Minute+10*time.Second)
		if events := o.run(healthCheck, "failed", 3*time.Minute+30*time.Second); len(events) != 0 || o.restarts() != 0 {
			t.Fatal("the changed check fails", eventSummary(events), o.restarts())
		}
	})
	t.Run("changes the quiet check", func(t *testing.T) {
		o := newOneCause(t)
		o.run(postgresCheck, "failed", 0)
		o.run(healthCheck, "failed", 10*time.Second)
		o.run(healthCheck, "failed", 130*time.Second)
		myapp := o.config.CheckGroups["myapp"]
		health := myapp.Checks["myapp_answers_health_request"]
		health.Inputs = &checksandalerts.HTTP{URL: "http://127.0.0.1:1/ready"}
		myapp.Checks["myapp_answers_health_request"] = health
		writeConfig(t, o.path, o.config)
		if _, err := o.d.reload(); err != nil {
			t.Fatal(err)
		}
		if got := o.sent(); len(got) != 0 {
			t.Fatal(got)
		}
		if due := o.restartDue(); due != "" {
			t.Fatal("a restart is due because of", due)
		}
		if events := append(o.run(healthCheck, "failed", 3*time.Minute), o.run(healthCheck, "failed", 5*time.Minute+10*time.Second)...); len(events) != 1 || events[0].Notify || o.restarts() != 0 {
			t.Fatalf("%+v, %d restarts", events, o.restarts())
		}
	})
}

// Two named checks fail and are healthy again at different times: the quiet
// time lasts until the last of them is.
func TestTwoCausesThatAreHealthyAgainAtDifferentTimes(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	o.run(healthCheck, "failed", 10*time.Second)
	o.run(healthCheck, "failed", 130*time.Second)
	o.run(myappUnitCheck, "failed", 5*time.Minute)
	o.postgresRecovers()
	if events := o.run(healthCheck, "failed", 20*time.Minute+45*time.Second); len(events) != 0 || o.restarts() != 0 || o.checkState(healthCheck).QuietNowWhileCheckFails != myappUnitCheck {
		t.Fatal(eventSummary(events), o.restarts(), o.checkState(healthCheck).QuietNowWhileCheckFails)
	}
	o.run(myappUnitCheck, "ok", 25*time.Minute)
	o.run(myappUnitCheck, "ok", 25*time.Minute+30*time.Second)
	if events := o.run(healthCheck, "failed", 25*time.Minute+45*time.Second); len(events) != 1 || !events[0].Notify || events[0].Type != "failed" || o.restarts() != 1 {
		t.Fatalf("%+v, %d restarts", events, o.restarts())
	}
}

// A chain: a names b, and b names c. While all three fail, only c alerts.
// Its alert names b, the check that names it; a is quiet because b fails.
// When c is healthy again, the next run of b decides, and then that of a.
func TestAChainOfCauses(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"team_chat": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:1"}, TimeLimitPerDeliveryAttempt: "1s"}}
	check := func(named ...string) checksandalerts.NamedCheck {
		return checksandalerts.NamedCheck{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true", SuccessExitCodes: []int{0}}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}, NoAlertsOrRecoveryActionsWhileTheseChecksFail: named}}
	}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"chain": {Timing: checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "30s"}, Checks: map[string]checksandalerts.NamedCheck{"a": check("chain/b"), "b": check("chain/c"), "c": check()}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	start := time.Now().UTC().Add(-time.Hour)
	run := func(check, status string, after time.Duration) []checksandalerts.Event {
		t.Helper()
		return acceptRun(t, d, "chain/"+check, status, start.Add(after))
	}
	for _, check := range []string{"c", "b", "a"} {
		run(check, "failed", 0)
	}
	for _, check := range []string{"c", "b", "a"} {
		run(check, "failed", 2*time.Minute)
	}
	queued := queuedEvents(t, d)
	if len(queued) != 1 || queued[0].Check != "c" || !strings.HasSuffix(queued[0].Body, "\nno alerts and no recovery actions for these checks while this check fails: chain/b") {
		t.Fatalf("%+v", queued)
	}
	d.mu.Lock()
	checks, _, _ := d.status("", time.Now().UTC(), nil)
	d.mu.Unlock()
	if a, b := checks["chain/a"].KeptQuietSentence(), checks["chain/b"].KeptQuietSentence(); a != "chain/a is failing; no alert and no recovery action while chain/b fails" || b != "chain/b is failing; no alert and no recovery action while chain/c fails" {
		t.Fatalf("%q %q", a, b)
	}
	run("c", "ok", 10*time.Minute)
	run("c", "ok", 10*time.Minute+30*time.Second)
	if events := run("a", "failed", 10*time.Minute+35*time.Second); len(events) != 0 {
		t.Fatal("a alerted while b was failing", eventSummary(events))
	}
	events := run("b", "failed", 10*time.Minute+40*time.Second)
	if len(events) != 1 || !events[0].Notify || !reflect.DeepEqual(events[0].NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails, []string{"chain/a"}) {
		t.Fatalf("%+v", events)
	}
	run("b", "ok", 11*time.Minute)
	run("b", "ok", 11*time.Minute+30*time.Second)
	if events = run("a", "failed", 11*time.Minute+35*time.Second); len(events) != 1 || !events[0].Notify {
		t.Fatalf("%+v", events)
	}
}

// A heartbeat can be named: it fails from its missed deadline until the
// next heartbeat. Its alert names the quiet check, and the quiet time
// survives a daemon start, because a deadline is durable evidence.
func TestAHeartbeatAsTheCause(t *testing.T) {
	t.Parallel()
	p, c := heartbeatConfig(t)
	c.CheckGroups["app"] = checksandalerts.CheckGroup{Timing: checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "30s"}, Checks: map[string]checksandalerts.NamedCheck{
		"health": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true", SuccessExitCodes: []int{0}}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}, NoAlertsOrRecoveryActionsWhileTheseChecksFail: []string{"backup/heartbeat"}}},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	start := time.Now().UTC().Add(-time.Hour)
	d.mu.Lock()
	d.snapshot.Checks["backup/heartbeat"].Deadline = start
	for _, rc := range d.checks {
		rc.next = time.Now().Add(time.Hour)
	}
	d.mu.Unlock()
	d.schedule(context.Background(), start.Add(time.Second))
	d.workers.Wait()
	queued := queuedEvents(t, d)
	if len(queued) != 1 || queued[0].Check != "heartbeat" || !strings.Contains(queued[0].Body, "no alerts and no recovery actions for these checks while this check fails: app/health") {
		t.Fatalf("%+v", queued)
	}
	acceptRun(t, d, "app/health", "failed", start.Add(10*time.Second))
	events := acceptRun(t, d, "app/health", "failed", start.Add(3*time.Minute))
	if len(events) != 1 || events[0].Notify || !strings.HasSuffix(events[0].Message, "; no alert while backup/heartbeat fails") {
		t.Fatalf("%+v", events)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if d, err = NewDaemon(p); err != nil {
		t.Fatal(err)
	}
	if events = acceptRun(t, d, "app/health", "failed", start.Add(4*time.Minute)); len(events) != 0 {
		t.Fatal("after the start", eventSummary(events))
	}
	if code := postHeartbeat(d); code != 204 {
		t.Fatal(code)
	}
	if events = acceptRun(t, d, "app/health", "failed", start.Add(5*time.Minute)); len(events) != 1 || !events[0].Notify {
		t.Fatalf("%+v", events)
	}
}

// An every_systemd_unit check that is kept quiet: the incident of each
// failed unit is held like the incident of a check. No alert, no raise, and
// no reminder goes out in the quiet time. After it, the recovery of a unit
// that was announced before goes out, and the failure of a unit that is
// still failed.
func TestAQuietCheckWithOneIncidentForEachFailedUnit(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"team_chat": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:1"}, RemindEvery: "10m", TimeLimitPerDeliveryAttempt: "1s"}}
	timing := checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "30s"}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"postgres": {Timing: timing, Checks: map[string]checksandalerts.NamedCheck{"up": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true", SuccessExitCodes: []int{0}}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "sos"}}}}},
		"machine": {Timing: timing, Checks: map[string]checksandalerts.NamedCheck{"units": {Check: checksandalerts.Check{Inputs: &checksandalerts.EverySystemdUnit{FailWhen: "a_systemd_unit_has_failed", SystemdUnitsToIgnore: []string{}},
			AlertSeveritySettings:                         checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: "10m"},
			NoAlertsOrRecoveryActionsWhileTheseChecksFail: []string{"postgres/up"}}}}},
	}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	start := time.Now().UTC().Add(-time.Hour)
	units := func(after time.Duration, names ...string) []checksandalerts.Event {
		t.Helper()
		o := checksandalerts.Observation{Status: "ok", Message: "no failed units", At: start.Add(after)}
		for _, name := range names {
			o.Status, o.Message = "failed", "failed units"
			o.Findings = append(o.Findings, failedUnit(name, "exit-code"))
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		events := d.accept(context.Background(), "machine/units", d.checks["machine/units"], o)
		if err := d.record(events); err != nil {
			t.Fatal(err)
		}
		return events
	}
	// A unit fails and is announced before the quiet time.
	units(0, "early.service")
	if events := units(2*time.Minute, "early.service"); len(events) != 1 || !events[0].Notify {
		t.Fatalf("%+v", events)
	}
	acceptRun(t, d, "postgres/up", "failed", start.Add(3*time.Minute))
	// In the quiet time a second unit fails, and the first is repaired.
	events := append(units(3*time.Minute+10*time.Second, "early.service", "late.service"), units(5*time.Minute+10*time.Second, "late.service")...)
	events = append(events, units(5*time.Minute+40*time.Second, "late.service")...)
	if len(events) != 2 || events[0].Type != "failed" || events[0].Finding != "late.service" || events[0].Notify || events[1].Type != "recovered" || events[1].Finding != "early.service" || events[1].Notify ||
		!strings.HasSuffix(events[0].Message, "; no alert while postgres/up fails") {
		t.Fatalf("%+v", events)
	}
	d.mu.Lock()
	held := append(d.raiseOpenIncidents(start.Add(16*time.Minute)), d.remind(start.Add(16*time.Minute))...)
	d.mu.Unlock()
	if len(held) != 0 {
		t.Fatal("MMWatchdog sent a raise or a reminder for a unit of a quiet check", eventSummary(held))
	}
	acceptRun(t, d, "postgres/up", "ok", start.Add(20*time.Minute))
	acceptRun(t, d, "postgres/up", "ok", start.Add(20*time.Minute+30*time.Second))
	events = units(20*time.Minute+45*time.Second, "late.service")
	if len(events) != 2 || events[0].Type != "recovered" || events[0].Finding != "early.service" || !events[0].Notify || events[1].Type != "failed" || events[1].Finding != "late.service" || !events[1].Notify || events[1].AlertSeverity != "sos" {
		t.Fatalf("%+v", events)
	}
}

// A recovery command waits like a restart: a quiet check does not run it,
// and after the quiet time the next failed run does.
func TestAQuietCheckRunsNoRecoveryCommand(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	timing := checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "30s"}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"postgres": {Timing: timing, Checks: map[string]checksandalerts.NamedCheck{"up": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true", SuccessExitCodes: []int{0}}}}}},
		"cache": {Timing: timing, Checks: map[string]checksandalerts.NamedCheck{"fresh": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true", SuccessExitCodes: []int{0}}, NoAlertsOrRecoveryActionsWhileTheseChecksFail: []string{"postgres/up"}}}},
			RecoveryCommand: &checksandalerts.RecoveryCommand{Script: "echo run >> " + testhelpers.ShellQuote(runs), TimeLimitPerAttempt: "5s", ExecOptions: testhelpers.OwnOptions(t),
				RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"fresh"}, Attempts: "3", MinTimeBetweenAttempts: "1ms", ResetAttemptsAfterHealthyFor: "10m"}}},
	}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	count := func() int { b, _ := os.ReadFile(runs); return strings.Count(string(b), "run") }
	// run applies one run made now, and lets a recovery command that is due
	// finish.
	run := func(key, status string) {
		t.Helper()
		acceptRun(t, d, key, status, time.Now().UTC())
		d.workers.Wait()
	}
	run("postgres/up", "failed")
	run("cache/fresh", "failed")
	if count() != 0 || d.snapshot.RecoveryAttemptsByTarget["cache"].Attempts != 0 {
		t.Fatal("a quiet check ran the recovery command", count())
	}
	// recovered_after_healthy_for is 30s: the database check has been
	// healthy since half a minute ago.
	acceptRun(t, d, "postgres/up", "ok", time.Now().UTC().Add(-31*time.Second))
	run("postgres/up", "ok")
	if count() != 0 {
		t.Fatal("a run from the quiet time ran the recovery command")
	}
	run("cache/fresh", "failed")
	if count() != 1 || d.snapshot.RecoveryAttemptsByTarget["cache"].Attempts != 1 {
		t.Fatal("the recovery command did not run after the quiet time", count())
	}
}

// The checks that a check names are no part of its identity: a reload that
// changes them keeps its incident. A check that no longer names the failing
// check is no longer kept quiet by it. A reload that removes a check that
// another check still names is refused.
func TestAReloadChangesTheNamedChecksAndKeepsTheIncident(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	o.run(healthCheck, "failed", 0)
	o.run(healthCheck, "failed", 2*time.Minute)
	incident := o.checkState(healthCheck).FailureIncident.Incident
	if incident == "" || len(o.sent()) != 0 {
		t.Fatal(incident, o.sent())
	}
	myapp := o.config.CheckGroups["myapp"]
	health := myapp.Checks["myapp_answers_health_request"]
	health.NoAlertsOrRecoveryActionsWhileTheseChecksFail = []string{myappUnitCheck}
	myapp.Checks["myapp_answers_health_request"] = health
	writeConfig(t, o.path, o.config)
	if _, err := o.d.reload(); err != nil {
		t.Fatal(err)
	}
	st := o.checkState(healthCheck)
	if st.FailureIncident.Incident != incident || st.QuietNowWhileCheckFails != "" || !reflect.DeepEqual(st.QuietWhileTheseChecksFail, []string{myappUnitCheck}) {
		t.Fatalf("%+v", st)
	}
	if got := o.checkState(postgresCheck).KeepsTheseChecksQuiet; len(got) != 0 {
		t.Fatal(got)
	}
	if got := o.checkState(myappUnitCheck).KeepsTheseChecksQuiet; !reflect.DeepEqual(got, []string{healthCheck}) {
		t.Fatal(got)
	}
	if events := o.run(healthCheck, "failed", 3*time.Minute); len(events) != 1 || events[0].Type != "failed" || !events[0].Notify || events[0].Incident != incident {
		t.Fatalf("%+v", events)
	}
	// The unit check of the application goes, and the health request still
	// names it.
	delete(myapp.Checks, "myapp_service_is_running")
	myapp.RestartSystemdUnit.TriggeredByChecks = []string{"myapp_answers_health_request"}
	writeConfig(t, o.path, o.config)
	if _, err := o.d.reload(); err == nil || !strings.Contains(err.Error(), `this node has no check named "`+myappUnitCheck+`"`) {
		t.Fatal(err)
	}
}

// Each name is a check of this node that can fail, written as
// CHECK_GROUP/CHECK. A check never names itself, and checks that name each
// other in a circle are refused: while all of them fail, MMWatchdog would send no
// alert at all.
func TestChecksNamedAsCausesAreValidated(t *testing.T) {
	t.Parallel()
	const setting = "no_alerts_or_recovery_actions_while_these_checks_fail"
	postgres := "  postgres: {checks: {postgres_service_is_running: {systemd_unit_state: {systemd_unit: postgresql.service, fail_when: the_unit_is_not_active}}}}\n"
	myapp := func(named string) string {
		return "  myapp: {checks: {unit: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}}, health: {tcp: '127.0.0.1:8080', " + setting + ": " + named + "}}}\n"
	}
	heartbeat := "heartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /c, tls_private_key_file: /k}\n"
	for _, tc := range []struct{ text, want string }{
		{"check_groups:\n" + postgres + myapp("[postgres/postgres_service_is_running]"), ""},
		// A check of the same check group is named in the same way.
		{"check_groups:\n" + postgres + myapp("[myapp/unit, postgres/postgres_service_is_running]"), ""},
		// A direct check has the name of its kind, and a direct check states
		// the setting beside its alert_severity.
		{"check_groups:\n  postgres: {systemd_unit_state: {systemd_unit: postgresql.service, fail_when: the_unit_is_not_active}}\n  myapp: {tcp: '127.0.0.1:8080', " + setting + ": [postgres/systemd_unit_state]}\n", ""},
		// A heartbeat can be named. It takes no such setting itself.
		{heartbeat + "check_groups:\n  feed: {heartbeat: {max_time_between_heartbeats: 1h, expected_bearer_token_file: /t}}\n" + myapp("[feed/heartbeat]"), ""},
		{heartbeat + "check_groups:\n" + postgres + "  feed: {heartbeat: {max_time_between_heartbeats: 1h, expected_bearer_token_file: /t}, " + setting + ": [postgres/postgres_service_is_running]}\n",
			"check_groups.feed." + setting + ": does not apply to a heartbeat check"},
		{"check_groups:\n" + postgres + myapp("[postgres/postgres_is_up]"),
			"check_groups.myapp.checks.health." + setting + `: this node has no check named "postgres/postgres_is_up": the checks of the check group postgres are postgres/postgres_service_is_running`},
		{"check_groups:\n" + postgres + myapp("[postgre/postgres_service_is_running]"), `this node has no check named "postgre/postgres_service_is_running": there is no check group postgre`},
		{heartbeat + "check_groups:\n  feed: {heartbeat: {max_time_between_heartbeats: 1h, expected_bearer_token_file: /t}}\n" + myapp("[feed/feed]"), `this node has no check named "feed/feed": the check of the heartbeat check group feed is feed/heartbeat`},
		{"check_groups:\n" + postgres + myapp("[postgres_service_is_running]"), setting + `: "postgres_service_is_running" is not written as CHECK_GROUP/CHECK, such as postgres/postgres_service_is_running`},
		{"check_groups:\n" + postgres + myapp("[postgres/postgres_service_is_running/extra]"), "is not written as CHECK_GROUP/CHECK"},
		{"check_groups:\n" + postgres + myapp("[postgres/postgres_service_is_running, postgres/postgres_service_is_running]"), setting + `: names the check "postgres/postgres_service_is_running" twice`},
		{"check_groups:\n" + postgres + myapp("[myapp/health]"), setting + ": names the check itself"},
		{"check_groups:\n" + postgres + myapp("[]"), setting + ": cannot be empty: name a check as CHECK_GROUP/CHECK, or remove " + setting},
		{"check_groups:\n" + postgres + myapp("postgres/postgres_service_is_running"), "expected a list"},
		// A check that only records never fails and never alerts.
		{"check_groups:\n  machine: {checks: {memory: {only_record_readings: true, node_resource: memory}}}\n" + myapp("[machine/memory]"), setting + `: the check "machine/memory" only records readings and never fails`},
		{"check_groups:\n" + postgres + "  machine: {checks: {memory: {node_resource: memory, " + setting + ": [postgres/postgres_service_is_running]}}}\n",
			"check_groups.machine.checks.memory." + setting + ": a node_resource check that only records its readings never alerts"},
		// A check group of named checks states it on each check.
		{"check_groups:\n" + postgres + "  myapp: {" + setting + ": [postgres/postgres_service_is_running], checks: {health: {tcp: '127.0.0.1:8080'}}}\n",
			"check_groups.myapp." + setting + ": set " + setting + " on each check under checks, not on the check group"},
		// Two checks that name each other, and a circle of three.
		{"check_groups:\n  a: {tcp: '127.0.0.1:1', " + setting + ": [b/tcp]}\n  b: {tcp: '127.0.0.1:2', " + setting + ": [a/tcp]}\n",
			"check_groups.a." + setting + ": the checks a/tcp -> b/tcp -> a/tcp name each other in a circle: while all of them fail, MMWatchdog would send no alert for any of them"},
		{"check_groups:\n  a: {tcp: '127.0.0.1:1', " + setting + ": [d/tcp, b/tcp]}\n  b: {tcp: '127.0.0.1:2', " + setting + ": [c/tcp]}\n  c: {tcp: '127.0.0.1:3', " + setting + ": [a/tcp]}\n  d: {tcp: '127.0.0.1:4'}\n",
			"the checks a/tcp -> b/tcp -> c/tcp -> a/tcp name each other in a circle"},
		// A circle that the first path from its start does not close, and
		// a check outside a circle that names a check of it.
		{"check_groups:\n  a: {tcp: '127.0.0.1:1', " + setting + ": [b/tcp, c/tcp]}\n  b: {tcp: '127.0.0.1:2', " + setting + ": [d/tcp]}\n  c: {tcp: '127.0.0.1:3', " + setting + ": [d/tcp]}\n  d: {tcp: '127.0.0.1:4', " + setting + ": [c/tcp]}\n",
			"check_groups.c." + setting + ": the checks c/tcp -> d/tcp -> c/tcp name each other in a circle"},
		{"check_groups:\n  0x: {tcp: '127.0.0.1:1', " + setting + ": [a/tcp]}\n  a: {tcp: '127.0.0.1:1', " + setting + ": [b/tcp]}\n  b: {tcp: '127.0.0.1:2', " + setting + ": [a/tcp]}\n",
			"check_groups.a." + setting + ": the checks a/tcp -> b/tcp -> a/tcp name each other in a circle"},
		// Two checks that name the same cause through two others are no circle.
		{"check_groups:\n  a: {tcp: '127.0.0.1:1', " + setting + ": [b/tcp, c/tcp]}\n  b: {tcp: '127.0.0.1:2', " + setting + ": [d/tcp]}\n  c: {tcp: '127.0.0.1:3', " + setting + ": [d/tcp]}\n  d: {tcp: '127.0.0.1:4'}\n", ""},
		// A chain is no circle: the last cause alerts.
		{"check_groups:\n  a: {tcp: '127.0.0.1:1', " + setting + ": [b/tcp, c/tcp]}\n  b: {tcp: '127.0.0.1:2', " + setting + ": [c/tcp]}\n  c: {tcp: '127.0.0.1:3'}\n", ""},
	} {
		_, err := config.Load(testhelpers.ConfigFile(t, tc.text))
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q:\ngot  %v\nwant %q", tc.text, err, tc.want)
		}
	}
	// The named checks are no part of the check's identity, and each check
	// learns which checks name it, in order of name.
	named, err := config.Load(testhelpers.ConfigFile(t, "check_groups:\n"+postgres+myapp("[postgres/postgres_service_is_running]")+"  other: {tcp: '127.0.0.1:9', "+setting+": [postgres/postgres_service_is_running]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := config.Load(testhelpers.ConfigFile(t, "check_groups:\n"+postgres+"  myapp: {checks: {unit: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}}, health: {tcp: '127.0.0.1:8080'}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := config.CheckDefinitions(named), config.CheckDefinitions(plain)
	if a["myapp/health"].Identity() != b["myapp/health"].Identity() || a[postgresCheck].Identity() != b[postgresCheck].Identity() {
		t.Fatal("naming a cause changed the identity of a check")
	}
	if got := a["myapp/health"].QuietWhileTheseChecksFail; !reflect.DeepEqual(got, []string{postgresCheck}) {
		t.Fatal(got)
	}
	if got := a[postgresCheck].KeepsTheseChecksQuiet; !reflect.DeepEqual(got, []string{"myapp/health", "other/tcp"}) {
		t.Fatal(got)
	}
}

// The alert of a failing check names the checks that it keeps quiet: its
// failure, a raise, and a reminder, and not its recovery or a cannot-check
// alert. It names at most 20 of them and counts the rest.
func TestTheAlertOfTheCauseNamesTheQuietChecks(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 30, 12, 2, 0, 0, time.UTC)
	since := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	code := 3
	quiet := []string{"myapp/myapp_answers_health_request", "shop/shop_answers_health_request"}
	st := &incidents.CheckState{CheckGroup: "postgres", Check: "postgres_service_is_running", KeepsTheseChecksQuiet: quiet,
		FailureIncident: incidents.Track{Incident: "incident-1", AlertSeverity: "sos", Evidence: &checksandalerts.Evidence{Incident: "incident-1", FailingSince: since, Observation: checksandalerts.Observation{ExitCode: &code}}}}
	body := func(track *incidents.Track, kind, message string) string {
		e := st.Event(track, kind, "incident-1", message, at, true)
		return checksandalerts.AlertBody(e, []checksandalerts.Event{e}, "")
	}
	line := "no alerts and no recovery actions for these checks while this check fails: myapp/myapp_answers_health_request, shop/shop_answers_health_request"
	if got, want := body(&st.FailureIncident, "failed", "systemd unit postgresql.service is failed"), "systemd unit postgresql.service is failed\nfailing since 12:00 UTC\n"+line+"\nexit code: 3"; got != want {
		t.Fatalf("%q", got)
	}
	if got := body(&st.FailureIncident, checksandalerts.FailureIncidentEvents.Raised, "raised"); got != "raised\nfailing since 12:00 UTC\n"+line+"\nexit code: 3" {
		t.Fatalf("%q", got)
	}
	if got := body(&st.FailureIncident, "reminder", "still failing"); got != "still failing\n"+line+"\nexit code: 3" {
		t.Fatalf("%q", got)
	}
	if got := body(&st.FailureIncident, "recovered", "active"); got != "active" {
		t.Fatalf("%q", got)
	}
	if got := body(&st.CannotCheckIncident, "cannot_check", "timed out"); strings.Contains(got, "no alerts") {
		t.Fatalf("%q", got)
	}
	// A destination that takes only the title gets none of the names.
	onlyTitle := st.Event(&st.FailureIncident, "failed", "incident-1", "failed", at, true).OnlyTheTitle()
	if len(onlyTitle.NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails) != 0 || onlyTitle.NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails != 0 {
		t.Fatalf("%+v", onlyTitle)
	}
	// A hub's grouped message shows the line with the lines of its first node.
	first := st.Event(&st.FailureIncident, "failed", "incident-1", "postgresql.service is failed", at, true)
	first.Node = "db-1"
	second := first
	second.Node = "db-2"
	message := store.Combine([]checksandalerts.Event{first, second}, "util")
	message.WriteAlertTexts([]checksandalerts.Event{first, second}, "util")
	if !strings.Contains(message.Body, "\nfrom db-1:\n  postgresql.service is failed\n  failing since 12:00 UTC\n  "+line+"\n") {
		t.Fatalf("%q", message.Body)
	}
	// More than 20 quiet checks: the alert names 20 and counts all.
	st.KeepsTheseChecksQuiet = nil
	for i := range 23 {
		st.KeepsTheseChecksQuiet = append(st.KeepsTheseChecksQuiet, "app/check_"+string(rune('a'+i)))
	}
	e := st.Event(&st.FailureIncident, "failed", "incident-1", "failed", at, true)
	if len(e.NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails) != 20 || e.NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails != 23 || !strings.Contains(checksandalerts.AlertBody(e, []checksandalerts.Event{e}, ""), "app/check_t, and 3 more\n") {
		t.Fatalf("%+v", e)
	}
}

// Each failed unit of an every_systemd_unit check has its own incident.
// The alert of each unit names the checks that the check keeps quiet.
func TestTheAlertOfEachFailedUnitNamesTheQuietChecks(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	units := checksandalerts.Observation{Status: "failed", At: at, Findings: []checksandalerts.Finding{failedUnit("backup.service", "exit-code")}}
	st := &incidents.CheckState{CheckGroup: "machine", Check: "no_failed_units", OneIncidentForEachFinding: true, Timing: timing,
		AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}, KeepsTheseChecksQuiet: []string{"backup/backup_is_recent"}}
	events := st.Observe(units, timing, false)
	if len(events) != 1 || events[0].Finding != "backup.service" || !events[0].Notify || !reflect.DeepEqual(events[0].NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails, []string{"backup/backup_is_recent"}) {
		t.Fatalf("%+v", events)
	}
}

// A scheduled run that MMWatchdog could not make is an observation of the check
// too: unknown. For a quiet check it sends no cannot-check alert either.
func TestAMissedRunOfAQuietCheckSendsNoAlert(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	o.run(postgresCheck, "failed", 0)
	missed := func(after time.Duration) {
		t.Helper()
		at := o.start.Add(after)
		o.d.mu.Lock()
		// Only the health request is due, and its previous run has not
		// finished.
		for key, rc := range o.d.checks {
			rc.next = time.Now().Add(time.Hour)
			if key == healthCheck {
				rc.running, rc.next = true, at.Add(-time.Minute)
			}
		}
		o.d.mu.Unlock()
		o.d.schedule(testhelpers.TestContext(t), at)
		o.d.workers.Wait()
	}
	missed(time.Minute)
	missed(4 * time.Minute)
	st := o.checkState(healthCheck)
	if st.QuietNowWhileCheckFails != postgresCheck || st.CannotCheckIncident.Incident == "" || st.CannotCheckIncident.Announced != "" || len(o.sent()) != 0 {
		t.Fatalf("%+v, sent %v", st, o.sent())
	}
	recorded, err := o.d.store.CheckGroupEvents("myapp", 1)
	if err != nil || len(recorded) != 1 || recorded[0].Type != "cannot_check" || recorded[0].Title != "" || !strings.HasSuffix(recorded[0].Message, quietBecausePost) {
		t.Fatal(recorded, err)
	}
}

// While the named check fails, the one alert has the named check's severity.
// A check that names a check with a lower alert_severity is a validation
// warning: its failure would reach fewer destinations than its own alert.
func TestNamingACheckWithALowerAlertSeverityIsAValidationWarning(t *testing.T) {
	t.Parallel()
	const setting = "no_alerts_or_recovery_actions_while_these_checks_fail"
	c, err := config.Load(testhelpers.ConfigFile(t, `heartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /c, tls_private_key_file: /k}
check_groups:
  postgres: {checks: {postgres_service_is_running: {systemd_unit_state: {systemd_unit: postgresql.service, fail_when: the_unit_is_not_active}, alert_severity: warn}}}
  cache: {tcp: '127.0.0.1:6379', alert_severity: sos}
  nightly: {heartbeat: {max_time_between_heartbeats: 26h, expected_bearer_token_file: /t}, alert_severity: info}
  web: {tcp: '127.0.0.1:443', alert_severity: sos, `+setting+`: [postgres/postgres_service_is_running, cache/tcp]}
  myapp:
    checks:
      health: {tcp: '127.0.0.1:8080', alert_severity: sos, `+setting+`: [postgres/postgres_service_is_running, cache/tcp, nightly/heartbeat]}
      queue: {tcp: '127.0.0.1:8081', alert_severity: warn, `+setting+`: [postgres/postgres_service_is_running, cache/tcp]}
      disk: {tcp: '127.0.0.1:8082', alert_severity: info, `+setting+`: [postgres/postgres_service_is_running]}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []config.CheckThatNamesACheckWithALowerAlertSeverity{
		{CheckGroup: "myapp", Check: "health", AlertSeverity: "sos", NamedCheck: "postgres/postgres_service_is_running", NamedCheckAlertSeverity: "warn"},
		{CheckGroup: "myapp", Check: "health", AlertSeverity: "sos", NamedCheck: "nightly/heartbeat", NamedCheckAlertSeverity: "info"},
		// A direct check has the setting on its check group.
		{CheckGroup: "web", Check: "tcp", AlertSeverity: "sos", NamedCheck: "postgres/postgres_service_is_running", NamedCheckAlertSeverity: "warn"},
	}
	if got := c.ChecksThatNameACheckWithALowerAlertSeverity(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if warnings := c.ValidationWarnings("web-3", true); !reflect.DeepEqual(warnings.ChecksThatNameACheckWithALowerAlertSeverity, want) || len(warnings.AlertTitlesThatWillBeShortened) != 0 || len(warnings.SystemdUnitsMissingFromSystemdUnitsToIgnore) != 0 {
		t.Fatalf("%+v", warnings)
	}
}

// A reload returns the validation warnings of the configuration that it
// activated, for mmwatchdog reload to print.
func TestAReloadReturnsTheValidationWarnings(t *testing.T) {
	t.Parallel()
	o := newOneCause(t)
	if warnings, err := o.d.reload(); err != nil || len(warnings.ChecksThatNameACheckWithALowerAlertSeverity) != 0 {
		t.Fatalf("%+v %v", warnings, err)
	}
	postgres := o.config.CheckGroups["postgres"].Checks["postgres_service_is_running"]
	postgres.AlertSeverity = "info"
	o.config.CheckGroups["postgres"].Checks["postgres_service_is_running"] = postgres
	writeConfig(t, o.path, o.config)
	warnings, err := o.d.reload()
	want := []config.CheckThatNamesACheckWithALowerAlertSeverity{{CheckGroup: "myapp", Check: "myapp_answers_health_request", AlertSeverity: "warn", NamedCheck: postgresCheck, NamedCheckAlertSeverity: "info"}}
	if err != nil || !reflect.DeepEqual(warnings.ChecksThatNameACheckWithALowerAlertSeverity, want) || o.checkState(postgresCheck).AlertSeveritySettings.AlertSeverity != "info" {
		t.Fatalf("%+v %v", warnings, err)
	}
}
