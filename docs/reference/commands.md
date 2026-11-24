# Commands reference

This page lists every `mmwatchdog` command with its flags, its exit codes,
and the fields of its `--json` output. You run `mmwatchdog` on a **node**,
a server that MMWatchdog watches. On each node, one `mmwatchdog` service
runs the checks and sends the alerts: this page calls it the **node
monitoring process**. Most commands ask that process through its socket,
so they need `sudo`. To read what a node sees step by step, see
[See what is happening](../how-to/see-what-is-happening.md).

The states that `mmwatchdog status` shows for each check are in
[Check states](#check-states), and the exit codes of all commands are in
[Exit codes](#exit-codes).

## Commands at a glance

"Asks the service" means that the command talks to the node monitoring
process through its socket, so the command fails with exit code 4 while
the service is stopped.

| Command | What the command does | Needs `sudo` | Asks the service |
|---|---|---|---|
| [`mmwatchdog status [CHECK_GROUP]`](#mmwatchdog-status) | Shows the state of each check, and whether alerts reach their destinations. | Yes | Yes |
| [`mmwatchdog events [CHECK_GROUP[/CHECK]]`](#mmwatchdog-events) | Shows what happened and when: failures, recoveries, restarts, maintenance, notices. | Yes | Yes |
| [`mmwatchdog check-now CHECK_GROUP`](#mmwatchdog-check-now) | Runs the checks of one check group now and shows the results. | Yes | Yes, except with `--without-daemon` |
| [`mmwatchdog readings CHECK_GROUP [CHECK]`](#mmwatchdog-readings) | Shows the measurement history of a check group or of one check. | Yes | No |
| [`mmwatchdog validate`](#mmwatchdog-validate) | Checks the configuration, and on the node the files and accounts that it names. | Yes, on the node | No |
| [`mmwatchdog reload`](#mmwatchdog-reload) | Reads the configuration again and uses it, if all of it is valid. | Yes | Yes |
| [`mmwatchdog config`](#mmwatchdog-config) | Prints the configuration that the service uses, as JSON. | Yes | Yes |
| [`mmwatchdog maintenance on` and `off`](#mmwatchdog-maintenance) | Starts or ends a maintenance window: no alerts, restarts, or recovery commands for the named check groups. | Yes | Yes |
| [`mmwatchdog reset-recovery-attempts CHECK_GROUP`](#mmwatchdog-reset-recovery-attempts) | Lets MMWatchdog restart a systemd unit or run a recovery command again after the attempts ran out. | Yes | Yes |
| [`mmwatchdog send-notice`](#mmwatchdog-send-notice) | Sends a one-time message through the alert destinations. | Yes | Yes |
| [`mmwatchdog heartbeat CHECK_GROUP`](#mmwatchdog-heartbeat) | Reports from a scheduled job that the job succeeded. | No | Yes, through the heartbeat socket, or a receiver on another machine |
| [`mmwatchdog fleet`](#mmwatchdog-fleet) | On the hub: shows every node of the fleet, one node, or one node group. | Yes | Yes |
| [`mmwatchdog fleet readings`](#mmwatchdog-fleet-readings) | On the hub: shows the measurement history of a joined node or of a node group. | Yes | No |
| [`mmwatchdog fleet findings CHECK_GROUP/CHECK`](#mmwatchdog-fleet-findings) | On the hub: lists every node with a finding of one check, and each node's exact finding. | Yes | Yes |
| [`mmwatchdog fleet set-aside-findings`](#mmwatchdog-fleet-set-aside-findings) | On the hub: lists the findings set aside, and the nodes that each one covers now. | Yes | Yes |
| [`mmwatchdog fleet join-code`, `rotate-join-code`, `forget`](#mmwatchdog-fleet-join-code-rotate-join-code-and-forget) | On the hub: prints or changes the join code, or removes a node from the fleet. | Yes | Yes |
| [`mmwatchdog describe-module [NAME]`](#mmwatchdog-describe-module) | Lists the modules that the node has, or prints the page of one module. | No | No |
| [`mmwatchdog module-usage [NAME]`](#mmwatchdog-module-usage) | Shows which modules the configuration uses, and where. | No | No |
| [`mmwatchdog run-starlark-module MODULE_DIR`](#mmwatchdog-run-starlark-module-and-module-json) | Runs the script of a module once, to try the module by hand. | No | No |
| [`mmwatchdog module-json ...`](#mmwatchdog-run-starlark-module-and-module-json) | Helper for a module that is an executable of your own. | No | No |
| [`mmwatchdog daemon`](#mmwatchdog-daemon) | Is the node monitoring process itself. systemd starts it. | Yes | It is the service |
| [`mmwatchdog version`](#mmwatchdog-version-and-help) | Prints the version. | No | No |
| [`mmwatchdog help`](#mmwatchdog-version-and-help) | Prints every command form and the exit codes. | No | No |

## Flags that several commands share

| Flag | Used by | What the flag does |
|---|---|---|
| `--json` | `version`, `validate`, `status`, `config`, `events`, `check-now`, `reload`, `readings`, `fleet`, `fleet NODE`, `fleet readings`, `fleet findings`, `fleet set-aside-findings`, `fleet join-code`, `fleet rotate-join-code`, `describe-module`, `module-usage` | Prints JSON on standard output instead of text. Each command's entry lists the fields. The exit code stays the same. |
| `--socket PATH` | `status`, `config`, `events`, `check-now`, `reload`, `maintenance`, `reset-recovery-attempts`, `send-notice`, `fleet` (not `fleet readings`) | The socket of the node monitoring process. The default is `/run/mmwatchdog/mmwatchdog.sock`. Give the flag only when the node setting `socket` names another path, because the command does not read the configuration to find the socket. |
| `--config-dir DIR` | `validate`, `check-now --without-daemon`, `describe-module`, `module-usage`, `readings`, `fleet readings`, `daemon` | The configuration directory. The default is `/etc/mmwatchdog`. `check-now` refuses the flag without `--without-daemon`. |
| `--official-extra-module-dir DIR`, `--external-module-dir DIR` | `validate`, `describe-module`, `module-usage` | Look for the official extra modules, or for the modules of your own, in `DIR` instead of the directories that the node settings `official_extra_module_dir` and `external_module_dir` name. A relative `DIR` starts at the current directory. An empty `DIR` is refused. |
| `--last DURATION`, `--since TIME`, `--until TIME` | `events`, `fleet NODE`, `readings`, `fleet readings` | Choose a time span: see [Time spans](#time-spans). |

A flag that a command does not have is refused with exit code 4, and the
command prints the flags that it has:

```text
$ mmwatchdog status --bogus
flag provided but not defined: -bogus
Usage of status:
  -json
    	JSON output
  -socket string
    	Unix socket of the node monitoring process (default "/run/mmwatchdog/mmwatchdog.sock")
mmwatchdog: flag provided but not defined: -bogus
```

`mmwatchdog status --help`, and `--help` after most other commands,
prints the same list of flags and exits with 0. A flag can stand before or after the check group's name. A flag's value
goes after a space or after `=`: `--last 2h` and `--last=2h` are the same.

### Time spans

`events`, `fleet NODE`, `readings`, and `fleet readings` take the same
three flags for a time span. Each span includes its start and its end.
Times in the output are UTC.

| Flag | What the flag does |
|---|---|
| `--last DURATION` | The span from `DURATION` ago until now. `DURATION` is positive and has a unit: `30s`, `5m`, `26h`, or `7d`. |
| `--since TIME` | The span from `TIME`, an RFC 3339 time such as `2026-10-07T14:00:00Z`, the form that JSON prints. `--since` and `--last` cannot be used together. |
| `--until TIME` | The end of the span. It needs `--since`. Without `--until`, the span ends when the command starts. |

Without a time flag, `readings` and `fleet readings` show the last hour,
and `events` and `fleet NODE` show the latest 100 events. A wrong time is
refused with exit code 4:

```text
$ sudo mmwatchdog events --last 5x
mmwatchdog: --last needs a positive duration, such as 30s, 5m, 26h, or 7d
$ sudo mmwatchdog events --until 2026-11-15T10:00:00Z
mmwatchdog: --until needs --since; state the start of the time span
```

## mmwatchdog status

`mmwatchdog status` shows each check's state and the result of its latest
run. The last lines say whether alerts reach their destinations, and
whether MMWatchdog is using a recovery action or a maintenance window.

```text
$ sudo mmwatchdog status
commands_page_example/a_check_with_a_long_name_so_that_its_alert_title_gets_shortened_ ok           command exited 0
commands_page_example/marker_file_exists confirming_failure the latest run is healthy, but runs have failed since 10:28:43 UTC; the check counts as failing until it has been healthy for 30s (recovered_after_healthy_for), so that a check that fails on and off still alerts: command exited 0
machine/cpu_use_is_recorded  ok           cpu_used_percent is 0.33 over 30.099s
machine/memory_is_below_90_percent ok           memory_used_percent is 19.3 (fails above 90)
machine/root_filesystem_is_below_85_percent ok           space_used_percent is 9.75 (fails above 85)
machine/root_filesystem_is_below_95_percent ok           space_used_percent is 9.75 (fails above 95)
node_reports/to_hub          ok           reports reach the hub
Alert deliveries: 0 waiting, 0 dropped
Recovery command commands_page_example: 1 of 1 attempts used; exit 0
Fleet hub: 203.0.113.11:8445 as web01; last report 2026-11-15T10:29:03Z
```

Each check has one line: `CHECK_GROUP/CHECK`, its
[state](#check-states), and the message of its latest run. In this
example, the node `web01` has joined a hub, so the check
`node_reports/to_hub` says whether the node's reports reach the hub. The
check `marker_file_exists` failed one run, and a recovery command fixed
the cause, so the check waits 30 seconds before it counts as healthy.

The lines below the checks:

| Line | What the line says |
|---|---|
| `Alert deliveries: 0 waiting, 0 dropped` | How many alerts wait for delivery, and how many MMWatchdog dropped without delivery since `state.db` was made. One delivery is one alert to one destination. A drop line adds the time of the first and the newest drop, such as `2 dropped since 2026-10-25 09:10 UTC, the newest at 2026-10-27 17:52 UTC`, so an old drop does not look new. |
| `Destination NAME: 3 alerts are not delivered. MMWatchdog tries again at 14:05 UTC. The latest attempt failed: ...` | A destination that fails. The line appears after a failed delivery attempt, or once the destination's verification has failed for 10 minutes. It counts every alert that waits for the destination, because the later alerts wait behind the oldest. [When a destination cannot be reached](../how-to/send-alerts-somewhere.md#when-a-destination-cannot-be-reached) says what MMWatchdog does then. On a joined node, the line for the hub starts with `The MMWatchdog fleet hub has not accepted 2 alerts.` |
| `Maintenance on: CHECK_GROUP until ..., turned on by USER: REASON` | A maintenance window, with its end, who started it, and why. |
| `Systemd unit restarts UNIT: 1 of 3 attempts used; ...`, `Recovery command CHECK_GROUP: ...` | The recovery actions that a check group has used, and the result of the last one. `max_attempts: unlimited` shows as `4 attempts used, unlimited`. |
| A line for each check that MMWatchdog keeps quiet | The check fails while a check that it names in `no_alerts_or_recovery_actions_while_these_checks_fail` fails, so it sends no alert. See [Stop alerts that follow another failure](../how-to/stop-alerts-that-follow-another-failure.md). |
| `Fleet hub: ADDRESS as NODE; last report ...` | On a joined node: the hub, and when the hub last accepted a report. `no settings from the hub yet, so alerts wait for the hub` and the latest error appear here too. |
| `Fleet hub: nodes report to ADDRESS; see mmwatchdog fleet` | On the hub. |
| `MMHotline DESTINATION: ...` | For each `mmhotline` destination: when MMHotline last gave a signed answer, and any delivery or clock problem. See the [`mmhotline` page](modules/mmhotline.md). |
| A line about the measurement history | Appears only when the size limit has cut the history shorter than `keep_measurement_history_for`. |
| `Storage: ...` | MMWatchdog cannot save or read its state on the node's disk. |
| `This node sends alerts to nobody.` | A standalone node without a destination. |

**One check group.** `mmwatchdog status CHECK_GROUP` shows only that
check group's checks, and below each check the details: the file and line
of its configuration, its timings, its alert severity settings, its latest
run with output and measurements, and the evidence of each open incident.

```text
$ sudo mmwatchdog status commands_page_example
commands_page_example/a_check_with_a_long_name_so_that_its_alert_title_gets_shortened_ ok           command exited 0
  Config: /etc/mmwatchdog/check_groups/commands-page.yml:11:7
  Rule: check every 30s, time limit per run 5s, alert after failing for 10m, recovered after healthy for 30s
  Alert severity: info
  Observed: 2026-11-15T10:29:14.173371061Z
  Exit code: 0
commands_page_example/marker_file_exists confirming_failure the latest run is healthy, but runs have failed since 10:28:43 UTC; the check counts as failing until it has been healthy for 30s (recovered_after_healthy_for), so that a check that fails on and off still alerts: command exited 0
  Config: /etc/mmwatchdog/check_groups/commands-page.yml:8:7
  Rule: check every 30s, time limit per run 5s, alert after failing for 10m, recovered after healthy for 30s
  Alert severity: info
  Observed: 2026-11-15T10:29:14.173421525Z
  Exit code: 0
Alert deliveries: 0 waiting, 0 dropped
Recovery command commands_page_example: 1 of 1 attempts used; exit 0
Fleet hub: 203.0.113.11:8445 as web01; last report 2026-11-15T10:29:03Z
```

- A heartbeat check shows `Rule: next heartbeat due by TIME` and
  `Last heartbeat: TIME` instead of the timings.
- In both forms, a check whose module is not built in has a line such as
  `module queued_jobs: the project's own, Starlark`. The words are
  `official extra`, `the project's own, Starlark`, or
  `the project's own, executable`.
- In both forms, an `every_systemd_unit` check, and another check that
  reports several findings, has one indented line for each finding, such
  as each failed unit, with the finding's own state.
- A name that is not a check group of the node is refused:
  `mmwatchdog: unknown check group name "nosuch"`, exit code 4.

**Exit codes:** 0 when every check is healthy. 1 when a check has an open
failure incident (`failed` or `confirming_recovery`). 3 when a check is
`unknown`, MMWatchdog has a storage problem, or a destination fails, and
no check has failed: a failed check makes the code 1, because a failure
takes precedence over health unknown. The output shows a failing
destination in both cases. With a check group's name, the code covers only
that check group's checks, plus the destinations and the storage. 4: the
command could not ask the service.

### JSON of mmwatchdog status

`sudo mmwatchdog status --json` prints one object:

| Field | What the field holds |
|---|---|
| `summary` | `exit_code`, the exit code above, and `attention`: one entry for each check with an open failure incident or unknown health, with `check`, `finding` (for one finding of a check), `state`, `since`, and `message`. Healthy checks, `confirming_failure`, and the two waiting states are not listed. A storage problem has no entry. |
| `checks` | One object for each check, by `CHECK_GROUP/CHECK`. See the next table. |
| `systemd_unit_restart_attempts`, `recovery_command_attempts` | By unit, and by check group: `attempts_used`, `max_attempts` (a number, or `unlimited`), `last_attempt_at`, `last_attempt_finished_at`, and `last_result`. |
| `maintenance` | The open maintenance windows, by check group, or `every_check_group` for the whole node: `until`, `reason`, `user`, and `without_maintenance_on_and_off_alerts`. |
| `delivery` | `queued_notifications` (alerts that wait), `delivery_gaps` (alerts dropped), `first_delivery_gap_at`, `newest_delivery_gap_at`, `last_error` (the error of the newest waiting alert that has a failed attempt), and `failing_destinations`. Each failing destination has `destination`, `undelivered_alerts`, `latest_attempt_error`, `next_attempt_at`, and `oldest_undelivered_alert_queued_at`, or `verification_failing_since` for a failed verification. On a joined node, the hub's queue has the name `hub`. The list is empty when no destination fails. |
| `storage_error` | Empty, or the storage problem. |
| `joined_fleet_hub` | On a joined node: `hub_address`, `this_node_name`, `last_accepted_report_at`, `last_error`, `last_error_at`, and `fleet_settings_version`. `null` on other nodes. |
| `fleet_hub_role_address` | On the hub only: the address where nodes report. |
| `sends_alerts_to_nobody` | `true` on a standalone node without a destination. |
| `measurement_history` | `oldest_reading_at`, `newest_reading_at`, `keep_measurement_history_for`, `measurement_history_size_limit`, and `cut_short_by_measurement_history_size_limit`. |
| `mmhotline_destinations` | One entry for each `mmhotline` destination, with its connection details. |

Each entry of `checks`:

| Field | What the field holds |
|---|---|
| `state` | The [check state](#check-states). |
| `needs_fresh_observation` | `true` when the latest result stopped counting, such as after a reload, and the check waits for its next run. |
| `last_observation` | The latest run: `status` (`ok`, `failed`, or `unknown`), `message`, `at`, `duration_ns`, and, where the check has them, `measurements` (each with `value` and `unit`), `exit_code`, `stdout`, `stderr`, `findings`, and `sample_window_ns`. |
| `check_group`, `check`, `source` | The names, and the `file`, `line`, and `column` of the check's configuration. |
| `module`, `module_kind` | The module's name, and its kind: `core`, `official_extra`, `project_starlark`, or `project_executable`. A heartbeat and a check of the fleet have neither. |
| `timing` | `check_every`, `time_limit_per_run`, `alert_after_failing_for`, and `recovered_after_healthy_for`. |
| `alert_severity_settings` | `alert_severity`, and `raise_to_sos_after_failing_for` when it is set. |
| `failing_since` | The start of the [failing time](../how-to/decide-when-a-check-alerts.md). Outside a failing time it holds the zero time `0001-01-01T00:00:00Z`. |
| `failing_time_runs_failed` | During a failing time: whether each of the last runs failed, at most 10, oldest first. |
| `healthy_since`, `unknown_since` | Since when the check has been healthy, or unknown. |
| `failure_incident`, `cannot_check_incident` | The open incident: `incident_id`, `opened`, `alert_severity`, and the `evidence` of the run that opened it. `announced_incident_id` names an incident whose failure alert went out and whose closing alert has not gone out yet. |
| `next_due_by`, `last_received_at` | For a heartbeat check: the deadline of the next heartbeat, and when the last heartbeat arrived. |
| `records_only` | `true` for a check that only records readings. |
| `send_alerts_only_to_destinations`, `no_alerts_or_recovery_actions_while_these_checks_fail` | The check's settings of the same names, when it has them. |
| `no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails` | The checks that name this check, which MMWatchdog keeps quiet while this check fails. |
| `no_alerts_or_recovery_actions_now_while_check_fails` | The named check whose failure keeps this check quiet now. |
| `no_alerts_or_recovery_actions_until_next_run_because_check_was_failing` | The named check that kept this check quiet and is no longer failing: this check stays quiet until its own next run. |
| `findings` | For a check that reports findings, such as `every_systemd_unit`: one entry for each finding, with the same fields. |

A field that has no value can be left out or hold the zero time.

[See what is happening](../how-to/see-what-is-happening.md) uses
`status` step by step.

## mmwatchdog events

`mmwatchdog events` shows the event history of the node, newest first:
each line is the time, the event's type, what the event is about, and its
message.

```text
$ sudo mmwatchdog events commands_page_example
2026-11-15T10:29:31Z maintenance_off      commands_page_example maintenance turned off by deploy: trying the commands page
2026-11-15T10:29:31Z maintenance_on       commands_page_example maintenance until 2026-11-15 10:39 UTC, turned on by deploy: trying the commands page
2026-11-15T10:29:31Z recovery_attempts_reset commands_page_example operator reset recovery attempts
2026-11-15T10:29:14Z recovery_command_result commands_page_example/marker_file_exists recovery command finished with exit code 0
  Observed: 2026-11-15T10:29:14.073825758Z
  Exit code: 0
2026-11-15T10:29:14Z recovery_command_started commands_page_example/marker_file_exists run recovery command, attempt 1
```

- Without a check group, the command shows every event of the node, one
  line each. With a check group, it also shows the incident of each event
  and the evidence below it: the output of a recovery command, or the run
  that opened an incident. Evidence stays in the history after recovery,
  and after the check group is removed from the configuration.
- With `CHECK_GROUP/CHECK`, such as `notes_app/notes_service_is_running`,
  the command shows only the events of that check, from the events of its
  check group.
- When the history holds no matching event, the command says so, such as
  `No events of the check group notes in the node's event history.`: check
  the name with `mmwatchdog status`.
- Without a time flag, the command shows the latest 100 events. With
  `--last`, or `--since` and `--until`, it shows every kept event in the
  span: see [Time spans](#time-spans). How long events are kept is in
  [Configuration files](configuration-files.md).
- A span whose events add up to more than 16 MiB is refused: choose a
  shorter span.

| Flag | Use |
|---|---|
| `--last`, `--since`, `--until` | A [time span](#time-spans). |
| `--json` | Prints the events as a JSON list. The fields of an event, such as `type`, `title`, `body`, and `alert_severity`, are in [Alert format](alert-format.md). |
| `--socket PATH` | See [Flags that several commands share](#flags-that-several-commands-share). |

Exit codes: 0, or 4.

## mmwatchdog check-now

`mmwatchdog check-now CHECK_GROUP` runs the checks of one check group once
and shows each result. The command waits for the results. Ctrl-C ends the
wait.

```text
$ sudo mmwatchdog check-now commands_page_example
commands_page_example/a_check_with_a_long_name_so_that_its_alert_title_gets_shortened_ ok       command exited 0
  Observed: 2026-11-15T10:28:43.971704373Z
  Exit code: 0
commands_page_example/marker_file_exists failed   command exited 1
  Observed: 2026-11-15T10:28:44.044007802Z
  Exit code: 1
mmwatchdog: one or more checks need attention
```

Below each check, the output shows what the run produced: its output on
standard output and standard error, its exit code, its measurements, and
when it ran.

**Without a flag, the run counts for nothing.** `check-now` records no
incident, sends no alert, restarts no service, and runs no recovery
command, because a look must not change anything. The node monitoring
process runs the checks one after another, each with the check group's
`time_limit_per_run`.

- A requested run first waits until a scheduled run of the same check has
  ended, and until fewer than `max_concurrent_checks` checks run on the
  node. The time limit starts after the wait.
- A requested run starts from what scheduled runs kept, such as the
  restart count of a `systemd_unit_crash_loop` check, and changes nothing
  of it.

| Flag | Use |
|---|---|
| `--count-as-scheduled-run` | After a fix: the node monitoring process starts the check group's scheduled runs at once, and their results count. The state changes, an alert opens or closes, and a failed check can restart a service or run its recovery command. An alert still closes only after the check has been healthy for `recovered_after_healthy_for`, and failed results count too, so one healthy run cannot close the alert of a check that fails on and off. Each check's next run comes `check_every` later. Without this flag, a fixed check that runs once an hour can keep its alert open for up to an hour. |
| `--without-daemon` | Runs the checks in the command itself, from the configuration in `--config-dir`, without the node monitoring process: use it while you write a configuration. Nothing is kept from earlier runs. Each check still runs as the account that the configuration names. It cannot be combined with `--count-as-scheduled-run`. |
| `--config-dir DIR` | Only with `--without-daemon`: the configuration to use. |
| `--json` | Prints one object with each check's name and its run: `status`, `message`, `at`, `duration_ns`, and where the check has them, `measurements`, `exit_code`, `stdout`, `stderr`, `findings`, and `sample_window_ns`. |

`--count-as-scheduled-run` ends with
`The runs counted as scheduled runs for check group commands_page_example; run mmwatchdog status commands_page_example to see the resulting check status.`

A heartbeat check group cannot run on demand: the command refuses it with
`heartbeat checks receive heartbeats instead of running on demand`.

Exit codes: 0 when every run is `ok`. 1 when a run failed. 3 when a run
is `unknown` and none failed. 4 for an unknown check group, a heartbeat
check group, or a configuration that cannot be loaded.

## mmwatchdog readings

`mmwatchdog readings CHECK_GROUP [CHECK]` lists the readings of the node's
measurement history, oldest first, with each reading's limit.

```text
$ sudo mmwatchdog readings machine memory_is_below_90_percent --last 3m
check group machine · Nov 15 10:25:11 to 10:28:11 UTC

memory_is_below_90_percent
memory_used_percent, percent · fails above 90

  6 readings, none beyond the limit
    10:25:14   18.7
    10:25:44   18.69
    10:26:14   18.69
    10:26:44   18.69
    10:27:14   18.67
    10:27:44   18.67
  lowest 18.67 · highest 18.7 at 10:25:14 · latest 18.67 at 10:27:44
```

Without `CHECK`, the command shows every measurement of the check group.
[Read the readings](../how-to/look-back-at-measurements.md#read-the-readings)
says what each line means, and how to read a reading beyond its limit.

- The command reads the history file directly, read-only, without the
  node monitoring process. So it works while the service is stopped. It
  needs `sudo`, because root owns the file:
  `mmwatchdog: readings requires access to root's measurement history; run with sudo`.
- It finds the file through `state_dir` in the configuration, and creates
  no missing file.
- It never shortens the list, because a short spike may explain a
  problem. Choose a shorter span when the output is too long.
- Times are to the second. The date appears only when the span covers
  more than one UTC day, such as `Oct 22 17:30:50`. Values have four
  significant digits, such as `37.25`, and the unit is in the
  measurement's title. On a terminal, the title is bold.
- Each reading is judged by the limits that were recorded with it, not by
  the configuration of today. A reading equal to its limit is within the
  limit.
- A measurement without a limit says `only records` in its title. When
  another measurement of the same check has a limit, the title says
  `recorded, not judged by this check` instead, such as for
  `inodes_used_percent` of a `node_filesystem` check with a limit on
  `space_used_percent`.

| Flag | Use |
|---|---|
| `--last`, `--since`, `--until` | A [time span](#time-spans). The default is `--last 1h`. |
| `--chart` | Draws each measurement as a chart instead of listing its readings. [Read the readings](../how-to/look-back-at-measurements.md#read-the-readings) shows one. |
| `--json` | See below. `--json` and `--chart` cannot be used together. |
| `--config-dir DIR` | The configuration whose `state_dir` holds the history. |

The JSON has every time and value in full:

| Field | What the field holds |
|---|---|
| `since`, `until` | The span, in UTC. |
| `check_group`, `check`, `node_group` | What you selected. A name that you did not give is left out. |
| `nodes` | One entry for `readings`, without `node`. For `fleet readings`, one entry for each node, in the order of the text. |
| `nodes[].node`, `nodes[].why_no_readings`, `nodes[].readings_are_healthy` | The node's name, why it has no readings, and whether its readings stayed within their limits. |
| `nodes[].measurements` | One block for each check and measurement: `check_group`, `check`, `measurement`, `readings`, and `summary`. |
| `readings` | Each reading: `at`, `value`, `unit`, the limits recorded with it (`fail_when_measurement_above`, `fail_when_measurement_below`), and `limit_status`: `above the limit`, `below the limit`, `within the limits`, or `only records`. |
| `summary` | `reading_count`, `readings_beyond_limit`, and `lowest`, `highest`, and `latest`, each a whole reading. |
| `nodes_with_healthy_readings_count` | How many nodes have healthy readings. |

Exit codes: 0, also when readings were beyond their limits. 4 for a wrong
argument or a history that cannot be read.

**For a calculation of your own,** open the history file read-only:
`sudo sqlite3 -readonly /var/lib/mmwatchdog/measurement_history.db`. Its
table `readings` has one row for each run:

| Column | What the column holds |
|---|---|
| `id` | The run's sequence number in the history. |
| `at` | The time of the run, in nanoseconds since 1970-01-01 UTC. |
| `kept_from` | The time from which the reading's age counts. In a node's own history, the time of the run. |
| `check_group`, `check_name` | The check group and the check. |
| `measurements` | The run's measurements as JSON, with the limits of that run, such as `{"memory_used_percent":{"value":41,"unit":"percent","fail_when_measurement_above":90}}`. A measurement without `fail_when_measurement_above` and `fail_when_measurement_below` only records. |
| `node` | Empty in a node's own history. On the hub, the joined node's name. |

## mmwatchdog validate

`mmwatchdog validate` checks the configuration in `/etc/mmwatchdog`. On
the node, it also checks the files, accounts, and modules that the
configuration names. It changes nothing, so it is safe while the service
runs.

```text
$ sudo mmwatchdog validate
Valid configuration: 2 check groups; alert destinations come from the hub
An alert title has at most 120 characters. For node web01, MMWatchdog will shorten the alert titles listed below; shorten the check group name or check name to keep the full title:
  commands_page_example/a_check_with_a_long_name_so_that_its_alert_title_gets_shortened_: the check's longest alert title has 132 characters and will be sent as: web01: MMWatchdog still cannot check commands_page_example/a_check_with_a_long_name_so_that_its_alert_title_ge…, now SOS
```

The first line counts the check groups and the destinations. On a joined
node, the destinations come from the hub. A standalone node without a
destination adds `This node sends alerts to nobody.` The lines after it
are [validation warnings](#validation-warnings).

| Flag | Use |
|---|---|
| `--config-dir DIR` | Checks the configuration in `DIR`, such as a copy that you want to install. `DIR` holds the node settings, the check groups, and the destinations. |
| `--config-only` | Checks only the configuration files. It opens no secret or certificate file, reads no saved state, and looks up no account. Use it on a machine that is not the node, such as your laptop with the project's files. Only the check on the node uses the saved fleet settings to check where alerts go. |
| `--official-extra-module-dir DIR`, `--external-module-dir DIR` | Checks against these module directories instead of the node's. They work with and without `--config-only`. A deploy uses them to check a new configuration against the modules that it has copied to the node and not installed yet. |
| `--json` | See below. |

Without `sudo`, the check on the node fails, because the secret files
belong to root:
`mmwatchdog: /etc/mmwatchdog/mmwatchdog_node_settings.yml:4:3: join_fleet_hub.join_code_file: stat /etc/mmwatchdog/secrets/fleet-hub-join-code: permission denied`.

**What every check covers.** An error starts with the file, the line, and
the column, such as `/etc/mmwatchdog/check_groups/myapp.yml:10:11:`, then
names the field. For a missing field, the error points at the nearest
enclosing key in the file. Every check, also with `--config-only`, checks
each check and each destination against the description of its module,
including a script's own `validate_inputs` function, before anything
runs.

**What the check on the node adds.** Without `--config-only`, MMWatchdog
also checks:

- The heartbeat receiver's TLS certificate and private key, and each
  heartbeat token. Each heartbeat check group needs its own token.
- Every file that a destination or a check names, such as webhook token
  files, email password files, HTTP credential files, the password files
  of `redis`, `postgresql`, and `mysql` checks, the certificate authority
  files of `http`, `tls`, `smtp`, `imap`, and `pop3` checks, and the
  MMHotline signing secret file. No other account may read a secret file.
- The accounts and directories of `run_as_user_account` and
  `run_from_directory`, for every `shell` check, module that is not built
  in, and recovery command.
- Each Starlark script: it must be valid and define the functions that
  MMWatchdog calls. Each `module_executable` must be a file the node can
  run.
- `module_state_dir`: a directory that MMWatchdog made, or none yet.
- The join code, and the size of the existing `state.db` and `fleet.db`,
  which must not be larger than their size settings allow.

A file that is missing on the machine ends the error with a hint to use
`mmwatchdog validate --config-only` away from the node.
[Configuration files](configuration-files.md) lists the errors about each
setting.

**Errors about modules.** These errors stop a deploy, because a deploy
runs `validate` on the copies before it installs anything. The examples
leave out the file and line in front:

| The problem | Example of the error |
|---|---|
| A check or destination names a module that the node does not have, also one that only an earlier deploy installed | `check_groups.myapp.checks.job_queue_is_short.queue_length: unknown field "queue_length": the field name matches neither a setting of a check nor an installed module. MMWatchdog has no core module of that name, and no directory of that name is in /usr/local/lib/mmwatchdog/official_extra_modules (slack, telegram) or in /usr/local/lib/mmwatchdog/external_modules (alarm_system, file_age, queued_jobs)` |
| An input that the module does not have | `check_groups.myapp.checks.job_queue_is_short.queued_jobs.queue_file: unknown field "queue_file": the inputs of the module queued_jobs are queue_file_path` |
| A required input is missing | `destinations.team_chat.slack.webhook_url_file: is required: The absolute path of a file that holds the address of the Slack incoming webhook, ...` |
| A value of the wrong type | `destinations.on_call_phone.telegram.chat_id: expected a string` |
| An input whose name ends in `_file` holds no absolute path | `destinations.team_chat.slack.webhook_url_file: an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set webhook_url_file to an absolute file path` |
| The inputs of one destination are longer than 256 KiB as JSON. Put long text in a file, and name the file with an input whose name ends in `_file` | `destinations.team_chat.slack: the inputs are 270042 bytes as JSON, and the inputs of one destination are at most 262144 bytes` |
| On the node: the files that one destination's inputs name hold more than 64 KiB together | `destinations.team_alarm_system.alarm_system: the files named in the destination's inputs total 70000 bytes, more than the limit of 65536 bytes for one destination` |
| A check module used as a destination, or the other way round | `destinations.team_chat.queued_jobs: the module queued_jobs is used as a check, not as an alert destination: its description says used_as: check` |
| A limit on a measurement that the module does not report | `check_groups.myapp.checks.job_queue_is_short.fail_when_measurement_above.jobs: this check has no such measurement; it measures queued_jobs` |
| The official extra modules come from another release than the `mmwatchdog` program | `the official extra modules in /usr/local/lib/mmwatchdog/official_extra_modules were shipped with MMWatchdog 1.0.0, and this mmwatchdog program is MMWatchdog 1.1.0. One MMWatchdog release holds both: deploy that release again` |
| The official extra modules directory has no file `shipped_with_mmwatchdog_version` | `/usr/local/lib/mmwatchdog/official_extra_modules has no file shipped_with_mmwatchdog_version, so it is not the official_extra_modules directory of an MMWatchdog release: ...` |
| A module of your own has the name of an official extra module, a core module, or a setting | `the module slack in /usr/local/lib/mmwatchdog/external_modules has the name of the official extra module slack, which every deploy installs. A name means one module on every node: give your module another name` |
| A directory name that no module can have | `/usr/local/lib/mmwatchdog/external_modules/queue-length is no module: the name of a module uses lower-case letters, digits, and underscores, starts with a letter, and has at most 64 characters, such as queued_jobs` |
| A module without a description file | `the module queued_jobs has no description: /usr/local/lib/mmwatchdog/external_modules/queued_jobs must hold the file module_description.yml, which lists the module's inputs` |
| A module without a script or an executable | `the module queued_jobs has no code: /usr/local/lib/mmwatchdog/external_modules/queued_jobs must hold the script module.star or the executable module_executable` |

The message about a missing module lists the modules that each directory
holds. Put the module back into the project, or take its name out of the
configuration, and deploy again.

### Validation warnings

A validation warning is a remark about a valid configuration that will
probably not do what you expect. A warning never makes the configuration
invalid: `validate` prints it and exits with 0, and `reload` prints it and
uses the configuration. Each warning names every check, unit, filesystem,
or destination that it is about, one on a line.

| MMWatchdog warns when | The warning starts with | List in `--json` |
|---|---|---|
| An alert title of a check would be longer than 120 characters on this node | `An alert title has at most 120 characters.` | `alert_titles_that_will_be_shortened`: `check_group`, `check`, `characters`, and the shortened `title` |
| A systemd unit has its own `systemd_unit_state` check and is missing from the `systemd_units_to_ignore` of an `every_systemd_unit` check, so a failure would send two alerts | `An every_systemd_unit check alerts for every failed systemd unit outside the check's systemd_units_to_ignore list.` | `systemd_units_missing_from_systemd_units_to_ignore` |
| A filesystem has its own `node_filesystem` check with a limit and is missing from the `filesystems_to_ignore` of an `every_disk_and_network_filesystem` check | `An every_disk_and_network_filesystem check alerts for every filesystem beyond the check's limits and outside the check's filesystems_to_ignore list.` | `filesystems_missing_from_filesystems_to_ignore` |
| A check names, in `no_alerts_or_recovery_actions_while_these_checks_fail`, a check with a lower `alert_severity` | `While a check named in another check's no_alerts_or_recovery_actions_while_these_checks_fail fails, MMWatchdog sends one alert,` | `checks_that_name_a_check_with_a_lower_alert_severity`: `check_group`, `check`, `alert_severity`, `named_check` (as `CHECK_GROUP/CHECK`), and `named_check_alert_severity` |
| No destination takes a check's `alert_severity` | `CHECK_GROUP/CHECK: no destination receives alerts with alert_severity ...` | `checks_without_alert_destination` |
| On a joined node: a check's `send_alerts_only_to_destinations` names a destination that the saved fleet settings do not have | `CHECK_GROUP/CHECK: the saved fleet settings have no destination named ...` | `checks_with_unknown_alert_destinations`, each with `unknown_destination_names` |
| On the hub: an `mmhotline` destination uses a loopback `mmhotline_url`, which joined nodes cannot reach | The file and line of the setting, then `destinations.DESTINATION.mmhotline.mmhotline_url: joined nodes send alerts to the mmhotline_url address when the hub does not accept an alert in time.` | `mmhotline_loopback_warnings`, one string for each destination |
| On the hub or a standalone node: the only destinations are `mmhotline` destinations, so an alert that MMHotline refuses reaches nobody | The file and line of the destination, then `destinations.DESTINATION: when MMHotline permanently refuses an alert, MMWatchdog sends the refused alert to the node's destinations that do not use the mmhotline module.` | `mmhotline_without_another_destination`, one string for each destination |

### JSON of mmwatchdog validate

`--json` prints `valid: true`, the whole configuration as `config` (the
same object that [`mmwatchdog config`](#mmwatchdog-config) prints), and
each list of the table above. Each list is always there, and is empty when
MMWatchdog has nothing to warn about. `config` shows the node's own
`official_extra_module_dir` and `external_module_dir`, also when the
directory flags name others. An invalid configuration prints the error on
standard error, not JSON.

Exit codes: 0 valid, also with warnings. 1 invalid, also a file that is
missing or cannot be read. 4 a wrong flag.
[Add a check](../how-to/add-a-check.md) uses `validate` before each
deploy.

## mmwatchdog reload

`mmwatchdog reload` reads the whole configuration directory again. The
node monitoring process uses the new configuration only if all of it is
valid. An invalid configuration changes nothing: the service keeps the
configuration that it has, and the command prints the error.

```text
$ sudo mmwatchdog reload
The configuration is active.
An alert title has at most 120 characters. For node web01, MMWatchdog will shorten the alert titles listed below; shorten the check group name or check name to keep the full title:
  commands_page_example/a_check_with_a_long_name_so_that_its_alert_title_gets_shortened_: the check's longest alert title has 132 characters and will be sent as: web01: MMWatchdog still cannot check commands_page_example/a_check_with_a_long_name_so_that_its_alert_title_ge…, now SOS
```

After `The configuration is active.`, the command prints the
[validation warnings](#validation-warnings) of the new configuration and,
for each new or changed destination, the result of its verification and a
`send-notice` command that sends it a test notice. The reload sends no
test message itself.

**What a reload keeps.**

- A check that the reload does not change keeps its state, its schedule,
  and the result of a run that is under way.
- A changed check starts again as a new check, and a removed check is
  gone. An open incident of a changed or removed check closes with a
  `closed_without_recovery` event that gives the configuration change as
  the reason. If the incident's failure alert went out, a closing alert
  goes out too, to the destinations of the new configuration, also during
  maintenance.
- A check group keeps the recovery attempts that it has used. A reload
  that changes a unit's `restart_systemd_unit` settings, or a
  `recovery_command`, cancels an action that has not yet saved its
  attempt. An action that saved its attempt goes ahead.
- The measurement history settings of the node and of the hub take effect
  at once.
- On the hub, a changed `set_aside_findings.yml` takes effect at once. An
  incident that an entry now covers closes with one
  `closed_without_recovery` alert for each destination and key,
  `Set aside after investigation: <reason>`. Incidents that an entry no
  longer covers send one opening alert for each destination and key, with
  the note `an earlier exact finding was set aside: <reason>`. The hub
  does the same when it starts, for changes made while it was stopped.

**What needs a restart instead.** These settings take effect only when
the service restarts: `max_concurrent_checks`, `socket`, `state_dir`,
`module_state_dir`, `heartbeat_receiver` and a renewed certificate of the
heartbeat receiver, adding, removing, or moving the hub
(`fleet_hub_role.listen_on_address`, `fleet_hub_role.address_for_nodes`),
`state_database_size_limit`, and
`fleet_hub_role.event_history_of_nodes_content_size_limit`. A deploy
through MMX restarts the service when it needs to.

`--json` prints `reloaded`, `new_or_changed_destinations`,
`destination_verifications` (each with `can_verify_without_message`,
`accepts_address_and_credential`, and `verification_message`), and the
warning lists of [`validate --json`](#json-of-mmwatchdog-validate).

Exit codes: 0 active. 1 active, but a destination failed its
verification. 4 invalid configuration, so nothing changed, or the service
cannot be reached.

## mmwatchdog config

`mmwatchdog config` prints the configuration that the node monitoring
process uses, as JSON, with every default filled in. The output is
always JSON; the command also accepts `--json`, which changes nothing.

- `sources` maps each setting to the file, line, and column that set it.
- On a joined node, `alerts` holds the destinations from the hub, and
  `fleet_timings_from_hub` holds the fleet's timings from the hub.

Exit codes: 0, or 4.
[Configuration files](configuration-files.md) describes each setting.

## mmwatchdog maintenance

`mmwatchdog maintenance on` starts a maintenance window for one or more
check groups. During the window, MMWatchdog keeps running the checks and
showing their results, but it sends no alert for those check groups,
restarts no systemd unit, and runs no recovery command. Every window has
an end that you write.

```text
$ sudo mmwatchdog maintenance on commands_page_example --for 10m --reason "trying the commands page"
commands_page_example is in maintenance until 2026-11-15 10:39 UTC (10m).
  MMWatchdog keeps running the checks of commands_page_example and shows the results in mmwatchdog status.
  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for commands_page_example.
  To end the maintenance early: sudo mmwatchdog maintenance off commands_page_example
  The MMWatchdog fleet hub received the maintenance change.
$ sudo mmwatchdog maintenance off commands_page_example
commands_page_example is out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.
  The MMWatchdog fleet hub received the maintenance change.
```

The forms:

```text
mmwatchdog maintenance on CHECK_GROUP [CHECK_GROUP ...] --for DURATION [--reason TEXT] [--without-maintenance-on-and-off-alerts]
mmwatchdog maintenance on --every-check-group --for DURATION [--reason TEXT] [--without-maintenance-on-and-off-alerts]
mmwatchdog maintenance off CHECK_GROUP [CHECK_GROUP ...]
mmwatchdog maintenance off --every-check-group
```

| Flag | Use |
|---|---|
| `--for DURATION` | Required with `on`: how long the window lasts, such as `10m`, `1h`, or `30d`. It must be positive and has no upper limit. |
| `--reason TEXT` | Optional with `on`: why, as one line of at most 200 bytes. A longer or multi-line reason is refused. |
| `--every-check-group` | In place of the names: the whole node, also check groups that a reload adds during the window. It cannot be combined with names. |
| `--without-maintenance-on-and-off-alerts` | With `on`: records the start and the end of the window in the event history, but sends neither the `maintenance_on` nor the `maintenance_off` alert, also not through a hub. Everything else stays the same. The command adds `Maintenance start and end are not announced.`, the status line ends with `maintenance start and end are not announced`, and the window in JSON has `without_maintenance_on_and_off_alerts: true`. |
| `--socket PATH` | See [Flags that several commands share](#flags-that-several-commands-share). |

- Separate the check group names with spaces. A name with a comma is
  refused. One unknown name refuses the whole command, and no window
  starts or ends:
  `mmwatchdog: unknown check group name "nosuch"`.
- One command sends one `maintenance_on` alert that names all its check
  groups, and later one `maintenance_off` alert. The alert names the end,
  the reason, and the person: the `sudo` user, or the login user after
  `sudo su -`.
- `on` for a check group that already has a window replaces that window.
  A window ends with `off`, at its end time, or when a reload removes the
  check group. A window survives a restart of the service.
- `off --every-check-group` ends only the whole-node window. A check group
  that has its own window stays in maintenance until that window ends, and
  the command says so.
- When a window ends, MMWatchdog waits for a fresh run of each check before
  it alerts, restarts, or runs a recovery command again.
- `node_reports` is a check group name too: on the hub,
  `maintenance on node_reports` stops the alerts that a joined node stopped
  reporting. On the hub, the whole-node window covers `node_reports` as
  well.
- Maintenance covers only the node where you run the command.

**On a joined node,** `on` and `off` wait until the hub accepts the
change, at most 25 seconds, and then add
`The MMWatchdog fleet hub received the maintenance change.` For a
whole-node window, the line is
`The MMWatchdog fleet hub has the window, so it sends no alert that this node stopped reporting until the window ends.`
When the hub does not accept the change, the window still starts or ends
on the node, but the command says that the hub does not know, and exits
with 1. So a script can stop before it reboots the node.

Exit codes: 0. 1 on a joined node whose hub did not get the change. 4 for
a refused command, such as a missing `--for`, an unknown check group, or
a change that could not be saved on the node's disk.
[Pause alerts during planned work](../how-to/pause-alerts-during-planned-work.md)
uses each form.

## mmwatchdog reset-recovery-attempts

`mmwatchdog reset-recovery-attempts CHECK_GROUP` sets the used restart and
recovery command attempts of one check group back to 0. Use it when the
attempts ran out, the cause is fixed, and MMWatchdog should act again on
the next failure.

```text
$ sudo mmwatchdog reset-recovery-attempts commands_page_example
The recovery attempts of check group commands_page_example are reset. MMWatchdog can attempt recovery actions again.
```

- It resets the restarts of the unit that the check group's
  `restart_systemd_unit` names, and the check group's `recovery_command`.
  A restart budget that several check groups share is reset for all of
  them.
- The reset is recorded as a `recovery_attempts_reset` event.
- A check group without either action is refused:
  `mmwatchdog: check group has no restart_systemd_unit and no recovery_command`.
  So is a check group whose recovery action runs right now.

Exit codes: 0, or 4. [Restart a service
automatically](../how-to/restart-a-service-automatically.md) says when the
attempts run out.

## mmwatchdog send-notice

`mmwatchdog send-notice` sends a one-time message, a **notice**, through
the alert destinations, the same way as an alert. Use it to test the
destinations, or from a script that announces an event.

```text
$ sudo mmwatchdog send-notice --alert-severity info --title "Test of the commands page. Nothing is wrong."
Notice sent to the MMWatchdog fleet hub at 203.0.113.11:8445, which delivers it to every destination that takes info:
  MMWatchdog fleet hub   accepted
To see the result for each destination, send the notice from the fleet hub: run the same mmwatchdog send-notice command there.
```

This node has joined a hub, so the notice went to the hub. On the hub and
on a standalone node, the command prints one line for each destination.

| Flag | Use |
|---|---|
| `--alert-severity info\|warn\|sos` | Required. The notice goes to every destination whose `alert_severities` hold the value. |
| `--title TEXT` | Required. One line. MMWatchdog puts the node's name in front, and the whole title has at most 120 characters. A longer title is refused with the number of characters to cut, because MMWatchdog never shortens a title that a person typed. |
| `--body TEXT` | Optional. More text, at most 8 KiB. |
| `--destination NAME` | Optional. Only this destination gets the notice, whatever its `alert_severities` hold. An unknown name is refused, and the error lists the destinations. On a joined node, `NAME` is a destination of the hub, and the command refuses the flag until the node has the fleet settings from the hub. |
| `--socket PATH` | See [Flags that several commands share](#flags-that-several-commands-share). |

The command waits for the first delivery attempt to each destination. Each
line says what happened:

| Line | Meaning |
|---|---|
| `pager   delivered` | The destination took the notice. |
| `MMWatchdog fleet hub   accepted` | On a joined node: the hub took the notice. The hub delivers it. |
| `pager   failed: REASON` | The first attempt failed. The notice stays queued, and MMWatchdog tries again as for every alert. |
| `pager   not attempted yet: 3 earlier alerts are not delivered, and the latest attempt failed: REASON` | The destination fails already. The notice waits behind the earlier alerts, because a destination gets its alerts in order. |
| `pager   not sent: ...` | The notice left the queue before an attempt, because a reload changed or removed the destination, or the queue was full. |

- When no destination takes the severity, the command prints
  `No destination takes info, so MMWatchdog sent the notice to nobody. The notice is recorded in mmwatchdog events.`
  and exits with 0.
- A notice has no check group, incident, recovery, or reminder. A
  maintenance window does not hold it back.
- The hub sends each notice alone, never in a grouped message.

Exit codes: 0 when every line says `delivered` or `accepted`, or no
destination takes the severity. 1 when a first attempt did not deliver,
and the command ends with a sentence such as
`mmwatchdog: the notice was not delivered at the first attempt to 1 of 2 destinations. MMWatchdog keeps the notice queued and tries again`.
4 when MMWatchdog did not take the notice, such as a refused title or a
stopped service. A script that must not fail while a chat service is down
adds `|| true`.
[Send a notice](../how-to/send-alerts-somewhere.md#send-a-notice) shows
what the destination receives.

## mmwatchdog heartbeat

`mmwatchdog heartbeat CHECK_GROUP` tells MMWatchdog that a scheduled job
succeeded. The check group has a `heartbeat` check, and MMWatchdog alerts
when no heartbeat arrives in time. The job runs the command at its end,
as its own account, without `sudo`. On success, it prints
`Heartbeat accepted: CHECK_GROUP`.

```text
mmwatchdog heartbeat CHECK_GROUP --bearer-token-file FILE [--receiver-url HTTPS_URL [--trusted-certificate-authority-file FILE]]
```

| Flag | Use |
|---|---|
| `--bearer-token-file FILE` | Required. The file with the check group's token, which the job's account can read. A token that other accounts can read is sent, with a warning on standard error that says how to fix the file's mode. |
| `--receiver-url HTTPS_URL` | For a job on another machine: the address of the node's heartbeat receiver, starting with `https://`. Without the flag, the heartbeat goes through the heartbeat socket of the local node, `/run/mmwatchdog/heartbeat.sock`. |
| `--trusted-certificate-authority-file FILE` | Only with `--receiver-url`: certificates in PEM form to trust instead of the system's certificate authorities. |

- Before it sends the token to the socket, the command checks that root
  owns the socket and its directory, and refuses otherwise. Then no other
  account can collect the token with a socket of its own.
- The command tries up to three times, with waits of 1 and 2 seconds,
  each try at most 5 seconds. A refusal, such as a wrong token, ends it at
  once: `the token does not match, or this node has no heartbeat check group named CHECK_GROUP`.
  For a stranger, a wrong token and an unknown check group look the same.

Exit codes: 0 accepted. 1 refused: a wrong token, or no heartbeat check
group of that name. 4 could not reach MMWatchdog or the receiver, or any
other error. A job's log then tells a setup fault (1) from an outage (4).
[Report from a scheduled job](../how-to/report-from-a-scheduled-job.md)
sets up the check group, the token, and the job.

## mmwatchdog fleet

`mmwatchdog fleet` runs on the hub and shows the nodes that report to it.
On another node it fails with
`mmwatchdog: fleet inspection runs on the hub; this node has no fleet_hub_role setting`.

```text
$ sudo mmwatchdog fleet
Fleet at 203.0.113.11:8445: 3 nodes, 3 healthy
Hub-outage alerter nodes: web01, web02
```

The first line counts the nodes and the healthy ones. Each node that is
not healthy gets its own lines, with its state and its problems. A finding
below `sos` that the hub [sets aside](#mmwatchdog-fleet-set-aside-findings)
does not make its node unhealthy: all such findings share one line, such
as `Set aside: 1 finding on 5000 nodes`. Below
come the hub-outage alerter nodes, the joined nodes that alert when the
hub stays unreachable, and then maintenance windows, the hub's
measurement history when its size limit cut it short, refused joins, and
storage problems.

The forms:

| Form | What it shows |
|---|---|
| `mmwatchdog fleet` | Every node, problems expanded. |
| `mmwatchdog fleet NODE` | One node in detail: its contact, node groups, build, node resources, alert deliveries, each check with its state and evidence, and its recent events. `--last`, `--since`, and `--until` choose the events: see [Time spans](#time-spans). |
| `mmwatchdog fleet --node-group NODE_GROUP` | Only the nodes of one node group. It takes no node name. |

```text
$ sudo mmwatchdog fleet web01
web01: reporting, exit 0
  Contact: last report 2026-11-15T10:29:33Z (13s ago)
  Joined: 2026-11-15T09:26:27Z from 203.0.113.12
  Node groups: none
  Watches the hub
  Build: mmwatchdog 0.1.1 (ad75e5909a0af9b03215d2b2ac4c5bdfa9a56777, linux/amd64); configuration 60181a67fbfd
  Node resources: cpu 3%  memory 19%  disk 10%
  Alert deliveries: 0 waiting, 0 dropped
commands_page_example/a_check_with_a_long_name_so_that_its_alert_title_gets_shortened_ ok           command exited 0
commands_page_example/marker_file_exists ok           command exited 0
machine/cpu_use_is_recorded  ok           cpu_used_percent is 0.33 over 30.099s
machine/memory_is_below_90_percent ok           memory_used_percent is 19.3 (fails above 90)
machine/root_filesystem_is_below_85_percent ok           space_used_percent is 9.75 (fails above 85)
machine/root_filesystem_is_below_95_percent ok           space_used_percent is 9.75 (fails above 95)
node_reports/to_hub          ok           reports reach the hub
Recent events:
2026-11-15T10:29:31Z notice                Test of the commands page. Nothing is wrong.
2026-11-15T10:29:31Z maintenance_off      commands_page_example maintenance turned off by deploy: trying the commands page
...
```

Below `Alert deliveries:`, a line for each failing destination of the
node follows, and then `Last delivery error:` with the node's latest
delivery error, when no line about a failing destination shows that
error. A finding that the hub sets aside reads
`set aside (reason …) until the exact finding changes`. On the node
itself, `mmwatchdog status` still shows the finding as failing, because
only the hub knows what is set aside.

**The state of a node:**

| State | Meaning | Exit code of the node |
|---|---|---|
| `reporting` | The node's reports arrive in time. | The node's own `status` exit code, raised to 3 when a destination of the node fails. 3 when reports arrive but the hub could not save any. |
| `waiting_for_first_report` | The node joined, or the hub started, less than three report intervals ago, and no report has arrived yet. | 0 |
| `silent` | No report arrived before the node's deadline. The hub's alert that the node stopped reporting follows, as [When a node goes silent](../how-to/watch-a-fleet.md#when-a-node-goes-silent) says. | 1 |

A node counts as healthy when it is `reporting`, its exit code is 0, no
destination of the node fails, its clock is at most 5 seconds off the
hub's, and no other node claimed its name in the last hour. Every other
node gets its own lines in `mmwatchdog fleet`. The text output replaces
control characters in the text that a node sent, so a node cannot send
terminal escapes.

A wrong node group is refused with the node groups that exist, or with
`no node is in node group "nosuch", because no node in the fleet states a node group.`
A time flag without a node name is refused with
`event time options require mmwatchdog fleet NODE; name the node whose events you want`.

**JSON.** `mmwatchdog fleet --json` prints `exit_code`, `hub_address`,
`hub_outage_alerter_nodes`, `node_group` (with `--node-group`),
`refused_joins` (`count`, `address`, `last`), `storage_error`,
`measurement_history_of_nodes`, `set_aside_finding_count` and
`nodes_with_set_aside_findings` (the numbers of the line `Set aside:`),
and `nodes`. Each node has `name`,
`state`, `exit_code`, `has_fleet_hub_role`, `is_hub_outage_alerter_node`,
`prefer_this_node_as_hub_outage_alerter`, `this_node_is_in_node_groups`,
`joined_at`, `joined_from_address`, `last_report_received_at`,
`clock_offset_ns`, `replaced_silent_node_of_same_name_at`, `conflict`
(`claimed_from_address` and `at`), `report`, the node's latest status
report without its checks, and `set_aside_findings`: each finding of the
node that the hub sets aside now, with `check`, `finding`,
`exact_finding`, and `reason`. A set-aside finding is not in
`report.summary.attention`, and does not count for `exit_code` or
`report.summary.exit_code`. `refused_joins` has
`last_refused_from_address` after a refused join. `mmwatchdog fleet NODE
--json` prints one node with the checks in `report.checks` and its events
in `events`. A field without a value is left out.

### Another program on the hub reads the fleet view

A program on the hub's machine, such as a dashboard, can read the output
of `mmwatchdog fleet --json`. The command must run as root, because
MMWatchdog's socket `/run/mmwatchdog/mmwatchdog.sock` belongs to root with
mode `0600`. Give the program a systemd socket unit that only the
program's account can connect to. For each connection, systemd runs
`mmwatchdog fleet --json` as root and sends the output back. A program
whose service runs with `NoNewPrivileges=yes` cannot use `sudo`, and this
pair of units needs no sudoers rule. For example, for a program that runs
as the account `myprogram`:

```ini
# /etc/systemd/system/myprogram-fleet-view.socket
[Socket]
ListenStream=/run/myprogram-fleet-view.sock
SocketUser=myprogram
SocketGroup=myprogram
SocketMode=0600
Accept=yes

[Install]
WantedBy=sockets.target
```

```ini
# /etc/systemd/system/myprogram-fleet-view@.service
[Service]
ExecStart=/usr/local/bin/mmwatchdog fleet --json
StandardInput=socket
StandardOutput=socket
StandardError=journal
PrivateNetwork=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
CapabilityBoundingSet=
RestrictAddressFamilies=AF_UNIX
```

The account `myprogram` connects to `/run/myprogram-fleet-view.sock`, and
systemd starts one `myprogram-fleet-view@` service for the connection.
The service runs `mmwatchdog fleet --json` as root, with a read-only file
system, no network, and no capabilities. Root still reaches MMWatchdog's
socket without capabilities, because root owns the socket. The account
cannot choose another command, such as `mmwatchdog fleet forget web02`.
MMHotline's `mmhotline install` writes such a pair of units itself.

The program reads these fields of each entry in `nodes`:

| Field | What the field holds |
|---|---|
| `name` | The node's name. |
| `state` | `reporting`, `waiting_for_first_report`, or `silent`. |
| `exit_code` | 0 healthy, 1 a failure, 3 unknown health. |
| `this_node_is_in_node_groups` | The node's node groups. |
| `last_report_received_at` | When the hub received the node's last report. |
| `report.summary.attention` | Each check that needs attention: `check`, `finding` for one finding of a check, `state`, `since` (left out for a check that has not run yet), and `message`. A healthy check is not listed, so the answer stays small in a large fleet. |

`hub_outage_alerter_nodes` at the top level names the nodes that send an
alert themselves when they cannot reach the hub.

Exit codes: the worst exit code of the nodes shown, 1 before 3, and 3 for
a storage problem of the hub. A finding below `sos` that is set aside
counts for neither. `fleet NODE` exits with that node's code. 4
on a node that is not the hub, or for a wrong argument.
[Watch a fleet](../how-to/watch-a-fleet.md) reads each form.

## mmwatchdog fleet readings

`mmwatchdog fleet readings` runs on the hub and shows the readings that
joined nodes sent, in the same form as
[`mmwatchdog readings`](#mmwatchdog-readings):

```text
mmwatchdog fleet readings NODE CHECK_GROUP [CHECK] [--last DURATION | --since TIME [--until TIME]] [--config-dir DIR] [--json | --chart]
mmwatchdog fleet readings --node-group NODE_GROUP CHECK_GROUP CHECK [--last DURATION | --since TIME [--until TIME]] [--config-dir DIR] [--json | --chart]
```

```text
$ sudo mmwatchdog fleet readings web01 machine memory_is_below_90_percent --last 2m
node web01 · check group machine · Nov 15 10:27:47 to 10:29:47 UTC

memory_is_below_90_percent
memory_used_percent, percent · fails above 90

  3 readings, none beyond the limit
    10:28:14   18.91
    10:28:45   19.1
    10:29:14   19.3
  lowest 18.91 · highest 19.3 at 10:29:14 · latest 19.3 at 10:29:14
```

- The node group form needs one check, and compares every node in the
  node group, also silent nodes. The worst node comes first, and the
  healthy nodes share one line.
- A node without readings in the span says
  `no readings in this time span; choose a longer --last or an earlier --since`.
- When you name the hub itself, the command reads the hub's own history.
- The flags, the JSON, and the exit codes are those of
  [`mmwatchdog readings`](#mmwatchdog-readings).
  [Read the readings of every node on the hub](../how-to/look-back-at-measurements.md#read-the-readings-of-every-node-on-the-hub)
  explains the node group view.

## mmwatchdog fleet findings

`mmwatchdog fleet findings` runs on the hub and lists every node with a
finding of one check, with no limit on the count. An update about nodes
that a known exact finding newly affects names at most 20 nodes for each
exact finding, and this command lists all of them.

```text
mmwatchdog fleet findings CHECK_GROUP/CHECK [--finding ID] [--json]
```

```text
$ sudo mmwatchdog fleet findings installed_packages/known_exploited_flaws
installed_packages/known_exploited_flaws: 1 finding on 3 nodes
CVE-2026-123158 openssh
  Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z: 2 nodes
    app001                   failed       warn  failing since 2026-11-22T22:51:12Z set aside (Needs musl without PAM)
    db01                     failed       sos   failing since 2026-11-22T22:52:12Z
  no exact finding: 1 node
    util01                   failed       warn  failing since 2026-11-23T00:14:00Z (node silent)
```

- For each finding, the command lists its exact findings, and below each
  exact finding every node that has it: the finding's state and severity,
  when its failing time began, the node's state when the node is not
  `reporting`, and the reason when the hub sets the finding aside. The
  command reads every node's latest saved report, also the hub's own and
  those of silent nodes.
- `--finding ID` lists only the finding with that `id`, such as
  `--finding "CVE-2026-123158 openssh"`.
- A finding without an exact finding, such as one from a node older than
  0.2.0, is listed under `no exact finding`.

**JSON.** `check`, and `findings`: each with `finding` and `nodes`. Each
node has `node`, `node_state`, `state`, `alert_severity`,
`exact_finding`, `failing_since`, `set_aside`, and `set_aside_reason`. A
field without a value is left out.

Exit codes: 0, or 4 on a node that is not the hub, or for a wrong
argument, such as a check that is not written as `CHECK_GROUP/CHECK`. [Exact findings](../changing-mmwatchdog/design/checks.md#exact-findings)
says which modules supply an exact finding.

## mmwatchdog fleet set-aside-findings

`mmwatchdog fleet set-aside-findings` runs on the hub and lists each entry
of `/etc/mmwatchdog/set_aside_findings.yml` that the hub uses now, so a
deploy can check that the hub has the entries that the project holds:

```text
mmwatchdog fleet set-aside-findings [--json]
```

```text
$ sudo mmwatchdog fleet set-aside-findings
installed_packages/known_exploited_flaws: CVE-2026-123158 openssh
  Exact finding: Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z
  Reason: Needs musl without PAM; every server with this exact finding runs glibc and sshd links PAM. Debian rates it unimportant.
  Investigation note: triage_agent_notes/2026-11-23_openssh_package_warning_on_app001.md
  Set aside by triage agent (MMHotline run b82fdd8b) on 2026-11-23
  Covers 2 nodes now: app001, web01
  Matches a node now
  Nodes that cannot check now: 0
```

Without an entry, the command prints
`No finding is set aside: the hub has no entry in /etc/mmwatchdog/set_aside_findings.yml.`

For each entry the command shows its check, finding, exact finding,
reason, investigation note, who set it aside, and when, and:

- how many nodes the entry covers now, with up to 20 of them;
- whether the entry matches no node now, over every node's latest saved
  report, also the hub's and those of silent nodes;
- how many nodes cannot check now: nodes whose check is `unknown`, nodes
  that wait for their first report, and nodes without a saved report.
  Remove an entry only when it matches no node and this count is 0,
  because one of those nodes may still have the exact finding.

An entry covers only incidents below `sos`; a node whose finding is `sos`
alerts, whatever the file says, and is not counted as covered.

**JSON.** `set_aside_findings`, a list in the order of the file. Each
entry has `check`, `finding`, `exact_finding`, `reason`,
`investigation_note`, `set_aside_by`, `set_aside_on`,
`covered_node_count`, `covered_nodes` (up to 20), `matches_no_node_now`,
and `nodes_that_cannot_check_now`.

Exit codes: 0, or 4 on a node that is not the hub, or for a wrong
argument.
[Set a finding aside](../changing-mmwatchdog/design/fleet.md#set-a-finding-aside)
gives every rule, and the
[configuration files reference](configuration-files.md#the-set-aside-findings-file)
the format of the file.

## mmwatchdog fleet join-code, rotate-join-code, and forget

These three commands run on the hub and change who may join the fleet.

| Command | What it does |
|---|---|
| `mmwatchdog fleet join-code` | Prints the join code, a secret that lets a node join the hub. `--json` prints `{"join_code": "..."}`. |
| `mmwatchdog fleet rotate-join-code` | Makes a new join code and prints it. Nodes that have joined stay joined. A node that joins later needs the new code. `--json` as for `join-code`. |
| `mmwatchdog fleet forget NODE [--json]` | Removes the node and its data from the hub, and prints `Removed node NODE from the fleet; if MMWatchdog still runs on the removed node, the node joins again with the current join code.` With `--json` it prints `{"removed_node": "NODE"}`. |

Treat the join code as a password: do not paste the output of
`join-code` or `rotate-join-code` into a chat or a ticket.

Exit codes: 0, or 4.
[Change a fleet](../how-to/change-a-fleet.md) uses each command, in
[Remove a node](../how-to/change-a-fleet.md#remove-a-node) and
[Rotate the join code](../how-to/change-a-fleet.md#rotate-the-join-code).

## mmwatchdog describe-module

`mmwatchdog describe-module` lists every module that the node has, under
its kind, with one line each. The built-in modules come by topic.

```text
$ mmwatchdog describe-module
Core modules (built into MMWatchdog), by topic

Services and protocols
  http          a check   Sends one HTTP or HTTPS request and checks the status code and, when asked, the content of the answer.
  tcp           a check   Opens a TCP connection. The check is healthy when the connection opens. It can also send a text and expect a reply.
...
Official extra modules (Starlark, shipped with MMWatchdog 0.1.1)
  apache_status   a check                Reads the status page of an Apache web server, which mod_status serves, and measures how many of Apache's workers are busy.
  discord         an alert destination   Delivers each alert to a Discord channel through a webhook.
...
```

`mmwatchdog describe-module NAME` prints the whole page of one module:
what it checks or where it delivers alerts, its inputs, measurements, and
examples, how it works, its errors, and why it works this way. On a
terminal, the page goes through the pager that `$PAGER` names, or
`less -FRX`. When the pager cannot start, the command prints the page.

- A built-in module's page is built into the program, and is the same as
  its page among the [module pages](modules/README.md).
- An official extra module's page is the `README.md` in the module's
  directory.
- For a module of your own, the command prints the module's inputs, then
  its `README.md` if there is one.
- A module that reports [findings](writing-modules.md#findings), such as
  `every_systemd_unit`, has the line `One run of the check can report
  several findings, and each finding is its own incident.` below its kind.

| Flag | Use |
|---|---|
| `--reference-only` | With a name: only the module's kind, inputs, measurements, and examples. |
| `--config-dir DIR` | The configuration whose node settings name the module directories. |
| `--official-extra-module-dir DIR`, `--external-module-dir DIR` | Look in these directories instead, such as the project's `mmwatchdog_external_modules` on your own machine. |
| `--json` | With a name: `name`, `kind` (`core`, `official_extra`, `project_starlark`, or `project_executable`), `topic` for a built-in module, `directory` for the others, `description` (the inputs, measurements, and examples), and `page`, which `--reference-only` leaves out. Without a name: the list `modules`, and `problem` for a module that MMWatchdog cannot use. |

```text
$ mmwatchdog describe-module node_resource --reference-only
node_resource: a core module (built into MMWatchdog), used as a check
Measures one resource of the node: its CPU, its memory, its swap space, its load, the time that programs wait for a disk, the memory pressure, or its open files.

Value
  (the value of node_resource)   cpu | memory | swap | load | io_wait | memory_pressure | open_files   required   The resource to measure.

Measurements
  cpu_used_percent           percent         For cpu: the share of the node's processor time that was in use since the run before.
  memory_used_percent        percent         For memory: the share of the node's memory that is not available to programs.
...

Example: Fail when more than 90 percent of the node's memory is in use.
  node_resource: memory
  fail_when_measurement_above:
    memory_used_percent: 90
  alert_severity: warn
```

Exit codes: 0. 4 for an unknown module, whose error lists the modules in
each directory, or `--reference-only` without a name.
[Add a check](../how-to/add-a-check.md) uses it to find a module.

## mmwatchdog module-usage

`mmwatchdog module-usage` lists the modules that the configuration
names, under their kind, with the file and line of each check or
destination that uses them. A module that is not built in also shows its
directory, and `Starlark` or `executable`.

```text
$ mmwatchdog module-usage
Core modules (built into MMWatchdog)
  node_filesystem   /etc/mmwatchdog/check_groups/node.yml:19, /etc/mmwatchdog/check_groups/node.yml:25
  node_resource     /etc/mmwatchdog/check_groups/node.yml:10, /etc/mmwatchdog/check_groups/node.yml:13

This node has joined a hub, and its alert destinations come from the hub. Run mmwatchdog module-usage on the hub to see the modules of the destinations.
```

- An executable of your own has two more lines:
  `MMWatchdog does not see what an executable does on this node.` and
  `The local node must provide everything the executable needs.` A
  Starlark script reaches the node only through the functions that
  MMWatchdog gives it. An executable does whatever its author wrote.
- `mmwatchdog module-usage NAME` shows one module. A module that no check
  or destination uses is refused with exit code 4.
- The flags `--config-dir`, `--official-extra-module-dir`, and
  `--external-module-dir` work as for `describe-module`.
- `--json` prints the list `modules`, each with `name`, `kind`,
  `directory`, `description`, and `called_at` (`file`, `line`, `column`),
  and `alert_destinations_come_from_the_hub`.

Exit codes: 0, or 4.

## mmwatchdog run-starlark-module and module-json

These commands help you write a module of your own. They need no service
and no `sudo`.

| Command | What it does |
|---|---|
| `mmwatchdog run-starlark-module MODULE_DIR` | Runs the script `module.star` of the module in `MODULE_DIR` once: it reads one request from standard input and writes the result to standard output. MMWatchdog runs every Starlark module this way. |
| `mmwatchdog module-json request`, `get`, `result`, `object` | Read the request and write the result of an executable module, from a Bash script. `result --exact-finding ID TEXT` gives a finding its exact finding. MMWatchdog gives the executable the path of the program in `MMWATCHDOG_MODULE_HELPER`. |

[Writing modules](writing-modules.md) describes the request, the result,
and each `module-json` command with its flags.

Exit codes: `run-starlark-module` 0, or 4 for a script with an error.
`module-json` 0, 1 for data that is not valid, or 4 for wrong arguments.

## mmwatchdog daemon

`mmwatchdog daemon` is the node monitoring process. The systemd unit
`mmwatchdog.service` starts it as root, so you do not run it yourself.
`--config-dir DIR` names the configuration, `/etc/mmwatchdog` by default.

It refuses to start without root, and on a machine that systemd did not
start, because the checks of systemd units, the restarts, and the checks
of the journal need systemd. [Installed files](installed-files.md#the-program-and-the-service)
shows the unit.

## mmwatchdog version and help

`mmwatchdog version` prints the version, the source revision, the system,
and whether the source had changes that were not committed:

```text
$ mmwatchdog version
mmwatchdog 0.1.1 (ad75e5909a0af9b03215d2b2ac4c5bdfa9a56777, linux/amd64, modified=false)
$ mmwatchdog version --json
{
  "arch": "amd64",
  "go": "go1.27.4",
  "modified": false,
  "os": "linux",
  "revision": "ad75e5909a0af9b03215d2b2ac4c5bdfa9a56777",
  "version": "0.1.1"
}
```

`mmwatchdog help`, `mmwatchdog --help`, and `mmwatchdog` alone print every
command form and the exit codes, and exit with 0. An unknown command exits
with 4: `mmwatchdog: unknown command "nosuch"; use mmwatchdog --help`.

## Exit codes

Every command exits with one of these codes. Each command's entry says
which codes it uses.

| Code | Meaning |
|---|---|
| `0` | Success, or healthy. A check in `confirming_failure`, `waiting_for_first_heartbeat`, or `waiting_for_first_report` counts as healthy. |
| `1` | Failure: a check with an open failure incident (`status`), a failed run (`check-now`), a silent node (`fleet`), an invalid configuration (`validate`), a destination that failed its verification (`reload`), a notice that was not delivered at the first attempt (`send-notice`), or a hub that did not get a maintenance change (`maintenance`). |
| `2` | Not used. |
| `3` | Health unknown: a check that is `unknown`, a destination that fails, or a storage problem on the node or the hub. |
| `4` | The command could not do its job: a wrong command, argument, or flag, a service that cannot be reached, a refused request, or a change that could not be saved on the node's disk, so the change did not happen. Also a configuration that cannot be loaded, except for `validate`. |
| `130` | Interrupted with Ctrl-C. |

When a command finds both a failure and an unknown, 1 wins over 3, in
every command, `mmwatchdog status` and `mmwatchdog fleet` included.

## Check states

Each check has one state at a time, which `mmwatchdog status` shows after
the check's name, in `state` in JSON, and on the hub in
`mmwatchdog fleet NODE`. The state comes from the latest runs and from the
check's timings, which [Decide when a check
alerts](../how-to/decide-when-a-check-alerts.md) explains.

| State | What it means | How a check gets there | Exit code of `status` |
|---|---|---|---|
| `ok` | The check is healthy, outside a failing time, and has no open incident. | Its latest run was healthy, and any failing time has ended. | 0 |
| `confirming_failure` | The check is in a failing time, but its alert has not gone out yet. | A run failed, and the check has failed for less than `alert_after_failing_for`. A check that fails on and off stays here while its latest run is healthy, until it has been healthy for `recovered_after_healthy_for`. | 0 |
| `failed` | A failure incident is open, and its alert went out. | The failing time lasted `alert_after_failing_for`. The check stays `failed` when its latest run is `unknown`: the text then says `no current result; the failure alert stays open`. | 1 |
| `confirming_recovery` | A failure incident is open, but the latest run is healthy. | A run of a `failed` check was healthy. The incident closes, and the recovery alert goes out, once the check has been healthy for `recovered_after_healthy_for`. | 1 |
| `unknown` | No current run says whether the check is healthy. | MMWatchdog could not check, such as a run that reached its time limit or a command that Bash cannot find. Or the latest result stopped counting, and the check waits for its next run: after MMWatchdog starts, after a reload changes the check, at the start or end of a maintenance window, and after a restart or a recovery command. The text then says `waiting for a new run; the earlier result no longer counts:`. A check that stays `unknown` for `alert_after_failing_for` opens a cannot-check incident, with a `cannot_check` event and later a `can_check_again` event. | 3 |
| `waiting_for_first_heartbeat` | A heartbeat check has not received its first heartbeat, and its first deadline has not passed. | The check is new, or MMWatchdog started without a saved heartbeat. Waiting is no proof that the job succeeds. | 0 |
| `waiting_for_first_report` | A check of the `node_reports` check group has no first report yet, and its first deadline has not passed. | On the hub, a node joined, or the hub started. On a joined node, the node has not reached the hub yet. | 0 |

- A heartbeat check never waits for a fresh run: MMWatchdog saves each
  heartbeat on the node's disk before it answers the job, so a heartbeat
  still counts after a restart.
- A finding, such as each failed unit of an `every_systemd_unit` check,
  has its own state and its own incident. The check is `failed` while one
  of its findings has an open incident.
- The state of a node in a fleet, `reporting`, `waiting_for_first_report`,
  or `silent`, is in [`mmwatchdog fleet`](#mmwatchdog-fleet).

## Related pages

- [See what is happening](../how-to/see-what-is-happening.md): `status`,
  `events`, and `check-now` step by step.
- [Configuration files](configuration-files.md): every setting that these
  commands read.
- [Alert format](alert-format.md): the fields of an event and of an alert.
- [When something fails](../when-something-fails.md): start from a
  symptom, such as an alert that did not arrive.
