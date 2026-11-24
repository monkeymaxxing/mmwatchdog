# MMWatchdog: check groups and checks

`docs/changing-mmwatchdog/design/checks.md` owns what MMWatchdog observes: named check groups and
their checks. The design covers timing and confirmation, checks with a limit
on a number, [modules](#modules) of every kind, and heartbeats.
Each module's own design,
from what it reads to why it works that way, is on its page in the
[module index](../../reference/modules/README.md). Recovery policy belongs to
[recovery](recovery.md), and alert delivery to [alerts](alerts.md).

The [product principles](README.md#product-principles) require checks that
many servers need to be built into MMWatchdog. An uncommon check is an
[official extra module](#official-extra-modules). A person's own application
gets a [`shell`](../../reference/modules/shell.md) check or a
[module of the project's own](#the-projects-own-modules).

## Named check groups and checks

A check group is a named set of related checks. The checks in a check group
share four timings. A check group can configure restarts of one
[systemd unit](recovery.md#restart-a-systemd-unit), one
[recovery command](recovery.md#run-a-recovery-command), and a
[maintenance window](recovery.md#maintenance-windows) for the check group.
The checks of one
check group can be about one systemd unit, about several units, or about the
node itself: a check group for an application can combine the state of the
application's unit, the application's HTTP health, and a limit on the unit's
memory. Each check keeps its own state, explanation, alert severity, and
incident. Inspection shows the checks together under the check group's name:

```text
myapp
  myapp_service_is_running        ok      systemd unit myapp.service is active
  myapp_is_not_crash_looping      ok      0 automatic restarts in the last hour
  myapp_answers_health_request    failed  expected HTTP 200, received 503
  myapp_memory_is_below_512_mib   ok      memory_used_mib is 184 (fails above 512)
```

Checks have explicit names under `checks`, chosen by the author. Names in
the MMWatchdog design documents state health, such as
`myapp_service_is_running`. Then `failed` beside a name reads as a sentence.
A check group with one check can instead write that check directly on the
check group. The direct check is named after the check's [module](#modules): `api/http`, `root_filesystem/node_filesystem`,
`database_port/tcp`, `myapp_selftest/shell`, or `jobs/queued_jobs`.
Heartbeats are their own named check groups, each with a check named
`heartbeat`. Every check that can alert states its
[`alert_severity`](#alert-severity).

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
      myapp_answers_health_request:
        http:
          url: http://127.0.0.1:8080/health
          expected_http_status_codes: [200]
          response_body_contains: ready
        alert_severity: sos
      myapp_memory_is_below_512_mib:
        systemd_unit_resource:
          systemd_unit: myapp.service
          resource: memory
        fail_when_measurement_above:
          memory_used_mib: 512
        alert_severity: warn
  database_port:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    tcp: 127.0.0.1:5432
    alert_severity: sos
```

The check group `myapp` has three named checks. The service-state check asks
systemd whether `myapp.service` runs. The HTTP check requests the
application's health page. The memory check fails when `myapp.service`
exceeds the configured memory threshold. The check group `database_port`
has one check, written directly on the group and shown as
`database_port/tcp`. Every unit
check names its unit on its own line, so each line can be read alone.

Check group and check names are stable identifiers in configuration,
inspection, history, and alerts. A successful observation of one check
never clears another's failure. The check group names `every_check_group` and
`node_reports` are reserved.

Check groups live in `/etc/mmwatchdog/check_groups/*.yml`, each file holding a
`check_groups:` mapping. MMWatchdog loads every `.yml` file in
`/etc/mmwatchdog/check_groups/`. A file in `/etc/mmwatchdog/check_groups/`
without the `.yml` extension, including `.yaml`, is a validation error.
Duplicate check group names are errors naming the locations of the
first definition and the duplicate definition. At least one check group is required.
Node settings, including the required
`alert_severity_when_this_node_goes_silent` ([silent nodes](fleet.md#silent-nodes)), belong in
`/etc/mmwatchdog/mmwatchdog_node_settings.yml`, which never contains check groups. Alert
settings use a `destinations:` mapping in
`/etc/mmwatchdog/alert_destinations.yml`.
`/etc/mmwatchdog/alert_destinations.yml` must not exist on a node with
`join_fleet_hub`, which receives alert settings from the hub.
Commands that read configuration use `--config-dir DIR` (default
`/etc/mmwatchdog`) to select the complete set of configuration files.
To validate a staged candidate, point `mmwatchdog validate --config-dir DIR` at a
directory containing the complete candidate set.

## Timing and confirmation

MMWatchdog runs each check again and again, and each run produces one observation.
A check group with checks that MMWatchdog runs normally states four timings.
An event check written directly on the check group's mapping omits
`alert_after_failing_for` ([the failing time](#the-failing-time)). There are no
node-wide defaults, so a reader finds every timing in the file that holds the
check group. A [heartbeat](#heartbeats) check group takes no `check_every`,
`time_limit_per_run`, `alert_after_failing_for`, or
`recovered_after_healthy_for`,
because MMWatchdog does not run a heartbeat.

| Setting | Allowed values | Meaning |
| --- | --- | --- |
| `check_every` | At least `100ms` | How often each check runs |
| `time_limit_per_run` | Positive and at most `check_every` | How long one run of a check may take |
| `alert_after_failing_for` | `0s` or longer | How long a check must be failing before its incident opens and its failure alert goes out |
| `recovered_after_healthy_for` | `0s` or longer | How long a check must be healthy before its [failing time](#the-failing-time) ends and an open incident resolves |

A duration is a number with a unit, such as `30s`, `5m`, `26h`, or `7d`.

A check may state `alert_after_failing_for`, `recovered_after_healthy_for`,
or both waiting times. For each waiting time omitted from a check, the check
group's value applies. `mmwatchdog config` shows each check's effective
timings and where each timing comes from.

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 1m
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
        alert_severity: sos
      myapp_memory_is_below_512_mib:
        systemd_unit_resource:
          systemd_unit: myapp.service
          resource: memory
        fail_when_measurement_above:
          memory_used_mib: 512
        alert_severity: warn
        alert_after_failing_for: 10m
        recovered_after_healthy_for: 5m
```

All three checks run every 30 seconds, and one run may take 5 seconds. The
first two checks alert after 5 minutes of failing and recover after 1 healthy
minute, the values of the check group. The memory check states its own
waiting times: the memory check alerts after 10 minutes above the limit and
recovers after 5 minutes below the limit.

### The failing time

A check's failing time lasts from its first failed observation until the
check has been healthy for `recovered_after_healthy_for`.
A healthy period shorter than `recovered_after_healthy_for`
does not end the failing time. A check that fails on and off is therefore
treated as failing the whole time. A failed observation starts the count of
healthy time over.

Every waiting time of a check counts from the start of the check's failing
time:

| Setting | What happens when the failing time has lasted the setting's duration |
| --- | --- |
| `alert_after_failing_for` | The check opens a failure incident and queues its failure alert, at its next failed observation |
| [`restart_after_failing_for`](recovery.md#restart-a-systemd-unit) | The restart of the systemd unit becomes due, at the check's next failed observation |
| [`run_after_failing_for`](recovery.md#run-a-recovery-command) | The recovery command becomes due, at the check's next failed observation |
| [`raise_to_sos_after_failing_for`](#alert-severity) | The check's alert is raised to `sos` |

A check's lifecycle is:

1. The first failed observation starts the failing time.
2. When the failing time has lasted `alert_after_failing_for`, the check
   opens a failure incident and queues its failure alert at its next failed
   observation. A healthy observation never opens an incident.
3. A continuing failure sends no further alert except
   [reminders](alerts.md#reminders).
4. When the check has been healthy for `recovered_after_healthy_for`, the
   failing time ends. An open incident resolves, and its recovery alert is
   queued.

With the check group above, a health request that fails on every second run
gives:

| Time | Observation | Result |
| --- | --- | --- |
| 12:00:00 | failed | The failing time starts |
| 12:00:30 | healthy | Healthy for less than 1 minute, so the failing time continues |
| 12:01:00 to 12:04:30 | failed and healthy in turn | The failing time continues |
| 12:05:00 | failed | The failing time has lasted 5 minutes: the incident opens and the failure alert is queued |
| 12:09:30 | healthy | The first observation of a lasting recovery |
| 12:10:30 | healthy | Healthy for 1 minute: the failing time ends, the incident resolves, and the recovery alert is queued |

When a check failed on and off, the body of its alert says so and says how
many of the last runs failed, such as `failing on and off since 12:00; 5 of
the last 10 runs failed`. There is no rule that counts failed runs, such as
"4 failures in the last 6 runs". MMWatchdog uses `alert_after_failing_for`
and `recovered_after_healthy_for` to handle intermittent failures by elapsed
time rather than failed-run counts.

`alert_after_failing_for: 0s` opens an incident on the first failed
observation, and `recovered_after_healthy_for: 0s` ends the failing time on
the first healthy one.

With a positive `alert_after_failing_for`, one failed observation followed
by health sends no alert. The check's failing time lasts until the check has
been healthy for `recovered_after_healthy_for` and then ends without an
incident, because no failed observation came after `alert_after_failing_for`.

An observation is `ok`, `failed`, or `unknown`. A check shows
`confirming_failure` while the check's failing time has not yet lasted
`alert_after_failing_for`.
A check shows `confirming_recovery` while the check is healthy with
an open failure incident that has not resolved yet. A healthy check outside a
failing time is `ok`.

Unknown means MMWatchdog could not establish health: a command that could not run, a
module whose result is not valid, or an unreadable measurement. Unknown
observations open a cannot-check incident after `alert_after_failing_for`
on a separate `cannot_check_incident` lifecycle, rather than a
`failure_incident`. The first `ok` or `failed` observation resolves a
cannot-check incident. Unknown never ends a confirmed failure, never
counts as healthy time, and never triggers recovery actions. An open failure
incident stays `failed` in inspection while the check's latest observation is
unknown.

A missed or late scheduled run, or an unknown result, keeps the failing time
running and starts healthy time over. A failure followed by unknown runs is
still unresolved; otherwise alternating failed and unknown runs could hide
an outage forever. Only a failed observation opens the failure incident.
A completed recovery action keeps the cannot-check timer running too:
an attempted fix proves neither health nor an ability to check.

A daemon start ends any failing time whose incident has not opened yet.
Starting or ending maintenance ends a failing time whose incident has not
opened yet.
Open incidents stay open until MMWatchdog observes
health for `recovered_after_healthy_for`.

An event check opens an incident at the first event. Opening an incident at
the first event applies to `out_of_memory_kills` and to `log_lines` with
either new-line condition.
A `log_lines` check with a new-line condition and an `out_of_memory_kills`
check each refuse `alert_after_failing_for` on the check.
The event checks open their failure incident without the check group's
`alert_after_failing_for` because the event has already happened and must
not disappear between runs without an alert. A run that cannot read the
events is no event, so a named event check's cannot-check incident waits
the check group's `alert_after_failing_for`, as any other check's does: a
log file that is being rotated for a moment sends nothing, and a check that
stays unable to read its events alerts once the wait has passed.

`recovered_after_healthy_for` starts at the first run that finds no new
event. With runs on schedule, the `recovered_after_healthy_for` wait can
therefore begin up to one
`check_every` after the last run that reports an event. MMWatchdog counts
observed health, not an assumed event-free interval. A log silence check
uses the ordinary failing time.
See [Decide when a check alerts](../../how-to/decide-when-a-check-alerts.md).

A timing-only reload keeps the identity of what the check observes, its
incidents, and its module state, including log position and the last
out-of-memory count. A new schedule must not discard an unread event.
When `check_every` changes, the next run is due at the last scheduled start
plus the new interval, or on the next scheduler tick if already overdue.
A shorter interval must not leave an outage unseen until the old interval
ends ([scheduling](../../how-it-works.md#scheduling)).
Removing `alert_severity` closes open incidents as `closed_without_recovery`;
choosing readings only does not prove recovery.

Module pages that rely on Timing and confirmation:
[`http`](../../reference/modules/http.md), [`ping`](../../reference/modules/ping.md),
[`tls`](../../reference/modules/tls.md), [`memcached`](../../reference/modules/memcached.md),
[`redis`](../../reference/modules/redis.md), [`postgresql`](../../reference/modules/postgresql.md),
[`pgbackrest`](../../reference/modules/pgbackrest.md),
[`mysql`](../../reference/modules/mysql.md),
[`sqlite_database`](../../reference/modules/sqlite_database.md),
[`docker_container`](../../reference/modules/docker_container.md).

## Alert severity

Each check's alerts carry an [alert severity](alerts.md#alert-severity):
`info`, `warn`, or `sos`. Destinations take alerts by severity, and only
`sos` wakes the person on call through MMHotline. Two settings, written on
each check, decide the severity of the check's alerts:

| Setting | Meaning |
| --- | --- |
| `alert_severity` | Required: the severity of the check's alerts, `info`, `warn`, or `sos` |
| `raise_to_sos_after_failing_for` | Optional: raise the check's alert to `sos` when the failing time has lasted `raise_to_sos_after_failing_for`, at least `1s` |

Write `alert_severity` and optional `raise_to_sos_after_failing_for` on the
check itself. A named check is under `checks`. A direct check or a heartbeat
is on the check group's mapping, which is the check.

There is no default and no inheritance for `alert_severity` or
`raise_to_sos_after_failing_for`. A check group of named checks refuses
`alert_severity` and `raise_to_sos_after_failing_for` on the check group.
The validation message says to set the rejected setting on each check.
A check without `alert_severity` is a validation error
such as
`check_groups.myapp.checks.myapp_answers_health_request.alert_severity:
required: info, warn, or sos`. An empty value is refused as well. A reader
under pressure sees a check's severity on the check itself, and no group or
node default can hide the severity of the alert for a stopped unit.

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
      myapp_answers_health_request:
        http:
          url: http://127.0.0.1:8080/health
        alert_severity: sos
      myapp_memory_is_below_900_mib:
        systemd_unit_resource:
          systemd_unit: myapp.service
          resource: memory
        fail_when_measurement_above:
          memory_used_mib: 900
        alert_severity: warn
```

`raise_to_sos_after_failing_for` works on every check, whatever its module.
Like every waiting time, `raise_to_sos_after_failing_for` counts from the
start of the [failing time](#the-failing-time):
in the example, a crash loop that has lasted 30 minutes is raised to `sos`.
A value at or below the check's `alert_after_failing_for` makes the failure
alert itself go out as `sos`. A cannot-check incident is raised to `sos`
when MMWatchdog has been unable to observe the check for
`raise_to_sos_after_failing_for`. When MMWatchdog
cannot run a check, its cannot-check alert has the check's current severity,
including a raise of the check's open failure incident, so no one is left
blind on an important check.
An open incident's severity never falls back: changing
`alert_severity` to a higher value raises open incidents at once, and a lower
one waits for the next incident.

Escalation by level is a second check with a higher limit and a higher
severity, one generic rule for every check:

```yaml
# /etc/mmwatchdog/check_groups/root_filesystem.yml
check_groups:
  root_filesystem:
    check_every: 1m
    time_limit_per_run: 5s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 5m
    checks:
      root_filesystem_is_below_85_percent:
        node_filesystem: /
        fail_when_measurement_above:
          space_used_percent: 85
        alert_severity: warn
      root_filesystem_is_below_95_percent:
        node_filesystem: /
        fail_when_measurement_above:
          space_used_percent: 95
        alert_severity: sos
```

The first check sends a `warn` alert when the filesystem at `/` is more than
85 percent full. The second check sends an `sos` alert above 95 percent.
To be told again while a value keeps growing, use a
[reminder](alerts.md#reminders), which carries the latest reading.

A [measurement check](#checks-with-a-limit-on-a-number) without a limit and
with `only_record_readings: true` records its readings and never alerts; a reading
it cannot make shows as unknown in status without opening an incident.
Inspection, `mmwatchdog status CHECK_GROUP` and `mmwatchdog status --json`, shows each check's
`alert_severity` and `raise_to_sos_after_failing_for` and the current
severity of its open incidents.

Two more settings on a check control who hears about the check.
A check can name the destinations that receive the check's alerts
([which alert goes to whom](alerts.md#which-alert-goes-to-whom)).
A check can stay quiet while the check of the cause fails
([one cause, one alert](#one-cause-one-alert)).

## Scheduling and observed state

MMWatchdog runs independent observations with `max_concurrent_checks` (8 at once by
default), set in `mmwatchdog_node_settings.yml`
([configuration files](operation.md#configuration-files-and-validation)).
MMWatchdog never overlaps two executions of the same check and never replays
a backlog of missed observations. Execution and confirmation count from
actual observation times on the monotonic clock, so a step of the system
clock moves neither a run nor a confirmation
([the system clock](operation.md#the-system-clock)). The scheduler has
100 ms resolution, and normal node checks use intervals of seconds or
minutes.

A run ends at the run's `time_limit_per_run`.
A run blocked in an uninterruptible kernel operation cannot be ended: the
kernel holds an operation on a file of another machine's filesystem whose
server is gone, without end.

MMWatchdog waits for a run for 5 seconds after the run's time limit and then
stops waiting. The check is unknown,
and its message says that the run did not end. The run no longer counts
among `max_concurrent_checks`, so one filesystem that does not answer
never stops the other checks of a node.

While a check's previous run has not ended, MMWatchdog starts no new run of
the check. Each run that MMWatchdog does not start is unknown. The unknown
observation's message names the time at which the old run started.
MMWatchdog drops the old run's result when the old run ends.

| Decision | Reason |
| --- | --- |
| MMWatchdog gives the place of a run that does not end back | A node with 8 places and 8 files on a dead network filesystem would otherwise run no check at all, and would say so about none of them |
| No new run of the check before the old one ends | Each new run would also wait for the kernel operation, and the waiting runs would grow by one at each interval without end |
| The check is unknown, not failed | MMWatchdog learned nothing about the file. Unknown for `alert_after_failing_for` sends the cannot-check alert, which is the truth |
| 5 seconds after the time limit, fixed | A module that responds to cancellation ends the run's programs at the time limit. Ending the programs needs a moment, also on a node under load. A setting would be one more number to get wrong |

Each check retains its latest observation: status, message, time,
duration, exit status, bounded stdout and stderr, and measurements.
An observation older than the check's schedule allows is shown as
needing a fresh result;
an old success never keeps a check green. Incidents freeze their
observation that opens the incident as evidence, so a later success does not
erase the failure's diagnostics.
[Status and diagnosis](operation.md#status-and-diagnosis) explains how
inspection shows the latest observation and the evidence kept for each
incident. Every reading of a measurement is also
kept in the node's [measurement history](operation.md#measurement-history).

Module pages that rely on Scheduling and observed state:
[`ssh`](../../reference/modules/ssh.md), [`smtp`](../../reference/modules/smtp.md),
[`imap`](../../reference/modules/imap.md), [`pop3`](../../reference/modules/pop3.md),
[`dns`](../../reference/modules/dns.md), [`node_resource`](../../reference/modules/node_resource.md),
[`node_filesystem`](../../reference/modules/node_filesystem.md),
[`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md),
[`node_disk_device`](../../reference/modules/node_disk_device.md),
[`node_network_interface`](../../reference/modules/node_network_interface.md),
[`node_temperature`](../../reference/modules/node_temperature.md).

## Checks with a limit on a number

Some checks measure a number: the memory in use, the space left on a
filesystem, the number of jobs that wait in a queue. Such a check only
measures. The limit is written beside the check's module, and MMWatchdog
applies the limit equally for a [module](#modules) of every kind.

Core percentage limits range from 0 to 100. Core limits other than
temperature limits are nonnegative. Temperature limits can be negative
because hardware can operate below zero Celsius. An unreadable temperature
sensor is unknown unless another sensor proves a failed limit. See
[`node_temperature`](../../reference/modules/node_temperature.md).

```yaml
# /etc/mmwatchdog/check_groups/machine.yml
check_groups:
  machine:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    checks:
      memory_is_below_90_percent:
        node_resource: memory
        fail_when_measurement_above:
          memory_used_percent: 90
        alert_severity: warn
      root_filesystem_is_below_90_percent:
        node_filesystem: /
        fail_when_measurement_above:
          space_used_percent: 90
          inodes_used_percent: 90
        alert_severity: warn
      root_filesystem_has_10_gib_free:
        node_filesystem: /
        fail_when_measurement_below:
          space_free_gib: 10
        alert_severity: sos
      job_queue_is_short:
        queued_jobs:                              # a module of the project's own
          queue_file_path: /var/lib/myapp/pending
        fail_when_measurement_above:
          queued_jobs: 500
        alert_severity: warn
```

The first check fails when more than 90 percent of the node's memory is in
use. The second fails when the filesystem at `/` is more than 90 percent
full, by space or by inodes. The third fails when less than 10 GiB are free
on `/`. The fourth runs a [module of the project's own](#the-projects-own-modules)
that counts the jobs that wait in a queue, and MMWatchdog fails the check above 500
jobs.

- `fail_when_measurement_above` and `fail_when_measurement_below` are maps
  from the name of a measurement to a number. A reading strictly greater
  than its `above` number, or strictly less than its `below` number, fails
  the check. A reading equal to the number passes, and zero is a valid
  number.
- A check can state `fail_when_measurement_above` and
  `fail_when_measurement_below`, with several measurements in each map.
  The check fails when any reading crosses a configured limit. The check's
  message names each measurement, reading, and limit, such as
  `memory_used_percent is 94 (fails
  above 90)`.
- The name of a measurement carries its unit, such as `memory_used_percent`,
  `memory_used_mib`, or `space_free_gib`. The measurement name appears in
  the limit, in alerts, and in charts.
- Every module states the measurements that it reports, a core module in MMWatchdog
  itself and every other module in its
  [module description](#the-module-description). A limit on a name that the
  check's module does not report is a validation error that lists the
  module's measurements.
- A result without a measurement that has a limit makes the observation
  unknown.
  A reading that cannot be made is unknown with an explanation, never a
  zero measurement.
- A module that is not core still decides two things by itself: `failed`,
  when the thing it checks does not work at all, and `unknown`, when it could
  not measure.
- A [`shell`](../../reference/modules/shell.md) check passes or fails and reports no number. A
  check of a team's own application that needs a number is a
  [module of the project's own](#the-projects-own-modules).

The core modules whose job is to measure are `node_resource`,
`node_filesystem`, `node_disk_device`, `node_network_interface`,
`node_temperature`, and `systemd_unit_resource`.
With `only_record_readings: true`, a check with one of the measurement
modules only records readings. The check never fails on a value, never
alerts, and takes no `alert_severity`.
A check with `only_record_readings: true` cannot appear in
`triggered_by_checks`.

With an `alert_severity` instead of `only_record_readings: true`,
`node_filesystem` and `node_network_interface` can also fail without a
limit. A [`node_filesystem`](../../reference/modules/node_filesystem.md) check fails when its
filesystem is not mounted or is read-only, and a
[`node_network_interface`](../../reference/modules/node_network_interface.md) check fails
when its interface has no link.

Every other module that reports measurements fails by itself, with or
without a limit, so its check always states an `alert_severity`. Where
MMWatchdog accepts a limit beside the module, MMWatchdog also fails the
check when a reading crosses the limit. A
[`file`](../../reference/modules/file.md) check fails when its file is missing, a
[`ping`](../../reference/modules/ping.md) check when the machine does not answer, and an
[`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md) check
for each filesystem that is read-only, does not answer, or answers with an
error in place of its size.

Escalation by level is a [second check](#alert-severity) with a higher limit
and severity.

The section "Measurements" of each module's page lists what the module
measures, with the units. The core modules that measure the node and its
parts are [`node_resource`](../../reference/modules/node_resource.md),
[`node_filesystem`](../../reference/modules/node_filesystem.md),
[`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md),
[`node_disk_device`](../../reference/modules/node_disk_device.md),
[`node_network_interface`](../../reference/modules/node_network_interface.md),
[`node_temperature`](../../reference/modules/node_temperature.md), and, for one systemd
unit, [`systemd_unit_resource`](../../reference/modules/systemd_unit_resource.md).

## Reboots

An unexpected reboot, such as a kernel panic or a watchdog reset, alerts even
when everything comes back healthy. MMWatchdog keeps the kernel's boot ID
(`/proc/sys/kernel/random/boot_id`) in its state. When MMWatchdog starts in a
new boot, MMWatchdog records a `rebooted` [event](alerts.md#alert-events)
with no `check_group`. The event's message contains the boot time and the
last time MMWatchdog recorded state before the reboot. The alert includes
the event's message:

```text
app-2 rebooted at 14:03 UTC; MMWatchdog last ran at 14:01 UTC
```

An `every_check_group` [maintenance window](recovery.md#maintenance-windows) open
before the reboot or when MMWatchdog starts again suppresses the reboot
alert notification, so a reboot during a deploy sends nothing. The event
is a notice, not an incident: nothing
recovers from it. MMWatchdog's first start on a node records the boot without
alerting.

Each node sets the severity of the node's reboot alert with the required
setting `alert_severity_when_this_node_has_rebooted`.
A typical value is `info` on an application server among many application
servers. On a database node whose unplanned reboot needs attention, a
typical value is `warn` or `sos`. The setting stands beside
`alert_severity_when_this_node_goes_silent`
([silent nodes](fleet.md#silent-nodes)). Reboot detection needs no other
configuration.

## Protocols and common server software

These core modules check a common service, each named for the protocol or
for the tool: [`tcp`](../../reference/modules/tcp.md) with text to send,
[`udp`](../../reference/modules/udp.md), [`ssh`](../../reference/modules/ssh.md),
[`smtp`](../../reference/modules/smtp.md), [`imap`](../../reference/modules/imap.md),
[`pop3`](../../reference/modules/pop3.md), [`dns`](../../reference/modules/dns.md),
[`memcached`](../../reference/modules/memcached.md), [`redis`](../../reference/modules/redis.md),
[`postgresql`](../../reference/modules/postgresql.md), [`pgbackrest`](../../reference/modules/pgbackrest.md)
for the backups of PostgreSQL, [`mysql`](../../reference/modules/mysql.md) for
MySQL and MariaDB, [`docker_container`](../../reference/modules/docker_container.md), and
[`sqlite_database`](../../reference/modules/sqlite_database.md). Each module's page
describes what its check does, its inputs, and its messages.

The following connection, authentication, and response rules apply to each
module that talks to a server:

- **Every address is written as host:port.** No module has a default port,
  so the check shows where the module connects.
- **A connection that does not open, and an answer that does not arrive
  within `time_limit_per_run`, fail the check.** What a server sent in place
  of the expected answer is in the body of the alert.
- **A check logs in only where the service gives no answer without a
  login,** and then with a password from a file that MMWatchdog reads for each run.
  `ssh`, `smtp`, `imap`, and `pop3` log in nowhere.
- **A module that can encrypt its connection states
  `connection_security`,** as an [`email`](../../reference/modules/email.md) destination
  does. No module has a default for `connection_security`, so each check
  shows how the connection is protected. Redis requires
  `connection_security` even without a login, and accepts `verified_tls` or
  `unencrypted`. The `postgresql` and `mysql` checks accept `verified_tls`
  or `unencrypted` for a network connection. TLS verifies the
  certificate by default; `dangerously_skip_server_certificate_check: true`
  explicitly skips server authentication while keeping encryption. Private
  authorities can be named in the certificate authority file input.
  MariaDB must refuse an unencrypted login before forwarding credentials,
  including when certificate checking is disabled. `unencrypted` is accepted
  for any host, for example
  across an operator's private tunnel.
- **HTTP requests use only the caller's time limit.** A separate connection
  or TLS handshake timeout would cut short a user's `time_limit_per_run`.
  The time-limit rule applies to `http_request` and webhook
  delivery through the shared primitive.
- **A streaming PostgreSQL replica without its primary fails without a limit.**
  `primary_conninfo` set with no row in `pg_stat_wal_receiver` means that
  the replica cannot receive changes. A copied-WAL standby does not require
  a streaming connection to the primary.
- **TXT record values compare exactly; host names ignore case and a final dot.**
  TXT is arbitrary data, so host-name normalization would hide a wrong answer.
- **A TCP reply rule requires a received byte.** A regular expression that
  matches empty text does not establish that the server answered.
- **Host:port inputs require a host.** An omitted host would check the node
  running the check instead of the machine the author intended. Write
  `127.0.0.1:5432` for the node running the check.
- **MMWatchdog sends the `text_to_send` of `tcp` and `udp` exactly as written,** as
  the UTF-8 text of the YAML string, and adds nothing, such as a line
  break. A double-quoted string can hold any character through an escape:
  `\r\n` is a line break, and `\x00` sends a zero byte. A byte that is no
  UTF-8 text cannot be written, because YAML writes `\x80` as the character
  U+0080, which is two bytes in UTF-8. A protocol that needs such bytes has
  a module of its own, such as `dns`.
- **A `dns` check asks the DNS server that it names, and no other:** a check
  of a DNS server must not get its answer from the machine that asks.
- **MMWatchdog reads no more of a server's answer than a fixed limit,** whatever the
  server sends. In displayed server text, MMWatchdog replaces every
  character that changes how text is shown.
  A password of the check is in no message, also when a server repeats what
  it was sent.

Module pages that rely on the connection, authentication, and response rules:
[`tcp`](../../reference/modules/tcp.md), [`udp`](../../reference/modules/udp.md),
[`ssh`](../../reference/modules/ssh.md), [`smtp`](../../reference/modules/smtp.md),
[`imap`](../../reference/modules/imap.md), [`pop3`](../../reference/modules/pop3.md),
[`dns`](../../reference/modules/dns.md), [`memcached`](../../reference/modules/memcached.md),
[`redis`](../../reference/modules/redis.md), [`postgresql`](../../reference/modules/postgresql.md),
[`mysql`](../../reference/modules/mysql.md).

## Modules

A **module** is the code that does the work of one check: `http` sends an
HTTP request, `systemd_unit_state` asks systemd about a unit, and
`node_filesystem` measures a filesystem. Every check names exactly one
module, by the module's name, and writes the module's **inputs** below that
name. An [alert destination](alerts.md#destinations) also writes a module's
name and places the module's inputs below the name.

The [module index](../../reference/modules/README.md) lists every core module and every
official extra module, each with its page.

A module has one of three kinds:

| Kind | Module implementation | Module location |
| --- | --- | --- |
| Core module | Go code that is built into the `mmwatchdog` program | In `mmwatchdog` itself |
| [Official extra module](#official-extra-modules) | A Starlark script that the MMWatchdog project writes and tests | In `official_extra_modules/` of an MMWatchdog release. Every deploy installs `official_extra_modules/` on every node |
| [The project's own module](#the-projects-own-modules) | A Starlark script, or any executable, that the people who run MMWatchdog write | In `mmwatchdog_external_modules/` of the deployment project maintained by the people who run MMWatchdog. Every deploy installs `mmwatchdog_external_modules/` on every node |

Starlark is a small language that reads like Python. Starlark has no way to
open a file, a network connection, or a program by itself. To read a file,
open a network connection, or run a program, a Starlark script must call
MMWatchdog's functions ([primitives](#primitives)).

A check calls its module by name with inputs below the name, whatever the
kind of the module:

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 1m
    checks:
      myapp_answers_health_request:
        http:                                     # a core module
          url: http://127.0.0.1:8080/health
          expected_http_status_codes: [200]
        alert_severity: sos
      job_queue_is_short:
        queued_jobs:                              # a module of the project's own
          queue_file_path: /var/lib/myapp/pending
          run_as_user_account: myapp
        fail_when_measurement_above:
          queued_jobs: 500
        alert_severity: warn
```

The first check names the core module `http`. The second names `queued_jobs`,
a module that the project's own people wrote: it reads the number of waiting
jobs from the file in its input `queue_file_path` and reports the number as
the measurement `queued_jobs`. MMWatchdog applies the limit and fails the check above
500 waiting jobs
([checks with a limit on a number](#checks-with-a-limit-on-a-number)). An
official extra module is called by writing the module's name with inputs
below the name. For example, the destination `team_chat`
under [destinations](alerts.md#destinations) names the official extra module
`slack`.

- **The name.** A module's name uses lower-case letters, digits, and
  underscores, starts with a letter, and has at most 64 characters, such as
  `queued_jobs`.
- **The lookup.** MMWatchdog looks a name up among the core modules first, then among
  the official extra modules, then among the project's own modules. A name
  absent from the core, official extra, and project's own modules is a
  validation error that says where MMWatchdog looked.
- **One module of each name.** A module of the project's own that has the
  name of a core module or of an official extra module is a validation error
  that says to give the module another name. A module of the project's own
  also causes a validation error when named after a setting, such as `checks` or
  `alert_severity`.
- **A check module inspects and measures.** MMWatchdog owns the check's
  identity, schedule, limits, waiting times, incidents, recovery, and alerts.
  Results from all modules appear in the same places, in a uniform form.
- **The kind is not written in the configuration.** Every module is called
  by name with inputs below the name, so a module can move from one kind to
  another without a change to anyone's configuration: an official extra
  module that many servers need can become a core module. Three commands
  show the kind:
  [`mmwatchdog module-usage`](#which-modules-a-node-uses) lists every module that
  the node's configuration names, with each module's kind and files;
  [`mmwatchdog describe-module NAME`](#the-module-description) shows one module; and
  `mmwatchdog status` shows the kind beside each check whose module is not core.

### How a module that is not core runs

For each run of a check using an official extra module or a module of the
project's own, MMWatchdog starts a separate process for the module:

- For a Starlark script, the daemon starts the `mmwatchdog` program again, as
  `mmwatchdog run-starlark-module`, and that process runs the script. For an
  executable, the daemon starts the executable.
- The process runs as the unprivileged `mmwatchdog` account, or as the account in
  `run_as_user_account`, in the directory in `run_from_directory`, default
  `/`, with the environment of a [`shell`](../../reference/modules/shell.md) check and
  the string values in `env`.
  The settings `run_as_user_account`, `run_from_directory`, and `env` belong
  inside the mapping below the module's name, beside the module's inputs,
  as the example above shows. A module cannot define an input named
  `run_as_user_account`, `run_from_directory`, or `env`.
- MMWatchdog tells the module how much time its run has left
  (`mmwatchdog_ends_the_module_after_milliseconds` in its
  [request](#executables-and-the-json-protocol)). In a script, a primitive
  waiting for an answer or a program returns an `error` shortly before the
  time limit. The script then decides the check's result. For example, a
  server may accept a connection and never answer. If the module treats
  the error as a failure, the check fails and can trigger a recovery action.
  "Shortly before" is a tenth of the time limit earlier, and at most one
  second earlier ([the primitives](../../reference/writing-modules.md#primitives)).
- A run that takes longer than the check's `time_limit_per_run` all the same
  is ended, and the observation is unknown, with a message such as `the
  module queued_jobs did not finish within 10s (time_limit_per_run), so MMWatchdog stopped the module`. MMWatchdog
  ends the script together with every program that the script started with
  `run_command`, so no program of a run stays behind. The one exception is a
  program that makes itself independent of the program that started it, as
  a daemon does when it starts; a shell check has the same exception. A
  script also ends after 10 million Starlark interpreter instructions,
  called execution steps. A check needs far fewer steps, so a script that
  counts without end does not keep a processor busy until the time limit.
- A module that MMWatchdog cannot start, that ends with an error, or whose result is
  not valid makes the observation unknown, and MMWatchdog keeps what the module wrote
  for diagnosis.

A script therefore has the rights of its account and no more. An
unprivileged script cannot open the daemon's state, the alert queue, or
files readable only by root. MMWatchdog reads
a check input whose name ends in `_file` before starting the module, and
passes the content in `file_contents`. Passing file contents to the module
lets credential files remain readable by root alone while the module runs
unprivileged. The configured path stays in `inputs`.
Scripts and executables receive the `inputs` and `file_contents` maps. The
[write your own check or destination](../../how-to/write-your-own-check-or-destination.md) owns the format
and size bounds.

### Primitives

The **primitives** are the functions through which a module reads the node:
reading a file, running a program, sending an HTTP request. Every core
module uses MMWatchdog's primitives. Starlark scripts call MMWatchdog's
primitives by the function names in the table below. Core modules and
Starlark scripts use a shared implementation for operations on a node.

| Primitive | Primitive operation | Primitive result |
| --- | --- | --- |
| `read_file(path)` | Reads a text file of at most 1 MiB | `content` |
| `run_command(arguments, standard_input = "")` | Runs a program with its arguments, as the module's account. `run_command` starts no shell: `arguments` is a list, such as `["systemctl", "is-active", "myapp.service"]` | `exit_code`, `standard_output` (at most 1 MiB), and `standard_error` (at most 16 KiB) |
| `http_request(method, url, headers = {}, body = "", trusted_certificate_authority_file = "")` | Sends one HTTP or HTTPS request. `http_request` follows no redirect and always verifies an HTTPS certificate | `http_status_code`, `headers`, and `body` (at most 1 MiB) |
| `verify_connection(url)` | Opens and closes TCP or verified TLS without an HTTP request | `error` |
| `tcp_exchange(address, text_to_send = "", read_until_text = "")` | Opens a TCP connection to `address`, such as `127.0.0.1:6379`. With `text_to_send`, `tcp_exchange` sends the supplied text. With `read_until_text`, `tcp_exchange` reads until the answer contains the supplied text, at most 64 KiB | `received` |
| `udp_exchange(address, text_to_send)` | Sends `text_to_send` in one UDP packet to `address`, such as `127.0.0.1:514`, and waits for one packet in return, at most 64 KiB | `received` |
| `read_state()` | Returns the value that `write_state` stored at an earlier run of the check calling `read_state`, or `None` | The value |
| `write_state(value)` | Stores a value for the next run of the check calling `write_state` ([state kept between runs](#state-kept-between-runs)) | Nothing |

- Each primitive except the two for state returns a result with an `error`
  text, which is empty when the call worked. A failed call does not stop the
  script, so the script decides what the failure means: a file that is
  missing can make one check `failed` and another `unknown`.
- The `error` of `http_request` never holds the address of the request,
  because an address can hold a secret, such as the path of a chat webhook.
  The `error` of `http_request` can name the host that MMWatchdog could not
  reach, such as `hooks.slack.com`, and never the path or the query behind
  the host.
- A script also has `json.encode(value)` and `json.decode(text)`.
- `print(...)` writes a line to the standard error of the script's process,
  like the diagnostics of an executable. `mmwatchdog check-now` shows the lines of a
  check's run, so `print` is how the author of a script looks at a value. MMWatchdog
  passes on the first 8 KiB that a run prints, leaving room in the captured
  standard error for the script's error after the printed diagnostics.
- A script has nothing else: it cannot load another file of code, and it
  cannot reach a file, a program, or the network except through a primitive.
- A new core module that needs another way of reading the node adds a
  primitive, and every script can then use it too.

### Official extra modules

An official extra module is a Starlark script that the MMWatchdog project writes,
maintains, and tests. Official extra modules are for uncommon work, and for
alert destinations that one company runs
([where a module belongs](#where-a-module-belongs)). The section "Tested on"
of each module's page says what its tests run against. `slack` and
`telegram` run against stand-in servers on
[every target distribution](../testing.md#every-target-distribution).
`apache_status` and `mongodb` run against the real tools on Debian and
Ubuntu, and `zfs_pool` against a real pool on Ubuntu, the one distribution
whose kernel packages hold ZFS's kernel module.

```text
official_extra_modules/
  shipped_with_mmwatchdog_version       one line: the version of the MMWatchdog release
  slack/
    module.star                 the script
    module_description.yml      the module's inputs, measurements, and examples
    README.md                   how to set the module up, and what its errors mean
  telegram/
    ...
```

- **Official extra modules share a release and version with `mmwatchdog`.**
  The `official_extra_modules/` directory is in the MMWatchdog repository
  and in every release, beside the `mmwatchdog` program and under the
  release's version number. Official extra modules are never released by
  themselves.
- **Every deploy installs the whole directory** on every node, in
  `/usr/local/lib/mmwatchdog/official_extra_modules`
  ([deploy with MMX](operation.md#deploy-with-mmx)). No step adds one module
  to a project, and a deploy needs no MMWatchdog command on the machine where MMX
  runs.
- **MMWatchdog loads only the modules that the node's configuration names.** A
  module that no check and no destination names is never read and never run.
  Unused official extra modules cost a few kilobytes of text.
- **The version must match.** When `mmwatchdog` starts, validates, or reloads, it
  compares the version in `shipped_with_mmwatchdog_version` with its own. With two
  different versions `mmwatchdog` refuses to run and names both, such as `the
  official extra modules in /usr/local/lib/mmwatchdog/official_extra_modules were
  shipped with MMWatchdog 1.0.0, and this mmwatchdog program is MMWatchdog 1.1.0. One MMWatchdog release
  holds both: deploy that release again`. Comparing
  `shipped_with_mmwatchdog_version` with the running program's version is
  the only version check. A
  node that has no such directory runs as long as its configuration names no
  official extra module.
- **A fix is a script change.** Adding an official extra module or changing
  one needs no change to the Go code and no new build of `mmwatchdog`. The `mmwatchdog`
  program changes for module work only when a core module or a primitive
  changes.
- **Nobody edits an official extra module** or copies an official extra
  module into a project. A team that wants a module that
  works differently writes a module of its own under another name.
- **Nothing is downloaded when MMWatchdog runs.** A node has exactly the modules that
  the last deploy put there.
- **A module that checks a tool asks through the tool's own program,** with
  `run_command`, unless the tool answers over a protocol, such as HTTP. The
  module's page says what the node needs. A program that needs root gets it
  from the check's `run_as_user_account: root`, which a reader sees in the
  configuration.
- **A module that cannot determine the checked tool's health reports
  `unknown`.** If the module cannot understand the tool's output, or the
  tool lacks permission to inspect the resource, the check is unknown,
  never healthy.
- **A module is named for its tool, and for the thing that it checks:**
  `zfs_pool`, `apache_status`. The name of the vendor's program
  alone does not tell a reader what the check is about.
- **A module is named for what it looks at.** The module's name is a noun
  for what the check looks at, never a claim that should be true. The
  module's name never starts with `no_` and never holds any of the words
  `is`, `are`, `has`, `have`, `runs`, `running`, `exists`, `healthy`, or
  `synchronized` as a word of its own between underscores:
  `systemd_unit_state`, not a claim
  such as `systemd_unit_is_active`. The input `fail_when` says in plain
  words when the check fails, from the values on the module's page.
- **Use `true` for switches.** The value `true` is only for a switch
  whose name says what the switch turns on, such as `only_record_readings`.
  A module is never called with `true` as the module's whole value. A Go
  test enforces the rule for MMWatchdog's own modules. A claim such as
  "no pending security updates: true" makes the reader supply
  an unwritten "check that", and its failure reads as a double negative.

For example, this check group watches security updates on the node:

```yaml
check_groups:
  security_updates:
    check_every: 1h
    time_limit_per_run: 30s
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    checks:
      security_updates_are_installed:
        apt_security_updates:
          fail_when: a_security_update_waits_to_be_installed
        alert_severity: warn
```

Every hour, the node checks for security updates that wait to be installed.
The check fails while an update waits and alerts at `warn` without a delay.
See the [module page](../../reference/modules/apt_security_updates.md#set-up) for the
node's required setup.

The official extra modules of release 1:

| Used as | Modules |
| --- | --- |
| Check | [`zfs_pool`](../../../official_extra_modules/zfs_pool/README.md), [`apache_status`](../../../official_extra_modules/apache_status/README.md), and [`mongodb`](../../../official_extra_modules/mongodb/README.md) |
| [Alert destination](alerts.md#destination-modules) | [`discord`](../../../official_extra_modules/discord/README.md), [`mattermost`](../../../official_extra_modules/mattermost/README.md), [`pushover`](../../../official_extra_modules/pushover/README.md), [`slack`](../../../official_extra_modules/slack/README.md), [`telegram`](../../../official_extra_modules/telegram/README.md) |

### The project's own modules

A team writes a module of its own for its own application: a check that no
core module and no official extra module does. The module is a directory in
`mmwatchdog_external_modules/` of the deployment project. Every deploy
installs the module's directory on every node,
in `/usr/local/lib/mmwatchdog/external_modules`.

```text
mmwatchdog_external_modules/
  queued_jobs/
    module.star                 the script
    module_description.yml      the module's inputs, measurements, and examples
```

**Write a module as a Starlark script.** A script has the same primitives and
the same powers as an official extra module. Running Starlark itself needs
nothing on the node but MMWatchdog. MMWatchdog checks a check's inputs
against the module's description before anything runs.

```python
# mmwatchdog_external_modules/queued_jobs/module.star
def run_check(inputs, file_contents):
    queue_file = read_file(inputs["queue_file_path"])
    if queue_file.error:
        return {"status": "unknown", "message": "MMWatchdog cannot read the queue file: " + queue_file.error}
    waiting = queue_file.content.strip()
    if not waiting.isdigit():
        return {"status": "unknown", "message": "the queue file does not hold a whole number"}
    return {
        "status": "ok",
        "message": waiting + " queued jobs",
        "measurements": {"queued_jobs": int(waiting)},
    }
```

- A script that is used as a check defines the function `run_check(inputs, file_contents)`.
  MMWatchdog calls `run_check(inputs, file_contents)` once for each run with
  the configured inputs and a dictionary of the file contents that
  MMWatchdog read as root.
- The function returns a dictionary with a `status`, a `message`, and
  the check run's `measurements`, by the rules for an
  executable's result ([the JSON protocol](#executables-and-the-json-protocol)).
- `status` is `ok` when the module could measure, `failed` when the thing
  that the module checks does not work at all, and `unknown` when the
  module could not measure.
  The module does not compare a measurement with a limit: with the
  check `job_queue_is_short` above, MMWatchdog fails an observation of 780 queued
  jobs because 780 is above 500.
- A script that delivers alerts defines `deliver_alert` instead
  ([destination modules](alerts.md#destination-modules)).

**An executable is the way out for a special need,** such as a library that
exists only in another language. The module's directory then holds the file
`module_executable` in place of `module.star`: a Bash script, a Go or Rust
binary, or anything that runs on the node.
MMWatchdog starts `module_executable` and exchanges requests and results with
`module_executable` through the
[JSON protocol](#executables-and-the-json-protocol). MMWatchdog cannot see
what an executable does on the node, and the executable needs whatever it
uses there, such as an interpreter. Beside each executable,
`mmwatchdog module-usage` warns that MMWatchdog cannot see what the
executable does and that the executable needs everything the executable
uses on the node.

[Writing modules reference](../../reference/writing-modules.md) is the authoring reference.

### The module description

Every module has a **module description**: its inputs, its measurements, and
examples. The description of a core module is built into `mmwatchdog`. An official
extra module and a module of the project's own keep the module description
in `module_description.yml` beside the script or the executable.

```yaml
# mmwatchdog_external_modules/queued_jobs/module_description.yml
summary: Counts the jobs that wait in a queue file.
used_as: check
inputs:
  queue_file_path:
    type: string
    required: true
    description: The absolute path of the file that holds the number of waiting jobs.
measurements:
  queued_jobs:
    unit: count
    description: The number of jobs that wait.
examples:
  - description: Fail when more than 500 jobs wait.
    config: |
      queued_jobs:
        queue_file_path: /var/lib/myapp/pending
      fail_when_measurement_above:
        queued_jobs: 500
      alert_severity: warn
```

| Field | Meaning |
| --- | --- |
| `summary` | Required. One sentence that says what the module does |
| `used_as` | Required. `check` or `alert_destination` |
| `inputs` | The module's inputs by name. Each input states its `type`, whether it is `required`, and a `description`. `type` is `string`, `integer`, `number`, `boolean`, `duration`, or `list_of_strings`. An optional `allowed_values` lists the values that the input may have |
| `measurements` | For a check: the measurements that the module reports, by name. Each measurement states a `unit` and a `description` |
| `one_incident_for_each_finding` | For a check: `true` for a module that reports [findings](#findings) |
| `service_keeps_incidents` | For an alert destination: `true` for a module whose service keeps incidents of its own, such as a pager. MMWatchdog then keeps one incident per check across the fleet for every destination with the module, and sends it one closing alert when the last node recovers ([one incident per check across the fleet](fleet.md#one-incident-per-check-across-the-fleet)) |
| `examples` | At least one example: a `description` and the `config` of one check or one destination that uses the module |

- **`mmwatchdog validate` checks every check and every destination against
  the description of the configured module before anything runs.** The
  following cases are validation errors with the file and line:

  - A check or destination supplies an input that the module does not have.
  - A required input is missing.
  - An input value has the wrong type.
  - A check sets a limit on a measurement that the module does not report.

- **`mmwatchdog describe-module NAME` prints the module's whole page.**

  - For a core module, the command prints the page built into `mmwatchdog`.
  - For an official extra module, the command prints `README.md` from the
    module's directory.
  - For a module of the project's own, the command prints the module
    description, followed by `README.md` from the module's directory when
    the module has a `README.md`.

  For every kind of module, `--reference-only` prints only the module
  description as a reference listing, with the module's kind and the
  location of the module's files. Without a name,
  `mmwatchdog describe-module` lists every module on the node, with each
  module's kind and summary, and lists the
  core modules by topic. `--json` gives a program the command's output.

  Core module pages are built into `mmwatchdog` because a person who needs
  a page at 3 AM is logged in on the node where a check fails, and
  MMWatchdog has no other program to carry the pages. The
  [documentation map](../documentation-map.md#module-pages) explains how a
  page is made.

- **A result must keep to the description.** A measurement that the
  description does not list makes the observation unknown, with a message
  that names the measurement. The unit of a measurement comes from the
  description, so a result holds only the number.
- **An example must be valid.** When MMWatchdog reads a module description,
  MMWatchdog checks each example against the module's own inputs.
- MMWatchdog reads the description of each module that the configuration names when
  the daemon starts and at a reload. A deploy that changes a module restarts
  MMWatchdog.

### Executables and the JSON protocol

MMWatchdog starts an executable once for each run of its check. MMWatchdog writes one JSON
object to the executable's standard input:

```json
{"protocol": 1,
 "inputs": {"queue_file_path": "/var/lib/myapp/pending"},
 "file_contents": {},
 "state_dir": "/var/lib/mmwatchdog-module-state/myapp/job_queue_is_short",
 "mmwatchdog_ends_the_module_after_milliseconds": 9987}
```

The executable writes one JSON object to its standard output, and
diagnostics to its standard error:

```json
{"protocol": 1,
 "status": "ok",
 "message": "780 queued jobs",
 "measurements": {"queued_jobs": 780}}
```

- `status` is `ok`, `failed`, or `unknown`, and `message` is required.
- `measurements` is optional: a map from the name of a measurement to its
  number, with the names that the module description lists.
- `findings` is for a module that reports [findings](#findings).
- `state_dir` is the state directory of the check whose run started the
  executable ([state kept between runs](#state-kept-between-runs)).
- `mmwatchdog_ends_the_module_after_milliseconds` is the time that the run has left
  when MMWatchdog prepares the request. MMWatchdog ends the executable at
  the check's `time_limit_per_run`, so the executable's author must make a
  wait for a server end shortly before the check's deadline and return a
  result before the deadline.
- Exit status 0 means that the executable produced a result, including a
  result with `status: failed` or `status: unknown`. Any other exit status,
  a failed start, a run past the time limit, an end by a signal, standard
  output that is not exactly one result object in the JSON format above,
  a key that is written twice, another protocol number, or more than 64 KiB
  of standard output is an execution error: the observation is unknown,
  and MMWatchdog keeps the output for diagnosis.
- MMWatchdog sets `MMWATCHDOG_MODULE_HELPER` in the executable's environment to the path of
  the `mmwatchdog` program, for the JSON helper below.

A Starlark script receives the request that an executable gets as the
arguments of its function, and returns the result that an executable writes
as a dictionary, without `protocol`.

### JSON helper for Bash executables

The `mmwatchdog` program includes a JSON helper for an executable that is a Bash
script. MMWatchdog sets `MMWATCHDOG_MODULE_HELPER` to its absolute path for executables only;
shell checks and recovery commands do not get `MMWATCHDOG_MODULE_HELPER`.
The helper works during authoring without a daemon or a configuration, and
needs no `jq`, Python, or MMX:

```bash
request=$("$MMWATCHDOG_MODULE_HELPER" module-json request --inputs queue_file_path) || exit 1
queue_file_path=$(printf '%s' "$request" | "$MMWATCHDOG_MODULE_HELPER" module-json get --type string /inputs/queue_file_path) || exit 1
queued_jobs=$(cat -- "$queue_file_path") || exit 1
"$MMWATCHDOG_MODULE_HELPER" module-json result --status ok \
  --message "$queued_jobs queued jobs" --measurement queued_jobs "$queued_jobs"
```

- `module-json request [--inputs NAME,...]` reads the request on stdin, checks
  the request's protocol and that the request holds only the listed inputs.
  The command prints the request for the `get` calls that follow.
  The request of a
  [destination module](alerts.md#destination-modules) also holds `alert` and
  `file_contents`. The `get` command reads the `alert` and `file_contents`
  values by JSON Pointer, as for `inputs`, such as `/alert/title`.
- `module-json get [--type TYPE] POINTER` reads one value from the request on
  stdin by JSON Pointer, such as `/inputs/queue_file_path`. Types are
  `string`, `integer`, `number`, `boolean`, `array`, `object`, `null`, and
  `json`. Missing values and wrong types fail clearly; `json` keeps null,
  false, zero, empty strings, lists, and mappings distinct. Strings are
  printed exactly, with no added newline or NUL framing. Use command
  substitution as above for ordinary values; redirect exact text straight to
  a file when trailing newlines matter. A value Bash cannot hold, such as an
  embedded NUL, is rejected.
- `module-json result --status ok|failed|unknown (--message TEXT | --message-stdin) [--measurement NAME VALUE]... [--finding ID MESSAGE]...`
  writes a valid result, so quotes, backslashes, Unicode, and multiline text
  need no hand-escaping. `--message-stdin` reads the message from stdin. Each
  measurement uses its own `--measurement` flag, and each
  [finding](#findings) its own `--finding` flag. A destination module answers
  with `--status delivered` or `--status failed`, without measurements and
  findings.
- `module-json object [--string NAME TEXT]... [--string-stdin NAME] [--json NAME JSON]...`
  writes one JSON object with the members in the order given, for a module
  that sends JSON to a service. `--string` adds a text. `--string-stdin`
  reads text from stdin once for each object, for a
  long text or a secret. `--json` adds a value that is JSON already,
  such as a number, `true`, or an object that an earlier call wrote, so
  objects can hold objects. A name given twice is an error.
- `integer` accepts whole numbers in the signed 64-bit range, including
  whole-valued forms such as `2.0` and `2e3`, and prints them plainly.
- The helper exits 4 for bad arguments and 1 for invalid data, like the
  MMX helper.
- Every `module-json` command reads at most 1 MiB from stdin. The 1 MiB
  limit also applies to each request that MMWatchdog writes to a
  destination module ([destination modules](alerts.md#destination-modules)).
- The request travels through stdin and the result through stdout. The
  message and measurements are arguments unless `--message-stdin` is used;
  arguments can appear in process listings, so keep secrets out of them. No
  generated shell code or `eval` is involved.

An executable in another language uses that language's own JSON library. The
helper's conventions match MMX's, while each product keeps its own result
schema.

### State kept between runs

Some checks must remember something from one run to the next: how far a log
file was read, or a count at the last run. MMWatchdog keeps one **state directory**
for each check whose module is not core.

- The check's state directory is `CHECK_GROUP/CHECK` under the directory
  named by the node setting `module_state_dir`. The default value of
  `module_state_dir` is `/var/lib/mmwatchdog-module-state`, so the default
  check path is `/var/lib/mmwatchdog-module-state/CHECK_GROUP/CHECK`.
  MMWatchdog creates the check's state directory before the check's first
  run. The directory named by `module_state_dir` belongs to MMWatchdog.
  MMWatchdog creates the directory named by `module_state_dir` and the
  directories inside. MMWatchdog refuses to work in a directory under
  `module_state_dir` whose type, ownership, or permissions do not match
  the state-directory requirements. If `module_state_dir`
  names a directory with other content by mistake, MMWatchdog leaves the
  pre-existing content unchanged and refuses to use the directory.
  MMWatchdog changes and removes nothing in the refused directory and
  reports the refusal at every run and in `mmwatchdog validate`. The
  check's state directory belongs to the account that the module runs as.
  No other account can read the check's state directory.
- An executable gets the check's state directory path as `state_dir` in
  the request and keeps any files in the check's state directory.
- A script calls `write_state(value)` to store one value, such as a number
  or a dictionary, and `read_state()` at a later run to get it back. MMWatchdog
  keeps the value in the state directory of the check whose script called
  `write_state`.
- `mmwatchdog check-now` gives the module a copy of the directory and removes the
  copy after the run. So looking at a check never changes what its next
  scheduled run sees. The copy holds the directories and the regular files
  that belong to the module's account; a symbolic link, a special file, or a
  file of another account is not copied. MMWatchdog copies at most 64 MiB
  of files and at most 10,000 files and directories. With more, the run of
  `mmwatchdog check-now` is unknown and says so, because the account of the module
  decides how much the directory holds, and MMWatchdog makes the copy as root.
- MMWatchdog runs as root, and the module's account controls what is inside
  the check's own state directory. MMWatchdog therefore never follows a path
  into a state directory with root's rights, except in a way that cannot
  leave the state directory. An account cannot use a symbolic link inside
  the check's state directory to make MMWatchdog read or give away a file
  elsewhere on the node.
- The state belongs to the check as its configuration states it. When the
  check is removed, or changed so that MMWatchdog treats it as a new check
  ([activation](operation.md#activate-a-configuration)), MMWatchdog removes the
  directory.

A core module that keeps state, such as `systemd_unit_crash_loop` with its
count of restarts, keeps it in the daemon's own state, together with the
check's incidents.

### Findings

Some checks watch many things at once: every systemd unit of the node, or
every package with a known security flaw. One run of such a check reports
each thing that is wrong as a **finding**, and each finding is its own
incident, with its own alert and its own recovery. The core module
[`every_systemd_unit`](../../reference/modules/every_systemd_unit.md) reports each failed
unit as a finding, with a separate incident, alert, and recovery for each unit.

A module of the project's own reports findings when its description states
`one_incident_for_each_finding: true`. The module's result then lists the
findings:

```json
{"protocol": 1,
 "status": "failed",
 "message": "2 certificates expire within 14 days",
 "findings": [
   {"id": "www.example.com", "message": "the certificate of www.example.com expires in 3 days"},
   {"id": "api.example.com", "message": "the certificate of api.example.com expires in 12 days"}]}
```

- `id` is the stable identity of a finding: the same thing has the same `id`
  at every run. The `id` is text of at most 255 bytes on one line.
  No two findings of one result have the same `id`. `message` says what is
  wrong.

  A filesystem path that cannot fit uses a stable SHA-256 identity. The
  finding's message keeps the path, so a protocol size limit cannot hide a
  filesystem.
- A finding may provide `recovery_message`, the message to use when that
  finding becomes healthy. MMWatchdog keeps `recovery_message` with the
  finding's incident across restarts. Without `recovery_message`, MMWatchdog
  uses the module's recovery wording or MMWatchdog's default.
  A long filesystem path stays readable in recovery as well as failure.
- A result with the status `failed` holds at least one finding, and a result
  with the status `ok` holds none. A result holds at most 500 findings.
- Each finding follows the check's timings separately. A finding's failing
  time starts at the first run that reports the finding.
  The finding's incident opens after `alert_after_failing_for`.
  The finding recovers when no run has reported the finding for
  `recovered_after_healthy_for`.
  A second finding is therefore announced while the first finding's incident
  is open.
- The check's `alert_severity`, `raise_to_sos_after_failing_for`,
  `send_alerts_only_to_destinations`, and a destination's reminders apply to
  each finding's incident on its own.
- A finding has no alert severity of its own: every finding takes the
  check's. Two levels need two checks. Where some findings matter more than
  others, such as a flaw in a service that faces the internet, the team's
  triage agent in MMHotline judges each finding. The team's policy file
  sets the level of the triage agent's report.
- Every alert about one finding names it: after the check in its
  [title](alerts.md#the-title), in its message, and as `finding` in its
  [event](alerts.md#alert-events).
- Inspection shows the check with one line for each finding. The check
  itself is failed while a finding has an open incident, and it has no
  failure incident of its own.
- An `unknown` result is the check's, not a finding's: there is one
  cannot-check alert, however many findings have an open incident. An open
  incident of a finding stays open, and the time in which the module could
  not check counts as healthy time for no finding.

#### Exact findings

A finding may carry an **exact finding**, `exact_finding`: one line of at
most 255 bytes that names the problem precisely. Two nodes with the same
exact finding have the same problem, and one investigation covers both.
The finding's `id` stays the identity of its incident, so a move from one
affected version to another is never reported as ok again; the exact
finding changes instead.

In a fleet, the hub sends one alert per exact finding across the fleet,
instead of one per node ([one alert per exact finding](fleet.md#one-alert-per-exact-finding)),
and a team can [set a finding aside](fleet.md#set-a-finding-aside) by its
exact finding. A finding without an exact finding keeps
[grouping within one minute](fleet.md#fleet-alert-grouping).

The core modules that report findings supply these exact findings:

| Module | Finding `id` | `exact_finding` |
| --- | --- | --- |
| [`known_exploited_vulnerabilities_in_installed_packages`](../../reference/modules/known_exploited_vulnerabilities_in_installed_packages.md) | The CVE and the source package | The release ecosystem, the CVE, the source package, the installed version, the fix state, and the `modified` time of the release's own OSV record, such as `Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z` |
| [`apt_security_updates`](../../reference/modules/apt_security_updates.md), [`dnf_security_updates`](../../reference/modules/dnf_security_updates.md), [`zypper_security_patches`](../../reference/modules/zypper_security_patches.md) | `package security`, the one finding of all waiting updates or patches | The release, `ID` and `VERSION_ID` of `/etc/os-release`, and what waits, such as `debian 13: security updates wait` |
| [`ubuntu_pro_security_fixes`](../../reference/modules/ubuntu_pro_security_fixes.md) | The source package | The release, the source package, the installed version, the fixed version, and the Ubuntu Pro service |
| [`every_systemd_unit`](../../reference/modules/every_systemd_unit.md) | The unit | The unit and systemd's `Result`, such as `backup.service exit-code` |
| [`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md) | The mount point | The mount point and the kind of failure: `space`, `inodes`, `read-only`, or `unreadable` |

- **The three security-update checks report one finding for all waiting
  updates,** `package security`, keyed to the node's release. The remedy
  is the same for every waiting update, to install the updates. So a node
  with 27 waiting kernel advisories sends one alert, a fleet whose nodes
  wait for updates gets one alert for each release, and a new update that
  starts to wait sends none. The message still names the waiting packages,
  advisories, or patches. Never set aside these routine-update checks: an
  entry would quiet every security update of the release; install the
  updates. `known_exploited_vulnerabilities_in_installed_packages` keeps one
  finding for each flaw, because each flaw needs its own investigation.
- **A flaw's fix state** has one of four forms: `fixed version: none`,
  `fixed version waits: X`, `fix needs Ubuntu Pro: X`, and
  `fixed kernel installed, runs after a reboot: X`.
- **An advisory change is a new exact finding.** The release's own OSV
  record, such as `DEBIAN-CVE-…`, changes when the distribution changes its
  assessment of the flaw, so its `modified` time is part of the exact
  finding, even when the fix state stays the same. The record also changes
  for small edits, which costs an occasional repeat investigation; a
  missed reassessment would hide a flaw. The records of other ecosystems
  are not part of the exact finding.
- **A longer text is shortened with a hash.** Text longer than 255 bytes
  keeps its first 180 bytes, cut on a UTF-8 character boundary, followed by
  ` sha256:` and the SHA-256 of the whole text as 64 hexadecimal digits.
- **A module of the project's own** may return `exact_finding` beside a
  finding's `id`. It must be plain text on one line, and MMWatchdog removes
  credential file contents from it, as from `message`. An executable
  module builds it with
  `mmwatchdog module-json result --exact-finding ID TEXT`, where `ID` names
  the finding ([writing modules](../../reference/writing-modules.md)).

**On the node,** each finding's incident keeps two exact findings: the
**current** one, from the latest run, and the **announced** one, which the
incident's latest alert carried.

- When the incident is open and announced, is not held back by maintenance
  or quiet time, and its current exact finding differs from the announced
  one, the node sends an `exact_finding_changed` alert and records the
  current exact finding as announced.
- A change during maintenance or quiet time goes out when that time ends,
  if the exact finding still differs.
- Each change of the current exact finding raises the incident's exact
  finding sequence number by one. Reports and alerts carry the number, so
  the hub never takes an older exact finding over a newer one.
- `exact_finding_changed` belongs to its incident: it carries the
  incident's key and follows the incident's recorded destinations, the
  record of incidents sent without the hub, and the fallback rules of
  [alerts in a fleet](fleet.md#alerts-in-a-fleet).
- A standalone node sends `exact_finding_changed` to its destinations like
  any alert of the incident.

Module pages that rely on the findings section:
[`every_systemd_unit`](../../reference/modules/every_systemd_unit.md),
[`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md),
[`known_exploited_vulnerabilities_in_installed_packages`](../../reference/modules/known_exploited_vulnerabilities_in_installed_packages.md),
[`apt_security_updates`](../../reference/modules/apt_security_updates.md),
[`dnf_security_updates`](../../reference/modules/dnf_security_updates.md),
[`zypper_security_patches`](../../reference/modules/zypper_security_patches.md),
[`ubuntu_pro_security_fixes`](../../reference/modules/ubuntu_pro_security_fixes.md).

### Which modules a node uses

Every module is called by its name alone. To identify the code behind each
name, `mmwatchdog module-usage` shows the node's modules and who wrote each
module. `mmwatchdog module-usage` reads the node's configuration directory
and needs no daemon.

```text
$ mmwatchdog module-usage
Core modules (built into MMWatchdog)
  email                /etc/mmwatchdog/alert_destinations.yml:8
  http                 /etc/mmwatchdog/check_groups/myapp.yml:11
  systemd_unit_state   /etc/mmwatchdog/check_groups/myapp.yml:6, /etc/mmwatchdog/check_groups/workers.yml:6

Official extra modules (Starlark, shipped with MMWatchdog 1.0.0)
  slack                /usr/local/lib/mmwatchdog/official_extra_modules/slack/
                       /etc/mmwatchdog/alert_destinations.yml:3

The project's own modules
  legacy_backup        /usr/local/lib/mmwatchdog/external_modules/legacy_backup/   executable
                       /etc/mmwatchdog/check_groups/backup.yml:7
                       MMWatchdog does not see what an executable does on this node.
                       The local node must provide everything the executable needs.
  queued_jobs          /usr/local/lib/mmwatchdog/external_modules/queued_jobs/   Starlark
                       /etc/mmwatchdog/check_groups/myapp.yml:16
```

- For each module, the command shows its kind, the place of its files, and
  every place where the configuration calls it, as file and line.
- `mmwatchdog module-usage NAME` shows one module. With `--json`, the command
  gives a program the module's kind, file locations, and configuration
  references as files and lines.
- A module that is an executable is marked, because MMWatchdog does not see what an
  executable does.
- On a node that has joined a hub, the destinations come from the hub, so the
  command lists the modules of the node's checks. To see the destination
  modules, the command says to run `mmwatchdog module-usage` on the hub.
- `mmwatchdog status` shows the kind beside each check whose module is not core, such
  as `module queued_jobs: the project's own, Starlark`.

Each command answers one question: `mmwatchdog describe-module NAME` says what a
module does and what its inputs are, and `mmwatchdog module-usage` says where the
modules of this node are used.

### Where a module belongs

- **Common work is a core module.** A check that many servers need is built
  into `mmwatchdog`: the node itself, systemd, files, HTTP, TCP, TLS, and the common
  protocols and server software, such as SSH, PostgreSQL, and Redis
  ([protocols and common server software](#protocols-and-common-server-software)).
  When it is not clear how common a check is, the check is core.
- **Uncommon work is an official extra module,** such as checking a ZFS pool.
- **An alert destination that one company runs is an official extra module,
  not a core one,** however many people use it: Slack, Telegram, Pushover,
  and the like. The
  core destinations are the general ones, `webhook`, `email`, and
  `system_log`, and `mmhotline`
  ([destination modules](alerts.md#destination-modules)).
- **A paging service, such as PagerDuty or Jira Service Management, gets no
  official module**. MMHotline is Monkeymaxxing's paging service.
  Every official module is tested against its real service before each
  release. The users of PagerDuty and Jira Service Management are mostly
  larger companies, outside the teams MMWatchdog is made for.
  A team that uses PagerDuty or Jira Service Management writes a destination
  module of its own ([writing modules reference](../../reference/writing-modules.md)).
- **A team's own application** gets a shell check or a module of the
  project's own.

A module is named for the tool that it checks, such as `postgresql`. MMWatchdog has
no general name that stands for several tools.

## Heartbeats

A heartbeat check group expects a report of successful work, such as a
nightly backup, within an interval. A job reports in one of two ways:

- **Through the heartbeat socket**, for a job on the same node. The job needs
  no certificate, port, or address.
- **Through HTTPS**, for a job on another machine. The receiving MMWatchdog
  instance, usually on the operations node `util`, listens for HTTPS reports.

Configure the heartbeat check group on the node that receives the reports:

```yaml
# /etc/mmwatchdog/check_groups/backups.yml
check_groups:
  nightly_backup:
    heartbeat:
      max_time_between_heartbeats: 26h
      expected_bearer_token_file: /etc/mmwatchdog/secrets/nightly-backup
    alert_severity: sos
```

A job on the same node reports only after the job succeeds:

```bash
backup-command &&
  mmwatchdog heartbeat nightly_backup \
    --bearer-token-file /etc/mmwatchdog/secrets/nightly-backup
```

The heartbeat socket is `/run/mmwatchdog/heartbeat.sock`, beside the
inspection socket. It exists whenever the configuration has at least one
heartbeat check group, and has no setting. The socket has mode 0666 so that
a job of any account can connect; every request still needs the check
group's bearer token, as over HTTPS. A reload that adds the first heartbeat
check group opens the socket, and a reload that removes the last one closes
it. The socket is the same endpoint as the HTTPS receiver's, served by the
same handler.

For jobs on other machines, the node also listens for HTTPS reports:

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
heartbeat_receiver:
  listen_on_address: 0.0.0.0:8444
  tls_certificate_file: /etc/mmwatchdog/secrets/server.crt
  tls_private_key_file: /etc/mmwatchdog/secrets/server.key
```

```bash
backup-command &&
  mmwatchdog heartbeat nightly_backup \
    --receiver-url https://util.example.com:8444 \
    --bearer-token-file /etc/mmwatchdog/secrets/nightly-backup
```

- The reason the socket needs no certificate: only root can listen at its
  path. systemd creates `/run/mmwatchdog` for the unit (`RuntimeDirectory`,
  mode 0755, owned by root) and removes it when MMWatchdog stops. `/run`
  belongs to root and is not writable by other accounts, so no other account
  can create `/run/mmwatchdog` or put a listener at the socket's path, also
  while MMWatchdog is stopped or restarting. A job therefore never sends its
  token to an impostor. A plain-HTTP port on the loopback address above 1023
  would not give this: any account could listen on it while MMWatchdog is
  down, collect tokens, and send fake heartbeats that hide a failed backup.
- Before the sender sends the token to the socket, it checks that the socket
  and its directory belong to root, that neither is a symbolic link, and that
  no account but root can write to the directory. It refuses with a message
  that says so otherwise. This keeps the reason above true on a node whose
  `/run` was changed. The sender refuses `--trusted-certificate-authority-file`
  without `--receiver-url`, because the socket has no certificate.
- The endpoint is `POST /v1/heartbeats/NAME` with the check group's Bearer token.
  Each heartbeat has its own token of 16–4096 bytes: the minimum protects
  against guessed tokens. Tokens are read at startup and
  reload; a request can never create a check group. Tokens stay out of URLs and
  command arguments.
- Over HTTPS, the sender's `--trusted-certificate-authority-file` replaces the system
  authorities, so naming a private authority does not also trust unrelated
  public authorities. Without the flag, the sender uses the system store.
- The receiver's receipt time is authoritative. MMWatchdog saves the receipt and the
  new deadline before answering `204`, and answers `503` while it cannot save
  them.
- A newly configured heartbeat check group is `waiting_for_first_heartbeat`
  for one interval. Status shows the waiting state without treating the
  state as needing attention. The waiting state is not proof of success.

  At the deadline, a missing report opens a failure incident at once, naming
  the last report (or when monitoring began) and the interval.
  The next valid report resolves the failure incident immediately.
  Heartbeats take no timing settings.
- Deadlines survive daemon restarts and reloads of unchanged heartbeats.
- Over HTTPS, the sender verifies TLS, optionally against `--trusted-certificate-authority-file` for a private
  certificate authority. Over the socket and over HTTPS, the sender makes up to three attempts with five-second
  timeouts. The heartbeat sender keeps no backlog, so an old job's success
  is never replayed as fresh.

Heartbeats work the same with or without a hub. Detecting a whole machine
that disappears needs an observer elsewhere: in a fleet, the hub's
[silence alerts](fleet.md#silent-nodes).

## One cause, one alert

When a database stops, the check of the database fails, and so does every
check of an application that needs the database. Without help, the database
check and every affected application check send alerts. MMWatchdog also
restarts the application although the database is the cause.

A check can name the checks of its causes with
`no_alerts_or_recovery_actions_while_these_checks_fail`. While one of the
named checks fails, MMWatchdog sends no alert for the check that has the setting and
takes no recovery action because of it: no restart of a systemd unit and no
recovery command. MMWatchdog **keeps the check quiet**, and the time in which it does
is the check's **quiet time**. The quiet check keeps running, and inspection
keeps showing the quiet check's state.

```yaml
# /etc/mmwatchdog/check_groups/postgres.yml
check_groups:
  postgres:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 2m
    recovered_after_healthy_for: 30s
    checks:
      postgres_service_is_running:
        systemd_unit_state:
          systemd_unit: postgresql.service
          fail_when: the_unit_is_not_active
        alert_severity: sos
```

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 2m
    recovered_after_healthy_for: 30s
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
        alert_severity: sos
        no_alerts_or_recovery_actions_while_these_checks_fail: [myapp/myapp_service_is_running, postgres/postgres_service_is_running]
    restart_systemd_unit:
      systemd_unit: myapp.service
      triggered_by_checks: [myapp_service_is_running, myapp_answers_health_request]
      restart_after_failing_for: 1m
      max_attempts: 3
      min_time_between_attempts: 1m
      reset_attempts_after_healthy_for: 10m
```

The health request of `myapp` names two causes: the application's own unit
check and the database's unit check. With
`/etc/mmwatchdog/check_groups/postgres.yml` and
`/etc/mmwatchdog/check_groups/myapp.yml` on one node:

1. At 12:00 `postgresql.service` crashes. The application cannot reach the
   database, so the health request starts to fail.
2. At 12:01 the health request has failed for 1 minute, which would make the
   restart of `myapp.service` due. The check
   `postgres/postgres_service_is_running` is failing, so MMWatchdog does not restart
   `myapp.service` and uses none of the 3 attempts.
3. At 12:02 MMWatchdog sends one `sos` alert, for the database. The database
   alert's body adds: `no
   alerts and no recovery actions for these checks while this check fails:
   myapp/myapp_answers_health_request`.
4. At 12:20 an operator starts `postgresql.service`. At 12:20:30 the database
   check has been healthy for 30 seconds, so the database check's failing
   time ends, and the health request's quiet time ends with the database
   check's failing time.
5. The next run of the health request decides. If the health request is
   healthy, MMWatchdog sends nothing about the application. If the health
   request still fails, MMWatchdog sends the health request's alert and
   restarts `myapp.service`, with all 3 attempts available.

### Which checks a check can name

- Each name is written as check group, slash, check, such as
  `postgres/postgres_service_is_running`, also when the named check belongs to
  the naming check's check group. A check that is written directly on its
  check group has the name of its module, such as `postgres/systemd_unit_state`.
  The check of a [heartbeat](#heartbeats) check group is named
  `nightly_backup/heartbeat`.
- These are validation errors: a name that matches no check on the node, a
  name that is not written as check group, slash, check, a name that is
  written twice, the check's own name, and an empty list. The error for a
  name that matches no check lists the checks of the named check group.
- A check that only records its readings
  ([checks with a limit on a number](#checks-with-a-limit-on-a-number))
  never fails and never alerts. No check can name a check that only records
  readings in `no_alerts_or_recovery_actions_while_these_checks_fail`.
  A check that only records readings cannot have
  `no_alerts_or_recovery_actions_while_these_checks_fail`.
- A heartbeat check group does not take the setting. A heartbeat has no runs
  that could decide after the quiet time, and a heartbeat that was missed
  stays missed after the cause is healthy again, so MMWatchdog would alert anyway.
- The setting is written on each check. A check group that has named checks
  refuses `no_alerts_or_recovery_actions_while_these_checks_fail` on the check
  group.
- Checks that name each other in a circle are a validation error. When the
  check `a/tcp` names `b/tcp` and `b/tcp` names `a/tcp`, each would keep the
  other quiet while both fail, and MMWatchdog would send no alert at all. A chain is
  valid: when `a/tcp` names `b/tcp` and `b/tcp` names `c/tcp`, and all three
  fail, `c/tcp` alerts and the other two stay quiet.
- The setting is no part of what makes a check the same check: a reload that
  changes the list keeps the check's open incident. A check that no longer
  names the failing check is no longer kept quiet by it.

### What MMWatchdog holds back in the quiet time

- MMWatchdog sends no alert of the quiet check: not its failure alert, not the alert
  that MMWatchdog cannot run the check, no raise to `sos`, no reminder, and no
  recovery alert.
- MMWatchdog takes no recovery action because of the quiet check. The
  setting covers only the check that has the setting. Another check that
  triggers the restart still does.
- The quiet check's incident opens and resolves as always, in the event
  history only. `mmwatchdog events` shows each such event with the reason at the end
  of its message, such as `expected 200, received 503; no alert while
  postgres/postgres_service_is_running fails`.
- `mmwatchdog status` has one line for each quiet check that fails, such as
  `myapp/myapp_answers_health_request is failing; no alert and no recovery
  action while postgres/postgres_service_is_running fails`, and one for each
  quiet check that MMWatchdog cannot run: `MMWatchdog cannot check
  myapp/myapp_answers_health_request; no alert while
  postgres/postgres_service_is_running fails`. A quiet check whose incident
  was announced before the quiet time and that is healthy again has the line
  `myapp/myapp_answers_health_request is ok again; no recovery alert while
  postgres/postgres_service_is_running fails`. Any other quiet check that is
  healthy has no line, because MMWatchdog holds nothing back for it.
- Between the end of the named check's failing time and the quiet check's
  next run, the named check no longer fails, and the line says so:
  `myapp/myapp_answers_health_request is failing; no alert and no recovery
  action until its next run, because postgres/postgres_service_is_running
  was failing`.
- `mmwatchdog status CHECK_GROUP` shows for each check the checks that the
  check names and the checks that name the check. JSON status lists the
  named checks as `no_alerts_or_recovery_actions_while_these_checks_fail`.
  JSON status lists the checks that name the check as
  `no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails`.
  JSON status names the check that currently keeps a check quiet as
  `no_alerts_or_recovery_actions_now_while_check_fails`.
  Until the quiet check's next run, JSON status instead names the check that
  kept the check quiet as
  `no_alerts_or_recovery_actions_until_next_run_because_check_was_failing`.
- The report that a node sends to the hub adds what MMWatchdog holds back to the
  message of the quiet check, such as `expected 200, received 503; no alert
  and no recovery action while postgres/postgres_service_is_running fails`,
  so `mmwatchdog fleet NODE` on the hub shows it.

### What the alert of the cause says

The alert of a failing check lists the checks that MMWatchdog keeps quiet while it
fails: every check that names it, whether or not that check fails at this
moment. The line is in the body of the failure alert, of a raise, and of
every reminder, as at 12:02 in the example. The failure event, severity-raise
event, and every reminder event carry the names as
`no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails`.

An alert names at most 20 quiet checks. With more, the line ends in `, and 3
more`, and the event's
`number_of_checks_with_no_alerts_or_recovery_actions_while_this_check_fails`
counts every quiet check, including checks omitted from the alert body.

- The one alert is the alert of the cause: it goes out at the cause's
  `alert_severity` and to the cause's destinations. When a check with
  `alert_severity: sos` names a cause with `alert_severity: warn`, a
  destination that takes only `sos` hears nothing while the cause fails.
  Give the cause the severity that its failure deserves. A check that names
  a check with a lower `alert_severity` is a validation warning: `mmwatchdog
  validate` and `mmwatchdog reload` name both checks with their severities, and the
  configuration stays valid
  ([what validation reports](operation.md#what-validation-reports-beyond-the-schema)).
- In a chain, the alert names the checks that name the failing check itself.
  When `a/tcp` names `b/tcp` and `b/tcp` names `c/tcp`, the alert of `c/tcp`
  names `b/tcp`. The check `a/tcp` is quiet only while `b/tcp` fails, which
  the failing of `c/tcp` does not decide.
- An alert for a destination with
  `send_only_the_alert_title` ([title and body](alerts.md#title-and-body))
  carries no names of quiet checks.

### When the quiet time starts and ends

- A named check counts as failing during the named check's whole
  [failing time](#the-failing-time). The failing time starts with the first
  failed observation and ends after `recovered_after_healthy_for` of health.
  A maintenance window of the named check's check group does not change
  when the named check counts as failing. A check that keeps the named
  check quiet does not change when the named check counts as failing.
- The quiet time starts at the moment a named check starts to fail.
  When a named check starts to fail, MMWatchdog holds the quiet check's
  alerts and recovery actions, even before the quiet check runs again.
- The quiet time ends with the quiet check's own first run after the failing
  time of the named check has ended. The first run after the named check's
  failing time ends decides whether to send alerts or take recovery actions.
  MMWatchdog does not use an earlier result from the quiet time to send
  alerts or take recovery actions after the cause recovers.
  The earlier result does not establish the quiet check's health after the
  cause recovers.
- MMWatchdog sends nothing later for quiet time. The quiet check's waiting
  times keep counting. If the first run after the named check's failing time
  ends still fails, MMWatchdog sends any failure alert now due.
  The failure alert uses the severity that the incident has reached, and any
  recovery action now due follows from the first failed run.
  A check that is healthy in the first run after quiet time sends nothing.
- An incident of the quiet check that was announced before the quiet time
  stays open at the incident's destinations. MMWatchdog holds the announced
  incident's reminders and severity raise. If the quiet check recovers
  during quiet time, MMWatchdog sends the recovery alert with the first
  healthy run after quiet time. If the incident remains open after quiet
  time, MMWatchdog sends any severity raise that became due.
  For an incident still open after quiet time, reminders count from quiet
  time's end, as after a [maintenance window](recovery.md#maintenance-windows).
- MMWatchdog can temporarily lack a current observation of a named check
  when the daemon starts. A maintenance window starting or ending for the
  named check's check group can also leave the named check without a current
  observation. A reload that changes the named check can also leave the
  named check without a current observation.

  If the named check's incident has not opened, the named check's failing
  time then ends without an observation. The named check still counts as
  failing until the named check's next run. The quiet check stays quiet
  until MMWatchdog has observed every check that the quiet check names again.
  So a daemon start in the first minutes of an outage neither sends an
  alert nor triggers a restart because of the quiet check.
- A run of the named check that MMWatchdog cannot make is an unknown
  observation. For example, every slot for concurrent checks may be taken.
  The unknown observation preserves the cause's failing time
  ([the failing time](#the-failing-time)), so the quiet time continues.
  A late run also preserves the failing time.
  Observed lasting health ends the cause's failing time.

### What the setting does not do

- A check of something on another machine is kept quiet through a local check
  of that thing. For a database on another node, the application's node gets
  a `tcp` check of the database's port, and the health request names that
  check.
- The setting `no_alerts_or_recovery_actions_while_these_checks_fail` does
  not define a dependency graph. MMWatchdog orders nothing, starts nothing,
  and stops nothing. Starting units in order and restarting units together
  belong to systemd.

## Check group decisions

The check group decisions in the table are settled. Revisit a check group
decision only with a concrete workflow that the decision fails to support.

| Decision | Reason |
| --- | --- |
| Confirmation uses durations, not cycle counts | A duration means the same thing whatever the interval. |
| A check keeps failing until it has been healthy for `recovered_after_healthy_for`; no rule counts failed runs | A check that fails on and off is broken for users. An open incident ends only after the check has been healthy for `recovered_after_healthy_for`. A count such as "4 failures in 6 runs" needs two numbers on every rule, counted in runs. |
| Every waiting time counts from the start of the failing time | A reader needs one clock to follow the alert, the recovery action, and the raise to `sos`. |
| An incident opens at a failed observation, never at a healthy one | One failed run followed by health would otherwise alert whenever `recovered_after_healthy_for` is longer than `alert_after_failing_for`. |
| Unknown is separate from failed and never drives recovery | A broken check must not restart a healthy unit or hide a real failure. |
| Every ordinary check group states its four timings, with no node-wide defaults; direct event checks omit the alert delay | A reader finds every timing in the file that holds the check group; coding agents write the configuration, so being explicit costs nothing. |
| A check may state its own two waiting times | A health request and a memory limit on the same unit need different waiting times, without a second check group. |
| `alert_severity` is written on every check, with no default | A reader under pressure sees the severity on the check itself; coding agents write the configuration, so being explicit costs nothing. |
| No group or node default for alert severity, and no inheritance | A default hid the severity of a stopped unit. Without inheritance, the model and code stay simple. |
| A cannot-check alert takes the check's current severity, including its raise | MMWatchdog must announce when an important check cannot run, using the check's current severity. |
| Escalation by level is a second check with a higher limit and severity | The ordinary limit and severity rules work for every check, with no raise setting per measurement. MMWatchdog needs no special condition logic to raise the severity of an individual measurement. |
| MMWatchdog applies the limit to a number; a module only measures | One way to write a limit for every module. A module needs no limit logic and no limit messages of its own, and its readings reach history and charts. |
| The name of a measurement carries its unit | The name can be read alone in a limit, in an alert, and in a chart. |
| A measurement check with `only_record_readings: true` records readings without limits or an alert severity; of these modules, only a missing or read-only filesystem and a network interface without a link fail without a limit | A busy machine is not a failure until the operator sets a limit, while a filesystem that is gone and a cable that is out are failures at any value. |
| An `every_disk_and_network_filesystem` check fails for a filesystem that does not answer or answers with an error, where a `node_filesystem` check of the same filesystem is unknown | Each filesystem is a [finding](#findings) of the check, and a finding is something that failed: one filesystem that MMWatchdog cannot measure must not make the whole check unknown and hide the other filesystems. A `node_filesystem` check watches one filesystem, so its unknown hides nothing else. |
| A check can be kept quiet while the check of its cause fails, with no dependency graph | One cause sends one alert, and a restart that cannot help uses no attempt. MMWatchdog still orders, starts, and stops nothing. |
| After the quiet time, the quiet check's own next run decides, and nothing is sent later for the quiet time | A run from the quiet time says nothing about the check once its cause is healthy again: an alert or a restart on that run could hit an application that has already recovered. |
| Checks that name each other in a circle are a validation error | If every check in a circular chain failed, every failing check would keep the check that names the failing check quiet. MMWatchdog would send no alert at all. |
| A named check that MMWatchdog cannot run does not keep a check quiet, unless its incident is open | MMWatchdog cannot tell whether the named check's underlying problem still exists. Allowing a possibly unnecessary alert or restart is safer than suppressing alerts and restarts indefinitely while MMWatchdog cannot run the named check. |
| The alert of a cause names the checks that name it, not the checks further down a chain | A check further down is quiet only while the check that it names fails, which the failing of the cause does not decide. |
| The one alert has the severity and the destinations of the cause, and a check that names a cause with a lower `alert_severity` is a validation warning, not an error | The cause is what failed, so its settings say who must hear of it. A lower severity on the cause can be what the author wants, so MMWatchdog points at it and still runs the configuration. |
| A recovery of the quiet check waits for the end of the quiet time, like every alert of that check | MMWatchdog holds every alert of a quiet check, including recovery alerts, until the quiet time ends. Maintenance holds every alert of a covered check until the maintenance window ends. `mmwatchdog status` says that the recovery alert waits. |
| A heartbeat receiver states the host of its [listening address](../../reference/configuration-files.md#listening-addresses), including an explicit wildcard for every interface | The node settings must show which interfaces accept heartbeat connections |
| A job on the same node reports through the heartbeat socket, which has no setting and needs no certificate | Only root can listen at `/run/mmwatchdog/heartbeat.sock`, so the token never reaches an impostor, also while MMWatchdog is stopped. A plain-HTTP loopback port could be taken by any account while MMWatchdog is down |
| A heartbeat takes no `no_alerts_or_recovery_actions_while_these_checks_fail`, and other checks can name it | A heartbeat has no runs that could decide after the quiet time, and a heartbeat that was missed stays missed after the cause is healthy again, so MMWatchdog would alert anyway. As a cause, a heartbeat check works like every check: it fails from its missed deadline until the next heartbeat. |
| Reboots are an always-on notice, not a check group | An unplanned reboot is worth one alert. Planned reboots are covered by maintenance. |
| TLS verifies certificates by default; the `tls` check always verifies | The explicit `dangerously_skip_server_certificate_check` input permits service checks to test encrypted endpoints without authentication. A `tls` check exists to guarantee certificate validity, so it takes no skip input. |
| Every check and every alert destination calls its module by the module's name, whatever the kind of the module | Every module looks the same in the configuration, so a module can move from one kind to another without a change to anyone's configuration. `mmwatchdog module-usage` shows what the YAML no longer shows: which modules are the project's own code. |
| A project module with the name of a core module or of an official extra module is a validation error | A name means one module on every node. A reader who knows `http` from one project knows it in every project. |
| Common work is a core module; an official extra module is only for uncommon work, and for an alert destination that one company runs | A person finds the common checks in MMWatchdog itself, tested with it, and the core is solid from the first release. How common a check is comes from real monitoring configurations; when unsure, the check is core. The destinations of single companies change with those companies, so they stay scripts that a release can fix without a new `mmwatchdog` program. |
| Each core module is in one place in the code, and a registry lists them | A core module's inputs, input validation, description, and code can be read and reviewed together. The rest of MMWatchdog knows no module by name. Adding a core module touches the module's place in the code and one line of the registry. |
| Core modules and scripts read the node through the same primitives | Reviewers check one shared implementation of node access: the primitives. Core modules and scripts read the node alike. |
| Official extra modules are Starlark scripts in a directory beside the `mmwatchdog` program, in the same release under the same version | Adding or fixing an official extra module requires only a script change with no new build of `mmwatchdog`. The core stays stable while the extras stay cheap. Starlark is small, reads like Python, and can touch the node only through the primitives. |
| `mmwatchdog` refuses to run when the official extra modules were shipped with another version | A script and the primitives that it calls come from one release. A mix of two releases would fail in ways that are hard to find. |
| Every deploy installs all official extra modules of the release on every node; they are not copied into the project one by one, and not downloaded when MMWatchdog runs | Nothing changes on a node without a deploy. Nobody edits an official extra module, so a project needs no copy of its own. An update of MMWatchdog updates the modules with no step that a person can forget. A module that nothing names is never run, so the unused ones cost a few kilobytes. |
| The project's own modules are Starlark scripts first, and any executable is the way out for a special need | Coding agents write most modules, and a script is the easiest kind to get right. A script uses the primitives of core modules and needs nothing else on the node. An executable in any language covers what a script cannot do. |
| A script runs in a process of its own, as the check's account | The kernel keeps an unprivileged script from the daemon's state and from credential files. The time limit ends a run for certain. A script that ran inside the daemon would run as root. |
| A call of a primitive in a script ends shortly before the time limit: a tenth of the time limit earlier, and at most one second earlier | The script keeps time to decide and return the result to MMWatchdog, also on a busy node. A server that hangs then fails the check, and a failed check drives recovery. A run that MMWatchdog ends at the time limit is unknown, which never triggers recovery actions. One second is far more than the script process needs to return a result, so a long time limit loses little time. |
| Every module has a module description, and `mmwatchdog validate` checks every call against it | A wrong input shows before anything runs, with the file and the line, for a module of every kind. `mmwatchdog describe-module` gives people and coding agents one place to look a module up. |
| A module states its measurements and their units in its description, and a result holds only numbers | A limit on a measurement that the module does not report is a validation error, not an unknown check at 3 a.m. A measurement's unit comes only from the module description, so the result cannot give a different unit. |
| A module's script, `module.star`, is at most 1 MiB (`moduleScriptLimit`) | MMWatchdog reads the whole file into memory before it parses the script, and a script is a few kilobytes, so a file this large is a mistake or a file of the wrong kind. MMWatchdog refuses a larger file instead of reading it into the memory of the node monitoring process. |
| A module's description, `module_description.yml`, is at most 256 KiB (`ModuleDescriptionLimit`) | MMWatchdog reads the whole file into memory for every validation and every call, and a description of inputs, measurements, and examples is a few kilobytes. A smaller limit than the script's, because a description holds no code. |
| A result of a module that is not core holds at most 128 measurements (`modulecall.go`) and at most 500 findings (`FindingsLimit`) | The result goes into status, into alerts, and into the reports to the fleet hub, and each of these must stay small enough for every destination and for the reports to the hub. A result over either count is an error of the run. |
| An `http` check reads at most 1 MiB of a response (`HTTPResponseBodyLimit`) | The memory of one run stays small whatever the server sends, so a server that answers with an endless stream cannot fill the node. A page that the check matches against is far smaller. A larger response is an error of the run, not a silent cut, so a rule never matches half a page. |
| A check whose module is not core has a state directory, and `mmwatchdog check-now` works on a copy | A module can remember where it was, in files of any size, in any language. Looking at a check never changes what its next scheduled run sees. |
| A check can report findings, each with a stable `id`, and each finding is its own incident | A second failed thing is announced at once, and each repaired thing reports a separate recovery. One mechanism serves `every_systemd_unit` and every module. |
| A finding takes the alert severity of its check | A reader sees the severity on the check itself, as for every alert. Two levels are two checks, as for a limit. |
| No metrics query language, tracing, or log ingestion | MMWatchdog watches node health. Metrics query languages, tracing, and log ingestion belong in separate products. |
| Configuration is data, with no expressions or templates | MMX generates configuration when needed; MMWatchdog stays readable. |

Node checks preserve uncertainty rather than report partial data as healthy:

- [`systemd_unit_crash_loop`](../../reference/modules/systemd_unit_crash_loop.md) counts
  the new restart value after a counter reset, so intervening crashes are
  visible.
- [`systemd_unit_resource`](../../reference/modules/systemd_unit_resource.md) requires every
  nested process list, without a depth cutoff. A partial count could miss load.
- [`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md)
  keeps permission errors and unknown filesystem observations as findings.
- [`node_disk_device`](../../reference/modules/node_disk_device.md) and
  [`mdadm_array`](../../reference/modules/mdadm_array.md) require the configured device path
  to resolve, so a different device cannot stand in.
  A missing array fails. Unreadable array data is unknown.
- [`log_lines`](../../reference/modules/log_lines.md) reports matches found before a read
  problem. After a read error, the next run retries from the saved reading
  position where reading failed. For each oversized line, `log_lines` checks
  the first 64 KiB, then advances the reading position past the line's
  remainder. Advancing past an oversized line prevents a long line from
  blocking later alerts. A silence check uses the time of the newest
  complete line across the rotated and current log files, even with unread
  older lines.
- [`apt_security_updates`](../../reference/modules/apt_security_updates.md)
  requires package lists and a recent successful-refresh stamp.
  A failed refresh can leave a fresh unrelated list beside stale security
  data, so list times cannot prove success. Without the stamp, the check is
  unknown.
  The [module page](../../reference/modules/apt_security_updates.md#set-up) names
  what writes the successful-refresh stamp: Ubuntu's package
  `update-notifier-common`, and apt's
  `Post-Invoke-Success` hook that a Debian administrator writes.
- [`zypper_security_patches`](../../reference/modules/zypper_security_patches.md)
  does for SUSE what `apt_security_updates` does for Debian and Ubuntu, with
  the same seven-day rule. zypper's list of patches names no packages, so the
  check names patches. The evidence of a successful refresh is the file
  `repodata/repomd.xml` of each enabled repository in zypper's cache: zypper
  touches the file only when a refresh succeeds for that repository. The
  check runs zypper as the unprivileged account with `--no-refresh`, so it
  never downloads. The [module page](../../reference/modules/zypper_security_patches.md#set-up)
  gives the daily refresh that openSUSE Leap's cloud image lacks.
- [`dnf_security_updates`](../../reference/modules/dnf_security_updates.md) does the same
  for the Red Hat family, with the same seven-day rule, and names packages.
  dnf's stamp `/var/cache/dnf/last_makecache` cannot show a successful
  refresh, because `dnf makecache --timer`, which the refresh timer runs,
  touches it also when the refresh fails. The evidence is the file
  `repodata/*primary.*` of each enabled repository in dnf's cache: dnf touches
  it when the repository's server answered, also when the repository has not
  changed, and never at a failed refresh. dnf's cache belongs to root and dnf
  run as an unprivileged account reads an empty cache of its own, so the check
  runs `dnf --cacheonly` as MMWatchdog itself. Fedora's dnf 5 is told from
  dnf 4 by the link `dnf` to `dnf5`, and is read in JSON: its repositories,
  each with the time of its package data and its own `metadata_expire`
  (a repository is stale after `metadata_expire` plus a day, at least seven
  days), and its list of security advisories. What the check does not
  understand is unknown. Amazon Linux is unknown.
- [`ubuntu_pro_security_fixes`](../../reference/modules/ubuntu_pro_security_fixes.md)
  reads the Ubuntu Pro client's local security status and downloads nothing.
  ESM Apps and ESM Infra fixes that apt cannot yet install are findings,
  one per installed source package. The check is opt-in for Ubuntu nodes
  without the needed Pro service. Missing ESM lists on an unattached node
  make the check unknown; apt's successful-refresh stamp supplies freshness.
  The module page owns the setup, messages, and reasons.
- [`distribution_release_security_support`](../../reference/modules/distribution_release_security_support.md)
  reads the release's dates from the package `distro-info-data`. A node
  without the release-date file, or a release without a date in the
  release-date file, is unknown and never healthy. A guess would hide the
  end of security fixes.
- [`known_exploited_vulnerabilities_in_installed_packages`](../../reference/modules/known_exploited_vulnerabilities_in_installed_packages.md)
  downloads the list of known exploited vulnerabilities from CISA, the US
  cybersecurity agency. The check sends OSV.dev the distribution release
  and source package names, never a version. OSV.dev is a public database
  of flaws in open source packages. The check compares versions on the node.

  A package that another apt repository installed is left out, because
  the package's versions are not the distribution's.

  The check leaves out one kind of entry: a Debian entry whose
  `ecosystem_specific` urgency is `unimportant`, Debian's rating for a flaw
  that its security team does not treat as a security problem for that
  package. Only that entry is left out. An entry of the same flaw for
  another range or another package decides for itself, so the flaw still
  alerts when one of those entries affects the installed version. Every
  other urgency, a missing urgency, and every Ubuntu entry count. When
  such an entry leaves out a flaw that affects an installed package, the
  healthy message names the flaw, so the rating is never silent.

  If CISA or OSV.dev cannot supply the required data, the check is unknown,
  never healthy. The check is also unknown if OSV.dev returns no flaw for
  any source package in the node's release. OSV.dev also returns no flaws
  for an unknown release, so an empty answer cannot prove health.

The reasons for the behavior of one module are on the module's page, under
"Why it works this way".

Module pages that rely on Check group decisions:
[`node_resource`](../../reference/modules/node_resource.md),
[`node_filesystem`](../../reference/modules/node_filesystem.md),
[`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md),
[`node_network_interface`](../../reference/modules/node_network_interface.md),
[`mdadm_array`](../../reference/modules/mdadm_array.md).

Measurement checks that only record readings state `only_record_readings: true`.
A check that alerts states `alert_severity` instead. Requiring
`only_record_readings: true` or `alert_severity` makes the choice between
recording only and alerting visible in the configuration. Input limits
still belong on the check.

A Starlark module may define `validate_inputs(inputs)` for input values that
types alone cannot check. Validation and reload call the function after type
checks, before any run or delivery. The function returns `None` or error text.
The `validate_inputs(inputs)` function has no node access.
See
[the writing modules reference](../../reference/writing-modules.md#check-input-values-before-a-run).
