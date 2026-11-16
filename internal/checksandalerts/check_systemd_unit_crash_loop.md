# `systemd_unit_crash_loop`

`systemd_unit_crash_loop` counts how often systemd restarted one `.service`
unit on its own, and fails when that happened too often within a time. MMWatchdog
reads systemd's own count of automatic restarts with `systemctl show`.

A unit that crashes and that systemd starts again by its `Restart=` setting
is `active` at almost every run, so a
[`systemd_unit_state`](check_systemd_unit_state.md) check of it stays
healthy. A crash-loop check catches such a unit. Use both checks for a unit
that must run: `systemd_unit_state` for a unit that stopped or failed, and
`systemd_unit_crash_loop` for a unit that keeps crashing. For a unit whose
automatic restarts are intentional, omit the crash-loop check.

## When the check fails, and when it cannot check

The check fails while the restarts that MMWatchdog counted within
`automatic_restarts_within` are more than `automatic_restarts_above`. The
crash-loop check is healthy again once the time window no longer holds
more than `automatic_restarts_above` restarts. With
`automatic_restarts_above: 2`, a single crash that systemd repairs shows in
status without an alert.

The message gives the count in the window. When the window holds at least one restart and a
check run saw an exit within the window, the message also gives the unit's
last exit:

| Case | Message |
| --- | --- |
| The first run, which has no earlier count to compare with | `first look at systemd unit myapp.service: MMWatchdog counts the unit's automatic restarts between check runs, from the current run on; systemd's own count of the unit's automatic restarts (NRestarts) is 5` |
| No restart within 1 hour | `0 automatic restarts in the last hour` |
| One restart | `1 automatic restart in the last hour` |
| Three restarts, the last after the kernel killed the program for lack of memory | `3 automatic restarts in the last hour; last exit: oom-kill (status 9)` |
| A window that is not 1 hour, such as `automatic_restarts_within: 30m` | `2 automatic restarts in the last 30m` |

The last exit is systemd's `Result` and `ExecMainStatus`. systemd clears
`Result` and `ExecMainStatus` when the unit runs again. MMWatchdog therefore
keeps the latest exit that a check run saw within the window. If no check
run saw an exit within the window, the message omits the last exit.

The measurement `automatic_restarts` holds the count of automatic restarts
within the window, and is 0 at the first run.

MMWatchdog cannot check, and the run is unknown, in three cases:

- systemd does not know the unit. The message is
  `systemd unit myapp.service not found`, as for every
  [check of a systemd unit](../../docs/reference/modules/systemd_unit_state.md).
- `systemctl` fails. The message starts with `systemd inspection:`.
- systemd gives no valid restart count. The message is
  `systemd returned no valid NRestarts for myapp.service`.

If the check stays unknown for `alert_after_failing_for`, MMWatchdog opens a
cannot-check incident, as for every check
([the failing time](../../docs/how-to/decide-when-a-check-alerts.md)).

## Examples

### Alert when the unit keeps crashing

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    checks:
      myapp_service_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active
        alert_severity: sos
      myapp_is_not_crash_looping:
        systemd_unit_crash_loop:
          systemd_unit: myapp.service
          automatic_restarts_above: 2
          automatic_restarts_within: 1h
        alert_severity: warn
        raise_to_sos_after_failing_for: 30m
