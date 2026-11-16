package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/testhelpers"
)

func TestEventChecksOpenAtTheFirstEvent(t *testing.T) {
	t.Parallel()
	for _, module := range []string{
		"out_of_memory_kills: {fail_when: the_kernel_kills_a_process_for_lack_of_memory}",
		"log_lines: {log_file_path: /var/log/app.log, fail_when_a_new_line_contains: ERROR}",
		"log_lines: {log_file_path: /var/log/app.log, fail_when_a_new_line_matches_regex: ERROR}",
	} {
		for _, direct := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/direct=%v", module, direct), func(t *testing.T) {
				group := "    checks:\n      events:\n        " + module + "\n        alert_severity: warn\n"
				if direct {
					group = "    " + module + "\n    alert_severity: warn\n"
				} else {
					group = "    alert_after_failing_for: 1h\n" + group
				}
				text := "check_groups:\n  app:\n    check_every: 30s\n    time_limit_per_run: 5s\n    recovered_after_healthy_for: 2m\n" + group
				path := testhelpers.ConfigFile(t, text)
				c, err := config.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				var key string
				for name := range config.CheckDefinitions(c) {
					key = name
				}
				definition := config.CheckDefinitions(c)[key]
				snapshot := incidents.FreshSnapshot()
				snapshot.ReconcileOne(c, key, &definition, time.Now())
				st := snapshot.Checks[key]
				timing := definition.Timing
				now := time.Now()
				events := st.Observe(checksandalerts.Observation{Status: "failed", At: now}, timing, false)
				if len(events) != 1 || events[0].Type != "failed" {
					t.Fatal("first event did not alert", events)
				}
				// A later event breaks the healthy streak and keeps the same incident.
				incident := st.FailureIncident.Incident
				st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(time.Minute)}, timing, false)
				st.Observe(checksandalerts.Observation{Status: "failed", At: now.Add(2 * time.Minute)}, timing, false)
				st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(3 * time.Minute)}, timing, false)
				if st.FailureIncident.Incident != incident {
					t.Fatal("event incident closed too early")
				}
				events = st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(5 * time.Minute)}, timing, false)
				if len(events) != 1 || events[0].Type != "recovered" {
					t.Fatal(events)
				}
				if direct {
					text = strings.Replace(text, "    "+module, "    alert_after_failing_for: 0s\n    "+module, 1)
				} else {
					text = strings.Replace(text, "        "+module, "        alert_after_failing_for: 0s\n        "+module, 1)
				}
				_, err = config.Load(testhelpers.ConfigFile(t, text))
				if err == nil || !strings.Contains(err.Error(), "remove alert_after_failing_for") || !strings.Contains(err.Error(), "recovered_after_healthy_for") {
					t.Fatal("event-check timing not refused clearly", err)
				}
			})
		}
	}
	// A silence check still inherits the group's ordinary alert delay.
	c, err := config.Load(testhelpers.ConfigFile(t, "check_groups:\n  app:\n    alert_after_failing_for: 5m\n    log_lines: {log_file_path: /var/log/app.log, fail_when_no_new_line_for: 10m}\n    alert_severity: warn\n"))
	if err != nil {
		t.Fatal(err)
	}
	if timing := config.CheckDefinitions(c)["app/log_lines"].Timing; timing.AlertAfterFailingFor != "5m" {
		t.Fatal(timing)
	}
}

