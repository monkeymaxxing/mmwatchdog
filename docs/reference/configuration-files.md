# Configuration files reference

This page lists every file of a node's MMWatchdog configuration, and every
setting in those files: its type, whether the setting is required or has a
default, what the setting does, and which values MMWatchdog accepts. The
page also lists the storage limits, the limits that are fixed with their
reasons, and the validation errors that you meet when a file is wrong.

A **node** is a server that MMWatchdog watches. A **check** is one rule that
MMWatchdog tests again and again, such as "the health page answers". A
**check group** holds the checks of one thing, such as an application, and
states how often those checks run. A **destination** is a place where
alerts go, such as a Slack channel. A **module** does the work of one check
or one destination: `http` asks a web page, and `slack` posts to Slack.

This page is for looking things up. To do a job, follow a how-to page:
[Add a check](../how-to/add-a-check.md),
[Decide when a check alerts](../how-to/decide-when-a-check-alerts.md),
[Send alerts where your team sees them](../how-to/send-alerts-somewhere.md),
or [Set up a fleet](../how-to/set-up-a-fleet.md). The inputs of each module
are on the module's own page in the [module pages](modules/README.md).

## The configuration directory

MMWatchdog reads one directory, `/etc/mmwatchdog`, on each node:

```text
/etc/mmwatchdog/
├── mmwatchdog_node_settings.yml    the settings of this node
├── alert_destinations.yml          where alerts go (not on a node that joins a hub)
├── set_aside_findings.yml          optional, on the hub only: findings set aside after an investigation
├── check_groups/                   the check groups
│   ├── myapp.yml
│   └── machine.yml
└── secrets/                        files with passwords and tokens, mode 0600
```