```

On the node, `myapp.service` has `Restart=always` in its unit file, and its
program crashes every few minutes.

1. systemd starts the program again after each crash, so
   `myapp_service_is_running` stays healthy at almost every run.
2. At each run, MMWatchdog reads systemd's count of automatic restarts. At the run
   that sees the third restart within the hour, `myapp_is_not_crash_looping`
   fails with `3 automatic restarts in the last hour; last exit: exit-code
   (status 1)`.
3. After 1 minute of failing, MMWatchdog sends a `warn` alert. If the check is still
   failing 30 minutes after its first failed run, MMWatchdog raises the alert to
   `sos` ([alert severity](../../docs/how-to/decide-when-a-check-alerts.md)).
4. When someone fixes the program, the crashes stop. Counted restarts leave
   the window once an hour old. At the first run with 2 or fewer restarts
   in the window, the check becomes healthy. After the check stays healthy
   for 30 seconds, MMWatchdog sends the recovery alert.

## How it works

1. At each run, MMWatchdog runs `systemctl show` for the unit as MMWatchdog itself, and reads
   `NRestarts`, `Result`, and `ExecMainStatus`. `NRestarts` counts only
   the restarts that systemd made by the unit's `Restart=` setting.
2. MMWatchdog compares `NRestarts` with its value at the run before.
   MMWatchdog counts the increase in `NRestarts` as restarts at the time of
   the current check run. A decrease means systemd reset its counter, for
   example after an operator restarted the unit. MMWatchdog counts the new
   value as automatic restarts since systemd reset the `NRestarts` counter.
   The operator's restart itself does not count.
3. When `Result` is not `success` or `ExecMainStatus` is not `0`, MMWatchdog keeps
   `Result` and `ExecMainStatus` as the last exit, such as `oom-kill (status 9)`.
4. MMWatchdog drops the restarts that are `automatic_restarts_within` old or older,
   and counts the rest.

A restart counts at the run that sees it, not at the moment systemd made it.
The first run after the check starts only reads the count and counts no
restart. The crash-loop check is healthy at the first run, with the
measurement `automatic_restarts` at 0. The first run's message identifies
the run as MMWatchdog's first look and gives systemd's current count
(`NRestarts`). The initial `NRestarts` value covers a time before MMWatchdog
looked, so MMWatchdog does not count the initial value against
`automatic_restarts_above`.

**The history of restarts is kept between runs.** The restarts within the
window, the last `NRestarts` value, and the last exit are the check's
history. MMWatchdog keeps the restart history in MMWatchdog's state,
together with the check's incidents. `mmwatchdog status --json` shows the
restart history as `module_state` of the check. Only a scheduled run
replaces the history.

- After MMWatchdog itself restarts, counting continues from the kept history. The
  restarts that systemd made in the meantime count at the first run, so an
  open incident stays open.
- A reload that changes the unit or the crash-loop settings starts the
  check afresh with no history. A timing-only reload keeps the history,
  because the check still observes the same unit and restart counters.
- `mmwatchdog check-now` counts on a copy of the daemon's history and does not change
  it. `mmwatchdog check-now CHECK_GROUP --without-daemon --config-dir DIR` has no
  history: each of its runs is a first look, which only reads the count and
  says so, with systemd's own count (`NRestarts`) in the message. To see the
  restarts that MMWatchdog counted, run `mmwatchdog check-now` with the daemon, or look at
  `mmwatchdog status`.

**Recovery actions.** A crash-loop check takes the check group's timings like
every other check. A recovery command must state `triggered_by_checks`. Name the crash-loop
check in `triggered_by_checks` only when the command can fix the cause of the crash loop.


A crash-loop check can also be named in the `triggered_by_checks` of a
[restart of the unit](../../docs/how-to/restart-a-service-automatically.md#steps-to-restart-a-systemd-unit), like any
check that can fail. A restart does not empty the window: the check stays
failed until the counted restarts are `automatic_restarts_within` old, so MMWatchdog
restarts the unit again at each due time until the restart attempts are used.

## Errors and what they mean

| Message | What to do |
| --- | --- |
| `check_groups.myapp.checks.myapp_is_not_crash_looping.systemd_unit_crash_loop.automatic_restarts_above: is required: a whole number of restarts, such as 2` | Write `automatic_restarts_above`. `0` fails the check at the first automatic restart. |
| `...systemd_unit_crash_loop.automatic_restarts_above: cannot be negative` | Write a whole number from 0. |
| `...systemd_unit_crash_loop.systemd_unit: is required: the systemd unit this check is about, such as myapp.service` | Name the unit to watch. |
| `...systemd_unit_crash_loop.systemd_unit: must be a systemd .service unit name, such as myapp.service` | Write the full name of a `.service` unit. The name holds only letters, digits, and the characters `_@.:\-`. |
| `...systemd_unit_crash_loop.automatic_restarts_within: is required: a positive duration such as 1h` | Write the time window, such as `1h` or `30m`. |
| `check_groups.myapp.checks.myapp_is_not_crash_looping.fail_when_measurement_above: the module systemd_unit_crash_loop records its measurements but takes no limit on them` | The check states `fail_when_measurement_above` or `fail_when_measurement_below`. The measurement `automatic_restarts` is only recorded; `automatic_restarts_above` is the limit. Remove `fail_when_measurement_above` or `fail_when_measurement_below`, whichever the check states. |
| `check_groups.myapp.recovery_command.triggered_by_checks: is required: the checks whose failure calls for the command; add triggered_by_checks: [check_name]` | The check group's only check that can fail is a crash-loop check. Name the crash-loop check in `triggered_by_checks` if the command should run for the crash-loop check's failure. |
| `systemd returned no valid NRestarts for myapp.service` (unknown) | systemd gave no restart count for the unit. Check the unit with `systemctl show --property=NRestarts myapp.service`. |

## Tested on

- **Go tests:** `check_test.go` `TestRestartsCountAutomaticRestartsWithinWindow`
  and `TestRestartsReadSystemdCountAndLastExit` (counting within the window,
  the message of the first look, a decrease continuing from the new value,
  recovery once the window empties, the last exit kept after systemd clears it, unknown for a missing
  unit or count); `config_test.go` `TestSystemdUnitChecks` (all three
  settings required, a name that is not a `.service` unit refused, and
  `recovery_command.triggered_by_checks` required for a check group whose
  only check is a crash-loop check); `daemon_test.go`
  `TestRestartsConditionFollowsScheduledHistory` (incident and evidence,
  `mmwatchdog check-now` on a copy of the history, an unchanged reload keeping it,
  and a restart of MMWatchdog continuing from the kept history without a recovery).
- **Suites on real nodes:** the `resources` suite (`lifecycle.py`): a real
  unit with `Restart=always` fails its crash-loop check with the count and
  `exit-code (status 3)` as its last exit, stays failed across a restart of
  MMWatchdog, and recovers once the window empties.
- The `resources` suite passed on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution).
  The [validation record](../../docs/changing-mmwatchdog/validation.md) has the latest run.

## Why it works this way

- **A lower counter still holds automatic restarts since its reset.** Ignoring
  the new value would hide a unit that began crashing again between runs.

- **The check counts systemd's `NRestarts`.** systemd counts in `NRestarts`
  only the restarts that systemd made by itself, so a restart by MMWatchdog
  or by an operator never looks like a crash.
- **A limit with a time window, not a single crash.** A single crash that
  systemd repairs is worth seeing in status, not an alert. Repeated crashes
  within a time are the failure.
- **`systemd_unit`, `automatic_restarts_above`, and
  `automatic_restarts_within` are required, with no default.** A reader sees
  on the check itself how many restarts within which time are too many.
- **The history is kept with the check's state.** A restart of MMWatchdog must not
  forget the restarts that it counted, or an open incident would close while
  the unit still crashes.
- **The window counts on the system clock.** Each restart time keeps only
  the restart time's system clock reading, so a step of the clock moves
  the restart times. MMWatchdog's own waits do not change with the
  system clock
  ([the system clock](../../docs/changing-mmwatchdog/design/operation.md#the-system-clock)). A
  forward step can drop previously counted restarts and delay the alert
  by at most one window.
  A backward step keeps the counted restarts longer.
- **A crash-loop check does not run a recovery command by default.**
  A unit in a crash loop is not fixed by acting on the unit again.
  If a recovery command can fix the cause, add the crash-loop check's name to `recovery_command.triggered_by_checks`.
- **`mmwatchdog check-now` works on a copy.** Looking at a check never changes what
  its next scheduled run sees.
- **A first look says that it is one.** A run without an earlier count,
  such as the first run of a new check or every run of `mmwatchdog check-now
  --without-daemon`, cannot know how many restarts fell within the window.
  `0 automatic restarts` would read as if MMWatchdog had looked and found none, so
  the message says that counting starts now and gives systemd's own count,
  which an operator can compare by hand with a later value of that count.
