# Restart a service automatically

**When to use this page:** a check of your application fails, and a
restart of the application's systemd unit usually fixes the problem, such as
an application that hangs. Or a command of yours fixes it, such as deleting
old cache files when the disk is full.

**Who acts:** you, or your coding agent. Each step runs on your controller
machine, the machine where you run `mmx`, except where the step says "on the
node". The test in step 7 makes your application hang for about two
minutes: on a production node, a coding agent asks the person first.

**What this page does:** you add a `restart_systemd_unit` block, or a
`recovery_command` block, to a check group, and install it with MMX. Then
you make the application hang and watch MMWatchdog restart it. MMWatchdog
makes a limited number of attempts, so it does not restart a broken service
forever.

**Terms:** a **recovery action** is a restart of a systemd unit or a run of
a recovery command. Each recovery action is an **attempt**: it does not
prove that the problem is fixed. The problem counts as over only when the
check has been healthy for its `recovered_after_healthy_for`. A check's
**failing time** starts at its first failed run; every waiting time on this
page counts from that start.

## Choose the action

| The fix | Block in the check group | Follow |
|---|---|---|
| Restart the application's systemd unit | `restart_systemd_unit` | [Steps to restart a systemd unit](#steps-to-restart-a-systemd-unit) |
| Run a command of yours, such as deleting old files | `recovery_command` | [Steps to run a command instead](#steps-to-run-a-command-instead) |

A check group can have both blocks.

## Before you start

- The check group exists in the MMX project that installed MMWatchdog,
  with a check that fails when the application is broken, such as a check
  of its health page. If not, follow [Add a check](add-a-check.md) first.
- For a restart: the application runs as a systemd unit, such as
  `myapp.service`.
- Open a terminal in the MMX project: the directory that holds
  `mmx_project_settings.yml`.

## Steps to restart a systemd unit

1. **Choose the checks that call for the restart.** List them in
   `triggered_by_checks`. This page restarts `myapp.service` when the check
   `health_page_answers` fails:

   ```yaml
       restart_systemd_unit:
         systemd_unit: myapp.service             # the unit that MMWatchdog restarts
         triggered_by_checks: [health_page_answers]
   ```

   - Each name is a check of the same check group. For a check group with
     one check written directly on the group, the name is the module's
     name, such as `tcp`.
   - Any check that can fail works, also a check that is not about the
     unit: a limit on the node's memory can call for a restart of
     `nginx.service`.
   - A check with `only_record_readings: true` never fails, so it is
     refused.
   - A check of the unit's state, such as `service_is_running`, is often
     not needed here. Reason: MMWatchdog never starts a stopped unit (see
     [If something is different](#if-something-is-different)), and
     systemd restarts a crashed unit itself when the unit has
     `Restart=on-failure`.
     A health page that stops answering is what systemd cannot see.

   **Check:** each name in `triggered_by_checks` is a check of the check
   group.

2. **Choose when the restart comes.** `restart_after_failing_for` is the
   time from the first failed run to the restart. It counts from the same
   start as the check group's `alert_after_failing_for`, so you choose what
   comes first:

   ```yaml
         restart_after_failing_for: 1m           # restart after 1 minute of failing
   ```

   - **Shorter than `alert_after_failing_for`:** MMWatchdog restarts first,
     and alerts only if the failure lasts. A restart that fixes the problem
     sends no alert; it appears only in `mmwatchdog events` and `status`:

     ```text
     2026-11-15T04:38:52Z systemd_unit_restart_result myapp/health_page_answers systemd unit restart finished. The next check runs will show whether the check has recovered
     2026-11-15T04:38:52Z systemd_unit_restart_requested myapp/health_page_answers restart systemd unit myapp.service, attempt 1
     ```

   - **Equal:** the restart and the alert come at the same failed run.
     Choose this when a person should know about every hang.
   - **Longer:** the alert goes out first, and the restart follows if the
     failure is still there.
   - `0s` restarts at the first failed run.

   **Check:** you know which comes first. This page uses `1m` for both.

3. **Choose the limits on attempts.** Three settings keep restarts in
   bounds:

   ```yaml
         min_time_between_attempts: 1m           # wait 1 minute after a restart before the next
         max_attempts: 3                         # restart at most 3 times
         reset_attempts_after_healthy_for: 10m   # count from 0 again after 10 healthy minutes
   ```

   - `min_time_between_attempts` counts from the end of one restart, when
     `systemctl restart` returns, to the start of the next. Reason: the
     application gets time to start and answer before MMWatchdog judges the
     restart.
   - `max_attempts` is a whole number from 1, or `unlimited`. When the
     attempts are used, MMWatchdog stops restarting and keeps running the
     checks; a person then fixes the cause. With `unlimited`, MMWatchdog
     never stops restarting, and `min_time_between_attempts` still spaces
     the restarts.
   - `reset_attempts_after_healthy_for` is how long every check in
     `triggered_by_checks` must be healthy, after their alerts have
     closed, before the count of attempts starts from 0 again. Reason: a
     hang next week gets its full attempts, while a service that keeps
     failing does not.

   **Check:** all six settings of `restart_systemd_unit` are written: each
   one is required.

4. **Add the block to the check group.** In
   `files/hub_and_standalone/check_groups/myapp.yml`, add
   `restart_systemd_unit` at the end, indented under `myapp:`, beside
   `checks` and not inside a check:

   ```yaml
       restart_systemd_unit:                     # restart myapp when the health page fails
         systemd_unit: myapp.service
         triggered_by_checks: [health_page_answers]
         restart_after_failing_for: 1m
         min_time_between_attempts: 1m
         max_attempts: 3
         reset_attempts_after_healthy_for: 10m
   ```

   [The check group, whole](#the-check-group-whole) shows the file.

   **Check:** `restart_systemd_unit` has the same indentation as `checks`.

5. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists your file as a change to `/etc/mmwatchdog`:

   ```text
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked input /etc/mmwatchdog/check_groups/myapp.yml would change
   ```

   The dryrun exits with code 2: something would change. It does not check
   the check group itself: its `validate_config` line checks only the copy
   of the configuration that the last run left on the node. Step 6 checks
   your file before the run changes anything.

6. **Install the change:**

   ```sh
   mmx run hub_and_standalone
   ```

   **Check:** the run ends with `Run ...: succeeded`. If it ends with
   `failed`, see [If something is different](#if-something-is-different).

7. **Make the application hang, and watch the restart.** On the node, stop
   the service's main process without ending it:

   ```sh
   ssh deploy@app01 'sudo kill -STOP "$(systemctl show --property MainPID --value myapp.service)"'
   ```

   The health page stops answering. After `restart_after_failing_for`, one
   minute, plus up to one `check_every`, MMWatchdog restarts the unit. Two
   minutes later, read the events on the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog events myapp --last 5m
   ```

   ```text
   2026-11-15T04:50:27Z recovered            myapp/health_page_answers HTTP 200 and configured content match
     ...
   2026-11-15T04:49:27Z systemd_unit_restart_result myapp/health_page_answers systemd unit restart finished. The next check runs will show whether the check has recovered
   2026-11-15T04:49:27Z systemd_unit_restart_requested myapp/health_page_answers restart systemd unit myapp.service, attempt 1
   2026-11-15T04:49:27Z failed               myapp/health_page_answers Get "http://127.0.0.1:8080/health": no answer within 5s (time_limit_per_run)
     ...
   ```

   The restart events name the check that called for the restart. A
   finished restart does not close the alert: the check must then be
   healthy for `recovered_after_healthy_for`, here 30 seconds. Because this
   page restarts and alerts at the same time, the destinations get the
   `SOS` message and then the `Resolved SOS` message:

   ```text
   SOS: app01: myapp/health_page_answers failed
   Get "http://127.0.0.1:8080/health": no answer within 5s (time_limit_per_run)
   failing since 04:48 UTC
   ```

   ```text
   Resolved SOS: app01: myapp/health_page_answers is ok again
   HTTP 200 and configured content match
   ```

   `sudo mmwatchdog status` on the node counts the attempts:

   ```text
   Systemd unit restarts myapp.service: 1 of 3 attempts used; systemd unit restart finished. The next check runs will show whether the check has recovered
   ```

   **Check:** the events show `systemd_unit_restart_requested` and then
   `recovered`. If they show `systemd_unit_restart_withheld`, or no restart,
   see [If something is different](#if-something-is-different).

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
    restart_systemd_unit:               # restart myapp when the health page fails
      systemd_unit: myapp.service
      triggered_by_checks: [health_page_answers]
      restart_after_failing_for: 1m
      min_time_between_attempts: 1m
      max_attempts: 3
      reset_attempts_after_healthy_for: 10m
```

[Decide when a check alerts](decide-when-a-check-alerts.md) explains the
four timings at the top of the check group.

## Steps to run a command instead

1. **Write the command.** The command runs on the node like a `shell`
   check: Bash with `pipefail`, written inline or as the path of an
   executable. This example deletes cache files older than one day:

   ```sh
   find /var/cache/myapp -type f -mtime +1 -print -delete
   ```

   - The command runs as the account `mmwatchdog`, unless
     `run_as_user_account` names another account. `root` must be named.
   - It runs in `/`, unless `run_from_directory` names another directory,
     with the variables of `env` and no other environment.
   - MMWatchdog adds `MMWATCHDOG_CHECK_GROUP` and `MMWATCHDOG_CHECK`, the
     failing check that called for the command, and
     `MMWATCHDOG_INCIDENT_ID` when that check's alert has gone out. Before
     the alert, `MMWATCHDOG_INCIDENT_ID` is empty.
   - What the command prints is kept in the event history, so `-print`
     records which files it deleted. The check's own output does not reach
     the command.

   **Check:** on the node, the command does its job when you run it
   yourself as that account.

2. **Write the check group with `recovery_command`.** Create a file in
   `files/hub_and_standalone/check_groups/`, or add the block to an
   existing check group, beside `checks`:

   ```yaml
   # files/hub_and_standalone/check_groups/root_filesystem.yml
   check_groups:
     root_filesystem:
       check_every: 1m
       time_limit_per_run: 5s
       alert_after_failing_for: 10m
       recovered_after_healthy_for: 5m
       checks:
         root_filesystem_is_below_90_percent:
           node_filesystem: /
           fail_when_measurement_above:
             space_used_percent: 90
           alert_severity: warn
       recovery_command:                   # delete old cache files when the disk is too full
         triggered_by_checks: [root_filesystem_is_below_90_percent]
         script: find /var/cache/myapp -type f -mtime +1 -print -delete
         run_as_user_account: root
         run_after_failing_for: 5m
         time_limit_per_attempt: 2m
         max_attempts: 2
         min_time_between_attempts: 10m
         reset_attempts_after_healthy_for: 1h
   ```

   When the disk has been more than 90 percent full for 5 minutes,
   MMWatchdog runs the command. If the command frees enough space, no alert
   goes out. If the disk is still too full 10 minutes into the failing
   time, the `warn` alert goes out. If it is still too full 10 minutes
   after the first command ended, MMWatchdog runs the command once more,
   and then stops.

   - `run_after_failing_for` works like `restart_after_failing_for` in
     [step 2 of the restart](#steps-to-restart-a-systemd-unit): shorter
     than `alert_after_failing_for` runs the command before the alert.
   - `time_limit_per_attempt` is how long one run of the command may take
     before MMWatchdog ends it.
   - `max_attempts`, `min_time_between_attempts`, and
     `reset_attempts_after_healthy_for` work as for a restart, in step 3.

   **Check:** all seven settings without a default are written:
   `triggered_by_checks`, `script`, `run_after_failing_for`,
   `time_limit_per_attempt`, `max_attempts`, `min_time_between_attempts`,
   and `reset_attempts_after_healthy_for`.

3. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists your file as a change, such as
   `tracked input /etc/mmwatchdog/check_groups/root_filesystem.yml would change`.
   As in the restart steps, the dryrun exits with code 2, and
   `validate_config` does not check your new file.

4. **Install the change:**

   ```sh
   mmx run hub_and_standalone
   ```

   **Check:** the run ends with `Run ...: succeeded`.

5. **Read what the command did, after its first run.** On the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog events root_filesystem
   ```

   ```text
   2026-11-15T04:44:08Z recovery_command_result root_filesystem/root_filesystem_is_below_90_percent recovery command finished with exit code 0
     Observed: 2026-11-15T04:44:08.211087112Z
     Exit code: 0
     stdout: "/var/cache/myapp/old-export.bin\n"
   2026-11-15T04:44:08Z recovery_command_started root_filesystem/root_filesystem_is_below_90_percent run recovery command, attempt 1
   ```

   `sudo mmwatchdog status` counts the attempts:

   ```text
   Recovery command root_filesystem: 1 of 2 attempts used; exit 0
   ```

   A recovery command sends nothing to the alert destinations. As with a
   restart, an exit code of 0 does not close the alert: the check must be
   healthy for `recovered_after_healthy_for`. Another attempt needs a
   failed run after the command ended.

   **Check:** the events show `recovery_command_started` and
   `recovery_command_result` with the exit code and the output. Until the
   check first fails, the events show neither.

## If something is different

- **The run failed at `validate_config`.** The output names the file, the
  line, and the fault, such as:

  ```text
  mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/myapp.yml:18:5: check_groups.myapp.restart_systemd_unit.restart_after_failing_for: is required: how long a check in triggered_by_checks must be failing before MMWatchdog restarts the unit, such as 1m
  ```

  The line and column are those of your file. Other faults of this page's
  settings read:

  ```text
  check_groups.myapp.restart_systemd_unit.max_attempts: expected a whole number of at least 1, or unlimited
  check_groups.myapp.restart_systemd_unit.triggered_by_checks.0: unknown check: health_page
  check_groups.myapp.restart_systemd_unit.triggered_by_checks.0: check memory_is_recorded only records its readings and never fails; give it a limit to use it for recovery
  check_groups.myapp.recovery_command.run_after_failing_for: is required: how long a check in triggered_by_checks must be failing before MMWatchdog runs the command, such as 1m
  ```

  Fix the file and run `mmx run hub_and_standalone` again. Nothing changed
  on the node.
- **The restarts did not help, and every attempt is used.** `status` says
  so, such as `Systemd unit restarts myapp.service: 2 of 2 attempts used`
  with `max_attempts: 2`. MMWatchdog keeps running the checks, and the
  alert goes out at `alert_after_failing_for` as usual. Fix the cause, then
  give MMWatchdog its attempts back. On the node:

  ```sh
  sudo mmwatchdog reset-recovery-attempts myapp
  ```

  ```text
  The recovery attempts of check group myapp are reset. MMWatchdog can attempt recovery actions again.
  ```

  The command also clears the wait of `min_time_between_attempts`, so the
  next failed run can restart the unit at once. The events show
  `recovery_attempts_reset myapp operator reset recovery attempts`. The
  command resets both the restarts and the recovery command of the check
  group. For a check group with neither, it says
  `check group has no restart_systemd_unit and no recovery_command` and
  exits with 4. Without the command, the attempts start from 0 again after
  `reset_attempts_after_healthy_for` of health.
- **The unit is stopped, and MMWatchdog did not start it.** MMWatchdog
  never starts a stopped unit. Before each restart, it asks systemd for the
  unit's state:

  | systemd reports | What it means | What MMWatchdog does |
  |---|---|---|
  | `active` | The unit runs, but a check fails, such as the health page. | Restarts the unit. |
  | `failed` | The program crashed or ended with an error, and systemd did not restart it. | Restarts the unit. |
  | `inactive` | Someone or something stopped the unit on request, or its program ended normally. | Leaves the unit alone and uses no attempt. |

  Reason: whoever stopped the unit decides when it runs again, such as a
  person repairing a database by hand, a package upgrade, or a failover
  tool. The events show one line for the failing time:

  ```text
  2026-11-15T04:42:30Z systemd_unit_restart_withheld myapp/health_page_answers no restart: systemd unit myapp.service is stopped, and MMWatchdog never starts a stopped unit
  ```

  The alert still goes out, from a check of the unit's state:

  ```text
  SOS: app01: myapp/service_is_running failed
  systemd unit myapp.service is stopped. systemd reports no failure: the unit was stopped on request, or its program ended normally. MMWatchdog did not start the unit.
  ```

  Start the unit with `sudo systemctl start myapp.service`. To stop a unit
  on purpose without alerts, turn on a
  [maintenance window](pause-alerts-during-planned-work.md) first. A program
  that must always run needs `Restart=always` in its unit file. Reason:
  systemd reports a program that ended with exit status 0 as `inactive`,
  the same as a stop on request; with `Restart=always`, systemd starts it
  again after every exit.
- **systemd refuses to start the unit.** MMWatchdog never clears systemd's
  start limits, so the unit stays `failed` and its check keeps alerting.
  `systemctl status myapp.service` on the node says why.
- **No restart happened although a check failed.** MMWatchdog restarts
  only when all of these hold: the latest run of a check in
  `triggered_by_checks` failed, at least `restart_after_failing_for` into
  its failing time; attempts remain, and `min_time_between_attempts` has
  passed; no maintenance window covers a check group that restarts the
  unit; and the check is not kept quiet because a check that it names
  fails ([Stop alerts that follow another failure](stop-alerts-that-follow-another-failure.md)).
  A check that is `unknown` never calls for a restart. Another restart
  needs a failed run after the previous restart ended. While systemd is
  starting, stopping, or reloading the unit, MMWatchdog waits. When the
  failing check is a `systemd_unit_state` check of the same unit, and
  systemd reports the unit `active` again, MMWatchdog does not restart it.
- **A restart takes long.** MMWatchdog waits until `systemctl restart`
  returns; the unit's `TimeoutStopSec` and `TimeoutStartSec` set how long
  that may take, and MMWatchdog adds no limit of its own.
  `min_time_between_attempts` counts from the return, so a slow restart
  still gets the full wait. If MMWatchdog stopped during the restart, such
  as at a power loss, the wait counts from the start of that restart.
- **Two check groups restart the same unit.** They share one count of
  attempts, and MMWatchdog runs their restarts one at a time. Both must
  state the same `max_attempts`, `min_time_between_attempts`, and
  `reset_attempts_after_healthy_for`; otherwise the run fails with a line
  such as:

  ```text
  check_groups.myapp_port.restart_systemd_unit.max_attempts: differs from check_groups.myapp.restart_systemd_unit.max_attempts at ...:17:7: restart actions for myapp.service share an attempt budget. Set max_attempts to the same value in the restart_systemd_unit of both check groups
  ```

  Each check group's own `restart_after_failing_for` applies to its own
  checks. `reset-recovery-attempts` with either check group resets the
  unit's attempts.
- **A check group has both a recovery command and a restart.** When both
  are due at the same failed run, the command runs first. The restart then
  becomes due only at a failed run after the command ended. While a restart
  of the check group's unit runs, the command waits.
- **The recovery command failed or ran too long.** An exit code other than
  0, a run past `time_limit_per_attempt`, or a command that could not start
  uses up the attempt. The event `recovery_command_result` gives the exit
  code and the output, shortened to 16 KiB with `truncated: true`. Long
  output is no failure: MMWatchdog reads up to 64 KiB of the command's
  standard output, and the rest does not change the attempt's result.
  MMWatchdog records each attempt before it starts the command or the
  restart, so a restart of MMWatchdog cannot forget an attempt. If it
  cannot record the attempt, it does not run the command.
- **A script or a coding agent reads the attempts.** `sudo mmwatchdog
  status --json` gives `systemd_unit_restart_attempts`, by unit, and
  `recovery_command_attempts`, by check group:

  ```text
  "systemd_unit_restart_attempts": {
    "myapp.service": {
      "attempts_used": 2,
      "max_attempts": 2,
      "last_attempt_finished_at": "2026-11-15T04:40:03.739548956Z",
      "last_attempt_at": "2026-11-15T04:40:03.680851305Z",
      "last_result": "systemd unit restart finished. The next check runs will show whether the check has recovered"
    }
  },
  "recovery_command_attempts": {}
  ```

  `max_attempts` is a number, or the text `unlimited`.
- **You change a node that MMX did not install.** Edit the file in
  `/etc/mmwatchdog/check_groups/` on the node, then run
  `sudo mmwatchdog validate` and `sudo mmwatchdog reload`.

## Related pages

- [Decide when a check alerts](decide-when-a-check-alerts.md)
- [See what is happening](see-what-is-happening.md)
- [Pause alerts during planned work](pause-alerts-during-planned-work.md)
- [Stop alerts that follow another failure](stop-alerts-that-follow-another-failure.md)
