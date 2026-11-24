# MMWatchdog implementation rules

The daemon has a small set of entry points that change state around one
runtime snapshot. The MMWatchdog implementation rules assign each kind of
job one path. A reviewer can check each rule once instead of tracing every combination of reload, maintenance, restart,
and storage failure.

[How MMWatchdog works](../how-it-works.md) explains the components and workflows.
Reviews check every changed path against the MMWatchdog implementation
rules.

## Packages

MMWatchdog's code is in seven packages under `internal/`. The layers run
from lowest to highest: `primitives`, `checksandalerts`, `config`,
`incidents`, `store`, `fleet`, and `daemon`
([code map](code-map.md)). A package imports
only lower layers. The Go compiler refuses an import cycle, so the layer
restriction needs no review:

- New code goes in the lowest package that can hold the code.
  A check module or a destination module belongs in `checksandalerts` and
  uses `primitives` to read the node. A check or destination module cannot
  reach a check's state, the store, or the fleet.
- A feature that spans layers keeps its file name in each package that
  holds a part of it, such as `onecause.go` in `config`, `incidents`, and
  `daemon`.
- A test goes beside the tested code, unless the test needs the node
  monitoring process. A test of a package below `daemon` is in that
  package's external test package, such as `package store_test`.
  External tests can build inputs with `config` and `internal/testhelpers`,
  which holds what tests of several packages share.
  A test that needs the node monitoring process is in
  `daemon`, even when the tested code belongs to a lower package.
- A package exports what the packages above it and the tests use. A name
  used only by tests is exported too. Examples include `Store.DB`,
  `MeasurementHistory.DB`, and `Hub.DB`, the database handles used to inject
  storage failures. Code outside `store` and `fleet` does not
  use the database handles, although the compiler permits access. The rules
  for the node's database ([two commit paths](#two-commit-paths)) and for
  the hub's database ([fleet store commits](#fleet-store-commits)) hold by
  review.

## Go dependencies

Beside Go's standard library, MMWatchdog builds with the 11 Go modules below.
`go.mod` pins the Go toolchain and the version of each module. MMWatchdog's code
imports packages of five of the 11 Go modules: `modernc.org/sqlite`,
`go.starlark.net`, `go.yaml.in/yaml/v3`, `golang.org/x/net`, and
`golang.org/x/sys`. The six modules marked `// indirect` in `go.mod` are
requirements of the five directly imported modules.

