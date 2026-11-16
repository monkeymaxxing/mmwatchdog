# Decide when a check alerts

**When to use this page:** you are writing or changing a check, and you
want to choose how soon a failure alerts you, how serious the alert is, and
when the problem counts as over.

**Who acts:** you, or your coding agent. Each step runs on your controller
machine, the machine where you run `mmx`, except where the step says "on the
node".

**What this page does:** you set the four timings of a check group, the
waiting times of a check that needs other values than its check group, and
the `alert_severity` of each check. Then you install the check group with
MMX and confirm on the node that MMWatchdog uses your values.

**Terms:** a **run** is one execution of a check; its result is `ok`,
`failed`, or `unknown`. A check's **failing time** starts at its first
failed run and ends when the check has been healthy for
`recovered_after_healthy_for`. Every waiting time of a check, such as
`alert_after_failing_for`, counts from the start of the failing time. The
**alert severity** of a check is `info`, `warn`, or `sos`; it decides which
alert destinations get the check's alerts.

## Before you start

- The check group exists in the MMX project that installed MMWatchdog. If
  not, follow [Add a check](add-a-check.md) first.
- Open a terminal in the MMX project: the directory that holds
  `mmx_project_settings.yml`.
- Open the check group's file in `files/hub_and_standalone/check_groups/`.
  This page uses `myapp.yml`, the check group of
  [Add a check](add-a-check.md), whose checks watch `myapp.service` and its
  health page.

## Steps

1. **Set how often the checks run.** On the check group, `check_every` says
   how often each check runs, and `time_limit_per_run` says how long one
   run may take:

   ```yaml
       check_every: 30s                    # run each check every 30 seconds
       time_limit_per_run: 5s              # one run of a check may take 5 seconds
   ```

   - A duration is a number with a unit: `100ms`, `30s`, `5m`, `26h`, or
     `7d` for whole days.
   - `check_every` is at least `100ms`. Every 30 seconds or every minute
     suits most checks.
   - Set `time_limit_per_run` to a little more than a healthy run takes.
     Reason: a run that reaches the limit is ended. For a check that asks a
     server, such as `http`, no answer within the limit is a failure. For a
     check that runs a program, such as `shell`, the result is `unknown`.
   - A check cannot have its own `check_every` or `time_limit_per_run`. A
     check that must run at another pace goes in a check group of its own.

   **Check:** both values are on the check group, and `time_limit_per_run`
   is not longer than `check_every`.

2. **Set how long a failure lasts before the alert.**
   `alert_after_failing_for` is the time from the first failed run to the
   alert:

   ```yaml
       alert_after_failing_for: 1m         # alert after 1 minute of failing
   ```

   - A short value alerts you sooner, and also for short problems that end
     on their own, such as a slow minute during a backup.
   - `0s` alerts at the first failed run.
   - With a value above `0s`, one failed run followed by healthy runs
     sends no alert.
   - The alert goes out at a failed run, never at a healthy one: at the
     first failed run after `alert_after_failing_for` has passed.

   **Check:** the value is on the check group.

3. **Set when a problem is over.** `recovered_after_healthy_for` is how
   long a check must be healthy before its failing time ends. When an alert
   went out, the recovery message goes out then:

   ```yaml
       recovered_after_healthy_for: 30s    # the problem is over after 30 healthy seconds
   ```

   Reason: a check that fails on and off is treated as failing the whole
   time. A healthy period shorter than `recovered_after_healthy_for` does
   not end the failing time, and each failed run starts the count of
   healthy time again. So a longer value turns a check that fails on and
   off into one alert, instead of an alert and a recovery each time.
   `0s` ends the failing time at the first healthy run.

   **Check:** the four timings `check_every`, `time_limit_per_run`,
   `alert_after_failing_for`, and `recovered_after_healthy_for` are all on
   the check group. Each one is required.

4. **Give a check its own waiting times, where it needs them.** A check
   under `checks` may state its own `alert_after_failing_for`, its own
   `recovered_after_healthy_for`, or both. A waiting time that the check
   does not state is the check group's. This memory check waits 10 minutes,
   because memory use above the limit for one minute is often harmless:

   ```yaml
         memory_is_below_512_mib:
           systemd_unit_resource:
             systemd_unit: myapp.service
             resource: memory
           fail_when_measurement_above:
             memory_used_mib: 512
           alert_severity: warn
           alert_after_failing_for: 10m      # this check waits 10 minutes, not 1
           recovered_after_healthy_for: 5m   # and is over after 5 healthy minutes
   ```

   **Check:** each check that needs other waiting times states them; every
   other check uses the check group's.

