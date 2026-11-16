# Stop alerts that follow another failure

**When to use this page:** one problem makes several checks fail, and you
get an alert, or an automatic restart, for each of them. For example, the
database stops, and the health page of every application that uses the
database fails too. You want one alert, for the cause, and no restart of an
application that only waits for the database.

**Who acts:** you, or your coding agent. Each step runs on your controller
machine, the machine where you run `mmx`, except where the step says "on the
node". The test in step 7 stops the database for about three minutes: on a
production node, a coding agent asks the person first.

**What this page does:** you name the check of the cause in each check that
fails because of it, with `no_alerts_or_recovery_actions_while_these_checks_fail`.
While the check of the cause fails, MMWatchdog **keeps the other check
quiet**: it sends no alert of that check, and restarts nothing and runs no
recovery command because of it. The quiet check keeps running, and
`status` shows its state. When the cause is healthy again, the quiet
check's next run decides whether it alerts.

**Terms:** the **cause** is the check that another check names, such as the
check of the database. A **quiet check** is a check that MMWatchdog keeps
quiet. Its **quiet time** starts when the cause starts to fail, and ends
with the quiet check's first run after the cause's failing time. A check's
**failing time** starts at its first failed run and ends when the check
has been healthy for its `recovered_after_healthy_for`.

## Before you start

- The check groups exist in the MMX project that installed MMWatchdog. This
  page uses `myapp.yml` from [Add a check](add-a-check.md), with the
  restart from
  [Restart a service automatically](restart-a-service-automatically.md).
- The node has a check of the cause. For a database on the same node, a
  check of its systemd unit or its port. For a database on another
  machine, a `tcp` check of its port from this node: MMWatchdog keeps a
  check quiet only for a cause that the same node checks.
- Open a terminal in the MMX project: the directory that holds
  `mmx_project_settings.yml`.

## Steps

