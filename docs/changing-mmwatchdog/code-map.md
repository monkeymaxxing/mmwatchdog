# Where the code lives

This page is for a contributor who wants to find the code of one part of
MMWatchdog, or who reads the source for the first time. The
[implementation rules](implementation-rules.md) say which constraints a change
keeps. [How MMWatchdog works](../how-it-works.md) says what the parts do and
how a user sees them.

MMWatchdog's Go code is in seven packages under `internal/`. The command line
of the `mmwatchdog` program is in `cmd/mmwatchdog`. The directory
`internal/testhelpers` holds what the tests of several packages share and no
production code.

## The seven packages

Each package imports only packages below the package's row in this table. The
Go compiler refuses an import in the opposite direction. So a check module
cannot reach the incidents, the store, or the fleet, and the store cannot
reach the daemon.

| Package | What the package holds | Imports of MMWatchdog's packages |
| --- | --- | --- |
| `internal/daemon` | The node monitoring process: `Daemon`, the scheduler, recovery, alert delivery, the local API, the fleet's HTTP handlers and loops | every package below |
| `internal/fleet` | A fleet hub and its joined nodes: the hub's database `fleet.db`, the node status report and the reporter, the fleet's alert settings and grouping, and the fleet checks of silent nodes | `store`, `incidents`, `config`, `checksandalerts`, `primitives` |
| `internal/store` | A node's SQLite database: the snapshot of its checks, the event history, the outbox, the hub's alert intake, queued MMHotline alerts, and the measurement history | `incidents`, `config`, `checksandalerts`, `primitives` |
| `internal/incidents` | How observations become each check's state, incidents, and events, with the severity an incident reaches, the quiet time of the one-cause rule, and the recovery policies | `config`, `checksandalerts` |
| `internal/config` | A node's whole configuration: reading files, finding each setting's source, and validating relationships between checks | `checksandalerts`, `primitives` |
| `internal/checksandalerts` | What a check and an alert destination are: their settings, the core modules and their registries, the modules that are not core, what a run observes, and the event that a destination delivers with the text of its alert | `primitives` |
| `internal/primitives` | The functions through which every module reads the node: a file, a program, an HTTP request, a TCP connection, the processes of the node, and the questions MMWatchdog asks systemd | none |

