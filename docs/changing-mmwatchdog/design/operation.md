# MMWatchdog: operation

This document owns how MMWatchdog runs on a node: its runtime and state, its
configuration and activation, the CLI, measurement history, retained
information, deployment through MMX, and running a program of your own as a
systemd unit. The [`mmhotline`](../../reference/modules/mmhotline.md) page owns the
connection to MMHotline. The
[product principles](README.md#product-principles) apply throughout: AI
agents write much of the configuration, humans read and diagnose it, and both
use the same commands and data.

## Install and run

MMWatchdog is one `mmwatchdog` binary: the daemon, the CLI, the core
[modules](checks.md#modules), the program that runs a Starlark module, the
Bash JSON helper, and the heartbeat sender. The
[official extra modules](checks.md#official-extra-modules) are a directory of
scripts beside the `mmwatchdog` binary. MMWatchdog runs on Linux
with systemd; the CLI also builds for
macOS for configuration validation and module authoring.

One **node monitoring process**, managed by systemd, runs as root on each node.
Running as root gives the node monitoring process direct systemd access.
Root access also lets the node monitoring process choose the account each
custom check runs as.

Node check workers are goroutines inside the node monitoring process.
A shell check and a module that is not core each run in a child process.

| Path | Owner and purpose |
| --- | --- |
| `/usr/local/bin/mmwatchdog` | The binary |
| `/etc/mmwatchdog/mmwatchdog_node_settings.yml` | Node settings only |
| `/etc/mmwatchdog/check_groups/*.yml` | All check groups; at least one is required |
| `/etc/mmwatchdog/alert_destinations.yml` | Alert destinations, on a standalone node or the hub |
| `/usr/local/lib/mmwatchdog/official_extra_modules` | The [official extra modules](checks.md#official-extra-modules) of the installed MMWatchdog release, for checks and for alert destinations |
| `/usr/local/lib/mmwatchdog/external_modules` | The [project's own modules](checks.md#the-projects-own-modules), for checks and for alert destinations |
| `/var/lib/mmwatchdog` | Daemon state: `state.db`, the [measurement history](#measurement-history), and the fleet files on a fleet member or hub. The directory is root-owned, mode 0700 |
| `/var/lib/mmwatchdog-module-state` | One directory for each check whose module is not core: the [state that the module keeps between runs](checks.md#state-kept-between-runs) |
| `/var/lib/mmwatchdog-checks` | Home of the unprivileged `mmwatchdog` check account, owned by `mmwatchdog`, mode 0700 |
| `/run/mmwatchdog/mmwatchdog.sock` | Root-owned local socket for CLI clients |
| `/run/mmwatchdog/heartbeat.sock` | Local socket (mode 0666) through which jobs on the node report [heartbeats](checks.md#heartbeats); it exists while the configuration has a heartbeat check group. Only root can create it, because only root can write to `/run` |

```sh
sudo mmwatchdog validate
sudo systemctl enable --now mmwatchdog
sudo mmwatchdog status
```

Security boundaries:

- A shell check and a module that is not core run as `mmwatchdog` unless the check
  or the destination names another account with `run_as_user_account`.
  The `mmwatchdog` check account's home stays separate from daemon state.
  Checks running as `mmwatchdog` cannot read or change the daemon's incidents,
  credentials, or alert queue. The account restriction applies to Starlark
  scripts too: MMWatchdog runs each script in a separate process, never
  inside the daemon.
- The daemon refuses a state directory that is a symlink or not a root-owned
  directory with mode 0700, before opening its lock or database.
- Clients reach the daemon only through the root-owned socket, so inspection
  and control use ordinary `sudo` access.
- Credentials live in files that configuration names: webhook and heartbeat
  tokens, the email password, the secrets that destination modules use,
  the heartbeat receiver's TLS keys, and the hub join code. The credentials
  never appear in configuration text, URLs, or command arguments.

The daemon needs a writable state database to start. On a full or read-only
state filesystem, the daemon exits with the SQLite error.
Systemd restarts the daemon until the state filesystem is usable.
Once running, a write failure does not stop
monitoring; [retained state](#retained-state-and-storage-limits) describes
what continues.

## Configuration files and validation

Configuration is YAML data with MMWatchdog's own small schema. There is no
expression or template language, and MMWatchdog does not interpolate values into
scripts. MMX can generate configuration when a fleet needs variation. Machine
interfaces, including CLI JSON output and the protocol of an executable module, use JSON.

The configuration directory defaults to `/etc/mmwatchdog`. Every command that reads
configuration accepts `--config-dir DIR`. Each command combines node
settings, check groups, and alert destinations from the selected
configuration directory:

- `mmwatchdog_node_settings.yml` holds only the node's settings, without
  check groups.

  Six settings are required on every node:
  `alert_severity_when_this_node_goes_silent`
  ([silent nodes](fleet.md#silent-nodes)),
  `alert_severity_when_this_node_has_rebooted`
  ([reboots](checks.md#reboots)), `keep_measurement_history_for`
  and `measurement_history_size_limit`
  ([measurement history](#measurement-history)), and
  `keep_undelivered_alerts_for` and `undelivered_alerts_content_size_limit`
  ([storage limits](#retained-state-and-storage-limits)).

  Optional node settings are
  `socket`, `state_dir`, `official_extra_module_dir`, `external_module_dir`,
  `module_state_dir`, `max_concurrent_checks`,
  `heartbeat_receiver`, the other
  [storage settings](#retained-state-and-storage-limits),
  `raise_to_sos_after_this_node_is_silent_for`
  ([silent nodes](fleet.md#silent-nodes)), `this_node_is_in_node_groups`
  ([node groups](fleet.md#node-groups)),
  `raise_to_sos_when_every_node_of_these_node_groups_is_silent`
  ([silent nodes](fleet.md#silent-nodes)).

  The role settings `fleet_hub_role` and `join_fleet_hub` are optional.
  A node can set at most one of `fleet_hub_role` or `join_fleet_hub`.

  `official_extra_module_dir` defaults to
  `/usr/local/lib/mmwatchdog/official_extra_modules`.
  `external_module_dir` defaults to
  `/usr/local/lib/mmwatchdog/external_modules`.
  `module_state_dir` defaults to `/var/lib/mmwatchdog-module-state`.
  `max_concurrent_checks` defaults to 8 and accepts any whole number from 1.
- `check_groups/*.yml` holds every check group. Each file contains a
  `check_groups:` mapping with any number of check groups. MMWatchdog loads
  every `.yml` file in `check_groups/`. Any other file in `check_groups/`,
  including a `.yaml` file, is a
  validation error. Duplicate check group names are an error naming both
  locations. At least one check group is required across the directory.
- `alert_destinations.yml` holds the `destinations:` mapping for
  [alert destinations](alerts.md#destinations). A standalone node and a hub
  need `alert_destinations.yml`. A node that should alert nobody says so with
  `destinations: {}` ([no destination](alerts.md#no-destination)). On a node
  with `join_fleet_hub`, `alert_destinations.yml` must not exist.
  MMWatchdog rejects `alert_destinations.yml` on a joined node because the
  joined node gets alert settings from the hub.
- `set_aside_findings.yml` is optional, and only the hub may have it. It
  lists the findings that a team [set aside](fleet.md#set-a-finding-aside).
  A joined node and a standalone node refuse it.

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
alert_severity_when_this_node_goes_silent: sos    # the severity of the alert when this node stops reporting
alert_severity_when_this_node_has_rebooted: warn   # the severity of the notice after a reboot of this node
keep_measurement_history_for: 7d                  # this node keeps every reading of every measurement for 7 days
measurement_history_size_limit: 256MiB            # and never more than 256 MiB on this node's disk
max_concurrent_checks: 8                          # at most 8 checks run at the same moment
```

```yaml
# /etc/mmwatchdog/check_groups/web.yml
check_groups:
  myapp:
    check_every: 30s                              # run each check every 30 seconds
    time_limit_per_run: 5s                        # one run of a check may take 5 seconds
    alert_after_failing_for: 1m                   # send the alert after 1 minute of failing
    recovered_after_healthy_for: 30s              # send "recovered" after 30 healthy seconds
    checks:
      myapp_service_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active       # fail when the unit myapp.service is not active
        alert_severity: sos
      myapp_answers_health_request:
        http:
          url: http://127.0.0.1:8080/health
          expected_http_status_codes: [200]
        alert_severity: sos
  api:
    check_every: 1m
    time_limit_per_run: 5s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 1m
    checks:
      api_answers_health_request:
        http:
          url: https://api.example.com/health
          expected_http_status_codes: [200]
        alert_severity: warn
```

[Check groups and checks](checks.md#named-check-groups-and-checks) owns the
check group schema, check names, reserved names, and timings. The node
settings hold no timings for checks: every check group states its own four
timings, as `myapp` and `api` do in the example
([timing and confirmation](checks.md#timing-and-confirmation)).

Validation is strict:

- Unknown fields, duplicate keys, and wrong types are errors.
- Booleans are `true` and `false`.
- A duration is a number with a unit: `s` for seconds, `m` for minutes, `h`
  for hours, or `d` for days, such as `30s` or `7d`. No setting is a
  negative duration.
- A size is a number with a unit: `MiB` or `GiB`, such as `256MiB`.
- YAML aliases, merge keys, and custom tags are rejected.
- Every error names the file, the line, the column, and the path of the
  setting, such as
  `/etc/mmwatchdog/check_groups/web.yml:9:9: check_groups.web.checks.ssh_answers.ssh:`.
  A missing field points to the nearest parent YAML node written in the
  configuration file. An error of a
  value's type says what was expected and what was found:
  `expected a string, found a mapping`, or
  `expected a mapping, found the string "127.0.0.1:7001"`.

A setting that changes what a reader must know is written where the reader
looks for it: a check group states its timings, and a node states how much
measurement history it keeps. Where a setting has a default, inspection shows
the effective value.
`mmwatchdog config` prints the effective configuration
with a `sources` mapping from each check group to its file and line, and for
each check its effective timings and whether they come from the check or from
its check group.

| Command | What the command validates |
| --- | --- |
| `mmwatchdog validate` | The complete configuration directory. Every check and destination is checked against the [description of the check's or destination's module](checks.md#the-module-description). MMWatchdog looks for official extra modules in `official_extra_module_dir` and the project's own modules in `external_module_dir` |
| `mmwatchdog validate --config-dir DIR` | The complete candidate configuration prepared in `DIR`: node settings, check groups, and alert destinations according to the node's role |
| `mmwatchdog validate --official-extra-module-dir DIR --external-module-dir DIR` | The complete configuration, using modules from the supplied directories instead of the node's module directories. A person uses the directory options to validate a deployment project's configuration on the machine holding the project. MMX's deployment workflow uses the directory options to validate against module directories copied to the node but not installed yet ([deploy with MMX](#deploy-with-mmx)) |
| `sudo mmwatchdog validate` | Also the target's files and accounts, as listed under runtime validation below |

Runtime validation checks:

- The heartbeat receiver's TLS certificate and key, the credential files
  that destinations name, and the hub join code.
- The tokens of all heartbeat check groups. Each heartbeat check group
  needs a distinct token. Matching tokens are a validation error.
- `run_as_user_account` and `run_from_directory` for each shell check,
  each module that is not core, and each recovery command.
- Whether each executable module can be started.
- Whether each script is valid Starlark, uses only defined names, and
  loads no other file. Each script must define the function MMWatchdog
  calls, with the arguments MMWatchdog supplies.
- Whether `module_state_dir` is a directory MMWatchdog made, or does not
  exist yet in a parent directory that only root can change.

Runtime validation reads but changes nothing, so validation is safe while
the daemon runs. A credential file must be on the node before the
configuration that references it is validated; the deploy workflow writes
the secret files first for this reason.

### What validation reports beyond the schema

`mmwatchdog validate` also checks relationships between node settings,
check groups, and alert destinations, as the findings table below describes.
An error makes the configuration invalid. A validation warning leaves the
configuration valid: `mmwatchdog validate` prints the warning and still
exits 0. `mmwatchdog reload` prints validation warnings and activates the
valid configuration.

`mmwatchdog validate --json` carries each kind of validation warning
as a separate list: `alert_titles_that_will_be_shortened`,
`systemd_units_missing_from_systemd_units_to_ignore`,
`filesystems_missing_from_filesystems_to_ignore`,
`checks_that_name_a_check_with_a_lower_alert_severity`,
`mmhotline_without_another_destination`, which names each `mmhotline`
destination of a node that has no other kind of destination, and
`mmhotline_loopback_warnings`.

`mmhotline_loopback_warnings` holds strings, one per
`mmhotline` destination on the MMWatchdog fleet hub whose `mmhotline_url`
is a loopback address. Each string says that joined nodes send alerts to the
loopback address when the MMWatchdog fleet hub does not accept an alert in
time. The string also says that loopback points to the joined node itself
and asks for an address that every node reaches
([exact warning](../../reference/modules/mmhotline.md#errors-and-what-they-mean)).

| Finding | Result | Owner |
| --- | --- | --- |
| `alert_destinations.yml` is missing on a standalone node or a hub | Error. The message shows both choices: add a destination, or write `destinations: {}` | [No destination](alerts.md#no-destination) |
| `alert_destinations.yml` says `destinations: {}` | Valid. `mmwatchdog validate` prints `This node sends alerts to nobody.` | [No destination](alerts.md#no-destination) |
| A check's `send_alerts_only_to_destinations` names a destination that `alert_destinations.yml` does not have | Error on a standalone node or a hub. A joined node has no destinations file. `mmwatchdog validate` cannot check destination names against `alert_destinations.yml` on a joined node. `mmwatchdog status` on the joined node shows a wrong destination name | [Which alert goes to whom](alerts.md#which-alert-goes-to-whom) |
| A check's `no_alerts_or_recovery_actions_while_these_checks_fail` names a check that this node does not have | Error. The message lists the checks of the named check group | [One cause, one alert](checks.md#one-cause-one-alert) |
| Checks name each other in that setting in a circle, such as `a/tcp` names `b/tcp` and `b/tcp` names `a/tcp` | Error naming the checks of the circle. While every check in the circle fails, MMWatchdog would send no alert for any check in the circle | [One cause, one alert](checks.md#one-cause-one-alert) |
| That setting names a check that only records its readings, or the check that has the setting | Error | [One cause, one alert](checks.md#one-cause-one-alert) |
| That setting names a check with a lower `alert_severity` than the check that has the setting | Validation warning naming the check containing the setting, the named check, and each check's `alert_severity`. While the named check fails, the alert goes out at the named check's lower severity | [One cause, one alert](checks.md#one-cause-one-alert) |
| A check's alert title would be longer than 120 characters | Validation warning naming the check and the length of its title. A title starts with the node's name, so `mmwatchdog validate` uses the name of the machine it runs on | [Title and body](alerts.md#title-and-body) |
| A systemd unit has its own `systemd_unit_state` check and is missing from `systemd_units_to_ignore` | Validation warning naming the unit: a failure of the unit would send two alerts | [`every_systemd_unit`](../../reference/modules/every_systemd_unit.md) |
| A fleet hub's `mmhotline` destination has a loopback `mmhotline_url` | Validation warning; joined nodes need an address that every node reaches for direct delivery | [`mmhotline`](../../reference/modules/mmhotline.md#errors-and-what-they-mean) |
| A fleet hub's `mmhotline` destination states `alert_source_for_nodes_sending_without_the_hub` while a joined node reports a version older than 0.2.0 | Validation warning naming the nodes: an older node refuses the changed fleet settings until it runs 0.2.0 | [MMHotline in a fleet](fleet.md#mmhotline-in-a-fleet) |
| `set_aside_findings.yml` exists on a joined node or a standalone node | Error: only the hub sets findings aside | [Set a finding aside](fleet.md#set-a-finding-aside) |
| An entry of `set_aside_findings.yml` lacks a field, or a field is empty | Error naming the entry and the field | [Set a finding aside](fleet.md#set-a-finding-aside) |
| A filesystem has its own `node_filesystem` check with a limit and is missing from `filesystems_to_ignore` | Validation warning naming the filesystem: a full filesystem would send two alerts | [`every_disk_and_network_filesystem`](../../reference/modules/every_disk_and_network_filesystem.md) |

### Credential file permissions

Secrets are private by default. Only read bits matter: a write or execute
bit does not expose the secret. Group access requires
`secret_readable_by_group: true`. Access by every account requires
`dangerously_let_every_account_read_secret: true`. A file readable by the
file's group and by every account requires `secret_readable_by_group: true`
and `dangerously_let_every_account_read_secret: true`.
TLS private keys have no exception, since
sharing a key is an operator's explicit choice.

`mmwatchdog validate` without `--config-only`, and `mmwatchdog reload`,
refuse secret files when read bits for the file's group or for every
account lack the corresponding acknowledgment. The refusal names the
credential file, mode, readers, permission line, and `chmod 600 FILE`.
Config-only validation cannot inspect node files.

At startup, the node monitoring process logs each credential permission
problem once, using the permission refusal message, and starts.
The `mmwatchdog heartbeat` command prints the permission refusal message
to standard error as a warning and sends the heartbeat.

Check runs, deliveries, check-ins, and join-code reads continue without
a permission check. A credential file's mode can change on a running node.
Refusing the credential file after a mode change would drop alerts or
heartbeats. Keeping delivery running protects
monitoring while the operator fixes the permissions.
[Configuration files reference](../../reference/configuration-files.md#secret-files)
owns the list of secret inputs and the placement of the permission lines.

## Activate a configuration

`sudo mmwatchdog reload` validates the whole candidate in the daemon's configuration
directory: node settings, check groups, and alert destinations.
The node monitoring process activates the candidate only if the candidate
is valid. A rejected reload leaves
the running configuration, schedules, and incidents untouched and reports the
errors. After a valid candidate is active, the command prints `The
configuration is active.` and below it the
[validation warnings](#what-validation-reports-beyond-the-schema) of that
configuration. Starting the daemon with an edited configuration applies
the activation rules below.

On activation:

- Unchanged checks keep state, schedules, and in-flight observations,
  even when other check groups change. Moving a check definition to another
  file or line without changing the check's behavior counts as unchanged.
- Changed and removed checks discard their in-flight results. A changed
  check starts fresh confirmation. The changed check's
  open incident closes with the
  message `the check was changed in the configuration; MMWatchdog does not
  know whether the problem is fixed` (or `removed from`): an edit is not
  evidence of recovery. An incident whose failure alert went out sends a
  `closed_without_recovery` alert, such as `web-3: myapp/http incident
  closed, maybe not fixed`, to destinations still configured.
  An incident whose failure alert never went out closes only in the event
  history.
- Systemd unit restart attempts stay attached to their unit, so an edit never
  resets them ([restart a systemd unit](recovery.md#restart-a-systemd-unit)).
- Destinations whose name, receiving address, or readable credential content
  changed or disappeared cancel queued deliveries.
  Moving an unchanged credential to another file keeps the queue.
  An unreadable credential has no fingerprint and keeps queued alerts
  until readable contents can be
  compared ([delivery](alerts.md#delivery)).

Some settings take effect only when the daemon restarts: `max_concurrent_checks`, the
socket, the state directory, `module_state_dir`, the heartbeat receiver and its certificate,
`fleet_hub_role.listen_on_address`, `fleet_hub_role.address_for_nodes`, and the two settings that size a database
file, `state_database_size_limit` and
`fleet_hub_role.event_history_of_nodes_content_size_limit`
([storage limits](#retained-state-and-storage-limits)).
Reload rejects a candidate that changes a setting listed as requiring a restart,
saying a daemon restart is required. MMX's deployment workflow restarts MMWatchdog on
configuration updates, so every setting has one deployment path. The settings
of the measurement history do not require a restart.
A reload applies the two measurement history settings of a node and the
two measurement history settings of a hub
([measurement history](#measurement-history),
[measurement history on the hub](fleet.md#measurement-history-on-the-hub)).

## Command reference

Inspection commands take `--json` for stable machine output carrying the same
facts as the readable form. `mmwatchdog config` has no readable form: it prints JSON
with or without `--json`. Commands that talk to the running
`mmwatchdog` service take `--socket PATH` and need `sudo`. Commands that run
without contacting the local daemon include `validate`,
`check-now --without-daemon`, `describe-module`,
`module-usage`, `run-starlark-module`, `module-json`, and
`heartbeat`. A deploy needs no MMWatchdog
command on the machine where MMX runs.
`daemon` runs the node monitoring process as root.

| Command | Purpose |
| --- | --- |
| `mmwatchdog version [--json]` | Build version, revision, modified-source flag, Go version, and platform |
| `mmwatchdog validate [--config-dir DIR] [--official-extra-module-dir DIR] [--external-module-dir DIR] [--config-only] [--json]` | Validate the complete configuration ([above](#configuration-files-and-validation)) |
| `mmwatchdog daemon [--config-dir DIR]` | Run the daemon; systemd starts it |
| `mmwatchdog status [CHECK_GROUP] [--json]` | Health summary and every check's state, or one check group in detail |
| `mmwatchdog config` | Effective configuration with sources and timings, always as JSON |
| `mmwatchdog events [CHECK_GROUP[/CHECK]] [--last DURATION or --since TIME [--until TIME]] [--json]` | Recent events, or one check group's or one check's events with retained evidence; an empty answer says that no event matched |
| `mmwatchdog readings CHECK_GROUP [CHECK] [--last DURATION or --since TIME [--until TIME]] [--json or --chart]` | This node's readings and summaries, even while the daemon is stopped |
| `mmwatchdog check-now CHECK_GROUP [--json]` | Ask the daemon to observe a check group now without recording incidents, alerting, restarting, or running recovery commands |
| `mmwatchdog check-now CHECK_GROUP --count-as-scheduled-run [--json]` | Ask the daemon to start the check group's scheduled runs now. The requested runs count as every scheduled run does. Each check's next run comes `check_every` later |
| `mmwatchdog check-now CHECK_GROUP --without-daemon [--config-dir DIR] [--json]` | Observe a check group locally from the configuration directory |
| `mmwatchdog reload` | Validate and activate the candidate configuration, and print its validation warnings |
| `mmwatchdog maintenance on CHECK_GROUP [CHECK_GROUP ...]\|--every-check-group --for DURATION [--reason TEXT]` | Turn on or replace a [maintenance window](recovery.md#maintenance-windows) for one or more check groups, or for every check group on the node. `--for` is required and has no upper limit, so `--for 30d` is valid. One command sends one alert |
| `mmwatchdog maintenance off CHECK_GROUP [CHECK_GROUP ...]\|--every-check-group` | Turn maintenance off for one or more check groups, or for the whole node; alerts and recovery actions resume after fresh checks |
| `mmwatchdog reset-recovery-attempts CHECK_GROUP` | Clear the named check group's recovery command attempts and the systemd unit restart attempts of the unit that the check group's `restart_systemd_unit` names, including the wait before another attempt ([recovery](recovery.md#restart-a-systemd-unit)) |
| `mmwatchdog send-notice --alert-severity SEVERITY --title TEXT [--body TEXT] [--destination NAME]` | Send a one-time message through the alert destinations: a test of the destinations, or an event from another program. The command waits for the first delivery attempt and prints `delivered` or the failure for each destination ([send a notice](alerts.md#send-a-notice)) |
| `mmwatchdog heartbeat CHECK_GROUP --bearer-token-file FILE [--receiver-url HTTPS_URL [--trusted-certificate-authority-file FILE]]` | Report a successful job to a [heartbeat](checks.md#heartbeats) check group: through the heartbeat socket of this node, or, with `--receiver-url`, to a receiver on another machine over HTTPS |
| `mmwatchdog fleet [NODE [--last DURATION or --since TIME [--until TIME]]] [--json]` | On the hub: the [fleet view](fleet.md#fleet-view), or one node |
| `mmwatchdog fleet --node-group NODE_GROUP [--json]` | On the hub: the fleet view of the nodes of one [node group](fleet.md#node-groups) |
| `mmwatchdog fleet readings NODE CHECK_GROUP [CHECK] [--last DURATION or --since TIME [--until TIME]] [--json or --chart]` | On the hub: one node's retained readings |
| `mmwatchdog fleet readings --node-group NODE_GROUP CHECK_GROUP CHECK [--last DURATION or --since TIME [--until TIME]] [--json or --chart]` | On the hub: one check across a node group, worst first with healthy nodes compressed |
| `mmwatchdog fleet findings CHECK_GROUP/CHECK [--finding ID] [--json]` | On the hub: every node with a finding of the check, each with its exact finding and first failure time, with no limit on the count ([set a finding aside](fleet.md#set-a-finding-aside)) |
| `mmwatchdog fleet set-aside-findings [--json]` | On the hub: each entry of `set_aside_findings.yml` in effect, with the nodes that it covers now, whether it matches no node, and how many nodes cannot check now ([set a finding aside](fleet.md#set-a-finding-aside)) |
| `mmwatchdog fleet join-code [--json]` | On the hub: print the join code |
| `mmwatchdog fleet rotate-join-code [--json]` | On the hub: replace the join secret and print the new code |
| `mmwatchdog fleet forget NODE` | On the hub: delete a node and its data |
| `mmwatchdog describe-module [NAME] [--reference-only] [--config-dir DIR] [--official-extra-module-dir DIR] [--external-module-dir DIR] [--json]` | Print the whole page of one module, through the pager on a terminal. With `--reference-only`, print only the named module's [description](checks.md#the-module-description). The description gives the module's kind, file locations, inputs, measurements, and examples. Without a name, list every module that this node has, the core modules by topic |
| `mmwatchdog module-usage [NAME] [--config-dir DIR] [--official-extra-module-dir DIR] [--external-module-dir DIR] [--json]` | List the modules that this node's configuration names, each with its kind, the place of its files, and every place that calls it ([which modules a node uses](checks.md#which-modules-a-node-uses)) |
| `mmwatchdog run-starlark-module MODULE_DIR` | Run the script of one Starlark module once: read one request from stdin and write one result to stdout, by the [JSON protocol](checks.md#executables-and-the-json-protocol). The daemon starts `mmwatchdog run-starlark-module` for each run of a script. A person can use `mmwatchdog run-starlark-module` to try a script by hand |
| `mmwatchdog module-json get [--type TYPE] POINTER` | Read a value from the request of an executable module by JSON Pointer |
| `mmwatchdog module-json request [--inputs NAME,...]` | Check the request of an executable module and print it |
| `mmwatchdog module-json result --status ok\|failed\|unknown\|delivered (--message TEXT \| --message-stdin) [--measurement NAME VALUE]... [--finding ID MESSAGE]... [--exact-finding ID TEXT]...` | Build the result of an executable module. A module of a check answers `ok`, `failed`, or `unknown`; a [destination module](alerts.md#destination-modules) answers `delivered` or `failed`. `--exact-finding` gives the finding named by `ID` its [exact finding](checks.md#exact-findings) |
| `mmwatchdog module-json object [--string NAME TEXT]... [--string-stdin NAME] [--json NAME JSON]...` | Build a JSON object, for a module that sends JSON to a service |

An executable module finds the helper through `MMWATCHDOG_MODULE_HELPER`; the
[JSON helper](checks.md#json-helper-for-bash-executables) owns its
interface. [Maintenance](recovery.md#maintenance-windows) owns what a
maintenance window stops, what `mmwatchdog maintenance on` prints, and the
`every_check_group` window.

Exit codes are the same everywhere, and match MMX's:

| Code | Meaning |
| --- | --- |
| 0 | Success; nothing needs attention. Checks in `confirming_failure` or waiting for their first heartbeat or report count as healthy. |
| 1 | A confirmed failure: a `failed` or `confirming_recovery` check, a failed observation from `mmwatchdog check-now`, a silent node in `mmwatchdog fleet`, invalid configuration found by `mmwatchdog validate`, or a failed first attempt to deliver the notice of `mmwatchdog send-notice` to a destination |
| 2 | Not used by MMWatchdog; in MMX exit code 2 means a dryrun would change something |
| 3 | Unknown: health cannot be established, from an `unknown` check or a storage problem |
| 4 | Command error: bad arguments, an unreachable daemon, configuration that fails to load for a command other than `validate`, or a control command whose change could not be committed |
| 130 | Interrupted with Ctrl-C |

When several exit codes apply, failure comes first, then unknown.
For a node with one `failed` check and one `unknown` check,
`mmwatchdog status` exits 1 and lists the failed and unknown checks in the
attention list.

Control commands (`reload`, `maintenance`, `reset-recovery-attempts`,
`send-notice`, `fleet forget`, `fleet rotate-join-code`) take effect only
after the requested change is committed. A failed control
command leaves the previous state in place and exits 4.
For `send-notice` the change is the notice in the outbox: a notice that
cannot be queued is not sent.

## Status and diagnosis

`mmwatchdog status` reports the health of the node's checks. The daemon
produces a summary for `mmwatchdog status` and fleet reports: the exit code
and an attention list. The attention list gives the state, time, and message
of each check with an open failure incident or unknown health. Healthy,
`confirming_failure`, `waiting_for_first_heartbeat`, and
`waiting_for_first_report` checks are not listed.

| State | Meaning | Exit |
| --- | --- | --- |
| `ok` | Healthy, no incident open | 0 |
| `confirming_failure` | The check is failing, and its failing time has not reached `alert_after_failing_for` | 0 |
| `waiting_for_first_heartbeat` | A heartbeat check before its first deadline | 0 |
| `waiting_for_first_report` | A `node_reports` check before its first deadline | 0 |
| `failed` | A failure incident is open | 1 |
| `confirming_recovery` | A failure incident is open and the latest observation is healthy | 1 |
| `unknown` | No current observation establishes health | 3 |

[Timing and confirmation](checks.md#timing-and-confirmation) owns the failing
time: when it starts, and how long a check must be healthy before it ends.

When a check has an open failure incident, the check's state stays `failed`
even when the check's latest observation becomes unknown.
Status shows the open failure incident and the unknown observation.
A storage problem makes the exit code 3, unless a failure makes it 1, with a
`storage_error` explanation. JSON status carries each check's state,
latest observation, source location, effective timing, open incidents with
their evidence, maintenance windows, recovery attempts, the notification
queue, how far back the measurement history reaches, and on a fleet member
the hub connection.

The check fields are `last_observation`, `needs_fresh_observation`,
`announced_incident_id`, `last_received_at`, and `next_due_by`.
An observation's `status` remains `ok`, `failed`, or `unknown`.
The incident lifecycles are `failure_incident` and `cannot_check_incident`.
A cannot-check incident
opens when a check stays unknown for `alert_after_failing_for`.
The cannot-check incident's events are `cannot_check` and `can_check_again`.
The cannot-check incident's reminder says, for example,
`MMWatchdog has not been able to run myapp/http since 14:02 UTC; latest result: ...`.

The delivery queue is `delivery.queued_notifications`. The list
`delivery.failing_destinations` names each destination that MMWatchdog cannot deliver
to, with `destination`, `undelivered_alerts`, `latest_attempt_error`, and
`next_attempt_at`, including `mmhotline` destinations. The connection and
check-in status of `mmhotline` destinations also appears in
`mmhotline_destinations`
([`mmhotline`](../../reference/modules/mmhotline.md#errors-and-what-they-mean)). A failing destination makes `mmwatchdog status` exit 3, unless a check has failed: a failed check makes it exit 1 (failure takes precedence over health unknown). The output shows the failing destination in both cases.

`mmwatchdog fleet NODE` on the hub shows the status lines for a joined
node's failing destinations.

Recovery attempts appear as `systemd_unit_restart_attempts` and
`recovery_command_attempts`; [recovery attempt status](recovery.md#recovery-attempt-status)
owns their fields and readable form.

Status also says in a full sentence when MMWatchdog is doing less than a reader would
expect. Each line belongs to the section that owns its rule:

| `mmwatchdog status` line | Shown when | Owner |
| --- | --- | --- |
| `This node sends alerts to nobody.` | `alert_destinations.yml` says `destinations: {}` | [No destination](alerts.md#no-destination) |
| `myapp/myapp_answers_health_request is failing; no alert and no recovery action while postgres/postgres_service_is_running fails` | A failing check is kept quiet because a check that it names is failing | [One cause, one alert](checks.md#one-cause-one-alert) |
| `MMWatchdog cannot check myapp/myapp_answers_health_request; no alert while postgres/postgres_service_is_running fails` | A check that MMWatchdog cannot run is kept quiet because a check that it names is failing | [One cause, one alert](checks.md#one-cause-one-alert) |
| `myapp/myapp_answers_health_request is ok again; no recovery alert while postgres/postgres_service_is_running fails` | A check whose incident was announced before its quiet time is healthy again in the quiet time | [One cause, one alert](checks.md#one-cause-one-alert) |
| `myapp/myapp_answers_health_request is failing; no alert and no recovery action until its next run, because postgres/postgres_service_is_running was failing` | `postgres/postgres_service_is_running` is no longer failing, and `myapp/myapp_answers_health_request` has not run since the failing time of `postgres/postgres_service_is_running` ended | [One cause, one alert](checks.md#one-cause-one-alert) |
| `The hub has no destination named mail_administrator_chat. Destinations listed in send_alerts_only_to_destinations that the hub knows still receive alerts. If the hub knows none of the listed destinations, the check's alerts go to every destination whose alert_severities include the alert's severity.` | On a joined node, a check names a destination that the fleet settings do not hold | [Which alert goes to whom](alerts.md#which-alert-goes-to-whom) |
| `Destination team_chat: 3 alerts are not delivered. MMWatchdog tries again at 14:05 UTC. The latest attempt failed: the module slack reported: Slack answered 404: no_service. The address of the webhook does not work any more. Make a new incoming webhook in Slack and write its address into the file that webhook_url_file names.` | The latest attempt to deliver the oldest alert waiting for `team_chat` failed. Later alerts for `team_chat` wait in the queue after the oldest undelivered alert for `team_chat`. On a joined node, the node's queue of alerts awaiting hub acceptance reads `The MMWatchdog fleet hub has not accepted 2 alerts. ...` | [Delivery](alerts.md#delivery) |
| `measurement history reaches back 5 days 3 hours; the size limit of 256 MiB prevented keeping the requested 7 days` | The size limit removed readings that are younger than `keep_measurement_history_for` | [Measurement history](#measurement-history) |

```sh
sudo mmwatchdog status myapp          # one check group: every check, its rule, alert severity, and sources
sudo mmwatchdog check-now myapp       # fresh observations now
sudo mmwatchdog events myapp          # what happened, with frozen evidence
```

`mmwatchdog check-now` runs each check of the named check group once,
sequentially, with the check's configured `time_limit_per_run`.
The command prints the result, exit code, stdout and stderr, and measurements.
`mmwatchdog check-now` records nothing, sends no alert, requests no restart,
and runs no recovery command.

By default, `mmwatchdog check-now` asks the daemon and uses the daemon's
crash-loop history.
A requested run waits for any active scheduled run of
the requested check to finish. While the requested run executes, scheduled
runs of the requested check wait. Ctrl-C cancels `mmwatchdog check-now`.

With `--count-as-scheduled-run` the daemon instead moves the next scheduled
run of each check in the named check group to now, as happens when
maintenance starts or ends. The daemon waits for the requested runs, and
`mmwatchdog check-now` prints the requested runs' results.
The requested runs count as every scheduled run does, so a fixed check's
alert closes without waiting for the check's next interval.
Canceling stops only the waiting.
A run that had already started before the request does not count as a
requested run.

`mmwatchdog check-now CHECK_GROUP --without-daemon --config-dir DIR` runs locally from
a complete configuration directory for module and check authoring.

`mmwatchdog events CHECK_GROUP` shows the check group's most recent 100
events and the evidence MMWatchdog saved when each incident was confirmed.
MMWatchdog keeps the saved evidence unchanged: the confirming observation,
the observation's output, the source location, and the effective timing.
The evidence survives recovery and removal of the check from configuration,
so the diagnosis remains after the failing service or node is fixed.

`mmwatchdog fleet NODE` also shows the node's most recent 100 retained events.
`mmwatchdog events CHECK_GROUP` and `mmwatchdog fleet NODE` read older
retained events with `--last DURATION`, or with
`--since TIME` and optional `--until TIME`. An explicit span returns every
retained event in that span, including both ends. The time options have the
[same meaning and validation as readings](../../how-to/look-back-at-measurements.md#read-the-readings).

Text output replaces control characters in anything a
check, a module, or a node produced, so terminal output cannot be spoofed or
corrupted.

The readings commands draw the measurement history as charts with `--chart`
([charts of the readings](#charts-of-the-readings)).

## Measurement history

Some checks measure a number: how much memory the machine uses, how full a
filesystem is, how far a database replica is behind. A **reading** is one
measured value at one time, for example `memory_used_percent` was 41 at
14:02:30. Every run of such a check produces one reading for each of its
measurements
([checks with a limit on a number](checks.md#checks-with-a-limit-on-a-number)).

Each node keeps its readings on its own disk, so that someone who looks at a
problem can see what the machine did before and during it. Two node settings
say how much the node keeps, and both are required on every node:

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
alert_severity_when_this_node_goes_silent: sos    # the severity of the alert when this node stops reporting
alert_severity_when_this_node_has_rebooted: warn   # the severity of the notice after a reboot of this node
keep_measurement_history_for: 7d                  # this node keeps every reading of every measurement for 7 days
measurement_history_size_limit: 256MiB            # and never more than 256 MiB on this node's disk
```

What happens on the node, in order:

1. Each time MMWatchdog runs a check on its schedule, MMWatchdog stores the reading of each
   of the run's measurements, with the time of the run, unit, and limits in
   force. The stored numbers are `fail_when_measurement_above` and
   `fail_when_measurement_below`, beside the value and unit. A module of every
   kind is treated alike, and a check without a limit is stored
   like a check with one.

   The result of the run makes no difference.
   A maintenance window holds alerts, not readings.
   A run that measured nothing stores nothing.
   `mmwatchdog check-now`, which records nothing at all, stores no readings.
2. A reading that is older than `keep_measurement_history_for` is removed.
   MMWatchdog looks once a minute, when the daemon starts, and right after each
   reload.
3. When the stored readings reach `measurement_history_size_limit` before
   the readings reach the age set by `keep_measurement_history_for`, the
   oldest readings are removed first.
   `mmwatchdog status` then says how far
   back the history reaches, for example `measurement history reaches back 5
   days 3 hours; the size limit of 256 MiB prevented keeping the requested 7 days`. The line
   stays for as long as a reading that the size limit removed would still be
   younger than `keep_measurement_history_for`, and goes when the size limit
   is raised. The removal of readings because of the size limit is not a
   storage problem and does not change the exit code. The status line reports
   the shorter history as information.

Every reading stays as it was measured. MMWatchdog never replaces old readings with
averages, so a short spike that explains an outage is still there on the last
day of the history.

To keep a node's readings for longer, the administrator changes one line,
for example
`keep_measurement_history_for: 90d`, and raises
`measurement_history_size_limit` to give the readings room.

The measurement history survives a daemon restart. MMWatchdog stores readings
in `/var/lib/mmwatchdog/measurement_history.db`, which only root can read.
The measurement history's size limit is separate from the state database's
size limit ([storage limits](#retained-state-and-storage-limits)).

- **The size limit bounds `/var/lib/mmwatchdog/measurement_history.db`.**
  The file never grows beyond `measurement_history_size_limit`.
  SQLite keeps writes awaiting application to `measurement_history.db`
  in `measurement_history.db-wal`, beside the database.
  MMWatchdog keeps `measurement_history.db-wal` at about 1 MiB,
  which `measurement_history_size_limit` does not count.
- **A reload applies `keep_measurement_history_for` and
  `measurement_history_size_limit`.** A lower size limit applies from the
  reload on. MMWatchdog removes the oldest readings and shrinks
  `/var/lib/mmwatchdog/measurement_history.db` to the new limit in small
  steps, several times a second. Checks and alerts never wait for a large
  removal.

  A shorter `keep_measurement_history_for` applies at once: right after the reload,
  MMWatchdog removes the readings that are older. Changing the retention
  duration or the size limit needs no daemon restart.
  MMWatchdog never refuses a measurement history that is larger than a new limit.
- **MMWatchdog runs without a measurement history that it cannot use.** When the
  measurement history database cannot be opened, for example because the
  database is damaged, MMWatchdog still starts and runs checks.
  MMWatchdog keeps no readings. `mmwatchdog status` exits 3.
  `mmwatchdog status` reports a storage
  problem that names the database and says what to do: `cannot save readings in measurement history file
  /var/lib/mmwatchdog/measurement_history.db: ...; move the measurement history file aside
  and restart MMWatchdog to create an empty measurement history`.
- **A reading that MMWatchdog cannot store is a storage problem.**
  `mmwatchdog status` exits 3.
  `storage_error` names `measurement history write` until a reading is
  stored again. The check itself is not affected: its state, its alerts, and
  its recovery actions do not depend on the history.
- **JSON status says how far back the history reaches.** The JSON output
  of `mmwatchdog status` contains a `measurement_history` object with:
  - `oldest_reading_at` and `newest_reading_at`: the times of the oldest and
    newest stored readings. The time fields are absent while the node has
    stored no reading.
  - `keep_measurement_history_for` and `measurement_history_size_limit`:
    the values stated in the node's settings.
  - `cut_short_by_measurement_history_size_limit`: true while
    `mmwatchdog status` prints the message about the size limit shortening
    the measurement history.

A node that has joined a hub also sends its readings to the hub with its
reports, and the hub keeps them for every node
([measurement history on the hub](fleet.md#measurement-history-on-the-hub)).

### Looking at the readings

`mmwatchdog readings CHECK_GROUP [CHECK]` prints this node's stored
measurements as one block per measurement of each check. Every reading
follows, oldest first, with its time, its value, and an explicit mark when
it breaches its own recorded limit. A summary counts the readings and the
breaches, and gives the lowest, the highest with its time, and the latest
with its time. The [text layout](#the-text-of-the-readings-commands) is
the same with and without `--chart`.

`--last DURATION` defaults to `1h` and takes MMWatchdog's usual durations, including
whole days. `--since TIME [--until TIME]` selects an inclusive span of RFC
3339 times; its end defaults to command start. `--last` excludes `--since`,
and `--until` requires `--since`. `--json` uses the selected nodes, checks,
and time span and includes readings, recorded limits, and summaries.
The page [Look back at measurements](../../how-to/look-back-at-measurements.md#read-the-readings) defines the JSON fields
of the readings commands and gives a complete output example.
`--chart` draws each measurement instead of listing its readings
([charts of the readings](#charts-of-the-readings)).

The local readings command and the fleet readings commands for a node or a
node group run as root. All three readings commands read `state_dir` from
the complete configuration selected with `--config-dir` (default
`/etc/mmwatchdog`). All three readings commands open history read-only with
SQLite's ordinary WAL reader locks, need no daemon, and never block the
daemon's writes.

The readings commands show every reading without shortening or averages,
so no spike disappears. Users can narrow the requested time
span. A successful inspection exits 0 regardless of historical failures.

When limits change, each reading shows the limits at its own run.
Limits travel with readings to the hub, because node group charts also
need the recorded limits.
Module results remain values and units without configuration fields.
A measurement without limits says `only records`.

### The text of the readings commands

The text is laid out for a person who scans it, with and without
`--chart`; `--json` carries every value at full precision.

```text
node group web_servers · check group hardware · Oct 21 23:00:00 to Oct 22 23:00:00 UTC

processor_is_below_90_degrees
highest_temperature_celsius, degrees Celsius · fails above 90

  (the chart, or nothing for a node group without --chart)

  web02   531 readings, 1 beyond the limit
            Oct 22 17:30:50   91 above the limit
          lowest 37.25 · highest 91 at Oct 22 17:30:50 · latest 37.5 at Oct 22 22:59:19
  1 healthy, none beyond the limit: web01
```

- **The first line names the selection,** each name with its kind: the
  node for `fleet readings NODE`, the node group for `--node-group`, the
  check group, and the span in UTC.
- **Each measurement is a block,** after a blank line. The block's title is the
  check's name, and a second line gives the measurement, its unit, and its
  limits: `fails above 90`, `fails below 10`, both, `only records`, or
  `limits vary` with an explanation of what varying limits mean for the output.
- **The summary of a node** starts with a count line, then shows readings,
  then ends with `lowest · highest at TIME · latest
  at TIME`.
  For a measurement with limits, the count line includes the reading count
  and breach count, for example `531 readings, 1 beyond the
  limit`.
  When no reading breaches a limit, `none beyond the limit` follows the
  reading count. For a measurement that only records, the count line gives
  only the reading count.

  Without `--chart`, the node's summary lists every reading, one line each,
  with `above the limit` or `below the limit` beside a reading that breaches
  a limit. With `--chart`, the node's summary lists consecutive sequences of
  readings beyond the limit instead of individual readings.

  For a node group, each node's name starts the node's count line, and a
  node without readings says why. The worst node comes first.
  Healthy nodes share one line that names the healthy nodes.
- **Times are to the second,** without the date when the span lies within
  one UTC day (`17:30:50`), with the month and day within one year
  (`Oct 22 17:30:50`), and with the year otherwise. The first line always
  has the date. **Numbers** have four significant digits and no exponent,
  such as `37.25`, `0.01407`, and `123457`. The unit is in the block's
  title, not on every value.
- **On a terminal,** the title is bold, and the chart's axes, its labels,
  and its legend are dim, so the readings stand out. Output to a file or a
  pipe, and output with `NO_COLOR` set, has no styling.

### Charts of the readings

`--chart` prints one chart for each measurement instead of the list of its
readings. All three readings commands take `--chart`:

```text
mmwatchdog readings CHECK_GROUP [CHECK] --chart
mmwatchdog fleet readings NODE CHECK_GROUP [CHECK] --chart
mmwatchdog fleet readings --node-group NODE_GROUP CHECK_GROUP CHECK --chart
```

The time options select the span as before, and the
[text layout](#the-text-of-the-readings-commands) is the same as without
`--chart`. `--chart` and `--json` exclude each other: JSON already holds every reading,
and a program has no use for a drawing. The
[Look back at measurements](../../how-to/look-back-at-measurements.md#read-the-readings) shows a complete output.

- **Time runs across, the value runs up.** A chart is as wide as the
  terminal, or 80 characters when the output goes to a file or a pipe, and
  its drawing is 12 rows high. Each column stands for an equal slice of the
  time. The time axis under the drawing names the start, the middle, and
  the end, and the legend says how long one slice is.

  When the first reading comes more than twice the usual interval after
  the span's start, the chart starts at the first reading.
  For example, the history may start after the requested span's start.
  When the chart starts late, the legend says that no reading came before
  the first reading.
- **The value axis spans the readings and the limit.** The vertical axis
  starts at the lowest reading or limit and ends at the highest, rather than
  always starting at zero. The value axis labels name the lowest and highest
  values.
- **The readings are one line,** drawn with box-drawing characters: `─`
  while the value stays in its row, and `╭`, `╯`, `╮`, `╰`, and `│` where
  it rises or falls.
- **The limit is a dashed line** (`╌`), with its value on the value axis. A
  measurement with a limit above and a limit below gets two lines; a
  measurement that only records gets none. Each column draws the limit
  recorded with the reading that the line takes, so a limit that changed
  within the span shows as a step.
- **No spike and no dip disappears.** When a slice holds several readings,
  the column draws a vertical stroke (`│`) from the lowest to the highest
  of them, which leaves a limit's dashed line showing. The line goes on
  from the reading farthest beyond a recorded limit, or, without a breach,
  closest to a recorded limit.
  Each reading is compared with the limits recorded with the reading.
  With `fail_when_measurement_above`, the signed distance is the recorded
  upper limit minus the value. With `fail_when_measurement_below`, the
  signed distance is the value minus the recorded lower limit.
  With two limits, the comparison uses the smaller signed distance.
  For a measurement that only records, the line takes the highest reading
  of the slice, and the stroke still reaches down to the slice's lowest. A
  spike of one column shows as a peak, `╭╮`.
- **A gap stays visible.** A slice without a reading of its own repeats the
  latest earlier reading while that reading is no older than twice the usual
  interval between the measurement's readings, the median interval within
  the span. When the latest earlier reading is older than twice the median
  interval, the line breaks off: the node recorded nothing then,
  for example because it was down.
- **Readings beyond the limit are marked and listed.** A `▲` under the
  drawing marks each column whose line reading is beyond its limit. In the
  summary, each sequence of consecutive readings beyond the limit gets one
  line: the sequence's first and last time, the number of readings in the
  sequence, and the reading farthest beyond the limit. The sequence summaries
  replace the list of readings, so
  every reading beyond the limit is still accounted for in a sequence summary.
- **A node group shares one chart for each measurement.** A shaded band
  (`░`) reaches, in each column, from the lowest reading of any node in the
  column's slice to the highest. One node is the line in front of the band:
  the node whose reading has the smallest signed distance to a recorded
  limit in the span, using the column selection rule above.
  For a measurement that only records, the line shows the node with the
  highest reading in the span. When readings with and without limits mix,
  the comparison uses signed distances for limited readings and negative
  values for readings without limits.
  The legend names the node drawn as the foreground line.
  The nodes' summaries follow the chart, worst first.
- **Color only as an aid.** On a terminal, the line is red in the columns
  beyond the limit, and so are their `▲` marks. Nothing in the chart needs
  color to be read, and the environment variable `NO_COLOR` turns color
  off.
- **The legend is two short lines:** the symbols, each with a few words
  (`╌ limit`, `▲ beyond the limit`), then how long one column is and, for a
  late start, `no readings before TIME`. A part that does not fit the width
  moves to the next line.

## Retained state and storage limits

State lives in embedded SQLite under `/var/lib/mmwatchdog`, using a cgo-free Go
driver, so the binary is self-contained and no database service runs.

Persisted across daemon restarts: active incidents with their evidence, systemd unit
restart attempts and recovery command attempts, maintenance windows, heartbeat
receipts and deadlines, each check's latest observation, the notification outbox, the
local event history, and the [measurement history](#measurement-history). An
incident transition and its outbox entries commit
atomically. On restart, confirmed incidents stay open.
Unfinished confirmation starts fresh, so restarting MMWatchdog never
manufactures a recovery or a second failure alert.

`state.db` records the format of its layout. A release that changes the
layout migrates `state.db` in place the first time it opens it, on every
node, whatever its role, so no open incident is lost. The migration to
format 21 (MMWatchdog 0.2.0) records the hub's announced severity of each
open key, with no exact finding announced. On a joined node it moves the
record of a direct send that still waits to be delivered to the moment the
destination accepts it, as 0.2.0 records it: the record keeps only the
severities that left the queue, so the hub settles the waiting send only
once the destination has it. For the three security-update
checks, whose result changed from one incident to one incident per
[finding](checks.md#findings), the migration counts the check as changed:
its old single incident closes once with `closed_without_recovery`, and
its findings open as new incidents. A `state.db` of a format that the
running build cannot migrate is refused, with a message that names both
formats.

Storage is bounded so monitoring never exhausts the disk it watches.
Preventing monitoring from exhausting the monitored disk requires a storage
bound but does not determine the bound's size. So
each store that grows with time is bounded by settings in
`mmwatchdog_node_settings.yml`, and a user who needs more gives the store more room.
MMWatchdog keeps a limit fixed only where the limit has a concrete reason of its own.

| Store | Settings that bound the store | Default |
| --- | --- | --- |
| Measurement history | `keep_measurement_history_for`, `measurement_history_size_limit` | None: the two settings are required ([measurement history](#measurement-history)) |
| Event history | `keep_event_history_for`, `event_history_content_size_limit` | `24h`, `16MiB` |
| Outbox: the alerts that wait for delivery | `keep_undelivered_alerts_for`, `undelivered_alerts_content_size_limit` | None: the two settings are required |
| State database: everything in `state.db`, which includes the event history and the outbox | `state_database_size_limit` | `128MiB` |

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
alert_severity_when_this_node_goes_silent: sos    # the severity of the alert when this node stops reporting
alert_severity_when_this_node_has_rebooted: warn   # the severity of the notice after a reboot of this node
keep_measurement_history_for: 7d                  # keep every reading for 7 days
measurement_history_size_limit: 256MiB            # and never more than 256 MiB
keep_event_history_for: 7d                        # keep events for 7 days; without this line, 24 hours
event_history_content_size_limit: 32MiB                   # and never more than 32 MiB; without this line, 16 MiB
```

In words: this node keeps its readings for 7 days within 256 MiB, and its
events for 7 days within 32 MiB. The node keeps undelivered alerts for 24 hours
within 8 MiB. The state database keeps its default size limit of 128 MiB.

- No count bounds a store: each is bounded by its time, its size, or both.
- `state.db` also holds every check's state, and SQLite needs free pages to
  rewrite stored check states. So `state_database_size_limit` must be at
  least twice the sum of `event_history_content_size_limit` and
  `undelivered_alerts_content_size_limit`.
  On a hub, the sum also includes
  `undelivered_alerts_of_nodes_content_size_limit`
  ([alerts in a fleet](fleet.md#alerts-in-a-fleet)).
  A smaller value is a validation error that names the smallest valid value.
- A reload applies the stores' retention durations and content size limits.
  `state_database_size_limit` applies when the daemon starts.
- SQLite never shrinks a file on its own. A `state.db` that already holds
  more than its setting allows is refused, by `sudo mmwatchdog validate`
  before a deploy and by the daemon when it starts, with a message that names
  the size to state, such as `state.db already holds 140 MiB, more than
  state_database_size_limit allows (128 MiB); state at least 140MiB`.
- The measurement history is different: its size limit is the size of its
  own file, a reload applies it, and MMWatchdog shrinks the file to a lower limit by
  removing the oldest readings ([measurement history](#measurement-history)).
  A reading that is too old can be given up; a check's state cannot.

Incident evidence and check output have fixed limits, each for a separate reason:

| Store | Fixed limit | Reason |
| --- | --- | --- |
| Incident evidence | 16 KiB per incident, with explicit truncation | The evidence travels in every alert of the incident, and an alert must stay small enough for every destination |
| Check output | stdout 64 KiB (more is an execution error), stderr 16 KiB kept | A status answer and a report to the hub must stay small |

MMWatchdog sets no maximum on the number of alert destinations or on
`max_concurrent_checks`.

When a measurement history, event history, or outbox reaches a limit,
MMWatchdog removes the oldest readings, events, or undelivered alerts first.
Expired or overflowing deliveries leave a recorded delivery gap in status
and history.

When writes fail, monitoring and inspection continue. `mmwatchdog status`
exits 3 with `storage_error` naming each failing operation until the operation
succeeds again.
What cannot be recorded does not take effect. Recovery actions wait for a
durable recovery attempt reservation.
Heartbeats are refused with `503`, and maintenance and
`reset-recovery-attempts` commands fail and leave the previous state.

Observed transitions wait in bounded memory and commit when storage returns.

## The system clock

MMWatchdog times every wait on the monotonic clock of the node monitoring process.
The monotonic clock counts the time that passes and never steps. The system
clock, which a time service or a person can set, gives the times that
MMWatchdog records and shows. A time service steps the system clock when
the service finds the clock far off: at boot, when the clock was set by
hand, on a server whose clock
battery has run out, or after a virtual machine was paused. Such a step,
backward or forward, changes none of MMWatchdog's waits:

- A check runs a check interval after its last run.
- An incident opens after `alert_after_failing_for` of failing time, and
  the failing time ends after `recovered_after_healthy_for` of health. A
  raise to `sos`, reminder, restart, and recovery command each wait for
  the action's configured time.
- A maintenance window ends after its `--for`, and a heartbeat is overdue
  after its `max_time_between_heartbeats`.
- A delivery whose attempt failed is tried again after its delay, and a
  joined node sends an alert directly after
  `send_alert_without_hub_after_waiting_for`.
- On the hub, a joined node is silent after three report intervals without
  a report. A forward step sends no silence alert for a node that reports.

A system-clock step affects recorded times, comparisons with external times,
the crash-loop window, and record retention:

- **Recorded times.** MMWatchdog records and shows each time as the
  system-clock reading when the recorded occurrence happened. Examples
  include event times and observation times. A person compares recorded
  event and observation times with other programs' logs, which show the
  system clock too. The node monitoring process runs in UTC, so the process's
  log lines show UTC times without a zone mark, regardless of the node's
  time zone.

  Displayed deadlines keep the system-clock readings assigned when MMWatchdog
  set the deadlines. Examples include a maintenance window's end, a
  heartbeat's `next_due_by`, and "MMWatchdog tries again at" for a delivery.
  After a system-clock step, the displayed deadline is off by the step,
  and the wait ends on time.
- **Times from outside MMWatchdog.** A comparison with such a time counts on the
  system clock: the age of a file, the expiry of a certificate, the age of
  the package lists, the times of journal lines and of the kernel's log, the
  times in a joined node's report, MMHotline's `Date` header and `Retry-After`.
- **The crash-loop window.** A
  [`systemd_unit_crash_loop`](../../reference/modules/systemd_unit_crash_loop.md) check
  keeps a history of the monitored unit's counted restarts. MMWatchdog stores
  the history as module state between check runs and across daemon restarts.
  The stored restart times contain only system-clock readings, so the
  `systemd_unit_crash_loop` window uses the system clock. A forward step can
  drop the restarts counted before the step, and the alert then comes at most
  one window later.
  A backward step keeps the counted restarts longer.
- **Retention.** Each store with a retention duration, such as the event
  history, outbox, and measurement histories, calculates record ages from
  the earlier of two times. The first time is the current
  system-clock reading. The second time is the daemon's system-clock reading
  at startup plus the elapsed monotonic time since startup. So a forward
  step removes no record early, especially no alert waiting for delivery.
  Records written after a forward step stay longer by the size of the step,
  within the store's size limit.

  A backward step also keeps records longer.

**A step while MMWatchdog is stopped.** `state.db` stores times as system-clock
readings only. At daemon startup, MMWatchdog gives each stored time the
distance from the system clock that the stored time has at startup. MMWatchdog counts on
the monotonic clock from startup on. So a system-clock step while MMWatchdog
was stopped counts as elapsed time. After a forward step of an hour, a
maintenance window with less than an hour left expires at daemon startup.
After a backward step, the maintenance window lasts longer by the size of
the step.

For an alert waiting across the stop, MMWatchdog retries delivery after
the delay shown in the outbox, but no later than the doubling delay after
the alert's failed attempts. The delay starts at 1 second and doubles after
each failed attempt, up to 256 seconds. The cap also applies when MMWatchdog
starts with a clock that is behind. A receiver that requested a longer
wait, such as [MMHotline](../../reference/modules/mmhotline.md) with `Retry-After`, can
request another wait in response to the next delivery attempt.

Retention uses the system-clock reading at daemon startup. After a forward
step while MMWatchdog was stopped, MMWatchdog deletes records whose age on
the new clock exceeds the store's retention duration. Startup deletion
includes alerts waiting for delivery.

At every daemon startup, a failing time starts over if the failure incident
has not opened. At every hub startup, the hub restarts each joined node's
silence timer, allowing three fresh report intervals before declaring the
node silent.

## Deploy with MMX

[MMX](https://github.com/monkeymaxxing/mmx/) is the normal way to install and update MMWatchdog, its
configuration, modules, credentials, and systemd unit. MMX is not a runtime
dependency: once MMX exits, monitoring, recovery, alerts, and inspection run
locally. Systemd supervises the daemon and runs timers, such as recurring
[maintenance windows](recovery.md#deploys-and-recurring-windows).

Each release is a versioned archive per architecture (`arm64`, `amd64`) with
the cgo-free binary, systemd unit, starter configuration, MMX workflows and
inventories for a standalone node and for nodes joining a hub, documentation,
the [official extra modules](checks.md#official-extra-modules) with the
version of the release, module examples, source, and license notices. `SHA256SUMS` verifies the
archives and a manifest inside each verifies its files.

```sh
sha256sum --check SHA256SUMS
tar -xzf mmwatchdog-1.0.0-linux-amd64.tar.gz && cd mmwatchdog-1.0.0-linux-amd64
mmx run hub_and_standalone
```

The shipped workflow creates the `mmwatchdog` account with
`primary_group: mmwatchdog` and `create_home_directory: false`. A separate
directory task sets the home directory's owner and permissions. The shipped
workflow's `systemd_service` task states `state: running` and
`start_at_boot: true`, so installation starts MMWatchdog and enables startup
after a reboot.

The workflow stages the binary and the node's complete configuration set
outside `/etc/mmwatchdog`. The inventory variable
`mmwatchdog_config_source_dir` selects the node's complete configuration
set, relative to the project root. An MMX inventory group or node can
override `mmwatchdog_config_source_dir` for the node's role. The workflow
stages the deploy's two module directories beside the installed module
directories.

The workflow validates the staged configuration and referenced
credential files against the staged modules with `mmwatchdog-candidate validate
--config-dir DIR --official-extra-module-dir DIR --external-module-dir DIR`.
Only after validation does the workflow install the validated configuration
set, binary, unit, and module directories. The workflow then starts
MMWatchdog.

The project owns all deployed check-group files. The workflow removes check-group files
from the node when the check-group files are removed from the deployed
set, but preserves secrets. Before runtime validation, the workflow writes
each secret file that the inventory's `mmwatchdog_secret_files` lists, and
the joined-node playbook the join code, from MMX's encrypted secrets file,
the MMX vault.

An invalid candidate, a missing credential file, or a missing module leaves
the live configuration, installed modules, and daemon untouched. The secret
files are already written then. A standalone node or the hub reads a
destination's secret file at each delivery, so a changed secret is in use
although the deploy failed; joined nodes keep the hub's earlier copy until
the hub restarts or reloads. An unchanged
rerun keeps the running process, and changed files restart MMWatchdog.
When a deploy needs to restart MMWatchdog, the deploy opens a
whole-node maintenance window immediately before the restart with
`mmwatchdog maintenance on --every-check-group --for 15m --reason "mmwatchdog deploy"`
([deploys](recovery.md#deploys-and-recurring-windows)). The deploy closes the
maintenance window once MMWatchdog answers again. In a fleet, joined nodes
fetch alert settings from the hub ([fleet setup](fleet.md#set-up-a-fleet)). The
[Install on one node](../../how-to/install-on-one-node.md) gives the steps, updates, and
removal; [Commands reference](../../reference/commands.md) gives exact commands and limits.

**Modules.** A node gets the modules that are not core from two directories
of the project, and every deploy installs all of both
([modules](checks.md#modules)):

| In the project | On the node | Holds |
| --- | --- | --- |
| `official_extra_modules/` | `/usr/local/lib/mmwatchdog/official_extra_modules` | The [official extra modules](checks.md#official-extra-modules). The directory comes with the release, beside the `mmwatchdog` program |
| `mmwatchdog_external_modules/` | `/usr/local/lib/mmwatchdog/external_modules` | The [project's own modules](checks.md#the-projects-own-modules). Required even without modules. Keep the directory's shipped `README.md` |

The directory name `mmwatchdog_external_modules/` includes `mmwatchdog`
because an MMX project can also hold MMX's own external modules, which are
separate from MMWatchdog's modules.

Nothing is downloaded while MMWatchdog runs, and a deploy runs nothing of MMWatchdog on the
machine where MMX runs, which is often a Mac: the `mmwatchdog` file in the project
is the Linux program for the nodes.

- **The form of a module.** In `official_extra_modules/` and
  `mmwatchdog_external_modules/`, each module has a directory named `NAME/`.
  The module's `NAME/` directory holds `module_description.yml` beside the
  script `module.star` or, for a module of the project's own, the executable
  `module_executable`. An official extra module also has a `README.md`.
- **One module of each name.** A module of the project that has the name of
  a core module or of an official extra module fails the validation step of
  the deploy, and the error says to give the module another name. A team
  that wants an official extra module to work differently writes a module of
  its own under another name; nobody edits an official extra module, which
  the next release brings again.
- **The candidate module directories.** On each node, the workflow copies
  `official_extra_modules/` to
  `/usr/local/lib/mmwatchdog/candidate_official_extra_modules` and
  `mmwatchdog_external_modules/` to
  `/usr/local/lib/mmwatchdog/candidate_external_modules`. The candidate
  module directories hold the deploy's modules and are owned by root.
  All three candidate copies use
  `also_delete_entries_not_in_source: true`. Removed files cannot survive
  in staging, and unchanged copies report no change. The source directories
  `official_extra_modules/` and `mmwatchdog_external_modules/` are required:
  MMX refuses a missing source before connecting to any node. The installed
  module directories stay unchanged during staging.
- **Validation sees the modules of this deploy.** The workflow validates the
  new configuration with `--official-extra-module-dir` and
  `--external-module-dir` naming the two candidate directories, and with the
  candidate `mmwatchdog` program. So a module and the check or the
  [alert destination](alerts.md#destination-modules) that names it arrive in
  one deploy. A module that the configuration names and that neither the
  release nor the project holds fails the deploy before anything live
  changes, also when the node still has the module from an earlier deploy.
  If the candidate `official_extra_modules/` and candidate `mmwatchdog`
  program come from different MMWatchdog versions, validation fails the
  deploy before anything live changes.
  The workflow installs modules in MMWatchdog's default directories, so a
  configuration that states another `official_extra_module_dir` or
  `external_module_dir` fails the deploy during candidate validation.
- **The install comes last.** Replacing the installed module directories is
  the last change before the new configuration goes live. The workflow
  replaces `/usr/local/lib/mmwatchdog/official_extra_modules` with a copy of
  `/usr/local/lib/mmwatchdog/candidate_official_extra_modules`. The workflow
  replaces `/usr/local/lib/mmwatchdog/external_modules` with a copy of
  `/usr/local/lib/mmwatchdog/candidate_external_modules`. A node then has
  exactly the deploy's modules. Replacement removes modules removed from
  the project and files added by hand to the installed module directories.
- **A changed module restarts MMWatchdog.** MMWatchdog reads the description of each module
  that the configuration names when it starts, so the workflow restarts MMWatchdog
  when a module directory changed.
- **An update of MMWatchdog** brings the official extra modules of the new release
  with its first deploy, in the same step as the new `mmwatchdog` program.

## Run your own program as a systemd unit

MMWatchdog never starts a program and never keeps a program running.
Systemd starts programs and keeps programs running on every Linux server
that MMWatchdog supports.
MMWatchdog watches programs that systemd runs. So a program of
your own that MMWatchdog should watch runs as a **systemd unit**. A unit has
a small file telling systemd how to start the program and a name, such as
`myapp.service`, used by systemd and MMWatchdog.

The example is a web program named `myapp`. So far, someone has started
`myapp` by hand on the server, as the account `myapp`:

```sh
cd /srv/myapp
MYAPP_ENVIRONMENT=production ./bin/myapp --port 8080
```

The following unit file starts `myapp` with the directory, environment
value, and command from the manual startup example. In a unit file, each
comment starts with `#` on a separate line:

```ini
# /etc/systemd/system/myapp.service
[Unit]
# The text that "systemctl status myapp.service" shows.
Description=My web program

[Service]
# The account that the program runs as.
User=myapp
# The directory that the program starts in.
WorkingDirectory=/srv/myapp
# A value that the program reads from its environment.
Environment=MYAPP_ENVIRONMENT=production
# The command that starts the program. systemd needs the full path.
ExecStart=/srv/myapp/bin/myapp --port 8080
# Start the program again every time it ends, also after a normal exit.
Restart=always

[Install]
# Start the program when the server boots.
WantedBy=multi-user.target
```

MMX installs `/etc/systemd/system/myapp.service` with MMX's
[`systemd_unit`](https://github.com/monkeymaxxing/mmx/blob/main/docs/reference/modules/systemd_unit.md) module, and
enables and starts `myapp.service` with MMX's
[`systemd_service`](https://github.com/monkeymaxxing/mmx/blob/main/docs/reference/modules/systemd_service.md) module. MMX then
performs the equivalent of the following manual `systemctl` commands on
the server:

```sh
sudo systemctl daemon-reload                 # systemd reads the unit file
sudo systemctl enable --now myapp.service    # start myapp now and at every boot
systemctl status myapp.service               # shows "active (running)"
```

The check group that watches the unit:

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s                              # run each check every 30 seconds
    time_limit_per_run: 5s                        # one run of a check may take 5 seconds
    alert_after_failing_for: 5m                   # send the alert after 5 minutes of failing
    recovered_after_healthy_for: 1m               # send "recovered" after 1 healthy minute
    checks:
      myapp_service_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active       # fail when the unit myapp.service is not active
        alert_severity: sos
      myapp_answers_health_request:
        http:
          url: http://127.0.0.1:8080/health       # does the program answer with status 200?
          expected_http_status_codes: [200]
        alert_severity: sos
    restart_systemd_unit:
      systemd_unit: myapp.service                 # the unit that MMWatchdog restarts
      triggered_by_checks: [myapp_service_is_running, myapp_answers_health_request]
      restart_after_failing_for: 1m               # restart after 1 minute of failing
      max_attempts: 3                             # at most 3 restarts
      min_time_between_attempts: 1m               # at least 1 minute between restarts
      reset_attempts_after_healthy_for: 10m       # after 10 healthy minutes, all 3 restarts are available again
```

Who does what, in three situations:

- **The program crashes.** systemd starts `myapp` again, because the unit
  file says `Restart=always`. MMWatchdog sees a failure that lasts a few
  seconds and does nothing.
- **The program hangs.** systemd still reports the unit as running, but the
  health request fails. After 1 minute of failing, MMWatchdog runs
  `systemctl restart myapp.service`. If the health request has failed for 5
  minutes, MMWatchdog sends the `sos` alert
  ([restart a systemd unit](recovery.md#restart-a-systemd-unit)).
- **Someone runs `sudo systemctl stop myapp.service`.** MMWatchdog leaves the unit
  stopped: the person who stopped the unit decides when it runs again. After
  5 minutes MMWatchdog sends an alert that says the unit is stopped. A
  [maintenance window](recovery.md#maintenance-windows), opened before the
  stop, prevents the alert saying `myapp.service` is stopped.

`Restart=always` matters for the third situation, a stopped unit.
A program that ends on its own
with exit status 0 looks to systemd like a program that someone stopped, and
MMWatchdog does not start a stopped unit
([a stopped unit stays stopped](recovery.md#a-stopped-unit-stays-stopped)).
With `Restart=always`, systemd starts the program again after every exit,
so `myapp.service` is stopped only when someone asked for that.

Module pages that rely on "Run your own program as a systemd unit":
[`systemd_unit_state`](../../reference/modules/systemd_unit_state.md).

## Operation decisions

The operation decisions in the table below are settled. Revisit an operation
decision only with a concrete workflow the decision fails to support.

| Decision | Reason |
| --- | --- |
| One systemd-managed daemon per node, running as root | Direct systemd access and a chosen execution account for each shell check and each module that is not core; systemd already supervises processes. |
| Every wait counts on the monotonic clock; the times that MMWatchdog records and shows are on the system clock ([the system clock](#the-system-clock)) | A time service steps the system clock at boot and after a pause, and a step must neither hold back an alert nor send a false one; a person reads recorded times beside other programs' logs, which show the system clock. |
| Retention never counts back from a time later than the daemon's start plus the time since then | A server whose clock battery has run out boots years behind and queues an alert while its network is down; the step forward of its time service must not remove that alert before it is delivered. |
| Custom checks run as the unprivileged `mmwatchdog` account unless `run_as_user_account` says otherwise | Least privilege by default, with explicit elevation where a check needs it. |
| Embedded cgo-free SQLite; no database service | A self-contained binary with durable, atomic state. |
| YAML configuration as data, strictly validated, with no expression or template language | Readable and diagnosable; MMX generates variation when needed. |
| JSON for every machine interface | Agents never parse terminal text or infer outcomes from prose. |
| Node settings, check groups, and alert destinations have separate files under `--config-dir` | Each file holds one kind of content; validation sees the complete candidate. |
| The node settings hold no timings for checks | A reader of a check group sees its timings in that check group; a value that lives in another file is a value the reader misses under pressure. |
| A duration takes days, and a size is a number with a unit | `30d` and `256MiB` read at first sight; `720h` and a bare number of bytes do not. |
| A validation warning never blocks activation | A long title or duplicate alerts for a systemd unit reduce clarity without losing an alert. An author can intentionally give a cause check a lower severity than the check naming the cause. A password file that other accounts can read needs explicit permission acknowledgments. An error is kept for configuration that MMWatchdog cannot run as written or refuses for safety. |
| A rejected reload changes nothing | A bad edit can never stop monitoring. |
| Configuration edits retire incidents rather than resolve them | An edit is not evidence of recovery. |
| `max_concurrent_checks`, socket, state directory, receiver, and hub address change only on restart | An explicit restart boundary is simpler than live rebinding listeners or reopening storage. The complete restart list is under [Activate a configuration](#activate-a-configuration). |
| Every operation is a direct command with JSON output | Humans and agents share one set of operations. JSON is available for inspection commands; maintenance commands print text. |
| No web application or dashboard builder | The CLI and JSON output serve humans and agents without a web surface to secure. |
| Inspection runs locally or over SSH; no network inspection API | SSH already authenticates operators; the hub's fleet view covers remote nodes. |
| Every node states how much measurement history it keeps, as a time and a size, with no default | A reader of the node settings sees how far back the node can look, and how much disk the node's measurement history may take. |
| Every reading is kept as measured; old readings are never thinned into averages | A short spike that explains an outage disappears in an average, and a chart must mean the same thing whatever the age of its data. |
| The text of the readings commands shows times to the second and numbers to four significant digits, with the unit in each block's title; JSON keeps every value whole | A person scans the text for the shape and the breaches, which nanoseconds and fifteen decimals hide; a program reads JSON. |
| `--chart` prints its charts as text into the terminal's scroll; MMWatchdog has no full-screen terminal program | A chart can be pasted into a ticket or a chat, and the program that runs as root on every node needs no terminal interface library; `watch -n 30 mmwatchdog fleet` refreshes a view of the fleet. |
| A chart column strokes from the lowest to the highest reading of its slice, and its line takes the reading closest to the limit, or farthest beyond it | An average or the latest reading of the slice hides a short spike or dip, and the spike or dip is the reading that someone looks for. |
| A chart draws the readings as a line, and its value axis spans the readings and the limit, not zero | A line needs no zero baseline, and an axis from zero leaves most rows empty when the values sit far from zero, such as temperatures; the labels name both ends of the axis. |
| A chart starts at its first reading when that reading comes more than twice the usual interval after the start of the span | Columns from before any reading only take width, and the legend says where the readings start; a first reading within the usual interval is an ordinary start. |
| A node group's chart draws as its line the node that came closest to the limit or went farthest beyond it | A chart shows values against the limit. The summaries under the chart count readings beyond the limit, with the worst node first. |
| `measurement_history_size_limit` is the size of the history's own database file, a reload applies it, and MMWatchdog shrinks the file to a lower limit | The setting promises how much of the disk the readings take, so it bounds the file and not a count of readings; and the oldest readings can always be given up, so a lower limit never needs a restart or a refusal. |
| A reading that cannot be stored is a storage problem in status and changes nothing about its check; MMWatchdog starts without a measurement history that it cannot open, and removes many readings in small steps | Measurement history is for looking back, so checks, alerts, and restarts must never wait for measurement history or depend on stored readings. |
| Every store that grows with time is bounded by a time and a size that the node settings can change; a limit is fixed only where it has a concrete reason | Stores are bounded by time, size, or both; `state.db` has only a size limit. Bounded storage protects the monitored disk. A user who needs a longer history gives the store more room. "it is better practice" is a reason for a default, not for a fixed bound. |
| A store that reaches a limit says so in status or history | A gap is never silent. |
| MMX deploys MMWatchdog but monitoring never depends on it | Monitoring must keep working when deployment tooling is absent or broken. |
| The project's own modules are in the deployment project and deployed by MMX; nothing is downloaded while MMWatchdog runs | The project's version control holds every module of its own that runs as part of monitoring, and a reviewer can read it. |
| `mmwatchdog validate` takes the two module directories as options | A person validates a project's configuration on the machine where the project is, and a deploy validates against the modules that it is about to install, with one command. |
| Systemd supervises processes and runs timers; a program of the user's own runs as a systemd unit | One owner for process lifecycle and scheduling outside monitoring checks. |

Validation checks configuration and then the node files and accounts it names.
`--config-only` omits node prerequisites so a deployment project can be checked
on another machine. Before activation, reload repeats the configuration,
node-file, and account checks listed in
[configuration files and validation](#configuration-files-and-validation).
A refused reload keeps the running configuration.

Daemon start still reports node problems without requiring full node-file and
account validation before startup. See [validation](../../reference/commands.md#mmwatchdog-validate).
