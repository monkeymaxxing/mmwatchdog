# MMWatchdog: fleet

This document owns the optional fleet hub role: joining, node status reports,
the measurement history that the hub keeps, node groups, the fleet view, alert
notifications, silence detection, the hub-outage alerter nodes, the fleet's optional
connection to MMHotline, and moving or shrinking the fleet. `fleet_hub_role:`
enables the fleet hub role inside a node monitoring process, normally on the
operations node `util`. The node monitoring process with the fleet hub role
still monitors the hub's own node. Every other node monitoring process keeps
its own checks, incidents, and recovery actions. Joining changes where the
joined node's alerts are delivered. A node that has joined no hub
and is no hub is a standalone node.

The [product principles](README.md#product-principles) apply, with one rule
for presentation: **Normality compresses. Exceptions expand.**

## Set up a fleet

The user turns on the hub, copies one code, and adds a `join_fleet_hub` block
to every other node. The user never handles certificates, node lists,
per-node credentials, or fleet alert rules.

```yaml
# util, /etc/mmwatchdog/mmwatchdog_node_settings.yml: the node that is the hub
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
fleet_hub_role:
  report_every: 30s
  alert_after_hub_unreachable_for: 3m
  send_alert_without_hub_after_waiting_for: 30s
  undelivered_alerts_of_nodes_content_size_limit: 32MiB
  listen_on_address: 0.0.0.0:8445
  address_for_nodes: util.example.com:8445
  keep_measurement_history_of_nodes_for: 7d         # how long the hub keeps the readings that the nodes send
  measurement_history_of_nodes_size_limit: 20GiB    # and how much of util's disk those readings may take
alert_severity_when_this_node_goes_silent: sos      # the severity of the alert when the hub itself goes silent
alert_severity_when_this_node_has_rebooted: warn   # the severity of the notice after a reboot of this node
keep_measurement_history_for: 7d                    # util's own readings, as on every node
measurement_history_size_limit: 256MiB
```

`fleet_hub_role` turns on the hub and requires `listen_on_address` and
`address_for_nodes`. The required timings are `report_every`,
`alert_after_hub_unreachable_for`, and
`send_alert_without_hub_after_waiting_for`. The hub also requires
`undelivered_alerts_of_nodes_content_size_limit` and the two settings for the
[measurement history on the hub](#measurement-history-on-the-hub).

Every node states the severity of the node's [silence alert](#silent-nodes).
Every node also states the node's own
[measurement history](operation.md#measurement-history) settings:
`keep_measurement_history_for` and `measurement_history_size_limit`.

The hub listens at the required `listen_on_address`, independently of the
required `address_for_nodes` that nodes dial. The
[listening address names its host](../../reference/configuration-files.md#listening-addresses) explicitly.
Separate addresses permit a specific local interface, a proxy, or port
forwarding without changing the join code. The hub
can run inside the network of its nodes or outside it, for example at another
provider, so that the fleet view stays reachable when the nodes' network is
not. Nodes push to the hub, and the hub never connects to a node, so the nodes
need to reach only the port in `address_for_nodes`.
The [hub-outage alerter nodes](#the-hub-outage-alerter-nodes)
watch the hub from among the nodes, and MMHotline, when the fleet uses it,
watches the hub from outside ([MMHotline in a fleet](#mmhotline-in-a-fleet)).

The fleet timings are required, set on the hub alone, and reach every node
with the fleet settings, so a node never disagrees with the hub:

```yaml
fleet_hub_role:
  undelivered_alerts_of_nodes_content_size_limit: 32MiB
  listen_on_address: 0.0.0.0:8445
  address_for_nodes: util.example.com:8445
  keep_measurement_history_of_nodes_for: 7d
  measurement_history_of_nodes_size_limit: 20GiB
  report_every: 30s                                 # three missed reports make a node silent
  alert_after_hub_unreachable_for: 3m               # the hub-outage alerter node alerts when its reports have failed this long
  send_alert_without_hub_after_waiting_for: 30s     # a node delivers an alert itself when the hub has not accepted it for this long
```

`report_every`, `alert_after_hub_unreachable_for`, and
`send_alert_without_hub_after_waiting_for` each take at least 1s, because a
shorter time would only load the hub. The three timings have no upper bound.
`alert_after_hub_unreachable_for` is at least twice `report_every`, so one
late report never alerts.

A reload applies changed fleet timings. Nodes receive the new timings with
the next report. Every fleet check keeps the check's state and uses the new
timing from the check's next receipt. Adding the hub or changing
`listen_on_address` or `address_for_nodes` needs a daemon restart.

On the hub, `sudo mmwatchdog fleet join-code` prints the join code, which
MMX stores as a secret on every other node. The settings file of every other
node has the `join_fleet_hub` block:

```yaml
# every other node, /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
join_fleet_hub:
  join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code
alert_severity_when_this_node_goes_silent: warn     # the severity of the hub's alert when this node stops reporting
alert_severity_when_this_node_has_rebooted: info   # the severity of the notice after a reboot of this node
keep_measurement_history_for: 7d
measurement_history_size_limit: 256MiB
```

Alert destinations live only in the hub's `/etc/mmwatchdog/alert_destinations.yml`
([destinations](alerts.md#destinations)). A node with `join_fleet_hub` refuses to
start or reload, and `mmwatchdog validate` fails, while `alert_destinations.yml` exists in
its configuration directory, so there is never doubt about where alert settings
come from. A hub cannot also join another hub.

A joined node can state three optional settings for the fleet:

```yaml
# db-1, /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
join_fleet_hub:
  join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code
  prefer_this_node_as_hub_outage_alerter: true                       # a long-lived node, preferred as the hub-outage alerter node
alert_severity_when_this_node_goes_silent: warn
alert_severity_when_this_node_has_rebooted: info   # the severity of the notice after a reboot of this node
raise_to_sos_after_this_node_is_silent_for: 10m               # after 10 minutes of silence, the alert becomes sos
this_node_is_in_node_groups: [database_servers, frankfurt]    # the fleet view shows db-1 with these node groups
keep_measurement_history_for: 7d
measurement_history_size_limit: 256MiB
```

- `prefer_this_node_as_hub_outage_alerter` marks a long-lived node, such as the
  database node, as a preferred [hub-outage alerter for the hub](#the-hub-outage-alerter-nodes).
- `raise_to_sos_after_this_node_is_silent_for` raises the silence alert of
  the node that states the setting to `sos` after the duration in
  `raise_to_sos_after_this_node_is_silent_for` ([silent nodes](#silent-nodes)).
- `this_node_is_in_node_groups` names the [node groups](#node-groups) that
  the node is in.

MMX deploys the join code, `join_fleet_hub`, and the hub's `alert_destinations.yml`
([deploy with MMX](operation.md#deploy-with-mmx)). The node settings and alert
destinations live under the configuration directory selected by
`--config-dir` (default `/etc/mmwatchdog`). See
[configuration files](operation.md#configuration-files-and-validation).

## Joining and identity

On first start with `fleet_hub_role:`, the hub generates its TLS key, a self-signed
certificate, and a join secret, and keeps them in `/var/lib/mmwatchdog/fleet.db`. The
join code combines `fleet_hub_role.address_for_nodes`, the certificate's
SHA-256 fingerprint, and the secret. Nodes trust only the certificate whose
SHA-256 fingerprint matches the fingerprint in the join code. Nobody runs a
certificate authority.

During the first reporting attempt, a node sends the node's system hostname
and the join secret to join the hub. The node's alerts carry the system
hostname. A node without a credential for the hub sends the first report at
once, at node start or right after a reload. So a deploy shows each new node
in the fleet view within seconds. The hub compares the secret in constant
time, records the node, and returns a per-node credential.

The node keeps the credential in `/var/lib/mmwatchdog/hub-credential.json`
and uses the credential from then on. The hub stores only the credential's
hash. A join with a wrong secret stores nothing and is only counted.

- **Names.** A name belongs to its node until that node is silent. A join
  request using an existing node's hostname is refused while the existing
  node is `reporting` or `waiting_for_first_report`. The hub shows a name
  conflict, so cloned VMs cannot mix their data. A join request using a
  silent node's hostname replaces the silent node, and the hub records the
  replacement.
- **Joining again.** A node joins again on its own with its join code when
  the hub refuses its credential, when its join code names a hub with another
  certificate, and when MMWatchdog starts under a new node name.
- **Identity from the credential.** The hub resolves every report and alert to
  a node by its credential alone. No node can report or alert as another,
  or change another node's data.

## Reports

A joined node pushes a complete report over HTTPS every `report_every`, 30
seconds in the example. A node that holds a credential starts at a random offset
within the first interval, so that nodes restarted together, as after a power
failure or a deploy of a new MMWatchdog version, spread their reports. A report
carries:

- the daemon-produced status summary that `mmwatchdog status` uses, and every
  check's state and explanation;
- open incidents with their evidence; for an incident of a finding, its
  current exact finding with the exact finding's sequence number, and
  `reminder_eligible`, whether the incident may get a reminder now
  ([one alert per exact finding](#one-alert-per-exact-finding));
- node CPU, memory, and root-disk use;
- maintenance windows, the notification queue, and storage problems;
- the node's `alert_severity_when_this_node_goes_silent`, its
  `raise_to_sos_after_this_node_is_silent_for`, and its node groups;
- the build and a configuration fingerprint;
- the local events the hub has not received yet, up to 500 per report; and
- the readings the hub has not received yet
  ([measurement history on the hub](#measurement-history-on-the-hub)).

The node status report names its configuration fingerprint
`config_fingerprint`, its daemon start `daemon_start_id`, its sequence
`report_sequence_number`, and its send time `sent_at`. Event history uses
`event_history_id` and the cursor `events_after_sequence_number`.
Measurement history uses `measurement_history_id` and
`readings_after_sequence_number` in the same way as event history.
The hub's reply reports progress for events and readings. `events_cursor`
is the sequence number of the last event the hub holds from the node.
`readings_cursor` is the sequence number of the last reading.
A `readings_cursor` of -1 says that the hub cannot keep readings now.
The node sends no readings until a reply says otherwise.

The hub saves a report before acknowledging the report. Within a daemon
start, the hub applies only newer reports by sequence. The hub never applies
reports from the start the hub most recently replaced. Events arrive in
order through one cursor exchange. If local retention removed events before
the hub received the events, the hub records a `history_gap` event in the
sending node's history on the hub.

A node whose `state.db` was restored from an older copy has given out fewer
sequence numbers than the hub holds. The restored node's next events
continue after the hub's number, so the hub accepts the events. After a
restore, only events recorded between node start and the first accepted
report do not reach the hub. Alert delivery does not depend on the missing
events because each alert reaches the hub in a separate request.
The hub's receipt time is authoritative. The fleet view shows a sender's
clock more than five seconds off the hub's.

A failed report is not retried; the next one supersedes it. Reporting never
blocks or changes a node's check groups, alerts, recovery, or status exit code,
except on the [hub-outage alerters](#the-hub-outage-alerter-nodes). `mmwatchdog status` on a joined
node shows the hub, the last accepted report, and the latest error. The
joined node's status JSON uses `joined_fleet_hub` with `hub_address`,
`this_node_name`, `last_accepted_report_at`, `last_error`, `last_error_at`, and
`fleet_settings_version`. The hub's own status JSON uses
`fleet_hub_role_address` for its role's address. The hub lists itself in the fleet as an ordinary entry.

The hub ignores an exact finding with a lower sequence number than the one
that it holds for the node incident, whether the exact finding came in a
report or in an alert, so an old report never brings back an older exact
finding.

If the hub cannot save a report, it answers `503`, but the authenticated
report still counts as contact, so a full disk on the hub never raises false
silence alerts. Such a report changes none of the node's settings that the hub
keeps: they stay those of the node's latest saved report. The hub's fleet
view shows the storage error and exits 3.

Reports and alerts are untrusted input. Requests are limited to 4 MiB and 10
seconds, and a join request, which anyone who reaches the hub's port can
send, to 64 KiB. Decoding is strict, and text output replaces control
characters in anything a node sent, so no node can write terminal escapes to
an operator's screen. The hub never connects to a node and holds no node
credential.

The hub keeps each node's latest report and the events of every node. Two
optional settings in `fleet_hub_role` bound the events:
`keep_event_history_of_nodes_for`, `7d` unless stated, and
`event_history_of_nodes_content_size_limit`, `4GiB` unless stated. An event older
than `keep_event_history_of_nodes_for` is removed. When the size limit is
reached, the hub removes the oldest events of the node with the most saved
events. So one noisy node cannot push out the events of the
remaining nodes. The hub's database file, `fleet.db`, may grow to twice
`event_history_of_nodes_content_size_limit`. Events take
at most `event_history_of_nodes_content_size_limit`, and the remaining space holds
each node's latest report. `mmwatchdog fleet NODE` prints the latest 100
events by default. With `--last DURATION` or
`--since TIME [--until TIME]`, `mmwatchdog fleet NODE` prints every retained
event in the inclusive span, using the event's own time
([fleet view](#fleet-view)). A node's own limits are in
[storage limits](operation.md#retained-state-and-storage-limits). The
readings of the nodes are kept apart from the events, under
`keep_measurement_history_of_nodes_for` and
`measurement_history_of_nodes_size_limit`.

## Measurement history on the hub

A measurement is a number that a check measures, such as
`memory_used_percent`. A reading is one measured value of one measurement at
one time, such as 41 percent at 14:02:30. Every node keeps its own readings on
its own disk ([measurement history](operation.md#measurement-history)). The hub
keeps the readings of every joined node as well, so that the fleet view can
show the history of one node, or of several nodes together, in one place.

```yaml
# util, /etc/mmwatchdog/mmwatchdog_node_settings.yml
fleet_hub_role:
  report_every: 30s
  alert_after_hub_unreachable_for: 3m
  send_alert_without_hub_after_waiting_for: 30s
  undelivered_alerts_of_nodes_content_size_limit: 32MiB
  listen_on_address: 0.0.0.0:8445
  address_for_nodes: util.example.com:8445
  keep_measurement_history_of_nodes_for: 7d         # the hub removes a reading when it is 7 days old
  measurement_history_of_nodes_size_limit: 20GiB    # all nodes' readings together never take more than 20 GiB
```

`keep_measurement_history_of_nodes_for` and
`measurement_history_of_nodes_size_limit` are required in `fleet_hub_role`.
The size limit exists because monitoring must never fill the disk that
MMWatchdog watches.

What happens, in order:

1. A node runs its checks and stores each reading on its own disk.
2. With each report, the node sends the readings that the hub has not
   received yet. The hub saves the report, then the readings, and answers
   the node after saving the report and readings.
3. The hub removes a reading when the reading is older than
   `keep_measurement_history_of_nodes_for`. The hub looks for old readings
   once a minute, at hub start, and right after each reload.

The hub keeps the readings of the nodes in a database file of their own,
`/var/lib/mmwatchdog/measurement_history_of_nodes.db`, which only root can read. Each
reading is stored under the name of its node.

Each reading carries the limits in force at its run beside its value and
unit: `fail_when_measurement_above` and `fail_when_measurement_below`.
The hub stores `fail_when_measurement_above` and
`fail_when_measurement_below` unchanged.
The hub can then mark historical breaches and draw limits on future charts
without knowing the node's current configuration, which may have changed.

`mmwatchdog fleet readings NODE CHECK_GROUP [CHECK]` reads one joined node's retained
readings. `mmwatchdog fleet readings --node-group NODE_GROUP CHECK_GROUP CHECK` reads
one check across every node currently in that node group, using saved
membership. The readings commands for a node or a node group take
`--last DURATION` (default `1h`) or `--since TIME [--until TIME]`, with
RFC 3339 times and the start and end included, and `--json`.
The readings commands for a node or a node group open the hub's files
read-only as root, without a daemon connection, and work with a stopped hub.
A selection that includes the hub reads the hub's entry from the hub's local
`measurement_history.db`, without copying the hub's readings into the
history of nodes.

The node-group view ranks nodes by the number of readings beyond limits,
highest first. Ties are ranked by the greatest numeric distance beyond any
limit, measured in the measurement's recorded unit. Next come nodes without
readings of the check in the span, because a node that stopped reporting
must not hide among healthy nodes. Nodes that
tie are in the order of node names. Each measurement value counts as one
reading.

Healthy nodes share one summary line: every healthy node has at least one
reading and no breach in the requested span. A check configured to record
readings without evaluating failure limits counts as healthy too. A node
with no readings or a missing check stays expanded. The readings in the
views of a node or a node group are historical, independent of the node's
current health or reporting status. JSON includes the full
readings of healthy nodes as well. The page [Look back at measurements](../../how-to/look-back-at-measurements.md#read-the-readings)
defines the output and summaries.

- **The first report asks where the hub is.**
  The first status report sent when a node joins, and after the node's
  MMWatchdog restarts, carries no readings. The hub's reply gives the last
  saved reading sequence number from the node (`readings_cursor`). The
  following reports carry readings with sequence numbers greater than the
  returned `readings_cursor`.
- **A reading arrives once.** The hub saves the last received reading's
  sequence number together with the readings. Readings count only when the
  readings continue from the saved sequence number. So a report sent again
  after a failure adds no reading twice.
- **After a hub outage, the node catches up.** While the hub is unreachable,
  the node keeps storing its own readings. When reports are accepted again,
  the node sends the readings that the hub missed, oldest first and in order,
  through a cursor exchange like the one for events. One report carries at
  most 256 KiB of readings when nodes report every 30 seconds, and the
  following reports carry the rest. A node with ten checks that measure
  every 30 seconds collects about 7 MiB of readings in a day without the
  hub, and sends them in about 28 reports. With another `report_every`, the
  maximum reading content per report changes in proportion, such as 2.5 MiB
  when nodes report every five minutes. The allowance has a minimum of
  128 KiB so the largest run's readings, with their limits, fit in one
  report: measured at about 80 KiB. A report with less room carries fewer
  readings. Above the 128 KiB minimum, a node sends the same amount in the
  same time. A node that
  stores more than about 8 KiB of readings a second, which is 700 MiB a
  day, would never catch up.
- **Readings never hold up a report.**
  The hub first saves a report and counts the sending node as reporting.
  After saving the report, the hub saves the report's readings. The hub accepts reports from other nodes while
  saving the readings. A report can never be too large for the hub because
  of its readings: a report that has little
  room left carries fewer readings, or none.
- **A reading the node no longer has cannot arrive.** If the node's own
  history removed readings before the hub received them, the hub's history of
  that node has no readings at the times of the removed readings.
- **A node with a new measurement history starts over.** A node whose
  measurement history was lost, or a new node that replaces a silent node of
  the same name, has a measurement history with another ID. The hub takes
  the new measurement history's readings from the first reading on. The hub
  keeps readings already stored under the node name until the stored
  readings are old enough to be removed.
- **Readings stay as measured.** The hub keeps every reading that it
  receives, with the time that the node measured it at, and does not replace
  old readings with averages.
- **A clock that runs ahead keeps nothing longer.** The age of a reading
  counts from the time of its run, or from the moment the hub received it
  when the node's clock runs ahead of the hub's. So a sender's clock cannot
  keep a reading longer than the measurement history settings on the hub say.
  The reading still shows the time that its node measured.
- **A restored node keeps sending.** A node whose measurement history was
  restored from an older copy has given out fewer sequence numbers than the
  hub holds. The restored node's next readings continue after the hub's
  number, so the hub accepts the restored node's readings. After a restore,
  only the readings stored between node start and the first accepted report
  do not reach the hub.
- **Readings that the hub cannot save do not refuse the report.**
  The hub saves and answers the report as usual. The fleet view shows a
  storage problem named `measurement history of nodes write` and exits 3
  until readings are saved again. The node keeps the unsaved readings and
  sends the unsaved readings with the node's next report.
- **The hub runs without a file that it cannot use.** When the hub cannot
  open `measurement_history_of_nodes.db`, for example because the file is
  damaged, it starts and takes joins, reports, and alerts as always. The
  fleet view shows a storage problem that names the file and says to move
  `measurement_history_of_nodes.db` aside and restart MMWatchdog.
  Until MMWatchdog restarts with a usable measurement history file, the
  hub's replies tell the nodes to send no readings. So no node sends
  megabytes that the hub would throw away. Each node keeps readings in the
  node's own history and sends the readings once the hub can keep readings
  again.
- **The size limit wins over the time.**
  The size limit is the size of the database file. When all readings reach
  `measurement_history_of_nodes_size_limit`, the oldest readings go first,
  whichever node they came from. The fleet view then says how far back the
  history reaches: `measurement history of nodes reaches back 5 days 3 hours;
  the size limit of 20 GiB prevented keeping the requested 7 days`.
  The information line about the measurement history size limit is not a
  storage problem and does not change the exit code of `mmwatchdog fleet`.
  Raising `measurement_history_of_nodes_size_limit` makes
  `mmwatchdog fleet` stop printing the information line about the measurement
  history size limit.
- **A reload applies both settings.**
  The settings are `keep_measurement_history_of_nodes_for` and
  `measurement_history_of_nodes_size_limit`. A lower size limit applies
  from the reload on. The hub removes the oldest readings and shrinks the
  file in small steps, several times a second, and takes reports between
  removal steps. A shorter `keep_measurement_history_of_nodes_for` applies
  the next time the hub looks for old readings.
  Changing `keep_measurement_history_of_nodes_for` or
  `measurement_history_of_nodes_size_limit` needs no daemon restart.
- **The hub's own readings.** The hub is also a node. The hub's own readings
  stay in the hub's local measurement history, under
  `keep_measurement_history_for` and `measurement_history_size_limit`.
  The fleet view reads the hub's readings from the hub's local measurement
  history. The hub copies none of the hub's readings into the history of
  the nodes.
- **A silent node keeps its history on the hub.** The readings that a node
  sent before it went silent stay until they are old enough to be removed, or
  until the node is [forgotten](#forget-a-node-rotate-the-join-code-move-the-hub).

`mmwatchdog fleet --json` carries `measurement_history_of_nodes` for all
joined nodes together. The object contains `oldest_reading_at` and
`newest_reading_at`, absent when the hub holds no readings from any joined
node. The object also contains the hub's
`keep_measurement_history_of_nodes_for` and
`measurement_history_of_nodes_size_limit` settings. The flag
`cut_short_by_measurement_history_of_nodes_size_limit` is true while
`mmwatchdog fleet` prints the information line about the measurement history
size limit.

## Node groups

A node group is a named selection of nodes, such as all application servers,
or all nodes in one data center. A node says which node groups it is in:

```yaml
# app-2, /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
join_fleet_hub:
  join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code
alert_severity_when_this_node_goes_silent: warn
alert_severity_when_this_node_has_rebooted: info   # the severity of the notice after a reboot of this node
this_node_is_in_node_groups: [application_servers, frankfurt]   # app-2 is in two node groups
keep_measurement_history_for: 7d
measurement_history_size_limit: 256MiB
```

- **The setting is always a list.** A node in one node
  group writes a list with one name, such as `[application_servers]`.
  Omitting `this_node_is_in_node_groups` or writing the empty list `[]` puts
  the node in no node group. Node group names follow the check-name rules.
  A name that the list holds twice is a validation error.
  A node can name at most 100 node groups: its join request
  has a fixed largest size, and a node whose configuration is valid must
  always be able to join.
- **A node group is a selection, not a division of the fleet.** A node can be
  in several node groups, and node groups can overlap: `app-2` above is one of
  the application servers and one of the nodes in Frankfurt.
- **A node group exists because nodes name it.** The hub has no list of node
  groups. A node sends its node groups when it joins and in every report, and
  the hub keeps them from the node's latest saved report, or from its join
  before the first report, also while the node is silent. A node group that
  no node names any more is gone.
- **The hub can be in node groups too.** The hub states
  `this_node_is_in_node_groups` in the hub's `mmwatchdog_node_settings.yml`.
- **A node group can raise silence alerts to `sos`.** A node can name some of its node
  groups in `raise_to_sos_when_every_node_of_these_node_groups_is_silent`,
  and the hub raises the node's silence alert to `sos` while every node of
  such a node group is silent ([silent nodes](#silent-nodes)).

The [fleet view](#fleet-view) shows each node's node groups and can list one
node group alone. `mmwatchdog fleet readings --node-group NODE_GROUP
CHECK_GROUP CHECK --chart` draws one measurement for all nodes of a node
group in one chart
([charts of the readings](operation.md#charts-of-the-readings)), from the
[measurement history on the hub](#measurement-history-on-the-hub).

## Fleet view

Fleet inspection runs on the hub. From elsewhere, the operator or the
operator's agent uses SSH to the hub and runs the `mmwatchdog fleet`
commands shown below. On a node without the fleet hub role,
the error says `fleet inspection runs on the hub; this node has no fleet_hub_role setting`.

```sh
sudo mmwatchdog fleet                                    # every node, problems expanded
sudo mmwatchdog fleet --node-group application_servers   # only the nodes of one node group
sudo mmwatchdog fleet app-2                              # one node in detail
sudo mmwatchdog fleet app-2 --json                       # the same facts for agents
sudo mmwatchdog fleet findings installed_packages/known_exploited_flaws   # every node with each finding of one check
sudo mmwatchdog fleet set-aside-findings                 # the findings set aside, and the nodes that each covers
```

`mmwatchdog fleet` counts the healthy nodes and lists the others with what needs
attention, their node groups, their node resource use, and last contact. Every
listed node appears once, whatever number of node groups it is in. Nodes in
maintenance, refused joins, and fleet storage problems follow, and the view
names the hub-outage alerter nodes and says how far back the
[measurement history on the hub](#measurement-history-on-the-hub) reaches when
its size limit cut it short.

`mmwatchdog fleet --node-group NAME` restricts the `mmwatchdog fleet` view to
the nodes of the named [node group](#node-groups). The first line of
`mmwatchdog fleet --node-group NAME` names the node group and counts the
node group's healthy nodes: `Node group application_servers of the fleet at
util.example.com:8445: 12 nodes, 11 healthy`. The node group's unhealthy
nodes follow, with the selected nodes' maintenance windows and any fleet
storage problem. The node-group view leaves out lines about the whole
fleet: the hub-outage alerter node, the refused joins, and how far back the
measurement history on the hub reaches.

- The exit code of `mmwatchdog fleet --node-group NAME` is the worst exit
  code among nodes in the selected node group. A fleet storage problem
  makes the command exit 3 when no node in the selected node group has failed.
- A name that no node states is an error that lists the node groups of the
  fleet: `no node is in a node group named "berlin"; the node groups of this
  fleet are application_servers, database_servers, frankfurt`.
- In the fleet status view, `--node-group` stands alone: together with a node
  name or a subcommand such as `join-code` it is an error. The separate
  `mmwatchdog fleet readings --node-group` form compares historical readings.

`mmwatchdog fleet NODE` shows the selected node's checks, incident evidence,
maintenance, node groups, and build. The command shows the latest 100 events
by default, or every event in the requested time span. The command
also shows the node's join time and address, any name conflict or
replacement, and whether the node watches the hub or prefers to.

`--json` includes the node's checks, incident evidence,
maintenance, node groups, build, events, join details, conflicts,
replacements, and hub-outage alerter status. JSON lists a node's node groups as
`this_node_is_in_node_groups`. The JSON view of a node group names the
selected node group as `node_group`. The fleet's address is
`hub_address`; each node's joining address is `joined_from_address`. A name
conflict uses `claimed_from_address` inside `conflict`, and refused joins use
`last_refused_from_address` inside `refused_joins`.

**Another program on the hub reads the fleet view.** A program that runs on
the hub's machine, such as MMHotline's dashboard on `util01`, reads the
fleet's health from the output of `/usr/local/bin/mmwatchdog fleet --json`,
run as root. MMHotline reads the fleet JSON through MMHotline's own systemd
socket unit. Only MMHotline's account can connect to the socket. For each
connection, systemd runs exactly `/usr/local/bin/mmwatchdog fleet --json`
as root under the unit's restrictions: a read-only file system, no network,
and no capabilities. Systemd sends the command's output back.

MMHotline's account gets the fleet view and nothing else. The unit names
which command runs, so MMHotline's account cannot choose a command through
the socket. The command reaches the node
monitoring process through `/run/mmwatchdog/mmwatchdog.sock`, which belongs
to root with mode `0600`, so a root process without capabilities connects
as the socket's owner. The JSON names, for each node, its `state`, its `exit_code`, its node
groups, when its last report arrived, and in `report.summary.attention` each
check that needs attention, with its state, since when, and its message. A
healthy check is not listed, so the answer stays small for a large fleet.
The node state, exit code, node groups, last-report time, and
`report.summary.attention` check details follow the
[compatibility rules](README.md#compatibility-between-releases), like every
other result field.

| State | Meaning | `mmwatchdog fleet` exit |
| --- | --- | --- |
| `reporting` | A report arrived within the last three intervals (90 seconds with `report_every: 30s`) | The node's own 0, 1, or 3 |
| `waiting_for_first_report` | The hub has received no report since the node joined or the hub started. Fewer than three intervals have passed since the join or start. | 0 |
| `silent` | No report within three intervals. The silent node's last known state and last report's age are shown as unknown health. | 1 |

A node is healthy when it is reporting with exit 0 and no failing
destination.
For a failing destination, the fleet view prints a separate entry for the
node, including the destination's name and latest error.
The affected node's exit code in the fleet view is 3 unless a failed check
makes the node's exit code 1.

A clock more than five seconds off, or a name conflict in the last hour,
also makes the fleet view print a separate entry for the affected node.

The exit code of `mmwatchdog fleet` is the worst node's, failure before
unknown, or 3 for a fleet storage problem when no node has failed. A
finding below `sos` that is [set aside](#set-a-finding-aside) counts for
neither: it shares one compressed line, such as
`Set aside: 1 finding on 5000 nodes`.

The hub sends only the settings that the whole fleet must share. The fleet
settings contain alert destinations, fleet timings, and the hub's own
silence severity. Nodes fetch the fleet settings from the hub, so every node uses
the hub's shared settings. A change on the hub reaches every node without
a deploy.

Everything about one node lives on that node, and MMX deploys the node's
checks, recovery, maintenance windows, node groups, and modules. The hub
sends no code and no commands.

On the hub, `mmwatchdog fleet readings` with `--chart` draws the measurement
history of one node, or of all nodes of a node group in one chart, with the
limit of the check drawn in
([charts of the readings](operation.md#charts-of-the-readings)).

## Alerts in a fleet

A standalone node delivers its alerts to the destinations in its own
`alert_destinations.yml`. In a fleet, detection, incidents, and recovery stay on each
node; only delivery changes ([who sends an alert](alerts.md#who-sends-an-alert)).

- **Through the hub first.** A node sends each alert to the hub as soon as it
  is queued, in order for each check, not on the report cycle. The hub
  accepts an alert only after saving it under the node and its event ID; a
  repeated ID is acknowledged without a second entry.
- **Without the hub after `send_alert_without_hub_after_waiting_for`.** The
  waiting period starts when the alert happens. After an attempt to reach
  the hub fails, the node checks how long each unaccepted alert has waited.
  Once `send_alert_without_hub_after_waiting_for` has passed since the alert
  happened, the node sends the alert directly to each destination.
  The wait is 30 seconds by default. A report that reaches the hub ends
  what the failed attempts left: the next alert gets its own attempt at
  once, and the hub route's wait and failed attempts in a row start over.
  An attempt that failed before the node joined, or while the hub was
  down, says nothing about the hub once it accepts the node's reports.

  While an earlier alert for a check remains queued at the destinations,
  later alerts for the check join the destination queues. While the hub is
  down, alerts therefore arrive after the
  `send_alert_without_hub_after_waiting_for` delay and are not grouped. An
  alert the hub saved but could not acknowledge can arrive twice with the same
  event ID.
- **Settings follow the hub.** Every report reply carries
  the version of the fleet settings. The fleet settings contain alert
  destinations with credential file contents, destinations whose service
  keeps incidents, and fleet timings. The fleet settings also contain the
  hub's silence severity and the hub's raise to `sos`.

  When the version changes, the node fetches the fleet settings and saves
  the settings in `/var/lib/mmwatchdog/fleet-settings-from-the-hub.json`,
  readable only by root. A node that has not fetched the fleet settings yet
  can deliver only through the hub. On a node without fetched fleet
  settings, `mmwatchdog status` says direct delivery is unavailable.
  The hub reads its credential files, such as a webhook's token file and an
  email destination's password file, when it starts and reloads, so reload
  the hub after rotating a token or a password.
- **A node that delivers without the hub connects from its own place in the
  network.** The joined node's monitoring process sends the webhook request
  or sends email through the mail server of an
  [email destination](../../reference/modules/email.md). The receiver must
  therefore be reachable from every node. A mail server written as
  `localhost:25` is the one on the node that sends: the hub's own for an
  alert that goes through the hub, and each joined node's own for an alert
  that the node delivers without the hub. A joined node without a mail server
  of its own fails that delivery and retries it like any failed delivery.
- **A destination module's settings follow the hub, and MMX installs its
  code.** A destination can deliver through an
  official extra module or a module of the project's own
  ([destination modules](alerts.md#destination-modules)). The fleet settings
  carry such a destination like every other: the module's name, its inputs,
  and the content of its credential files
  (`file_contents_by_destination` in the node's `fleet-settings-from-the-hub.json`),
  which the hub reads when it starts and reloads. For direct delivery, the
  node runs the module named by the fetched destination settings, with the
  fetched inputs and credential file contents. MMX deploys modules, and the
  hub sends no code and no commands.
  Every deploy installs the official extra modules and the
  project's own modules on every node, together with MMWatchdog.
  On a node without the configured destination's module, delivery to the
  configured destination fails and is retried like any failed delivery.
  `mmwatchdog status` on the node missing the destination's module names the
  missing module: `Destination team_chat: 1 alert is
  not delivered. MMWatchdog tries again at 14:05 UTC. The latest attempt failed: the
  module slack is not installed on this node: ...`.
- **A check may name hub destinations.** A check on a joined node can state
  `send_alerts_only_to_destinations` with names from the hub's
  `alert_destinations.yml`
  ([which alert goes to whom](alerts.md#which-alert-goes-to-whom)). The alert
  that the node sends carries the check's `send_alerts_only_to_destinations`
  names. The hub delivers the alert to exactly the destinations named by the
  check. When delivering without the hub, the node uses the check's named
  hub destinations. A joined node cannot check destination names during
  configuration validation, because `alert_destinations.yml` is on the hub.

  The node monitoring process checks the check's destination names against
  the fleet settings once the node has fetched the fleet settings: an alert
  of a check with a nonempty destination list goes by severity only when
  none of the names exist on the hub. Recognized names still receive alerts
  when the list also contains unknown names. On the node, `mmwatchdog status`
  shows the wrong names. No alert is lost.
- **Every kind of alert.** Check alerts, [reminders](alerts.md#reminders),
  `rebooted` alerts, maintenance alerts, and
  [notices](alerts.md#send-a-notice) from nodes take the route described above.
  The hub's own alerts use fleet alert grouping alongside alerts received
  from joined nodes
  ([alert events](alerts.md#alert-events)).

The hub holds the alerts that it has accepted and not delivered yet, up to
the required `fleet_hub_role` setting
`undelivered_alerts_of_nodes_content_size_limit`, and
remembers delivered alert IDs for an hour. An alert beyond the limit, or one
the hub cannot save, is refused, so the node delivers it without the hub.

### Fleet alert grouping

No single event produces one message per node. The hub groups alerts in
one of two ways:

- **By exact finding**, for an alert about a [finding](checks.md#findings)
  whose module supplies an [exact finding](checks.md#exact-findings). One
  exact finding gets one alert across the fleet, however long the nodes
  take to report it ([one alert per exact finding](#one-alert-per-exact-finding)).
- **Within one minute**, for every other alert: a check without findings,
  a finding without an exact finding (also every finding from a node older
  than 0.2.0), a `cannot check` incident, a silence alert, a maintenance
  alert, and a reboot. The rest of this section describes grouping within
  one minute.

Grouping within one minute:

- For each combination of check group, check, change, and
  [alert severity](alerts.md#alert-severity), the first alert goes out at
  once, with the original event. Matching alerts in the following minute
  become one message when the minute ends. The grouped message has a new
  event ID, no single `node`, and the
  [`fleet_incident_key`](#one-incident-per-check-across-the-fleet) of the
  grouped alerts.

  The grouped message's [title](alerts.md#title-and-body) counts the nodes,
  `myapp/http failed on 7
  nodes`. The grouped message's body names up to 20
  nodes and counts the rest: `app-2, app-3,
  …, and 2 more`.

  Alerts with different severities never share a message. Recovery alerts
  also stay separate when the incidents' lists of previously sent
  severities differ. Each grouped
  message therefore carries one `alert_severity` and goes to the
  destinations that accept the message.
- Alerts of checks that name different destinations with
  `send_alerts_only_to_destinations` are never one message either, so a
  grouped message goes to one set of destinations.
- An alert waits while an earlier alert for the same node and check is
  waiting, so neither passes the other.
- The alerts of an
  [`every_systemd_unit`](../../reference/modules/every_systemd_unit.md) check group by
  unit as well: `backup.service` failing on seven nodes is one message,
  `failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service
  failed on 7 nodes`, and another unit that fails in the same minute has its
  own.
- The hub's silence alerts group by change and severity alone, since
  each node is a separate check. The silence alert's title reads
  `3 nodes stopped reporting`, and the body names the nodes,
  `app-2, app-3, app-4`.
- Maintenance alerts group by reason and user
  ([maintenance and deploys](#maintenance-and-deploys-in-a-fleet)).
- A [notice](alerts.md#send-a-notice) is never grouped: it is the message of
  one person or one program, so each notice goes out at once and alone.
- A check alert delivered more than two minutes after the alert happened
  ends the alert body with the present. For example, after a hub outage:
  `(alert delivered late; the event happened at 14:02 UTC. Now app-2 shows myapp/http as
  ok)`.

### One alert per exact finding

A check that runs every 6 hours runs on each node's own schedule, so one
security flaw reaches the hub from different nodes over hours. Grouping
within one minute would send one alert, and start one investigation, for
nearly every node. An exact finding names the problem precisely, so the
hub can tell that a later node has a problem that the first alert already
announced.

Only a module-supplied exact finding joins a later node to an earlier
alert. One key can fail for different reasons: an HTTPS check fails with a
certificate error on `app-1` and with HTTP 503 on `app-2`. Only the module
knows the reason, so every alert without an exact finding keeps grouping
within one minute.

**Per destination and key.** The hub keeps a set of open node incidents
for each destination and each [key](#one-incident-per-check-across-the-fleet)
with an exact finding. This includes every destination, also chat and
email. An incident is in a destination's set only when its alerts go to
that destination, by `send_alerts_only_to_destinations`,
`alert_severities`, and the recipients recorded on the incident. Each
destination's messages are built from its own set only.

For each destination and key, the hub records what it has announced: the
**announced severity** and the **announced exact findings**. The hub
writes both only when it queues an alert that carries them, in the same
transaction as the outbox entry. A node status report never writes them.
They go back to "not announced" when the key closes, when an alert that
carried them leaves the outbox undelivered (by retention, a permanent
refusal, or a replaced destination), and, for the severity, when
`resolved-sos` goes out: the announced severity then becomes the highest
severity still open.

For one destination and one key:

1. **The first alert goes out at once,** as the first node's own alert.
2. **Covered growth waits for the shared tick.** A later node, or a node
   whose exact finding changed, is covered when its severity is not above
   the announced severity and its exact finding is among the announced
   ones. A covered node is not sent on its own, from the first second.
   - The node joins the set, marked `joined` or `changed`. A node whose
     incident closes is marked `recovered`.
   - On the **shared tick**, the hub sends one `finding_nodes_changed`
     update for each key with marked nodes. The tick falls on the
     multiples of `fleet_hub_role.send_newly_affected_nodes_every`
     (default `6h`) since 1970-01-01 00:00 UTC, such as 00:00, 06:00,
     12:00, and 18:00 UTC. Every key's updates go out together, so
     MMHotline's triage agent can cover them in one run.
   - The update lists the nodes that joined, changed, or are ok again.
   - A node's opening that reaches the hub after the node's own raise,
     which the set holds already, is older news: the node keeps its state,
     and no update names it as joined.
   - **An update's severity** is the highest severity among the nodes that
     it adds or changes. A `warn` node that joins an `sos` key goes in a
     `warn` update, and never wakes the person who handles the SOS. An
     update with only recovered nodes is `info`.
   - **An update that adds `sos` nodes does not wait for the tick.** It
     goes out at the end of each minute in which `sos` nodes joined, as
     `finding_nodes_changed`, and joins the open MMHotline incident.
3. **Something new goes out at once,** once for each destination and key:
   a severity above the announced severity, or an exact finding that was
   not announced. A new exact finding comes from a new node or from a
   node's `exact_finding_changed`, and the alert names the exact finding.
   Later nodes with the same exact finding are covered growth. So 5,000
   nodes that move from version A to version B give one message for B at
   once, and then updates on the shared tick. A raise on a key that is
   already `sos` is growth.
4. **A new exact finding keeps the key's severity.** It goes out at the
   severity that is routed to the destination: `info` stays `info`, and
   `warn` stays `warn`.
5. **`resolved-sos` goes out at once** when no node left in the
   destination's set is `sos`. A later `sos` sends a new `sos` alert and
   opens a new MMHotline incident. For example, `app-1` recovers, `app-2`
   stays `warn`, and `app-3` reaches `sos`: the alert about `app-3` wakes
   someone.
6. **Closing.** When the destination's set for the key empties, the hub
   queues the marked nodes' update and then the closing alert, in one
   transaction. The closing alert claims recovery only for the nodes that
   it covered: `CVE-2026-123158 openssh is ok again on the 3 nodes that
   this alert covered`. Other nodes may still fail with an exact finding
   that is [set aside](#set-a-finding-aside), or have alerts that go to
   another destination. The closing alert says "ok on every node" only
   when no node of the fleet has an open incident of the key. A key that
   opens again starts with nothing announced. An incident that a report
   opens again goes through the same rules.
7. **Reminders, one schedule per destination and key.** The hub sends a
   reminder of the key by the destination's `remind_every`, while at least
   one node incident in the set is eligible: not in maintenance or quiet
   time, and still routed to the destination at its current severity.
   Each node incident in a node status report carries this eligibility as
   `reminder_eligible`. A node of 0.2.0 makes no reminders of such a key
   while its latest report reached the hub, and makes its own only while
   it delivers without the hub. The hub drops a node's reminder only when
   its own reminder covers the node's incident: the set of the destination
   that the reminder names holds the incident open, grouped by exact
   finding, and the hub keeps a reminder schedule of the key for that
   destination. Any other reminder goes on, such as one of an incident
   without an exact finding, from a node older than 0.2.0 or from a module
   that could not name the cause, routed to a destination where only other
   nodes' incidents of the key have an exact finding.

The hub writes `at` of an update as the time when the update is sent, so
the note about late delivery never applies to an update.

**One message has a size limit.** An update names up to 20 nodes for each
exact finding in `nodes_by_exact_finding`, with a count of each, and lists
the exact findings that were not announced before first. The whole
encoded message stays within 12 KiB, inside MMHotline's limit of 16 KiB.
What does not fit is counted, together with the command that lists every
node: `mmwatchdog fleet findings CHECK_GROUP/CHECK --finding ID`.

**Scale.** The hub keeps one row for each open node incident and a small
membership row for each destination, so each arriving alert or report is
constant work. One exact finding on 5,000 nodes gives at most one first
alert, one update for each shared tick (plus one each minute while `sos`
nodes join), one message for each new exact finding, and one closing alert
for each destination. `joined_node_limit` keeps its default of 1000, and
[Set up a fleet](../../how-to/set-up-a-fleet.md#if-something-is-different)
says how to raise it and `state_database_size_limit` for thousands of
nodes.

**The reason for the 6-hour tick.** Growth below `sos` asks a person to
act within days. MMHotline's triage agent must look at the new nodes, but
need not look within the hour. With one tick for every key, MMHotline gets
at most four growth batches a day, which its agent covers in about four
runs, well inside its default limit of 50 runs a day. An hourly tick could
take 24 runs a day while a flaw spreads. A team that wants a faster look
states `send_newly_affected_nodes_every: 1h`; the minimum is `1m`. A new
exact finding, a higher severity, and every `sos` node go out at once
anyway. Grouping by exact finding has no switch to turn it off.

## Silent nodes

Each joined node is a check of the built-in `node_reports` check group on the hub,
such as `node_reports/app-2`, which behaves like a [heartbeat](checks.md#heartbeats)
created by joining. Before its first deadline the check is
`waiting_for_first_report`. Each report is a receipt. The node's check
deadline is three report intervals after the last receipt, or after the
join or the hub's start. A missed deadline opens a failure incident,
`app-2 has sent no report to the hub
since 14:02 UTC`, and the node's next current report resolves it with `app-2 is
reporting again`.

An old report never counts as current.

- **Each node states its silence severity.** The alert takes the node's own
  `alert_severity_when_this_node_goes_silent`, required in every node's
  `mmwatchdog_node_settings.yml`, with no default by role: typically `warn` on an app
  server, because one of a thousand app servers dying should not wake anyone,
  and `sos` for a database node that matters. The node sends the silence
  severity when joining and in every report, so the hub knows the severity
  before the first report. The hub keeps the silence severity from the
  node's latest saved report after the node goes quiet and across hub
  restarts. On the hub, the setting is the severity of
  the [hub-outage alerter nodes'](#the-hub-outage-alerter-nodes) alert.
- **Each node can name where its silence alerts go.** A node can state
  `send_silence_alerts_of_this_node_only_to_destinations` beside its
  silence severity: a list of the hub's destinations. Every alert about
  this node's silence goes to the listed destinations and to no other,
  whatever their `alert_severities` say: the alert that the node stopped
  reporting, its raise, and every reminder. Silence alert delivery follows
  the rules of a check's
  [`send_alerts_only_to_destinations`](alerts.md#which-alert-goes-to-whom).
  Recoveries follow the incident's recorded recipients. An empty list
  sends nothing. A nonempty list with no name known to the hub sends the
  alert by severity. If the list includes recognized names, silence alerts
  go only to the recognized destinations. On the hub,
  `mmwatchdog status node_reports` shows an unknown name on the node's check, `node_reports/NODE`, as
  `alert_destinations_the_hub_does_not_have`. Without
  `send_silence_alerts_of_this_node_only_to_destinations`, silence alerts go
  by severity. The node sends the setting when
  it joins and in every report, as it sends its silence severity. On the
  hub, the setting names where the [hub-outage alerter nodes](#the-hub-outage-alerter-nodes)
  send their alert about the hub, and `mmwatchdog validate` on the hub
  refuses a name that `alert_destinations.yml` does not have.
- **A long silence raises to `sos`.** A node can state
  `raise_to_sos_after_this_node_is_silent_for` beside its silence severity.
  The time counts from when the hub received the last report, including
  the missed report intervals before the silence incident opens. Once the
  silence incident opens and `raise_to_sos_after_this_node_is_silent_for`
  has elapsed, the hub raises the node's silence alert to `sos`. The
  `failure_alert_severity_raised` alert gives the reason, `app-2 has
  sent no report to the hub for 10m, so this alert is now SOS
  (raise_to_sos_after_this_node_is_silent_for: 10m)`. The raised alert
  stays `sos` until the node reports again.

  The node sends the setting when joining and in every report, as the node
  sends the silence severity. A node without the setting is never raised
  by time. On the hub, the setting raises the
  [hub-outage alerter nodes'](#the-hub-outage-alerter-nodes) alert in the same way.

  ```yaml
  # app-2, /etc/mmwatchdog/mmwatchdog_node_settings.yml
  join_fleet_hub:
    join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code
  alert_severity_when_this_node_goes_silent: warn         # the first alert is a warning
  alert_severity_when_this_node_has_rebooted: info   # the severity of the notice after a reboot of this node
  raise_to_sos_after_this_node_is_silent_for: 10m         # after 10 minutes of silence, the alert becomes sos
  keep_measurement_history_for: 7d
  measurement_history_size_limit: 256MiB
  ```

  With reports every 30 seconds, and measured from the last report of `app-2`:

  | Time | What the hub does |
  | --- | --- |
  | 14:00:00 | Receives the last report of `app-2` |
  | 14:01:30 | Three reports are missing: opens the silence incident and sends the `warn` alert |
  | 14:10:00 | `app-2` has sent no report for 10 minutes: raises the alert to `sos` |
  | When `app-2` reports again | Resolves the incident; every destination that got an alert gets `app-2 is reporting again` |

- **Many silent nodes raise to `sos`.** On the hub,
  `fleet_hub_role.raise_to_sos_when_silent_nodes_reach_count` takes a whole
  count of at least 1. Alternatively,
  `raise_to_sos_when_silent_nodes_reach_percent` takes a number above 0 and
  at most 100, without a `%` suffix. Set at most one of
  `raise_to_sos_when_silent_nodes_reach_count` and
  `raise_to_sos_when_silent_nodes_reach_percent`. Without a count or
  percentage threshold, the number of silent nodes never raises an alert.
  Separate numeric settings make the unit explicit.
  At 5 percent of 30 joined nodes, two silent nodes reach the threshold. In a small fleet a share can be a single
  node: 5% of 10 joined nodes is one, so one silent node is raised to `sos`.
  Use a count, such as `3`, for a small fleet. Each raise is a
  `failure_alert_severity_raised` alert, `2 of 30 nodes are not reporting to
  the hub at the same time, so this alert is now SOS
  (raise_to_sos_when_silent_nodes_reach_percent: 5)`, and a raised alert stays
  `sos` until its node reports again, even after fewer nodes are silent. A
  node whose silence alert is kept back by a whole-node window or by
  maintenance does not count. Without a count or percentage threshold,
  the count raises nothing.

  ```yaml
  fleet_hub_role:
    report_every: 30s
    alert_after_hub_unreachable_for: 3m
    send_alert_without_hub_after_waiting_for: 30s
    undelivered_alerts_of_nodes_content_size_limit: 32MiB
    listen_on_address: 0.0.0.0:8445
    address_for_nodes: util.example.com:8445
    keep_measurement_history_of_nodes_for: 7d
    measurement_history_of_nodes_size_limit: 20GiB
    raise_to_sos_when_silent_nodes_reach_percent: 5
  ```
- **A node group whose nodes are all silent raises to `sos`.** Some nodes
  share one job, such as two servers that run the background jobs of an
  application. Each background-job server warns when only that server is
  silent, because the remaining background-job server can still run the jobs.
  When the two background-job servers are silent, no background job runs
  anywhere, and two warnings would not say so. A node can state
  `raise_to_sos_when_every_node_of_these_node_groups_is_silent` beside its
  node groups: a list of node groups that the node is in. While every node
  of a node group named in the setting is silent, the hub raises the
  node's silence alert to `sos`. The `failure_alert_severity_raised` alert
  gives the reason, `no node of the node group
  jobs_servers is reporting to the hub (jobs-1 and jobs-2 are silent), so
  this alert is now SOS
  (raise_to_sos_when_every_node_of_these_node_groups_is_silent:
  [jobs_servers])`, which names the first such node group of the list and
  at most 20 of its nodes. The raised alert stays `sos` until the node
  reports again.

  ```yaml
  # jobs-1, and the same on jobs-2, /etc/mmwatchdog/mmwatchdog_node_settings.yml
  join_fleet_hub:
    join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code
  alert_severity_when_this_node_goes_silent: warn
  alert_severity_when_this_node_has_rebooted: info
  this_node_is_in_node_groups: [jobs_servers]
  raise_to_sos_when_every_node_of_these_node_groups_is_silent: [jobs_servers]
  keep_measurement_history_for: 7d
  measurement_history_size_limit: 256MiB
  ```

  - The nodes of a node group are the joined nodes whose latest saved
    settings name it, also while they are silent: the nodes of the node
    group in the [fleet view](#fleet-view). The hub keeps no list of node
    groups, so the rule cannot live on the hub. The role that puts a node
    in a node group, which MMX deploys, also states what silence of the node
    group means.
  - Silent means what the raise by count counts. A node whose silence alert
    a whole-node window or maintenance holds does not count as silent.
    A node whose fresh window after a hub restart is not over does not count
    as silent. So a node group with one node in maintenance and another
    silent keeps the silent node's own severity. The whole-node window says
    that the silence of the node in maintenance is expected.
  - Only a node that states the setting is raised; another silent node of
    the node group keeps its own severity, because each node's settings say
    what its own silence means.
  - The hub counts as a node that reports, so a node group that the hub is
    in never raises an alert. In the hub's own settings,
    `raise_to_sos_when_every_node_of_these_node_groups_is_silent` has no effect.
  - The node sends the setting when it joins and in every report, as it
    sends its silence severity. A name that is not one of the node's own
    node groups is a validation error on the node and makes the hub refuse
    the join or the report.
- **Hub restarts are quiet.** A hub restart gives
  every node three fresh report intervals from the hub's start. The restart
  alone sends no silence alerts or notifications of reporting resuming.
  An incident open before the restart stays open. Until a node's fresh
  three intervals are over, the hub does not know whether the node is still
  silent. So the hub does not raise the node's alert by time or count the
  node as silent.

  At the fresh three-report deadline, a still-silent node's alert rises to
  `sos` if
  `raise_to_sos_after_this_node_is_silent_for` has elapsed since the hub
  received the node's last report.

  The hub-outage alerter node treats its own restart in the same way.
- **Replacement is not recovery.** A node that replaces a silent one of the
  same name closes the old silence incident without claiming recovery,
  with the text `<node name> joined the fleet again as a new node;
  MMWatchdog does not know whether the problem is fixed`.
- **Maintenance keeps silence alerts back.** While a node's reported
  whole-node window lasts, the hub sends no silence alert for that node and
  does not raise one; a node that is still silent when the window ends alerts
  then. The time of its `raise_to_sos_after_this_node_is_silent_for` keeps
  counting from the last report, so a node that has
  been silent for that long when the window ends alerts at `sos`.
  `sudo mmwatchdog maintenance on node_reports --for 1h` on the hub holds
  back every node's silence alerts and severity raises. Silence time keeps
  counting, so a still-silent node alerts at `sos` when maintenance ends if
  `raise_to_sos_after_this_node_is_silent_for` has elapsed. A node that reports
  again before the hub's `node_reports` maintenance ends sends no silence
  alert. Maintenance on the hub's `node_reports` check group covers only
  stopped-reporting alerts. Each node's own alerts still go out. Turning on
  maintenance for `every_check_group` on the hub also covers `node_reports`,
  including nodes that join during the window.
- **A standalone node has no hub-outage alerter inside MMWatchdog.** Nothing on a standalone node
  can report that its machine died. MMHotline watches a standalone node
  through check-ins, with warning and SOS times set for the standalone
  node's alert source in MMHotline. Without MMHotline, a second MMWatchdog node detects and
  alerts about the original node's silence. The original node and the
  second node form [a fleet of two](#the-hub-outage-alerter-nodes) and watch each
  other.

The check group name `node_reports` is reserved.

## The hub-outage alerter nodes

The hub chooses two reporting nodes other than itself as its hub-outage alerter
nodes, or one while only one other node reports, and says so in each node's
report replies. A node with
`join_fleet_hub.prefer_this_node_as_hub_outage_alerter: true` takes the role
from a hub-outage alerter without the preference. Otherwise a hub-outage alerter keeps the role
while reporting. A replacement is the
node that has reported continuously the longest, preferred nodes first.
A reporting hub-outage alerter keeps the role when a candidate has equal preference.
So a preference copied to every node changes nothing after the hub chooses
two hub-outage alerters. Watching the hub needs no setup.

**Why two.** The hub's server often runs more than the hub. When one
server, such as `util01`, runs the fleet hub and MMHotline, and that server
dies, the fleet's usual way to alert dies with it. One hub-outage alerter would leave
the hub and that one node failing together unnoticed; with two, the hub can fail
together with any one other node, and the other hub-outage alerter still alerts. A
third hub-outage alerter would cover a rarer failure, and it would send one more copy
of every alert about the hub.

Each hub-outage alerter holds the check `node_reports/to_hub`, fed by its
own reports. The check never collides with a node named `to_hub`: the hub
never watches itself, so `node_reports/to_hub` and the hub's per-node checks
never exist on the same node.
When no report from the hub-outage alerter has reached the hub for
`alert_after_hub_unreachable_for`, the `node_reports/to_hub` check opens a
failure incident. The example sets `alert_after_hub_unreachable_for` to
three minutes. The incident message is `no report from this node has reached the
MMWatchdog fleet hub since 14:02 UTC; mmwatchdog status on this node shows
the latest error`. At the failure deadline, the hub-outage alerter delivers the alert
directly, without the hub or another fallback delay. The hub-outage alerter already
waited for the hub to
be unreachable; another delay would postpone the alert about the outage.

The alert leaves out the failed report's network error: an urgent alert
often reaches a phone's lock screen, where `dial tcp ...: connection
refused` tells the person nothing the title does not, and status and the
journal keep the network error for whoever looks into the outage.

The hub-outage alerter's alert
about the unreachable hub takes the hub's own
`alert_severity_when_this_node_goes_silent`, typically `sos` because nothing
else in the fleet watches the hub. The severity value reaches the hub-outage alerter
in the fleet settings as `alert_severity_when_the_hub_goes_silent`.
The hub's own `raise_to_sos_after_this_node_is_silent_for`, when configured,
reaches the hub-outage alerter in the fleet settings as
`raise_to_sos_after_the_hub_is_silent_for`. The duration counts from the
hub-outage alerter's last report that reached the hub. When the duration has elapsed,
the hub-outage alerter raises the alert about the unreachable hub with the
reason `no report from this node has reached the MMWatchdog fleet hub for
10m, so this alert is now SOS (the hub's
raise_to_sos_after_this_node_is_silent_for: 10m)`. The hub-outage alerter's next
successful report to the hub resolves the `node_reports/to_hub` failure
incident. The `node_reports/to_hub` failure incident counts in the
hub-outage alerter's `mmwatchdog status` exit code. During
`mmwatchdog maintenance on node_reports --for 1h` on the hub-outage alerter, the hub-outage alerter
sends no alert for the `node_reports/to_hub` incident. The hub-outage alerter keeps the
role across its own restarts; after a hub restart
the role may move, which costs nothing.

**Where the alert about the hub goes.** The hub-outage alerter delivers the alert
about the unreachable hub through the hub's destinations kept from the
fleet settings. By default, the hub-outage alerter selects destinations by the
severity of the alert about the hub. If the hub sets
[`send_silence_alerts_of_this_node_only_to_destinations`](#silent-nodes),
the hub-outage alerter uses only the named destinations. The destination list
reaches the hub-outage alerter as
`send_silence_alerts_of_the_hub_only_to_destinations`. A destination that
runs on the hub's server, such as MMHotline on `util01`, is down with the
hub, so the hub names a destination that does not depend on it, such as a
push app:

```yaml
# util01, /etc/mmwatchdog/mmwatchdog_node_settings.yml: the node that is the hub
alert_severity_when_this_node_goes_silent: sos
send_silence_alerts_of_this_node_only_to_destinations: [on_call_push_app]   # the hub-outage alerters' alert about util01 goes only here
```

In words: when reports stop reaching `util01`, each of its two hub-outage alerters
sends an `sos` alert to `on_call_push_app` alone, a destination in
`util01`'s `alert_destinations.yml`, and not to MMHotline.

Both hub-outage alerters alert about one outage, so a person gets the alert twice,
once from each hub-outage alerter. The two hub-outage alerters' alerts about the hub outage
carry an identical [fleet incident key](#one-incident-per-check-across-the-fleet).
So a destination whose service keeps incidents opens one incident for the
outage.

**A fleet of two.** Two nodes are enough for each to be watched. One node
takes the `fleet_hub_role`, and the other joins it. The hub watches the joined
node through reports. The joined node, as the only hub-outage alerter node,
watches the hub and directly delivers the alert about the unreachable hub,
using the hub's saved destinations. A standalone node that must be noticed
when the node's machine dies, and that does not use MMHotline, gets a second
node through a fleet of two.

If the hub and both of its hub-outage alerter nodes fail together, or the hub and
its one hub-outage alerter in a fleet of two, nobody is told until one of them returns,
unless the MMWatchdog fleet hub is connected to the optional MMHotline,
which alerts when the check-in the MMWatchdog fleet hub sends to
MMHotline stops arriving ([MMHotline in a fleet](#mmhotline-in-a-fleet)).

## Maintenance and deploys in a fleet

Deploys open a whole-node [maintenance window](recovery.md#maintenance-windows)
on each node they touch ([deploys](recovery.md#deploys-and-recurring-windows)).
The whole-node window is named `every_check_group` and covers all check
groups on the node, including groups added while it is on. There is no
fleet-wide maintenance command. In a fleet:

- Maintenance alerts are ordinary alerts, so the alerts go through the hub.
  The hub collects maintenance alerts by reason and user rather than by
  check group. The first maintenance start for a reason and user produces
  one message. Later starts for the reason and user, such as a rolling
  deploy's next batch, produce no additional start message.

  Once no node has reported a current maintenance window for the reason
  and user for ten minutes, the hub announces the maintenance end.
  The end message names the nodes whose maintenance windows ended.
  A deploy therefore produces one `maintenance_on` and one
  `maintenance_off` alert.

  The `maintenance_off` message says whether maintenance was turned off,
  expired, or ended because a check group was removed. The
  `maintenance_off` message gives the maintenance end time, or the last
  end time for several nodes. The `maintenance_off` message says that each
  node's alerts resumed when the node's maintenance ended. The
  `maintenance_off` message also explains why the hub announces the end ten
  minutes later. Without the explanation, a reader could mistake the
  `maintenance_off` message's arrival time for the time alerts resumed.
- One `mmwatchdog maintenance on` command on a node can name several check groups, and
  that command sends one `maintenance_on` alert, which names its check groups,
  not one alert for each check group. The hub collects the command's
  `maintenance_on` alert by reason and user like any other maintenance alert.
- A node's reports carry its whole-node window. `mmwatchdog maintenance on` and
  `off` on a joined node send a report at once. The command returns
  successfully only once the hub has accepted the report. If reporting fails, the command exits 1 and says
  that the hub does not know of the change.

  For example, a playbook can reboot a node right after `maintenance on`.
  A node waits up to `report_every` for the first report after startup.
  Without the immediate report from `maintenance on`, the hub could count
  the node as silent during a planned reboot. Until the window ends, the hub
  sends no silence alert for that node, so a reboot during a deploy stays
  quiet, and the node sends no `rebooted` alert of its own during the same
  window. A `rebooted`
  event has no `check_group`, because it describes the node. The fleet
  view shows nodes in maintenance with the end time, reason, and user, even
  while they are silent.
- If a deploy fails midway, the window still expires and alerts resume.

## One incident per check across the fleet

Some services keep incidents of their own. Pushover repeats an emergency
until cancellation. In a fleet, one check can fail on many nodes at once. Such a service must open
one incident for the check, not one for each node, and close the incident
only when the last node has recovered. So MMWatchdog gives every
destination of such a service one incident per check across the fleet.

**Which destinations.** A destination keeps incidents when the
[module description](checks.md#the-module-description) of its module states
`service_keeps_incidents: true`, as the official extra module
[`pushover`](../../../official_extra_modules/pushover/README.md) does, and so
can a module of the project's own. The
[`mmhotline` destination](../../reference/modules/mmhotline.md) always keeps incidents
([MMHotline in a fleet](#mmhotline-in-a-fleet)). For a key with an
[exact finding](checks.md#exact-findings), every destination, also a chat
or a mailbox, gets one incident per key, by the rules of
[one alert per exact finding](#one-alert-per-exact-finding). For every
other key, a destination whose service keeps no incidents gets every
node's alerts unchanged.

**The key.** Every alert about an incident carries `fleet_incident_key`: the
same 32 lowercase hexadecimal digits for one check on every node of the
fleet. A standalone node has its own scope. MMWatchdog starts with a readable key:

| MMWatchdog incident | Readable key |
| --- | --- |
| A check's failure incident on any node | `{check_group}/{check} failed`, such as `myapp/http failed` |
| A check's cannot-check incident on any node | `{check_group}/{check} cannot check` |
| One failed unit of an [`every_systemd_unit`](../../reference/modules/every_systemd_unit.md) check on any node | `{check_group}/{check}: {systemd_unit} failed`, such as `failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service failed` |
| A joined node [stopped reporting](#silent-nodes) | `node_reports stopped reporting`, one key for every silent node |
| A [hub-outage alerter node](#the-hub-outage-alerter-nodes) cannot reach the hub | `node_reports/to_hub failed`, the same key on the two hub-outage alerter nodes |

MMWatchdog first hashes the readable key to 32 lowercase hexadecimal
digits. MMWatchdog then hashes the scope, a zero-byte separator, and the
check key, again keeping the first 16 bytes as 32 hexadecimal digits.
The scope is
`fleet:` followed by the hub certificate's SHA-256 fingerprint in a fleet,
or `node:` followed by the node's name on a standalone node. The hub and
its joined nodes get the same fingerprint from the join code. Copying the
hub's state when moving it keeps the fingerprint; an independent fleet or
standalone node gets a different scope. Including the scope in the second
hash prevents unrelated nodes from merging or resolving each other's incidents.

Every destination, including `mmhotline`, receives the fixed-length scoped
key. Long names cannot be clipped into the same key. Failures,
cannot-check alerts, raises, reminders, and closing alerts (`recovered`,
`can_check_again`, `closed_without_recovery`) keep the same key of their
incident kind. Notices, reboots, and maintenance alerts have none.
A grouped message of [fleet alert grouping](#fleet-alert-grouping) carries
the key of its alerts.

**One closing alert when the last node recovers.** The MMWatchdog fleet hub
keeps a set for each destination whose service keeps incidents, and each
key, and for every destination and each key with an exact finding. The
rules below hold for both; [one alert per exact finding](#one-alert-per-exact-finding)
adds its own rules for the second, among them its closing wording. The set contains open MMWatchdog incidents whose alerts went to the
destination, with each incident's severity. The hub updates the set from
alerts the hub sends and from each node's latest report.

Failures, raises, and reminders go out unchanged. A node's recovery
or retirement removes the node's incident from the set. While another
incident of the key remains open, the destination receives no closing alert
for the removed incident.
When the set empties, the destination gets one closing alert:

- When a node's own recovery or retirement closed the key, and every
  incident of the key that closed came from that node, the hub uses the
  node's own closing alert. For example, `app-2: myapp/http is ok again`.
  So a standalone node, and a check that fails on one node only, send what
  the standalone node and the single failing node would send without the set.
- Otherwise the hub creates a new closing alert about the key across the
  fleet. The new alert covers incidents of several nodes, and one node when
  a report closed the key. For one node, the report no longer shows the
  incident, the node was forgotten, or the wait after a hub start ended.

  The new fleet closing alert has the title
  `myapp/http is ok on every node again`. The new fleet closing alert's
  message is
  `myapp/http is ok on every node again (was failing on app-2, app-3)`.
  The new fleet closing alert lists nodes in `nodes` and `node_count`, and
  has no `incident_id`.

  The new fleet closing alert's type is `recovered` for a failure key.
  A cannot-check key uses `can_check_again`:
  `MMWatchdog can check
  myapp/http on every node again`.
  When a node's retirement alert or a forget closed the last incident,
  the new fleet closing alert's type is `closed_without_recovery`.
  The new fleet closing alert's message is
  `myapp/http incidents closed
  on every node, maybe not fixed`.

  When incidents from only one node closed, the new fleet closing alert's
  title and message use the node's own words. For example,
  `app-2:
  myapp/http is ok again` or
  `app-2 reports to the MMWatchdog fleet hub
  again`.
  The node's failure alert named the node, so the closing alert must pair
  with the failure alert for a reader.

  A report cannot tell a recovery from a retirement, so a key that a report
  closes is `recovered` or
  `can_check_again`.
- The node's own closing alert and the new fleet closing alert each carry
  `alert_severities_sent_for_incident` and `alert_severity`.
  `alert_severities_sent_for_incident` lists the severities the receiving
  destination got for the key since the last closing alert for that
  destination and key. `alert_severity` is the highest listed severity.
  So two destinations of one service that take different severities each
  close only what the destination opened. For example, a `warn` destination
  and an `sos` destination can use the same Pushover application token.

```yaml
# util, /etc/mmwatchdog/alert_destinations.yml: the node that is the hub
destinations:
  on_call_pager:
    remind_every: never
    pushover:                                    # an official extra module whose service keeps incidents
      application_token_file: /etc/mmwatchdog/secrets/pushover-application-token
      user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-on-call-group-key
      repeat_sos_alerts_every: 5m
      stop_repeating_sos_alerts_after: 1h
    alert_severities: [warn, sos]
  team_chat:
    remind_every: never
    slack:                                        # a chat, which keeps no incidents
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]
```

When the check `myapp/http` fails on two nodes, the hub sends:

| Time | What happens | `on_call_pager` gets | `team_chat` gets |
| --- | --- | --- | --- |
| 14:00 | `myapp/http` fails on `app-2` | `app-2: myapp/http failed`, which Pushover repeats at emergency priority for an `sos` alert | `app-2: myapp/http failed` |
| 14:05 | `myapp/http` fails on `app-3` | `app-3: myapp/http failed`, whose `sos` emergency repeats carry the same tag | `app-3: myapp/http failed` |
| 14:10 | `app-2` recovers | nothing | `app-2: myapp/http is ok again` |
| 14:12 | `app-3` recovers | `myapp/http is ok on every node again`, which cancels the repeats by tag and sends a normal-priority message | `app-3: myapp/http is ok again` |

- An incident joins the set when the hub sends the incident's failure or
  raise to the destination. An incident also joins when a node's report
  shows an announced incident.
  The alert must have a severity the destination takes, or the check must
  name the destination with `send_alerts_only_to_destinations`.

  Reports also carry the severities of each incident's alerts. A raise
  that the hub never delivered still counts, such as a raise the node sent
  without the hub. The set takes only severities routed to the destination,
  because the closing alert lists what the destination got.
- A reminder goes out only while its incident is open in the set. A
  reminder that waited in fleet alert grouping while the key closed would
  open the service's incident again, with nothing left to close it. It
  goes under the key of its incident's row in the set, or, when the set
  has no row yet, under the key that the hub derives from the incident,
  never under a key that a node's alert carried.
- A failure still waiting in the one-minute [fleet alert
  grouping](#fleet-alert-grouping) window keeps its key open, so no closing
  alert goes out before a failure that is about to be sent. A failure whose
  incident the set already knows is over is not sent at all.
- An incident leaves the set when its recovery or retirement reaches the
  hub, or when a report that the node sent after the incident opened no
  longer shows it, such as after a recovery that the node did not send
  without the hub. A report sent before the alert never removes the incident.
- A silent node whose last report shows the check failing keeps the key open
  until it reports again or is forgotten. Forgetting a node removes its
  incidents, also when the hub restarts before it has applied the forget.
- After the hub starts, no key closes until every node has reported once,
  or three report intervals have passed. A node may have failed while the
  hub was down and sent its failure without the hub, and the hub does not
  know about the failure incident sent without the hub before the node's
  report. The incidents still close in the set meanwhile, and their keys
  close once the wait is over.
- **A failure goes out again when a report shows it after the key closed.**
  Nodes that were cut off from the hub report again in any order. A key
  can close before a node that still fails has reported. The node sent
  the failure to the service without the hub, but the key's closing alert
  closed the service's incident.

  When a report adds an open incident to the set, the hub compares the
  incident's opening time with the destination's last closing-alert time
  for the key. If the closing alert went out after the incident opened,
  the hub sends the incident's failure again. The hub reconstructs the
  failure from the report: node, check, kind, severity the destination
  takes, key, and incident opening time. The service opens the incident again, and the key closes
  once more when the node recovers.

  A failure that the destination gets twice costs less than a service
  that shows no open incident while a node fails. The hub keeps the time
  of each destination's last closing alert for a key for a day while the
  key has nothing open.
- The hub remembers a closed incident for up to a day after the closing
  alert. While the hub remembers a closed incident, a report sent before
  recovery cannot add the incident again, neither to a set nor to the
  hub's node incidents, which decide whether a closing alert can say "on
  every node". A direct send that such a report carries settles as
  closed: it gets its closing alert and makes the node no open member.
- The sets live in the hub's `state.db`, so a restart of the hub loses none.
  A set belongs to its destination: removing or renaming the destination,
  or a module that no longer states `service_keeps_incidents: true`, starts
  it over.
- A standalone node keeps incident sets per destination and key, containing
  only the standalone node's incidents.
- If the hub is lost permanently, a person closes the incident in the service.

**Alerts sent without the hub.** A joined node that delivers an alert
itself, because the hub did not accept it in time
([alerts in a fleet](#alerts-in-a-fleet)), sends failures, raises, and
reminders to such a destination with the key, so they join the fleet's
incident there. The joined node never sends such a destination an alert
that closes an incident, because another node may still fail.

The joined node retains a record of each incident the joined node sent
without the hub, with the destination and every severity sent by the
joined node. The node records a send when the destination accepts it, in
the same transaction that removes the delivery from the queue; a send
that waits to be retried is not in the record yet. The joined node
includes the records in reports as `incidents_sent_without_the_hub`. The
records have no age or count expiry. The joined node removes a record
only after a report reply confirms that the hub has settled the record,
in `incidents_known_to_the_hub`, and only when the record has not changed
since the report that the reply answers. The hub confirms a record only
once it has settled that version of it: the severities sent, the alert
source, the sequence of the exact finding, and when the destination
accepted the node's latest direct reminder. A reminder is recorded like a
failure, because it can open the service's incident again, such as
Pushover's repeats, also after the hub's closing alert; the time of its
acceptance makes a new version even at a severity already recorded. This
holds for every key,
with or without an exact finding. So the hub, when back, knows the
incident and sends the closing alert, even if the joined node recovered
while the hub was down.

The hub settles a send that it learns of against the key's state at that
time, for every key. When the hub has already sent the key's closing alert, the send may
have reached the destination after it, such as a retry that MMHotline
accepted late, or under another alert source, so the send gets a closing
alert of its own.

For a key with an exact finding, the joined node keeps such a record for
every destination, also a chat or a mailbox, and sends no recovery
there, so the hub sends the closing alert later. For every other key, a
chat or a mailbox gets the node's direct recovery, as from a standalone
node.

The hub settles each record whatever the sets hold. It records the direct
delivery, decides the destination's closing alert, and then confirms the
record. When the incident is [set aside](#set-a-finding-aside) on the hub,
and nothing else of the key is open for the destination, the hub queues
`closed_without_recovery` with `Set aside after investigation: <reason>`.
The hub decides this once every report of the batch is applied. While a
failure of the key still waits in fleet alert grouping, the hub settles
no set-aside send of the key and does not confirm its record: a later
report settles it. The
severities of the set-aside send count for the key's closing alert, so a
key that closes in the same batch for another reason gets that one closing
alert, at the highest severity sent and under each alert source that the
key reached; the set-aside closing alert is then not sent. Like every
closing alert, the set-aside closing alert records its time, so a node
whose later report shows an incident of the key that opened before it gets
its failure sent again. So a set-aside incident never holds back the
confirmation of a report's records. When the incident is not set aside, the hub marks the node
`joined` for the next update of [one alert per exact finding](#one-alert-per-exact-finding),
because MMHotline may have merged the node's direct warning into another
node's ([MMHotline in a fleet](#mmhotline-in-a-fleet)). A direct reminder
marks no node that the key's set already held open, at a severity at
least as high as the reminder's, before the report: the destination had
the node already, from the hub or from an earlier direct send, so the
node is not newly affected. This holds also when the reminder is the
node's first direct send of the incident. MMHotline gets no reminders, so for MMHotline such a
version changes nothing.

A report carries as many unconfirmed records as fit its request size;
remaining records follow after earlier records are confirmed.
A joined node learns which
destinations keep incidents from the fleet settings
(`destinations_whose_service_keeps_incidents` in its `fleet-settings-from-the-hub.json`),
which the hub takes from its own modules. A node whose copy of a module is
missing or older therefore never sends such a destination an alert that
closes an incident.

Module pages that rely on "One incident per check across the fleet":
[`mmhotline`](../../reference/modules/mmhotline.md), [`webhook`](../../reference/modules/webhook.md),
[`pushover`](../../../official_extra_modules/pushover/README.md).

## MMHotline in a fleet

A fleet works fully without MMHotline. When a fleet uses MMHotline, the
MMWatchdog fleet hub holds the
[`mmhotline` destinations](../../reference/modules/mmhotline.md), one for each team's
MMHotline. The fleet hub is the fleet's one sender to each destination.
Joined nodes reach MMHotline only when they deliver an alert themselves,
without the hub. An `mmhotline` destination keeps one incident per check
across the fleet, as every destination of a service that keeps incidents does
([one incident per check across the fleet](#one-incident-per-check-across-the-fleet)).
"MMHotline in a fleet" describes behavior specific to the `mmhotline`
destination.

**MMHotline receives the fixed-length scoped key.** The message sent to
MMHotline has `sender_incident_key` equal to the alert's `fleet_incident_key`
([the key](#one-incident-per-check-across-the-fleet)). Only `sos` alerts open
MMHotline incidents
([severities](../../reference/modules/mmhotline.md#what-it-sends)). A fleet key's alert
carries the highest [alert severity](alerts.md#alert-severity) of the key's
open incidents, so once one node's incident is `sos`, the key is `sos`. The
first `sos` alert for a key opens the MMHotline incident and notifies the
person on call. Later alerts with the key join the MMHotline incident
without contacting anyone again, including the grouped message
`myapp/http failed on 7
nodes`. So 40 nodes going silent together and
raised to `sos` produce one MMHotline incident and one notification of the
person on call.

A `warn` check without an exact finding, failing on 40 nodes over an
hour, produces up to 40 `warn` alerts. A finding with an exact finding
produces one first alert and growth updates
([one alert per exact finding](#one-alert-per-exact-finding)). A raise is a
new alert for the key, so a `warn` key raised to `sos` opens the MMHotline
incident then.

**What MMHotline gets.** MMHotline refuses a request with a field that it
does not know, so MMWatchdog adds no field to the signed request. Each
kind of message maps to a severity and a key:

| Message | MMHotline severity | `sender_incident_key` |
| --- | --- | --- |
| The first alert of a key | The alert's severity | The key |
| An update that adds nodes: new nodes, or nodes whose exact finding changed to one already announced | The highest severity among the added nodes, on the shared tick, so MMHotline's triage agent investigates the new nodes | The key |
| An update with only recovered nodes | `info` | The key |
| An exact finding not announced before, or a finding no longer set aside | The severity of the node that brought it, never raised | The key |
| An update that adds `sos` nodes | `sos`, each minute, joining the open incident | The key |

Added nodes go at their own severity, not as `info`, because a triage
agent's verdict without a [set-aside entry](#set-a-finding-aside) holds
only for the nodes that it checked. A node that an entry covers joins no
destination's set, so it is in no update. An `info` message with a key
goes to the team's info destinations in MMHotline and starts no triage
run. A `warn` after the key's triage session closed starts a new one.
Every message has its own `sender_alert_id`; an update takes a new event
ID.

**The first line names the problem the same way on every node.** MMHotline
merges a repeated warning from the alert source for nodes sending without
the hub (**two alert sources**, below) by key and by the message's first
line, within one hour. So the first line of every `warn` and `sos` message
to MMHotline names the problem with no node name, from the hub and from a
node that sends directly. Node names go in the title or in later lines.

- For a key with an exact finding, the first line is the exact finding.
- An update's first line is its summary, such as
  `packages/flaws: CVE-2026-123158 openssh: 3 nodes added`, which names no
  node and differs from a node's own first line, so MMHotline never merges
  an update into a node's warning.
- Otherwise the first line is the node's own failure message without the
  node's name. Different causes have different first lines and are never
  merged.

A first line that holds something that differs from node to node, such as
a number or a node's own address, is never merged. During a hub outage on a
large fleet, such a check can use up MMHotline's hourly budget for
warnings. A module whose message starts with a number that is not the
cause moves that number off the first line.

**One resolution when the last node recovers.** When a key's set empties,
the fleet hub sends MMHotline one resolution. When incidents from several
nodes closed, the resolution uses the fleet closing alert's words:
`myapp/http is ok on every node again (was failing on
app-2, app-3)`.
When incidents from only one node closed, the resolution uses the node's
words, such as `app-3: myapp/http is ok again`. The resolution's words pair
with the node's failure alert `app-3: myapp/http failed`.

A failure that a report shows after the resolution goes out again as for
every destination. If the failure is `sos`, the repeated failure alert
opens a new MMHotline incident.
The resolution's severity is `resolved-` followed by the highest severity
sent for the key since the key's last resolution. A standalone node's
resolution is its own recovery, such as
`web-3: myapp/http is ok again`.

**The SOS resolves as soon as no node is `sos`.** One of thousands of app
servers going silent must not wake people, and must not keep an SOS
escalating after the `sos` problem is over. So when a key was sent at `sos`
and no incident left in its set is `sos`, the fleet hub sends `resolved-sos`
at once, which closes the MMHotline incident, naming the nodes still open:
`No node that stopped reporting to the MMWatchdog fleet hub is SOS any more;
still Warning on app-7`.

After `resolved-sos`, the key has the severity of the remaining open
incidents. A later raise to `sos`, or a new `sos` incident, sends a new
`sos` alert and opens a new MMHotline incident. When the last incident
closes, the resolution uses the highest severity sent since the last
resolution. Without a later `sos` alert, the final resolution can be
`resolved-warn`, never a second `resolved-sos` for the already resolved SOS.

The fleet hub keeps the severities sent for each key in `state.db`, so
restarts preserve the severity used for resolutions. An incident a node
sent without the hub at `sos` counts as `sos`.
A `resolved-sos` counts as a closing alert of the key when a report shows a
failure after it: the failure goes out again.

**The set belongs to one alert source.** An `mmhotline` destination's set
follows the destination's alert source ID. A new address or signing secret
keeps the set when the destination's alert source ID stays unchanged.

Another alert source ID starts the set over, as removing the destination
does.

**Alerts sent without the hub.** The fleet hub copies each MMHotline signing
secret into each joined node's `fleet-settings-from-the-hub.json` (under
`file_contents_by_destination`, beside the webhook tokens and the mail
server passwords). An alert the fleet hub has not accepted within
`send_alert_without_hub_after_waiting_for` goes from the joined node
straight to MMHotline, at the destination's `mmhotline_url`, with the
alert's `fleet_incident_key`, so the alert joins the fleet incident. When
the destination states `alert_source_for_nodes_sending_without_the_hub`,
the node sends under that alert source (**two alert sources**, below).
The direct alert's message ends `Sent directly by the
MMWatchdog node app-2 because the MMWatchdog fleet hub did not accept the alert in
time.` The joined node therefore needs an address that it reaches, which a
loopback address on the hub is not, and `mmwatchdog validate` warns about
one on a hub. A joined node never sends a resolution: its reports carry
each incident that it sent without the hub, and the fleet hub, when it is
back, sends the resolution
([alerts sent without the hub](#one-incident-per-check-across-the-fleet)).

**Check-ins and whole-node maintenance.** The MMWatchdog fleet hub sends
each `mmhotline` destination a check-in every 30 seconds
([check-in](../../reference/modules/mmhotline.md#what-it-sends)), because each team's
MMHotline watches its own alert source. Joined nodes never send check-ins,
because the fleet hub already watches joined nodes through node status
reports. Each check-in lists every node in a whole-node maintenance window
(`every_check_group`),
taken from the nodes' reports and including the fleet hub's own window, with
its end time, reason, and the user who started it. Check-group windows are not
listed. MMHotline treats:

- the fleet hub's own whole-node window as planned downtime for its check-ins, so a planned restart or move of the fleet hub sends no missing-check-in alert from MMHotline; and
- a listed node's window as planned downtime for an MMHotline outside check
  (MMHotline requesting a public URL from outside) that the team tied to
  that node.

If the fleet hub stops sending check-ins during a window, MMHotline
keeps honoring the last windows it received until they end.

**Two alert sources.** The hub sends under the destination's own alert source,
`mmhotline_alert_source_id`, and MMHotline merges no warning from it, so a
growth warning is never folded into the first warning and lost. A fleet
can give the nodes that send without the hub a second alert source, in the
`mmhotline` destination's optional block:

```yaml
# util, /etc/mmwatchdog/alert_destinations.yml: the node that is the hub
destinations:
  team_hotline:
    mmhotline:
      mmhotline_url: http://10.20.0.1:8484
      mmhotline_alert_source_id: mmwatchdog_fleet
      mmhotline_signing_secret_file: /etc/mmwatchdog/secrets/mmhotline-fleet
      alert_source_for_nodes_sending_without_the_hub:
        mmhotline_alert_source_id: mmwatchdog_nodes_without_the_hub
        mmhotline_signing_secret_file: /etc/mmwatchdog/secrets/mmhotline-nodes-without-the-hub
    alert_severities: [warn, sos]
```

- Only the second source merges repeats in MMHotline, so a hub outage on a
  large fleet cannot use up MMHotline's hourly budget. Without the block,
  direct sends use the hub's source.
- The block may state its own `mmhotline_url` when the nodes reach
  MMHotline at another address than the hub. Its
  `mmhotline_alert_source_id` must differ from the destination's own.
- The block reaches the joined nodes in the fleet settings, like the main
  signing secret. It is left out of the fleet settings' fingerprint when it
  is not set, so fleets without it are unaffected. A fleet that sets it
  needs 0.2.0 on every joined node, because an older node refuses the
  changed fleet settings. `mmwatchdog validate` on the hub warns while a
  joined node reports an older version.
- MMHotline matches an incident by alert source and key. So once the hub
  has settled a node's direct send of a key under the second source, it
  sends every further alert of that key under the second source, until
  the key's closing alert. The hub has both secrets. Otherwise an `sos`
  from the hub would open a second incident and wake someone again. The
  closing alert, and a `resolved-sos`, go under each source that got
  alerts of the key since its last closing alert, each with its own
  `sender_alert_id`, so an incident that the hub opened under its own
  source before it went away closes too. The hub keeps these sources for
  each destination and key in `state.db`.
- A joined node's record of a direct send names the ID of the source it
  went under, also the destination's own. So when the destination's own
  source changes before the node's report reaches the hub, the closing
  alert goes under the old source while the destination still has it,
  such as its second source; otherwise it is a delivery gap and a
  validation warning, as for a removed second source. A 0.1.x node's
  record names no source; the hub takes it as the destination's own.
- Check-ins go on the hub's source only, because MMHotline never expects
  check-ins from a source that has never sent one.
- A resolution on the second source ends MMHotline's one-hour merge window
  there, so the next outage starts fresh.

Module pages that rely on "MMHotline in a fleet":
[`mmhotline`](../../reference/modules/mmhotline.md).

## Set a finding aside

After an investigation, a team may decide that a finding needs no action
on any node with that exact finding, such as a flaw that needs a library
that none of the nodes use. The hub's optional file
`/etc/mmwatchdog/set_aside_findings.yml` sets such a finding aside, keyed
to its [exact finding](checks.md#exact-findings):

```yaml
# util, /etc/mmwatchdog/set_aside_findings.yml: the node that is the hub
set_aside_findings:
  - check: installed_packages/known_exploited_flaws
    finding: CVE-2026-123158 openssh
    exact_finding: "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z"
    reason: Needs musl without PAM; every server with this exact finding runs glibc and sshd links PAM. Debian rates it unimportant.
    investigation_note: triage_agent_notes/2026-11-23_openssh_package_warning_on_app001.md
    set_aside_by: triage agent (MMHotline run b82fdd8b)
    set_aside_on: 2026-11-23
```

- **The file is on the hub only.** A joined node and a standalone node
  refuse it. It is not part of the fleet settings, so joined nodes never
  get it, and nodes older than 0.2.0 are not affected.
- **Every field of an entry is required and nonempty.** A missing field is
  a validation error. `set_aside_findings: []` is valid and sets nothing
  aside. There is no entry for a whole check, and no entry without an
  exact finding. MMWatchdog defines only the format and the path; where the
  file comes from, such as a file in Git that a deploy copies to the hub,
  is the team's choice.
- **An entry matches only an exact match** of the check, the finding's
  `id`, and the exact finding. A finding without an exact finding never
  matches. A new installed version, release, fix state, or advisory record
  changes the exact finding, so the entry stops matching and the finding
  alerts again. An entry has no expiry date.
- **`sos` is never set aside.** An entry covers only incidents below
  `sos`, in alerts, in the fleet view, and in the listing. A finding at
  `sos` alerts, whatever the file says.
- **The hub applies the file everywhere it handles alerts:** alert intake,
  report reconciliation, reminders, and updates. A covered node incident
  stays in the hub's records with the entry that covers it, and never
  joins a destination's set. The exact finding with the highest sequence
  number decides whether an entry covers the node incident.
- **The hub settles the file at every start and every reload,** against
  the open node incidents:
  - An incident that is newly covered leaves each destination's set. A
    destination whose set for the key empties gets one
    `closed_without_recovery` alert, `Set aside after investigation:
    <reason>`, so an MMHotline or Pushover incident that opened before the
    verdict closes. A node whose exact finding moves onto an entry is
    newly covered too.
  - Incidents that are no longer covered, because an entry was removed or
    changed or a node's exact finding moved off the entry, are applied
    together first, by the rules of grouping by exact finding. An
    incident whose exact finding the destination already has at its
    severity is covered growth for the key's next update. For the others,
    each destination and key gets **one** opening alert, with the note
    `an earlier exact finding was set aside: <reason>`. The alert names
    each of their exact findings with its nodes, and announces only the
    exact findings that it names; an exact finding that does not fit
    whole in the message's bound, counted with the text that the alert
    sends, goes in the next update. The hub keeps the reason on the node
    incident while it is set aside, so the reason is known after the
    entry is gone.
- **Joined nodes never set anything aside.** While the hub is unreachable,
  nodes send every alert themselves, so a hub outage can only add alerts.
  `mmwatchdog status` on the node still shows the finding as failing,
  because the node does not know the verdict.
- **The fleet view counts set-aside findings apart.** A covered finding
  below `sos` does not set the exit code of `mmwatchdog fleet` and does not
  expand its node. `mmwatchdog fleet` shows one compressed line, such as
  `Set aside: 1 finding on 5000 nodes`, and `mmwatchdog fleet NODE` shows
  the finding as `set aside (reason …) until the exact finding changes`.
- **An invalid file is a validation error.** `mmwatchdog validate` refuses
  it, and `mmwatchdog reload` keeps the previous configuration.
- **Removing the file.** A deploy that no longer has the file must remove
  it from the hub. The example deploy playbook
  `examples/deploy/playbooks/hub_and_standalone.yml` removes the hub's
  `set_aside_findings.yml` when the project has none, as it removes check
  group files that the project no longer has. A project whose deploy tool
  refuses a missing source file keeps the file with
  `set_aside_findings: []` instead.

**Two commands on the hub show what is set aside and where a finding is.**

- `mmwatchdog fleet set-aside-findings [--json]` lists each entry in
  effect with its reason, investigation note, and date, and:
  - `covered_node_count`, the number of nodes that it covers now, and
    `covered_nodes`, up to 20 of them;
  - `matches_no_node_now`, computed over every node's latest saved report,
    also the hub's and silent nodes';
  - `nodes_that_cannot_check_now`: nodes whose check is `unknown` or has
    an open cannot-check incident, nodes that wait for a first report, and nodes without a saved report. Remove
    an entry only when `matches_no_node_now` is true and this count is 0.
- `mmwatchdog fleet findings CHECK_GROUP/CHECK [--finding ID] [--json]`
  lists every node with the check's findings, each with its exact finding
  and its first failure time, with no limit on the count. An update that
  names only 20 nodes for an exact finding points to this command.

**Facts that MMWatchdog cannot see.** A verdict can rest on facts of the
running nodes that the exact finding does not hold, such as which C
library a node uses. An entry is safe only when the evidence holds for
every node with that exact finding. MMWatchdog guarantees only that a
changed package, release, fix state, or advisory record alerts again.

## Forget a node, rotate the join code, move the hub

To remove a node permanently, stop MMWatchdog on the node before running
`mmwatchdog fleet forget NODE`.

```sh
sudo mmwatchdog fleet forget app-2          # on the hub
sudo mmwatchdog fleet rotate-join-code      # on the hub
```

**Forget.** `mmwatchdog fleet forget NODE` deletes the node's entry, latest report,
event history, measurement history on the hub, and credential. The readings
are in a file of their own, so the hub removes them right after the node's
entry; readings of a node that the hub no longer lists also go when the hub
starts. The hub's
`node_reports/NODE` check for the forgotten node retires, and an open
silence incident closes with the message `NODE was removed from the fleet;
MMWatchdog does not know whether the problem is fixed`, not as a recovery.
If the forgotten node was a hub-outage alerter node, the hub chooses a replacement
hub-outage alerter node when the hub accepts the next report. The hub cannot
forget itself.

A forgotten node that still runs finds the node's credential refused and
joins again visibly. The forgotten node then resends the local events and
readings the node still retains. MMX can run the forget when MMX removes a
node.

**Rotate.** `mmwatchdog fleet rotate-join-code` replaces the join secret and prints
the new code. Joined nodes keep reporting with their credentials, whichever
code the file named by `join_fleet_hub.join_code_file` holds. The old code
joins nothing. Deliver the new code to every node anyway, so each can join
again when it must.

**Move.** Restoring the hub's `/var/lib/mmwatchdog` on a new machine, with the daemon
stopped, keeps its identity, so nodes keep reporting with their credentials.
If the hub's address changes, set `fleet_hub_role.address_for_nodes` and
deliver the new join code, which keeps the fingerprint and secret.
A hub started without the old hub's
`/var/lib/mmwatchdog` creates a new identity and join code. Until a node has
the new code, the hub's certificate does not match: its reports fail,
it delivers its alerts itself
after `send_alert_without_hub_after_waiting_for`, and the hub-outage alerter alerts after
`alert_after_hub_unreachable_for`. Once a joined node has the
new code, the joined node joins the new hub.

## Fleet decisions

The fleet decisions below are settled. Revisit a fleet decision only when
a concrete workflow fails because of the decision.

| Decision | Reason |
| --- | --- |
| Listening addresses state the local host, including an explicit wildcard for every interface | A reader sees which interfaces accept connections; an omitted host must not hide the listener's exposure |
| Separate listening and advertised addresses | The local interface and the address nodes dial can differ behind a proxy or port forwarding |
| Silence raises count from the last report | Missed report intervals are already time without contact and must not add a second waiting time |
| Scoped, fixed-length incident keys | Independent fleets and standalone nodes must never merge or resolve each other's incidents, and long names must never collide through clipping |
| Fallback incident records wait for hub confirmation | Age or count expiry could leave a destination's incident open forever after a long outage |
| Event history accepts a time span | Retained diagnostics must remain accessible beyond the default latest 100 events |
| Membership is defined by joining; there is no node inventory or "expected nodes" list | MMX's inventory already decides which nodes get `join_fleet_hub`; a second list would drift |
| Nodes push; the hub never connects to a node and holds no credential for one | A compromised hub cannot reach into nodes, and nodes behind NAT or firewalls work |
| The hub can run outside the network of its nodes | Nodes push to one port of the hub, so the fleet view stays reachable when the nodes' network is not |
| Nodes pin the hub's certificate fingerprint from the join code | Nobody runs a certificate authority, and the code carries everything needed |
| One hub holds the one copy of the alert settings; a node with `join_fleet_hub` refuses its own `alert_destinations.yml` | There is never doubt about where alerts go |
| A check on a joined node may name hub destinations with `send_alerts_only_to_destinations`; a name the hub does not have sends the alert by severity and shows in `mmwatchdog status` | A nonempty destination list falls back to severity only when the hub recognizes none of the names. The reader of a check sees who is told, the destinations still live in one place, and a wrong name never loses an alert |
| Every official extra module and every module of the project's own is deployed to every node | A node that delivers without the hub runs the module itself |
| Fleet timings are set on the hub only | A node can never disagree with the hub about silence |
| A failed report is not retried | The next complete report supersedes the failed report. Replaying old state is worse than waiting one interval |
| Silence is a heartbeat created by joining: three missed intervals | One mechanism for "expected contact did not arrive"; one late report never alerts |
| Hub restarts grant a fresh window, and no silence alert is raised before it is over | Hub downtime never produces false silence alerts, and hub downtime never counts as a node's silence either. The fresh window delays silence detection, but the time for a raise to `sos` still counts from the last report |
| A node can name at most 100 node groups | A join request comes from a machine that the hub does not know yet, so its size is fixed at 64 KiB, and a node whose configuration is valid must always be able to join; a fleet view also shows the node groups of every node that it lists |
| A name conflict is refused while the named node is alive; a silent one is replaced | Cloned VMs cannot mix data, and rebuilt nodes keep their names without operator steps |
| Late and duplicate alerts around a hub outage are accepted | Delivery without the hub keeps alerts flowing. Receivers deduplicate by event ID |
| The hub keeps two hub-outage alerter nodes, or one while only one other node reports | The hub's server can also run MMHotline, as on `util01`, so a server failure can stop the usual alert path. With two hub-outage alerters, a surviving hub-outage alerter alerts if the hub and another node fail together. A third hub-outage alerter covers a rarer failure and sends another copy of every alert about the hub |
| A node can name the destinations of its silence alerts, `send_silence_alerts_of_this_node_only_to_destinations`; on the hub, that is where the hub-outage alerters send their alert about the hub | No user-configured check defines the silence alert, so the node states the silence-alert route beside the node's silence severity. The alert about a dead hub must reach a person through a destination that does not run on the hub's server. Every other alert keeps the usual route |
| The hub and every one of its hub-outage alerter nodes failing together is not covered inside MMWatchdog | A triple failure, or a double one in a fleet of two; MMHotline notices missing check-ins from outside |
| A standalone node is watched from outside by MMHotline or by a second node in a fleet of two; MMWatchdog adds no other hub-outage alerter | A machine that died cannot report its own death, and two nodes that watch each other need nothing beyond the fleet |
| A destination whose service keeps incidents, MMHotline among them, gets one incident per check across the fleet, and the hub closes it when no node still fails | One fleet-wide problem is one incident that notifies the person on call, and the service needs no knowledge of nodes |
| The closing alert is the node's own when one node's incidents closed, and a new alert about the key when several nodes' did; either carries the severities that its destination got for the key | A standalone node and a single failing node send the closing alerts each would send without fleet incident tracking. A destination never closes what another destination using the first destination's service opened |
| Fleet alert grouping within one minute keys on check group, check, change, severity, and the destinations that the check names; silence on change and severity; maintenance on reason and user | Matching alerts compress without guessing at causes, and a grouped message never mixes a page with a notice or two sets of destinations |
| Only a finding with a module-supplied exact finding joins a later node to an earlier alert; every other alert keeps grouping within one minute | One key can fail for different reasons on different nodes, such as a certificate error and an HTTP 503. Only the module knows the reason, so only an exact finding may join a later node to an earlier investigation |
| The exact finding is a separate field beside the finding's `id` | `id` stays the incident's identity, so an upgrade from one affected version to another is never reported as ok again |
| The announced severity and exact findings are written only with the outbox entry that carries them, and reset when that alert leaves the outbox undelivered | A report that arrives before its alert, or an alert that is dropped, must never hold back the first alert of a problem |
| Covered growth below `sos` goes out on one shared tick for every key, every `send_newly_affected_nodes_every` (default `6h`); `sos` growth goes each minute; a new exact finding or a higher severity goes at once | MMHotline's triage agent covers every key's growth in one run, at most four a day by default, well inside its daily limit of runs. Nothing that asks for a faster look waits |
| An update's severity is the highest severity among the nodes that it adds; only an update of recovered nodes is `info` | A `warn` node that joins an `sos` key never wakes the person on call, and a node added as `info` would never be investigated |
| A new exact finding keeps the key's routed severity | An `info` or `warn` problem must not page someone because a version changed |
| The closing alert of an exact-finding key claims recovery only for the nodes that it covered | Other nodes may still fail with a set-aside exact finding, or have alerts that go to another destination |
| One reminder schedule per destination and key with an exact finding, kept by the hub, and only while a node incident is eligible | 5,000 nodes must not send 5,000 reminders, and a node in maintenance or quiet time must not keep a reminder going |
| The hub's MMHotline alert source merges nothing; an optional second alert source for nodes sending without the hub merges repeats by key and first line | A growth warning folded into the first warning would leave the new node uninvestigated, while a hub outage on a large fleet must not use up MMHotline's hourly budget |
| The first line of every `warn` and `sos` message to MMHotline names the problem with no node name | MMHotline can merge repeats from many nodes during a hub outage; different causes keep different first lines and are never merged |
| Set-aside findings live in a hub-only file, keyed to every field of the exact finding, with every field required and no expiry | The file never reaches nodes, so older nodes and their fleet settings are unaffected. An exact match makes any change of package, release, fix state, or advisory alert again, and the weekly review lists every entry |
| `sos` is never set aside, and nodes never set anything aside | A set-aside list must never hide an emergency, and a hub outage can only add alerts, never hide one |
| Each node states the severity of its own silence, required, with no default by role | Large fleets must not page for one silent node, while a node that matters, and the hub that nothing else watches, still wake someone; the setting is visible on every node. |
| A node can state `raise_to_sos_after_this_node_is_silent_for`, which raises its silence alert to `sos` after that time | A silent node that may wait a few minutes must not stay a warning all night; a check has the same kind of setting, `raise_to_sos_after_failing_for` |
| A maintenance window holds a silent node's alert and its raise, and the time of `raise_to_sos_after_this_node_is_silent_for` keeps counting through the window | A silent node's maintenance window marks silence as expected until the window ends, without confirming recovery. If the node has already been silent for `raise_to_sos_after_this_node_is_silent_for`, the hub sends an `sos` alert when maintenance ends. A check's `raise_to_sos_after_failing_for` also keeps counting through maintenance |
| Many nodes silent at once raise to `sos` with an explicit count or percentage | One dead app server can wait; many at once is an outage someone must see now |
| A node names the node groups whose silence raises its own silence alert to `sos`, `raise_to_sos_when_every_node_of_these_node_groups_is_silent`, and may name only node groups that it is in | Two servers doing one job each warn when silent alone, but an outage occurs when the two servers are silent together. The MMX deployment role assigns node groups and defines what silence of each assigned node group means. The hub keeps no list of node groups in which to store the rule |
| The raise by node group raises only the nodes that state it, and counts a node held by a window or by maintenance as not silent, as the raise by count does | Each node's settings say what its own silence means; a window says that a node's silence is expected, so one node in maintenance and one silent node are not an outage of the node group |
| A node states its node groups as a list; a node group is a selection of nodes, not a division of the fleet, and the hub keeps no list of node groups | One machine often does two jobs, and what a node does and where it is are two questions; the fleet view still lists every node once, and a second list on the hub would drift |
| The view of one node group leaves out the hub-outage alerter nodes, the refused joins, and the line about the measurement history on the hub | The hub-outage alerter nodes, refused joins, and hub measurement history describe the whole fleet, which `mmwatchdog fleet` shows. Facts about the whole fleet would appear to describe only the selected node group in a node-group view |
| Fleet inspection runs on the hub, over SSH from elsewhere; no network inspection API or web dashboard | SSH is the existing trusted access path, and the hub opens no read interface to protect |
| Another program on the hub reads the fleet view through a socket unit of its own that runs exactly `mmwatchdog fleet --json` as root | The local socket also accepts commands that change state, such as forgetting a node. A unit running one fixed command gives the requesting program read access and nothing more, with no new interface to protect. The unit works for a service running with `NoNewPrivileges=yes`, which prevents the service from using `sudo` |
| The hub keeps the readings of every node, sent with the reports, for a time and up to a size that its settings state | Several nodes in one chart need the readings in one place; the history of a node that died is the history someone wants most; and the hub cannot ask a node for it, because the hub never connects to a node |
| The hub keeps every reading as received, without averaging old readings | A short spike that explains an outage disappears in an average, and a chart's meaning must not change with the age of its data |
| The age of a reading counts from the time of its run, or from its receipt on the hub when that is earlier; the reading keeps the time of its run | The hub removes readings by their age, so a node whose clock runs ahead could keep its readings longer than the measurement history settings on the hub say; and a reading must stay as it was measured, or every node with a fast clock would show its runs at the times of its reports |
| The hub runs without a measurement history of nodes that it cannot open, and removes many readings in small steps between reports | Reports and alerts must never wait for the history or depend on it: a removal that held up reports for long would look like silent nodes |
| One report carries at most 256 KiB of readings for each 30 seconds between two reports, although a request may be 4 MiB | Readings are for looking back, so they can arrive slowly. A hub decodes and saves a full request of readings in about half a second, and many nodes catch up at the same moment after a hub outage: with full requests, a few dozen nodes would take all of the hub's time, the reports of the other nodes would wait, and the hub would call nodes silent that are not. With 256 KiB a report costs the hub a few hundredths of a second. The amount follows `report_every`, because a fixed amount would let a node that reports every five minutes send only a tenth as much in the same time, less than a node with many checks stores |
| The hub saves a node's readings after it has saved the report and counted the node as reporting, and without the lock that guards the fleet | Whether a node is alive, and what fails on it, must never wait for the history |
| A node and a hub take no sequence number above 2^53 from each other | A node continues its own numbers after the number that the hub holds. A number near the largest integer, from a mistake or from a hub that someone else controls, would leave the node without a number for its next event; no node reaches 2^53 |
| Readings that the hub cannot save do not refuse the report that carried them | The report says whether the node is alive and what fails on it, and that must arrive whatever happens to the history; the node still has the readings and sends them again |
| The hub sends only the settings that the whole fleet must share. Those are where alerts go, the fleet timings, how loud the hub's own silence is. Nodes take them from the hub so that no node can disagree with it, and a change on the hub reaches every node without a deploy. Everything about one node lives on that node, and MMX deploys it: its checks, recovery, maintenance windows, node groups, and modules. The hub sends no code and no commands. | Nodes cannot disagree with the hub about the shared settings; a change on the hub reaches every node without a deploy; and MMX is what acts on nodes. |
| The hub does not open or end maintenance windows on other nodes. | One way per job: an MMX run or an ssh loop runs `mmwatchdog maintenance on` on each node. A second path through the hub would control maintenance windows another way. In years of running M/Monit, acting on a node from the central view was very rare. The central view served inspection. A second path costs code, tests, and documentation, and every reader must learn which path to use. |
| The hub's stores of events and of undelivered alerts are bounded by a time and a size that `fleet_hub_role` can state; the fleet timings have no upper bound; a report stays limited to 4 MiB and 10 seconds | A limit without a concrete reason is a setting. A report is input from another machine, so its two bounds protect the hub and stay fixed. |

The hub states `report_every`, `alert_after_hub_unreachable_for`,
`send_alert_without_hub_after_waiting_for`, and
`undelivered_alerts_of_nodes_content_size_limit` explicitly, because the
settings choose fleet failure timing and queued-alert retention, and an
omitted setting must not choose them.

## Beyond release 1

- Several hubs or hub high availability.
