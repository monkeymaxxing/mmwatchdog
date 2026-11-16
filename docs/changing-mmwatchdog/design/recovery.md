# MMWatchdog: recovery and maintenance

This document owns what MMWatchdog does about a failing check, restarting a systemd
unit or running a recovery command, and how planned work keeps alerts and
recovery actions quiet. A recovery action is one restart of a systemd unit or
one run of a recovery command.

MMWatchdog is not a service manager: systemd starts and supervises programs, and MMWatchdog
acts only through the recovery each check group explicitly configures. A
program of your own becomes a systemd unit, which MMX installs, and MMWatchdog watches
that unit
([run your own program as a systemd unit](operation.md#run-your-own-program-as-a-systemd-unit)).
The [product principles](README.md#product-principles) explain why.

## Restart a systemd unit

A check group can restart one systemd unit when selected checks of the check
group fail. Restarting is never implicit. Without `restart_systemd_unit`, MMWatchdog
only runs checks and alerts.

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s                    # run each check every 30 seconds
    time_limit_per_run: 5s              # one run of a check may take 5 seconds
    alert_after_failing_for: 5m         # send the alert after 5 minutes of failing
    recovered_after_healthy_for: 30s    # the failure is over after 30 healthy seconds

    checks:
      myapp_service_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active
        alert_severity: sos
      myapp_answers_health_request:
        http:
          url: http://127.0.0.1:8080/health
          expected_http_status_codes: [200]
          response_body_contains: ready
        alert_severity: sos

    restart_systemd_unit:
      systemd_unit: myapp.service       # the unit that MMWatchdog restarts
      triggered_by_checks: [myapp_service_is_running, myapp_answers_health_request]
      restart_after_failing_for: 1m     # restart after 1 minute of failing
      max_attempts: 3
      min_time_between_attempts: 1m
      reset_attempts_after_healthy_for: 10m
```

| Setting | Default | Meaning |
| --- | --- | --- |
| `systemd_unit` | required | The systemd unit that MMWatchdog restarts |
| `triggered_by_checks` | required | Checks of the check group containing `restart_systemd_unit` whose failure calls for the restart |
| `restart_after_failing_for` | required | How long a check in `triggered_by_checks` must have been failing before MMWatchdog restarts the unit |
| `max_attempts` | required | Maximum systemd unit restart attempts: a number, or `unlimited` |
| `min_time_between_attempts` | required | Minimum time from the end of one restart attempt to the start of the next |
| `reset_attempts_after_healthy_for` | required | Continuous health of every `triggered_by_checks` check that resets the attempts used, once their failure incidents have also closed |

`triggered_by_checks` takes any check of the check group that can fail: a
systemd unit check, an HTTP, TCP, or shell check, a check with a module of
the project's own, or a check about the whole machine. A
[measurement check](checks.md#checks-with-a-limit-on-a-number) without a limit
only records readings. Configuration validation rejects naming a measurement
check without a limit in `triggered_by_checks`.

### The restart's own waiting time

A check counts as failing from its first failed run until the check has been
healthy for `recovered_after_healthy_for`. The period from the first failed
run through confirmed health is the check's failing time. See
[timing and confirmation](checks.md#timing-and-confirmation).
Two waiting times count independently from the start of the failing time:

- `restart_after_failing_for` on the restart: when a run of a check in
  `triggered_by_checks` fails at least this long after the start of the
  check's failing time, the unit is due for a restart.
- `alert_after_failing_for` on the check group or the check: when a run of
  the check fails at least this long after the start of its failing time, MMWatchdog
  opens an incident and queues the failure alert.

MMWatchdog measures `restart_after_failing_for` and `alert_after_failing_for`
at the failing check's own runs, because another check's run cannot confirm
whether the failing check still fails.

The operator chooses the order of restarting and alerting by setting
`restart_after_failing_for` and `alert_after_failing_for`:

- `restart_after_failing_for` shorter than `alert_after_failing_for`: MMWatchdog
  restarts first and alerts only if the failure lasts. A restart that fixes
  the failure sends no alert and appears only in `mmwatchdog events` and in status.
- `restart_after_failing_for` longer: the alert goes out first, and the
  restart follows if the failure continues.
- `restart_after_failing_for` equal to `alert_after_failing_for`: the restart
  and the alert come with the same failed run, at the same moment.

With the check group above, the health request fails and the restart fixes
it. Times count from the first failed run:

| Time | Health request | What MMWatchdog does |
| --- | --- | --- |
| 0s | 503 | The failing time starts |
| 30s | 503 | Nothing yet |
| 60s | 503 | Failing for 1 minute: restart `myapp.service`, attempt 1 of 3 |
| 90s | 200 with `ready` | First healthy run after the restart |
| 120s | 200 with `ready` | Healthy for 30 seconds: the failing time ends. No alert went out |

The `myapp` check group can also keep failing after restarts. Each restart
in the following example takes 10 seconds. MMWatchdog checks again after each
restart and waits a full minute from completion before another attempt:

| Time | Health request | What MMWatchdog does |
| --- | --- | --- |
| 0s | 503 | The failing time starts |
| 60s | 503 | Restart `myapp.service`, attempt 1 of 3; completes at 70s |
| 130s | 503 | Restart, attempt 2 of 3; completes at 140s |
| 200s | 503 | Restart, attempt 3 of 3; completes at 210s, all attempts used |
| 300s | 503 | Failing for 5 minutes: open the incident and queue the failure alert |
| 330s | 200 with `ready` | First healthy run, after someone repaired the application |
| 360s | 200 with `ready` | Healthy for 30 seconds: resolve the incident and queue the recovery alert |

A continuing failure sends no alert at every run;
[reminders](alerts.md#reminders) repeat the alert on their own schedule.

### When a restart is due

A restart is due only when every condition in the following list holds:

- the latest run of a check in `triggered_by_checks` failed, at least
  `restart_after_failing_for` after the start of the check's failing time;
- attempts remain, and `min_time_between_attempts` has passed since the end of the last
  attempt;
- no [maintenance window](#maintenance-windows) covers the check group;
- the failing check is not kept quiet by another failing check
  ([one cause, one alert](checks.md#one-cause-one-alert)); and
- systemd does not report the unit as stopped
  ([a stopped unit stays stopped](#a-stopped-unit-stays-stopped)).

MMWatchdog then:

- inspects the unit and waits while systemd is starting, stopping, or reloading
  it, so MMWatchdog never competes with a job systemd is already running;
- records the attempt durably before asking systemd to restart the unit, so a
  daemon restart cannot forget a restart MMWatchdog made;
- runs `systemctl restart` and records the result as an event.

A restart command completing does not end the failing time. Each check needs
fresh runs, and another restart needs a failed run after the command. A
restart that fixes the unit therefore shows as the ordinary recovery of its
checks.

When all systemd unit restart attempts are used, MMWatchdog stops restarting but keeps
running the checks. The failure stays visible, and the alert goes out at
`alert_after_failing_for` regardless of the attempts used. If the unit
recovers on its own or by hand, fresh runs of the failing check resolve the
incident.
`sudo mmwatchdog reset-recovery-attempts myapp` clears the check group's recovery
command attempts and the systemd unit restart attempts of the unit that its
`restart_systemd_unit` names, including their minimum-time waits.

With `max_attempts: unlimited`, MMWatchdog never stops restarting.
`min_time_between_attempts` still spaces the restarts, and the failure alert
still goes out at `alert_after_failing_for`.

Failure behavior:

- If the attempt cannot be recorded because state storage is unwritable, the
  restart is withheld; MMWatchdog never takes an action it could forget.
- A failed `systemctl restart` is recorded as the attempt's result and consumes
  the attempt.
- MMWatchdog respects systemd's own start limits and never clears them. A unit that
  systemd refuses to start stays failed and visible.
- Unknown health, from a check that could not run, never triggers a restart.

Restart attempts and results appear in `mmwatchdog events` as
`systemd_unit_restart_requested` and `systemd_unit_restart_result`, under the
check group and check whose failure called for the restart. Status
shows each unit's attempts, minimum time between attempts, and last result.
Restarts send no alerts of their own: the failure alert follows
`alert_after_failing_for`, and the recovery alert follows observed health.

### A stopped unit stays stopped

MMWatchdog does not start a stopped unit. Before a restart, MMWatchdog asks systemd for the
unit's state:

| systemd reports | What it means | What MMWatchdog does |
| --- | --- | --- |
| `active` | The unit runs, and a check in `triggered_by_checks` fails all the same, for example the health request | Restarts the unit |
| `failed` | The program crashed or ended with an error, and systemd did not restart it | Restarts the unit |
| `inactive` | The unit is stopped without a failure: someone or something stopped it on request, or its program ended normally | Leaves the unit alone |

For `inactive`, MMWatchdog withholds the restart and uses no attempt.
MMWatchdog writes one `systemd_unit_restart_withheld` event per failing time
rather than at every run.
The event says:
`no restart: systemd unit myapp.service is stopped, and MMWatchdog never
starts a stopped unit`. No setting allows MMWatchdog to start a stopped unit.

Whoever stopped the unit decides when it runs again: an operator repairing a
database by hand, a package upgrade, or a failover tool. MMWatchdog starting the
application during an operator's repair, a package upgrade, or a failover
could damage data.

The alert still goes out. A stopped unit fails the unit's
[`systemd_unit_state`](../../reference/modules/systemd_unit_state.md) check. After
`alert_after_failing_for`,
MMWatchdog sends the stopped-unit check's alert.
The alert says the unit is stopped and MMWatchdog did not start the unit.
An operator who stops a unit on
purpose and wants no alert turns on a [maintenance window](#maintenance-windows)
before the stop. A unit that should stay stopped permanently is removed from the
node's check groups.

systemd reports a stop on request and a program that ended on its own with
exit status 0 in the same way, as `inactive`. A program that must always run
therefore needs `Restart=always` in its unit file, which makes systemd restart
it after every exit; then `inactive` can only mean that the unit was stopped
on request.

### A machine check that restarts a unit

The restart names its unit, so the check that triggers it does not have to be
a check of that unit. In the `machine_memory` example below, the machine's
memory use triggers a restart of the web server.
Restarting is the usual fix for a program that slowly takes more and more
memory:

```yaml
# /etc/mmwatchdog/check_groups/machine_memory.yml
check_groups:
  machine_memory:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 2m
    recovered_after_healthy_for: 5m

    checks:
      memory_is_below_95_percent:
        node_resource: memory             # MMWatchdog measures the memory of this machine
        fail_when_measurement_above:
          memory_used_percent: 95         # the check fails above 95 percent
        alert_severity: warn

    restart_systemd_unit:
      reset_attempts_after_healthy_for: 10m
      systemd_unit: nginx.service
      triggered_by_checks: [memory_is_below_95_percent]
      restart_after_failing_for: 2m
      max_attempts: 3
      min_time_between_attempts: 10m
```

When memory use exceeds 95 percent for 2 minutes, MMWatchdog restarts
`nginx.service` and sends the `warn` alert at the same moment.
Restarting and alerting happen together because `restart_after_failing_for`
and `alert_after_failing_for` are each 2 minutes.
`nginx.service` runs when the memory check calls for a restart,
so systemd reports the unit as `active` and MMWatchdog restarts the unit. If
`nginx.service` is stopped, MMWatchdog leaves it stopped.

MMWatchdog waits until `systemctl restart` returns. The unit's `TimeoutStopSec` and
`TimeoutStartSec` control the wait for `systemctl restart` to return;
MMWatchdog adds no separate time limit.
`min_time_between_attempts` counts from the return of `systemctl restart`,
so a slow restart gets the full waiting time before another attempt.
If MMWatchdog stopped before the restart returned, such as at a power
loss, the wait counts from the start of that restart.

### Shared systemd unit restart attempts

All check groups whose `restart_systemd_unit` names the same systemd unit
share one set of systemd unit restart attempts, persisted by unit across
daemon restarts and configuration reloads. In the examples above, a second
check group that also restarts `nginx.service` would share the three attempts
with `machine_memory`. Restarts of one unit are serialized, so checks failing
together cannot cause competing restarts. Every check group that names the unit must state identical `max_attempts`,
`min_time_between_attempts`, and `reset_attempts_after_healthy_for`.
Validation refuses a difference and names the configuration file and line
of each conflicting setting. Each check group keeps separate
`triggered_by_checks` and `restart_after_failing_for` settings.
Requiring identical values for the three shared attempt settings avoids
silently changing the operator's chosen values.

Used restart attempts reset only after every check listed in
`triggered_by_checks` by any check group restarting the unit has stayed
healthy for `reset_attempts_after_healthy_for`.
Every failure incident of the listed checks must also have closed.
Alternatively, `mmwatchdog reset-recovery-attempts CHECK_GROUP` resets the
unit's used restart attempts. A brief success does not reset the unit's
used restart attempts.

A failed, unknown, or missed run of any check triggering the unit's restarts
interrupts healthy time, even while state storage is unwritable.
Runs of checks absent from every `triggered_by_checks` list for the unit do
not interrupt the healthy time. Reloads keep consumed attempts.

## Run a recovery command

A check group can run a command when a check has been failing for a set time,
for fixes that are not a systemd unit restart: clearing a cache when a
filesystem passes its limit, removing a stale lock, or rotating a runaway log.

```yaml
# /etc/mmwatchdog/check_groups/root_filesystem.yml
check_groups:
  root_filesystem:
    check_every: 1m
    time_limit_per_run: 5s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m

    checks:
      root_filesystem_is_below_90_percent:
        node_filesystem: /                # MMWatchdog measures the filesystem mounted at /
        fail_when_measurement_above:
          space_used_percent: 90          # the check fails above 90 percent
        alert_severity: warn

    recovery_command:
      reset_attempts_after_healthy_for: 10m
      triggered_by_checks: [root_filesystem_is_below_90_percent]
      script: find /var/cache/myapp -type f -mtime +1 -delete
      run_as_user_account: myapp
      run_after_failing_for: 5m           # run the command after 5 minutes of failing
      time_limit_per_attempt: 2m          # one run of the command may take 2 minutes
      max_attempts: 2
      min_time_between_attempts: 10m
```

In the `root_filesystem` example, the command runs after 5 minutes of
failing. The alert goes out after 10 minutes, only if the cleanup did not
bring the filesystem below the configured limit.

`recovery_command` takes the settings that say how a
[`shell`](../../reference/modules/shell.md) check runs, its own waiting time, and the
recovery attempt settings:

| Setting | Default | Meaning |
| --- | --- | --- |
| `script` | required | Bash with `pipefail`, inline or a path to an existing script |
| `run_after_failing_for` | required | How long a check in `triggered_by_checks` must have been failing before MMWatchdog runs the command |
| `run_as_user_account` | `mmwatchdog` | Account the command runs as; `root` must be named explicitly |
| `run_from_directory`, `env` | `/`, none | Working directory and string environment values |
| `time_limit_per_attempt` | required | Time one run of the command may take before MMWatchdog terminates it |
| `triggered_by_checks` | required | Checks whose failure runs the command |
| `max_attempts`, `min_time_between_attempts`, `reset_attempts_after_healthy_for` | required | As for restarts; `max_attempts` takes a number or `unlimited` |

`run_after_failing_for` works like
[`restart_after_failing_for`](#the-restarts-own-waiting-time): it counts from
the start of the check's failing time, and it can be shorter or longer than
`alert_after_failing_for`.

A heartbeat check group takes no `recovery_command` because no check runs
could observe whether the command fixed the problem.

The command runs noninteractively without the daemon's environment.
MMWatchdog adds `MMWATCHDOG_CHECK_GROUP` and `MMWATCHDOG_CHECK` to name the
failing check that triggered the recovery command.
MMWatchdog adds `MMWATCHDOG_INCIDENT_ID` when the triggering check's incident
is already open. `MMWATCHDOG_INCIDENT_ID` is empty when the command runs
before the alert. The command
receives no check output: text from a monitored endpoint never reaches a
command line or script.

Like a restart, a recovery command requires every condition in the following
list:

- The latest run of a check in `triggered_by_checks` failed at least
  `run_after_failing_for` after the start of the check's failing time.
- Recovery command attempts remain.
- The minimum time between command attempts has passed.
- No maintenance window covers the check group containing the recovery
  command.
- The failing check is not kept quiet by another failing check
  ([one cause, one alert](checks.md#one-cause-one-alert)).

The attempt is recorded durably first, then the command runs with a
`recovery_command_started` event. A `recovery_command_result` event records
the recovery command's exit status and bounded stdout and stderr,
explicitly marked when truncated, like check evidence.
The recovery command's result appears in `mmwatchdog events` and in the
check group's status.

Recovery command attempts belong to the check group, persist like a
unit's systemd unit restart attempts, and reset after
`reset_attempts_after_healthy_for` of health in every `triggered_by_checks`
check, once their failure incidents have also closed.
`min_time_between_attempts` counts from the end of the previous command,
so a long command still gets a full waiting time before another attempt.
If MMWatchdog stopped before the command ended, the wait counts from the
start of that command.
`mmwatchdog reset-recovery-attempts CHECK_GROUP` clears the named check
group's recovery command attempts. If the named check group has a
`restart_systemd_unit`, the command also clears the restart attempts of the
named systemd unit.

The command's success does not end the failing time; fresh runs of the check
do, and another attempt needs a failed run after the command. After the
command ends, MMWatchdog needs fresh runs of every check of the command's
check group, and of every check group that checks or restarts a systemd unit
that the command's check group also names: runs from before the command no
longer describe those units. A nonzero exit,
timeout, or failed launch is recorded as the attempt's result and consumes the
attempt. If the attempt cannot be recorded, the command does not run.

A check group can have both `recovery_command` and `restart_systemd_unit`,
each with its own waiting time. When a failure makes the recovery command
and the restart due at the same moment, the command runs first, and the
restart runs only if a run of the check after the command still fails.
A targeted recovery command can be cheaper than a restart and often makes
the restart unnecessary. After a recovery command, a systemd unit restart
requires a failed check run observed after the command. After a systemd unit restart, a recovery command requires a
failed check run observed after the restart.

The command runs with the privileges of its `run_as_user_account`. The
operator who writes the configuration is trusted; the boundary MMWatchdog keeps is
that monitored output never becomes part of a command.

## Recovery attempt status

Status JSON has two maps: `systemd_unit_restart_attempts`, keyed by systemd unit,
and `recovery_command_attempts`, keyed by check group. Each entry contains
`attempts_used`, `max_attempts`, `last_attempt_at`,
`last_attempt_finished_at` after completion, and `last_result`.
Readable status shows the attempted action's result without claiming recovery:

```text
Systemd unit restarts myapp.service: 1 of 3 attempts used; restart requested
Systemd unit restarts nginx.service: 4 attempts used, unlimited; restart requested
Recovery command root_filesystem: 1 of 2 attempts used; exit 0
```

With `max_attempts: unlimited`, `max_attempts` in status JSON is the text
`unlimited`.

`mmwatchdog reset-recovery-attempts CHECK_GROUP` clears the check group's recovery
command attempts and the systemd unit restart attempts of the unit that its
`restart_systemd_unit` names. A reset is recorded as
`recovery_attempts_reset` in event history.

## Maintenance windows

Planned work should not wake anyone or surprise a teammate. A maintenance
window tells MMWatchdog that work on one or more check groups is planned, until a time
the operator states.

Work that finishes before alerting or a recovery action becomes due needs
no window. A unit that is restarted by hand and is healthy
again before `alert_after_failing_for` has passed never alerts, and a unit
that is stopped by hand is [not started by MMWatchdog](#a-stopped-unit-stays-stopped).
For longer work, turn maintenance on for one check group, for several, or for
every check group on the node. During a maintenance window MMWatchdog keeps running
the checks of the covered check groups. MMWatchdog sends no alert and takes
no recovery action for a covered check group: no systemd unit restart and
no recovery command. Nothing is sent later for the maintenance period:
when the window ends, the next runs of the checks decide.

```text
$ sudo mmwatchdog maintenance on myapp --for 1h --reason "fixing the migration by hand"
myapp is in maintenance until 2026-09-30 15:02 UTC (1h).
  MMWatchdog keeps running the checks of myapp and shows the results in mmwatchdog status.
  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for myapp.
  To end the maintenance early: sudo mmwatchdog maintenance off myapp

$ sudo mmwatchdog maintenance off myapp
myapp is out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.
```

One command covers several check groups, for example the workers of one
application during a deploy:

```text
$ sudo mmwatchdog maintenance on worker_1 worker_2 worker_3 --for 10m --reason "deploy v1.4"
worker_1, worker_2, worker_3 are in maintenance until 2026-09-30 14:12 UTC (10m).
  MMWatchdog keeps running the checks of the check groups in maintenance and shows the results in mmwatchdog status.
  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for the check groups in maintenance.
  To end the maintenance early: sudo mmwatchdog maintenance off worker_1 worker_2 worker_3
```

For a whole-node deployment:

```sh
sudo mmwatchdog maintenance on --every-check-group --for 15m --reason "deploy v1.4"
# deploy
sudo mmwatchdog maintenance off --every-check-group
```

The first command prints:

```text
Every check group on this node is in maintenance until 2026-09-30 14:17 UTC (15m).
  MMWatchdog keeps running the checks of every check group and shows the results in mmwatchdog status.
  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for the check groups in maintenance.
  To end the maintenance early: sudo mmwatchdog maintenance off --every-check-group
```

The second command ends only the whole-node window, and says so:

```text
The maintenance for every check group on this node is off. Alerts, restarts and recovery commands resume after fresh checks. A check group that has its own maintenance window stays in maintenance until that window ends.
```

- `mmwatchdog maintenance on CHECK_GROUP [CHECK_GROUP ...] --for DURATION [--reason TEXT] [--without-maintenance-on-and-off-alerts]`
  covers the named check groups; `--every-check-group` in place of the names
  covers every check group on the node, including check groups a reload adds
  during the window. Each check group has its own window, and one command
  gives every check group it names the same end time. A check group is
  covered while its own window or the whole-node window lasts. Running `on`
  again for a check group replaces that check group's window.
- `--for` is required with `on` and must be positive. The duration given to
  `--for` has no upper limit:
  `--for 30d` is valid, and `d` means days. Every window therefore has an end
  that the operator wrote, and a unit that should stay stopped permanently is
  removed from the node's check groups.
- `mmwatchdog maintenance off CHECK_GROUP [CHECK_GROUP ...]` ends the windows of the
  named check groups; `mmwatchdog maintenance off --every-check-group` ends the
  whole-node window. A window expires on its own and survives a daemon
  restart. Turning off a window that has already expired does nothing.
- On the hub, the whole-node window includes `node_reports`, so the hub sends
  no stopped-reporting alert for any joined node during the window.
  `mmwatchdog maintenance on node_reports --for 1h` covers only
  stopped-reporting alerts on the hub. Each node's own alerts still go out.
  On a hub-outage alerter node, maintenance of `node_reports` covers the
  `node_reports/to_hub` alert.
  There is no fleet-wide maintenance command.
- The checks keep running, and the latest result and transition history are
  kept. MMWatchdog sends no alerts from the check runs, restarts no
  systemd unit, and runs no recovery command. Configuration retirement still
  closes incidents that were already announced.
- By default, one command sends one alert. Turning maintenance on or changing a window
  sends one `maintenance_on` [alert](alerts.md#alert-events) for the command,
  and ending maintenance sends one `maintenance_off` alert. Windows that one
  command opened and that expire at the same moment send one
  `maintenance_off` alert together. The alerts carry the check groups or
  `every_check_group`, the end time, the optional reason, and who ran the
  command: the `sudo` user, or the Linux login user when `sudo` ran from a
  root shell. The `maintenance_off` message says whether maintenance was
  turned off, expired, or ended because its check group was removed.
- When a window ends, the next runs of the checks decide what MMWatchdog announces,
  and the `maintenance_off` alert goes out before any alert that follows from
  those runs. A failure or a recovery that MMWatchdog did not announce during the
  window is not announced afterwards.
- A [heartbeat](checks.md#heartbeats)'s `last_received_at` and `next_due_by`
  stay current evidence.
  When no window covers the heartbeat, MMWatchdog announces a failure
  withheld during maintenance or the recovery of an announced failure that
  recovered during maintenance.
- A reload or restart that removes a check group ends its window, with the
  end alert unless its start and end are not announced.

`--without-maintenance-on-and-off-alerts` on `maintenance on` keeps the window
and the window's events but delivers no maintenance-start or maintenance-end
alert. The suppression includes expiry and removal of a check group.
The window stores the boolean `without_maintenance_on_and_off_alerts`, which
status and event JSON expose.
Status says that maintenance start and end are not announced.
Event creation uses the window's stored choice, so the maintenance end
stays quiet after a daemon restart.

Joined nodes report the window and events to the hub for inspection but
queue no maintenance-start or maintenance-end alert.
The suppression covers hub delivery and direct fallback.

A window without announcements does not delay the end of another window's
fleet alert group. Storing the announcement choice on the window keeps the
window's lifecycle together.
Keeping the announcement choice separately would require another identity and a lookup
whenever a window ends.

`--reason` is optional single-line text of up to 200 bytes. Status shows each
window's end time, reason, and user:

```text
Maintenance on: myapp until 2026-09-30 15:02 UTC, turned on by matt: fixing the migration by hand
```

The whole-node window is named `every_check_group`, which is reserved as a check
group name. A maintenance command takes effect only after it is committed; if
state storage is unwritable, the command fails and the existing windows stay
in place.

### Deploys and recurring windows

MMX's deployment workflows open a whole-node window immediately before a
restart for changed configuration, binary, unit, or modules.
The deployment workflows' `activate` section uses a
`shell` task with `only_when_tracked_inputs_changed: true` and an `id`,
before the `systemd_service` task. The command is
`mmwatchdog maintenance on --every-check-group --for 15m --reason "mmwatchdog deploy"`.

The `verify` section waits for the socket and a successful `mmwatchdog config --json`.
Then `verify` runs `mmwatchdog maintenance off --every-check-group` only if
the opening task reported `changed`.
The `activate` and `verify` sections share the task results in MMX.

An unchanged run opens no window and sends no maintenance alerts. A first
install has no running MMWatchdog and opens none.
Rejected candidates open no window.

The conditional opening avoids scheduled-deploy noise while keeping alerts
for real restarts. The 15-minute expiry restores alerting if the deployment
process exits unsuccessfully or is interrupted before verification.
The workflows announce windows by default; frequent
automated deploys may add `--without-maintenance-on-and-off-alerts`.
In a fleet, the hub groups announced windows by reason and user and sends
no silence alert during a node's reported whole-node window; see
[maintenance and deploys in a fleet](fleet.md#maintenance-and-deploys-in-a-fleet).

For recurring work, such as a nightly backup that loads the database, a
systemd timer runs the maintenance command:

```ini
# /etc/systemd/system/db-backup-window.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/mmwatchdog maintenance on postgres --for 45m --reason "nightly backup"
```

A matching `.timer` unit sets the schedule. MMWatchdog never infers planned work on its
own.

## Recovery decisions

The decisions in the following table are settled. Revisit a decision only
with a concrete workflow that the decision does not support.

| Decision | Reason |
| --- | --- |
| Systemd supervises processes; MMWatchdog never starts or stops services and never clears start limits. | One supervisor per process; MMWatchdog only adds bounded, observed recovery. |
| MMWatchdog does not start a unit that systemd reports as stopped, and no setting changes that. | Whoever stopped the unit decides when it runs again, and MMWatchdog starting an application in the middle of an operator's repair could damage data. systemd reports a stop on request and a normal exit the same way, and `Restart=always` in the unit file covers every exit of the program. |
| Restarting the systemd unit is the one built-in action; other fixes are a recovery command. | One way per job, and any fix a shell can express needs no new built-in. |
| Every restart names its unit, and any check of the check group that can fail can trigger it. | A setting is written on the thing it affects, and the cause of a restart can be measured anywhere on the node, such as the machine's memory. |
| The restart and the recovery command each have their own waiting time, separate from the alert's. | Some failures should trigger a systemd unit restart or recovery command before alerting. MMWatchdog then reports the failure only if the recovery action does not help. Other failures should be reported before anything is restarted. One check's runs support recovery actions before alerts and alerts before recovery actions. |
| `max_attempts` takes a number or `unlimited`. | A machine that nobody watches should keep trying, and `min_time_between_attempts` still spaces the restarts. A fixed upper bound would have no concrete reason. |
| Systemd unit restart attempts are per unit and shared by every check group whose restart names that unit. | Checks failing together must not multiply restarts. |
| Health must be observed after an action; a command succeeding proves nothing. | A restarted unit can still be broken. |
| Actions need a durable record before they run. | A daemon restart must never forget an action and repeat it past its maximum attempts. |
| Waiting times, spacing between recovery attempts, and healthy reset periods use durations, not check cycles. | A duration means the same thing whatever the check interval. |
| No separate alert when recovery attempts run out. | The failure alert goes out at `alert_after_failing_for` whatever the attempts did, reminders repeat it, and status shows that all recovery attempts are used. |
| No dependency graph between check groups: MMWatchdog orders nothing, starts nothing, and stops nothing because of another check. A check can only be kept quiet while its cause fails. | systemd already orders units and restarts them together. MMWatchdog prevents duplicate alerts and restarts that cannot help, because useless restarts consume attempts. MMWatchdog sends no alert and takes no recovery action for a check while a check named in `no_alerts_or_recovery_actions_while_these_checks_fail` remains in failing time. See [one cause, one alert](checks.md#one-cause-one-alert). |
| Maintenance windows always expire: `--for` is required and has no upper limit. | A forgotten window can never silence a node permanently, because every window has an end that the operator wrote. A fixed maximum duration would add no protection against permanent silence. |
| One maintenance command takes several check group names and sends one alert. | Several instances of one program are several check groups, and a deploy that touches all of them is one piece of planned work. |
| The hub does not open or end maintenance windows on other nodes. | One way per job: an MMX run, or an ssh loop, runs `mmwatchdog maintenance on` on each node. The [fleet decisions](fleet.md#fleet-decisions) give the reasons in full. |
| Recurring windows are a systemd timer running the maintenance command. | Systemd already schedules; MMWatchdog never infers planned work. |
| No separate controls to suppress alerts but not actions, or the reverse. | A maintenance window marks planned work on the check groups covered by the window. |
| No built-in AI interpretation or autonomous AI recovery. | Recovery stays deterministic. The operator's agent uses the `mmwatchdog` commands available to a human operator. |

## Beyond release 1

- Recovery commands triggered by recovery or by cannot-check incidents.

Recovery triggers, attempt budgets, and command time limits are required.
A reader can see which checks cause an action and how long MMWatchdog may keep acting.
The configuration never selects every check or an attempt budget by omission.
