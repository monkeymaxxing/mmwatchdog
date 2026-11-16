# `systemd_unit_commands`

`systemd_unit_commands` watches one program of a systemd unit that runs
several. Some units start several programs, and when one of them ends,
systemd still reports the unit as `active`, so a
[`systemd_unit_state`](check_systemd_unit_state.md) check stays healthy.
A `systemd_unit_commands` check is healthy while a command of the unit
contains the text in
`fail_when_no_command_contains`.
The matching text can be the program's name. The check fails when no command
of the unit contains the value of `fail_when_no_command_contains`.

`systemctl status myapp.service` lists the commands that the unit runs:

```text
├─1201 /usr/local/bin/myapp-web --port 8080
└─1215 /usr/local/bin/myapp-worker --queue mail
```

The check looks at exactly this list: the commands of the named unit, and of
no other program on the node. The text can be any part of a command: the
program's name, or one of its arguments.

Use `systemd_unit_state` for the unit as a whole. Give each program that
matters a check of its own with `systemd_unit_commands`. To count the
processes of a unit, use
[`systemd_unit_resource`](check_systemd_unit_resource.md) with
`resource: processes`, which reads the processes of the named unit.

## When the check fails, and when it cannot check

The check is healthy while one command of the unit contains the text, with
the message `systemd unit myapp.service runs
/usr/local/bin/myapp-worker --queue mail (process 1215)`. The message shows
the command with the lowest process ID that contains the text, cut after 120
characters.

The check fails when:

- **the program ends** and the rest of the unit keeps running: `systemd unit
  myapp.service runs no command that contains myapp-worker`. The body of the
  alert lists the commands that the unit does run, under the line `the
  commands that the unit runs:`. Compare `fail_when_no_command_contains`
  with the listed commands to find typing mistakes. The alert body lists
  at most 11 commands, each cut after 120 bytes and followed by its
  process ID;
- **systemd reports a state other than `active` or `reloading`.** A unit that
  does not run runs no command, so MMWatchdog fails the check with the unit's state:
  `systemd unit myapp.service runs no command that contains
  myapp-worker: systemd unit myapp.service is inactive`, or `is failed`,
  `is activating`, or `is deactivating`. MMWatchdog counts a unit as running
  while systemd reports `active` or `reloading`, and reads the commands
  ([checks of a systemd unit](../../docs/reference/modules/systemd_unit_state.md));
- **the unit is active and runs no program at all.** systemd keeps such a
  unit active when the unit says so (`RemainAfterExit=yes`), as a unit made
  from an old init script does.

MMWatchdog cannot check, and the run is unknown, when systemd does not know the unit
(`systemd unit myapp.service not found`, the message of every check of a
systemd unit), when `systemctl` fails (a message
that starts with `systemd inspection:`), when systemd names a control group
with `..` in its path, or when the node has no control group file system at
`/sys/fs/cgroup`. An unknown run opens a cannot-check
incident after `alert_after_failing_for`, as for every check
([the failing time](../../docs/how-to/decide-when-a-check-alerts.md)).

## Examples

### Watch the worker program of a unit

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 30s
    checks:
      myapp_service_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active
        alert_severity: sos
      myapp_worker_is_running:
        systemd_unit_commands:
          systemd_unit: myapp.service
          fail_when_no_command_contains: myapp-worker
        alert_severity: sos
        no_alerts_or_recovery_actions_while_these_checks_fail: [myapp/myapp_service_is_running]