The code of the Go modules that MMWatchdog builds with, such as SQLite and the
Starlark interpreter, is in `third_party/`, one pruned copy for each module, as
the [implementation rules](implementation-rules.md#go-dependencies) describe.

A feature that spans packages keeps one file name in each package that holds a
part of the feature. For example, `onecause.go` in `config` checks the checks
that a check names as its causes, `onecause.go` in `incidents` keeps the quiet
time in a check's state, and `onecause.go` in `daemon` decides, while the
daemon runs, which checks are kept quiet. When you look for a feature, search
for its file name in all packages.

## Start here, by topic

| Topic | Files | What the files explain |
| --- | --- | --- |
| Commands and startup | [`cmd/mmwatchdog/main.go`](../../cmd/mmwatchdog/main.go), [`fleet.go`](../../cmd/mmwatchdog/fleet.go) | CLI commands, daemon startup, and local API requests |
| Configuration | [`internal/config/config.go`](../../internal/config/config.go), [`source.go`](../../internal/checksandalerts/source.go), [`settings.go`](../../internal/checksandalerts/settings.go) | Configuration types, validation, defaults, and source locations; the settings of a check group, a check, and a destination |
| Check modules | [`module.go`](../../internal/checksandalerts/module.go), [`checkmodules.go`](../../internal/checksandalerts/checkmodules.go), `check_NAME.go` | What every check module implements, the registry with one line for each core module, and one file for each module with its inputs, its checks, and its code |
| Destination modules | [`destination.go`](../../internal/checksandalerts/destination.go), [`destinationmodules.go`](../../internal/checksandalerts/destinationmodules.go), `destination_NAME.go` | The core destination modules: `webhook`, `email`, `system_log`, and `mmhotline`. The registry has one line per module. Each module's file holds the inputs, input checks, and code |
| Primitives | [`primitives.go`](../../internal/primitives/primitives.go) | The functions through which every module touches the node: reading a file, running a program, an HTTP request, a TCP exchange |
| Modules that are not core | [`modulecatalog.go`](../../internal/checksandalerts/modulecatalog.go), [`moduledescription.go`](../../internal/checksandalerts/moduledescription.go), [`modulecall.go`](../../internal/checksandalerts/modulecall.go) | The two module directories of a node and the version check, the module description and how a check or a destination is checked against it, and one call of such a module for a check or for a destination |
| Scripts and executables | [`starlark.go`](../../internal/checksandalerts/starlark.go), [`modulestate.go`](../../internal/checksandalerts/modulestate.go), [`helper.go`](../../internal/checksandalerts/helper.go) | How a Starlark script runs and the primitives under their names for scripts, the state directory of a check, and the JSON helper for an executable that is a Bash script |
| Module commands | [`moduleusage.go`](../../internal/config/moduleusage.go), [`cmd/mmwatchdog/modules.go`](../../cmd/mmwatchdog/modules.go) | `mmwatchdog describe-module` and `mmwatchdog module-usage` |
| Runtime owner | [`daemon.go`](../../internal/daemon/daemon.go) | `Daemon`, startup and reload, the commit paths `record` and `control`, and the node lifecycle |
| Scheduler | [`scheduler.go`](../../internal/daemon/scheduler.go) | Due work, the per-check gate and the execution slots, `execute`, and acceptance of completed observations |
| What every check shares | [`check.go`](../../internal/checksandalerts/check.go), [`resources.go`](../../internal/checksandalerts/resources.go), [`processes.go`](../../internal/primitives/processes.go), [`systemd.go`](../../internal/primitives/systemd.go), [`mailserver.go`](../../internal/checksandalerts/mailserver.go), [`databaseserver.go`](../../internal/checksandalerts/databaseserver.go), [`operatingsystem.go`](../../internal/primitives/operatingsystem.go) | An observation and one run of a check, the decoding of a module's result, the CPU, memory, and load readings, the readings of the kernel's counters for a rate, the limits MMWatchdog applies to measurements, the processes of the node and of a systemd unit, the questions MMWatchdog asks systemd, the connection of the three checks of a mail server, the connection inputs of the two checks of a database server, and the family of the node's distribution |
| Events and alert text | [`event.go`](../../internal/checksandalerts/event.go), [`alerttext.go`](../../internal/checksandalerts/alerttext.go), [`evidence.go`](../../internal/checksandalerts/evidence.go) | An event and its kinds, the one place that writes an alert's title and body, and the explanation that an event keeps |
| Incident state | [`state.go`](../../internal/incidents/state.go) | Observations becoming incidents and events |
| Severity | [`config/severity.go`](../../internal/config/severity.go), [`incidents/severity.go`](../../internal/incidents/severity.go) | Alert severity: levels, each check's settings and their validation, and the destination filter (`config`); the raises of an incident's severity (`incidents`) |
| Recovery | [`incidents/recovery.go`](../../internal/incidents/recovery.go), [`daemon/recovery.go`](../../internal/daemon/recovery.go) | Recovery attempts and their policies (`incidents`); action eligibility, reservation, the restart or the command, and completion (`daemon`) |
| One cause, one alert | [`config/onecause.go`](../../internal/config/onecause.go), [`incidents/onecause.go`](../../internal/incidents/onecause.go), [`daemon/onecause.go`](../../internal/daemon/onecause.go) | The checks that a check names as its causes and their validation (`config`), and the time in which MMWatchdog keeps a check quiet (`incidents`, `daemon`) |
| Storage and delivery | [`store.go`](../../internal/store/store.go), [`incidentdestinations.go`](../../internal/store/incidentdestinations.go), [`notify.go`](../../internal/daemon/notify.go) | SQLite persistence, outbox ordering, the destinations that an incident's alerts went to, retention, and the delivery worker |
| Notices | [`store/notice.go`](../../internal/store/notice.go), [`daemon/notice.go`](../../internal/daemon/notice.go) | Notices such as `mmwatchdog send-notice`: their queue (`store`) and their delivery (`daemon`) |
| Local API | [`api.go`](../../internal/daemon/api.go) | Inspection, maintenance, diagnostic checks, and heartbeat intake |
| Measurement history | [`store/measurementhistory.go`](../../internal/store/measurementhistory.go), [`daemon/measurementhistory.go`](../../internal/daemon/measurementhistory.go) | The store of the readings on a node and on the hub, its time and size limits (`store`), and its pruning and what status and the fleet view say about it (`daemon`) |
| Fleet membership and reports | [`fleet/fleet.go`](../../internal/fleet/fleet.go), [`fleet/reporter.go`](../../internal/fleet/reporter.go), [`daemon/fleet.go`](../../internal/daemon/fleet.go), [`daemon/reporter.go`](../../internal/daemon/reporter.go) | Hub membership and report storage, and the node's report (`fleet`); the hub's HTTP handlers and the node's outbound reporting loop (`daemon`) |
| Fleet alerts and silent nodes | [`fleet/fleetalerts.go`](../../internal/fleet/fleetalerts.go), [`store/intake.go`](../../internal/store/intake.go), [`fleet/fleetwatch.go`](../../internal/fleet/fleetwatch.go), [`daemon/fleetalerts.go`](../../internal/daemon/fleetalerts.go), [`daemon/fleetwatch.go`](../../internal/daemon/fleetwatch.go) | The fleet's alert settings, the hub's alert intake and grouping, direct fallback, and the checks of silent nodes and hubs |
| One incident across the fleet | [`checksandalerts/fleetincidentkey.go`](../../internal/checksandalerts/fleetincidentkey.go), [`store/fleetincidentset.go`](../../internal/store/fleetincidentset.go), [`fleet/fleetincidentset.go`](../../internal/fleet/fleetincidentset.go), [`daemon/fleetincidentset.go`](../../internal/daemon/fleetincidentset.go) | The incident key (`checksandalerts`); the fleet incident set of each destination that keeps incidents, one closing alert per key, and what a joined node sent without the hub (`store`); the sets in reports (`fleet`); and applying reports (`daemon`) |
| MMHotline | [`checksandalerts/mmhotline.go`](../../internal/checksandalerts/mmhotline.go), [`store/mmhotlinequeue.go`](../../internal/store/mmhotlinequeue.go), [`fleet/mmhotline.go`](../../internal/fleet/mmhotline.go), [`daemon/mmhotline.go`](../../internal/daemon/mmhotline.go) | The connection to MMHotline, beside the destination module in `destination_mmhotline.go`: signing and receipts (`checksandalerts`); MMHotline alerts, severities, resolutions, and the `resolved-sos` of a key (`store`); what the MMWatchdog fleet hub reports and the check-ins (`fleet`, `daemon`) |

## Which goroutines run

The node monitoring process is one program. Its workers are goroutines, not
separate operating-system processes. A check whose module is a shell script or
a module that is not core starts a child process from inside its goroutine.

| Worker | Where it starts | Notes |
| --- | --- | --- |
| Scheduler tick | `Daemon.Run` in [`daemon.go`](../../internal/daemon/daemon.go) calls `Daemon.schedule` in [`scheduler.go`](../../internal/daemon/scheduler.go) | Under `Daemon.mu`, it picks the due checks and starts one `observe` goroutine for each |
| Node check worker | `Daemon.observe` in [`scheduler.go`](../../internal/daemon/scheduler.go) | Carries the check's epoch as a ticket and calls `execute`, which runs the module's code in a goroutine of its own |
| Node recovery worker | `Daemon.runCommand` and `Daemon.restart` in [`recovery.go`](../../internal/daemon/recovery.go) | Reserves an attempt through `control` before the action |
| Node notification worker | `Daemon.notifications` in [`notify.go`](../../internal/daemon/notify.go) | One delivery goroutine for each due delivery |
| Reporting loop and MMHotline check-ins | `Daemon.reports` and `Daemon.checkIns`, started in `Daemon.Run` | The reporting loop is the joined node's. The check-ins go to MMHotline destinations; a joined node sends none |
| Local API and heartbeat listeners | `Daemon.Run`, [`heartbeatsocket.go`](../../internal/daemon/heartbeatsocket.go) | The heartbeat socket has its own lock |

Check execution, systemd commands, webhook and MMHotline requests, mail-server
conversations, and destination module runs never hold `Daemon.mu`. The
[locks and gates](implementation-rules.md#one-lock) and
[execution limits](implementation-rules.md#execution-limits-the-gate-and-the-slots)
are in the implementation rules.

## A reading path for a first reading

1. Read [How MMWatchdog works](../how-it-works.md) up to its section on modules, so that the
   names of the parts are familiar.
2. Follow `Daemon` in `daemon.go`, then the scheduler in `scheduler.go`,
   `CheckState.Observe` in `internal/incidents/state.go`, `Store.Save` in
   `internal/store/store.go`, and the node notification worker in `notify.go`.
3. Read the recovery files and then the fleet files from the table above.
4. Read the [implementation rules](implementation-rules.md) for the constraints
   that the code keeps.

Tests that explain behavior, beside the code they test:

- [state tests](../../internal/incidents/state_test.go) cover the failing time
  and the two incident tracks;
- [daemon tests](../../internal/daemon/daemon_test.go) cover reload,
  maintenance, and action races;
- [store tests](../../internal/store/store_test.go) cover atomic persistence and
  restart;
- [fleet alert tests](../../internal/daemon/fleetalerts_test.go) cover hub
  delivery and fallback.

Each package keeps its Go tests beside its code, and the tests of
`internal/daemon` test how the packages work together. The tests supplement the
[acceptance contracts](acceptance.md) and do not define product scope.
The [testing guide](testing.md) says which tests run where, and the
[validation record](validation.md) holds the current platform results.