func TestSharedRestartSettingsMustMatch(t *testing.T) {
	t.Parallel()
	for _, setting := range []string{"max_attempts", "min_time_between_attempts", "reset_attempts_after_healthy_for"} {
		t.Run(setting, func(t *testing.T) {
			path := testhelpers.ConfigFile(t, "check_groups: {}\n")
			first := "check_groups:\n  app:\n    check_every: 30s\n    time_limit_per_run: 5s\n    alert_after_failing_for: 1m\n    recovered_after_healthy_for: 30s\n    shell: 'false'\n    alert_severity: warn\n    restart_systemd_unit:\n      systemd_unit: app.service\n      triggered_by_checks: [shell]\n      restart_after_failing_for: 0s\n      max_attempts: 3\n      min_time_between_attempts: 1m\n      reset_attempts_after_healthy_for: 10m\n"
			second := strings.Replace(first, "  app:", "  peer:", 1)
			replacements := map[string]string{"max_attempts": "4", "min_time_between_attempts": "2m", "reset_attempts_after_healthy_for": "20m"}
			old := map[string]string{"max_attempts": "3", "min_time_between_attempts": "1m", "reset_attempts_after_healthy_for": "10m"}
			second = strings.Replace(second, setting+": "+old[setting], setting+": "+replacements[setting], 1)
			for name, text := range map[string]string{"first.yml": first, "second.yml": second} {
				if err := os.WriteFile(filepath.Join(path, "check_groups", name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := config.Load(path)
			if err == nil || !strings.Contains(err.Error(), "first.yml:") || !strings.Contains(err.Error(), "second.yml:") || !strings.Contains(err.Error(), setting) {
				t.Fatal("missing both setting locations", err)
			}
		})
	}
}

func TestTimingReloadKeepsLogReadPosition(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	log := filepath.Join(t.TempDir(), "application.log")
	if err := os.WriteFile(log, []byte("ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Checks: map[string]checksandalerts.NamedCheck{"log": {Check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log, FailWhenANewLineContains: "ERROR"}}}}}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	scheduledRun(t, d, "app/log")
	if err := os.WriteFile(log, []byte("ready\nERROR: happened during reload\n"), 0600); err != nil {
		t.Fatal(err)
	}
	group := c.CheckGroups["app"]
	group.Timing = checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "10s", AlertAfterFailingFor: "5m", RecoveredAfterHealthyFor: "2m"}
	c.CheckGroups["app"] = group
	writeConfig(t, path, c)
	if _, err = d.reload(); err != nil {
		t.Fatal(err)
	}
	if observation := scheduledRun(t, d, "app/log"); observation.Status != "failed" || !strings.Contains(observation.Message, "1 new line") {
		t.Fatal("reload lost the unread log event", observation)
	}
}

func TestTimingReloadKeepsOutOfMemoryCount(t *testing.T) {
	root := testhelpers.KernelFiles(t, map[string]string{"proc/vmstat": "oom_kill 7\n", "proc/sys/kernel/random/boot_id": "boot\n"})
	path, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Checks: map[string]checksandalerts.NamedCheck{"kills": {Check: checksandalerts.Check{Inputs: &checksandalerts.OutOfMemoryKills{FailWhen: "the_kernel_kills_a_process_for_lack_of_memory"}}}}}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if first := scheduledRun(t, d, "app/kills"); first.Status != "ok" {
		t.Fatal(first)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/vmstat"), []byte("oom_kill 8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	group := c.CheckGroups["app"]
	group.Timing = checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "10s", AlertAfterFailingFor: "5m", RecoveredAfterHealthyFor: "2m"}
	c.CheckGroups["app"] = group
	writeConfig(t, path, c)
	if _, err = d.reload(); err != nil {
		t.Fatal(err)
	}
	if next := scheduledRun(t, d, "app/kills"); next.Status != "failed" {
		t.Fatal("reload hid an out-of-memory kill", next)
	}
}

func TestRecoverySpacingCountsFromAttemptEnd(t *testing.T) {
	t.Parallel()
	for _, command := range []bool{false, true} {
		t.Run(fmt.Sprintf("command=%v", command), func(t *testing.T) {
			path, c := daemonConfig(t)
			group := c.CheckGroups["app"]
			settings := checksandalerts.RecoveryAttemptSettings{Attempts: "3", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m", TriggeredByChecks: []string{"shell"}}
			key := "app.service"
			if command {
				key = "app"
				group.RecoveryCommand = &checksandalerts.RecoveryCommand{RecoveryAttemptSettings: settings, Script: "true", TimeLimitPerAttempt: "1m", ExecOptions: testhelpers.OwnOptions(t)}
			} else {
				group.RestartSystemdUnit = &checksandalerts.RestartSystemdUnit{SystemdUnit: key, RecoveryAttemptSettings: settings}
			}
			c.CheckGroups["app"] = group
			writeConfig(t, path, c)
			d, err := NewDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			now := d.now()
			st := d.snapshot.Checks["app/shell"]
			st.Observe(checksandalerts.Observation{Status: "failed", At: now}, st.Timing, false)
			d.checks["app/shell"].fresh = true
			budget := &incidents.RecoveryAttempts{Attempts: 1, Last: now.Add(-10 * time.Minute), LastFinished: now.Add(-30 * time.Second)}
			d.snapshot.RecoveryAttemptsByTarget[key] = budget
			if d.recoveryDue(key, now, "") != "" {
				t.Fatal("spacing counted from start")
			}
			budget.LastFinished = now.Add(-time.Minute)
			if d.recoveryDue(key, now, "") == "" {
				t.Fatal("not due one minute after completion")
			}
		})
	}
}

func TestRecoveryAttemptCompletionStartsFullWait(t *testing.T) {
	t.Parallel()
	for _, command := range []bool{false, true} {
		t.Run(fmt.Sprintf("command=%v", command), func(t *testing.T) {
			path, c := daemonConfig(t)
			dir := t.TempDir()
			started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
			waitForRelease := "touch " + testhelpers.ShellQuote(started) + "; while [ ! -f " + testhelpers.ShellQuote(release) + " ]; do sleep 0.01; done"
			t.Cleanup(func() { os.WriteFile(release, nil, 0600) })
			group := c.CheckGroups["app"]
			settings := checksandalerts.RecoveryAttemptSettings{Attempts: "3", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m", TriggeredByChecks: []string{"shell"}}
			key := "app.service"
			if command {
				key = "app"
				group.RecoveryCommand = &checksandalerts.RecoveryCommand{RecoveryAttemptSettings: settings, Script: waitForRelease, TimeLimitPerAttempt: "1h", ExecOptions: testhelpers.OwnOptions(t)}
			} else {
				group.RestartSystemdUnit = &checksandalerts.RestartSystemdUnit{SystemdUnit: key, RecoveryAttemptSettings: settings}
				testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=failed\\n'; else "+waitForRelease+"; fi\n")
				testhelpers.PutProgramsFirst(t, dir)
			}
			c.CheckGroups["app"] = group
			writeConfig(t, path, c)
			clock := testhelpers.NewClock()
			d := newDaemonOnClock(t, path, clock)
			defer os.WriteFile(release, nil, 0600)
			d.mu.Lock()
			state := d.snapshot.Checks["app/shell"]
			state.Observe(checksandalerts.Observation{Status: "failed", At: clock.Now()}, state.Timing, false)
			d.checks["app/shell"].fresh = true
			d.startRecovery(testhelpers.TestContext(t), clock.Now())
			d.mu.Unlock()
			testhelpers.WaitFor(t, "recovery attempt starts", func() bool { _, err := os.Stat(started); return err == nil })
			clock.Pass(10 * time.Minute)
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			d.workers.Wait()
			d.mu.Lock()
			defer d.mu.Unlock()
			budget := d.snapshot.RecoveryAttemptsByTarget[key]
			if budget.Attempts != 1 || !budget.LastFinished.Equal(clock.Now()) {
				t.Fatal("completion time not recorded", budget)
			}
			state = d.snapshot.Checks["app/shell"]
			fail := func() {
				state.Observe(checksandalerts.Observation{Status: "failed", At: clock.Now()}, state.Timing, false)
				d.checks["app/shell"].fresh = true
			}
			fail()
			if d.recoveryDue(key, clock.Now(), "") != "" {
				t.Fatal("a long attempt removed the wait after completion")
			}
			clock.Pass(time.Minute)
			fail()
			if d.recoveryDue(key, clock.Now(), "") == "" {
				t.Fatal("next attempt not due after full waiting time")
			}
			restored, err := d.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !restored.RecoveryAttemptsByTarget[key].LastFinished.Equal(budget.LastFinished) {
				t.Fatal("completion time not durable")
			}
		})
	}
}

func TestReloadRecomputesCheckScheduleAndKeepsLogPosition(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name                string
		previousInterval    checksandalerts.Duration
		newInterval         checksandalerts.Duration
		reloadAfter         time.Duration
		firstRunAfterReload time.Duration
	}{
		{"shorter interval already due", "1h", "30s", time.Minute, time.Minute},
		{"shorter interval not yet due", "1h", "30s", 10 * time.Second, 30 * time.Second},
		{"longer interval", "30s", "1m", 10 * time.Second, time.Minute},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			path, c := daemonConfig(t)
			logPath := filepath.Join(t.TempDir(), "application.log")
			if err := os.WriteFile(logPath, []byte("ready\n"), 0600); err != nil {
				t.Fatal(err)
			}
			c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
				Timing: checksandalerts.Timing{CheckEvery: scenario.previousInterval, TimeLimitPerRun: "5s", AlertAfterFailingFor: "1h", RecoveredAfterHealthyFor: "30s"},
				Checks: map[string]checksandalerts.NamedCheck{"log": {Check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: logPath, FailWhenANewLineContains: "ERROR"}}}},
			}}
			writeConfig(t, path, c)
			clock := testhelpers.NewClock()
			d := newDaemonOnClock(t, path, clock)
			start := clock.Now()
			d.schedule(testhelpers.TestContext(t), start)
			d.workers.Wait()
			first := d.snapshot.Checks["app/log"].Last
			if first.Status != "ok" || !first.At.Equal(start) {
				t.Fatal("the scheduler did not establish the log's read position", first)
			}
			if err := os.WriteFile(logPath, []byte("ready\nERROR: an event before reload\n"), 0600); err != nil {
				t.Fatal(err)
			}
			clock.Pass(scenario.reloadAfter)
			group := c.CheckGroups["app"]
			group.CheckEvery = scenario.newInterval
			c.CheckGroups["app"] = group
			writeConfig(t, path, c)
			if _, err := d.reload(); err != nil {
				t.Fatal(err)
			}
			if scenario.firstRunAfterReload > scenario.reloadAfter {
				clock.Pass(scenario.firstRunAfterReload - scenario.reloadAfter - time.Second)
				d.schedule(testhelpers.TestContext(t), clock.Now())
				d.workers.Wait()
				if observation := d.snapshot.Checks["app/log"].Last; !observation.At.Equal(start) {
					t.Fatal("the check ran before the new interval was due", observation)
				}
				clock.Pass(time.Second)
			}
			d.schedule(testhelpers.TestContext(t), clock.Now())
			d.workers.Wait()
			observation := d.snapshot.Checks["app/log"].Last
			if !observation.At.Equal(start.Add(scenario.firstRunAfterReload)) || observation.Status != "failed" || !strings.Contains(observation.Message, "1 new line") {
				t.Fatal("the new schedule did not find the unread log event", observation)
			}
		})
	}
}