1. **Find the name of the cause's check.** On the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status
   ```

   ```text
   ...
   myapp/health_page_answers    ok           HTTP 200 and configured content match
   myapp/service_is_running     ok           systemd unit myapp.service is active
   ...
   postgres/postgres_service_is_running ok           systemd unit postgresql.service is active
   ```

   The name is written as `CHECK_GROUP/CHECK`, here
   `postgres/postgres_service_is_running`. If the node has no check of the
   cause, add one, as [Add a check](add-a-check.md) shows. This page uses:

   ```yaml
   # files/hub_and_standalone/check_groups/postgres.yml
   check_groups:
     postgres:
       check_every: 30s
       time_limit_per_run: 5s
       alert_after_failing_for: 1m
       recovered_after_healthy_for: 30s
       checks:
         postgres_service_is_running:
           systemd_unit_state:
             systemd_unit: postgresql.service
             fail_when: the_unit_is_not_active
           alert_severity: sos
   ```

   **Check:** you have the cause's full name, with its check group.

2. **Give the cause the severity of the whole outage.** The one alert of
   the outage is the cause's alert: it goes out at the cause's
   `alert_severity`, to the destinations that take that severity. So a
   database that stops every application gets `sos`, as the applications'
   checks would.

   **Check:** the cause's `alert_severity` is at least the severity of each
   check that will name it.

3. **Choose the checks that the cause makes fail.** Name the cause only on
   those checks. Here, the health page of `myapp` fails while the database
   is down. The check `service_is_running` does not name the database:
   `myapp.service` keeps running without the database, so a stopped
   `myapp.service` is a problem of its own and still alerts.

   **Check:** each check that you choose fails when the cause fails, and
   says nothing more than that while the cause fails.

4. **Name the cause on each chosen check.** In
   `files/hub_and_standalone/check_groups/myapp.yml`, under
   `health_page_answers`:

   ```yaml
         health_page_answers:
           http:
             url: http://127.0.0.1:8080/health
             expected_http_status_codes: [200]
           alert_severity: sos
           # no alert and no restart because of this check while the database is down
           no_alerts_or_recovery_actions_while_these_checks_fail: [postgres/postgres_service_is_running]
   ```

   - Write each name as `CHECK_GROUP/CHECK`, also for a check of the same
     check group.
   - The setting stands on the check, beside `alert_severity`. On a check
     group with a single check written directly on the group, it stands on
     the group, as `alert_severity` does.
   - A list of several causes keeps the check quiet while any one of them
     fails.
   - The setting holds back only what this check calls for. The restart of
     `myapp.service` still comes when another check in its
     `triggered_by_checks` fails.

   [The check group, whole](#the-check-group-whole) shows the file.

   **Check:** each name is written with its check group, and the setting
   is indented as deep as `alert_severity`.

5. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists your file as a change:

   ```text
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked inputs /etc/mmwatchdog/check_groups/myapp.yml and 1 more would change
   ```

   The dryrun exits with code 2: something would change. It does not check
   the new configuration: step 6 does.

6. **Install the change, and read it on the node:**

   ```sh
   mmx run hub_and_standalone
   ```

   The run ends with `Run ...: succeeded`. Then, on the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status myapp
   ```

   ```text
   myapp/health_page_answers    ok           HTTP 200 and configured content match
     Config: /etc/mmwatchdog/check_groups/myapp.yml:14:7
     Rule: check every 30s, time limit per run 5s, alert after failing for 1m, recovered after healthy for 30s
     Alert severity: sos
     No alerts or recovery actions while these checks fail: postgres/postgres_service_is_running
   ...
   ```

   `sudo mmwatchdog status postgres` shows the other side:
   `While this check fails, no alerts or recovery actions for: myapp/health_page_answers`.

   **Check:** both lines are there. If the run failed, or printed a
   warning, see [If something is different](#if-something-is-different).

7. **Stop the cause, and watch the one alert.** On the node, stop the
   database:

   ```sh
   sudo systemctl stop postgresql.service
   ```

   The health page of `myapp` fails too. A minute later, `status` says what
   MMWatchdog holds back, below the list of checks:

   ```sh
   sudo mmwatchdog status
   ```

   ```text
   ...
   myapp/health_page_answers    failed       expected HTTP 200, received 404
   myapp/service_is_running     ok           systemd unit myapp.service is active
   postgres/postgres_service_is_running failed       systemd unit postgresql.service is stopped. systemd reports no failure: the unit was stopped on request, or its program ended normally. MMWatchdog did not start the unit.
   myapp/health_page_answers is failing; no alert and no recovery action while postgres/postgres_service_is_running fails
   Alert deliveries: 0 waiting, 0 dropped
   Systemd unit restarts myapp.service: 0 of 3 attempts used
   ```

   The destinations get one alert, for the database. Its last line names
   the checks that MMWatchdog keeps quiet:

   ```text
   SOS: app01: postgres/postgres_service_is_running failed
   systemd unit postgresql.service is stopped. systemd reports no failure: the unit was stopped on request, or its program ended normally. MMWatchdog did not start the unit.
   failing since 07:11 UTC
   no alerts and no recovery actions for these checks while this check fails: myapp/health_page_answers
   ```

   The health page's failure is recorded on the node only, in
   `sudo mmwatchdog events myapp`:

   ```text
   2026-11-15T07:12:44Z failed               myapp/health_page_answers expected HTTP 200, received 404; no alert while postgres/postgres_service_is_running fails
   ```

   Then start the database again:

   ```sh
   sudo systemctl start postgresql.service
   ```

   **Check:** the destinations got the database's alert, then
   `Resolved SOS: app01: postgres/postgres_service_is_running is ok again`,
   and nothing about `myapp`. `status` shows `0 of 3 attempts used`: no
   restart of `myapp.service`.

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
        # no alert and no restart because of this check while the database is down
        no_alerts_or_recovery_actions_while_these_checks_fail: [postgres/postgres_service_is_running]
    restart_systemd_unit:               # restart myapp when the health page fails
      systemd_unit: myapp.service
      triggered_by_checks: [health_page_answers]
      restart_after_failing_for: 1m
      min_time_between_attempts: 1m
      max_attempts: 3
      reset_attempts_after_healthy_for: 10m
```

## When the cause is healthy again

The quiet time ends with the quiet check's first run after the cause's
failing time, so the cause must first be healthy for its
`recovered_after_healthy_for`. That run decides. Reason: a result from the
quiet time says nothing about the check now that the cause works again.

- **The quiet check is healthy:** nothing is sent about it, as in step 7.
- **The quiet check still fails:** MMWatchdog treats it as it would without
  the setting, at once. Its waiting times kept counting during the quiet
  time, so an alert or a restart that became due then follows from this
  run.

In a test where the database was down for 90 seconds and the health page
still failed after the database came back, `status` showed, between the
end of the database's failing time and the health check's next run:

```text
myapp/health_page_answers is failing; no alert and no recovery action until its next run, because postgres/postgres_service_is_running was failing
```

At that next run, the restart and the alert came together, in
`sudo mmwatchdog events myapp`:

```text
2026-11-15T07:19:15Z systemd_unit_restart_requested myapp/health_page_answers restart systemd unit myapp.service, attempt 1
2026-11-15T07:19:15Z failed               myapp/health_page_answers expected HTTP 200, received 404
2026-11-15T07:17:45Z failed               myapp/health_page_answers expected HTTP 200, received 404; no alert while postgres/postgres_service_is_running fails
```

The alert counts the failure from its first failed run, in the quiet time:

```text
SOS: app01: myapp/health_page_answers failed
expected HTTP 200, received 404
failing since 07:16 UTC
```

In that test the database recovered before its own
`alert_after_failing_for`, so the database sent no alert at all. A cause
keeps a check quiet during the cause's whole failing time, also before the
cause's own alert.

Nothing that MMWatchdog held back is sent later: no failure, no reminder,
no recovery.

## If something is different

- **The run failed at `validate_config`.** The output names the file, the
  line, and the fault, such as:

  ```text
  mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/myapp.yml:20:9: check_groups.myapp.checks.health_page_answers.no_alerts_or_recovery_actions_while_these_checks_fail: this node has no check named "postgres/postgres_is_up": the checks of the check group postgres are postgres/postgres_service_is_running
  ```

  The other faults of this setting end in:

  | Error | Fix |
  |---|---|
  | `this node has no check named "postgre/postgres_service_is_running": there is no check group postgre` | Use a check group that `status` lists |
  | `"postgres_service_is_running" is not written as CHECK_GROUP/CHECK, such as postgres/postgres_service_is_running` | Add the check group and a slash |
  | `names the check itself` | Name another check |
  | `names the check "postgres/postgres_service_is_running" twice` | Remove the second copy of the name |
  | `cannot be empty: name a check as CHECK_GROUP/CHECK, or remove no_alerts_or_recovery_actions_while_these_checks_fail` | Name a check, or remove the setting |
  | `set no_alerts_or_recovery_actions_while_these_checks_fail on each check under checks, not on the check group` | Move the setting onto each check |
  | `the check "machine/cpu_use_is_recorded" only records readings and never fails: no_alerts_or_recovery_actions_while_these_checks_fail can name only checks with an alert_severity` | Name a check that can fail. A check with `only_record_readings: true` can neither be a cause nor be kept quiet |
  | `a node_resource check that only records its readings never alerts; remove no_alerts_or_recovery_actions_while_these_checks_fail` | The setting is on a check with `only_record_readings: true`, which has no alert to keep quiet. Remove the setting |
  | `the checks myapp/health_page_answers -> postgres/postgres_service_is_running -> myapp/health_page_answers name each other in a circle: while all of them fail, MMWatchdog would send no alert for any of them` | Remove one name of the circle |
  | `the cause "postgres/postgres_service_is_running" sends alerts to no destination while keeping "myapp/health_page_answers" quiet; add a destination for the cause's alert_severity or change its send_alerts_only_to_destinations` | Let a destination take the cause's alerts. Reason: otherwise the outage sends no alert at all |

  Fix the file and run `mmx run hub_and_standalone` again. Nothing changed
  on the node.
- **The run prints a warning about a lower `alert_severity`.** The
  configuration is valid, and the run goes on. The warning names each
  check that names a cause with a lower severity:

  ```text
  While a check named in another check's no_alerts_or_recovery_actions_while_these_checks_fail fails, MMWatchdog sends one alert, for the named check and with the named check's alert_severity. Each check listed below names a check that has a lower alert_severity than the listed check. So while the named check fails, a destination that takes only the listed check's higher alert_severity gets no alert. Give the named check the alert_severity that the named check's failure deserves:
    myapp/health_page_answers has alert_severity sos and names postgres/postgres_service_is_running, which has alert_severity warn
  ```

  Here, a destination that takes only `sos`, such as the phone of the
  person on call, hears nothing while the database is down. Raise the
  cause's `alert_severity`, as in step 2. `sudo mmwatchdog validate` on
  the node prints the same warning.
- **Checks form a chain.** A cause can itself name a cause. When the
  application's health page names its database's check, and the database's
  check names the check of the disk under it, and all three fail, only the
  disk's check alerts. Its alert names the database's check, the check
  that names it; the health page is quiet because the database's check
  fails.
- **The quiet check alerted before the cause failed.** Its alert stays
  open. During the quiet time MMWatchdog holds back its reminders, a raise
  to `sos`, and its recovery message. If the check is healthy after the
  quiet time, the recovery message goes out with that first run; if not,
  a raise that became due goes out, and the reminders count from the end
  of the quiet time.
- **MMWatchdog restarts during the outage,** such as for a deploy, or the
  cause's check group enters or leaves a maintenance window. The quiet
  check stays quiet until MMWatchdog has run every cause that it names
  again. So a deploy in the first minutes of an outage sends no alert of
  the quiet check and restarts nothing.
- **A maintenance window covers the cause's check group.** The cause still
  counts as failing during its failing time, so the checks that name it
  stay quiet.
- **MMWatchdog cannot run the cause's check,** for example because too many
  checks run at once. That run is `unknown`, which keeps the cause's
  failing time going, so the quiet time goes on. Only lasting health of
  the cause ends it.
- **The cause is a heartbeat or an `every_systemd_unit` check.** A heartbeat
  check group can be a cause, named as `nightly_backup/heartbeat`. It
  cannot have the setting itself: the run fails with
  `check_groups.nightly_backup.no_alerts_or_recovery_actions_while_these_checks_fail: does not apply to a heartbeat check`.
  Reason: a heartbeat has no runs that could decide after the quiet time.
  A quiet [`every_systemd_unit`](../reference/modules/every_systemd_unit.md) check
  holds back the alert and the recovery actions of each failed unit.
- **You want MMWatchdog to start the database first, or in order.**
  The setting starts, stops, and orders nothing. Start units in order with
  systemd, such as `After=` and `Requires=` in the application's unit file.
- **You remove a cause from the list.** The removed cause stops keeping the
  check quiet at once. The change does not close the check's open alert,
  and does not make the check new.
- **You change a node that MMX did not install.** Edit the file in
  `/etc/mmwatchdog/check_groups/` on the node, then run
  `sudo mmwatchdog validate` and `sudo mmwatchdog reload`.

## Related pages

- [Add a check](add-a-check.md)
- [Restart a service automatically](restart-a-service-automatically.md)
- [Decide when a check alerts](decide-when-a-check-alerts.md)
- [See what is happening](see-what-is-happening.md)
- [One cause, one alert](../how-it-works.md#one-cause-one-alert): every rule of
  quiet checks, with its reason.