5. **Set the alert severity of each check.** Each check that can alert
   states its own `alert_severity`; there is no default. Each alert
   destination, in `files/hub_and_standalone/alert_destinations.yml`, takes
   the severities that its `alert_severities` names. So the severity
   decides who gets the alert:

   | Severity | Use it for | Example | Typical destination |
   |---|---|---|---|
   | `sos` | A person must act now, at any hour. | The application is down. | A destination that wakes a person. Only `sos` opens an incident in MMHotline. |
   | `warn` | A person should look today. | The disk is 85 percent full. | The team's chat channel. |
   | `info` | A record; nobody needs to act. | A notice that one of many web servers rebooted. | The team's chat channel, or a quiet channel. |

   A check whose problem grows more urgent with time can start at `warn`
   and rise to `sos`. `raise_to_sos_after_failing_for` raises the check's
   open alert to `sos` when the check has been failing that long, counted
   from the start of the failing time:

   ```yaml
           alert_severity: warn
           raise_to_sos_after_failing_for: 1h
   ```

   For a problem that grows by amount, such as a fuller disk, use a second
   check with a higher limit and a higher severity, as the starter check
   group does: `warn` above 85 percent and `sos` above 95 percent.

   **Check:** every check has an `alert_severity` of `info`, `warn`, or
   `sos`, written on the check and not on the check group. A check with
   `only_record_readings: true` never alerts and needs none.

6. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists your file as a change to `/etc/mmwatchdog`:

   ```text
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked input /etc/mmwatchdog/check_groups/myapp.yml would change
   ```

   The dryrun of this project exits with code 2: something would change.
   It does not check your check group: `validate_config` checks only the
   copy of the configuration that the last run left on the node. Step 7
   checks your check group before the run changes anything.