- **`mmwatchdog_node_settings.yml`** holds the settings of the node that
  reads the file, such as how long the node keeps its readings. The file
  never holds a check group. See [Node settings](#node-settings).
- **`check_groups/`** holds one or more files, each with a `check_groups:`
  mapping. MMWatchdog reads every `.yml` file in `check_groups/`. Any other
  file, also a `.yaml` file or a directory, is a validation error. The
  directory as a whole needs at least one check group. A check group name
  that two files define is a validation error that names both places. See
  [Check group files](#check-group-files).
- **`alert_destinations.yml`** holds the destinations. A standalone node
  and a fleet hub need the file. A node that joins a hub gets its
  destinations from the hub, so `alert_destinations.yml` on that node is a
  validation error. See [The alert destinations file](#the-alert-destinations-file).
- **`set_aside_findings.yml`** is optional, and only a fleet hub may have
  it. It lists the findings that the hub sends no alert for below `sos`.
  See [The set-aside findings file](#the-set-aside-findings-file).
- **`secrets/`** is where the release's MMX project puts the files that
  settings ending in `_file` name, such as a Slack address. MMWatchdog
  itself reads a secret file from any path that a setting names. See
  [Secret files](#secret-files).

Every command that reads the configuration takes `--config-dir DIR`, with
`/etc/mmwatchdog` as the default. For example,
`sudo mmwatchdog validate --config-dir /tmp/candidate` checks a copy before
you install it. [Files that an install puts on a node](installed-files.md)
lists the owner and mode of each file.

**When MMWatchdog reads the files.** The node monitoring process, the
service `mmwatchdog`, reads the whole directory when it starts and at each
reload (`sudo systemctl reload mmwatchdog`, or `sudo mmwatchdog reload`).
A reload with an invalid configuration changes nothing: the node keeps
running with the configuration that it has. `sudo mmwatchdog validate`
checks the directory without changing anything.

**Settings that need a restart.** A reload refuses a change to
`max_concurrent_checks`, `socket`, `state_dir`, `module_state_dir`,
`heartbeat_receiver`, `fleet_hub_role.listen_on_address`, or
`fleet_hub_role.address_for_nodes`, and the addition or removal of
`fleet_hub_role`. A reload also refuses a change to
`state_database_size_limit` or
`fleet_hub_role.event_history_of_nodes_content_size_limit`, because these
two settings size a database file when the file opens. For any of these
settings, restart MMWatchdog: `sudo systemctl restart mmwatchdog`. A deploy
with the release's MMX project restarts MMWatchdog when a file changes.

## Rules for every file

These rules hold for `mmwatchdog_node_settings.yml`, every check group
file, and `alert_destinations.yml`:

- **No other field.** A misspelled or unknown field is a validation error,
  such as
  `check_groups.myapp.checks.health_page_answers.alert_severty: unknown field "alert_severty"`.
  In a check or a destination, the error also says where MMWatchdog looked
  for a module of that name, because a check names its module as a field.
- **One plain YAML document, which is a mapping.** MMWatchdog refuses a
  second document after `---`, a key written twice, anchors and aliases
  (`&name`, `*name`), the merge key `<<`, and explicit tags such as `!!str`.
- **Booleans are `true` or `false`.** MMWatchdog refuses `yes`, `no`,
  `on`, and `off`, so a value never changes its type by accident.
- **A value has the type that the setting takes.** For example,
  `max_concurrent_checks: "8"` fails with `expected an integer`.
- **At most 1 MiB for each file.** A larger file fails with
  `configuration exceeds 1 MiB`. A real configuration file is a few
  kilobytes, so the limit only stops a file that is not a configuration.

**How an error reads.** An error names the file, the line, the column, the
setting's path, and what is wrong:

```text
mmwatchdog: /etc/mmwatchdog/check_groups/myapp.yml:4:5: check_groups.myapp.time_limit_per_run: 45s exceeds check_every 30s
```

For a required setting that is missing, the line and column are those of
the mapping that should hold the setting. [Validation errors](#validation-errors)
lists the errors that you meet most.

## Values

Settings take these kinds of value:

| Kind | Written as | Rules |
| --- | --- | --- |
| Duration | A number with a unit: `100ms`, `30s`, `5m`, `26h`, `1h30m`, or `7d` | `d` takes a whole number of days. Every other form follows Go's durations: `ms`, `s`, `m`, `h`, combined. Never negative, and at most 64 characters. Each setting below says its own smallest value |
| Size | A whole number with `MiB` or `GiB`: `256MiB`, `20GiB` | From 1. `256MB`, `256 MiB`, and `0.5GiB` are refused |
| Alert severity | `info`, `warn`, or `sos` | `sos` is the most urgent. A destination takes the alerts of the severities that the destination lists |
| Name | Letters, digits, hyphens, and underscores, such as `myapp` or `web-1` | 1 to 64 characters, and the first is a letter or a digit. Applies to check groups, checks, destinations, and node groups |
| Path | An absolute path, such as `/etc/mmwatchdog/secrets/api-token` | A relative path is refused |
| List | `[info, warn, sos]`, or one item on each line after `-` | |

Reserved names: no check group may be named `every_check_group` (the name
of whole-node maintenance) or `node_reports` (the built-in check group of a
fleet). No destination may be named `hub`, the name of a joined node's
queue to its hub.

## Node settings

`mmwatchdog_node_settings.yml` holds the settings of one node. The smallest
valid file states six settings, which have no default:

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h                 # keep an alert that cannot be delivered for 24 hours
undelivered_alerts_content_size_limit: 8MiB      # and keep at most 8 MiB of such alerts
alert_severity_when_this_node_goes_silent: sos   # the severity of the alert when this node stops reporting
alert_severity_when_this_node_has_rebooted: warn # the severity of the notice after this node rebooted
keep_measurement_history_for: 7d                 # keep every reading of every measurement for 7 days
measurement_history_size_limit: 256MiB           # and never more than 256 MiB of this node's disk
```

MMWatchdog names a node by its host name, so every node needs a host name
of its own. MMWatchdog refuses to start on a machine without a host name.
Set one with `sudo hostnamectl set-hostname NAME`.

Every setting of the file:

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| `keep_undelivered_alerts_for` | duration, above `0s` | Required | How long the node keeps an alert that waits for delivery. [Storage limits](#storage-limits) |
| `undelivered_alerts_content_size_limit` | size | Required | How much alert content may wait for delivery. [Storage limits](#storage-limits) |
| `alert_severity_when_this_node_goes_silent` | alert severity | Required | The severity of the alert when this node stops reporting. [Silence and node groups](#silence-and-node-groups) |
| `alert_severity_when_this_node_has_rebooted` | alert severity | Required | The severity of the notice that the node sends after a reboot. [Alert format](alert-format.md) shows the notice |
| `keep_measurement_history_for` | duration, above `0s` | Required | How long the node keeps each reading. [Measurement history](#measurement-history) |
| `measurement_history_size_limit` | size | Required | How much disk the readings may take. [Measurement history](#measurement-history) |
| `keep_event_history_for` | duration, above `0s` | `24h` | How long the node keeps its events, which `mmwatchdog events` shows. [Storage limits](#storage-limits) |
| `event_history_content_size_limit` | size | `16MiB` | How much event content the node keeps. [Storage limits](#storage-limits) |
| `state_database_size_limit` | size | `128MiB` | How large `state.db` may grow. [Storage limits](#storage-limits) |
| `max_concurrent_checks` | whole number, from 1 | `8` | How many checks run at the same time on this node |
| `socket` | path | `/run/mmwatchdog/mmwatchdog.sock` | The Unix socket where commands such as `mmwatchdog status` reach the service |
| `state_dir` | path | `/var/lib/mmwatchdog` | Where MMWatchdog keeps its databases |
| `official_extra_module_dir` | path | `/usr/local/lib/mmwatchdog/official_extra_modules` | [Module directories](#module-directories) |
| `external_module_dir` | path | `/usr/local/lib/mmwatchdog/external_modules` | [Module directories](#module-directories) |
| `module_state_dir` | path | `/var/lib/mmwatchdog-module-state` | [Module directories](#module-directories) |
| `heartbeat_receiver` | mapping | None: no receiver | Receives heartbeats from other machines over HTTPS. [heartbeat_receiver](#heartbeat_receiver) |
| `raise_to_sos_after_this_node_is_silent_for` | duration, at least `1s` | None: time never raises the alert | [Silence and node groups](#silence-and-node-groups) |
| `send_silence_alerts_of_this_node_only_to_destinations` | list of destination names | None: the alert goes by severity | [Silence and node groups](#silence-and-node-groups) |
| `this_node_is_in_node_groups` | list of names | None: the node is in no node group | [Silence and node groups](#silence-and-node-groups) |
| `raise_to_sos_when_every_node_of_these_node_groups_is_silent` | list of names | None | [Silence and node groups](#silence-and-node-groups) |
| `fleet_hub_role` | mapping | None: the node is not a hub | Makes this node the fleet hub. [fleet_hub_role](#fleet_hub_role) |
| `join_fleet_hub` | mapping | None: the node joins no hub | Makes this node report to a hub. [join_fleet_hub](#join_fleet_hub) |

A node states at most one of `fleet_hub_role` and `join_fleet_hub`: a hub
cannot join another hub. A node with neither is a **standalone node**.

`mmwatchdog_node_settings.yml` never holds check groups or the timings of
checks. Every timing of a check is in the file of its check group, so a
reader finds the timings of a check in one place.

### socket, state_dir, and max_concurrent_checks

These three settings have defaults that suit every node. Leave them out
unless a node needs another place.

- **`state_dir`** must be a directory owned by root with mode `0700`, and
  not a symbolic link. MMWatchdog checks the owner and mode at startup,
  before MMWatchdog opens a database, and refuses to start otherwise:
  other accounts must not read or change the history of the node.
- **`socket`** is the path that commands use. A command for a node with
  another socket needs `--socket PATH`, such as
  `sudo mmwatchdog status --socket /run/other/mmwatchdog.sock`.
- **`max_concurrent_checks`** has no upper limit. When that many checks
  are running, a check that is due waits for a free place.

### Module directories

Three settings name the directories of modules that are not built into
`mmwatchdog`: the **official extra modules** of the release, such as
`slack`, and the **modules of your own**. Each setting takes an absolute
path:

| Setting | Default | What the directory holds |
| --- | --- | --- |
| `official_extra_module_dir` | `/usr/local/lib/mmwatchdog/official_extra_modules` | The official extra modules of the installed release, one directory each |
| `external_module_dir` | `/usr/local/lib/mmwatchdog/external_modules` | Your own modules, one directory each |
| `module_state_dir` | `/var/lib/mmwatchdog-module-state` | One directory for each check whose module is not core, as `CHECK_GROUP/CHECK`, for what the module keeps from one run to the next |

- **The release's MMX workflows install modules in the two default
  directories** and refuse a configuration that states another
  `official_extra_module_dir` or `external_module_dir`. State another
  directory only when you install modules in another way.
- **`module_state_dir` is a path that does not exist yet.** MMWatchdog
  creates the directory, in a parent directory that only its owner can
  change. MMWatchdog refuses a directory at that path that MMWatchdog did
  not create, so MMWatchdog never changes or removes another program's
  data. `sudo mmwatchdog validate` checks the parent directory when a check
  uses a module that is not core.
- [Writing a module](writing-modules.md) says what a module keeps in its
  state directory.

### Silence and node groups

On a fleet, the hub alerts when a joined node stops reporting, and two
joined nodes alert when the hub stops reporting. Each node's own settings
say how that alert about the node goes out:

| Setting | What the setting does |
| --- | --- |
| `alert_severity_when_this_node_goes_silent` | Required on every node. The severity of the alert when this node stops reporting. For a standalone node the setting is required too, but nothing sends the alert |
| `raise_to_sos_after_this_node_is_silent_for` | Raises that alert to `sos` once the node has been silent for the duration, such as `15m`. At least `1s` |
| `send_silence_alerts_of_this_node_only_to_destinations` | Sends every alert about this node's silence only to the destinations named, such as `[on_call_phone]`, whatever their `alert_severities` say. `[]` sends no alert |
| `this_node_is_in_node_groups` | The node groups that this node is in, such as `[web, eu]`. A node group exists because nodes name the node group. At most 100 names, each once |
| `raise_to_sos_when_every_node_of_these_node_groups_is_silent` | Raises the alert to `sos` while every node of one of these node groups is silent. Each name must be in this node's `this_node_is_in_node_groups` |

A node can be in at most 100 node groups. Reason: a node sends its node
groups in its join request, which the hub limits to 64 KiB because the hub
does not know the node yet, and a node with a valid configuration must
always be able to join.

On a standalone node and on the hub, each name in
`send_silence_alerts_of_this_node_only_to_destinations` must be a
destination in `alert_destinations.yml`. A joined node has no destination
file, so nothing refuses a wrong name there. See
[A destination that the hub does not have](#a-destination-that-the-hub-does-not-have).
[Watch a fleet](../how-to/watch-a-fleet.md) and
[Change a fleet](../how-to/change-a-fleet.md) show these settings at work.

### A destination that the hub does not have

A joined node names destinations that live on the hub, in a check's
`send_alerts_only_to_destinations` or in
`send_silence_alerts_of_this_node_only_to_destinations`. Validation cannot
refuse a name that the hub lacks, so MMWatchdog never drops an alert
because of such a name:

- The named destinations that the hub has still get the alert.
- When the hub has none of the named destinations, the alert goes by
  severity: to every destination whose `alert_severities` include the
  alert's severity.
- `sudo mmwatchdog status` shows each missing name under the check, as
  `The hub has no destination named on_call_phone.`, and the JSON of
  `status` lists the names in `alert_destinations_the_hub_does_not_have`.
  For a node's silence settings, the hub shows the field on the node's
  check in `sudo mmwatchdog status node_reports`.

### heartbeat_receiver

`heartbeat_receiver` lets jobs on other machines report to this node over
HTTPS. A job on the node itself reports through the local socket and needs
no receiver ([Report from a scheduled job](../how-to/report-from-a-scheduled-job.md)).

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
heartbeat_receiver:
  listen_on_address: 10.0.0.5:8444                          # the local address and port where MMWatchdog listens
  tls_certificate_file: /etc/mmwatchdog/secrets/server.crt  # the receiver's certificate
  tls_private_key_file: /etc/mmwatchdog/secrets/server.key  # the certificate's private key
```

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| `listen_on_address` | host and port | Required | See [Listening addresses](#listening-addresses) |
| `tls_certificate_file` | path | Required | The certificate that the receiver shows |
| `tls_private_key_file` | path | Required | The private key of the certificate, a [secret file](#secret-files) |
| `secret_readable_by_group`, `dangerously_let_every_account_read_secret` | boolean | `false` | Allow a private key that other accounts can read. See [Secret files](#secret-files) |

`sudo mmwatchdog validate` loads the certificate and the key, and fails
when the pair does not load.

### fleet_hub_role

`fleet_hub_role` makes this node the **fleet hub**: the node that collects
the reports of the other nodes, keeps their readings, and delivers their
alerts. [Set up a fleet](../how-to/set-up-a-fleet.md) installs a hub.

```yaml
# hub01, /etc/mmwatchdog/mmwatchdog_node_settings.yml
fleet_hub_role:
  listen_on_address: 0.0.0.0:8445                         # the hub listens on port 8445 of every interface
  address_for_nodes: hub01.example.com:8445               # the address and port that the nodes connect to
  report_every: 30s                                       # each node reports every 30 seconds
  alert_after_hub_unreachable_for: 3m                     # a node alerts that it cannot reach the hub after 3 minutes
  send_alert_without_hub_after_waiting_for: 30s           # a node sends an alert itself when the hub has not taken it for 30 seconds
  undelivered_alerts_of_nodes_content_size_limit: 32MiB   # the hub keeps at most 32 MiB of the nodes' alerts that wait for delivery
  keep_measurement_history_of_nodes_for: 7d               # the hub keeps the readings that the nodes send for 7 days
  measurement_history_of_nodes_size_limit: 20GiB          # and lets those readings take at most 20 GiB of its disk
```

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| `listen_on_address` | host and port | Required | Where the hub accepts reports. See [Listening addresses](#listening-addresses) |
| `address_for_nodes` | host and port | Required | The address that joined nodes connect to, such as `hub01.example.com:8445`. The host is required, the port is from 1 to 65535. The join code carries the address |
| `report_every` | duration, at least `1s` | Required | How often each joined node reports. Three missed reports make a node silent |
| `alert_after_hub_unreachable_for` | duration, at least `1s` and at least twice `report_every` | Required | How long a joined node waits before the node alerts that the hub is unreachable. Twice `report_every` at least, so one late report does not alert |
| `send_alert_without_hub_after_waiting_for` | duration, at least `1s` | Required | How long a joined node waits for the hub to accept an alert before the node delivers the alert to the destinations itself |
| `undelivered_alerts_of_nodes_content_size_limit` | size | Required | How much of the nodes' alert content the hub keeps while the alerts wait for delivery. One node's alerts may hold a quarter of it, so a node with many alerts, or a hostile one, cannot fill it for the others; the node delivers an alert beyond its share itself, without the hub |
| `keep_measurement_history_of_nodes_for` | duration, above `0s` | Required | How long the hub keeps the readings that the nodes send. [Measurement history](#measurement-history) |
| `measurement_history_of_nodes_size_limit` | size | Required | How much of the hub's disk those readings may take. All nodes share it: when it is full, the oldest readings go, of any node, so one node that sends many readings can push out the history of the others |
| `keep_event_history_of_nodes_for` | duration, above `0s` | `7d` | How long the hub keeps the nodes' events |
| `event_history_of_nodes_content_size_limit` | size | `4GiB` | How much of the nodes' event content the hub keeps |
| `raise_to_sos_when_silent_nodes_reach_count` | whole number, from 1 | None | Raises the silence alerts to `sos` when this many joined nodes are silent at the same time |
| `raise_to_sos_when_silent_nodes_reach_percent` | number above 0, at most 100 | None | The same, as a percentage of the joined nodes, written without `%`, such as `20` |
| `send_newly_affected_nodes_every` | duration, at least `1m` | `6h` | How often the hub sends the updates about nodes that an exact finding already announced newly affects, for every key together, on the multiples of this time since 1970-01-01 00:00 UTC, such as 00:00, 06:00, 12:00, and 18:00 UTC. The default gives MMHotline's triage agent at most four batches of new nodes a day. A new exact finding, a higher severity, and every `sos` node go out at once anyway. [One alert per exact finding](../changing-mmwatchdog/design/fleet.md#one-alert-per-exact-finding) |
| `joined_node_limit` | whole number, from 1 | `1000` | How many nodes may join. The default is far above the fleets that MMWatchdog is tested with and well within the hub's database, so that a leaked join code cannot fill the database with nodes. A join beyond the limit is refused with a message that says to raise it |

- **The three fleet timings have no upper limit,** so a fleet with slow
  links can state longer timings. The joined nodes take the timings from
  the hub.
- **State at most one of the two silence thresholds.** With both, the
  validation error says to keep one. With neither, no count of silent
  nodes raises the alerts.
- **Keep `send_alert_without_hub_after_waiting_for` at several seconds or
  more,** such as `30s`. At boot, the network can come up after
  MMWatchdog starts, so the node's first attempt to send an alert to the
  hub can fail with `network is unreachable`. With a wait of a second or
  two, the node then sends its reboot notice to the destinations itself,
  and the hub cannot combine it with the reboot notices of other nodes.
- **The hub is a node too.** The hub's own readings and events follow the
  hub's own node settings, outside `fleet_hub_role`.

### join_fleet_hub

`join_fleet_hub` makes this node a **joined node**, which reports to a hub
and sends its alerts through the hub's destinations.

```yaml
# web01, /etc/mmwatchdog/mmwatchdog_node_settings.yml
join_fleet_hub:
  join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code   # the code that lets this node join the hub
```

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| `join_code_file` | path | Required | The file that holds the join code, which `sudo mmwatchdog fleet join-code` prints on the hub. A [secret file](#secret-files) |
| `prefer_this_node_as_hub_outage_alerter` | boolean | `false` | Asks the hub to choose this node as one of the two nodes that alert when the hub stops reporting |
| `secret_readable_by_group`, `dangerously_let_every_account_read_secret` | boolean | `false` | See [Secret files](#secret-files) |

A joined node has no `alert_destinations.yml`: delete the file before the
node joins. [Set up a fleet](../how-to/set-up-a-fleet.md) and
[Change a fleet](../how-to/change-a-fleet.md) cover joining, the join
code, and the hub-outage alerter nodes.

## Listening addresses

`fleet_hub_role.listen_on_address` and `heartbeat_receiver.listen_on_address`
name the local host and port where MMWatchdog accepts connections:

- `10.0.0.5:8445` listens on one interface, such as an internal network.
- `0.0.0.0:8445` listens on every IPv4 interface.
- `[::]:8445` listens on every IPv6 interface.

An address without a host, such as `:8445`, is refused. Reason: the
configuration must show which interfaces accept connections. MMWatchdog
listens on no network port unless one of the two settings says so.

## Check group files

Each file in `check_groups/` holds a `check_groups:` mapping with one or
more check groups. A check group holds its checks under `checks`, each
with a name that you choose:

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:                                # the check group's name
    check_every: 30s                    # run each check every 30 seconds
    time_limit_per_run: 5s              # one run of a check may take 5 seconds
    alert_after_failing_for: 1m         # alert after 1 minute of failing
    recovered_after_healthy_for: 30s    # the problem is over after 30 healthy seconds
    checks:
      service_is_running:               # a check's name
        systemd_unit_state:             # the check's module
          systemd_unit: myapp.service   # the module's inputs
          fail_when: the_unit_is_not_active
        alert_severity: sos
      health_page_answers:
        http:
          url: http://127.0.0.1:8080/health
          expected_http_status_codes: [200]
        alert_severity: sos
    restart_systemd_unit:               # restart myapp when the health page fails
      systemd_unit: myapp.service
      triggered_by_checks: [health_page_answers]
      restart_after_failing_for: 1m
      max_attempts: 3
      min_time_between_attempts: 1m
      reset_attempts_after_healthy_for: 10m
```

`mmwatchdog status` shows these checks as `myapp/service_is_running` and
`myapp/health_page_answers`.

### Settings of a check group

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| `check_every` | duration, at least `100ms` | Required | How often each check of the check group runs |
| `time_limit_per_run` | duration, above `0s`, at most `check_every` | Required | How long one run of a check may take. A run that reaches the limit is ended |
| `alert_after_failing_for` | duration, `0s` or more | Required, except on a direct [event check](#event-checks) | How long a check must be failing before its incident opens and its alert goes out |
| `recovered_after_healthy_for` | duration, `0s` or more | Required | How long a check must be healthy before its failure is over and an open incident resolves |
| `checks` | mapping of check names to checks | One of `checks`, a direct check, or `heartbeat` | The named checks. See [Settings of a check](#settings-of-a-check) |
| `restart_systemd_unit` | mapping | None | Restarts a systemd unit when a check fails. See [restart_systemd_unit](#restart_systemd_unit) |
| `recovery_command` | mapping | None | Runs a command when a check fails. See [recovery_command](#recovery_command) |
| `heartbeat` | mapping | None | Makes the check group a heartbeat. See [Heartbeat check groups](#heartbeat-check-groups) |

- **There are no node-wide defaults for the four timings.** Each ordinary
  check group states all four, so the timings of a check are in one file.
  [Decide when a check alerts](../how-to/decide-when-a-check-alerts.md)
  explains how to choose them, and the
  [failing time](../how-it-works.md) explains how they work together.
- **Some modules need more time for each run.** A module that samples over
  a time, such as `node_resource` measuring the CPU, needs a
  `time_limit_per_run` longer than its sample. The validation error names
  the time, such as `must exceed 1s for ...`.
- **A check cannot state its own `check_every` or `time_limit_per_run`.**
  A check that must run at another pace goes in a check group of its own.
- **A setting of a check belongs on each check.** In a check group with
  `checks`, writing `alert_severity`, `raise_to_sos_after_failing_for`,
  `only_record_readings`, a limit, `send_alerts_only_to_destinations`, or
  `no_alerts_or_recovery_actions_while_these_checks_fail` on the check
  group is a validation error that says to set it on each check.

### Settings of a check

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| The module | the module's name as the key, its inputs below | Required: exactly one module | What the check tests, such as `http` or `tcp`. The [module pages](modules/README.md) list each module's inputs |
| `alert_severity` | alert severity | Required on a check that can alert | The severity of the check's alerts. A check that MMWatchdog cannot run alerts with the same severity |
| `raise_to_sos_after_failing_for` | duration, at least `1s` | None | Raises the check's open alert to `sos` once the check has been failing for the duration |
| `only_record_readings` | boolean | `false` | Records the check's measurements and never alerts. See [Checks that measure](#checks-that-measure) |
| `fail_when_measurement_above`, `fail_when_measurement_below` | mapping of measurement names to numbers | None | Limits on the check's measurements. See [Checks that measure](#checks-that-measure) |
| `send_alerts_only_to_destinations` | list of destination names | None: the alerts go by severity | Sends every alert of the check to these destinations only, whatever their `alert_severities` say. `[]` sends no alert |
| `no_alerts_or_recovery_actions_while_these_checks_fail` | list of `CHECK_GROUP/CHECK` | None | Keeps this check quiet while one of the named checks, its causes, fails. [Stop alerts that follow another failure](../how-to/stop-alerts-that-follow-another-failure.md) |
| `alert_after_failing_for`, `recovered_after_healthy_for` | duration, `0s` or more | The check group's values | The check's own waiting times. Only on a named check under `checks` |
| `secret_readable_by_group`, `dangerously_let_every_account_read_secret` | boolean | `false` | See [Secret files](#secret-files) |

- **The module is a key of the check.** A check names exactly one module:
  a core module, an official extra module, or a module of your own, each
  by the module's name. Two module keys on one check are a validation
  error.
- **Destination names must exist.** On a standalone node and on a hub,
  each name in `send_alerts_only_to_destinations` must be a destination in
  `alert_destinations.yml`. On a joined node the hub's destinations count:
  `sudo mmwatchdog validate` on the node warns about a name that the node's
  saved copy of the hub's settings does not have. See
  [A destination that the hub does not have](#a-destination-that-the-hub-does-not-have).
- **A check names its causes in other check groups too,** written as
  `CHECK_GROUP/CHECK`. A check cannot name itself or name one check twice,
  and checks that name each other in a circle are refused: while every
  check of the circle fails, MMWatchdog would send no alert at all.
- **`mmwatchdog config --json` shows each named check's waiting times** and
  where each one is stated: `alert_after_failing_for_stated_on` and
  `recovered_after_healthy_for_stated_on` are `check` or `check_group`.

### A direct check

A check group with a single check can hold the check directly, without
`checks`. The check is then named after its module:

```yaml
# /etc/mmwatchdog/check_groups/database_port.yml
check_groups:
  database_port:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    tcp: 127.0.0.1:5432                 # the one check, written on the check group
    alert_severity: sos
```

`mmwatchdog status` shows the check as `database_port/tcp`. The settings
of the check, such as `alert_severity`, go on the check group's mapping. A
direct check cannot share its check group with named checks or a
heartbeat.

### Event checks

Two kinds of check alert at the first new event, so they have no use for
`alert_after_failing_for`:

- an [`out_of_memory_kills`](modules/out_of_memory_kills.md) check, which
  fails when the kernel ends a process to free memory.
- a [`log_lines`](modules/log_lines.md) check with
  `fail_when_a_new_line_contains` or `fail_when_a_new_line_matches_regex`.

A direct event check's check group takes no `alert_after_failing_for`. A
named event check under `checks` uses the check group's value only for its
cannot-check alert, so a short moment in which the check cannot read its
events sends nothing, and `alert_after_failing_for` on the check itself is
a validation error. An
event check takes `recovered_after_healthy_for`, which counts from the
first run that finds no new event. A `log_lines` check with
`fail_when_no_new_line_for` is not an event check and uses all four
timings.

### Checks that measure

Many modules measure numbers, such as `node_resource` with
`memory_used_percent`. Each module page lists the module's measurements
and units under "Measurements". A check of such a module makes a choice:

- **Alert:** state `alert_severity` and a limit,
  `fail_when_measurement_above` or `fail_when_measurement_below`. A reading
  strictly above or below the limit fails the check.
- **Only record:** state `only_record_readings: true`, with no limit and no
  `alert_severity`. The readings go to the
  [measurement history](#measurement-history), and the check never alerts.

```yaml
# /etc/mmwatchdog/check_groups/machine.yml
check_groups:
  machine:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    checks:
      memory_is_below_90_percent:
        node_resource: memory
        fail_when_measurement_above:
          memory_used_percent: 90       # fails above 90 percent
        alert_severity: warn
      cpu_use_is_recorded:
        node_resource: cpu
        only_record_readings: true      # records the readings, never alerts
```

- A measuring check that states neither `alert_severity` nor
  `only_record_readings` is a validation error, so the choice is always
  written down.
- A few measuring modules can fail without a limit, such as `ping` when no
  answer comes. Such a module takes `alert_severity` without a limit.
- A limit names a measurement of the check's module. A name that the
  module does not measure is a validation error that lists the module's
  measurements.
- For a core module, a limit on a measurement that ends in `_percent` is
  from 0 to 100, and every other limit is 0 or more. A temperature limit,
  `highest_temperature_celsius`, may be negative, because hardware can run
  below zero Celsius. For a module that is not core, a limit can be any
  number, also a negative one.
- The message of a run names each measurement that has a limit, with its
  reading and its limits, such as `memory_used_percent is 50 (fails above
  90, below 5)`. A check of a core measuring module without a limit names
  every reading. A check of another module shows the module's own message.
- A check that only records cannot be a cause in
  `no_alerts_or_recovery_actions_while_these_checks_fail`, nor a trigger of
  a restart or a recovery command, because the check never fails.

#### Readings over a time

Some readings come from the change of a counter between two runs: the
`cpu` and `io_wait` of `node_resource`, the `cpu` of
`systemd_unit_resource`, every reading of `node_disk_device`, the traffic
of `node_network_interface`, the cache hit ratio of `postgresql`, and the
hits, misses, hit ratio, and evictions of `memcached`. Such a reading
covers the time since the check's run before, not a long-term average.

- The first run measures over 200 ms, so such a check needs a
  `time_limit_per_run` longer than `200ms`.
- The measuring starts again with a 200 ms sample after a counter goes
  back, a restart of the unit, a change in the number of processors, a run
  that could not read the counters, or a gap longer than `2 × check_every + time_limit_per_run`.
- A reload that does not change the check keeps the counters of the run
  before. A changed check, and a restart of MMWatchdog, start again.
- Without `--count-as-scheduled-run`, `mmwatchdog check-now` takes its own
  200 ms sample and leaves the scheduled runs' counters alone.
- The time that a reading covers shows as `Sample window` in
  `mmwatchdog check-now`, and as `sample_window_ns` in JSON.

### restart_systemd_unit

`restart_systemd_unit` restarts one systemd unit when a check of the check
group has been failing for a time.
[Restart a service automatically](../how-to/restart-a-service-automatically.md)
walks through the settings.

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| `systemd_unit` | a `.service` unit name, such as `myapp.service` | Required | The unit that MMWatchdog restarts |
| `triggered_by_checks` | list of check names of this check group | Required, not empty | The checks whose failure calls for the restart |
| `restart_after_failing_for` | duration | Required | How long a check in `triggered_by_checks` must be failing before the restart |
| `max_attempts` | whole number from 1, or `unlimited` | Required | How many restarts MMWatchdog may make before the checks are healthy again |
| `min_time_between_attempts` | duration, above `0s` | Required | The shortest time between two restarts |
| `reset_attempts_after_healthy_for` | duration, above `0s` | Required | How long the checks must stay healthy before the count of attempts starts over |

Check groups that restart the same unit share one count of attempts. So
`max_attempts`, `min_time_between_attempts`, and
`reset_attempts_after_healthy_for` must be the same in each of those check
groups. A difference is a validation error that names both places.

### recovery_command

`recovery_command` runs a command of your own when a check of the check
group has been failing for a time, such as a command that removes old
files when a disk is full.

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| `script` | text | Required | The command, which Bash runs with `pipefail`, as for a `shell` check |
| `triggered_by_checks` | list of check names of this check group | Required, not empty | The checks whose failure calls for the command |
| `run_after_failing_for` | duration | Required | How long a check in `triggered_by_checks` must be failing before the command runs |
| `time_limit_per_attempt` | duration, above `0s` | Required | How long one run of the command may take |
| `max_attempts`, `min_time_between_attempts`, `reset_attempts_after_healthy_for` | as in `restart_systemd_unit` | Required | The count of attempts, as in [restart_systemd_unit](#restart_systemd_unit) |
| `run_as_user_account` | account name | `mmwatchdog` | The account that runs the command. The account must exist on the node |
| `run_from_directory` | path | `/` | The working directory. The directory must exist on the node |
| `env` | mapping of names to text | None | Environment variables of the command |

### Run settings of a shell check and of a module that is not core

A `shell` check, a check with a module that is not core, and a
`recovery_command` take three run settings: `run_as_user_account`
(default `mmwatchdog`), `run_from_directory` (default `/`), and `env`.
For a module that is not core, the three settings stand beside the
module's inputs, below the module's name.

- **No inherited environment.** The command or the module gets no
  environment from the service. MMWatchdog supplies a standard `PATH`, the
  account's home and name, and `LANG=C.UTF-8`, then the variables of `env`.
- **Variable names** start with a letter or an underscore and contain
  letters, digits, and underscores. `env` refuses `BASH_ENV`, `ENV`,
  `MMWATCHDOG_BIN`, and `MMWATCHDOG_MODULE_HELPER`, because these names
  change how the shell starts or are set by MMWatchdog.
- **An account that does not exist** makes the check's result `unknown`,
  with a message such as
  `run_as_user_account "myapp": user: unknown user myapp`.
- The [`shell` module page](modules/shell.md) lists the other inputs of a
  shell check.

### Connection to a mail server: smtp, imap, and pop3

`smtp`, `imap`, and `pop3` connect to a mail server in the same way, with
`address`, `connection_security`, `trusted_certificate_authority_file`,
and `dangerously_skip_server_certificate_check`:

```yaml
      submission_port_answers:
        smtp:
          address: mail.example.com:587
          connection_security: starttls     # starttls, implicit_tls, or unencrypted
          trusted_certificate_authority_file: /etc/mmwatchdog/internal-ca.pem
        alert_severity: warn
```

- With `starttls` and `implicit_tls`, MMWatchdog uses TLS 1.2 or later and
  verifies the server's certificate for the host name in `address`, unless
  the check states `dangerously_skip_server_certificate_check: true`.
- `trusted_certificate_authority_file` holds at most 1 MiB of PEM
  certificates, which MMWatchdog trusts for this check in place of the
  node's own. MMWatchdog reads the file at each run. A file that cannot be
  read makes the run `unknown`, and `sudo mmwatchdog validate` refuses it.

Each error starts as in [Validation errors](#validation-errors), here
with `check_groups.mail.checks.submission_port_answers.smtp.`:

| Validation error | Cause and fix |
| --- | --- |
| `connection_security: required: starttls, implicit_tls, or unencrypted` | The check states no `connection_security`, or another value, such as `tls` |
| `address: missing port in address` | The check states no `address` |
| `address: address mail.example.com: missing port in address` | The address has no port. Write it as `host:port` |
| `address: port must be a number from 1 to 65535` | The port is not a number from 1 to 65535 |
| `trusted_certificate_authority_file: must be absolute` | The path does not start with `/` |
| `trusted_certificate_authority_file: has no use with connection_security: unencrypted, which verifies no certificate` | An unencrypted connection checks no certificate. Remove the file, or encrypt |
| `trusted_certificate_authority_file: open /etc/mmwatchdog/internal-ca.pem: no such file or directory` | The file is not on the node |
| `trusted_certificate_authority_file: read /etc/mmwatchdog/internal-ca.pem: is a directory` | The path names a directory |
| `trusted_certificate_authority_file: /etc/mmwatchdog/internal-ca.pem: contains no PEM certificates` | The file holds no certificate in PEM form |
| `trusted_certificate_authority_file: /etc/mmwatchdog/internal-ca.pem holds more than 1048576 bytes: the file is larger than its limit` | The file holds more than 1 MiB |

A value in place of the inputs, such as `smtp: mail.example.com:587`, gives
`check_groups.mail.checks.submission_port_answers.smtp: expected a mapping,
found the string "mail.example.com:587"`: write `address` and
`connection_security` below `smtp:`.

### Heartbeat check groups

A heartbeat check group fails when a job, such as a nightly backup, has
not reported for a time:

```yaml
# /etc/mmwatchdog/check_groups/nightly_backup.yml
check_groups:
  nightly_backup:
    heartbeat:
      max_time_between_heartbeats: 26h                                # alert when no report came for 26 hours
      expected_bearer_token_file: /etc/mmwatchdog/secrets/nightly-backup-token
    alert_severity: warn
```

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| `heartbeat.max_time_between_heartbeats` | duration, above `0s` | Required | The longest time between two reports of the job |
| `heartbeat.expected_bearer_token_file` | path | Required | The file with the token that the job sends. A [secret file](#secret-files). Each heartbeat check group needs a different token |
| `alert_severity`, `raise_to_sos_after_failing_for`, `send_alerts_only_to_destinations` | as on a check | `alert_severity` is required | As on a check, written on the check group |
| `secret_readable_by_group`, `dangerously_let_every_account_read_secret` | boolean | `false` | Written beside `heartbeat` |

A heartbeat check group holds nothing else: no timings, no `checks`, no
restart, no recovery command, no limits. Reason: MMWatchdog does not run
a heartbeat, so the heartbeat's deadline is the heartbeat's only rule.
`mmwatchdog status` shows the heartbeat as `nightly_backup/heartbeat`.
[Report from a scheduled job](../how-to/report-from-a-scheduled-job.md)
sets up the job's side.

## The alert destinations file

`alert_destinations.yml` holds a `destinations:` mapping. Each destination
has a name that you choose and names exactly one destination module:

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  team_slack_channel:                   # the destination's name
    slack:                              # the destination's module
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos] # this destination takes alerts of every severity
    remind_every: 4h                    # repeat each open problem every 4 hours
```

| Setting | Type | Required or default | What the setting does |
| --- | --- | --- | --- |
| The module | the module's name as the key, its inputs below | Required: exactly one module | Where the alert goes, such as `email`, `webhook`, `slack`, or `mmhotline`. Each [module page](modules/README.md) lists the inputs |
| `alert_severities` | list of alert severities, each once | Required | The severities of the alerts that the destination takes from checks that name no destinations. `[]` takes only the alerts of checks that name this destination |
| `remind_every` | duration above `0s`, or `never` | Required, except on `mmhotline`, which takes none | How often the destination gets a reminder of a problem that is still open |
| `time_limit_per_delivery_attempt` | duration, above `0s` | `5s`, or `30s` for `email` | How long one attempt to deliver an alert may take |
| `send_only_the_alert_title` | boolean | `false` | Sends the alert's title and no body, for a short channel such as SMS. `mmhotline` takes none. See [Alert format](alert-format.md) |
| `secret_readable_by_group`, `dangerously_let_every_account_read_secret` | boolean | `false` | See [Secret files](#secret-files). On `mmhotline`, write the two settings inside the `mmhotline` mapping |

**No destination.** A node can run without sending alerts. MMWatchdog then
runs its checks, restarts units, runs recovery commands, and records every
event, and tells nobody. Write the choice down:

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations: {}
```

- `sudo mmwatchdog validate` and `sudo mmwatchdog status` then print
  `This node sends alerts to nobody.`, and the JSON of `status` has
  `sends_alerts_to_nobody: true`.
- A missing `alert_destinations.yml` on a standalone node or a hub is a
  validation error, and so is a file without a `destinations:` mapping,
  such as an empty file. So a person who forgot the destinations finds
  out, and a person who wants only restarts writes one line.

[Send alerts where your team sees them](../how-to/send-alerts-somewhere.md)
says which alert goes to which destination.

## The set-aside findings file

On a fleet hub, the optional file `/etc/mmwatchdog/set_aside_findings.yml`
sets findings aside after an investigation. The hub sends no alert below
`sos` for a node incident whose check, finding, and
[exact finding](../changing-mmwatchdog/design/checks.md#exact-findings)
match an entry:

```yaml
# /etc/mmwatchdog/set_aside_findings.yml
set_aside_findings:
  - check: installed_packages/known_exploited_flaws
    finding: CVE-2026-123158 openssh
    exact_finding: "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z"
    reason: Needs musl without PAM; every server with this exact finding runs glibc and sshd links PAM. Debian rates it unimportant.
    investigation_note: triage_agent_notes/2026-11-23_openssh_package_warning_on_app001.md
    set_aside_by: triage agent (MMHotline run b82fdd8b)
    set_aside_on: 2026-11-23
```

| Field | Value |
|---|---|
| `check` | The check, as `CHECK_GROUP/CHECK`. |
| `finding` | The finding's `id`, as `mmwatchdog fleet findings` shows it. |
| `exact_finding` | The finding's exact finding, copied whole from `mmwatchdog fleet findings`. |
| `reason` | Why the finding needs no action. The hub's closing alert says `Set aside after investigation: ` and this text. |
| `investigation_note` | Where the investigation is written down, such as a file in the project. |
| `set_aside_by` | Who set it aside: a person, or an agent with its run. |
| `set_aside_on` | The date. |

- **Every field is required and nonempty.** A missing or empty field is a
  validation error. `set_aside_findings: []` is valid and sets nothing
  aside. An entry always names one exact finding: there is no entry for a
  whole check.
- **An entry matches only when all three of `check`, `finding`, and
  `exact_finding` are equal.** A new installed version, release, fix
  state, or advisory record changes the exact finding, so the finding
  alerts again. An entry has no expiry date.
- **`sos` is never set aside.** A finding at `sos` alerts, whatever the
  file says.
- **Never set aside the routine-update checks** (`apt_security_updates`,
  `dnf_security_updates`, `zypper_security_patches`): an entry would quiet
  every security update of the release; install the updates.
- **Only the hub reads the file.** On a joined node or a standalone node
  the file is a validation error. Joined nodes never get a copy, so a node
  that sends without the hub sends every alert.
- **A reload applies a changed file,** and the hub applies it again at
  each start. An incident that an entry newly covers closes with
  `closed_without_recovery`; incidents that an entry no longer covers open
  with one alert for each destination and key
  ([`mmwatchdog reload`](commands.md#mmwatchdog-reload)).
- **Deleting the file in your project must remove it from the hub.** The
  example playbook `examples/deploy/playbooks/hub_and_standalone.yml`
  removes the hub's file when the project has none. If your deploy tool
  refuses a missing source file, keep the file with
  `set_aside_findings: []`.
- `sudo mmwatchdog fleet set-aside-findings` shows the entries that the
  hub uses ([commands](commands.md#mmwatchdog-fleet-set-aside-findings)).

[Set a finding aside](../changing-mmwatchdog/design/fleet.md#set-a-finding-aside)
gives the rules and their reasons.

## Secret files

A setting or input whose name ends in `_file`, such as `bearer_token_file`
or `login_password_file`, names a file that MMWatchdog reads, such as a
token. MMWatchdog refuses a secret file that other accounts can read,
because the file holds a password, a token, a key, or a webhook address.
Only read permission counts: write and execute bits do not expose a file's
contents.

| File mode | What MMWatchdog needs |
| --- | --- |
| `0600` or `0400` | Nothing: only the owner reads the file |
| Readable by the file's group, such as `0640` | `secret_readable_by_group: true` |
| Readable by every account, such as `0644` | `dangerously_let_every_account_read_secret: true` |
| Readable by both, such as `0644` with a group | Both settings |

Write the two settings in the mapping that owns the file:

- for a check or a destination, beside the module's name.
- for an `mmhotline` destination, `heartbeat_receiver`, and
  `join_fleet_hub`, inside that mapping.
- for a heartbeat, beside `heartbeat`, where its `alert_severity` stands.

For example, a heartbeat receiver whose private key has mode `0640`:

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
heartbeat_receiver:
  listen_on_address: 10.0.0.5:8444
  tls_certificate_file: /etc/mmwatchdog/secrets/server.crt
  tls_private_key_file: /etc/mmwatchdog/secrets/server.key
  secret_readable_by_group: true        # the private key's mode is 0640
```

The private key may now be readable by its group. Access by every account
stays refused.

**What counts as a secret file.** The rule covers every `_file` input of
the core modules that holds a credential:

- `login_password_file` of `redis`, `postgresql`, `mysql`, and `email`.
- `bearer_token_file` and `basic_auth_file` of `http`, and
  `bearer_token_file` of `webhook`.
- `mmhotline_signing_secret_file` of `mmhotline`.
- `expected_bearer_token_file` of a heartbeat, `tls_private_key_file` of
  `heartbeat_receiver`, and `join_code_file`.

The rule also covers every `_file` input of a module that is not core, such
as `connection_string_file` of `mongodb`. An input whose name ends in
`certificate_authority_file` names a public list of trusted certificates
and is exempt, in every module.

**When MMWatchdog checks.** `sudo mmwatchdog validate` and a reload refuse
a secret file that is readable by others, with an error that names the
file, the mode, who can read the file, and both fixes:

```text
mmwatchdog: /etc/mmwatchdog/alert_destinations.yml:5:7: destinations.team_webhook.webhook.bearer_token_file: /etc/mmwatchdog/secrets/api-token has permissions mode 0640: the file's group can read the secret; run chmod 600 /etc/mmwatchdog/secrets/api-token, or add secret_readable_by_group: true to the owning check, destination, or setting block to allow that access
```

- `sudo mmwatchdog validate --config-only` opens no secret file, so the
  command can check a configuration on another machine.
- When the service starts, the service logs each such problem once as a
  warning and keeps running.
- A running check, an alert delivery, and a join never stop because of a
  file's mode. A mode can change on a running node, and refusing the file
  then would stop alerts: a duplicate alert is acceptable, a dropped alert
  is not.

MMWatchdog reads every `_file` input of a module that is not core as root,
and gives the module the file's contents.
[Send alerts where your team sees them](../how-to/send-alerts-somewhere.md)
shows how the release's MMX project writes secret files from its vault.

## Measurement history

A **measurement** is a number that a check measures, such as
`memory_used_percent`, and a **reading** is one value of a measurement at
one time. The **measurement history** keeps the readings of every
scheduled run, on the node's own disk, so that you can see what the node
did before and during a problem. Two node settings, both required, bound
the history:

| Setting | What the setting does |
| --- | --- |
| `keep_measurement_history_for` | MMWatchdog removes a reading once the reading is older than this duration |
| `measurement_history_size_limit` | `/var/lib/mmwatchdog/measurement_history.db` never grows beyond this size. When the readings reach the size first, the oldest go first |

- **What is stored:** the readings of each scheduled run that measures,
  also of a failed run, of a check that only records, and of a run during
  maintenance. `mmwatchdog check-now` stores nothing. MMWatchdog never
  replaces readings with averages.
- **When readings go:** MMWatchdog removes old readings once a minute, when
  the service starts, and right after each reload.
- **A reload applies both settings.** A shorter time removes the older
  readings right after the reload. A lower size removes the oldest readings
  in small steps, several times a second, so that no check waits.
  MMWatchdog never refuses a history that is larger than a new limit.
- **The working file** `measurement_history.db-wal` stays at about 1 MiB
  beside the history, and the size limit does not count the working file.

On a hub, `fleet_hub_role` holds two more required settings for the
readings that the joined nodes send: `keep_measurement_history_of_nodes_for`
and `measurement_history_of_nodes_size_limit`, for the file
`/var/lib/mmwatchdog/measurement_history_of_nodes.db`. The two hub
settings work like the node's two settings: a reload applies both, and the
oldest readings of any node go first at the size limit. A shorter time on
the hub applies at the hub's next removal, within a minute.

[Look back at measurements](../how-to/look-back-at-measurements.md) reads
the readings, chooses the settings, and covers the hub's history.

## Storage limits

Every store that grows with time has a time limit, a size limit, or both,
so that MMWatchdog never fills the disk that MMWatchdog watches. No store
has a limit on the number of entries. When a limit is reached, the oldest
entries go first.

| Store | Settings | Default |
| --- | --- | --- |
| [Measurement history](#measurement-history): this node's readings, in `measurement_history.db` | `keep_measurement_history_for`, `measurement_history_size_limit` | None: both are required |
| Event history: what `mmwatchdog events` shows | `keep_event_history_for`, `event_history_content_size_limit` | `24h`, `16MiB` |
| Outbox: the alerts that wait for delivery | `keep_undelivered_alerts_for`, `undelivered_alerts_content_size_limit` | None: both are required, such as `24h` and `8MiB` |
| State database: all of `state.db`, which includes the event history and the outbox | `state_database_size_limit` | `128MiB` |
| On a hub: the events of the nodes, in `fleet.db` | `fleet_hub_role.keep_event_history_of_nodes_for`, `fleet_hub_role.event_history_of_nodes_content_size_limit` | `7d`, `4GiB` |
| On a hub: the nodes' alerts that the hub accepted and has not delivered | `fleet_hub_role.undelivered_alerts_of_nodes_content_size_limit` | None: required, such as `32MiB` |
| On a hub: the [readings of the nodes](#measurement-history), in `measurement_history_of_nodes.db` | `fleet_hub_role.keep_measurement_history_of_nodes_for`, `fleet_hub_role.measurement_history_of_nodes_size_limit` | None: both are required |

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
alert_severity_when_this_node_goes_silent: sos
alert_severity_when_this_node_has_rebooted: warn
keep_measurement_history_for: 7d
measurement_history_size_limit: 256MiB
keep_event_history_for: 7d                # keep events for 7 days (24 hours without this line)
event_history_content_size_limit: 32MiB   # and at most 32 MiB of events (16 MiB without this line)
state_database_size_limit: 128MiB         # the default, written out
```

This node keeps its readings for 7 days within 256 MiB, its events for 7
days within 32 MiB, and undelivered alerts for 24 hours within 8 MiB.

- **`state_database_size_limit` is at least twice the sum** of
  `event_history_content_size_limit` and
  `undelivered_alerts_content_size_limit`, and on a hub also
  `fleet_hub_role.undelivered_alerts_of_nodes_content_size_limit`. Reason:
  `state.db` also holds the state of every check, and SQLite needs free
  space to rewrite that state. A smaller value is a validation error that
  names the smallest valid value, such as `must be at least 48MiB`.
- **On a hub, `fleet.db` may grow to twice
  `event_history_of_nodes_content_size_limit`:** the events take at most
  the setting, and the rest holds each node's latest report.
- **SQLite never shrinks a file on its own.** So a `state.db` or
  `fleet.db` that already holds more than its setting allows is refused,
  by `sudo mmwatchdog validate` and by the service when the service
  starts. The message names the size to state, such as
  `state.db already holds 140 MiB, more than state_database_size_limit allows (128 MiB); state at least 140MiB`.
- **A reload applies the time and size limits,** except
  `state_database_size_limit` and
  `fleet_hub_role.event_history_of_nodes_content_size_limit`, which apply
  when the service starts. Right after a reload, MMWatchdog removes the
  entries older than each store's time limit. Otherwise MMWatchdog looks
  once a minute.
- **A dropped alert is counted.** An alert that a time or size limit
  removes before delivery counts in the `dropped` number of
  `mmwatchdog status`, and a `delivery_gap` event says why.
- **The limits are not a disk quota.** The databases' working files
  (`-wal`) keep up to about 1 MiB after each checkpoint, and a large write
  can need more for a moment.

**When a store cannot be written,** such as on a full disk, monitoring
goes on:

- `sudo mmwatchdog status` names each failing write as a storage problem
  and exits with code 3, or 1 when a check fails. The problem clears when
  the write succeeds again.
- What cannot be saved does not take effect. A restart or a recovery
  command waits until its attempt is saved. A heartbeat gets HTTP `503`
  until its receipt is saved. A maintenance command and
  `mmwatchdog reset-recovery-attempts` fail and leave the state as it was.
- An alert that cannot be saved still goes out: MMWatchdog sends the alert
  from memory, and logs each attempt with
  `state.db cannot be written, so MMWatchdog sends the alert from memory:`.
  When `state.db` can be written again, the same alert, with the same `id`,
  is saved and delivered a second time. A full disk must not silence the
  alert that reports the full disk. A joined node sends such an alert to
  the hub, and to the destinations directly when the hub has not accepted
  the alert within `send_alert_without_hub_after_waiting_for`.
- The alerts and status changes that wait in memory are bounded by
  `undelivered_alerts_content_size_limit`. When older ones must go, a
  `delivery_gap` event says
  `N alerts and status changes were lost because MMWatchdog could not save them to state.db`.
  Entries that wait in memory are lost if MMWatchdog stops before saving
  them.
- The service needs a writable `state.db` to start. On a full or read-only
  filesystem the service exits with the SQLite error, and systemd starts
  the service again until the write succeeds.

[When something fails](../when-something-fails.md) covers a full disk and a
damaged measurement history.

## Built-in limits

These limits are fixed, each for a reason. Every other limit is a setting.

| What | Limit | Reason |
| --- | --- | --- |
| A configuration file | 1 MiB | MMWatchdog reads each file whole. A real configuration is a few kilobytes |
| The title of an alert | 120 characters | A title must fit an SMS and a phone call. [Alert format](alert-format.md) says how a long title is shortened |
| The evidence of an incident | 16 KiB, marked `truncated` when cut | The evidence goes with every alert of the incident, and an alert must stay small enough for every destination |
| The standard output of a `shell` check or a module that is not core | 64 KiB. More output is an error of the run | A status answer and a report to the hub must stay small |
| The standard error of a `shell` check or a module that is not core | 16 KiB kept, with a mark where the text was cut | The same |
| The lines of an observation's details | 12 lines of at most 200 bytes each | The details go into the body of an alert |
| A web page that an `http` check reads for content | 1 MiB | Keeps the memory of one run small, whatever the server sends |
| The result of a module that is not core | 128 measurements, 500 findings | The result goes into status, alerts, and the reports to the hub, which must stay small |
| The request to a destination module | 1 MiB | A fixed size lets every module read the whole request. The largest alert and the limits below always fit |
| The files that a destination's inputs name | 16 KiB for each file, 64 KiB together | Keeps the request within 1 MiB. Validation finds a file that is too large before a deploy |
| The inputs of a destination, as JSON | 256 KiB | The same |
| A module's script, `module.star` | 1 MiB | MMWatchdog reads the whole file into memory |
| A module's description, `module_description.yml` | 256 KiB | MMWatchdog reads the whole file into memory |
| One run of a module's script | 10 million steps | Ends a script that loops without end before the script keeps a processor busy until its time limit |
| What a script keeps between runs | 1 MiB as JSON | MMWatchdog keeps the value in a file and gives the whole value to the next run |
| The node groups of one node | 100 | See [Silence and node groups](#silence-and-node-groups) |

There is no limit on the number of destinations, checks, or check groups,
nor on `max_concurrent_checks`. [Writing a module](writing-modules.md) has
the limits of each function that a module's script calls.

## Validation errors

Each error starts with `mmwatchdog:`, the file, the line, and the column,
as in [Rules for every file](#rules-for-every-file). The table shows the
rest of each error, from real runs of `sudo mmwatchdog validate`, with the
paths of `/etc/mmwatchdog`. `sudo mmwatchdog validate` exits with code 1
for an invalid configuration.

| Validation error | Cause and fix |
| --- | --- |
| `check_groups.myapp.checks.health_page_answers.alert_severty: unknown field "alert_severty": the field name matches neither a setting of a check nor an installed module. ...` | A misspelled setting or module name. The rest of the message lists the module directories and the modules in each |
| `check_groups.myapp.check_every: is required: how often each check of this check group runs, such as 30s` | The check group misses one of the four timings. The error names the missing timing |
| `check_groups.myapp.time_limit_per_run: 45s exceeds check_every 30s` | A run may not take longer than the time between runs |
| `check_groups.myapp.checks.health_page_answers.alert_severity: required: info, warn, or sos` | The check has no `alert_severity` |
| `check_groups.myapp.checks.memory_is_below_90_percent.only_record_readings: is required without alert_severity: choose whether to record or alert; add only_record_readings: true, or add alert_severity: warn and a limit for a module that needs one` | A measuring check states neither choice. See [Checks that measure](#checks-that-measure) |
| `check_groups.myapp.checks.memory_is_below_90_percent.alert_severity: a node_resource check needs a limit to alert; add fail_when_measurement_above or fail_when_measurement_below, or replace alert_severity with only_record_readings: true` | A measuring check alerts without a limit |
| `check_groups.myapp.checks.memory_is_below_90_percent.fail_when_measurement_above.memory_percent: this check has no such measurement; it measures memory_used_percent` | The limit names a measurement that the module does not report |
| `check_groups.myapp.checks.memory_is_below_90_percent.fail_when_measurement_above.memory_used_percent: expected a number from 0 to 100` | A percentage limit outside 0 to 100 |
| `check_groups.myapp.checks.health_page_answers: names the modules http and tcp: a check names exactly one module` | Two modules on one check. Make two checks |
| `check_groups.myapp.checks.no_process_was_killed_for_memory.alert_after_failing_for: an event check opens its incident at the first new event; remove alert_after_failing_for from this check. Set recovered_after_healthy_for to keep the incident open after the last event` | See [Event checks](#event-checks) |
| `check_groups.myapp.checks.health_page_answers.send_alerts_only_to_destinations: alert_destinations.yml has no destination named "team_chat"` | A destination name with a typo, or a destination that is not in `alert_destinations.yml` |
| `check_groups.my.app: name must use letters, digits, hyphens or underscores (1..64 characters)` | The check group's name has a dot. See [Values](#values) |
| `duplicate check group name "myapp" (first defined at /etc/mmwatchdog/check_groups/myapp.yml:2:3)` | Two files define the same check group. The error starts with the second place |
| `check_groups must contain only .yml files` | A file such as `myapp.yaml` or `myapp.yml.bak` in `check_groups/`. Rename the file to `.yml`, or move the file out |
| `check_groups: expected at least one named check group` | `check_groups/` holds no check group |
| `check_groups.myapp.check_every: duplicate key "check_every": it is also written on line 3` | A key written twice in one mapping |
| `check_groups.myapp: expected a value without YAML aliases or anchors, found the YAML anchor &timings` | YAML anchors and aliases are refused. Write each value out |
| `configuration exceeds 1 MiB` | A file larger than 1 MiB |
| `destinations.team_webhook.remind_every: is required: how often to repeat an open incident; add remind_every: 1h, or remind_every: never` | The destination has no `remind_every` |
| `destinations.team_webhook.alert_severities: required: a list of distinct severities from info, warn, sos, such as [info, warn, sos]. Use [] to receive alerts only from checks that name the destination` | The destination has no `alert_severities`, or a list with a severity twice |
| `destinations.team_webhook.send_only_the_alert_title: expected true or false, found the string "yes"` | A boolean written as `yes`, `no`, `on`, or `off` |
| `/etc/mmwatchdog/alert_destinations.yml: alert destinations file is missing. Create the file with a destination, or write "destinations: {}" into the file to run MMWatchdog with no destination. With no destination, MMWatchdog runs checks, restarts units, and runs recovery commands, and sends alerts to nobody` | A standalone node or a hub without `alert_destinations.yml` |
| `/etc/mmwatchdog/alert_destinations.yml: expected a destinations: mapping; write "destinations: {}" to run MMWatchdog with no destination` | An empty `alert_destinations.yml` |
| `join_fleet_hub: remove /etc/mmwatchdog/alert_destinations.yml: a node that joins a hub gets its alert settings from the hub` | A joined node still has `alert_destinations.yml` |
| `destinations.team_webhook.webhook.bearer_token_file: /etc/mmwatchdog/secrets/api-token has permissions mode 0640: the file's group can read the secret; run chmod 600 ..., or add secret_readable_by_group: true ...` | See [Secret files](#secret-files) |
| `keep_undelivered_alerts_for: is required: how long the node keeps alerts awaiting delivery, such as 24h` | One of the six required node settings is missing. The other five read the same way |
| `alert_severity_when_this_node_goes_silent: required: info, warn, or sos` | The setting is missing or has another value |
| `keep_measurement_history_for: is required: how long this node keeps the readings of its measurements, such as 7d` | The node settings do not state the time |
| `measurement_history_size_limit: is required: how much of this node's disk the readings of its measurements may take, such as 256MiB` | The node settings do not state the size |
| `keep_measurement_history_for: must be positive` | The time is `0s` |
| `keep_measurement_history_for: expected a duration such as 30s, 5m, 26h, or 7d, found the string "7 days"` | A duration without its unit right after the number |
| `measurement_history_size_limit: expected a size: a whole number with MiB or GiB, such as 256MiB, found the string "256MB"` | A size with another unit, or not a whole number from 1 |
| `state_database_size_limit: must be at least 48MiB, twice the sum of event_history_content_size_limit and undelivered_alerts_content_size_limit, which state.db holds beside every check's state` | See [Storage limits](#storage-limits). On a hub, the sum also counts `fleet_hub_role.undelivered_alerts_of_nodes_content_size_limit` |
| `max_concurrent_checks: expected a whole number of at least 1` | `max_concurrent_checks: 0` |
| `external_module_dir: expected an absolute path` | A path that does not start with `/` |
| `heartbeat_receiver.listen_on_address: expected the local host and port where MMWatchdog listens; set listen_on_address: '10.0.0.5:8445', or listen_on_address: '0.0.0.0:8445' for every interface` | An address without a host, such as `:8444`. See [Listening addresses](#listening-addresses) |
| `fleet_hub_role.keep_measurement_history_of_nodes_for: is required: how long the hub keeps the readings that the nodes send, such as 7d` | `fleet_hub_role` does not state the time. `measurement_history_of_nodes_size_limit` reads the same way |
| `fleet_hub_role.keep_measurement_history_of_nodes_for: must be positive` | The time is `0s` |
| `fleet_hub_role.alert_after_hub_unreachable_for: must be at least twice report_every, so one late report does not alert` | Raise `alert_after_hub_unreachable_for` or lower `report_every` |
| `fleet_hub_role.raise_to_sos_when_silent_nodes_reach_count: both silence thresholds are set; keep only raise_to_sos_when_silent_nodes_reach_count or raise_to_sos_when_silent_nodes_reach_percent to choose when silence alerts rise to sos` | State one of the two thresholds |
| `raise_to_sos_when_every_node_of_these_node_groups_is_silent: names the node group "databases", which the node is not in: add the node group to this_node_is_in_node_groups, or remove the node group from raise_to_sos_when_every_node_of_these_node_groups_is_silent` | See [Silence and node groups](#silence-and-node-groups) |

The [module pages](modules/README.md) list the errors of each module's
inputs. [Commands](commands.md) describes `mmwatchdog validate`, its
validation warnings, and `--config-only`.

## Related pages

- [Files that an install puts on a node](installed-files.md): the owner,
  mode, and purpose of each file.
- [Alert format](alert-format.md): the title and body of each alert, and
  the JSON that a webhook receives.
- [Commands](commands.md): `mmwatchdog validate`, `reload`, and `config`.
- [Module pages](modules/README.md): the inputs of each module.
- [Add a check](../how-to/add-a-check.md) and
  [Send alerts where your team sees them](../how-to/send-alerts-somewhere.md):
  the jobs that change these files.
