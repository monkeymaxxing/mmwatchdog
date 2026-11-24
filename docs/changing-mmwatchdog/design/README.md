# MMWatchdog: release plan

The release plan holds MMWatchdog's product principles, the requirements of
the Monkeymaxxing course, and all of MMWatchdog's status: what release 1
contains, what is built, and the ordered work to finish it. The design
documents describe the complete release-1 product without status. The
[README](../../../README.md) says what MMWatchdog is, and the
[acceptance contracts](../acceptance.md) say what MMWatchdog must get right.

## Where we are

**MMWatchdog 0.1.0 is released.** Every change keeps the
[compatibility rules](../../../AGENTS.md#compatibility-between-releases):
a patch release changes nothing that a user must act on, and a minor
release's notes list each change that a user must act on.

The standalone daemon, CLI, checks, recovery, webhook alerts, heartbeat
checks that expect reports of successful work, the fleet hub, alert severity,
the measurement history on a node and on the hub,
the three kinds of [module](checks.md#modules), and the connection to
MMHotline are built and validated, except for pending protocol VM verification
listed in the status table ([validation record](../validation.md)).
The connection is tested against a fake MMHotline built from MMHotline's
alert intake contract, and against a real MMHotline by the real service
test, whose SOS woke a person through Pushover. The suites run on each of the nine
[target distributions](../testing.md#every-target-distribution): Debian 12
and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10, Fedora 45, Arch
Linux, and openSUSE Leap 16.1.

**The design documents are ahead of the build.** The design documents describe
the planned release 1. The build uses the setting names and forms
in the design documents. The [status table](#status) names each part that is
not built, and the
[remaining work](#remaining-work) orders it. The guides and
the [module pages](../../reference/modules/README.md) describe what is built today, and
the [documentation map](../documentation-map.md) says which document owns
which kind of fact.

## Release 1

Release 1 is a deployable Linux/systemd monitor that replaces Monit for most
users. For fleets, release 1 also replaces M/Monit for most users.
Release 1 provides the checks, recovery, alerts, maintenance, fleet features,
and inspection in the design documents. MMX deploys release 1 as versioned
ARM64 and AMD64 bundles.

A check that many servers need is a core
[module](checks.md#modules), built into MMWatchdog: the node, systemd, files, HTTP,
and the common protocols and server software, such as SSH, PostgreSQL, and
Redis. An uncommon check is an
[official extra module](checks.md#official-extra-modules), a script that
ships with MMWatchdog. An alert destination that one company runs, such as
Slack, is also an official extra module
([where a module belongs](checks.md#where-a-module-belongs)).

MMWatchdog watches Linux servers on amd64 or arm64. MMWatchdog is built
around systemd, which nearly every current Linux distribution uses.
MMWatchdog is tested on the
[target distributions](../testing.md#every-target-distribution), and each
[module page](../../reference/modules/README.md) lists under "Tested on" the
distributions where the module is tested. MMX, which deploys MMWatchdog, is
tested on the same distributions; a server MMX can manage but MMWatchdog
cannot watch would leave a gap.

On a distribution that runs systemd but is not a target distribution,
MMWatchdog is expected to work, because its checks of the node, of systemd,
and of network services use the same mechanisms there, but MMWatchdog is not
tested there. The checks that know only some distribution families report
`unknown` there, with their reason.

MMWatchdog does not watch servers that systemd does not run, such as Alpine
Linux with OpenRC. This is a deliberate choice of scope: MMWatchdog aims to
serve the common case well, not every server. Its daemon runs as the
systemd service `mmwatchdog.service`, and its checks of systemd units, its
restarts of systemd units, and its checks of the journal rest on systemd. On
a server that systemd does not run, `mmwatchdog daemon` refuses to start
with a message that says so. For such servers, use another monitoring tool.

Most checks work the same on every distribution. Some know only some
distribution families and are unknown elsewhere, with their reason:
`apt_security_updates` knows Debian and Ubuntu, `dnf_security_updates`
knows the Red Hat family, Fedora included, `zypper_security_patches` knows SUSE,
`reboot_after_updates` and `distribution_release_security_support` know
Debian, Ubuntu, the Red Hat family, and SUSE,
`known_exploited_vulnerabilities_in_installed_packages` knows Debian and
Ubuntu, and `ubuntu_pro_security_fixes` knows Ubuntu. Security updates get
one check for each package manager, because a reader of the check should see
which package manager it asks; the checks with plain names learn each family
inside the check. One function, `NodeDistributionFamily` in
`internal/primitives/operatingsystem.go`, finds the node's family from `ID`
and `ID_LIKE`, so a new family is one more case. Fedora joins the Red Hat
family by its own `ID` only, because Amazon Linux names `fedora` in its
`ID_LIKE` and is no member. The Red Hat family has two versions of dnf, and
the checks tell them apart by the node: on Fedora 41 and newer, the program
`dnf` is a link to `dnf5`. Each version has its own way to read the answer
of dnf, in one small table in `check_dnf_security_updates.go`, and what a
version does not understand is `unknown`.

`known_exploited_vulnerabilities_in_installed_packages` stays on Debian and
Ubuntu because the check needs vulnerability data that records every flaw
of a release, also a flaw without a fix, and only Debian's and Ubuntu's data
does. The Red Hat family's and SUSE's data in OSV.dev holds advisories
only, and when it was measured on 2026-11-14, Rocky Linux 9's data missed
known exploited flaws and openSUSE Leap 16.1 had 3 advisories. On those
families the check would only repeat the waiting fixes of
`dnf_security_updates` and `zypper_security_patches`, and a healthy result
would sometimes be wrong. The measurements, and what a user can do, are in
[Limits on each distribution](../../reference/limits-on-each-distribution.md).
The decision changes when the data does: a family whose published data
records each flaw of each release, also the unfixed ones, can get the
check. SUSE's own OVAL files already do that for openSUSE Leap 16.1, at the
cost of a second data format and about 10 MB to download for each run.

Arch Linux has no security-update check, because pacman's package databases
hold no security data, and the Arch Security Team's separate tracker
referenced few recent flaws when it was measured on 2026-11-14 (5 CVEs from
2026, 79 from 2025); the measurements are in
[Limits on each distribution](../../reference/limits-on-each-distribution.md).
A check becomes possible when pacman or a maintained Arch Linux source
records the flaws of current packages.

Release 1 is MMWatchdog's beta: the first published release.

## Product principles

Systems should remain comprehensible when a smart engineer has effectively
become a monkey at 3 AM.

- Make one happy path clear for developers who may be new to server systems.
- Write each setting that decides behavior on the thing it affects, so that a
  reader sees the setting on the affected thing. Where MMWatchdog has a
  default, inspection shows the effective value.
- Keep configuration readable, validate it, and explain failures concretely.
- Build the checks that many servers need into MMWatchdog itself, so that a person
  finds them there from the first release, tested with it.
- Keep uncommon needs out of the core. An uncommon check, and an alert
  destination that one company runs, is an official extra module.
  The official extra module's script ships with MMWatchdog. A fix to the
  script needs no change to the program's code, but it still comes in a new
  release: a node runs only official extra modules from the program's own
  release (`shipped_with_mmwatchdog_version`).
  A team's own need is an easy-to-write module of its own.
- Support for everyday monitoring workflows is the product. MMWatchdog
  replaces Monit for most users only if the everyday Monit workflows work
  simply. Everyday workflows must also work simply for MMWatchdog to replace
  M/Monit for most fleet users. The everyday workflows:

  Watch a systemd unit and restart the unit when the unit fails or crash-loops.
  Report any failed systemd unit. Watch node CPU, memory, swap, load, disk
  space and inodes. Notice a read-only filesystem or an unexpected reboot.

  Check HTTP endpoints with the required method, headers, and credentials.
  Check TCP ports and certificate expiry. Expect scheduled jobs to report
  success. Run a script as a check or as a recovery action. Confirm failures
  over time. Alert by webhook, email, or chat with reminders until fixed.

  Keep planned work quiet, and show every node in one place.

  Specialized checks are modules. A workflow that is not
  built yet is a gap on the [release plan](#remaining-work),
  never something to trim. Simplicity comes from one way to do each job
  and no machinery for rare cases, not from dropping everyday workflows.
- Everything works without paying. MMWatchdog is complete on its own and alerts
  through MMWatchdog's destination modules. MMHotline is
  the on-call program that a team runs on its own server. MMHotline knows who is
  on call, notifies that person through their chosen contact methods, and
  escalates an `sos` alert that nobody acknowledges. Check-ins from the
  MMWatchdog fleet hub let MMHotline notice when the fleet goes silent.
  MMWatchdog never requires MMHotline.
- Keep the implementation small and built on a few explicit design rules, so
  a reviewer, human or LLM, can reason through it and verify it against the
  [acceptance contracts](../acceptance.md). Code for a case that
  neither the typical deployment nor the 10,000-node lab will meet costs more in
  reasoning than the code saves. Leave code for such cases out.
- MMWatchdog's operators are administrators with full root on their nodes, and the
  people they grant access are trusted. MMWatchdog monitors and recovers their nodes
  reliably and makes that easy; it does not guard against its own operators.
  Protect the real boundaries (authentication of joins, reports, and
  heartbeats; alert and join credentials; network input treated as untrusted;
  no node changing another's data) and test the common workflows well; do
  not write code for rare cases or for trusted people misusing
  their access.

### AI authors, human readers

Expect advanced coding agents to write much of the configuration and custom
code. Optimize configuration and custom code for the human who must read,
understand, and diagnose the configuration and code.
Many human readers of configuration and custom code are developers who work
on servers for the first time. Many human readers are not native English
speakers. A setting name says in plain words what happens, so that nobody
has to look it up; an abbreviation is fine only
where everyone knows it. Prefer explicit names, ordinary syntax, and visible
behavior over clever shorthand.

#### env, config, and dir have one spelling

Every name that a user reads, such as a setting, a module input, a
measurement, a JSON field, or a placeholder, uses one spelling for
environment, configuration, and directory, as MMX does:

- `env` names environment variables. The whole word "environment" stays
  only for another meaning, such as a deployment in `MYAPP_ENVIRONMENT:
  production`.
- `config` stands for configuration: `--config-dir`, `config_fingerprint`,
  the `config` of a module description's example.
- `dir` is written in a label without a verb whose value is a directory
  path: `state_dir`, `module_state_dir`, `MODULE_DIR`. A name with a verb, a
  plural, and a name whose first word is "directory" keep the whole word:
  `run_from_directory`, `module_directories`.
- A tool's own name keeps its spelling, such as systemd's
  `StateDirectory=`.

**Why:** Linux tools write `env` (`env`, `printenv`, `.env` files) and
`config` (`git config`, `~/.config`), and "environment" alone also means a
deployment such as staging. Tools write `dir` in labels (`--root-dir`,
`XDG_RUNTIME_DIR`) and the whole word in phrases ("home directory", "make
parent directories"). With one spelling, a reader and a coding agent look a
name up instead of guessing it. `TestNamesFollowTheNamingTable` in
`docs/reference/modules/generator` checks every YAML and JSON name in the code and
every module input and measurement. Whether a label has a verb stays a call
for review.

Humans and agents use the same commands and data. Inspection provides readable
text and stable JSON output, with explicit identities, timestamps, units, and
failure explanations. An agent must be able to operate MMWatchdog directly without
an interactive interface. Keep the normal monitoring and recovery behavior
deterministic. The user's agent uses the inspection interfaces.

### Fleet presentation

**Normality compresses. Exceptions expand.**

9,997 healthy machines shouldn't consume 9,997 units of human attention. Three
broken machines should.

## Monkeymaxxing requirements

The Monkeymaxxing course's monitoring lesson introduces MMWatchdog after
learners have performed the checks manually. Linux and server systems are
the subject. Students learn what MMWatchdog does and use it through its
configuration, its CLI, and its modules; its internals are not course
material.

The course's student lab places local monitoring on every node, and the
central view and check groups that reach services over the network on the
operations machine, `util`.
A node's configuration role owns the check groups for the services it
installs.

### Service checks and measurements

| Service | Required visibility |
| --- | --- |
| Caddy | Load-balancer service and reachability checks |
| Application | Process and health checks |
| Database | Process, primary/replica role, replication sequence, lag, connections in use, cache hit ratio, and how long the write-ahead log waits to be archived |
| Database backups | The age of the latest backup, and every problem that the backup tool reports |
| Memcached and its application use | Availability, memory, items, hits, misses, hit ratio, evictions, cache errors, calculations avoided, and database work caused by misses |

The cache measurements must make cold-cache, warm-cache, invalidation,
expiration, failure, stampede, request-coalescing, and eviction behavior visible.
The application provides the evidence for calculations avoided and database
work caused by misses; those are not solely cache-node measurements.

### Failure behavior to prove

The operating exercises require visible failure and recovery for a stopped
application process, broken application health check, growing replication lag,
approaching disk limit, and unavailable machine. Operators must be able to
diagnose the check and confirm that the alert clears after recovery.

Local monitoring on every node, with a central view and network checks on
`util`, should remain useful from small fleets through the
10,000-node lab. Increasing scale should primarily change resource requirements
and presentation, without requiring a fundamentally more complicated
architecture.

### Custom check exercise

1. Give students something deliberately unsupported.
2. Read [Writing modules reference](../../reference/writing-modules.md) and the example modules.
3. Have the students' coding agent implement the missing check.
4. Write and run the missing check's tests.
5. Use the new check against the real system.

The lesson is: "This isn't magic. Here's how the abstraction works. If it
doesn't cover your situation, extend it."

## Design map

| Document | Owns |
| --- | --- |
| [Check groups and checks](checks.md) | Named check groups and checks, timing and the failing time, limits on measurements, the rules that the protocol checks share, modules of every kind (core modules, official extra modules, and the project's own modules), the primitives, the module description, findings and exact findings, keeping a check quiet while its cause fails, and heartbeats |
| [Recovery and maintenance](recovery.md) | Restarts of a systemd unit, recovery commands, recovery attempts, and maintenance windows |
| [Alerts](alerts.md) | Alert ownership, events, the title and body of an alert, destinations and destination modules, which alert goes to whom, notices, durable delivery, and reminders |
| [Fleet](fleet.md) | The hub, joining, reports, the fleet view, node groups, measurement history on the hub, fleet alerts and fleet alert grouping, one alert per exact finding, silence, the hub-outage alerter nodes, MMHotline in a fleet, and setting a finding aside |
| [Operation](operation.md) | Runtime and state, configuration and activation, the CLI, measurement history and storage, running your own program as a systemd unit, and MMX deployment |

The design of one module, from what it reads to why it works that way, is on
its page in the [module index](../../reference/modules/README.md).

The [implementation rules](../implementation-rules.md) govern the daemon's code, and
the guides and the module pages describe the built behavior.

## Status

Each row was checked against the code. A row that reads "Built. Not built:"
names what the build still lacks of the feature in the row.

| Feature | Status |
| --- | --- |
| [Named check groups and checks](checks.md#named-check-groups-and-checks) | Built |
| [Timing and confirmation](checks.md#timing-and-confirmation) | Built |
| [Scheduling and observed state](checks.md#scheduling-and-observed-state) | Built |
| Systemd unit checks: [`systemd_unit_state`](../../reference/modules/systemd_unit_state.md) | Built |
| Failed systemd units: [`every_systemd_unit`](../../reference/modules/every_systemd_unit.md) | Built |
| One program of a unit: [`systemd_unit_commands`](../../reference/modules/systemd_unit_commands.md) | Built |
| Crash loops: [`systemd_unit_crash_loop`](../../reference/modules/systemd_unit_crash_loop.md) | Built |
| [Checks with a limit on a number](checks.md#checks-with-a-limit-on-a-number), and the modules that measure the node and its systemd units: [`node_resource`](../../reference/modules/node_resource.md), [`node_filesystem`](../../reference/modules/node_filesystem.md), [`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md), [`node_disk_device`](../../reference/modules/node_disk_device.md), [`node_network_interface`](../../reference/modules/node_network_interface.md), and [`systemd_unit_resource`](../../reference/modules/systemd_unit_resource.md) | Built |
| Unix socket checks, ping checks, the clock, and out-of-memory kills: [`unix_socket`](../../reference/modules/unix_socket.md), [`ping`](../../reference/modules/ping.md), [`node_clock`](../../reference/modules/node_clock.md), and [`out_of_memory_kills`](../../reference/modules/out_of_memory_kills.md) | Built |
| Read-only filesystems, in [`node_filesystem`](../../reference/modules/node_filesystem.md) | Built |
| [Reboots](checks.md#reboots) | Built |
| HTTP checks: [`http`](../../reference/modules/http.md) | Built: an explicit certificate-check override, body rules that must be nonempty, and the check group's request time limit |
| TCP checks: [`tcp`](../../reference/modules/tcp.md) | Built: reply rules require a received byte |
| [Protocols and common server software](checks.md#protocols-and-common-server-software) | Built: explicit connection security, including Redis TLS. Certificate checks are on by default, with an explicit override. A disconnected streaming PostgreSQL replica fails even without a limit. VM verification of connection security, certificate checking, and disconnected-replica failure is pending |
| The cache hit ratio and the write-ahead log's archive of PostgreSQL, in [`postgresql`](../../reference/modules/postgresql.md), and its backups: [`pgbackrest`](../../reference/modules/pgbackrest.md) | Built. Tested on a real node of every [target distribution](../testing.md#every-target-distribution), with the distribution's own PostgreSQL and pgBackRest |
| Transaction-ID wraparound in [`postgresql`](../../reference/modules/postgresql.md#how-far-the-transaction-ids-are-from-wraparound) | Built: the oldest transaction ID age across all databases as a percentage of 2^31, with an optional limit. Passed the services suite on every target distribution (2026-11-05) |
| Security updates, required reboots, the end of a release's security support, software RAID, and temperature: [`apt_security_updates`](../../reference/modules/apt_security_updates.md), [`dnf_security_updates`](../../reference/modules/dnf_security_updates.md), [`zypper_security_patches`](../../reference/modules/zypper_security_patches.md), [`reboot_after_updates`](../../reference/modules/reboot_after_updates.md), [`distribution_release_security_support`](../../reference/modules/distribution_release_security_support.md), [`mdadm_array`](../../reference/modules/mdadm_array.md), and [`node_temperature`](../../reference/modules/node_temperature.md) | Built. The release-support check names regular or long-term security support; VM verification of both choices is pending. Security updates, required reboots, and the end of security support are tested on a real node of every [target distribution](../testing.md#every-target-distribution), with the real `apt` and the real `distro-info-data`; on Rocky Linux 9 and AlmaLinux 10, with the real `dnf`, the real `dnf-plugins-core`, and the real `SUPPORT_END` of `/etc/os-release`, and on Fedora 45, with the real dnf 5, the real `dnf5-plugins`, and the real `SUPPORT_END`, each with SELinux enforcing; on openSUSE Leap, with the real `zypper` and the real product file |
| Security fixes that need Ubuntu Pro: [`ubuntu_pro_security_fixes`](../../reference/modules/ubuntu_pro_security_fixes.md) | Built. Tested on a real node of every [target distribution](../testing.md#every-target-distribution): on Ubuntu with a real fix of ESM Apps, on Debian the unknown result. A node attached to Ubuntu Pro is not tested |
| Known exploited flaws in installed packages: [`known_exploited_vulnerabilities_in_installed_packages`](../../reference/modules/known_exploited_vulnerabilities_in_installed_packages.md) | Built: the check reads CISA's list and OSV.dev itself. Go tests use stand-ins for CISA's list service and OSV.dev. The node suite uses the real `dpkg` and `apt` on the Debian and Ubuntu releases of the [target distributions](../testing.md#every-target-distribution), and checks the `unknown` result and its message on the others. The node suite uses stand-ins for CISA's list service and OSV.dev. Tests against the real CISA list service and OSV.dev ran on two Debian 13 servers and an Ubuntu 24.04 server ([validation record](../validation.md#current-build)) |
| TLS certificates: [`tls`](../../reference/modules/tls.md) | Built |
| Shell checks: [`shell`](../../reference/modules/shell.md) | Built |
| [Modules](checks.md#modules): one way to call a module of every kind, [the module description](checks.md#the-module-description) with `mmwatchdog describe-module`, and [`mmwatchdog module-usage`](checks.md#which-modules-a-node-uses) | Built |
| [The project's own modules](checks.md#the-projects-own-modules) as Starlark scripts, with the [primitives](checks.md#primitives) | Built, including optional contracts for input validation and quiet destination verification. MMWatchdog reads check-module `_file` inputs as root and passes the inputs separately to the module |
| [Executables and the JSON protocol](checks.md#executables-and-the-json-protocol), and the [JSON helper](checks.md#json-helper-for-bash-executables) | Built |
| [State kept between runs](checks.md#state-kept-between-runs) | Built, including preservation across timing-only reloads |
| [Findings](checks.md#findings) | Built |
| [Exact findings](checks.md#exact-findings), from every core module that reports findings and from modules of the project's own; `apt_security_updates`, `dnf_security_updates`, and `zypper_security_patches` report one finding per source package, advisory, or patch | Built |
| [Official extra modules](checks.md#official-extra-modules) | Built: Starlark scripts in `official_extra_modules/` of the repository and of every release, the version check, and a directory for official extra modules on a node. Five modules deliver alerts: `discord`, `mattermost`, `pushover`, `slack`, and `telegram`. Three modules check a tool: `zfs_pool`, `apache_status`, and `mongodb`. The destination scripts support configuration-time input validation and quiet verification |
| [One cause, one alert](checks.md#one-cause-one-alert) | Built |
| [Heartbeats](checks.md#heartbeats) | Built |
| [Restart a systemd unit](recovery.md#restart-a-systemd-unit) | Built: identical shared budgets, spacing after completion, and systemd-owned restart waits |
| [Run a recovery command](recovery.md#run-a-recovery-command) | Built, including spacing from the end of each attempt |
| [Maintenance windows](recovery.md#maintenance-windows) | Built, including windows without start and end alerts |
| [Deploys and recurring windows](recovery.md#deploys-and-recurring-windows) | Built: deploy windows open only before a restart for changed inputs |
| [Who sends an alert](alerts.md#who-sends-an-alert) | Built |
| [Alert events](alerts.md#alert-events) | Built |
| [Title and body](alerts.md#title-and-body) | Built |
| [Destinations](alerts.md#destinations) and [`webhook`](../../reference/modules/webhook.md) | Built, including explicit certificate policy, routing validation, quiet verification at reload and once a day, and destination health notices |
| Email: [`email`](../../reference/modules/email.md) | Built, including partial-recipient delivery and quiet SMTP verification. The acceptance and fleet suites test the `email` destination module against Mailpit, a mail server for tests, inside each guest |
| [Destination modules](alerts.md#destination-modules), with [`discord`](../../../official_extra_modules/discord/README.md), [`mattermost`](../../../official_extra_modules/mattermost/README.md), [`pushover`](../../../official_extra_modules/pushover/README.md), [`slack`](../../../official_extra_modules/slack/README.md), [`telegram`](../../../official_extra_modules/telegram/README.md) | Built. Tested against the real services: `discord`, `pushover`, `slack`, and `telegram` by the [real service test](../testing.md#real-services-before-a-release) before each release, and `mattermost` by the services suite with a real Mattermost server |
| System log: [`system_log`](../../reference/modules/system_log.md) | Built |
| [No destination](alerts.md#no-destination) | Built |
| [Which alert goes to whom](alerts.md#which-alert-goes-to-whom) | Built |
| [Send a notice](alerts.md#send-a-notice) | Built |
| MMHotline destination: [`mmhotline`](../../reference/modules/mmhotline.md) | Built. Tested against a fake MMHotline built from MMHotline's alert intake contract, and against a real MMHotline by the [real service test](../testing.md#real-services-before-a-release) |
| [Alert severity](alerts.md#alert-severity), [per check](checks.md#alert-severity), and [for silent nodes](fleet.md#silent-nodes) | Built |
| [Delivery](alerts.md#delivery) | Built |
| [Reminders](alerts.md#reminders) | Built |
| [Set up a fleet](fleet.md#set-up-a-fleet), [joining](fleet.md#joining-and-identity), and [reports](fleet.md#reports) | Built |
| [Fleet view](fleet.md#fleet-view) | Built |
| [Node groups](fleet.md#node-groups) | Built |
| [Measurement history on the hub](fleet.md#measurement-history-on-the-hub) | Built |
| [Alerts in a fleet](fleet.md#alerts-in-a-fleet) and [fleet alert grouping](fleet.md#fleet-alert-grouping) | Built |
| [One alert per exact finding](fleet.md#one-alert-per-exact-finding): per destination and key, growth on the shared tick, one reminder schedule per key, and an `sos` first in each destination's queue | Built |
| [Set a finding aside](fleet.md#set-a-finding-aside), with `mmwatchdog fleet findings` and `mmwatchdog fleet set-aside-findings` | Built |
| [Silent nodes](fleet.md#silent-nodes) and [the hub-outage alerter nodes](fleet.md#the-hub-outage-alerter-nodes) | Built |
| [Maintenance and deploys in a fleet](fleet.md#maintenance-and-deploys-in-a-fleet) | Built |
| [Forget, rotate, and move](fleet.md#forget-a-node-rotate-the-join-code-move-the-hub) | Built |
| [One incident per check across the fleet](fleet.md#one-incident-per-check-across-the-fleet), for MMHotline and every destination whose service keeps incidents | Built |
| [MMHotline in a fleet](fleet.md#mmhotline-in-a-fleet), including the optional alert source for nodes sending without the hub | Built |
| [Install and run](operation.md#install-and-run) | Built |
| [Configuration files and validation](operation.md#configuration-files-and-validation) | Built: explicit recovery, queue, fleet, reminder, and module settings; node prerequisites checked by validation and reload; `--config-only` for authoring elsewhere. Not built: the validation messages for the parts that are not built |
| [Activate a configuration](operation.md#activate-a-configuration) | Built |
| [Command reference](operation.md#command-reference) | Built |
| [Status and diagnosis](operation.md#status-and-diagnosis) | Built |
| [Measurement history](operation.md#measurement-history) | Built |
| [Retained state and storage limits](operation.md#retained-state-and-storage-limits) | Built |
| [The system clock](operation.md#the-system-clock): every wait on the monotonic clock | Built |
| [Deploy with MMX](operation.md#deploy-with-mmx) | Built |
| [Readings commands](operation.md#looking-at-the-readings) | Built |
| [Charts of the readings](operation.md#charts-of-the-readings) | Built |
| File checks and log line checks: [`file`](../../reference/modules/file.md) and [`log_lines`](../../reference/modules/log_lines.md) | Built |

## Remaining work

The [release checklist](#release-checklist) is done for 0.1.0
([validation record](../validation.md#current-build)). What remains:

- **Real use** in a fleet beyond the test guests. A fleet of three servers
  has run since 2026-10-22 ([validation record](../validation.md#real-use));
  what remains is the running time that exercises outages, deploys, and
  reminders.
- **The Ubuntu Pro step of the ARM64 `node` suite.** On Ubuntu 26.04 on a
  Hetzner `cax11` server, the step runs longer than 4 minutes inside the
  suite, while the same check takes 43 seconds on that kind of server
  ([validation record](../validation.md#current-build)). Find the cause.

## Definition of done

A feature is done when:

- its design section is implemented as written, or the section is changed
  with the user's agreement first;
- it has its row in the [acceptance contracts](../acceptance.md), and
  [testing](../testing.md) maps that row to focused tests and a Linux
  workflow that pass;
- the guides and the module pages that describe its current behavior are
  updated, as the [documentation map](../documentation-map.md) says; and
- its row in the status table above reads Built.

## Release checklist

Beyond the features:

- **Release notes.** The notes of every release carry the hand-written
  section "Changes you must act on" at the top, which
  [Update MMWatchdog](../../how-to/update-mmwatchdog.md) tells the reader
  to follow. [Release MMWatchdog](../releasing.md) says how to publish a
  release and write the section.
- **Contracts and tests.** Every contract row has passing focused tests and
  Linux workflow coverage, and no known correctness blocker remains.
- **Beta validation.** The selected real-world monitoring scenarios have been
  run and checked, including their failure and recovery behavior, and every
  correctness blocker they found is fixed. The tests, examples, and coverage
  index that beta validation leaves behind are in the MMWatchdog repository.
  The coverage index is in
  [evidence and coverage](../testing.md#evidence-and-coverage).
- **Static analysis and fuzzing.** Static analysis and fuzzing are a release step only.
  Ordinary changes do not run the release step.
  `staticcheck`, `errcheck`, `exhaustive`, `nilaway`,
  and `govulncheck` report nothing except reviewed exceptions, each with a
  one-line reason beside it, and the Go tests pass under the race detector.
  Fuzz tests cover what MMWatchdog reads from outside: check group configuration
  files, module descriptions, the output of a module, the fleet hub's join requests and node status
  reports, and the answers of MMHotline's alert intake. Each fuzz test
  runs for 10 minutes on a Linux test server. A failing input found by
  fuzzing is fixed, and Go keeps the failing input as an ordinary test case.
  `govulncheck` runs against the
  upstream versions of the dependency copies in `third_party/`, as the
  [implementation rules](../implementation-rules.md#when-a-copy-or-the-toolchain-changes)
  describe. The [testing guide](../testing.md#fuzz-tests-for-a-release) lists
  the exact commands and the input that each fuzz test covers.
- **A weekly check for vulnerabilities in the dependencies:** the
  scheduled workflow `.github/workflows/dependency-vulnerabilities.yml` runs
  `scripts/check-dependency-vulnerabilities.py` every Monday against the
  upstream versions of MMWatchdog's copies, as MMX's does. The workflow
  fails, and GitHub sends an email, only for an advisory that MMWatchdog's code
  reaches and that no copy's record lists as fixed.
- **Real alert destinations.** The
  [real service test](../testing.md#real-services-before-a-release) passes
  against Slack, Telegram, Discord, and Pushover, and a person checks the
  Slack channel and the phone as that section says. The other destinations
  need no such run: the services suite tests `mattermost` against a real
  Mattermost server, and the suites test `webhook` against a real HTTPS
  server and `email` against Mailpit inside each guest, with no mail
  account. What a real service on the internet
  would add for `mattermost`, `webhook`, and `email` is the network on the way
  and, for email, spam
  filtering, which are not MMWatchdog's code. MMWatchdog sends a real alert and check-ins to
  MMHotline through the MMHotline destination, and a person is woken
  through a push app.
- **Platforms.** Every suite passes on every
  [target distribution](../testing.md#every-target-distribution) on AMD64,
  and the [ARM64 suites](../testing.md#test-in-levels) pass on ARM64, on
  the release build, together with the sustained workload, the
  release-bundle deployment, and the 10,000-node load run. The load run
  fails on an unexpected restart of the hub's daemon and on sustained
  reporting failures ([large-fleet load run](../testing.md#large-fleet-load-run)).
- **Builds and checksums.** `scripts/release.py` builds ARM64 and AMD64 from
  a clean tagged checkout; `SHA256SUMS`, manifests, and license notices are
  complete, and the [validation record](../validation.md) describes that build.
- **Publication.** Complete the public-readiness review, then upload the
  reviewed tree as one initial commit to the public repository
  `mmwatchdog` of the `monkeymaxxing` organization on GitHub.
- **License.** MMWatchdog uses the Apache License 2.0 in the root `LICENSE`, the same
  license as MMX, so the two tools have the same terms. There is no `NOTICE`
  file and no per-source-file license header: the root license states the terms.
- **Versioning.** Tag the first release 0.1.0 and have `mmwatchdog version` report it,
  using [compatibility between releases](#compatibility-between-releases).
  A build that is not a release reports `0.1.0-dev`, including its official
  extra modules. Release builds set the version through linker flags.
- **Install and upgrade documentation.** [Install on one node](../../how-to/install-on-one-node.md)
  covers a fresh install, an update, and removal for the release, and the
  README gives a short quick start.
- **Continuous integration.** Tests, vet, and cross-builds run automatically on
  every push to GitHub: `.github/workflows/checks.yml`, the same setup as
  MMX's ([testing guide](../testing.md#continuous-integration)). The
  `.github/workflows/checks.yml` workflow first runs at publication.
  The workflow's first run on GitHub passes.
- **Real use.** MMWatchdog runs a real fleet beyond the two test guests, such as the
  Monkeymaxxing lab, long enough to exercise ordinary outages, deploys, and
  reminders.
- **Compatibility.** The release keeps the
  [compatibility rules](../../../AGENTS.md#compatibility-between-releases):
  a state file that the release cannot read needs a migration or a
  documented reset, and the release notes list each change that a user must
  act on.

### Compatibility between releases

MMWatchdog follows semantic versioning, starting at 0.1.0, as MMX does:

- A patch release, such as 0.1.1, holds fixes of faults and text changes,
  and makes no incompatible change.
- While MMWatchdog is 0.x, a minor release, such as 0.2.0, holds new
  commands, modules, and fields, and may break compatibility. Its notes list
  each incompatible edit and what the operator must do.
- Version 1.0.0 comes once names and file formats settle. From then on,
  incompatible changes require a new major version.

Incompatible changes include configuration files that no longer validate or
behave as under the preceding release, renamed or removed commands, flags,
or result fields, state files that the new release cannot read, and changes
to node status reports that a fleet hub cannot accept or interpret correctly.
Starting at 0.1.0 lets real use improve names and formats before they settle.

An MMWatchdog fleet hub accepts node status reports from nodes one minor release
older. Upgrade the hub first, then the nodes. The current hub stores the
node's `build` for inspection; it does not compare build versions or select
a decoder by version. The current hub strictly decodes and validates
report fields.
The one-minor-release rule is therefore a promise to preserve report formats
between a hub and nodes one minor release older than the hub.
The one-minor-release rule adds no negotiation or support for wider version
gaps.

## Open decisions

Any open decision in the release plan's Open decisions section needs the
user's decision. Resolve each open decision in the design document and
acceptance contract that own the decision. Then remove the resolved decision
from the Open decisions section of the release plan.

None is open.

Decided 2026-11-17: every joined node keeps every destination secret, including MMHotline's signing secret.
Reason: each node must be able to alert by itself when the hub is down, so it needs the secrets.

## Implementation notes

- **Channels.** Each core destination module has one `NotificationSender`,
  and one more runs every destination module that is not core. The outbox,
  ordering, and retries stay shared. Never log a request URL that holds a
  credential.
