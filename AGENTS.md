# MMWatchdog development

**Read the [documentation map](docs/changing-mmwatchdog/documentation-map.md) before you change
behavior, a name that users read, or a document.** The documentation map says
which document owns each kind of fact, how module pages are built, and what
else to update when something changes. Keep each fact in the document that
owns it.

The acceptance contracts live in [docs/changing-mmwatchdog/acceptance.md](docs/changing-mmwatchdog/acceptance.md), not
in the README. Use the acceptance contracts and the
[acceptance checks](docs/changing-mmwatchdog/acceptance.md#standalone-acceptance-checks) to
decide when work is complete. For review, use the acceptance contracts'
typical deployment and correctness-blocker criteria.

The [release plan](docs/changing-mmwatchdog/design/README.md) owns the
[product principles](docs/changing-mmwatchdog/design/README.md#product-principles), the status
of each feature, and the remaining work. Update the release plan's status
table when a feature lands.
The [design documents](docs/changing-mmwatchdog/design/README.md#design-map)
specify the complete release-1 product. [How MMWatchdog works](docs/how-it-works.md) and
the [module pages](docs/reference/modules/README.md) describe the built behavior and
its limits, and [How MMWatchdog works](docs/how-it-works.md) explains the
parts of MMWatchdog and how they work together. Daemon changes follow the
[implementation rules](docs/changing-mmwatchdog/implementation-rules.md).

## Public terminology

The table below is MMWatchdog's glossary. Use the glossary's terms exactly
in configuration, CLI output, JSON, and documentation. Preserve standard
hostname, HTTP Host, SSH host key, and systemd restart vocabulary.
Distinguish an attempted recovery action from confirmed recovery, and an
observation from an incident, event, or delivered alert notification. The
[how MMWatchdog works](docs/how-it-works.md) introduces the glossary's terms
for users.

| Term | Meaning |
|---|---|
| node | A monitored machine. |
| check group | A named configuration container (`check_groups`) for a monitored object, such as an application. The check group states the timings of the check group's checks and recovery. |
| check | An independently observed rule inside a check group (`checks`). |
| standalone node | A node that runs MMWatchdog without a fleet hub. |
| joined node | A node that reports to a fleet hub. |
| node monitoring process | The one systemd-managed `mmwatchdog` process on each node. The node monitoring process owns configuration, observations, incidents, maintenance, recovery attempts, the event history, and alert delivery. |
| node check worker | A goroutine inside the node monitoring process that performs one bounded run of a check and returns an observation. |
| fleet hub role | The optional role that `fleet_hub_role:` enables in one node monitoring process: it accepts node status reports, holds the fleet's alert settings, notices silent nodes, and delivers fleet alerts. |
| fleet settings | The settings that joined nodes copy from the hub: alert destinations and the destinations' credential file contents. Fleet settings also include the destinations whose services keep incidents, fleet timings, the hub's silence severity, the hub's raise to `sos`, and the destinations for the hub's silence alerts. |
| hub-outage alerter node | A joined node that the fleet hub selects to send a direct alert when the hub stays unreachable. The hub selects two joined nodes from the fleet. In a fleet of two nodes including the hub, the hub selects the only joined node. |
| node status report | The snapshot of its current monitoring status that each joined node sends to the hub, with a bounded batch of its events and readings. |
| fleet alert grouping | The hub sends the first matching alert at once and combines nearby matching alerts from several nodes into one message. For a finding with an exact finding, the hub sends one alert per exact finding across the fleet, and later nodes in updates on the shared tick. |
| shared tick | The times at which the hub sends the updates about nodes newly affected by an exact finding already announced: the multiples of `send_newly_affected_nodes_every` since 1970-01-01 00:00 UTC, such as 00:00, 06:00, 12:00, and 18:00 UTC with the default `6h`, the same for every key. |
| run | One execution of a check. A run produces one observation. |
| observation | The result of one run: `ok`, `failed`, or `unknown`, with its time and evidence. |
| failing time | The time from a check's first failed run until the check has been healthy for `recovered_after_healthy_for`. |
| incident | A problem that has lasted `alert_after_failing_for`, with an identity and a lifecycle that opens and resolves. An observation is not an incident. |
| event | A recorded occurrence, such as an incident that opens or resolves. |
| alert notification | The delivery of an event to an alert destination. An alert notification can be queued, retried, delivered, or fail, independently of the monitored service. |
| recovery action | An attempted restart of a systemd unit or a recovery command. A successful recovery action does not prove that the problem is fixed. |
| recovery | Confirmed health: the check has been healthy for `recovered_after_healthy_for`, which ends the failing time and resolves an open failure incident. |
| keep a check quiet | What MMWatchdog does while a check named in the quiet check's `no_alerts_or_recovery_actions_while_these_checks_fail` is in the named check's failing time: MMWatchdog sends no alert of the quiet check and takes no recovery action because of the quiet check. |
| quiet time | The time while MMWatchdog keeps a check quiet. Quiet time ends with the quiet check's own first run after the failing time of the check that the quiet check names. |
| measurement | A named number that a check produces, such as `memory_used_percent`. |
| reading | One value of a measurement at one time. |
| measurement history | Where a node keeps the readings of every scheduled run, in a file of its own, for a time and up to a size that the node's settings state. |
| measurement history on the hub | Where a hub keeps the readings that its joined nodes send. Settings, files, and JSON call the hub's measurement history the measurement history of nodes (`measurement_history_of_nodes`). |
| node group | A named selection of nodes (`this_node_is_in_node_groups`). Never write the bare word "group", because "group" could mean a check group or a node group. |
| validation warning | A remark about a valid configuration that will probably not do what its author expects. `mmwatchdog validate` and `mmwatchdog reload` print validation warnings. A validation warning never makes the configuration invalid. |
| validation error | A fault that makes the configuration invalid. |
| module | The code that does the work of one check or of one alert destination. A check or a destination names exactly one module, by the module's name, such as `http` or `slack`. |
| inputs | What a check or a destination writes below the name of its module. |
| kind | Where a module comes from: core, official extra, or the project's own. Never write "kind" for the module of a check. |
| core module | A module that is built into the `mmwatchdog` program. |
| official extra module | A Starlark script that the MMWatchdog project writes and keeps in `official_extra_modules/` of the MMWatchdog repository and of every release. |
| module of the project's own | A Starlark script or an executable that the people who run MMWatchdog write, in `mmwatchdog_external_modules/` of their deployment project. |
| destination module | A module that delivers alerts. |
| primitive | One of the functions through which every module reads the node, such as `read_file`, `run_command`, and `http_request`. |
| module description | What a module's inputs, measurements, and examples are. A core module's description is built into the `mmwatchdog` program. Every module that is not core has the file `module_description.yml`. |
| finding | One of several things that one run of a check reports as failed, such as one failed systemd unit. Each finding is a separate incident. |
| exact finding | One line, beside a finding's `id`, that names the finding's problem precisely, such as the package, its installed version, and the advisory. Two nodes with the same exact finding have the same problem. Only a module supplies an exact finding. |
| set-aside entry | An entry of the hub's `set_aside_findings.yml` that sets one exact finding of one check's finding aside after an investigation: the hub sends no alert below `sos` for a node incident with that exact finding. |
| controller machine | MMX's term for the machine where a person runs `mmx`, such as a laptop. MMWatchdog runs nothing on the controller machine. The `mmwatchdog` file in a deployment project is the Linux program for the nodes. |
| title | The one short line of an alert. |
| body | Everything of an alert except its title. |
| notice | An alert event without an incident, such as `rebooted` or one sent with `mmwatchdog send-notice`. |

## Reference projects

Reference projects inform our understanding of problems and possible
solutions. MMWatchdog's specification, configuration schema, command syntax, and
implementation are developed independently.

- [Inspeqtor](https://github.com/mperham/inspeqtor): its Go source may be read
  to understand its approach to checks and operating-system interaction. Use
  Inspeqtor's Go source for ideas only. Do not copy, adapt, translate,
  vendor, or link Inspeqtor's code into MMWatchdog, including its tests and
  examples. Implement against our requirements and the underlying system
  interfaces.

  Inspeqtor's documented
  [design](https://github.com/mperham/inspeqtor/wiki/Design),
  [deployment handling](https://github.com/mperham/inspeqtor/wiki/Deployments),
  and [recurring jobs](https://github.com/mperham/inspeqtor/wiki/Recurring-Jobs)
  provide ideas for init-system ownership, expiring maintenance, and expected
  successful-job reports.
- [Monit](https://mmonit.com/monit/) and [M/Monit](https://mmonit.com/): use
  their documentation to study behavior and operating workflows. Monit's
  documented
  [service checks](https://mmonit.com/monit/documentation/monit.html#Service-checks),
  [test actions](https://mmonit.com/monit/documentation/monit.html#ACTION), and
  [fault tolerance](https://mmonit.com/monit/documentation/monit.html#FAULT-TOLERANCE)
  are the primary behavioral references for the standalone design. Monit's
  and M/Monit's source code is not an implementation reference for MMWatchdog.
- Do not copy or adopt Inspeqtor's, Monit's, or M/Monit's specifications or
  configuration languages. MMWatchdog owns its scope, behavior, schema, and
  syntax. Examples and tests are written for MMWatchdog's design.

The [MMX product principles](https://github.com/monkeymaxxing/mmx/blob/main/docs/changing-mmx/design/README.md#product-principles)
provide the shared direction: a clear happy path, AI authors and human
readers, and a deliberately small core.

## Compatibility between releases

**MMWatchdog is released (since 0.1.0). Every change keeps the promises in
[Compatibility between releases](docs/changing-mmwatchdog/design/README.md#compatibility-between-releases).**

- A patch release, such as 0.1.1, makes no incompatible change. An
  incompatible change makes the next release a minor release, such as 0.2.0:
  a configuration file that no longer validates or behaves as before, a
  renamed or removed command, flag, or result field, or a state file that the
  new release cannot read.
- A state file that the new release cannot read needs a migration, or a
  documented reset that says what the user loses.
- The release notes of each release record every incompatible change, under
  "Changes you must act on", with the edit or the step that a user needs.
  [Release MMWatchdog](docs/changing-mmwatchdog/releasing.md) says how to
  write that section.
- A fleet hub accepts the node status reports of nodes one minor release
  older than the hub. Keep those report formats readable for one minor
  release.

Within the compatibility promises above:

- Implement the current agreed design directly. Delete superseded code and
  replace or remove tests that assert discarded behavior.
- Do not add backward-compatibility layers, legacy-state readers, migrations,
  deprecation paths, or historical-format fixtures beyond the promises above.
  Add a compatibility requirement only for a concrete need explicitly agreed
  with the user.
- Test current requirements and meaningful failure cases. Do not add tests
  merely to freeze existing behavior or preserve the history of a bug.
- Reset disposable test state when its format changes. Do not build migration
  machinery for test data.
- Keep verification proportional to the change.
- Update implementation, tests, and owning documentation together so they
  describe one current design.
- Documents describe the current build only. Delete completed plans, fixed
  findings, superseded results, and "we found X, so now Y" narratives; Git
  history keeps them.

Follow [docs/changing-mmwatchdog/testing.md](docs/changing-mmwatchdog/testing.md) for relevant test commands, short
fixture timings, and ownership and locking of MMWatchdog's isolated test environments.
[Test in levels](docs/changing-mmwatchdog/testing.md#test-in-levels): while iterating, run the
tests of the Go packages changed by the work. After an ordinary change,
run the affected VM suites on Debian 13 and Ubuntu 26.04. At a stage's end,
run the stage's new and changed suites once on every target distribution.
Every suite runs on every target distribution only before a release and
after changes to the test harness or base VMs. A test never depends on the
machine's speed.
Even when a rerun passes, find and fix the cause of the test failure at once.
See
[tests that never depend on the machine's speed](docs/changing-mmwatchdog/testing.md#tests-that-never-depend-on-the-machines-speed).