7. **Install the change:**

   ```sh
   mmx run hub_and_standalone
   ```

   **Check:** the run ends with `Run ...: succeeded`. If it ends with
   `failed`, see [If something is different](#if-something-is-different).

8. **Confirm the timings on the node:**

   ```sh
   ssh deploy@app01 sudo mmwatchdog status myapp
   ```

   Each check shows the timings and the severity that MMWatchdog uses:

   ```text
   myapp/health_page_answers    ok           HTTP 200 and configured content match
     Config: /etc/mmwatchdog/check_groups/myapp.yml:13:7
     Rule: check every 30s, time limit per run 5s, alert after failing for 1m, recovered after healthy for 30s
     Alert severity: sos
     Observed: 2026-11-15T04:47:01.824325463Z
   myapp/memory_is_below_512_mib ok           memory_used_mib is 9.86 (fails above 512)
     Config: /etc/mmwatchdog/check_groups/myapp.yml:18:7
     Rule: check every 30s, time limit per run 5s, alert after failing for 10m, recovered after healthy for 5m
     Alert severity: warn
     Raise to SOS after failing for: 1h
     Observed: 2026-11-15T04:47:01.359331501Z
     memory_used_mib: 9.86328125 MiB
   ...
   ```

   **Check:** the `Rule` line of each check shows your values, including
   the check's own waiting times.

### The check group, whole

```yaml
# files/hub_and_standalone/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s                    # run each check every 30 seconds
    time_limit_per_run: 5s              # one run of a check may take 5 seconds
    alert_after_failing_for: 1m         # alert after 1 minute of failing
    recovered_after_healthy_for: 30s    # the problem is over after 30 healthy seconds
    checks:
      service_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active
        alert_severity: sos
      health_page_answers:
        http:
          url: http://127.0.0.1:8080/health
          expected_http_status_codes: [200]
        alert_severity: sos
      memory_is_below_512_mib:
        systemd_unit_resource:
          systemd_unit: myapp.service
          resource: memory
        fail_when_measurement_above:
          memory_used_mib: 512
        alert_severity: warn
        alert_after_failing_for: 10m      # this check waits 10 minutes, not 1
        recovered_after_healthy_for: 5m   # and is over after 5 healthy minutes
        raise_to_sos_after_failing_for: 1h
    restart_systemd_unit:               # restart myapp when the health page fails
      systemd_unit: myapp.service
      triggered_by_checks: [health_page_answers]
      restart_after_failing_for: 1m
      min_time_between_attempts: 1m
      max_attempts: 3
      reset_attempts_after_healthy_for: 10m
```

The `restart_systemd_unit` block is explained in
[Restart a service automatically](restart-a-service-automatically.md). Its
`restart_after_failing_for` counts from the same start of the failing time
as `alert_after_failing_for`, so you choose whether the restart or the
alert comes first.

## If something is different

- **The run failed at `validate_config`.** The output names the file, the
  line, and the fault, such as:

  ```text
  mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/myapp.yml:4:5: check_groups.myapp.time_limit_per_run: 10s exceeds check_every 5s
  ```

  The path is the copy that the run checks on the node; the line and column
  are those of your file. Other faults of this page's settings read:

  ```text
  check_groups.myapp.check_every: is required: how often each check of this check group runs, such as 30s
  check_groups.myapp.check_every: must be at least 100ms
  check_groups.myapp.alert_severity: set alert_severity on each check under checks, not on the check group
  check_groups.myapp.checks.health_page_answers.alert_severity: required: info, warn, or sos
  ```

  Fix the file and run step 7 again. Nothing changed on the node.
- **A check fails on and off.** It stays in one failing time, so it sends
  one alert after `alert_after_failing_for`, and its alert says that it
  failed on and off:

  ```text
  Warning: app01: myapp/health_page_answers failed
  expected HTTP 200, received 404
  failing on and off since 04:32 UTC; 4 of the last 7 runs failed
  ```

  While its latest run is healthy, `status` shows `confirming_failure`
  before the alert, or `confirming_recovery` after it. To get fewer such
  alerts, raise `recovered_after_healthy_for`. MMWatchdog has no rule that
  counts failed runs, such as "4 failed runs out of 6": the two waiting
  times do that job.
- **An alert rises to `sos`.** The destinations get a second message with
  the reason:

  ```text
  SOS: app01: myapp/health_page_answers still failing, now SOS
  myapp/health_page_answers has been failing for 1m, so this alert is now SOS (raise_to_sos_after_failing_for: 1m)
  ```

  A `raise_to_sos_after_failing_for` at or below the check's
  `alert_after_failing_for` makes the first alert `sos`. A maintenance
  window holds the raise until the window ends. The severity of an open
  alert only rises, never falls, until the problem is over. A change of a
  check's `alert_severity` keeps its open alert; a higher severity raises
  the open alert soon after the change is installed.
- **A check is `unknown`: MMWatchdog cannot run it.** An unknown run never
  opens a failure, but it keeps the failing time running and starts the
  count of healthy time again. A check that stays unknown for
  `alert_after_failing_for` sends a cannot-check alert, at the check's
  severity:

  ```text
  Info: app01: MMWatchdog cannot check slow/sleeps_too_long
  the run took longer than 5s (time_limit_per_run), so MMWatchdog ended it
  MMWatchdog has not been able to run the check since 04:32 UTC
  ```

  This one is a `shell` check whose run took longer than
  `time_limit_per_run`. With `raise_to_sos_after_failing_for`, a
  cannot-check alert rises to `sos` after that long, counted from the first
  unknown run.
- **A run does not end even after its time limit.** Some runs stay blocked
  in the kernel, such as reading a file on a network filesystem whose
  server is gone. MMWatchdog waits 5 seconds after `time_limit_per_run`,
  then marks the run `unknown` with `the check run did not finish within
  the time limit`, and starts no new run of that check until the old run
  ends. The other checks go on. After `alert_after_failing_for`, the
  cannot-check alert goes out.
- **The check reports events, such as `out_of_memory_kills`, or
  `log_lines` with `fail_when_a_new_line_contains`.** Such an event check
  alerts at the first new event. It ignores the check group's
  `alert_after_failing_for` and refuses its own:

  ```text
  check_groups.myapp.checks.no_oom_kills.alert_after_failing_for: an event check opens its incident at the first new event; remove alert_after_failing_for from this check. Set recovered_after_healthy_for to keep the incident open after the last event
  ```

  Its `recovered_after_healthy_for` counts from the first run that finds no
  new event. A `log_lines` check with `fail_when_no_new_line_for` is not an
  event check: it uses all four timings.
- **You deploy, or MMWatchdog restarts, while a check is failing.** A
  restart of MMWatchdog, and the start or end of a maintenance window,
  end a failing time that has not alerted yet: the next failed run starts
  a new one. An alert that went out stays open until the check is healthy
  for `recovered_after_healthy_for`. Each `mmx run` that changes the
  configuration restarts MMWatchdog inside a maintenance window.
- **A failure goes on for hours.** MMWatchdog sends no second failure
  alert. A destination's `remind_every` sends a reminder of each open
  problem; see
  [Delivery, reminders, and evidence](send-alerts-somewhere.md).
- **A script or a coding agent needs each check's effective waiting
  times.** On the node, `sudo mmwatchdog config --json` gives them, with
  where each one is stated:

  ```text
  "alert_after_failing_for": "10m",
  "alert_after_failing_for_stated_on": "check",
  ...
  "recovered_after_healthy_for": "5m",
  "recovered_after_healthy_for_stated_on": "check",
  ```

  `stated_on` is `check` or `check_group`.
- **You change a node that MMX did not install.** Edit the file in
  `/etc/mmwatchdog/check_groups/` on the node, then run
  `sudo mmwatchdog validate` and `sudo mmwatchdog reload`.

## Related pages

- [Add a check](add-a-check.md)
- [Restart a service automatically](restart-a-service-automatically.md)
- [See what is happening](see-what-is-happening.md): what each state in
  `status` means.
- [Which destination gets an alert](send-alerts-somewhere.md)
- [Stop alerts that follow another failure](stop-alerts-that-follow-another-failure.md)
