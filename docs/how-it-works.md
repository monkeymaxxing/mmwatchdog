# How MMWatchdog works

This page explains why MMWatchdog behaves the way MMWatchdog does. The
page starts with one node: what runs on the node, how a failed check
becomes an alert, and what survives a restart. Then the page adds the fleet hub, the security model, and the
connection to MMHotline. The [documentation index](README.md) lists the
pages that show how to do each job, and the reference pages for every
command and setting.

## One node, one program

MMWatchdog watches a Linux machine with systemd, called a **node**. One
program, `mmwatchdog`, does all of the work on the node:

- The **node monitoring process** is `mmwatchdog daemon`, which systemd
  runs as root from `mmwatchdog.service`. The node monitoring process runs
  the checks, decides when a problem is an incident, restarts services,
  and sends alerts.
- Each **command**, such as `sudo mmwatchdog status`, is the same program.
  A command talks to the node monitoring process through the socket
  `/run/mmwatchdog/mmwatchdog.sock`. Only root may use that socket, so the
  commands need `sudo`.

```text
YOUR LAPTOP                         THE NODE (a Linux server)

  mmx run hub_and_standalone        mmwatchdog.service: the node monitoring process
    installs the program,  ------>    runs each check, keeps state in /var/lib/mmwatchdog/,
    the configuration, and            restarts services, sends alerts
    the modules                     sudo mmwatchdog status: a command, through
                                      /run/mmwatchdog/mmwatchdog.sock
```