| Module | What MMWatchdog uses each module for | Go files kept | Lines kept |
| --- | --- | --- | --- |
| `modernc.org/sqlite` | SQLite, translated from C into Go, so MMWatchdog needs no C compiler and no C library. The node monitoring process uses `modernc.org/sqlite` for `state.db` and `measurement_history.db`. On a fleet hub, the process also uses `modernc.org/sqlite` for `fleet.db` and `measurement_history_of_nodes.db`. The [`sqlite_database`](../reference/modules/sqlite_database.md) check module uses `modernc.org/sqlite` to open the database being checked. | 139 of 597 | 396,236 of 1,535,424 |
| `modernc.org/libc` | The C library functions that the translated SQLite calls, translated into Go as well. | 69 of 1,383 | 403,859 of 2,178,784 |
| `modernc.org/mathutil`, `modernc.org/memory` | The integer functions and the memory allocator of `modernc.org/libc`. | 4 of 26, 6 of 12 | 9,087 of 18,715, 847 of 2,114 |
| `github.com/google/uuid` | The UUID functions of `modernc.org/libc`. | 11 of 21 | 1,021 of 2,759 |
| `github.com/mattn/go-isatty`, `github.com/ncruces/go-strftime` | The terminal test and the time formatting of `modernc.org/libc`, on macOS only. | 1 of 10, 3 of 11 | 20 of 322, 694 of 1,553 |
| `go.starlark.net` | The Starlark interpreter, which runs the official extra modules and the [Starlark scripts](../reference/writing-modules.md#the-starlark-language) of modules of the project's own. | 24 of 54 | 16,349 of 23,666 |
| `go.yaml.in/yaml/v3` | The YAML parser of the configuration files and of the module descriptions. | 13 of 18 | 11,346 of 17,456 |
| `golang.org/x/net` | Only `dns/dnsmessage`: the [`dns`](../reference/modules/dns.md) check module writes its question and reads the DNS server's answer with it. | 2 of 742 | 3,066 of 193,184 |
| `golang.org/x/sys` | Only `unix`: the system calls that Go's standard library lacks, for [`node_clock`](../reference/modules/node_clock.md), for the read-only flag of a filesystem, and for the terminal test of `mmwatchdog describe-module`. | 48 of 487 | 30,245 of 253,423 |

The node monitoring process runs as root on every node, and on a fleet hub
it receives the node status reports of every joined node. Every line of
dependency code that MMWatchdog builds runs in the node monitoring process.
So MMWatchdog keeps each dependency as a pruned copy in the MMWatchdog
repository:

- **Where a copy lives.** `third_party/<module path>/` holds the copy of
  one module, for example `third_party/modernc.org/sqlite/`. A `replace`
  line in `go.mod` points the module at `third_party/<module path>/`.
  The import paths, and the version in the module's `require` line, stay
  upstream's, and there is no `go.sum`.
- **Why.** A build downloads no dependency code, so nobody can change the
  code that MMWatchdog builds by publishing a new version or by changing what a
  module proxy serves. Dependency code changes only through a commit in
  the MMWatchdog repository, which a reviewer reads like MMWatchdog's own
  code.
- **What a copy keeps.** A pruned copy keeps packages used by MMWatchdog's
  programs and tests. Each package keeps only whole files that MMWatchdog
  needs and a supported build compiles. Supported builds: Linux on AMD64
  or ARM64, or macOS on ARM64, with cgo or without, and with the race
  detector or without.
  Retained files contain functions MMWatchdog reaches, assembly, or
  declarations needed by retained code. The declarations include assembly
  function declarations, types, constants, variables, and methods.

  `deadcode` from golang.org/x/tools v0.52.0 measured which functions
  MMWatchdog reaches. Go's type checker then identified declarations
  required by the retained code. Files containing required declarations
  were added until another type check required no more files. Upstream
  tests, examples, tools, and the files of other platforms are cut. Of the
  4,227,400 lines of Go in the 11 upstream modules, the copies keep 872,770
  in 320 of 3,361 files. Two further modules, `github.com/dustin/go-humanize` and
  `github.com/remyoudompheng/bigfft`, are gone: only files that the copies
  cut imported them.
- **Kept files stay identical to upstream,** byte for byte, except for
  the copy's `go.mod`. The `go.mod` loses
  requirements that no kept package needs. Preserving upstream bytes lets
  an upstream fix apply to a kept file unchanged. To preserve upstream
  bytes, nothing reformats the copies: run `gofmt` on MMWatchdog's own
  directories, never on `third_party/`. Each copy is a Go module of its own.
  So `./...` in MMWatchdog's `go build`, `go vet`, and `go test` commands
  excludes the dependency copies.
- **The record.** Each copy's `PRUNED_COPY_RECORD.md` names the upstream
  version and its module hash, the kept packages, the number of kept files
  and lines, every file that differs from upstream, and the fixes applied
  since. The `Module:` line in `PRUNED_COPY_RECORD.md` names the module in
  backticks. Under `## Applied fixes`, write one bullet per fix, starting
  with the advisory ID, for example
  `- GO-2026-1234: Apply upstream commit <commit> to fix
  <fault>.` Write `None.` when no fixes have been applied.

To compare a copy with upstream, here the copy of `modernc.org/sqlite`,
from the repository root:

```sh
go mod download modernc.org/sqlite@v1.59.0
upstream=$(go list -m -f '{{.Dir}}' modernc.org/sqlite@v1.59.0)
diff -r "$upstream" third_party/modernc.org/sqlite | grep -v "^Only in $upstream"
```

`go mod download` downloads the upstream version into the Go module cache
and checks it against the Go checksum database; `go list` then names its
directory there. The output of `diff` lists only
`third_party/modernc.org/sqlite/PRUNED_COPY_RECORD.md` and the files named
under "Changed files" in `third_party/modernc.org/sqlite/PRUNED_COPY_RECORD.md`.

When MMWatchdog needs code that a copy cut, copy whole files from the
upstream version named in the copy's `PRUNED_COPY_RECORD.md`.
Add any new package to the copy's record.

### When a copy or the toolchain changes

A dependency's copy, and the Go toolchain that `go.mod` pins, change for
only two reasons, and each must be shown, not assumed:

1. **A vulnerability that MMWatchdog is exposed to,** with three proofs:
   - **Reached:** `govulncheck` shows a call path from MMWatchdog's code to the
     vulnerable function.
   - **Open to an attacker:** the input that triggers the vulnerability
     comes from outside the user's control. Examples include a node status
     report received by a fleet hub and a monitored service's reply to a
     check. Replies from an alert destination's service, including
     MMHotline, also count. The user's own check group files do not count.
     Through check group files, the user already runs commands as root.
   - **Reproduced:** a test fails before the fix and passes after it. The
     test stays in MMWatchdog's tests.
2. **A high-value fault in real use,** shown by a failing test.

These are not reasons to change a copy or the toolchain: a newer upstream
version, an advisory for code that the copy cut or that MMWatchdog does not call,
a scanner report without a reproduction, speed, cleanup, or staying
current. MMWatchdog's dependencies stay stable and boring on purpose.

Each change is one commit. The change's commit names the advisory or the
fault and holds the test. The change's commit also names the upstream
commit being applied. The copy's record lists the change under
"Applied fixes", with the advisory's ID.

When a vulnerability cannot be reproduced on demand, such as a timing leak,
the first two proofs go to the project's maintainer, who decides.

`govulncheck` cannot see the copies: it skips a module that a `replace`
line points at a directory. So the check runs on a temporary copy of the
MMWatchdog repository whose `go.mod` has no `replace` lines. The MMWatchdog
code in the temporary copy is unchanged from the checkout being checked
and calls the upstream dependency versions. `govulncheck` finds the call
paths present in the pruned copies, because MMWatchdog calls identical
functions in the upstream versions and in the pruned copies, and the copies
cut only code that MMWatchdog does not reach.
[`scripts/check-dependency-vulnerabilities.py`](../../scripts/check-dependency-vulnerabilities.py),
the same script as MMX's, creates and removes the temporary MMWatchdog
repository copy with no `replace` lines. The dependency-vulnerability
script installs govulncheck v1.9.0 and reads the JSON findings.
Installation and analysis use the repository's resolved Go toolchain.

The check fails only for a symbol-level finding: a call path to a
vulnerable function. A dependency finding fails when the advisory ID is
absent from `## Applied fixes` in the vulnerable module's
`PRUNED_COPY_RECORD.md`. An ID in another module's record does not count.
Reached Go standard library and toolchain advisories always fail the
check, because they have no copy record. A finding starts the investigation
above; it does not by itself justify changing a copy or the toolchain.

Exit status 1 means a reached advisory lacks a matching ID in the
vulnerable module's `## Applied fixes` section, or concerns Go's standard
library or toolchain.
Exit status 2 means the check could not run.

The static analysis step of the
[release checklist](design/README.md#release-checklist) runs
`scripts/check-dependency-vulnerabilities.py`.
The [weekly check](design/README.md#release-checklist),
`.github/workflows/dependency-vulnerabilities.yml`, also runs
`scripts/check-dependency-vulnerabilities.py` every Monday.

## Entry points

State changes enter through these paths and no others:

| Entry point | Kind |
| --- | --- |
| Daemon start (load and reconcile) | record |
| Scheduler tick (missed schedules, heartbeat and fleet deadlines, maintenance expiry, raises, reminders) | record |
| Observation completion | record |
| Restart or recovery command result | record |
| Fleet receipt (a report on the hub, a successful report on the hub-outage alerter node) | record, committed by the next tick |
| Fleet membership on the hub (a join, replacement, or forget) | record, committed by the next tick |
| Hub-outage alerter role on a joined node | record |
| Reload activation | control |
| Maintenance command | control |
| Recovery attempt reset command | control |
| Heartbeat receipt | control |
| Restart or recovery command reservation | control |
| Alert settings activation on a joined node | control |
| Delivery acknowledgment and retention, including a receiver's permanent refusal | outbox |
| Fallback on a joined node | outbox |
| Fleet incident set step on the hub | outbox |

## One lock

`Daemon.mu` guards all in-memory runtime state.
State commits happen while holding `Daemon.mu`.
Check execution, systemd commands, and webhook and MMHotline requests
never hold `Daemon.mu`. Mail-server conversations and destination module
runs never hold `Daemon.mu`.

A goroutine that finishes outside `Daemon.mu` re-enters through the
appropriate entry point and revalidates the goroutine's ticket.

The MMHotline connection has a small lock for the clock offset, latest
signed answer, and latest check-in. Code holding `Daemon.mu` may take the
MMHotline connection lock. Code holding the MMHotline connection lock must
not take `Daemon.mu`.
No request holds the MMHotline connection lock. Check-ins change no daemon
state, so check-ins commit nothing.

The hub's fleet store has its own lock, `Hub.Mu`, and its own database. `Hub.Mu`
and `Daemon.mu` are never held together ([fleet store
commits](#fleet-store-commits)). The heartbeat socket has a lock of its own
too.

A node's state and where it lives:

| State | Owner and persistence |
| --- | --- |
| Desired check groups and destinations | YAML and secret files; the daemon activates a validated configuration |
| Checks with their failing times, open incidents, evidence, recovery attempts, maintenance, heartbeat receipts, boot identity | The snapshot in memory, committed to `state.db` |
| Events, pending deliveries, delivery bookkeeping, and the hub's alert intake | `state.db`; delivery is independent of whether a check is running |
| Hub identity, members, credential hashes, latest reports, and received event history | The hub's `fleet.db` |
| The readings of every scheduled run of a check that measures | `measurement_history.db`, written when a run is accepted and never part of a state commit |
| The readings that joined nodes sent, and how far the hub has received each node's readings | The hub's `measurement_history_of_nodes.db` |
| A joined node's hub credential and copied fleet settings | `hub-credential.json` and `fleet-settings-from-the-hub.json` in the joined node's state directory |
| The fleet incident set of each destination that keeps incidents; a joined node's incidents sent without the hub | `state.db` of the hub or the standalone node; the sent-without-the-hub list in the joined node's `state.db` |
| Running workers, schedules, epochs, gates, and slots | Memory; rebuilt on daemon start |

At daemon start `Interrupt` ends every failing time whose incident has not
opened: a restart is not evidence of recovery. Open incidents with their
failing times, recovery attempts, maintenance, heartbeat deadlines, and
queued notifications survive. After a hub restarts, the hub grants nodes a
fresh reporting window and keeps open silence incidents.

## Two commit paths

`Daemon.record` and `Daemon.control` are the only callers of `Store.Save`.
Each entry point commits once, except that fleet receipts and hub
membership changes leave pending transitions for the scheduler's next tick.

`Store.Save` keeps recovery attempts and maintenance in one small row and
each check in a separate row. `Store.Save` writes only check rows whose
content changed since the last commit, so the cost follows the change
rather than all retained state.

A hub has a fleet check per node, so Save does not even encode an
unchanged fleet check. State methods give a check a new in-memory revision
when persisted fields change.
Save rewrites a fleet check only when the revision differs from the last
written revision.

A commit enforces the event history and outbox size limits only when the
commit adds events. Age expiry runs in the retention step
(`Daemon.retention`), once a minute and right after each reload.
State commits and retention read the node's storage settings
from `Store.Limits`, which a reload replaces.

The database records the database's format version. The daemon refuses to
start on another format rather than load only part of the database.

- **Record** applies an observed fact to live state immediately, then commits
  the fact with any pending transitions. If the write fails, the fact stays
  in memory and the fact's transitions stay pending until a later commit
  succeeds. Pending transitions are bounded, with a visible gap.
  Monitoring keeps working while storage is down. Meanwhile the
  notifications worker sends each pending alert straight from memory
  (`deliverFromMemory`), routed as `Store.Save` would queue it
  (`Store.DeliveriesFromMemory`), and remembers in `Daemon.fromMemory`,
  bounded by the pending alerts, which destination accepted which alert.
  The later commit queues the same alerts, which go out again: a duplicate
  is better than a dropped alert, and the record of what went out cannot be
  saved.
- **Control** builds its change on `Snapshot.Clone()`, commits the clone
  together with pending transitions, and swaps it in only after the commit
  succeeds. Runtime changes that follow a control, such as advancing an
  epoch, happen only after the control's
  snapshot commit succeeds. A failed control changes nothing: no
  maintenance, no reset, no acknowledged heartbeat, no reserved restart or
  recovery command, and no activated configuration.

## One storage-health registry

Each storage operation reports the result under a distinct name in
`Daemon.storage`: state commit, outbox read, retention, and delivery
bookkeeping per destination.
Success clears only the registry entry for the storage operation that
succeeded. Reload also clears a destination's delivery bookkeeping entry
when reload removes the destination.

Status reads the registry and never writes it. The registry logs a problem
when the problem starts and when the problem's text changes, not at every
failed attempt. MMWatchdog keeps running with a storage problem, such as
an unusable measurement history. Status shows a storage problem for as
long as the problem lasts.

## Measurement history

The readings of the measurements are no part of a check's state, so they go
through neither commit path. Measurement readings have a separate store,
`MeasurementHistory` in `measurementhistory.go`. `MeasurementHistory` serves
`measurement_history.db` and `measurement_history_of_nodes.db`:

| File | Holds | Owner |
| --- | --- | --- |
| `measurement_history.db` | The node's own readings | `Daemon.history` |
| `measurement_history_of_nodes.db` | On a hub, the readings of the joined nodes | `Hub.Readings` |

- **One row for each run.** A row of the table `readings` is one
  `RunReadings`: the time of the run, the check group, the check, and the
  run's measurements as JSON text. Each `MeasurementReading` stores its
  value and unit, `fail_when_measurement_above` and
  `fail_when_measurement_below` when present.
  The measurement limits are history fields, never fields of a module
  result. The row's `id` is the row's sequence number. A sequence number is
  never reused during the database file's lifetime (`AUTOINCREMENT`). Each
  database file has a random ID, as the event history has. A node's own
  rows have an empty node name; a hub's rows have the name of their node,
  and an index by node that leaves out rows without one.
- **Two times.** `at` is the time of the run as its node measured it, and
  never changes. `kept_from` is the time from which the reading's age
  counts: `at`, or on a hub the time of the receipt when that is earlier.
  `kept_from` has the index. Removals to enforce the retention duration or
  the size limit follow `kept_from`, oldest first. `historyReach` holds the
  lowest and highest retained `kept_from` values, as do the status fields.
- **Lock order.** `MeasurementHistory` has a separate lock. Code that holds
  `Daemon.mu` may take the history lock. Code holding the history lock must
  never take `Daemon.mu`. While holding the history lock, the store takes
  no additional lock and calls nothing.
  `Hub.Mu` is never held while the store's lock is taken, except when the
  hub starts (`Hub.load`): every
  report needs `Hub.Mu`, so nothing that holds it may wait for the history.
  The fleet view reads the history's reach before it takes `Hub.Mu`.
  `Hub.forgetting` orders `mmwatchdog fleet forget` and the saving of readings: it is
  taken before `Hub.Mu` and before the store's lock, and nothing else is
  taken before it.
- **A removal of many readings is many steps.** Checks, alerts, and reports
  must never wait for the history. A removal of many readings therefore
  releases the history lock between steps. `prune`, `ForgetNode`, and
  `TrimStep` remove at most `HistoryRemovalRows` readings per step. Each
  removal step takes the history lock separately. `Hub.Prune` and
  `Hub.Forget` perform the history removals without holding `Hub.Mu`.

  Forgetting a node first forgets how far the hub had received readings
  (`ForgetReceived`). Then
  `RemoveReadingsOfNode` removes only readings present when `ForgetReceived`
  ran, in steps. A node that joins again between removal steps therefore
  keeps the new readings.
- **MMWatchdog runs without a history that it cannot use.** `OpenMeasurementHistory`
  always returns a history. When `measurement_history.db` or
  `measurement_history_of_nodes.db` cannot be opened, the returned history
  has no database. A history without a database stores nothing, and every
  write returns the reason the database could not open. The caller reports
  the reason as a storage problem at startup, so the problem is visible
  immediately. A history without a database still has an ID, because a
  report needs a measurement history ID.
- **Statements read no whole table.** Every statement that runs while MMWatchdog
  works finds its rows through the sequence number or an index, and is a
  constant beside the others (`HistoryReachQuery` and the ones after it).
  `TestMeasurementHistoryStatementsReadNoWholeTable` checks the query plans
  of `HistoryReachQuery` and the SQL statement constants that follow.
- **Where readings enter on a node.** `Daemon.accept` applies a scheduled
  run whose ticket matched, and calls `keepReadings` for a run that has
  measurements. `mmwatchdog check-now` goes through `execute` alone and a
  missed run has no measurements, so neither a diagnostic run nor a missed
  run stores readings. Maintenance and quiet time hold alerts, not readings.
- **Where readings leave a node.** The reporter calls `addReadings` outside
  `Daemon.mu`, after building the report and measuring the node.
  `addReadings` encodes the report, then uses `ReadingsRoom` to calculate the
  readings byte budget:

  1. Allow `ReportReadingsBytes` per `reportReadingsEvery` in the interval
     between reports. A node therefore sends readings at a constant byte rate,
     regardless of report frequency.
  2. Raise the allowance to at least `ReportReadingsFloor`, enough for the
     largest run the hub accepts.
  3. Cap the allowance at `ReportLimit` minus the encoded report size and
     `ReportSpareBytes`, so readings cannot make the hub refuse the report.

  `rep.readingsAfter` is the sequence number
  from the hub's last reply and -1 after a daemon start, a join, and a
  refused credential, so the first report carries no readings. The hub's own
  report carries no readings, just as a joined node's first report carries
  no readings.
- **Where readings enter a hub.** `Hub.Accept` saves the report in
  `fleet.db` and saves no readings. The handler of a joined node's report
  then gives the report to the silence check (`receipt`) and only after that
  calls `Hub.SaveReadings`, which calls `AddFromNode` without `Hub.Mu`. So a
  node counts as reporting before its readings are saved, and a busy history
  holds up no other node's report. `Hub.Mu` was free in between, so the node
  may have been forgotten and another node may have joined under its name.
  `SaveReadings` therefore saves only while the node's entry still names the
  daemon that sent the report (`StartID`), and answers -1 otherwise.
  `SaveReadings` holds `Hub.forgetting` for reading from the `StartID` check
  until the readings are saved. `Hub.Forget` holds `Hub.forgetting` for
  writing while removing the entry and calling `ForgetReceived`.
  Readings from a report accepted before a forget are saved before the
  forget and removed by the forget, or never saved. The hub's own report
  goes to `Hub.Accept` alone, so the hub copies
  none of its own readings.
  `AddFromNode` writes the readings and the table `received`, which
  says for each node which measurement history its readings come from and
  the sequence number of the last one saved, in the same transaction,
  using `readingsWriteBytes` as the batching target for each transaction.

  Readings count only when the report's `readings_after_sequence_number`
  equals the saved sequence number. When the measurement history ID changes,
  the hub starts the saved sequence number for the new history at 0.
  A report that is sent again therefore adds no reading twice. A reading
  keeps `at`, and the reading's `kept_from` is `at` or `now`, whichever is
  earlier.

  A failure to save readings through `AddFromNode` is reported as a storage
  problem and does not reject the node status report. The reply carries
  the last successfully saved sequence number.
  The report that the hub keeps for the node holds neither its events nor
  its readings (`Hub.Accept`). A hub whose
  `measurement_history_of_nodes.db` cannot be used returns -1.
  A node has -1 before the first reply, so no node sends readings to a hub
  with an unusable measurement history.
- **A node never falls behind the hub's number.** After each accepted report
  the reporter calls `ContinueAfter` with the hub's `readings_cursor`. A
  history that has given out only lower sequence numbers, as after a restore
  from an older copy, moves SQLite's counter to the hub's `readings_cursor`.
  Advancing the counter lets the hub take the next reading.
- **The size limit is the size of the file.** `maxPages` is the limit in
  pages. Before every write, `makeRoom` compares the pages in use
  (`page_count` less `freelist_count`) with `maxPages`. `makeRoom` removes
  the oldest readings by time until the write fits with `historySparePages`
  to spare. SQLite's `max_page_count` enforces the page limit if `makeRoom`
  does not free enough space. `measurement_history.db` and
  `measurement_history_of_nodes.db` use SQLite's incremental auto-vacuum
  mode. A new database must select incremental auto-vacuum before any
  writes, so `OpenDatabaseFile` accepts initialization statements as `first`.

  Incremental auto-vacuum lets a measurement history return free pages when
  the size limit is lowered. A measurement history is never refused at
  startup because of size. In contrast, `OpenDatabase` refuses oversized
  `state.db` and `fleet.db` files. `SetSizeLimit` only states the limit.

  While a measurement-history file exceeds `maxPages` (`shrinking`),
  each `TrimStep` removes some oldest readings or returns up to
  `HistoryVacuumPages` free pages and applies the write-ahead log. During
  shrinking, a write needs room only within the current database size.
  `Daemon.trimMeasurementHistories` runs a step
  for each file at every tick of the notification loop; a step costs nothing
  while a file is within its limit. The write-ahead log beside the file is
  not counted in the limit; SQLite keeps it at about 1 MiB.
- **What "cut short" means.** `removeOldest`, the only removal for size,
  writes the time of the newest reading that it removed to
  `history.removed_by_size_limit_until`, in the transaction that removes it.
  `removeOldest` saves the size limit in force in `removed_under_max_pages`,
  beside `history.removed_by_size_limit_until`. `reach` reports a cut-short
  history while `history.removed_by_size_limit_until` falls within the
  current retention period. The current size limit must also be no higher
  than the limit that removed the reading. A raised limit has removed
  nothing, and the cut-short status line names the current size limit.

  `prune`, the removal by age, never records a new size-based removal in
  `history.removed_by_size_limit_until` or `removed_under_max_pages`.
  `prune` clears `history.removed_by_size_limit_until` and
  `removed_under_max_pages` once `history.removed_by_size_limit_until` is
  outside the retention period. Clearing the removal record prevents a
  later increase in retention duration from making an old removal count again.
- **Settings.** Each `Daemon.retention` step reads
  the retention duration from configuration. `Daemon.retention` runs
  `Daemon.pruneMeasurementHistory` and, on a hub, `Hub.Prune` for the hub's
  file.

  Retention runs at daemon start, once a minute, and right after each
  reload. A reload requests retention from the notifications worker through
  `runRetentionNow`. A reload calls `SetSizeLimit` when a size changed.
- **Forgetting.** `Hub.Forget` removes a node's readings and its row in
  `received` after `fleet.db` has committed the forget. Hub start removes the
  readings of every node that `fleet.db` does not list. Startup cleanup
  therefore removes readings left when the hub stopped after committing
  the forget in `fleet.db` but before removing readings from
  `measurement_history_of_nodes.db`. Readings that startup cleanup cannot
  remove are a storage problem and never stop the hub from starting.
- **Storage operations.** A node reports `measurement history write` for a
  write, and for a file that it cannot use, and `measurement history
  retention` for every removal, in `Daemon.storage`. A hub reports
  `measurement history of nodes write` and `measurement history of nodes
  retention` in its fleet store's registry. A reload reports nothing: it
  only states a limit, and the steps that apply it report what fails.
- **One sentence.** `HistoryCutShortSentence` writes the line for
  `mmwatchdog status` and `mmwatchdog fleet`. `MeasurementHistoryStatus` and
  `MeasurementHistoryOfNodesStatus` carry the facts under the names of the
  corresponding settings.
- **CLI inspection has its own read-only connections.** `ReadMeasurementHistory`
  opens an existing file with SQLite's `mode=ro` and `query_only`, reads an
  inclusive time span, and closes its cursor before printing.
  `ReadMeasurementHistory` takes no daemon lock and no writer lock, so
  inspection works with a stopped daemon or a live WAL writer.
  `ReadReadingNodes` reads saved membership from `fleet.db` in the way that
  `ReadMeasurementHistory` reads history. `ReadReadingNodes` never opens a
  hub or loads the hub's identity.
- **Readings from another machine.** `NodeStatusReport.Validate` checks a
  report's readings with `RunReadings.Validate`. Validation checks names,
  a time that
  `measurement_history.db` and `measurement_history_of_nodes.db` can store
  (nanoseconds since 1970, so from 1970 to 2262), 1 to 128 measurements for
  each run, and units of plain text. Reading sequence numbers must increase
  strictly, starting after
  the report's `readings_after_sequence_number`.

## Tickets for in-flight work

Every scheduled execution carries the check's epoch, a version counter, as
a ticket. The epoch advances whenever earlier evidence stops counting:

- The check's definition changes.
- The check's check group enters or leaves maintenance.
- A restart command completes for a systemd unit named by the check's check
  group, in `restart_systemd_unit` or a unit check.
- A recovery command completes in the check's check group or in a check
  group that names a unit also named by the check's check group.
- The daemon starts.

A result is discarded when the result's ticket differs from the check's
current epoch. An executed check needs a fresh observation until a result
is accepted in the check's current epoch.
Execution arguments are copied under the daemon lock before work uses them
outside it, including when an unchanged reload replaces the definition.

Before reading check state or executing, a scheduled worker validates the
check's runtime identity and epoch under the daemon lock.
Stale work releases the running slot without executing. Work already executing
revalidates runtime identity and epoch before applying a result.

A check's definition holds only the settings that affect how it is
evaluated, so an unrelated edit never changes it. A heartbeat's definition is
its `max_time_between_heartbeats` and `expected_bearer_token_file`.
Timing does not apply to heartbeats.

Heartbeats have no executions; their receipt and deadline are durable
evidence, so they never need a fresh observation. Because no execution will
reconcile a heartbeat after maintenance, maintenance end applies its latest
observation again: a suppressed failure is announced, and an announced
failure that recovered during maintenance is closed.

A check group is in maintenance while the check group's own window or the
whole-node window (`every_check_group`) exists. Starting, changing, or
ending a check group window advances the epochs of checks in the covered
check group. Starting, changing, or ending the whole-node window advances
the epochs of checks in every check group.

The maintenance command, or the scheduler tick that finds an expired
window, removes the window and advances the covered checks' epochs
together. Suppression and tickets therefore change at one moment.
A heartbeat is reconciled at a window's end only once no other window
covers the heartbeat's check group.

`maintenance_on` and `maintenance_off` events notify unless
`WithoutMaintenanceOnAndOffAlerts` is set.
`maintenance_off` states whether maintenance was turned off, expired, or
ended because the window's check group was removed. The maintenance command
commits the maintenance events with the maintenance change as a control.
The scheduler tick that finds an expired window commits the window's end
as a record. The end event is queued before the transitions that
reconciliation at the window's end produces.

One maintenance command names one or more check groups, or
`every_check_group` alone. Each named check group gets its own window, all
with the same end, and the command sends one event: `MaintenanceEvent` is the
only place a maintenance event is built, and it names the check groups in
`check_groups`. `Snapshot.EndMaintenance` ends the windows it is given and
groups windows with matching end, reason, user, and
`WithoutMaintenanceOnAndOffAlerts` into one `maintenance_off`
event. The windows of one command therefore end with one event whether an
operator turns the windows off or the windows expire.
A window has no maximum duration. The command requires a positive
duration, with the window ending as the operator specified.

A restart carries the unit's policy as a ticket, and a recovery command
carries the check group's policy. Reload keeps an unchanged policy.
Only a reload that changes the policy invalidates an action worker's ticket
before the worker reserves an attempt. `recoveryDue` rechecks maintenance
and recovery attempt limits.

### Execution limits: the gate and the slots

Three things limit the runs of checks. Each lives in `internal/daemon`:

- **`Daemon.gates` holds one gate for each check,** a channel with room for
  one run (`Daemon.execute`). A scheduled run and a run of `mmwatchdog
  check-now` both take the check's gate, so no two runs of a check overlap and
  the check's `CheckRun.Memory` has one user at a time. A run waits for the
  gate without a time limit of the run's own: the check's time limit starts
  after the gate and the slot are taken.
- **`Daemon.slots` holds the execution slots,** a channel with room for
  `max_concurrent_checks` runs. A run takes a slot after its gate, and
  scheduled and diagnostic runs share the slots. The channel's size is fixed
  when the daemon starts, so a reload that changes `max_concurrent_checks`
  is refused: that setting needs a restart of MMWatchdog.
- **`Daemon.running` counts the scheduled runs that have started,** under
  `Daemon.mu`. The scheduler starts no run while `Daemon.running` reaches
  `max_concurrent_checks`, or while the check's own `running` flag is set.
  `Daemon.observe` gives back its count and flag when it ends, also when its
  ticket is stale and it ends without executing.

A check that is due but cannot start for lack of capacity gets no backlog.
When a whole interval has passed since the check's last observation and since
the check was due, the scheduler records one unknown observation
(`scheduled observation missed: execution capacity unavailable`) and waits
for the next interval. The scheduler records no such observation for a check
in `Daemon.runsInProgress`: that run brings its own observation.

`execute` takes, in this order: the check's gate, a look at
`runsInProgress`, a slot, and then the lock for the step that rechecks the
check's definition and puts the run into `runsInProgress`. Waiting for the
gate or a slot ends when the caller's context ends, with an unknown
observation. A run that did not end gives back its gate and slot when
`waitForRun` stops waiting ([a run that does not end](#a-run-that-does-not-end)).

The epoch is one counter for the whole daemon (`Daemon.epoch`), not one
counter for each check. Each time earlier evidence stops counting,
`Daemon.epoch` goes up by one and the new number becomes the epoch of the
affected check (`setDefinition` for a new or changed definition,
`requireFresh` for a maintenance boundary or a finished action). A check's
`checkRuntime.fresh` is false from that moment until `Daemon.accept` takes a
result in the new epoch. `requireFresh` also makes the check due at once.
A reload with an unchanged definition keeps the check's epoch, schedule, and
memory and applies only a new interval.

### A run that does not end

Check-module code must observe the run's context cancellation. At the time
limit, the module must end subprocesses, close connections, and return
promptly. Code waiting for a blocked kernel call cannot promptly stop and
return: the kernel holds a call on a file of another machine's filesystem
whose server is gone, without end, and nothing takes the call back. `Daemon.execute` is the single place
that handles runs remaining blocked after cancellation, for every module:

- **The module's code runs in a goroutine of its own,** and `execute` waits
  for it until the time limit and `timeForARunToEndAfterItsTimeLimit`
  longer (`waitForRun`). A run that ends before the time limit or during
  the additional `timeForARunToEndAfterItsTimeLimit` is accepted normally,
  regardless of the run's result.
- **A run that has not ended by then gives back what it holds:** the
  check's gate and the run's place among `max_concurrent_checks`.
  The run's observation is `runThatDoesNotEnd`, which is unknown.
- **`Daemon.runsInProgress` holds, by check, the run whose module has not
  returned.** `execute` starts no run of a check while an entry for the
  check remains in `Daemon.runsInProgress`. `execute` answers with
  `noRunBeforeTheEarlierOneEnds` at once, to the scheduler and to
  `mmwatchdog check-now` alike. No two runs therefore use a check's
  module memory concurrently. A check that
  cannot end keeps one goroutine waiting, and not one more at each interval.
- **The scheduler reports no missed observation for a check that is in
  `runsInProgress`.** A run that is under way brings its own observation,
  at the latest when `waitForRun` ends, so the first observation of a run
  that does not end says what happened, also for a check that runs more
  often than MMWatchdog waits for one run.
- **A panic in the module's code is an unknown observation** (`runCheck`),
  whose message names the panic on one line; the journal gets the stack.
  A destination module's panic is a failed delivery attempt (`send`). A
  panic that ended the process would end, at each start, every failing
  time whose incident has not opened (`Interrupt`), so a bug that panics
  at every run would keep every new incident from opening.
- **A run executes against a private copy of persisted module state.**
  `execute` hands the state that the run saved to the caller only when the
  run ended in time.
  When a run that MMWatchdog stopped waiting for ends, its observation and its
  newly saved state are dropped. The abandoned run's temporary module
  memory is cleared, as after a start of MMWatchdog,
  which every module must handle anyway.
- **A changed definition is a new check,** with a new `checkRuntime` and a
  memory of its own, so `setDefinition` takes the old run out of
  `runsInProgress`. The old goroutine removes only its own entry when it
  ends. A run can wait for its turn while a reload changes its check, for
  example behind a run of `mmwatchdog check-now`. So `execute` compares the
  definition that it was given with the one that the daemon has, in the
  locked step that puts the run into `runsInProgress`. `execute` refuses
  to start a run whose supplied definition identity no longer matches the
  check's current definition identity.
- **A start after a long run is not late.** The scheduler breaks the healthy
  streak when a run starts late, because MMWatchdog did not observe the check
  in between. `checkRuntime.finished` is the end of the latest run. A start
  within half an interval plus one scheduler tick of `checkRuntime.finished`
  is on time. A check whose every run ends long after the check's time limit
  stays unknown
  without a break, and sends the check's cannot-check alert.
- **A stop of MMWatchdog waits for a run no longer than `waitForRun` does.**
  `Daemon.workers` counts `observe`, which returns when `execute` does, and
  not the goroutine of the module's code.

`CheckNow`, the run of `mmwatchdog check-now --without-daemon`, waits as
`waitForRun` does, and continues to the next check in the check group.

## Continuity

A check's failing time (`CheckState.FailingSince`) starts at its first failed
observation and ends in `Observe` when the check has been healthy for
`recovered_after_healthy_for`. A healthy observation inside a failing time
does not end the failing time, so a check that fails on and off stays in one failing time.
Every waiting time of the check counts from `CheckState.FailingSince`:
the alert (`alert_after_failing_for`), the restart
(`restart_after_failing_for`), the recovery command (`run_after_failing_for`),
and the raise
(`raise_to_sos_after_failing_for`). No waiting time counts from the moment
the incident opened. `FailingTimeRunsFailed` keeps whether each of the last
10 runs of the failing time failed. When the incident opens, `advance`
freezes the start of the failing time in the incident's evidence
(`failing_since`). The incident's evidence also freezes the number of
failed runs (`last_runs_failed`) and the total number of runs counted
(`last_runs_counted`). The alert's body includes the saved evidence.

The healthy streak (`HealthySince`) counts continuous observed health.
The unknown streak (`UnknownSince`) counts time for which MMWatchdog cannot check.
Outside `Observe`, two functions change `FailingSince`, `HealthySince`, and
`UnknownSince`:

- `Interrupt` records a maintenance boundary or daemon start. `Interrupt`
  clears the healthy and unknown streaks and ends a failing time whose
  incident has not opened. An open failure incident keeps its failing time
  until observed lasting health.
- `BreakHealthyStreak` records a late run or a finished recovery action.
  `BreakHealthyStreak` clears only healthy time. `BreakHealthyStreak` keeps
  failing time and cannot-check time.

An unknown observation, including an execution-capacity miss, clears healthy
time and keeps failing time through `Observe`.
`Summary` reports `confirming_failure` for a check inside a
failing time without an open incident, also while its latest observation is
healthy.

A named check's effective `alert_after_failing_for` and
`recovered_after_healthy_for` come from `NamedCheck.WaitingTimes`, the only
place that chooses between the check's own value and its check group's.
Validation records where `alert_after_failing_for` and
`recovered_after_healthy_for` are stated for `mmwatchdog config`.
A direct check has no waiting times of the check's own: the check group's
waiting times are the check's waiting times.
Event checks use zero alert delay for the failure track and reject an
explicitly written `alert_after_failing_for` on the check. Their
cannot-check track waits the check group's `alert_after_failing_for`
(`Timing.AlertAfterCannotCheckFor`, which `config.CheckDefinitions` fills
in).

A diagnostic execution waits for the check's gate like a scheduled execution.

The healthy window for resetting recovery attempts is calculated, not
stored. For a systemd restart, the window uses the `triggered_by_checks`
lists of every `restart_systemd_unit` that names the unit. For a recovery
command, the window uses the command's check group's
`recovery_command.triggered_by_checks`. Every triggering check must be
healthy with no open incident. The window starts at the latest
healthy-streak start among the triggering checks.
Anything that interrupts a check, and every finished action for it, therefore interrupts every recovery-attempt reset window
that depends on it. An action in progress cannot reset its recovery attempts; its healthy
window begins with accepted observations after the command completes.

## One incident lifecycle

Each check stores two incident lifecycles, called tracks:
`failure_incident` and `cannot_check_incident`. Each track uses the `Track`
shape: an open incident, the incident's frozen evidence, and an announced
incident. An announced incident has been announced to destinations, but
the incident's recovery or retirement has not been announced.
One function, `CheckState.advance`, applies the following lifecycle table
to `failure_incident` and `cannot_check_incident`:

| Transition | When | Notification |
| --- | --- | --- |
| open | a failing observation at or after the track's waiting time, with no open incident | failure; recorded locally only while suppressed |
| resolve | a healthy observation at or after the track's recovery time, with an open incident | recovery if that incident was announced and the check is not suppressed. Otherwise recorded locally |
| announce | not suppressed, still failing, open incident not announced | recovery of any earlier announced incident, then failure |
| close announced | not suppressed, healthy, no open incident, an earlier incident still announced | recovery |
| retire | definition changed or removed | closed_without_recovery, only for the announced incident |

A check is suppressed while a maintenance window covers the check's check
group or while the check is kept quiet because a check named by the check
is failing
([one rule for a check that is kept quiet](#one-rule-for-a-check-that-is-kept-quiet)).

For `failure_incident`, failing means a failed observation. The incident
opens at the first failed observation at or after `alert_after_failing_for`
into the check's failing time, never at a healthy observation, and resolves
when the check has been healthy for `recovered_after_healthy_for`, which
also ends the failing time. For `cannot_check_incident`, failing means an
unknown observation, the incident opens when the unknown streak has lasted
`alert_after_failing_for`, and any ok or failed observation
resolves the cannot-check incident. Opening and resolving
`cannot_check_incident` produce
`cannot_check` and `can_check_again` events, respectively.

Reminders belong to an announced incident that is still open.
The track stores the reminder start time and the last reminder time for each
destination. The reminder start time resets at announcement, at the
announced incident's latest raise in `CheckState.Raise`, or when the
maintenance window holding reminders ends. The latest reset sets the start
time. Reminder times commit with the incident and survive a restart.
When an incident stops being announced, `advance` clears the reminder times.

The scheduler tick's reminder step, `CheckState.Reminders`, is the only place
reminders are made. For each check outside maintenance and quiet time,
`CheckState.Reminders` makes one reminder per destination selected by `GoesTo`
([one rule for who gets an alert](#one-rule-for-who-gets-an-alert)).
A destination's `remind_every` must have passed since the later of the
track's reminder start time and the destination's last reminder time.
So no reminder is recorded that no destination gets.

`EndMaintenance` restarts the reminder interval for checks no longer covered
by maintenance. `SetQuietNow` restarts the reminder interval at the end of a
check's quiet time.
A `rebooted` event belongs to no track, so it is never reminded.
`remind_every` takes any positive duration, so reminders must not be able to
fill the outbox: `replaceQueuedReminder` removes the undelivered reminders
that a new reminder supersedes, those for the same incident and destination,
wherever a reminder is queued, including a joined node's queue for its hub
and the hub's alert intake.

Each track also keeps its open incident's alert severity and the severities
the announced incident's alerts went out with. When `advance` opens a failure
incident, `advance` assigns the check's `alert_severity`.
For a cannot-check incident, `advance` assigns the higher of the check's
`alert_severity` and the severity of the check's open failure incident.
When `advance` announces an incident, `advance` records the incident's
assigned severity among the announced incident's alert severities.
`CheckState.Raise` is the only place an open incident's severity changes,
and only upward.

Only the scheduler tick's raise step, `raiseOpenIncidents`, calls
`CheckState.Raise`, for the following conditions:

- A reload increases the check's `alert_severity`. The open incident rises
  at the next tick, or once maintenance holding the raise ends.
- `raise_to_sos_after_failing_for` is reached for `failure_incident` or
  `cannot_check_incident`.
- A fleet check reaches the configured time of silence.
- On the hub, `raise_to_sos_when_silent_nodes_reach_count` or
  `raise_to_sos_when_silent_nodes_reach_percent` is reached.
- A node's `raise_to_sos_when_every_node_of_these_node_groups_is_silent`
  applies (`Daemon.raiseSilentNodeGroups`, over the silent nodes counted
  for the count threshold).

Reconcile only updates the settings.
The raise by time, `CheckState.RaiseAfterTime`, counts from the
start of the failing time for the failure track and from the start of the
unknown streak for the cannot-check track, and falls back to the opening of
the incident only where state holds no such start.
`Observe` applies the count used by `CheckState.RaiseAfterTime` when
`Observe` opens an incident.
So an incident whose raise is already due opens at `sos`.

A raise of an announced incident appends the new severity to the track's
list of severities already sent and queues `failure_alert_severity_raised` or
`cannot_check_alert_severity_raised`; a suppressed announced incident is not
raised until the suppression ends. A check's `AlertSeveritySettings` are kept
out of its definition's identity, so changing them never retires the
incident.

On the hub, the silence check for a joined node takes severity and raise
time from the node's join and reports instead of the configuration. The
node's `raise_to_sos_after_this_node_is_silent_for` is the
`RaiseToSosAfterFailingFor` of its silence check (`NodeFleetSettings.Silence`).
`Daemon.raiseSilentNodeAfterTime` counts from the last receipt instead of
from the first failed observation, because missed intervals already count
as silence. The silence raise's reason comes from `Daemon.silentForReason`.

A daemon start gives every fleet check a fresh deadline.
Until a fleet check's fresh deadline passes, MMWatchdog cannot know whether
the node with a silence incident open before startup is still silent.
So `raiseOpenIncidents` raises a fleet check by time, and counts the fleet
check for
`raise_to_sos_when_silent_nodes_reach_count` or
`raise_to_sos_when_silent_nodes_reach_percent`, only when the fleet check's
deadline has passed.

While MMWatchdog runs, an open silence incident always has a passed
deadline.
Receiving a report moves the fleet check's deadline and resolves the
silence incident in the same call.

Severity is also needed during check execution, before an incident opens.
A `node_filesystem` check fails on a read-only filesystem only when the
check states an `alert_severity`, so execution uses
`Definition.CheckWithSeverity`. A check that only records its readings
(`CheckState.RecordsOnly`: a check with a core module that measures, without an
`alert_severity`) opens no incidents.

### One incident for each finding

A check whose module reports findings has failure incidents that are not the
check's own ([findings](design/checks.md#findings)). The
`every_systemd_unit` module is the one such module in MMWatchdog: each failed
unit is one finding, and the rules below say "unit" for a finding where they
describe that module. The check's state is marked
`CheckState.OneIncidentForEachFinding`, from the registry entry of its
module, and holds one nested `CheckState` for each finding in
`CheckState.Findings`, by the finding's ID. A finding's state has the shape
of a check's state on purpose: each finding goes through the same `Observe`,
`advance`, `raise`, `reminders`, and `retire` as a check, so the lifecycle
table above is applied once per finding and nothing about it is written
twice.

- **`observeFindings` is the only place a finding's state is made and
  observed.** `observeFindings` gives each finding that the observation lists
  (`Observation.Findings`) a failed observation of its own, with the
  finding's message. Each finding no longer listed in `Observation.Findings`
  gets a healthy observation.
  An optional `Finding.RecoveryMessage` is kept in the finding state's
  `FindingRecoveryMessage`, beside the finding's identity, and supplies the
  healthy message across daemon restarts.
  Without a stored `FindingRecoveryMessage`, the healthy message comes from
  `CheckModule.findingIsGoneMessage`, which `RunCheck` puts on the observation.
  For a module without `CheckModule.findingIsGoneMessage`, the fallback is
  `FindingIsGone`.

  `observeFindings` then forgets findings whose failing time has ended and
  whose incidents are neither open nor announced. If maintenance delays a
  recovered finding's recovery alert, `observeFindings` keeps the finding
  until the recovery alert has gone out.
- **The ID of a finding is checked wherever it enters MMWatchdog from another
  machine** (`ValidFindingID`): in a report and in an alert that a hub
  takes.
- **Every action on a unit goes through `findings()`.**
  Before every use, `findings()` copies the check's names and configuration
  source into each unit.
  `findings()` also copies the check's timings, severity settings, named
  destinations, the check keeping the check quiet, and the checks kept quiet.
  So reloaded check settings apply to the units without a second place to
  update. Only the questions that
  read a unit's incident and nothing else, such as `failureIncidentOpen`,
  look at the map directly.
- **The check itself keeps a failing time and no failure incident.** The
  parent check's failing time lasts while systemd lists any unit as failed.
  `recoveryDue` and the healthy window of a recovery budget read the parent
  check's failing time like any check's.
  `failureIncidentOpen` returns true when the check or any unit has an open
  failure incident. The callers are `interrupt`, the unknown path of
  `Observe`, `Summary`, and the healthy window.
- **Unknown belongs to the check.** When systemd cannot be asked, the
  cannot-check track of the check runs as for every check. Each unit is
  interrupted, as by any time in which MMWatchdog did not observe: its healthy
  streak clears, and a failing time whose incident has not opened ends.
- **The unit is part of what an alert is about.** `Event.Finding` carries
  the unit, and every place identifying an alert by check also uses the unit:
  the title (`AlertTitleParts`), the messages (`AboutCheck`), the hub's
  intake subject and ordering and grouping (`IntakeSubject`), the fleet
  incident key and the subject of the key's closing alert, the incidents
  carried by reports and `incidents_sent_without_the_hub`, the late line,
  and `replaceQueuedReminder`.
- **Inspection has one entry for each unit.** `Daemon.status` makes a
  `CheckStatus` for each unit, in `FindingStatuses`. `Summarize` adds one
  `attention` entry for each unit with an open incident.
  If any unit has an open incident, `Summarize` sets the command's exit
  code to 1. A report carries the units in `ReportedCheck.Findings`.

`systemd_units_to_ignore` is part of the check's definition, so changing it
retires the incident of every unit, like any other change of a definition.
The module's `identity` sorts `systemd_units_to_ignore`, so reordering the
ignored units changes nothing.

## One place for each module

A module is the code that does the work of one check or of one alert
destination ([modules](design/checks.md#modules)). Each core module has one
file that holds everything about it, and the rest of MMWatchdog knows no module by
name.

| | Check module | Destination module |
| --- | --- | --- |
| File | `check_NAME.go` | `destination_NAME.go` |
| Page source, beside the file | `check_NAME.md` | `destination_NAME.md` |
| Registry, one line for each module, with its topic | `CoreCheckModules` in `checkmodules.go` | `CoreDestinationModules` in `destinationmodules.go` |
| What the input type implements | `CheckInputs` in `module.go` | `destinationInputs` in `destination.go` |
| What holds the inputs | `Check.Inputs` | `Destination.Inputs` |

- **A module's file holds its input type, its description, the checks of
  its inputs (`validate`), and its code** (`run` for a check, `sender` for a
  destination). Adding a core module adds the module's code file, page
  source, and one line of the registry. Nothing else changes: the loader, validation,
  the scheduler, and the delivery worker ask the registry and the
  interfaces.
- **Every core module has a page source beside its code file,** the text of
  its module page, and its registry line names one of `CoreModuleTopics`
  (`checkmodules.go`), the topic that groups the module in the index of the
  module pages and in `mmwatchdog describe-module`. `go generate ./docs/reference/modules`
  joins the page source and the description into `docs/reference/modules/NAME.md`,
  which `mmwatchdog` carries for `mmwatchdog describe-module`
  ([module pages](documentation-map.md#module-pages)). Only `cmd/mmwatchdog` imports
  the package of the pages, `docs/reference/modules`.
- **Every core module has a description beside its code,** a
  `ModuleDescription` that its registry entry names: the summary, the
  inputs, the measurements, and at least one example. `mmwatchdog describe-module`
  prints the description. A core module can take one scalar in place of
  the module's mapping (`Value`), and the module's inputs can
  have the types `duration`,
  `list_of_integers`, and `mapping_of_strings`, which a description file
  cannot state: the module's own `validate` checks those values.
- **`Inputs` holds a pointer to a value of the module's input type, and the
  type says which module it is.** `Check.Module` and `Destination.Module`
  look the registry entry up by the type of the pointer in `Inputs`.
  A check or a destination that names no module has `Inputs == nil`, and
  `NoCheckModule` or
  `noDestinationModule` answers every question about it with "no".
- **What the engine must know about a module is a field of the module's
  registry entry or an optional interface of the module's input type.**
  A fact that holds for every check with the module is a field, such as
  `measures` or
  `OneIncidentForEachFinding`. A fact that depends on the inputs is an
  interface, such as `aboutSystemdUnit` or `TakesTimeForEachRun`. The engine
  never asserts a concrete input type. A new need of the engine gets a new
  field or interface, with a comment that says what the field or interface means.
- **Modules that share inputs share one type for them.** `tcp` and `udp`
  embed `ReplyRules` (`check_tcp.go`), and `smtp`, `imap`, and `pop3` embed
  `MailServer` (`mailserver.go`), which also opens the connection and
  switches it to encryption, and `postgresql` and `mysql` embed
  `DatabaseServer` (`databaseserver.go`). A shared type validates its own
  inputs and describes them once.
- **A module that refuses a setting beside its inputs says so itself**
  (`ValidatesWholeCheck` for a check, `ValidatesWholeDestination` for a
  destination). A `file` check with `must_not_exist` refuses a limit through
  `ValidatesWholeCheck`, and a `system_log` destination refuses
  `send_only_the_alert_title`.
- **A module never takes `true` as its whole value.** A check module names
  what it looks at and takes its failure rule as an input, as described in
  [the module naming rule](design/checks.md#modules). A boolean is only for
  a switch whose name says what it turns on, such as `only_record_readings`.
- **Two parts of MMWatchdog may know other modules.** The warning that an
  `every_systemd_unit` check gives about `systemd_unit_state` checks
  is in the `every_systemd_unit` module's file. The connection to MMHotline is a
  part of MMWatchdog of its own (`mmhotline.go` in `checksandalerts`,
  `store`,
  `fleet`, and `daemon`): it finds its destinations with
  `Destination.MMHotline`.
- **A mapping that names a module is read in one way.** `validateNode`
  takes a key that is no setting of the mapping as the name of a module. For
  a core module of the registry (`ModulesNamedIn`), `validateNode` checks
  the value against the module's input type. For a module of the node's
  catalog that is not core, `validateNode` checks the value as a
  `ModuleCallMapping`.
  `validateNode` refuses a name absent from the core registry and the node's
  catalog, with the places where MMWatchdog looked. `validateNode` also
  refuses a second module in a check, check group, or destination mapping.
  `splitModule` then separates the module's entry from the settings for
  decoding
  (`CheckGroup.UnmarshalYAML`, `NamedCheck.UnmarshalYAML`,
  `Destination.UnmarshalYAML`), and a name that no core module has decodes
  into a `ModuleCall`.
- **JSON shows a check and a destination as the configuration writes
  them:** the module's name with its inputs, beside the settings
  (`marshalWithModule`). `Check`, `NamedCheck`, `CheckGroup`, and
  `Destination` each have their own `MarshalJSON`, because a method of an
  embedded type would replace the JSON of the whole type. The identity of a
  check is the fingerprint of the check's JSON. The test configurations
  use the JSON format with module names and inputs.
  `Destination.UnmarshalJSON` reads the destination's JSON back for a queued
  delivery and for a joined node's copy of the fleet settings.
- **The path of an input in an error is below the module's name.** A
  module's `validate` returns the path of the input alone, and the caller
  puts the mapping and the module's name in front.

### Modules that are not core

An official extra module and a module of the project's own are called like a
core module and have no Go code of the modules' own
([how a module that is not core runs](design/checks.md#how-a-module-that-is-not-core-runs)).

- **`ModuleCall` is the input type of every module that is not core,** for a
  check and for a destination (`modulecall.go`). `ModuleCall` holds the
  module's name, the inputs as written, and the run settings (`ExecOptions`),
  which are written beside the inputs. The `ModuleCall` methods `CheckModule` and
  `destinationModule` answer the engine's questions from the
  module's description, where a core module has a registry entry.
- **`ModuleCatalog` knows which modules that are not core a node has**
  (`modulecatalog.go`). `LoadModuleCatalog` lists the two module directories
  and refuses official extra modules of another version than `Version`, a
  directory whose name no module can have, and a module with the name of a
  core module, of a setting (`SettingNamesBesideAModule`), or of an official
  extra module. `ModuleCatalog` reads a module's description only when
  something asks for the module, so MMWatchdog loads only the modules that
  the configuration names. `LoadWithModuleDirs` uses the two directories
  from a command line in place of the node's module directories.
  `Config.Modules` holds the catalog.
- **The description is what a call is checked against**
  (`moduledescription.go`). `ReadModuleDescription` reads
  `module_description.yml` strictly and checks each example against the
  description's own inputs. `ModuleCall.Validate` checks a call with
  `InputsProblem`, and `ModuleCall.Run` checks a result: a measurement that
  the description does not list, and findings that do not fit
  `one_incident_for_each_finding`, make the observation unknown.
- **MMWatchdog reads check-module file inputs before dropping privileges.**
  `ModuleCall.Run` reads each `_file` input through `ReadCredentialFile`,
  puts the text in `checkModuleRequest.FileContents`, and removes the file
  contents from every observation text. The configured paths stay in
  `Inputs`; scripts receive `run_check(inputs, file_contents)`. Passing the
  file contents lets an unprivileged module use credentials from root-only
  files without opening the files.
- **A script and an executable are started in one way and speak one
  protocol** (`ModuleCall.start`). For a script, the daemon starts this
  program again as `mmwatchdog run-starlark-module DIRECTORY`; for an executable, it
  starts `module_executable`, with `MMWATCHDOG_MODULE_HELPER` in the environment.
  The script and executable each read
  one JSON request and write one JSON result, so `ModuleCall.Run` and
  `ModuleSender.Send` do not distinguish a script from an executable.
- **`RunStarlarkModule` runs a script in the child process**
  (`starlark.go`). The child process already has the module's account,
  directory, and environment. A script gets the primitives
  under their names for scripts (`scriptRun.predeclared`), each a thin
  wrapper that returns a value with named fields and an `error` text
  (`primitiveResult`). Starlark's default options stay in force: no `while`
  loop, no recursion, and no loading of another file. `starlarkStepLimit`
  ends a script that does not end by itself.
- **A script's calls end shortly before MMWatchdog ends the module.** `ModuleCall.Run`
  and `ModuleSender.Send` write into the request how long the run has left
  by the end of their context (`mmwatchdogEndsTheModuleAfter`, the field
  `mmwatchdog_ends_the_module_after_milliseconds`). `RunStarlarkModule` gives the
  primitives a context that ends a tenth of the remaining run time earlier,
  and at most `timeForTheResultAfterTheCalls` earlier, and
  `endedShortlyBeforeTheTimeLimit` writes the error of a call that this
  context ended, with the name of the time limit. So the script decides its
  result, and `RunCommand`'s end of the module at the time limit stays the
  last resort, for a script that does not return.
- **The kind of a module is never read from the configuration.** A module's
  kind comes from where the catalog found the module (`ModuleKindCore`,
  `ModuleKindOfficialExtra`, `ModuleKindProjectStarlark`,
  `ModuleKindProjectExecutable`). Status carries the module's kind as
  `module_kind`, and `ModuleKindWords` writes the kind in words for a
  person.
- **A joined node checks the hub's destinations without the modules**
  (`InputValidation.WithoutModules`): the hub checked the inputs, and the
  node need not have the module. A delivery through a module that the node
  does not have fails with `moduleNotInstalled`.
- **Official module tests embed the module source files used in the tests.**
  The data-only package in `official_extra_modules_test_seeds.go`, at the
  repository root, embeds the scripts, descriptions, and pages of
  `official_extra_modules/` directly. Only tests import it; MMWatchdog's
  program does not. It is not in `official_extra_modules/`, because the
  playbooks install that directory on every node. The
  compiled official-module test binary handles `run-starlark-module` through
  `RunStarlarkModule`, so guest tests need neither Go nor a source checkout.
- **A new official extra module is a directory in `official_extra_modules/`
  and no Go code.** When a script needs another way of touching the node,
  the change is a new primitive, which every script can then use.

### One set of primitives

`primitives.go` and the two platform files hold the functions through which
a module touches the node: `ReadFile`, `OpenRegularFile` with `ReadFileFrom`
and `Sha256ChecksumOfFile`, `ListDirectory`, `ResolvePath`,
`InformationAboutFile`, `RunCommand`
(with `RunSystemProgram`, `systemctl`, and `journalctl` on top of it),
`HTTPRequest`, `ConnectTCP`, `TCPExchange` and `TCPExchangeUntil`,
`UDPExchange`, `eachAddressOf`, `ConnectUnixSocket`, `ResolveHostName`,
`DNSLookup`, `ping`, `TLSCertificateExpiry`, `MountedFilesystems`,
`FilesystemStatistics`, `ClockIsSynchronized`, `SQLiteQuery`,
`ConnectSystemLog`, `WriteToSystemLog`, `LookUpUserAccount`, `UserAccountName`, `GroupName`,
`IsExecutableFile`, and `IsDirectory`.

- **A module touches the node through a primitive and in no other way.** A
  module does not call `os.Open`, `exec.Command`, `net.Dial`, or
  `http.Client` directly. The daemon's own files, such as its state and the
  boot ID, are not
  a module's and are read where they are used.
- **Each primitive that reads has a limit on what it reads:** `ReadFile`
  takes its limit from the caller and reports a larger file as
  `ErrFileLargerThanLimit`; `RunCommand` reads 64 KiB of the standard output
  of a check's program and 1 MiB of a program that MMWatchdog runs as itself;
  `HTTPRequest` reads 1 MiB of a response; `TCPExchange` reads 64 KiB.
  `ConnectTCP` gives a core module the connection for a protocol of its own,
  such as SMTP, and that module limits what it reads.
- **`RunCommand` runs a program in one of two ways, and the caller states
  which.** With `runAs`, the program has the account, directory, and clean
  environment of a shell check.
  A shell check, a module that is not core, and a recovery command use
  `runAs`. With `asMMWatchdogItself`, the program runs as the calling
  MMWatchdog process, with the calling process's environment. The daemon
  runs as root, so a program using `asMMWatchdogItself` in the daemon runs
  as root, and only the programs through which MMWatchdog asks the system
  run this way (`RunSystemProgram`), such as `systemctl` and `journalctl`.
  In the process that runs a Starlark script, `asMMWatchdogItself` uses the
  module's account. The script's
  `run_command` uses `asMMWatchdogItself`, with the output limit of a check's
  program (`standardOutputLimit`). A run that states neither `runAs` nor
  `asMMWatchdogItself` is refused, so no caller runs a program as root by
  leaving a field out. A program gets a process group of its own, and
  the program's process group ends with the run.
  The one exception is a program that a script starts
  (`StaysInThisProcessGroup`): it stays in the process group of the process
  that runs the script, and the daemon ends that whole group at the time
  limit, so the script and its programs end together.
- **`RunCommand` reads a program's outputs through pipes of its own,
  never through `exec.Cmd`'s.** `exec.Cmd` with `WaitDelay` stops reading
  its pipes a fixed time after the program ends, and on a busy node what
  the program wrote can still be in a pipe then: the result of a check, or
  the last line of standard error that gives the reason of a failure.
  `RunCommand` ends the program's process
  group once the program ends, waits a moment for the output pipes to
  close, and then reads what is in each pipe without waiting further: only
  a program outside the group, such as a daemon that the program started,
  can still hold an output open.
- **`HTTPRequest` never writes the request's address into an error.** An
  address can be a secret, such as the address of a chat webhook. A caller
  whose address is no secret, such as the `http` check, adds it to its
  message.
- **`HTTPRequest` reads a response body only when the caller wants it**
  (`ReadsResponseBody`, by the status code). The `http` check asks for the
  body of an answer with an expected status code only, so an answer with
  another status code fails the check at once, also when its body is slow.
- **The kernel's files are read below `ProcDirectory` and `SysDirectory`,**
  which are `/proc` and `/sys`. A module never writes `/proc` or `/sys`
  itself, so a test puts a directory of its own in their place
  (`testhelpers.KernelFiles`) and runs the module on a Mac.
- **`FilesystemStatistics` never waits longer than its caller allows.** The
  kernel can block a query about a filesystem on another machine indefinitely.
  MMWatchdog cannot cancel the kernel query, so the goroutine that asked
  stays until the kernel answers.
  MMWatchdog therefore keeps at most one pending filesystem-statistics call
  per path (`FilesystemQuestions`). A caller that finds a pending call for
  the path starts no second call and waits for the pending call's result,
  up to the caller's own time limit: two checks of one filesystem that run
  in the same moment both get the answer, and a
  check that runs every minute keeps one goroutine waiting, not one more
  for each run. A caller whose time ends gets `ErrFilesystemDoesNotAnswer`.
- **A module that reads a file opens it once** (`OpenRegularFile`) and takes
  everything from that open file: what kind of file it is, its owner and
  size, and its content. So a path at which one file takes the place of
  another shows one file only, and a named pipe at the path holds no run up.
  The kernel's question about a file has no time limit, as the one about a
  filesystem has none: a file on a filesystem of another machine that is
  gone keeps its run until the kernel answers. The daemon releases the
  blocked run's place ([a run that does not end](#a-run-that-does-not-end)).
- **Text from a server is read through a limit.** `TCPExchangeUntil` and the
  mail conversation read at most a fixed number of bytes. A reader that
  takes whole answers into memory by itself, as Go's SMTP client does, gets
  a `ReadLimitedConnection`, in the checks and in the email destination.
- **A program whose output can be very long ends at its limit**
  (`EndsAtStandardOutputLimit`): `journalctl` for the journal of a unit.
  Every other program runs to completion, because writing output may not
  be the program's only work.
- **`DNSLookup` writes the DNS question itself** (`DNSQuestion`, with
  `golang.org/x/net/dns/dnsmessage`) and sends it to the server that the
  caller names. `DNSLookup` does not use Go's resolver: Go's resolver
  answers from the node's hosts file first and takes its times from the
  node's settings.
  `ResolveHostName` is the node's own resolver, for the name of a machine.
- **A caller that sends UDP packets to a host name tries each of its
  addresses** (`eachAddressOf`). TCP tries each address by itself.
- **A core module asks a tool of the node through the tool's own program,**
  as MMX's modules do: `psql` and `mariadb` for the database servers,
  `docker`, and `apt-get`. The module chooses the account by the access
  the program needs. For a database connection through a local socket
  without a login, use the database server's administrator account
  (`PostgreSQLAdministratorAccount`, `MySQLAdministratorAccount`). Wherever
  an unprivileged account is enough, use `UnprivilegedAccount`. Use
  MMWatchdog's own account, root, through `RunSystemProgram` only when a
  program needs root. For example, `docker` answers only root and members
  of the group `docker`. A secret reaches a program in the program's
  environment or on standard input, never as an argument. A test learns as
  which account a program ran through `LookUpUserAccount` (`asThisAccount` in
  `server_checks_test.go`) and asserts it.
- **A file of the operating system is read below `NodeFilesRoot`**
  (`NodeFile`), such as `/etc/os-release` and `/run/reboot-required`, so a
  test puts a directory of its own there. `NodeOSRelease`
  (`operatingsystem.go`) is the one place that reads `/etc/os-release`.
- **`SQLiteQuery` opens the database of another program for reading only**
  and with SQLite's own `query_only`, as an ordinary reader with SQLite's
  locks. Never read a live database without locks (`immutable=1`): SQLite
  allows reading without locks only for a file that cannot change. An
  unlocked reader can falsely report corruption while SQLite copies
  changes from the write-ahead log into a healthy database.
  A caller fails a check only on SQLite's verdict of damage,
  `SQLITE_CORRUPT` or `SQLITE_NOTADB`, by the error's code, never by its
  text: see `queryProblem` in `check_sqlite_database.go`.
- **`ping` opens a socket that only root may open,** and returns
  `ErrPingNeedsRoot` for another account. The socket receives every ICMP
  message that reaches the node, so a request carries a random marker, and
  only the answer with that marker counts.
- **A new primitive comes with the first core module that needs it,** and
  has a test of its own in `primitives_test.go`, or in the test file of
  that module when the two cannot be told apart.
- **A script calls the same primitives under their names for scripts:**
  `read_file`, `run_command`, `http_request`, `tcp_exchange`, and
  `udp_exchange` in `starlark.go` call `ReadFile`, `RunCommand`,
  `HTTPRequest`, `TCPExchange`, and `UDPExchange`. A primitive that scripts
  should have gets its wrapper in `starlark.go` and a row in the table of the
  [design](design/checks.md#primitives) and of the
  [writing modules reference](../reference/writing-modules.md#primitives).

### What a check keeps between runs

- **`CheckRun.Memory` lasts while MMWatchdog runs.** `checkRuntime.memory`
  holds the check's memory for the scheduled runs of one check, and the
  check's gate lets one run use
  the memory at a time. A run of `mmwatchdog check-now` gets an empty
  `CheckRun.Memory`, so the diagnostic run never changes what the scheduled
  runs see. The CPU modules keep their sampler in `CheckRun.Memory`
  (`CheckRun.CPUSampler`), and a module that measures a rate from the
  kernel's counters, such as `node_disk_device`, keeps the previous reading
  of the counters in `CheckRun.Memory` (`CheckRun.CounterReadings`).
  CPU modules and modules that measure rates from kernel counters forget
  a baseline that is too old for the check's schedule
  (`CheckRun.baselineIsTooOld`). When a CPU or counter-rate module has no
  baseline, the module takes a sample of `CPUSampleWindow` duration.
  A CPU module or a module that measures rates from kernel counters states
  the sampling duration with `TimeOfEachRun`.
- **`CheckRun.State` lasts across restarts of MMWatchdog.** The check's
  persistent state is JSON in `CheckState.ModuleState`.
  `out_of_memory_kills` keeps the kernel's last count in
  `CheckState.ModuleState`, and `log_lines` stores how far the check has
  read in `CheckState.ModuleState`. A run loads the module's persistent
  state and can ask to save the updated state (`ModuleState`);
  `Daemon.observe` stores what a scheduled run saved, in
  the commit of that run's observation, and only when the run's ticket still
  matched. A run of `mmwatchdog check-now` starts from a copy and stores nothing.
  The crash-loop module keeps the module's history of restarts in
  `CheckState.ModuleState`. A changed definition starts a new `CheckState`,
  and so an empty state.
- **A module that is not core keeps its state in a directory of the check,**
  not in `CheckState` (`modulestate.go`):
  `MODULE_STATE_DIR/CHECK_GROUP/CHECK`, which belongs to the module's
  account. For a scheduled run, `CheckRun.ModuleStateDirectory` prepares
  `MODULE_STATE_DIR/CHECK_GROUP/CHECK`. For `mmwatchdog check-now`,
  `CheckRun.ModuleStateDirectory` prepares a temporary copy and removes the
  copy afterwards. An executable gets the scheduled run's directory path
  or the diagnostic run's copy path in the request. A script's `write_state`
  stores one value as JSON in the directory prepared for the run.
- **MMWatchdog works only in a `module_state_dir` that it made.** Before
  MMWatchdog makes, uses, or removes anything in `module_state_dir`,
  MMWatchdog checks that each directory above a state directory is
  MMWatchdog's own (`NotMMWatchdogsOwnDirectory`): a real directory and
  no link, owned by MMWatchdog's account, with the permission mode assigned
  at creation.
  `ModuleStateDirProblem` also refuses a `module_state_dir` below a
  directory that other accounts can change. So a setting that names a
  directory with other content by mistake does not lose the content.
  MMWatchdog never changes the mode of a directory that MMWatchdog did not
  make.
- **A check that is gone loses its state directory in two steps**
  (`TakeAwayModuleStateOfChecksThatAreGone`). Under the daemon's lock, MMWatchdog
  moves the directory into `.removed`, which costs the same whatever the
  directory holds, so a new check with the removed check's check-group name
  and check name starts with no state.
  After the lock is free, MMWatchdog removes the moved directory. The
  time needed to remove the directory depends on the module's account
  and must not hold the daemon. MMWatchdog names each directory by the
  check that MMWatchdog knew. MMWatchdog never takes away directories found
  by listing `module_state_dir`. Startup also removes leftovers from an
  earlier run in `.check-now` and `.removed` (`RemoveLeftoverModuleState`). A
  reload refuses another `module_state_dir`: the checks that it keeps would
  lose their state.
- **The daemon never follows a path into a state directory with root's
  rights, except through `os.Root`.** The daemon is root, and the module's
  account controls everything inside the directory. With a plain path, the
  module's account could put a symbolic link in the place of a directory
  while the daemon works, and root would read or give away a file elsewhere
  on the node. So `StateDirectoryOwner.give`
  changes one directory and nothing inside the directory.
  `CopyStateDirectory` opens every path of the copy through
  `os.Root`, copies a file only after it has seen on the open file that the
  file is regular, and opens without waiting, so a named pipe cannot hold
  the daemon. `CopyStateDirectory` leaves out a file of another account
  (`StateDirectoryOwner.owns`), which the module's account can only have put
  in the check's state directory as a hard link, so the copy never gives
  the module's account a file that the account could not read. The copy ends
  with the time limit of the check, after `CheckNowStateCopyEntryLimit`
  files and directories, and at
  `CheckNowStateCopyLimit` bytes, because reading a sparse file can produce
  far more bytes than the file occupies on disk. The copy stays owned by
  root and closed until copying finishes. The directories
  above a state directory belong to root. Removal uses `os.RemoveAll`,
  which does not follow links.

### What a module adds for the alert's body

- **`Observation.Details` holds lines that say more than the message has
  room for,** such as the five commands that use the most memory
  (`processes.go`) or the commands that a unit runs. The module writes the
  detail lines during every failed run, outside the daemon's lock. The
  incident freezes the detail lines from the confirming observation, and
  `evidenceLines` puts the saved detail lines into the alert's body.
  No part of MMWatchdog reads the node when an incident opens.
- **A line of the details is plain text on one line.** A module passes text
  that the node or a server gave, such as a command line, through
  `DetailLine`, which replaces each control character, the line separators
  of Unicode, and the characters that turn the direction of text, so a line
  cannot change an operator's screen. A message that holds
  such text goes through `DetailLine` too. The error of a non-core module
  or of a delivery goes through `checksandalerts.OneLine`, which keeps
  300 bytes in place of 200 and is built on the same
  `primitives.OnOneLine`, so both follow one rule. An observation holds at
  most `DetailLinesLimit` lines.

## One limit rule

A check that measures a number only reports measurements. The check's
limits are the two maps
`fail_when_measurement_above` and `fail_when_measurement_below` on the check.
`applyLimits` is the only place that decides a check's observation from its
measurement limits, and it writes the message that names each reading and
its limit. The modules whose job is to measure (`measures` in the registry,
such as `node_resource` and `node_disk_device`) call `applyLimits` through
`MeasurementObservation`. A module that says "failed" by itself and also
reports measurements
(`limitsOnItsMeasurements`) calls `applyLimits` through `withLimits`.
For an `ok` result from a module that is not core, `ModuleCall.Run` calls
`applyLimits`. A `failed` or `unknown` result is the module's own verdict
and passes unchanged. `every_disk_and_network_filesystem` applies the limits
of its check to each filesystem by itself, through `FilesystemObservation`.
A limit on a
measurement that the observation does not have makes it unknown, never a zero
reading. The limits are part of a check's definition, so changing one retires
the check's incident like any other change of its definition.
`Check.MeasurementNames` lists what the check's module measures, for
validation and for the order of the message: a module whose inputs decide
its measurements names them itself (`namesMeasurements`), and every other
module lists them in its description.

## One recovery decision

Recovery policy is computed when configuration activates: per unit for
restarts, from every `restart_systemd_unit` that names that unit, and per
check group for its `recovery_command`. Check groups that restart one unit
must state the same `max_attempts`, `min_time_between_attempts`, and
`reset_attempts_after_healthy_for`. Validation refuses different values and
names the source locations of the conflicting values. No merge chooses a
different budget.
`max_attempts` is a number or `unlimited` (`MaxAttempts`). The policy also keeps each
triggering check's waiting time: the `restart_after_failing_for` of the
check group whose restart lists the check, or the `run_after_failing_for`
of the check group's `recovery_command`. Policies and recovery attempts are
keyed by unit or by check group name, which cannot collide. `recoveryDue(key, now,
unitState)` is the only test for a systemd restart or a recovery command.
MMWatchdog calls `recoveryDue` when an observation completes and again before
the reservation commits (for a restart, after the systemd inspection).
`recoveryDue` returns the name of the eligible check in `triggered_by_checks`.
A recovery command receives the triggering check's name. The triggering
check must meet all the following conditions:

- The check's latest observation failed.
- The latest observation is fresh.
- At the latest observation, the check's failing time had lasted the
  action's waiting time.
- The check is not kept quiet
  ([one rule for a check that is kept quiet](#one-rule-for-a-check-that-is-kept-quiet)).

The action's waiting time counts to the triggering check's latest failed
observation, never to the time of another check's observation.
The triggering check's observation also opens the check's incident.
So an action and an alert that wait equally long come with the same
observation.
The check's incident need not be open, so an action can run before
the alert, and a recovery command then gets an empty `MMWATCHDOG_INCIDENT_ID`. A
`systemd_unit_state` check of
the unit being restarted does not call for the restart once systemd reports
that unit active. A check group's recovery command and a restart of a unit
that the check group names never run at once, and commands are considered first, so a
restart due at the same moment waits. Because each result advances the epochs
of the checks of every check group it concerns, `recoveryDue` sees only failures observed
after the last action.

MMWatchdog never starts a stopped unit. `restart` is the only function that
decides whether the unit's systemd state permits a restart. When systemd
reports the unit `inactive`, `restart` withholds the restart and uses no
attempt. The `restart` function records one `systemd_unit_restart_withheld`
event for the triggering check's failing time, remembered in
`RecoveryAttempts.WithheldForFailingTimeSince`.
The `restart` function restarts a unit that systemd reports `active` or
`failed`. No setting changes which systemd states permit a restart.
A unit can stay stopped for days, so `startRecovery`
lets `restart` ask systemd about it once for each failed observation of the
check that calls for the restart (`Daemon.stoppedUnitSeenAt`), not at every
completed observation on the node.

## One rule for a check that is kept quiet

`onecause.go` owns the setting
`no_alerts_or_recovery_actions_while_these_checks_fail`
([one cause, one alert](design/checks.md#one-cause-one-alert)): its
validation, the question whether a check is kept quiet, and the texts that
say so. The check with the setting names the checks whose failures should
keep the configured check quiet. While a named check is in failing time,
MMWatchdog keeps the configured check quiet. The period of suppression is
the configured check's quiet time.

- **A quiet time is suppression without a boundary.** The quiet time is
  passed to `Observe` as `suppressed`, together with maintenance, so
  `advance` applies the lifecycle table as in maintenance: an incident opens
  and resolves in the records. For an announced incident, MMWatchdog
  postpones queueing the recovery alert. Unlike a maintenance boundary,
  the start and the end of a quiet time call no `interrupt` and start no
  new epoch. The quiet check's failing time and streaks continue, so the
  quiet check's waiting times keep counting.
- **`quietAtObservation` alone records the start and end of quiet time in
  the check's state.** `quietAtObservation` runs when an observation of the
  check is applied. `Daemon.accept` is the one place that applies a
  scheduled run's observation. The scheduler tick also calls
  `quietAtObservation` for the unknown observation recorded for a missed
  run. `quietAtObservation` stores the answer in
  `CheckState.QuietNowWhileCheckFails`: the first named check that counts as
  failing (`Snapshot.FailingCause`).

  When no named check counts as failing, `quietAtObservation` keeps the
  previous cause while any named check needs a fresh observation
  (`checkRuntime.needsFresh`). Fresh observations are required after a
  daemon start, a maintenance boundary, or a reload that changed a named
  check. Until MMWatchdog has observed every named check again, the quiet
  check's run does not decide quiet time.
- **`CheckState.CountsAsFailing` says when a named check keeps others
  quiet:** during its failing time, and after `interrupt` ended a failing
  time whose incident had not opened, for as long as the check's latest
  observation remains the failed observation from before the interruption.
  A late run of the named check therefore ends no quiet time.
  An unknown observation of the named check also keeps quiet time while the
  named check remains in failing time.
  `TestAMissedRunOfTheCauseKeepsTheQuietTime` checks that a missed run of the
  cause keeps quiet time while the cause remains in failing time.
- **`quietWhile` decides whether a check is quiet between observations.**
  The raise and reminder steps of the scheduler tick and `recoveryDue`
  call `quietWhile`. `quietWhile` returns a named check that counts as
  failing now, so suppression takes effect as soon as the named check
  starts to fail. Otherwise, `quietWhile` returns the stored cause, so quiet
  time ends only at the quiet check's own next observation. A run observed during quiet
  time cannot trigger an alert or recovery action after quiet time ends.
  `Daemon.status` distinguishes a currently failing cause from a stored
  cause awaiting the quiet check's next observation. The currently failing
  cause appears in `CheckState.QuietNowWhileCheckFails`. Otherwise, the
  stored cause appears in `CheckStatus.QuietUntilNextRunAfterCheckFailed`.
- **`recoveryDue` skips a quiet check** and goes on to the other checks of
  the policy. Another check in the policy remains eligible to trigger the
  recovery action even when a quiet check cannot trigger the action.
- **`SetQuietNow` restarts the reminder count at the end of a quiet time**
  (`ResumeReminders`), as `EndMaintenance` does at the end of a window.
- **`CheckState.Event` writes what an event says about the rule.** For the
  failure track's `failed`, raise, and `reminder` events, `CheckState.Event`
  copies the names of checks kept quiet into the event. The event includes
  at most `QuietChecksNamedInAnAlert` names and the count of all checks kept
  quiet (`NamedInAnAlert`).
  `AlertBody` writes the checks kept quiet into the body with
  `quietChecksLine`. For an event that is only recorded while the check is
  kept quiet, `CheckState.Event` adds the reason to the message.
  `Event.OnlyTheTitle` removes the names with the body.

  An announced incident can resolve during quiet time or
  maintenance, with the recovery alert sent when a later incident is
  announced. The delayed recovery alert uses the message text in
  `TrackEvents.Stale`. The message describes recovery delayed by quiet time
  or maintenance.
- **The named causes and the checks kept quiet belong to a definition,
  but do not define the check's identity.**
  `CheckDefinitions` gives each definition the checks that it names and,
  from the other definitions, the checks that name it, in order of name.
  `Reconcile` copies the named causes and the checks kept quiet into the
  check's state, as `Reconcile` does for severity and named destinations.
  `Reconcile` clears the stored cause when the check no longer names the
  stored cause. `findings()` copies the kept answer and
  the checks that the check keeps quiet into each failed unit of an
  `every_systemd_unit` check.
- **`validateChecksNamedAsCauses` runs after every check group is
  validated,** because a name can point into any check group, and a direct
  check gets its name in that validation. `validateChecksNamedAsCauses`
  refuses checks that name each other in a circle (`circleBackTo`): while
  all checks in the circle fail, each would
  keep the next one quiet, and MMWatchdog would send no alert.
- **Heartbeats and fleet checks never have the setting.** Validation refuses
  `no_alerts_or_recovery_actions_while_these_checks_fail` on a heartbeat
  check group, and fleet checks come from membership, not from configuration.
  So the paths that apply heartbeat and fleet check observations
  (`Daemon.heartbeat`, the scheduler tick's overdue heartbeat,
  `EndMaintenance`, and `fleetwatch.go`) pass only maintenance as
  suppression. A heartbeat can be named: it has a failing time like every
  check.
- **Inspection has one wording.** `CheckStatus.KeptQuietSentence` is the line
  of `mmwatchdog status`, and `ReportedCheck` adds the same note to the message that
  a report carries. The status line and report note come from
  `KeptQuietNote`, which answers for a quiet check that fails, that
  MMWatchdog cannot run, or whose recovery alert waits
  for the end of the quiet time, and says whether the named check still
  counts as failing.

## One place writes an alert's text

`alerttext.go` writes the title, the title with the alert's severity in
front (`titleWithAlertSeverity`), and the body of every alert
(`Event.WriteAlertTexts`), and nothing else does. The body's line about the
checks that a failing check keeps quiet is `quietChecksLine` in
`onecause.go`, which `AlertBody` calls. `Store.Save` calls
`Event.WriteAlertTexts` for a node's own notifying events, where the event
gets the node name. So the texts are in the event history, in every
destination's queue, and in the alert a joined node sends to the hub.
`Store.Emit` calls
`Event.WriteAlertTexts` for the hub's grouped message, which has a title and
a body of its own.

A destination that keeps incidents gets one closing alert for a key across
the fleet when the incidents of several nodes have closed. The closing
alert has separate text. `WriteFleetIncidentClosingTexts`
writes the closing alert's texts from the words of `FleetIncidentClosingTitle`.

The MMHotline destination takes an alert's texts unchanged. Only MMHotline
resolutions, which concern one key across the fleet, have separate titles,
using the words of `FleetIncidentClosingTitle`.

A destination with `send_only_the_alert_title` gets `Event.OnlyTheTitle`,
made in `QueueDirect`.
`Hub.Late` adds its line to the message and to the body at delivery. No
sender writes wording of its own: a sender or a module that shows the
severity to a person takes `Event.TitleWithAlertSeverity`, as the email
subject does, and writes no label itself. The email sender (`emailMessage`) adds only
the closing lines that identify the alert, its time and its IDs.
The email sender omits the alert body and identifying closing lines for a
delivery that was queued for a destination with
`send_only_the_alert_title` (`Delivery.Target`). A notice sent with `mmwatchdog send-notice` is the
one alert whose words come from outside: its message is the title text and
its body the text that its sender gave, and `WriteAlertTexts` puts the node's
name in front of the title and keeps the body.

## One rule for who gets an alert

`GoesTo` in `severity.go` is the only place that decides whether a
destination gets an alert: a check that names its destinations
(`send_alerts_only_to_destinations`, carried on every event of the check)
reaches exactly those, and every other alert goes by the destination's
`alert_severities` (`Destination.Takes`). A name that the destinations in
force do not have is discarded without discarding known names. Only a
nonempty list without known names falls back to severity.
`receives` adds the exception for a recovery or retirement: MMWatchdog first
uses the incident's recorded destinations. `RememberIncidentDestinations`
records destinations by the recovery and retirement routing rule.
`CheckState.Reminders` asks `GoesTo` before it makes a reminder, with the
reminder itself, so the reminders that MMWatchdog records and those it queues
cannot disagree. A reminder follows the incident's current severity, not
the incident's recorded destinations: after a raise to `sos`, a destination
that takes only `warn` gets no more reminders. `GoesTo`
serves a standalone node, the hub's grouped messages, and a joined node that
delivers without the hub from its copy of the fleet settings. The hub's
grouping key (`Event.RoutingGroup`) keeps alerts with different severities or
different named destinations apart. A check with an empty list sends nothing:
`CheckState.Event` records the check's events without queueing them. The
check's list of named destinations is kept out of the check's identity,
like the severity.

A notice that `mmwatchdog send-notice` sends to one destination carries that name in
`send_alerts_only_to_destinations`, so `GoesTo` routes the notice by name on
the node and on the hub.
`queueNotice` in `notice.go` commits a notice as a control change: a notice
that cannot be saved is not sent. The wait by `mmwatchdog send-notice`
changes nothing about delivery. The delivery worker tells the waiting `mmwatchdog send-notice` command the
result of each attempt
(`noticeAttempted`), and `NoticeDelivery` reads from the outbox whether an
attempt can happen now; the wait ends at once for a destination whose
oldest undelivered alert has already failed. The hub's grouping step sends a
notice at once and alone (`AlertGroupingState.Plan`).

## One destination identity

Alert destinations come from `alert_destinations.yml` in the configuration
directory selected by `--config-dir`, beside `mmwatchdog_node_settings.yml` and
`check_groups/`. MMWatchdog validates and activates the destinations as one
complete candidate.

A destination's identity consists of the configured name, module,
module-specific target, and credential content. The name selects the
configured destination. For `mmhotline`, the target is the alert source ID.
For `email`, the target is the mail server and recipients, compared by
`sameEmail` without regard to recipient order. For a module that is not
core, the target is the module's recipient inputs.

Activation captures a SHA-256 fingerprint of credential contents on
`Destination`. The private store representation keeps the credential
fingerprint in queued targets and destination health in `state.db`.
Public destination JSON, including inspection and fleet settings, never carries
the fingerprint: an unsalted hash would let a reader test password guesses.
`SameDestination` compares the captured credential fingerprint and the
module's receiving address. An unreadable credential has no fingerprint.
Only two readable, different credential fingerprints cancel queued alerts
because of a credential change.

The MMHotline signing secret is an exception to credential comparison.
An MMHotline signing secret can rotate without cancelling queued alerts
because `SameDestination` excludes the signing secret's content from receiver
identity. The alert source ID alone identifies the MMHotline receiver,
so a new address for the same alert source also keeps queued alerts.

Credential paths and message presentation such as `mention` are not receiver
identity. An email destination's identity excludes `from`, `login_user`, and
`smtp_connection_security`. A destination module's `run_as_user_account`,
`run_from_directory`, and `env` are also excluded from destination identity.
`time_limit_per_delivery_attempt`, `remind_every`, `send_only_the_alert_title`,
and `alert_severities` are not part of destination identity.

`ModuleCall.sameTarget` compares inputs as the JSON that a queued delivery
stores. A queued delivery reads a number back as a float, where the
configuration holds a whole number. Comparing the Go values would cancel
every queued delivery at the next commit.

Every queued delivery keeps the destination as the destination was queued.
Every commit cancels queued deliveries whose destination identity is no
longer configured. So configuration activation and cancellation of removed
destinations always happen in one transaction. Before sending a queued
event, the delivery worker also checks whether the queued destination's
identity is still configured. Each destination has at most one delivery in
flight, independent of deliveries to other destinations.

Every notifying event is queued for each destination selected by `receives`.
An alert, reminder, or notice is queued by severity (`Destination.Takes`).
For a recovery or retirement, `receives` first uses
`incident_alert_destinations`. `QueueDirect` records the destinations for
which each incident's failure and raises were queued. The records have no
age limit and remain until the incident closes.

Without an incident destination record, a recovery or retirement is queued
by any severity used to announce or raise the incident. A reminder is
queued only for the destination the reminder names.

An `mmhotline` destination's delivery holds the MMHotline alert when queued
(`QueueMMHotline`). The alert is built and fitted to MMHotline's limits,
including the encoded body, so a retry sends the same alert.
A signed permanent refusal drops the delivery with a delivery gap and an
event, in one transaction.

The outbox keeps `queued_at` separately from the event time: an old event
received now has not already waited in the delivery queue. Destination
health puts the stored `queued_at` time on the daemon's monotonic clock
once and measures the ten-minute notice delay on the monotonic clock.

Verification requests run outside `Daemon.mu`. Completion checks the
captured destination identity and state before changing destination health.
`destinationHealth` owns one schedule and one notice lifecycle per
destination. The durable part of `destinationHealth`,
`store.DestinationHealth`, is stored per receiver in `destination_health`:
the target identity, time verification started failing, latest verification
error, whether the failure notice was queued, and next verification time.
`Store.Save` commits the destination-health state and destination-health
notice events in the same transaction, through the existing
`record` and `control` paths. Startup restores unchanged receivers and
places the verification failure start time and next verification time on
the daemon's monotonic clock. Only verification advances the
destination-health verification schedule. A delivery does not reset the age
of another alert that is still queued. Health notices use `record`,
with `Event.DeliverOnThisNode` selecting direct queueing even on a joined
node. Destination-health notices remain ordinary
`notice` events in storage and reports.

## One run of the module for each attempt

`modulecall.go` owns the use of a module that is not core as an alert
destination: what a destination can state for it (`ModuleCall.Validate`),
the destination's identity (`ModuleCall.sameTarget`), and one delivery attempt
(`ModuleSender.Send`).

- One attempt is one run of the destination module, through
  `ModuleCall.start`, which also runs the module of a check: the account,
  the environment, the process group, and the output limits are the same.
  The time limit is the destination's `time_limit_per_delivery_attempt`.
- MMWatchdog reads every file that an input names (`Destination.ReadCredentialFiles`) and writes the
  contents into the request. A module never gets a path that it must open.
  On a joined node the contents come from the copy of the fleet settings
  (`FleetSettings.FileContentsByDestination`), and the sender never
  reads the hub's paths on the node's disk.
- Only the answer `delivered` with a message acknowledges the delivery.
  Every outcome except `delivered` with a message is an error that names the
  module. The outbox retries the delivery. The sender never retries a delivery.
- Every text that comes from the module goes through `WithoutFileContents`
  and `OneLine` before becoming part of an error. The error excludes the
  contents of files and occupies one line.
- The request is written with `PlainJSON`, which leaves `<`, `>`, and `&`
  as they are, as `module-json object` does.
- A request is at most `ModuleRequestLimit` bytes, which is also what the
  `module-json` helper reads. `ModuleFilesLimit` bounds the files of one
  destination together, and `ModuleInputsLimit` bounds the destination's
  inputs. The file and input limits leave room for the largest alert.
  `Destination.ReadCredentialFiles` enforces `ModuleFilesLimit` for a
  delivery, for `mmwatchdog validate`, and for fleet settings.
  `ModuleCall.Validate` enforces `ModuleInputsLimit` when the configuration
  is loaded.

  `TestAnAlertAndTheFilesOfADestinationFitInOneRequest` checks that a request
  containing the largest alert, inputs, and file contents fits within
  `ModuleRequestLimit`. A change that makes an alert larger must keep
  `TestAnAlertAndTheFilesOfADestinationFitInOneRequest` passing.

`Store.FailingDestinations` reads, for `mmwatchdog status` and a node's report, each
destination whose oldest undelivered alert has a failed attempt.
`Store.FailingDestinations` is the one place that decides what "failing"
means, for every destination module, including `mmhotline`.
Per-destination MMHotline connection status also reports signed answers
and check-ins.

### One way to carry a credential

Every destination module says which of its inputs name a credential file
(`CredentialFiles`) and how MMWatchdog reads each file (`ReadCredentialFile`).
The module's `ValidCredential` checks what the credential-file contents can be.

- **`Destination.ReadCredentialFiles` is the one reader.**
  `mmwatchdog validate` uses `Destination.ReadCredentialFiles` to find a
  file that cannot be read. A hub uses `Destination.ReadCredentialFiles`
  when the hub starts and reloads (`NewFleetSettings`). An error names
  the path of the input in the configuration (`Destination.PathOfInput`).
- **A hub sends the contents to its joined nodes in one map,**
  `FleetSettings.FileContentsByDestination`: by destination, then by the name
  of the input. `FleetSettings.Validate` checks on the node that each
  destination has contents for exactly the credential files named by the
  destination. `FleetSettings.Validate` also checks each file's contents
  to confirm the destination module can have read the contents.
- **A sender asks `DeliveryOf.Credential` for a secret.**
  `DeliveryOf.Credential` returns the hub's copy on a joined node, and reads
  the file anew for each delivery everywhere else. Where `DeliveryOf.Credential`
  reads the credential file anew, replacing the secret at the credential
  file's existing path needs no reload. A joined node never opens a path of
  the hub on the joined node's disk.
- The credential files of one destination hold at most
  `credentialFilesLimit` bytes together.

## One conversation for each email attempt

`destination_email.go` owns the email destination: its settings
(`Email.Problem`), its identity (`sameEmail`), and one delivery attempt
(`EmailSender.Send`).

- One attempt is one conversation with the mail server, under the
  destination's `time_limit_per_delivery_attempt`. The sender closes the
  connection when the time limit passes or the daemon stops, which ends a
  command that waits for the mail server. A step that fails goes through
  `problem`. When the time limit has passed, `problem` reports the time
  limit and the step that the sender reached. Otherwise, `problem` reports
  what the mail server refused, with the mail server's answer. For an error
  without a mail server answer, `problem` reports the step that the sender
  reached.
- The sender names the sender address with `smtpMailFrom`, which asks the
  mail server for nothing more because every message line is ASCII. The
  `Mail` method of `net/smtp` would request `SMTPUTF8`, which some
  mail servers cannot pass on. The sender names every recipient before
  sending the message. A refusal is kept as an error, and accepted recipients
  still receive the message. A retry sends to all recipients again, so
  accepted recipients can get duplicates with the same event ID.
- `Email.Problem` requires the explicit `smtp_connection_security` setting.
  `Email.Problem` accepts an unencrypted login to any host. A private
  network can protect the unencrypted connection. Validation runs for
  `alert_destinations.yml` and a joined node's fleet settings
  (`FleetSettings.Validate`). The login methods use the configured
  connection security without imposing another policy.
- Each SMTP answer is one line (`smtpAnswer`). An attempt reports every refused
  recipient, and no error holds the password.

## Configuration defaults

`--config-dir` selects one complete configuration directory for every reader.
`mmwatchdog_node_settings.yml` holds node settings only. Every check group
comes from `check_groups/*.yml`. Other files in the check-group directory
are rejected. Duplicate names report the locations of the duplicate definitions.
At least one check group is required.

Node settings allow at most one of `fleet_hub_role` and `join_fleet_hub`.

Defaults fill only omitted settings. An authored value, including zero or an
empty list, is validated as written, and a setting that does not apply to a
check group is rejected. Write check settings on the check, with the
following restrictions on check group settings:

- A check group of named checks rejects `alert_severity`,
  `raise_to_sos_after_failing_for`, the two limit maps,
  `send_alerts_only_to_destinations`, and
  `no_alerts_or_recovery_actions_while_these_checks_fail`.
- A heartbeat check group rejects the timings, the limit maps, and
  `no_alerts_or_recovery_actions_while_these_checks_fail`.

Required settings and their scope are listed in the
[configuration files reference](../reference/configuration-files.md#node-settings). The loader refuses an
omitted required setting before deriving the check definitions. The loader
does not fill recovery budgets, fleet timings, queue retention, or reminder
choices.


A validation warning is a remark about a valid configuration. All validation
warnings come from one place.
`Config.ValidationWarnings(node)` returns every warning of a configuration
as one `ValidationWarnings` value, with one list for each kind of warning.
`mmwatchdog validate` obtains the `ValidationWarnings` value for the loaded
configuration. A reload returns the `ValidationWarnings` value for the
activated configuration in `ReloadResult`. The function
`validationWarningsText` in `cmd/mmwatchdog` formats the warnings for
`mmwatchdog validate` and `mmwatchdog reload`, so the commands print the
same text.

A new kind of warning requires another list in `ValidationWarnings` and
another block of text in `validationWarningsText`. No code path refuses a
configuration because of a warning.

`SecretFileAccess.ValidateSecretFile` checks only read bits.
`Config.SecretFilePermissionErrors` scans the credential files of every
owning block and returns each problematic file once, with the file's
configuration location. `ValidateRuntime` refuses the first problem, so
full validation and reload enforce the read-bit rule. Startup logs every
problem and continues. The heartbeat sender warns on standard error and
continues sending.

Runtime readers do not apply the permission check. A mode can change on a
running node; stopping checks, deliveries, check-ins, or heartbeats
then would drop alerts.

Public core certificate authority files hold no secrets. Destination
certificate authority files keep the 1 MiB certificate-file bound separate
from the 64 KiB total for secrets, including in fleet settings.

## Time is a parameter

State and lifecycle functions take `now` from their entry point instead of
calling `time.Now()`, so every transition can be tested with explicit
timestamps.

Every daemon entry point takes `now` from `Daemon.now`, the daemon's only
clock. MMWatchdog uses `time.Now`, while clock-jump tests use
`testhelpers.Clock`. A time from `Daemon.now` carries Go's monotonic reading
beside the wall reading. Go compares times on the monotonic clock when each
time carries a monotonic reading. So every wait counts on the monotonic
clock. Changing the system clock leaves each scheduled wait's remaining
duration unchanged ([the system clock](design/operation.md#the-system-clock)).

A time without a monotonic reading is compared on the wall clock. Go gives
no error or warning when a comparison uses the wall clock because a time
lacks a monotonic reading. Preserve the monotonic reading on every time
used by a wait:

- **No call drops the monotonic reading.** Never call `.UTC()`, `.Round(0)`,
  `.Truncate`, `.In`, or `.Local()` on a time that feeds a wait: each drops
  the monotonic reading. The daemon's process makes UTC its local time
  (`time.Local = time.UTC` in `cmd/mmwatchdog`, and in the `TestMain` of
  `internal/daemon`), so a time of `time.Now()` is in UTC with its reading.
  An observation takes its time from `CheckRun.Now`, which the daemon sets
  to its clock.
- **`Snapshot.Clone` copies field by field, never through JSON**, which
  keeps only the wall reading. `TestCloneCopiesEveryFieldAndSharesNothing`
  fills every field by reflection and fails for a field that Clone does not
  copy, or shares with the snapshot.
- **A loaded time is put on the monotonic clock once.** A time read back
  from storage has only a wall reading. Daemon start calls
  `Snapshot.OnThisClock` right after the load.
  A time from `fleet.db` that enters the snapshot, such as a node's last
  report, goes through `incidents.OnThisClock`.
  `TestOnThisClockPutsEveryLoadedTimeOnTheMonotonicClock` fails for a time of
  the snapshot that OnThisClock leaves out.
- **SQL decides no wait.** The outbox stores the next attempt time for
  status and the first wait after a start. The first wait lasts no longer
  than the doubling delay after the delivery's failed attempts
  (`store.DelayAfterFailedAttempts`). The stored next attempt time can be
  off by any amount after a start. The notifications worker decides in
  memory when the head of each queue is due (`attemptDue`, `afterAttempt`).
  A joined node's fallback decides when the oldest alert for the hub
  happened (`hubAlertHappened`). Retention counts back from
  `Daemon.retentionNow`, which is never later than the daemon's start time
  plus monotonic elapsed time. The store takes `Daemon.retentionNow` as
  `Store.RetentionNow` for the age limits of a commit.
- **The following comparisons stay on the wall clock.** Times from outside
  MMWatchdog have no monotonic reading:

  - The age of a file (`file`).
  - A certificate's expiry (`tls`).
  - The age of package lists (`apt_security_updates`), of dnf's package data
    (`dnf_security_updates`), and of zypper's repository metadata
    (`zypper_security_patches`).
  - The times of journal lines and the kernel's log (`log_lines`,
    `out_of_memory_kills`).
  - The times in another node's report: the whole-node window, the reported
    maintenance window's end, the node's clock offset, and a late alert's
    age when the hub delivers the alert.
  - MMHotline's `Date` header and a `Retry-After` date.
  - The age of a refused claim on a node's name in `mmwatchdog fleet`.

  The window of a `systemd_unit_crash_loop` check also uses the wall clock.
  The crash-loop times live in the module's state.
- **Never `==` on times, and never a time as a map key.** `==` compares the
  monotonic readings and the locations too. Use `Equal`, `Before`, and
  `After`.
- **Format a time only through `.UTC().Format` or `ClockTime`.** `String`
  and `%v` add the monotonic reading, such as `m=+120.0153`.

`TestEveryTimeOfTheSnapshotCarriesAMonotonicReading` checks the daemon's
state by reflection after a start, a tick, and a heartbeat receipt. Further
checks follow maintenance on and off, a reminder, a recovery command, a
restart, and reports on the hub. The test fails when a time used by a wait
has no monotonic reading. The checked locations are the snapshot, each
check's schedule, and each hub member's deadline.

## Fleet reporting

The [fleet](design/fleet.md) adds a reporter and an alert route
to every joined daemon, and a fleet store, alert intake, and fleet alert grouping to the hub.
The daemon rules above stay unchanged. The fleet store follows the daemon's
structure.

### Reporter

- **Read, then send.** The reporter copies the status summary and event tail
  under `Daemon.mu`, then releases `Daemon.mu`. The reporter then measures
  the node, encodes the report, and sends the report. HTTP requests and node
  measurements never hold `Daemon.mu`.
  The reporter never calls `Store.Save` directly.
- **One summary.** Reports, `mmwatchdog status`, and the TUI use the same
  daemon-produced summary; a report adds no health interpretation of its own.
- **Only the credential and the fleet settings are durable.** The reporter
  atomically writes the credential before using the credential, and
  atomically writes fleet settings before using fleet settings.
  The reporter writes the credential and fleet settings under `/var/lib/mmwatchdog`.
  Last success, last error, and the hub's two cursors, for events and for
  readings, stay in memory. A reporting failure is not an observation,
  except on the hub-outage alerter node, whose built-in `node_reports/to_hub` check
  takes each successful report as a receipt.
- **Monotonic event cursor.** Local event sequence numbers are never reused for
  the life of a database, so the events table uses `AUTOINCREMENT`. Each
  database also has a random `event_history_id`; a changed ID resets the hub's cursor
  for the reporting node.
- **One cursor exchange.** A report carries `events_after_sequence_number` from the hub's last
  reply, or none after a daemon start or a join, and the events after
  `events_after_sequence_number`. The hub accepts report events only when
  `events_after_sequence_number` matches the hub's cursor. Every reply
  returns the hub's cursor as `events_cursor`. A lost reply or restart
  therefore delays event transfer by one `report_every` interval, without
  duplicating events or creating a false gap. Sequence numbers missing
  between the cursor and the first event sent were removed by local
  retention and become a gap.
- **Readings follow the event-cursor exchange.** A report carries
  `readings_after_sequence_number` and the readings after
  `readings_after_sequence_number`, and every reply
  returns the hub's `readings_cursor`
  ([measurement history](#measurement-history)). Readings that local
  retention removed leave no gap event: the hub has no readings for the
  period removed by local retention.
- **A node never falls behind the hub's numbers.** After each accepted
  report the reporter calls `Store.ContinueEventsAfter` with the hub's
  `events_cursor`, and `MeasurementHistory.ContinueAfter` with the hub's
  `readings_cursor`. The hub calls `Store.ContinueEventsAfter` for the hub's
  own report. A restored `state.db` or measurement history can have a
  sequence counter below the hub's cursor.

  `Store.ContinueEventsAfter` advances SQLite's event counter in `state.db`
  to the hub's `events_cursor`, never backwards.
  `MeasurementHistory.ContinueAfter` advances SQLite's reading counter in
  the measurement history to the hub's `readings_cursor`, never backwards.
  The hub therefore accepts subsequent events and readings.
  After restoring an older database, records made between node startup and
  the first accepted report have sequence numbers the hub already holds, so
  those records do not reach the hub.
- **Sequence numbers from another machine have a bound.** A node continues
  its own numbers after the hub's, so a number near the largest integer
  would leave the node without a number for its next event. A node treats a
  reply with a number above `MaxSequenceNumber` as an invalid reply
  (`ReportReply.Valid`). `NodeStatusReport.Validate` refuses a report
  with a sequence number above `MaxSequenceNumber`, so a hub holds no
  sequence numbers above `MaxSequenceNumber`.
- **Old events on the hub are found through an index.** `Hub.pruneEvents`
  runs in the retention step under `Hub.Mu`. The SQL statement used by
  `Hub.pruneEvents` is `HubRemoveEventsReceivedBefore`.
  `HubRemoveEventsReceivedBefore` finds events by receipt time through the
  `events_received` index and reads no other event.
- **The hub reports to itself.** The hub's reporter hands its report to report
  acceptance in-process, so the hub is an ordinary entry in its own fleet.
- **A node's settings for the fleet travel with the join and with every
  report.** `NodeFleetSettings` holds the settings that the hub needs
  from a node: the severity and destinations of its silence alerts, the time
  after which those alerts are raised to `sos`, its node groups, and the node
  groups whose silence raises those alerts to `sos`.
  `Config.NodeFleetSettings` reads the `NodeFleetSettings` fields from the
  configuration. The join request and report carry the `NodeFleetSettings`
  fields under the settings' own names. `NodeFleetSettings.Problem` checks
  what another machine sent, for a join and for a report alike.

  Add any further node setting needed by the hub as a field of
  `NodeFleetSettings`. A report the hub cannot save counts only as contact
  (`Daemon.contact`). But the node settings in memory remain the settings
  from the latest saved report, which `startFleet` would read after a restart.
  A node names at most `MaxNodeGroupsOfANode` node groups, so that a join
  request of a valid configuration always fits `JoinLimit`.

### Alert route on a joined node

- **The hub is the one destination.** `AlertRoute` sends a joined node's
  alerts to a single outbox queue, `hub`, so the existing worker keeps their
  order, retries, and delivery gaps. The queue's identity never changes, so a
  new join code preserves the existing `hub` queue. The alert sender reads
  the reporter's credential file. Only the reporter joins.
- **One fallback rule.** The delivery worker's fallback step moves the hub
  queue's oldest alert, in one transaction, to each destination in the
  fleet settings copy, as on a standalone node. A reminder goes only to the
  destination named by the reminder. Each destination has a separate delivery.

  The fallback step moves the oldest alert after
  `send_alert_without_hub_after_waiting_for` has elapsed since the alert
  happened and the latest delivery attempt to the hub failed.
  If an earlier alert for the check is queued directly, fallback moves the
  oldest hub alert immediately, without requiring the wait or a failed
  attempt. The check's alerts therefore never pass each other.

  Before the delivery worker reads the queue, the fallback step moves alerts
  until no alert is due. A later alert for a check follows an earlier alert
  moved during the fallback step, instead of going to a hub that just returned.
  Each queued alert's wait starts when the alert happened. Fallback uses
  the latest delivery attempt to the hub, without requiring an attempt for
  each queued alert. Alerts behind the hub queue's oldest alert therefore
  avoid waiting `send_alert_without_hub_after_waiting_for` or a request
  timeout for each alert while the hub is down.

  Nothing falls back while an alert is in
  flight to the hub or before the node holds the settings. Whether the latest
  attempt failed is kept in memory. After a daemon start,
  one attempt must fail first.
- **Settings copy.** The hub reads its destinations and their token files when
  the hub starts and reloads, and adds the hub's fleet timings.
  The version fingerprints the destinations, token-file contents, and fleet timings.
  A joined node takes every fleet timing from copied fleet settings, or
  defaults before holding fleet settings. The hub is the source of the
  joined node's fleet timings.

  When a report reply names a version the node does not hold, the node
  fetches the fleet settings.
  The node writes the fleet settings atomically under the joined node's
  state directory, then activates the fleet settings as a control commit.
  Destination identity rules apply to the copy exactly as to a standalone
  node's `alert_destinations.yml`: the commit cancels queued deliveries to a changed
  destination.
- **One source.** A daemon with `join_fleet_hub` refuses to start or reload while
  `alert_destinations.yml` exists in its selected configuration directory.
- **The node rejoins after a hub or node-name change.** The credential
  records the hub certificate fingerprint and node name for which the
  credential was issued. Before each report, the node compares the
  credential's fingerprint and node name with the configured join code's
  fingerprint and the configured node name. A difference discards the
  credential and joins with the configured code. Rotating only the join
  secret preserves the existing node credential. A credential the hub
  refuses is also discarded before rejoining with the configured code.
  A hub whose certificate does not match the code's fingerprint is
  unreachable, so alerts fall back.

### Hub entry points

| Entry point | Store | Kind |
| --- | --- | --- |
| Hub start (load nodes and latest reports) | `fleet.db`, then `measurement_history_of_nodes.db` | control |
| Join, including replacement | `fleet.db` | control |
| Report acceptance, including hub-outage alerter choice | `fleet.db`, then `measurement_history_of_nodes.db` | control |
| Join-code rotation | `fleet.db` | control |
| Forget | `fleet.db`, then `measurement_history_of_nodes.db` | control |
| Retention tick | `fleet.db` and `measurement_history_of_nodes.db` | record |
| Alert acceptance | `state.db` intake | outbox |
| Alert grouping tick | `state.db` intake and outbox | outbox |
| Fleet incident set step | `state.db` fleet incident sets and outbox | outbox |
| Fleet query (CLI and daemon status) | `fleet.db`, `measurement_history_of_nodes.db`, `state.db`, and `measurement_history.db` | read-only |

### Fleet store commits

- **Separate store, one direction.** `fleet.db` has its own lock and
  connection, so report traffic never contends with `Daemon.mu` or `state.db`.
  `Hub.Mu` and `Daemon.mu` are never held together.
  Membership flows one way: the fleet store commits a join, replacement, or
  forget first, then the daemon applies it as a record entry point that creates
  or retires the affected node's check alone. Daemon start reconciles
  `node_reports` checks with the store's membership. Because startup repeats
  reconciliation, membership changes during operation wait for the next
  scheduler tick to commit. A join needs no separate daemon commit.
- **Commit before reply.** Each join and report commits in its own
  transaction, then updates memory and replies; a failed commit replies `503`.
  One transaction per report sustained the 10,000-node load run, so reports
  are not batched.
- **Identity from the credential only.** The handler resolves the credential's
  hash to a node name before reading the body; the node's name comes from
  nothing else. The join secret is compared in constant time.
- **One replacement test.** A join may take an existing name only if the
  existing node is `silent`. Otherwise the join is refused, and a bounded
  conflict record keeps the latest claimant's address and time.
- **One newer-report test.** Within a `daemon_start_id`, `report_sequence_number` orders
  reports. A new start ID replaces the stored report, and the store refuses the
  start ID it replaced most recently, so a request delayed across a daemon
  restart cannot overwrite the new daemon's state. Each request is bounded to
  10 seconds end to end.
- **Two hub-outage alerters.** The hub keeps a ranked list of at most two reporting
  nodes other than the hub: preferred nodes first, then current hub-outage alerters,
  then the longest continuous reporting. Equal ranks preserve the current
  hub-outage alerter selection.

  `hub_watcher_nodes(name TEXT PRIMARY KEY)` in `fleet.db` stores the set; the table keeps its name from before the rename to hub-outage alerters, so an upgraded hub keeps its choice.
  A changed hub-outage alerter set is saved in the transaction accepting the report
  that triggered selection, or forgetting the hub-outage alerter that was removed.
  Report acceptance ranks nodes only when a place is open, a hub-outage alerter is
  gone or no longer reporting, a preferred node reports while some hub-outage alerter
  is not preferred, or a hub-outage alerter drops its preference.

  Preference and the start of continuous reporting are receipt facts,
  like the receipt time.
  Each reply tells its node whether it is a hub-outage alerter. Forgetting a hub-outage alerter
  removes it; the next report acceptance fills the place.
- **Forget deletes, rotation keeps credentials.** A forget deletes the node's
  entry and events in one transaction, then drops its credential from memory;
  the daemon removes the membership only if the forgotten node's join time
  still matches, so a join committed meanwhile stays. Join-code rotation
  replaces only the secret in the identity row.
  Credentials and the certificate are unchanged, so joined nodes keep
  reporting and only a join needs the newly rotated join code.
- **One answer to what a node's settings are.** `member.fleetSettings`
  returns the settings of the node's latest saved report, or the settings
  sent when the node joined before the first report. At daemon start,
  `Hub.Members` copies the settings returned by `member.fleetSettings` into
  each daemon member (`FleetMember`). The silence check takes the check's
  severity, raise time, and named destinations from the daemon's
  `FleetMember` copy. Afterwards `Daemon.joined` and `Daemon.receipt` apply
  the settings of each join and report as the join or report arrives.

  The fleet view reads node groups through `member.fleetSettings`
  (`Hub.View`). The raise by node group selects nodes whose saved settings
  name the node group, as the fleet view does. But the raise reads the
  daemon's member copy because the raise runs under `Daemon.mu`.
  The fleet store's lock and `Daemon.mu` are never held together.
  A silent node therefore retains the last saved silence settings and node
  groups, including across a hub restart.

  The hub keeps no list of node groups. On each request,
  `Hub.FleetOfNodeGroup` collects node-group names from the nodes and selects
  only nodes of the requested node group. An unknown node-group name
  produces an error listing the existing node groups.
- **A restored hub replaces its former self entry.** Restoring state under a
  different node name removes the old credentialless hub entry and its events
  before adding the new self entry. The old credentialless hub entry cannot
  become a monitored fleet member.
- **Bounded storage.** The store keeps the latest report per node, a
  per-node event tail capped by bytes and age, and a database
  ceiling. Refused joins update counters, never a row per unknown sender.
  The readings of the nodes are in a file of their own, bounded by its own
  time and size ([measurement history](#measurement-history)).

### Silence is a heartbeat created by joining

- Each silence check takes `SendAlertsOnlyToDestinations` from its node's
  `send_silence_alerts_of_this_node_only_to_destinations`. On a hub-outage alerter,
  the fleet settings carry the hub's list as
  `send_silence_alerts_of_the_hub_only_to_destinations`. The fleet settings
  version changes with the hub's destination list.
  The hub's alerts about silent nodes and the hub-outage alerter's alerts about the hub
  use existing check alert routing, including recorded recipients for
  recoveries. Silence-alert routing preserves an explicitly empty list.
  The hub validates destination-name syntax in joins and reports, and its
  status shows missing names on each `node_reports/NODE` check.

- Fleet checks are the definitions that membership adds to the
  configuration's: on the hub `node_reports/NAME` per joined node, identified by
  when it joined so a replacement retires the old check; on the hub-outage alerter
  `node_reports/to_hub`. The hub's per-node checks and the hub-outage alerter's
  `node_reports/to_hub` check reconcile like ordinary check definitions.
  The hub-outage alerter keeps the role across restarts while its
  state holds `node_reports/to_hub`. A fleet check's deadline period is not
  part of the check's identity. A changed fleet timing therefore preserves
  the fleet check's state and applies from the next receipt.
- A fleet check's alert severity settings do not come from the
  configuration of the daemon running the check. On the hub, the check's
  alert severity settings come from the joined node
  (`NodeFleetSettings.Silence`).
  The definition has `SeverityFromReports`, so reconciling never overwrites
  the reported severity settings.
  On the hub-outage alerter, the check's alert severity settings are the hub's.
  The hub's severity settings reach the hub-outage alerter with the fleet settings as
  `alert_severity_when_the_hub_goes_silent` and
  `raise_to_sos_after_the_hub_is_silent_for`.
- A fleet check's deadline is its last receipt plus three times
  `report_every` on the hub, or `alert_after_hub_unreachable_for` on the
  hub-outage alerter. Deadlines live in memory only. Daemon start sets every deadline
  to start plus three times `report_every` on the hub, or start plus
  `alert_after_hub_unreachable_for` on the hub-outage alerter. A restart therefore never
  manufactures alerts. Hub
  member receipts come from fleet.db; the node snapshot leaves them out.
  The hub-outage alerter keeps its receipt in its snapshot for raises after restart;
  successful receipts touch the hub-outage alerter's one check row.
- The fleet view, name-replacement test, and hub-outage alerter selection use the
  receipt-based window. Each member keeps its current deadline when the report
  interval changes; the new interval applies at its next receipt. A report
  saved before hub startup is last-known state. The hub waits to receive a
  report from the member during the current hub run before classifying the
  member as reporting.
- An authenticated current report is a receipt even when the fleet store cannot
  commit it, and it is handed to the daemon after the fleet store releases its
  lock. A receipt applies an observation only when it changes the check's
  state, and its transitions commit with the scheduler's next tick, so
  hundreds of reports per second, or a thousand nodes returning at once,
  share a few daemon commits.
- The scheduler tick finds missed deadlines without queueing fleet checks,
  which execute nothing. Fleet checks produce ordinary alerts.
  The hub's silence alerts pass through fleet alert grouping keyed by
  change alone, because the check names the node.
  When the deadline for a successful report to the hub passes, the hub-outage alerter's
  silence alerts go directly to the hub-outage alerter's copied destinations.
  The silence alerts bypass the usual hub queue and fallback delay.

### Alert intake and fleet alert grouping

- **Saved once, by event ID.** Alert acceptance saves the alert to the hub's
  `intake` table under its node and original event ID before replying, and
  the node's name comes from its credential. A repeated combination of node
  name and event ID is acknowledged without a second entry.
  A retry after a lost reply therefore creates no duplicate through the hub.
  A delivered alert keeps only the alert's ID until one hour after intake
  receipt. An alert beyond the intake limits, or an alert that cannot be
  saved, gets `503`, so the alert falls back.
- **One alert grouping step.** Every alert the hub delivers, from nodes or
  the hub itself, passes through the alert grouping step.
  The hub's own commits queue
  alerts in `intake` too.
  `AlertGroupingState.Plan` decides from the queued rows alone and `Store.Emit`
  commits its messages to the outbox, in order, with the rows' new states in
  one transaction, under `Daemon.mu` on the delivery worker's tick. An alert
  goes out at once unless an alert with the matching grouping key went out
  in the last minute. Otherwise the alert joins the single combined
  message sent one minute after the previous send for the grouping key.

  The grouping key is the check group, check, change, and severity group.
  The severity group is the severity and, for a recovery, the severities
  sent for the recovery's incident (`Event.severityGroup`). The hub's silence
  alerts use the check group, change, and severity group. For a reminder,
  the key also includes the named destination.
  A reminder's message goes only to the destination named by the reminder.

  A combined message names up to 20 nodes and counts the rest.
  A single alert keeps the original event ID.
- **Order survives fleet alert grouping.** An alert waits while an earlier
  alert for the node and check is still queued. Neither a combined message
  nor a single message passes an earlier alert for the node and check.
- **Maintenance groups.** A maintenance group combines maintenance alerts
  with matching reason and user. A maintenance group opens with one message
  for the starts queued when the maintenance group opens. The start alerts
  stay held while the maintenance group remains open. Later starts join
  quietly, and end alerts wait in the maintenance group.

  The maintenance group's end is announced, naming the nodes that ended,
  after ten minutes with no matching current window in any node's latest
  report. The current windows are read from the fleet store before
  `Daemon.mu` is taken. A deploy therefore produces one start and one end
  message. An end can follow the alerts that
  reconciliation after the deploy produced.
- **Memory stores send and maintenance-group timers only.** The time each
  grouping key last sent and each maintenance group's quiet start are in
  memory. A hub restart forgets the last-send and quiet-start times.
  Forgetting the timers can only send a message sooner or end a maintenance
  group later. Queued and held alerts are durable.
- **Late alerts include the latest reported check state.** A check alert
  delivered more than two minutes late includes each named node's current
  state for the check. The hub adds the check state from the latest report
  when sending the alert. A maintenance end, always late because the
  grouping holds it ten minutes,
  carries when the maintenance ended and why the end alert comes later.
  A maintenance group's end happened when the maintenance group's last
  node's maintenance ended (`Combine`).
- **One key for an incident across the fleet.** `CheckState.Event`, the
  one place that makes an incident's events and knows the track of each,
  gives every failure, cannot-check alert, raise, reminder, and closing
  alert its `fleet_incident_key`: `checksandalerts.FleetIncidentKey` of the
  readable key (`ReadableFleetIncidentKey`). `Store.Save` scopes the key
  with `ScopeFleetIncidentKey`, using the hub certificate fingerprint or
  the standalone node name.

  `Store.FleetIdentity` changes when the daemon
  activates its hub or join code. Only the hub knows that a check of
  `node_reports` names a silent node. So `Store.Save` on the hub gives all
  hub-generated node-silence incident alerts a shared key.
  `Combine` keeps the key shared by the alerts in a grouped message.
  The hub accepts a node's incident alert only with a valid incident key.
  A non-incident alert must contain no incident key
  (`BelongsToAnIncident`, `FleetIncidentKeyPattern`).
- **One fleet incident set for each destination that keeps incidents**
  (`fleetincidentset.go` in `store`, `fleet`, and `daemon`). The `mmhotline`
  destination keeps a fleet incident set, and so does every destination
  whose module's description states `service_keeps_incidents`
  (`Destination.ServiceKeepsIncidents`). `IncidentSet` names a set by its
  destination, and for the `mmhotline` destination by its alert source too.

  The table `fleet_incident_set` in `state.db` holds a row for each
  MMWatchdog incident in a set, by readable key: open, closed, or resolved.
  Each row keeps the highest alert severity the incident reached.
  For each key, `fleet_incident_keys` keeps the severities sent since the
  key's last closing alert and the time of the closing alert.
  - **Alerts update rows and queue closing alerts.** `QueueDirect` hands an
    alert to the destination's incident set in the transaction queuing the
    alert. `QueueMMHotline` handles the `mmhotline` destination, and
    `queueWithIncidentSet` handles other destinations that keep incidents.
    A failure or raise opens members' rows or raises the rows' severity
    (`openIncidents`). Members whose rows are already closed or resolved
    are not sent. A reminder goes out for members whose rows are not closed
    or resolved (`notOver`). A recovery or retirement closes the members'
    rows (`closeIncidents`). The recovery or retirement then closes each
    affected key with no open row and no failure still waiting in `intake`,
    excluding the message's own members (`closeKeys`).

    A destination that keeps incidents then gets one closing alert for the
    key (`closingAlert`). When all closed rows belong to a single node,
    `closingAlert` reuses the node's closing alert if present in the message
    being processed. Otherwise, a new event carries the key, node names,
    and text from `WriteFleetIncidentClosingTexts`.
    The reused node alert or new fleet alert carries the severities sent
    to the destination for the key.

    The `mmhotline` destination instead gets an MMHotline resolution.
    MMHotline alone also gets `resolved-sos` as soon as a key previously
    sent at `sos` still has open rows, but no open row remains at `sos`.
    The `resolved-sos` marks the key's closed rows resolved and leaves the
    key's sent severities at the highest severity of the key's open rows.
  - **Reports reconcile incident rows and close fleet keys.** Report
    acceptance records announced incidents and incidents sent without the
    hub in fleet-store memory. The delivery worker's tick takes the reported
    incidents before acquiring `Daemon.mu`. `ReconcileIncidentSets` then
    applies the reported incidents to every incident set in one transaction.
    An unknown announced incident opens a row with the severities accepted
    by the set's destination (`severitiesSentTo`, by `GoesTo`).

    An unknown incident sent without the hub to the destination gets a
    closed row. A resolved row of an incident whose direct send the hub
    has not settled in the reported version is replaced by a closed row,
    so the send gets its own closing alert, with or without an exact
    finding.
    Reported severities that the destination takes raise a row and the
    row's key. A row closes when a newer node report
    no longer shows the incident, or when the node is forgotten.

    When an open row joins a key whose last closing alert followed the
    incident's opening, `ReconcileIncidentSets` queues the incident's failure
    again from the report. The earlier closing alert closed the shared
    fleet incident key in the destination's service.
    A report sent before a row opened never closes the row.
    A resolved row stays until a newer report no longer carries the
    incident, or a day.
    While the resolved row remains, an older report cannot open the
    incident again.

    Reports `ReconcileIncidentSets` could not apply wait for the next tick.
    Hub start treats a reporter the fleet store no longer lists as
    forgotten, so a forget is never lost.
  - **After a hub start.** No key closes while the hub awaits a joined
    node's first report since the later of hub startup and the node's join.
    The wait lasts at most three report intervals
    (`fleet.Hub.AwaitingFirstReports`). `ReconcileIncidentSets` and the
    `QueueDirect` alert path close rows only
    (`QueuedBy.AwaitingFirstReports`, read before
    `Daemon.mu`). After the initial wait, the next `ReconcileIncidentSets`
    execution closes every key left with only closed rows.
  - **Which sets exist.** Every commit deletes the rows of a set that the
    destinations in force do not keep (`keepOnlyIncidentSets`): a
    destination that is gone or no longer keeps incidents, and the
    `mmhotline` destination's rows of another alert source. A standalone
    node keeps an incident set for each local destination whose service
    keeps incidents, using the standalone node's own alerts.
  - **A joined node** keeps no set and never queues a closing alert for
    a destination whose service keeps incidents. The joined node takes the
    destinations that keep incidents from the fleet settings
    (`FleetSettings.DestinationsWhoseServiceKeepsIncidents`,
    `QueuedBy.DestinationsWhoseServiceKeepsIncidents`). The hub builds the
    destination list from the hub's modules, never from the joined node's
    module copies.
    The joined node records each failure or raise sent through fallback to
    a destination whose service keeps incidents once the destination
    accepts it: the queued delivery carries the record
    (`outbox.sent_without_the_hub`), `Store.Pending` hands it to the
    in-flight delivery, and `Store.Delivered` writes it in the transaction
    that removes the delivery. Retention can remove the row during the
    attempt: the accepted send is still recorded, and only a row that
    still matches the delivery's sequence number, destination, and event
    ID is removed. The record in
    `incidents_sent_without_the_hub` includes the destination and every
    severity sent, without age or count expiry. A reply's acknowledgment
    removes a record only while it still equals the version that the
    acknowledged report carried.

    The joined node's reports carry the fallback incident records until a
    report reply lists the records in `incidents_known_to_the_hub`.
    The hub acknowledges a fallback incident record only when
    `fleet_direct_sends_settled` holds the version that the report carried
    (`directSendVersion`), for every key, so a hub restart or failed
    `ReconcileIncidentSets` transaction loses no fallback incident records.

### Planned maintenance

- Maintenance events take a joined node's alert route like any other alert.
- A `node_reports` check is suppressed during a `node_reports` or
  `every_check_group` window on the daemon running the check.
  The last reported whole-node window of the node named by the check also
  suppresses the check while the window lasts.

  A reported window ends without an event, so the scheduler tick applies
  the announce and close rules to fleet checks. Once nothing suppresses a
  fleet check, the scheduler announces an open incident only when the
  fleet check's current deadline has been reached. The scheduler also closes
  an announced incident that resolved during suppression. Heartbeat
  reconciliation at a window's end leaves fleet checks to the scheduler tick.

### Reports and alerts are untrusted input

Decoding is strict and size-limited: a report to `ReportLimit`, 4 MiB, an
alert to `AlertLimit`, 64 KiB, and a join request to `JoinLimit`, 64 KiB. A
join comes from a sender that has no credential yet, so its limit is small,
and it still lets a node with hundreds of node groups join.

Report readings are checked like the result of a module that is not core
(`RunReadings.Validate`).

Text output
replaces control characters in
every string a node sent, so a node cannot write terminal escapes to an
operator's screen; JSON output relies on JSON escaping. The hub never connects
to a node.