```

On the node, every 30 seconds, MMWatchdog reads the commands of `myapp.service`.
While `/usr/local/bin/myapp-worker --queue mail` runs, the check
`myapp_worker_is_running` is healthy.

1. The worker program ends, and `myapp-web` keeps running. At its next run
   the check fails with `systemd unit myapp.service runs no command that
   contains myapp-worker`.
2. After 5 minutes of failing, MMWatchdog sends an `sos` alert. The `sos`
   alert's body lists
   `/usr/local/bin/myapp-web --port 8080 (process 1201)`.
3. When someone stops `myapp.service`, `myapp_service_is_running` and
   `myapp_worker_is_running` fail. The example's last line suppresses alerts
   and recovery actions for `myapp_worker_is_running` while
   `myapp_service_is_running` fails.
   MMWatchdog sends one alert for the stopped unit, from
   `myapp_service_is_running`
   ([one cause, one alert](../../docs/how-to/stop-alerts-that-follow-another-failure.md)).

A second program of the unit gets a second `systemd_unit_commands` check.

To restart the unit when the worker ends, name `myapp_worker_is_running`
in the `triggered_by_checks` of the check group's
[`restart_systemd_unit`](../../docs/how-to/restart-a-service-automatically.md#steps-to-restart-a-systemd-unit). While
the unit is stopped in the example, MMWatchdog suppresses alerts and
recovery actions for `myapp_worker_is_running`. MMWatchdog therefore does
not restart the unit because of `myapp_worker_is_running`.

## How it works

1. At each run, MMWatchdog asks systemd for the unit's `ActiveState` and its
   `ControlGroup` with `systemctl show`, as MMWatchdog itself.
2. systemd keeps the processes of a unit in one control group of the
   kernel. MMWatchdog reads the process IDs of the unit's control group and
   every nested control group, without a depth limit, from the files
   `cgroup.procs` below `/sys/fs/cgroup`. On a node with the older
   arrangement of control groups, MMWatchdog reads systemd's tree in
   `/sys/fs/cgroup/unified` or `/sys/fs/cgroup/systemd`.
3. MMWatchdog reads the command line of each process from `/proc`. A process without
   a command line, such as a thread of the kernel, has its name in brackets,
   as `systemctl status` shows it. A process that ended in the meantime is
   left out.
4. The check is healthy when one command contains the text.

An active unit without a control group runs no command. If the kernel removes
the control group with the unit's last process after systemd answers but
before MMWatchdog reads the control group, the run is unknown.

**Choose matching text unique to the program you want to watch.** The check
is healthy while any command of the unit contains the text. A unit that
starts its programs from a shell command, such as
`sh -c 'myapp-web & myapp-worker & wait'`, also runs
that shell, and the shell's own command line holds both names.

## Errors and what they mean

| Message | What to do |
| --- | --- |
| `check_groups.myapp.checks.myapp_worker_is_running.systemd_unit_commands.systemd_unit: is required: the systemd unit this check is about, such as myapp.service` | Name the unit that runs the program. |
| `...systemd_unit_commands.systemd_unit: must be a systemd .service unit name, such as myapp.service` | Write the full name of a `.service` unit. The name holds only letters, digits, and the characters `_@.:\-`. |
| `...systemd_unit_commands.fail_when_no_command_contains: is required: a text that one of the unit's commands contains, such as the name of the program` | Write the text. A text of only spaces counts as none. |
| `systemd unit myapp.service runs no command that contains myapp-worker`, with the commands of the unit in the body | The program does not run. If the program runs, compare `fail_when_no_command_contains` with the commands in the body and with `systemctl status myapp.service`. |
| `systemd gave a control group for myapp.service that MMWatchdog does not read: ...` (unknown) | systemd named a control group with `..` in its path, which MMWatchdog refuses to follow. |
| `MMWatchdog cannot read the control group of myapp.service below /sys/fs/cgroup` (unknown) | The node has no control group file system that MMWatchdog can read at `/sys/fs/cgroup`. |

## Tested on

- **Go tests:** `processes_test.go` `TestTheProcessesOfASystemdUnit` (the
  processes of the unit's control group and of the groups inside it and of no
  other unit, a command found by its name and by an argument, the commands
  listed when none matches, a reloading unit read, a stopped and a
  deactivating unit failed, the older
  arrangement of control groups, a unit that systemd keeps active without a
  process, and a control group that is gone) and
  `TestTheInputsOfASystemdUnitCommandsCheck`; `check_test.go`
  `TestEveryCheckOfAnUnknownSystemdUnitSaysNotFound` (an unknown unit).
- **Suites on real nodes:** the `node` suite (`node_scenarios.py`): a real
  unit with two programs. The check fails when one program is killed and
  names the surviving program in the alert, and recovers when the killed
  program is started again. When the unit stops, the program check fails,
  but MMWatchdog suppresses alerts and recovery actions for the program
  check.
  The unit's `systemd_unit_state` check reports the stop.
- The `node` suite passed on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution).
  The [validation record](../../docs/changing-mmwatchdog/validation.md) has the latest run.

## Why it works this way

- **An incomplete process list is unknown.** A command in an unreadable or
  deeply nested control group must not be reported as absent.

- **The check looks only among the commands of the named unit.** The text
  cannot match a program of another unit. `systemctl status` shows the
  operator the commands of the named unit. A search of the whole node, as
  `pgrep` does, would find a program with the watched program's name
  elsewhere.
- **The check compares a text with the whole command, not a program name.**
  Some services run every program as the same interpreter, such as `perl`,
  each with a different script. Matching text in a script argument
  distinguishes the interpreter processes.
- **`systemd_unit_commands` and `fail_when_no_command_contains` use
  "command", the word that `systemctl status` shows.** An
  operator does not need to know the kernel's words, process and command
  line, to write the check.
- **A failed check lists the commands of the unit.** Compare
  `fail_when_no_command_contains` with the commands in the alert to find
  typing mistakes.
- **A unit that does not run fails the check, not unknown.** A unit that
  systemd reports in another state than `active` or `reloading`, such as a
  stopped one or one that systemd is stopping, runs no command, which is a
  fact that MMWatchdog observed. The unit's own `systemd_unit_state` check is the
  cause, and
  `no_alerts_or_recovery_actions_while_these_checks_fail` keeps the `systemd_unit_commands` check
  quiet while the `systemd_unit_state` check fails. Every check of a systemd unit counts the
  same states as running
  ([checks of a systemd unit](../../docs/reference/modules/systemd_unit_state.md)).