MMWatchdog runs nothing on your laptop. [MMX](https://github.com/monkeymaxxing/mmx/blob/main/docs/how-it-works.md)
installs and updates MMWatchdog on each node, and is not part of the
monitoring: a node goes on watching itself while your laptop is off.
[Installed files](reference/installed-files.md) lists every file, account,
and socket that an install puts on a node.

Each node watches itself, even in a fleet. An optional **fleet hub** is one
node whose MMWatchdog has the fleet hub role. The fleet hub collects reports
from the other nodes and sends the other nodes' alerts, but never runs the
other nodes' checks.
[The fleet](#the-fleet) explains the hub.

## Configuration: check groups contain checks

The configuration lives in `/etc/mmwatchdog/` on the node:

| File | What the file holds |
|---|---|
| `mmwatchdog_node_settings.yml` | The node's own settings, such as how long the node keeps its history |
| `check_groups/*.yml` | The check groups, in one or more files |
| `alert_destinations.yml` | Where alerts go. A node that joined a fleet hub has no such file, because the node gets its destinations from the hub |

A **check group** is a named set of checks for one thing that you run, such
as the application `myapp`. A **check** is one rule that MMWatchdog
watches, such as "the service runs" or "the health page answers 200":

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s                    # run each check every 30 seconds
    time_limit_per_run: 5s              # one run of a check may take 5 seconds
    alert_after_failing_for: 1m         # send the alert after 1 minute of failing
    recovered_after_healthy_for: 30s    # the failure is over after 30 healthy seconds
    checks:
      myapp_service_is_running:
        systemd_unit_state:             # the module of this check
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
```

The four timings of the check group `myapp` apply to all three checks. A
check can state its own `alert_after_failing_for` and
`recovered_after_healthy_for`. Each check fails and recovers on its own,
with its own name, such as `myapp/myapp_answers_health_request`. MMWatchdog
adds no checks that you did not write.

Each check names one **module**, such as `http`, with the module's inputs
below the module's name. A check group can also hold a single check without
`checks:`. That check is named after its module, such as `api/http`. A
**heartbeat check group** runs no check at all. The heartbeat check group
waits for a scheduled job, such as a nightly backup, to report success. The
heartbeat check group fails when no report arrives in time.

[Add a check](how-to/add-a-check.md) builds a check group step by step, and
the [configuration files reference](reference/configuration-files.md) lists
every setting.

## The parts of the node monitoring process

The node monitoring process is one program, with parts that each do one
job. The parts are not separate programs, so no part needs its own install.

| Part | What the part does |
|---|---|
| Node scheduler | Starts each check when the check is due, notices runs that were missed and heartbeats that did not arrive, and starts recovery actions when the recovery actions are due |
| Node check worker | Does one run of one check within the check's time limit, and returns the result. A `shell` check and a check whose module is not core run a child process |
| Node recovery worker | Saves a recovery attempt in `state.db`, then restarts the systemd unit or runs the recovery command |
| Node notification worker | Sends each alert to each destination, in order, and retries an alert that did not arrive |
| Node heartbeat receiver | Takes reports from scheduled jobs: through the heartbeat socket `/run/mmwatchdog/heartbeat.sock` for a job on the same node, or over HTTPS for a job on another machine |

The node keeps its state in two SQLite database files in
`/var/lib/mmwatchdog/`:

- `state.db` holds each check's state, open incidents, recovery attempts,
  maintenance windows, the event history, and the **outbox**, the alerts
  that wait for delivery.
- `measurement_history.db` holds the **readings** of every scheduled run.
  A reading is one value of a **measurement**, such as `memory_used_mib`
  at 14:02. No alert and no recovery action depends on the measurement
  history: the history is only for looking back, as
  [Look back at measurements](how-to/look-back-at-measurements.md) shows.

## From a failed run to an alert

MMWatchdog alerts when a check has been failing for a time that you
choose, `alert_after_failing_for`. So a short failure, such as a service
that restarts in a few seconds, need not wake anybody. A check that reports
new events, such as `out_of_memory_kills`, is the exception: the check
alerts at the first event.

Each run of a check gives an **observation**: `ok`, `failed`, or
`unknown`. `unknown` means that MMWatchdog could not tell, for example
because a program that the check runs did not start.

The first failed observation starts the check's **failing time**. The
failing time ends only when the check has been healthy for
`recovered_after_healthy_for`. So a check that fails on and off counts as
failing the whole time. Take a check group with
`check_every: 30s`, `alert_after_failing_for: 5m`, and
`recovered_after_healthy_for: 1m`, and a health request that fails on every
second run:

| Time | Observation | What MMWatchdog does |
|---|---|---|
| 12:00:00 | failed | Starts the failing time |
| 12:00:30 | ok | Healthy for less than 1 minute, so the failing time goes on |
| 12:01:00 to 12:04:30 | failed and ok in turn | The failing time goes on |
| 12:05:00 | failed | The failing time has lasted 5 minutes: opens an incident and queues the alert |
| 12:09:30 | ok | Starts counting healthy time |
| 12:10:30 | ok | Healthy for 1 minute: ends the failing time, resolves the incident, and queues the recovery alert |

The words on the status screen and in the alerts keep apart what happened:

| Term | Meaning |
|---|---|
| **Observation** | The result of one run: `ok`, `failed`, or `unknown`, with its time and evidence |
| **Failing time** | From the first failed observation until the check has been healthy for `recovered_after_healthy_for` |
| **Incident** | A problem that has lasted `alert_after_failing_for`. An incident opens and later resolves |
| **Event** | A recorded change, such as an incident that opens or resolves |
| **Alert notification** | The delivery of an event to a destination. A delivery can wait, be retried, or fail, apart from the state of the service |
| **Recovery action** | A restart of a systemd unit, or a recovery command. A recovery action that succeeds does not prove that the problem is fixed |
| **Recovery** | The check has been healthy for `recovered_after_healthy_for`, which ends the failing time |

Every waiting time of a check counts from the start of the failing time:
the alert, the restart (`restart_after_failing_for`), the recovery command
(`run_after_failing_for`), and the raise to `sos`
(`raise_to_sos_after_failing_for`). So a restart can come before the alert
or after the alert. A restart that fixes the service before
`alert_after_failing_for` sends no alert at all.

A few rules follow from the same idea, that only observed health ends a
failure:

- **An incident opens at a failed observation,** never at an `unknown` one.
  A check that stays `unknown` for `alert_after_failing_for` opens a
  separate **cannot-check incident** instead, so the alert does not claim
  that your application failed.
- **A finished recovery action proves nothing.** After a restart, fresh
  observations must be healthy for `recovered_after_healthy_for` before the
  incident resolves.
- **A missed or late run keeps the failing time running** and starts the
  healthy count over, because nobody saw the check healthy.

`mmwatchdog status` shows `confirming_failure` for a check in its failing
time whose incident has not opened, and `confirming_recovery` for a check
that is healthy while its incident is still open.
[Decide when a check alerts](how-to/decide-when-a-check-alerts.md) shows
how to choose the timings.

## Scheduling

The node scheduler looks for due checks every 100 milliseconds. Each check
starts `check_every` after its last scheduled start. A run counts as
**missed** when the run starts more than half an interval and 100
milliseconds late, unless only the check's own previous run caused the
delay. A missed run is an `unknown` observation.

The node runs at most `max_concurrent_checks` checks at the same time.
When the node cannot keep up, MMWatchdog records each run that MMWatchdog could
not make as an `unknown` observation. MMWatchdog does not build up a backlog of runs, so a slow
node gets late alerts about its checks, not an ever longer queue.

`sudo mmwatchdog check-now` runs a check at once, and takes turns with the
check's scheduled runs. So a run that you start by hand can delay a
scheduled run by up to one `time_limit_per_run`. A run that you start by
hand only shows the result: the run opens no incident, resolves none, and
restarts nothing. With `--count-as-scheduled-run`, the run counts as the
check's next scheduled run instead, so a fix shows in the check's state at
once.

A reload that changes only `check_every` keeps the check's state. The next
run then comes at the last scheduled start plus the new interval, or at
once if that time has passed.

## A run that does not end

Each run has a time limit, `time_limit_per_run`. At the limit, MMWatchdog
ends the run's programs and closes the run's connections:

- A check that runs a program, such as `shell`, gives `unknown`: `the run
  took longer than 5s (time_limit_per_run), so MMWatchdog ended it`.
- A check that asks a server, such as `http` or `tcp`, gives `failed`:
  `no answer within 5s (time_limit_per_run)`. A server that gives no answer
  in time has failed.

Some runs cannot be ended at all. For example, the kernel can hold a file
read on a network filesystem without end while the server that provides the
filesystem is gone. No program can cancel such a kernel operation. So
MMWatchdog waits 5 seconds after the time limit, then stops waiting:

- The run's observation is `unknown`: `the check run did not finish within
  the time limit of 5s. MMWatchdog stopped waiting for the run to finish`.
- The stuck run no longer counts against `max_concurrent_checks`, so the
  node's other checks go on.
- MMWatchdog starts no new run of that check until the stuck run ends. Each
  run that falls due meanwhile is `unknown` at once. So a check whose runs
  cannot end leaves one stuck run, not one more at each interval.
- When the stuck run ends at last, MMWatchdog throws its result away. The
  next run is an ordinary run.

A reload that changes the check, such as to another path, starts the
check's runs again, because a changed check is a new check.

## Restarts, and why a stopped unit stays stopped

A check group can restart a systemd unit when a check fails, with
`restart_systemd_unit`. A restart is due when all of the following conditions hold:

- A check that the restart names has failed, at least
  `restart_after_failing_for` after the start of its failing time.
- An attempt remains, and `min_time_between_attempts` has passed since the
  last attempt.
- No maintenance window covers the check group.
- The check is not kept quiet because of another failure
  ([One cause, one alert](#one-cause-one-alert)).

An `unknown` observation never makes a restart due, because a check that
MMWatchdog cannot run says nothing about the service.

Before the restart, MMWatchdog asks systemd for the unit's state:

| systemd reports | What MMWatchdog does |
|---|---|
| `active` | Restarts the unit: the unit runs, but a check of the unit fails, such as the health request |
| `failed` | Restarts the unit: the program crashed, and systemd did not restart the unit |
| `inactive` | Leaves the unit alone, and uses no attempt |
| starting, stopping, or reloading | Waits, and asks again after the next observation |

**MMWatchdog never starts a stopped unit,** and no setting changes that
rule. systemd reports `inactive` for a unit that someone stopped on
purpose: an operator who repairs a database by hand, a package upgrade, or
a failover tool. Whoever stopped the unit decides when the unit runs again.
MMWatchdog records one `systemd_unit_restart_withheld` event for the
failing time, and the check's alert still goes out at
`alert_after_failing_for`. To stop a unit on purpose without an alert,
[pause alerts](how-to/pause-alerts-during-planned-work.md) first.

systemd also reports `inactive` for a program that ended by itself with
exit status 0. So a program that must always run needs `Restart=always` in
its unit file. Then `inactive` can only mean a stop on request.

MMWatchdog saves each attempt in `state.db` before the attempt runs
`systemctl restart`. So an attempt counts even when MMWatchdog stops during
the restart. When
all attempts are used, MMWatchdog stops restarting, and keeps running the
checks and sending alerts. MMWatchdog never clears systemd's start limits,
so a unit that systemd refuses to start stays failed and visible.

Check groups that restart the same unit share the unit's attempts, so two
check groups cannot restart one unit twice as often as each allows.
[Restart a service automatically](how-to/restart-a-service-automatically.md)
sets up a restart or a recovery command.

## One cause, one alert

When a database stops, the database check fails, and so does every check
of an application that needs the database. A check can name the checks of
its causes in `no_alerts_or_recovery_actions_while_these_checks_fail`.
While a named check is in its failing time, MMWatchdog **keeps the check
quiet**: no alert of the quiet check and no recovery action because of the
quiet check. The one alert is the alert of the cause, and MMWatchdog does
not restart the application while the database is down.
[Stop alerts that follow another failure](how-to/stop-alerts-that-follow-another-failure.md)
shows the settings.

Each rule of the quiet time has a reason:

- **The quiet check keeps running** and `mmwatchdog status` shows the quiet
  check's state, so you still see everything that fails.
- **The quiet time ends with the quiet check's own first run after the
  cause is healthy again.** A result from during the outage says nothing
  about the check now. So that first run decides: a healthy run sends
  nothing, and a failed run sends the alert and starts the restart, if the
  alert and the restart are due.
- **Nothing is sent later for the quiet time.** The quiet check's waiting
  times kept counting. An alert that fell due in the quiet time goes out
  with the first failed run after the quiet time, at the severity that the
  incident reached.
- **A start of MMWatchdog keeps the check quiet** until MMWatchdog has
  observed each named check again. Without that rule, a restart of
  MMWatchdog in the first minutes of an outage would send the alerts that
  the setting holds back.
- **Checks cannot name each other in a circle.** Two checks that keep each
  other quiet would send no alert at all while the two checks fail. A chain is
  allowed: the check at the end of the chain alerts.
- **A cause must have a destination** while the check that names the cause
  would alert. Otherwise a cause that alerts nobody would hide the only
  alert.
- **A heartbeat check group cannot be kept quiet.** A heartbeat has no runs
  that could decide after the quiet time, and a missed heartbeat stays
  missed after the cause is healthy again.
- **`mmwatchdog validate` warns about a cause with a lower severity.** The
  one alert goes out at the cause's `alert_severity`, so a destination that
  takes only `sos` hears nothing while a `warn` cause fails. The warning
  leaves the configuration valid.

MMWatchdog orders nothing and starts nothing because of the setting:
starting services in order belongs to systemd.

## Modules

Every check and every alert destination names one **module**, such as
`http` or `slack`. A module has one of three kinds:

| Kind | What the module is | Where the module lives on a node |
|---|---|---|
| **Core module** | Go code built into `mmwatchdog`, such as `http`, `systemd_unit_state`, `postgresql`, or `webhook` | In the `mmwatchdog` program |
| **Official extra module** | A [Starlark](https://github.com/bazelbuild/starlark) script that the MMWatchdog project writes, such as `slack` or `zfs_pool`. Starlark is a small language that reads like Python | `/usr/local/lib/mmwatchdog/official_extra_modules/NAME/` |
| **Module of the project's own** | A Starlark script, or an executable, that your team writes | `/usr/local/lib/mmwatchdog/external_modules/NAME/` |

A check names a module of every kind in the same way, so the configuration
never says the kind. `sudo mmwatchdog module-usage` shows which modules a
node's configuration names, and the kind of each.

**Why some modules are not core.** The checks that many servers need are
core, so that you find them in the program from the first install. Core
checks cover the node itself, systemd, files, HTTP, TCP, TLS, and common
server software such as SSH, PostgreSQL, and Redis. The core destinations are the general
ones: `webhook`, `email`, `system_log`, and `mmhotline`. An uncommon check,
such as a ZFS pool, and a destination that one company runs, such as Slack
or Pushover, are official extra modules. The official extra modules come in
the same release as the program, and MMWatchdog refuses to run with official
extra modules of another release. MMWatchdog reads only the modules that
the configuration names, so an official extra module that you do not use
is never read and never runs.

Three ideas shape every module:

- **A module touches the node only through the primitives,** functions
  such as `read_file`, `run_command`, and `http_request`. The core modules
  use the same primitives that a script calls. An executable module of
  your own is the exception: MMWatchdog cannot see what an executable does.
- **A module describes its inputs.** A core module's description is built
  in, and every other module has `module_description.yml`.
  `mmwatchdog validate` checks every check and destination against the
  description of its module before anything runs.
- **A check module measures, and MMWatchdog does the rest.** The module
  reports what the module found. MMWatchdog owns the schedule, the time
  limit, the failing time, incidents, restarts, and alerts.

### A run of a module that is not core

When a check of a module that is not core falls due, the node check worker
starts a child process on the node:

1. For a script, the child process is `mmwatchdog run-starlark-module`
   with the module's directory. For an executable, the child process is the
   module's file `module_executable`.
2. The child process runs as the account in `run_as_user_account`, by
   default the unprivileged account `mmwatchdog`, with a fresh environment.
3. The child process gets the check's inputs as JSON on standard input.
   For each input whose name ends in `_file`, MMWatchdog reads the file as
   root and passes the file's contents, so the module never needs to open
   a credential file.
4. The module answers with one JSON result: a status, a message, and any
   measurements. MMWatchdog checks the result against the module
   description, and applies the check's limits, such as
   `fail_when_measurement_above`.

The run ends at `time_limit_per_run`, and a script also ends after 10
million steps. A script error, a crash, or a result that does not match
the module description gives `unknown`, not `failed`. Each such check has
a directory of its own under `/var/lib/mmwatchdog-module-state/` for what
the module keeps from one run to the next. One run can report several
**findings**, such as several failed systemd units, and each finding is an
incident of its own.

A destination module that is not core runs in the same way, once for each
delivery attempt, and answers `delivered` or `failed`.
[Writing modules](reference/writing-modules.md) has every detail of the
protocol, the primitives, and the limits.

### How modules reach a node

Nothing is downloaded while MMWatchdog runs. Each MMX deploy copies two
directories of your deployment project to each node:
`official_extra_modules/`, which comes with the release, and
`mmwatchdog_external_modules/`, which holds your own modules. The deploy
then works in this order:

1. The deploy copies the two directories to candidate directories beside the
   installed ones.
2. The new `mmwatchdog` program validates the new configuration against the
   candidate directories. A module with a fault stops the deploy here,
   before anything that runs has changed.
3. The candidate directories replace the installed directories. So a node
   has exactly the modules of its last deploy, and a module that you
   removed from the project is removed from the node.
4. The deploy restarts MMWatchdog when a module file changed, because
   MMWatchdog reads each module's description when MMWatchdog starts.

A script needs nothing on the node but MMWatchdog. An executable brings its
own needs, such as an interpreter, and must match the node's processor.
[Write your own check or destination](how-to/write-your-own-check-or-destination.md)
deploys a module of your own.

## The outbox and delivery

An alert must not get lost when a destination is down, when the network
fails, or when MMWatchdog restarts. So every alert passes through the
outbox in `state.db`:

1. When an incident opens, MMWatchdog saves the incident, the event, and
   one delivery for each destination in one database commit. Either all
   three are saved, or none is.
2. The node notification worker sends each destination's deliveries in
   order. Each destination has a queue of its own, so a destination that is
   down never delays another destination.
3. The delivery leaves the outbox only when the destination accepts the
   alert. What counts as accepted depends on the module, such as a `2xx`
   answer for a `webhook`, or the answer `delivered` from a module that is
   not core.
4. A failed attempt is retried after 1 second, then 2, 4, and so on, up to
   every 256 seconds. MMHotline can ask for a wait of its own choosing,
   and MMWatchdog then waits that long. A retry carries the same event ID, so a receiver
   can tell a repeat from a new alert.

Each attempt writes one line in the node's journal, which
`journalctl -u mmwatchdog` shows: the alert's title, the destination, and
whether the destination accepted the alert.

**A stopping MMWatchdog lets a delivery already on its way finish,** for up
to 5 seconds. A normal answer takes well under a second. Without the 5
seconds, the destination could get the alert while MMWatchdog never
records the success, and the next start would send the alert again. An
attempt still unanswered after the 5 seconds stays in the outbox, and
MMWatchdog sends the alert again after the next start.

**The outbox has limits.** An alert that waits longer than
`keep_undelivered_alerts_for`, or that would grow the outbox past
`undelivered_alerts_content_size_limit`, is dropped. MMWatchdog then
records a `delivery_gap` event, and `mmwatchdog status` shows the gap. So
a receiver can get a recovery alert for an incident whose failure alert the
receiver never got. Match alerts by `incident_id`.

**A changed destination loses its queue.** When a reload removes a
destination, or changes the destination's module, receiving address, or
credential contents, MMWatchdog cancels the destination's queued alerts. The
queued alerts were meant for the old receiver, so MMWatchdog does not send
the queued alerts to a new receiver. A new password or token for the same receiver keeps the
queue, because the receiver stays the same.

**Reminders repeat an open incident** to each destination with a
`remind_every` duration. A reminder that still waits for delivery is
replaced by the next reminder for the same incident and destination. So
reminders cannot fill the outbox and push out a real alert, however long a
destination is down. A maintenance window and a quiet time hold the
reminders.

**Evidence is frozen when the incident opens.** Each incident event carries
the observation that opened the incident, the configuration source, and
the timings, up to 16 KiB. Retries, reminders, and the recovery alert carry
the same evidence, so a late delivery still shows what MMWatchdog saw at
the time.

[Send alerts where your team sees them](how-to/send-alerts-somewhere.md)
sets up destinations, and the [alert format](reference/alert-format.md)
shows what an alert contains.

## Reboot notices

A node that rebooted without warning may need a look, even when every
check is healthy again. So MMWatchdog sends a **reboot notice** after each
reboot, with no setting other than the notice's severity,
`alert_severity_when_this_node_has_rebooted`. The notice says, for example,
`web01 rebooted at 14:03 UTC; MMWatchdog last ran at 14:01 UTC`.

MMWatchdog tells a reboot from a restart of the service by the kernel's
**boot ID**, `/proc/sys/kernel/random/boot_id`, which is new at each boot:

- At each start, MMWatchdog compares the boot ID with the boot ID saved in
  `state.db`. A new boot ID means that the node rebooted.
- A restart of `mmwatchdog.service` within one boot keeps the boot ID, and
  sends nothing.
- The first start on a node has no saved boot ID, and sends nothing.
- The time of the reboot comes from `btime` in `/proc/stat`. When
  MMWatchdog cannot read the boot ID or the boot time, it logs
  `reboot detection:` with the error in the journal, and sends no reboot
  notice for that start.

MMWatchdog also saves the node's **machine ID**, from `/etc/machine-id`,
which stays the same across reboots. State copied from another machine,
such as when you move the fleet hub to a new server, has another machine
ID. The new server then sends no reboot notice, because the new server did
not reboot. Machines cloned from one disk image can share a machine ID.
A move between such machines then looks like a reboot and sends the
notice, which is the safe direction.

The time "MMWatchdog last ran" is the last time MMWatchdog saved its state
before the reboot: the shutdown after a clean reboot, or the last
observation before a crash. A whole-node maintenance window that was open
before the reboot keeps the notice local, so a reboot during a deploy
sends nothing.

## State that survives restarts

MMWatchdog saves its state in `state.db` as the state changes. A restart of
MMWatchdog, of the node, or an update keeps:

- open incidents, with their failing times and evidence
- recovery attempts, so a restart of MMWatchdog gives no new attempts
- maintenance windows, which still end at their set time
- heartbeat deadlines
- the outbox, and the time of each destination's last reminder, so a
  restart neither repeats nor skips a reminder

A start of MMWatchdog ends a failing time whose incident has not opened
yet, and starts each healthy count over. MMWatchdog did not watch the node
while MMWatchdog was stopped, and unobserved time earns no credit in either
direction. An open incident stays open until MMWatchdog observes lasting
health.

Every wait in MMWatchdog counts on the system's monotonic clock, which
only moves forward. So setting the clock, or a time-sync jump, does not
shorten or lengthen a waiting time.

When `state.db` cannot be written, MMWatchdog keeps observations in memory
and saves them once writing works again, so monitoring goes on. A change
that you make, such as a reload or a maintenance window, must be saved
before the change takes effect, so the command fails instead.
`mmwatchdog status` shows the storage problem.

## Configuration changes while checks run

`sudo mmwatchdog reload` reads the whole new configuration and validates
the configuration before anything changes. An invalid configuration leaves
the running configuration as the configuration was. A valid configuration
is saved first, then put into use, and the command prints any validation
warnings.

- **An unchanged check keeps its state and its schedule,** so a reload
  in the middle of an outage does not restart the failing time.
- **A changed or removed check closes its open incident** with a
  `closed_without_recovery` alert, which says that the incident closed,
  maybe not fixed. MMWatchdog never claims that the service recovered.
- **A run that started before the reload cannot update the changed
  check.** Each run carries a number for the check definition that the run
  started under, and MMWatchdog throws away a result whose number is old.
  The start and end of a maintenance window throw away older results in
  the same way.
- **Some settings need a restart of MMWatchdog,** because the settings
  open files and sockets at start: `max_concurrent_checks`, `socket`,
  `state_dir`, `module_state_dir`, `heartbeat_receiver`,
  `state_database_size_limit`, and on the hub
  `fleet_hub_role.listen_on_address`, `fleet_hub_role.address_for_nodes`,
  and `fleet_hub_role.event_history_of_nodes_content_size_limit`.
  `mmwatchdog reload` refuses a change of any setting in the list, and says
  so.

## The fleet

A fleet lets you see many nodes in one place and send the nodes' alerts
from one place. One node gets the **fleet hub role** with `fleet_hub_role:` in
its settings. The hub still watches itself, as every node does. Every
other node joins the hub with a **join code**, and is then a **joined
node**:

```text
                      FLEET HUB (hub01): watches itself, and
                        keeps the fleet's destinations and every node's reports
                        groups alerts, notices silent nodes
                         ^                  |
   reports and alerts,   |                  |  alerts
   over HTTPS            |                  v
                         |            YOUR DESTINATIONS (Slack, email, MMHotline)
   JOINED NODES                             ^
   web01   web02   app01                    |
   each watches itself    ------------------+
                          direct delivery, only when the hub does not accept an alert
```

**The joined nodes start every connection.** The hub holds no login for any
node, cannot run a node's checks, and cannot start a node's recovery
actions. A joined node keeps watching itself and restarting its services
while the hub is down.

**A joined node sends a node status report** every `report_every`. A report
is a snapshot of the node's current state, with the events and readings
that the hub has not received yet. A failed report is not retried, because
the next report is newer. Events and readings are the exception: the hub
tells each node how far the hub has received, so after a hub outage the
hub catches up.

**Alerts take a faster path than reports.** A joined node sends each alert
to the hub at once. The hub accepts an alert only after saving the alert,
and then sends the alert through the hub's own outbox.

**The hub groups alerts from many nodes.** The first alert of a kind goes
out at once. Matching alerts in the following minute, such as the same
check failing on seven nodes, become one message when the minute ends:
`myapp/http failed on 7 nodes`. A notice is never grouped. A finding with
an exact finding, such as one security flaw in one package version, gets
one alert across the whole fleet, however many hours the nodes take to
report it ([one alert per exact finding](#one-alert-per-exact-finding)).

**A node sends directly when the hub does not accept an alert.** Each
joined node keeps a copy of the fleet's destinations, with their
credentials, in `/var/lib/mmwatchdog/fleet-settings-from-the-hub.json`.
When the hub has not accepted an alert after
`send_alert_without_hub_after_waiting_for`, the node sends the alert to the
destinations itself. If the hub saved the alert but the hub's answer got
lost, the alert can arrive twice, with the same event ID. A rare duplicate
is better than a lost alert. A node that has no copy of the fleet settings
yet can only wait for the hub.

### Silent nodes and the hub-outage alerter nodes

**The hub notices a node that stops reporting.** On the hub, each joined
node is a check, such as `node_reports/web01`. After three report intervals
without a report, 90 seconds with `report_every: 30s`, the node is
**silent**, and the hub alerts. Each node states the severity of the
node's own silence alert, because only the node's owner knows how much one
silent server matters. For example, use `warn` for one of many web servers,
and `sos` for the database server. A restart of the hub gives every node three fresh report
intervals, so a hub restart alone alerts nobody.

A new node can take the name of a silent node. The hub then closes the old
node's incident without claiming recovery, with the text `web01 joined the
fleet again as a new node; MMWatchdog does not know whether the problem is
fixed`, and the new node starts with its own check.

**Two joined nodes watch the hub.** A hub that is down cannot alert about
itself. So the hub chooses two reporting nodes as **hub-outage alerter
nodes**, or one in a fleet with only one joined node. Each hub-outage
alerter node has the check `node_reports/to_hub`. When no report of the
alerter node reaches the hub for `alert_after_hub_unreachable_for`, the
alerter node sends an alert directly to the destinations. A node with
`prefer_this_node_as_hub_outage_alerter: true`, such as a long-lived
database server, takes the role first.

Both alerter nodes use the same fleet incident key for the hub outage
(`node_reports/to_hub failed`), so a service that keeps incidents of its own
gets one incident for the outage, not one for each alerter node.

If the hub and both alerter nodes fail together, MMWatchdog cannot alert
until the hub or one of the alerter nodes is back. [The MMHotline connection](#the-mmhotline-connection)
covers that case from outside: MMHotline alerts when the hub's check-ins
stop. [Watch a fleet](how-to/watch-a-fleet.md#when-the-hub-stops) shows
what a hub outage looks like.

### One incident per check across the fleet

Some services keep incidents of their own, such as MMHotline or Pushover,
which repeats an `sos` alert until somebody acknowledges the alert. Such a
service should get one incident for `myapp/http`, not one incident for each
node where `myapp/http` fails.

So every alert about an incident carries a `fleet_incident_key`, the same
for one check across the whole fleet. When `myapp/http` fails on `web01`
and then on `web02`, the service gets the failures with the same key. When
`web01` recovers, the service gets nothing, because `web02` still fails.
When `web02` recovers too, the hub sends one closing alert: `myapp/http is
ok on every node again (was failing on web01, web02)`. A chat destination,
which keeps no incidents, gets each node's recovery as usual.

A joined node that sends directly never sends a closing alert, because only
the hub knows whether another node still fails. The hub sends the closing
alert once the hub is back. A standalone node uses the node's own name in
the key, so separate nodes never share an incident.

### One alert per exact finding

Some checks report each problem as a [finding](#a-run-of-a-module-that-is-not-core),
such as each package with a known security flaw. A core module that
reports findings also gives each one an **exact finding**: one line that
names the problem precisely, such as
`Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z`.
Two nodes with the same exact finding have the same problem, and one
investigation covers both.

A check that runs every 6 hours reaches the hub from each node at a
different time. So the hub keeps, for each destination, what it has
already announced about each key:

- **The first node's alert goes out at once.**
- **A later node with an announced exact finding waits.** The hub sends one
  update for every such key together on the **shared tick**, every
  `send_newly_affected_nodes_every` (6 hours by default, at 00:00, 06:00,
  12:00, and 18:00 UTC). The update names the nodes that joined, changed,
  or are ok again, at the highest severity among the nodes that it adds.
- **Anything new goes out at once:** an exact finding that was not
  announced, such as after an upgrade to another affected version, a
  severity above the announced one, and every `sos` node, which goes out
  within a minute.
- **One closing alert** goes out when the last node of the key recovers.
  It names the nodes that it covered: `CVE-2026-123158 openssh is ok again
  on the 3 nodes that this alert covered`.

This holds for every destination, also a chat or a mailbox. An alert
without an exact finding, such as an HTTPS check that fails with a
certificate error on one node and with HTTP 503 on another, keeps the
one-minute grouping, because only the module knows whether two failures
have the same cause.

**Set a finding aside after an investigation.** When a team decides that
an exact finding needs no action on any node, an entry in the hub's file
`/etc/mmwatchdog/set_aside_findings.yml` sets it aside. The hub then sends
no alert below `sos` for it, closes the incident that it opened with
`Set aside after investigation: <reason>`, and the fleet view counts it on
one line, such as `Set aside: 1 finding on 5000 nodes`. Any change to the
package, release, fix state, or advisory changes the exact finding, so the
finding alerts again. `sos` is never set aside, and joined nodes never set
anything aside, so a hub outage can only add alerts.
`mmwatchdog fleet set-aside-findings` lists the entries in effect, and
`mmwatchdog fleet findings CHECK_GROUP/CHECK` lists every node with each
exact finding of a check ([commands](reference/commands.md#mmwatchdog-fleet-findings)).
The [fleet design](changing-mmwatchdog/design/fleet.md#set-a-finding-aside)
gives every rule.

[Set up a fleet](how-to/set-up-a-fleet.md) joins the nodes, and
[Change a fleet](how-to/change-a-fleet.md) adds and forgets nodes, rotates
the join code, and moves the hub.

## Security model

MMWatchdog is for administrators who already have root on their nodes.
MMWatchdog trusts the administrators, the configuration, and the modules
that the administrators install, as `sudo` does. MMWatchdog protects the
boundaries between the node monitoring process and everything else on the
node, and between the nodes of a fleet. MMWatchdog does not protect a node
from the node's own administrators.

**The node monitoring process runs as root,** because the node monitoring
process reads systemd and restarts services. Core modules run inside the
node monitoring process. A `shell` check, a module that is not core, and a
recovery command run as a child process under the account in
`run_as_user_account`, by default the unprivileged account `mmwatchdog`.
That account is an operating system boundary, not a sandbox: a module can
do anything that the module's account can do. The time limit, the step
limit, and the output limits keep a run in bounds, but do not make an
untrusted module safe to install.

**Files and sockets on the node:**

| What | Protection |
|---|---|
| The configuration and the modules | Owned by root. The configuration and the modules are trusted input: the configuration can choose `run_as_user_account: root`, and a module does whatever the module's account may do |
| Secret files, such as a webhook token | Owned by root with mode `0600`. `mmwatchdog validate` and `mmwatchdog reload` refuse a secret file that other accounts can read, unless the configuration allows that file. A module gets a secret on standard input, never as a path, an argument, or an environment variable |
| `/var/lib/mmwatchdog/` | MMWatchdog refuses to start unless root owns the directory with mode `0700`. The databases have mode `0600`. MMWatchdog adds no encryption: file permissions protect the stored secrets |
| `/run/mmwatchdog/mmwatchdog.sock` | Owned by root with mode `0600`. Every command goes through the socket, so `sudo` decides who may read status and who may change anything. The socket has no separate read-only role |
| `/run/mmwatchdog/heartbeat.sock` | Any account may connect, and each request needs the bearer token of one heartbeat check group. See below |

**Why the heartbeat socket is safe.** A scheduled job, such as a backup
that runs as the account `backup`, must report success without `sudo`. So
any account may connect to `/run/mmwatchdog/heartbeat.sock`. The socket
accepts only one kind of request: a heartbeat for one heartbeat check
group, with that group's bearer token. A token can mark only that group's
job as done, and opens no other command. The socket also needs no
certificate, because only root can listen at that path. systemd creates
`/run/mmwatchdog/` for MMWatchdog, owned by root, and removes the directory
when MMWatchdog stops. No other account can create a directory in `/run`.
Before `mmwatchdog heartbeat` sends the token, the command checks three
things. Root owns the socket and the socket's directory. The socket and
the directory are not symbolic links. No other account can write to the
directory. So a job never
sends its token to a program that only pretends to be MMWatchdog, also
while MMWatchdog is stopped. A job on another machine reports over HTTPS
instead, and checks the receiver's certificate.

**Joining a fleet.** The join code holds the hub's address, the SHA-256
fingerprint of the hub's certificate, and a shared join secret. So the join
code is a secret, and the deploy installs the join code as a private file.
A joining node checks the hub's certificate against the fingerprint before
the node sends the join secret, so the node cannot join an impostor. For
the join and for every later report and alert, the node trusts only that
certificate, never a certificate authority, and follows no redirect from
the hub. The hub then gives the node a credential of its own, and keeps only a hash of
the credential. The hub takes each report's node from the credential, never
from a name in the report.

- **Every join sends an `info` notice** that names the node and the
  address it joined from, also when a new node takes a silent node's name.
  A node that you did not add means that someone else has the join code.
- **A name can be taken over only from a silent node.** Anyone with the
  current join secret can join under a new name. A node that reports keeps
  its name, so the join secret cannot take over a working node.
- **Rotating the join code and forgetting a node do different things.**
  `mmwatchdog fleet rotate-join-code` changes the secret for future joins,
  and leaves joined nodes connected. `mmwatchdog fleet forget NAME` deletes
  one node's credential, but the node can join again with a current join
  code. To shut out a node, rotate the join code and forget the node.

**Direct delivery puts the destination credentials on every joined node.**
A joined node must reach the destinations while the hub is down, so the hub
copies each destination's tokens and passwords into every joined node's
`fleet-settings-from-the-hub.json`. Root on any joined node can therefore
read the destination credentials. So can anyone with the join code alone:
the code lets them join as a new node, and the hub then sends them the
same settings. Guard the join code like the destination credentials
themselves.

With the signing secret of the `mmhotline` destination, a person can send
check-ins and alerts that MMHotline accepts as the hub's, if they can
reach MMHotline's alert intake address. Such check-ins can keep MMHotline
from warning that the hub's check-ins stopped, and a check-in can put the
hub in planned maintenance. Keep MMHotline's alert intake address on a
private network that only your servers reach.

Root on the hub can read the destination credentials too, and can change
the settings that the nodes receive. A joined node runs the hub's
destination modules as the account `mmwatchdog`, from `/`, with no added
environment variables, whatever the hub's settings say, so the hub cannot
choose how programs run on a node. The hub cannot run a node's checks or
recovery commands.

**Data from outside is bounded.** Module results, node status reports, and
alerts from nodes must match a strict format and a size limit. Text that a
node sends is shown with its control characters replaced, so a node cannot
control your terminal. The format checks and size limits keep the data in bounds, but cannot
prove that a node reports the truth: root on a node can fake that node's
reports.

**Connections to destinations.** A `webhook` destination may use HTTP or
HTTPS, and never follows a redirect. An HTTP address sends the alert and
any token in plain text. An `email` destination with `starttls` or
`implicit_tls` checks the mail server's certificate, and `unencrypted`
sends the alert and any login in plain text. The `mmhotline`
destination signs every request and accepts only a signed receipt.

## The MMHotline connection

MMHotline is the on-call program that a team runs on the team's own
server. MMHotline knows who is on call, notifies that person, and escalates
an `sos` alert that nobody acknowledges. MMWatchdog works fully without MMHotline.

Each team's MMHotline is a destination of its own, with the module
`mmhotline`, the team's address, and the team's signing secret. Alerts to
MMHotline go through the outbox, as every alert does. Each attempt is one
signed request, and only a signed receipt from MMHotline counts as
delivered, so an impostor cannot accept an alert in MMHotline's place.

**Check-ins show MMHotline that MMWatchdog runs.** The fleet hub, or a
standalone node, sends each `mmhotline` destination a short signed
check-in every 30 seconds, at start, and after each reload. When the
check-ins stop, MMHotline alerts. So MMHotline notices a hub that died
together with the hub-outage alerter nodes, and a standalone node that
stopped. Each destination has its own time limit, so one team's slow
MMHotline does not delay another team's check-ins.

**In a fleet, the hub sends to MMHotline.** Alerts that share a
`fleet_incident_key` become one MMHotline incident: 40 nodes that go silent
together are one incident. When no node remains at `sos` for the key, the
hub sends `resolved-sos`, so a server left silent at `warn` does not keep
the `sos` alert escalating. An update that adds nodes to a key with an
exact finding goes at the highest severity of the added nodes, so
MMHotline's triage agent looks at the new nodes; an update with only
recovered nodes is `info`.

A joined node sends to MMHotline only an alert that the hub did not accept
in time. The joined node uses the same `mmhotline_url` as the hub, so that
address must be one that every node reaches. A joined node never sends a
check-in or a resolution. The `mmhotline` destination can give such
direct alerts an alert source of their own,
`alert_source_for_nodes_sending_without_the_hub`. MMHotline merges repeated
warnings from that source only, by key and by the message's first line,
so a hub outage on a large fleet cannot use up MMHotline's hourly budget.
That is why the first line of every `warn` and `sos` message to MMHotline
names the problem with no node name. The block needs MMWatchdog 0.2.0 on every joined
node ([MMHotline in a fleet](changing-mmwatchdog/design/fleet.md#mmhotline-in-a-fleet)).

A joined node holds each team's signing secret for direct delivery. Root on
a joined node can therefore send alerts to MMHotline as the fleet.
[MMHotline as a destination](how-to/send-alerts-somewhere.md#mmhotline-as-a-destination)
sets the connection up, and the [`mmhotline` module page](reference/modules/mmhotline.md)
covers signatures, receipts, and rotating the signing secret.

## Where to go next

- [Get started](get-started.md): your first hour, from install to an alert
  in Slack.
- [When something fails](when-something-fails.md): what to do when an alert
  does not arrive, a check shows `unknown`, or a node goes silent.
- [Commands reference](reference/commands.md): every command and what each
  check state means.
- [Configuration files reference](reference/configuration-files.md): every
  setting, default, and limit.
