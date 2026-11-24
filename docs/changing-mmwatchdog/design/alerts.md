# MMWatchdog: alerts

This document owns who sends an alert, what an alert event contains, the title
and the body of an alert, the destinations that receive it, which alert goes
to which destination, notices from people and other programs, durable
delivery, and reminders. Alerts exist to wake the right person with enough
evidence to act, and never to repeat noise; the
[product principles](README.md#product-principles) set that bar.

A node is a machine that MMWatchdog monitors. A standalone node works alone and
delivers its own alerts. In a fleet, nodes join a hub: the one node that shows
every node of the fleet and delivers their alerts
([fleet](fleet.md#alerts-in-a-fleet)).

## Who sends an alert

The node monitoring process that evaluates a check owns its incidents and
their failure and recovery alert events. A standalone node delivers failure
and recovery alert events to the destinations in its own
`alert_destinations.yml`. A joined node delivers failure and recovery alert
events through the hub, which groups related alerts. When the hub does not
accept the events in time, the joined node delivers the events directly.
[Alerts in a fleet](fleet.md#alerts-in-a-fleet) defines the alert delivery
route.

| Observation | Alert owner |
| --- | --- |
| Local systemd unit, resource, or custom-check failure | The node monitoring process on the node where the check runs |
| Recovery of that check | The node monitoring process on the node where the recovered check runs |
| Outside-in HTTP, TLS, or TCP failure | The MMWatchdog instance running the outside-in check |
| Expected heartbeat did not arrive | The MMWatchdog instance evaluating that [heartbeat](checks.md#heartbeats) |
| The machine rebooted | The node monitoring process on the rebooted node, on the process's first start in the new [boot](checks.md#reboots) |
| An incident is still open | The node monitoring process that owns the incident, as a [reminder](#reminders) |
| A person or another program announces an event | The MMWatchdog instance on the machine where `mmwatchdog send-notice` runs ([send a notice](#send-a-notice)) |
| A joined node stopped reporting | The hub ([silent nodes](fleet.md#silent-nodes)) |
| The hub is unreachable | Each of the two nodes the hub chose as [hub-outage alerters](fleet.md#the-hub-outage-alerter-nodes) |

Alerts follow confirmed incidents, not single observations: the
[confirmation rules](checks.md#timing-and-confirmation) decide when a failure
or recovery is real. A continuing failure produces no new failure alert on
each observation; reminders repeat it only where configured. A check can also
stay quiet while the check that is its cause fails
([one cause, one alert](checks.md#one-cause-one-alert)).

## Alert events

Every alert is one event with a common shape, whatever the destination:

| Field | Meaning |
| --- | --- |
| `id` | Stable event ID; retries and every destination use the same one |
| `incident_id` | The incident the event belongs to, when it has one |
| `fleet_incident_key` | On every alert about an incident: 32 lowercase hexadecimal digits. For one check, or one finding of a check, and incident kind, the key is the same on every node of a fleet and on a standalone node ([one incident per check across the fleet](fleet.md#one-incident-per-check-across-the-fleet)). A notice, a reboot notice, and a maintenance alert have no key |
| `node` | The monitored node's hostname |
| `check_group`, `check` | The named check group and check, such as `myapp` and `http` |
| `finding` | On an alert about one [finding](checks.md#findings) of a check, the `id` of that finding. For an [`every_systemd_unit`](../../reference/modules/every_systemd_unit.md) check, the `finding` field contains the failed unit's name, such as `backup.service` |
| `exact_finding` | On an alert about one finding whose module supplies an [exact finding](checks.md#exact-findings), the exact finding that the alert announces, such as `backup.service exit-code` |
| `exact_finding_sequence` | With `exact_finding`: the incident's sequence number of the exact finding, which rises by one with each change, so a receiver can tell the newer of two exact findings |
| `type` | The change, listed below |
| `alert_severity` | `info`, `warn`, or `sos` ([alert severity](#alert-severity)) |
| `alert_severities_sent_for_incident` | On a recovery or closure without recovery (`closed_without_recovery`), every severity the incident's alerts went out with |
| `at` | When the change happened |
| `title` | One short line that names the node, the check, and what changed ([title and body](#title-and-body)) |
| `title_was_shortened` | `true` when MMWatchdog cut the title to fit its limit; left out otherwise |
| `title_with_alert_severity` | The title with the alert's severity in front, such as `SOS: web-3: myapp/http failed`: the line for a person who reads the alert in an email or a chat ([the title with the alert's severity](#the-title-with-the-alerts-severity)) |
| `body` | Everything else a person needs to act, as readable text ([title and body](#title-and-body)) |
| `message` | A readable explanation of the observation, such as `expected 200, received 503`. For a hub's grouped alert, `message` names the nodes covered by the alert. For a [notice](#send-a-notice), `message` contains the title text that the sender gave, without the node's name |
| `evidence` | The confirming observation, for incident events |
| `maintenance` | The window's `until`, `reason`, and `user`, for maintenance events |
| `nodes`, `node_count` | The nodes a hub's grouped message covers, or whose incidents a closing alert of the fleet closes. In a `finding_nodes_changed` update, the nodes that the update adds or changes |
| `nodes_by_exact_finding` | In a `finding_nodes_changed` update: for each exact finding, `exact_finding`, up to 20 `nodes`, and `node_count` ([one alert per exact finding](fleet.md#one-alert-per-exact-finding)) |
| `recovered_nodes`, `recovered_node_count` | In a `finding_nodes_changed` update: up to 20 nodes whose incidents closed since the previous update, and their count |
| `destination` | The one destination a `reminder` is for |
| `send_alerts_only_to_destinations` | The destinations that the alert's check names, when it names its destinations ([which alert goes to whom](#which-alert-goes-to-whom)), or the one destination that an `mmwatchdog send-notice` command names with `--destination` |
| `no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails` | On a failure alert, its raise, and its reminders: the checks that MMWatchdog keeps quiet while the alert's check fails. The field lists at most 20 quiet checks, each as check group, slash, check ([one cause, one alert](checks.md#one-cause-one-alert)) |
| `number_of_checks_with_no_alerts_or_recovery_actions_while_this_check_fails` | With `no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails`: how many checks MMWatchdog keeps quiet in all |

| Type | Sent when |
| --- | --- |
| `failed`, `recovered` | A failure incident opens or resolves |
| `cannot_check`, `can_check_again` | MMWatchdog cannot observe a check for its confirmation period, or produces the first known observation again |
| `failure_alert_severity_raised`, `cannot_check_alert_severity_raised` | An announced failure or cannot-check incident is [raised](#alert-severity) to a higher severity |
| `closed_without_recovery` | A configuration change or a forgotten node closes an announced incident without claiming recovery, or the hub [sets a finding aside](fleet.md#set-a-finding-aside) |
| `exact_finding_changed` | The [exact finding](checks.md#exact-findings) of an announced, open finding changed, such as after a package moved to another affected version |
| `finding_nodes_changed` | On the hub: the nodes of a key with an exact finding changed since the last update: nodes joined, changed their exact finding to one already announced, or are ok again ([one alert per exact finding](fleet.md#one-alert-per-exact-finding)) |
| `maintenance_on`, `maintenance_off` | A [maintenance window](recovery.md#maintenance-windows) is turned on or changed, and is turned off, expires, or ends when its check group is removed |
| `rebooted` | MMWatchdog starts for the first time in a new boot; the message names the boot time and when MMWatchdog last ran |
| `reminder` | An announced incident is still open after a destination's `remind_every` |
| `notice` | A person or another program ran [`mmwatchdog send-notice`](#send-a-notice) |

A maintenance event lists the check groups selected by the event's command in
`check_groups`. For a whole-node window, `check_groups` contains only the
value `every_check_group`. When the event names one check group,
`check_group` holds that name too. A `maintenance_off` message says whether
maintenance was turned off, expired, or ended because the window's check
group was removed.

MMWatchdog sends `maintenance_off` before any alert produced by applying
check results to incident state after maintenance ends.

After a delivery gap, a destination can receive a recovery or
`closed_without_recovery` alert without having received the failure alert.
Match events by `incident_id`. A `rebooted` event and a `notice` event carry no
`check_group`.

Incident events carry `evidence` with the following fields:

- `incident_id`: the incident ID.
- `observation`: the confirming observation, including the message, time,
  available exit status, stdout/stderr, measurements, and `details`.
- `source`: the original configuration source.
- `timing`: the effective timing.

The `details` are lines that the check's module adds
when the message has no room for them, such as the five commands that use
the most memory.
The evidence of a failure incident also carries `failing_since`, the start of
the check's [failing time](checks.md#timing-and-confirmation), and, for a check
that failed on and off, `last_runs_failed` and `last_runs_counted`. The
evidence of a cannot-check incident carries `cannot_check_since`, the time
from which MMWatchdog could not run the check.
Evidence is frozen when the incident is confirmed, so retries and later
observations cannot change the evidence. Recovery, `closed_without_recovery`,
and reminder events carry the evidence captured at confirmation.
Each encoded evidence object is limited to 16 KiB. MMWatchdog shortens
stdout/stderr first, then removes details and measurements and shortens
the message. The field `truncated: true` marks any reduction.

Local bookkeeping, such as delivery gaps, cancelled deliveries, restarts, and
[recovery command](recovery.md#run-a-recovery-command) results, is recorded in
event history and shown by `mmwatchdog events`; it is not sent to destinations.

Module pages that rely on the Alert events section:
[`webhook`](../../reference/modules/webhook.md).

## Title and body

MMWatchdog writes each alert's title and body once, and every destination
gets the title and body.

- **The title** is one short line that is complete without the body. The
  title names the node, the check group and the check, and what changed.
- **The body** holds everything else a person needs to act.

The following alert is for the memory check `memory_is_below_95_percent`, in
the check group `machine_memory`, on the node `app-1`. The memory alert's
title:

```text
app-1: machine_memory/memory_is_below_95_percent failed
```

The memory alert's body:

```text
memory_used_percent is 97 (fails above 95)
failing since 14:02 UTC
the commands that use the most memory:
  3051 MiB: mysqld (process 812)
  1840 MiB: ruby (process 1934)
  212 MiB: systemd-journald (process 301)
  96 MiB: nginx (process 955)
  95 MiB: nginx (process 956)
```

Because MMWatchdog writes the title and the body, every channel uses
MMWatchdog's wording, and no destination needs wording of its own.

### The title

| Alert | Title |
| --- | --- |
| A check failed on one node | `web-3: myapp/http failed` |
| The hub grouped the same failure of several nodes | `myapp/http failed on 7 nodes` |
| MMWatchdog cannot run a check | `web-3: MMWatchdog cannot check myapp/http` |
| A failure was raised to `sos`, and an incident of a check that MMWatchdog cannot run was | `web-3: myapp/http still failing, now SOS`, `web-3: MMWatchdog still cannot check myapp/http, now SOS` |
| A check recovered on one node | `web-3: myapp/http is ok again` |
| The hub grouped the same recovery of several nodes | `myapp/http is ok again on 3 nodes` |
| MMWatchdog can run a check again | `web-3: MMWatchdog can check myapp/http again` |
| A [reminder](#reminders) of a failure, and of a check that MMWatchdog cannot run | `web-3: myapp/http still failing`, `web-3: MMWatchdog still cannot check myapp/http` |
| An incident was closed without a recovery, because its check was removed or changed | `web-3: myapp/http incident closed, maybe not fixed` |
| A joined node is silent, and its alert was raised | `app-2 stopped reporting to the MMWatchdog fleet hub`, `app-2 still not reporting to the MMWatchdog fleet hub, now SOS` |
| A silent node reports again | `app-2 reports to the MMWatchdog fleet hub again` |
| The hub-outage alerter node cannot reach the hub | `app-5 cannot reach the MMWatchdog fleet hub` |
| A maintenance window was turned on for the whole node | `web-3: maintenance on for the whole node` |
| A maintenance window was turned off for two check groups | `web-3: maintenance off for worker_1, worker_2` |
| A maintenance window was turned on for more than three check groups | `web-3: maintenance on for 5 check groups` |
| One node rebooted, and several did | `web-3 rebooted`, `3 nodes rebooted` |
| A notice | `app-1: Test of the alert destinations. Nothing is wrong.` |
| One unit of an [`every_systemd_unit`](../../reference/modules/every_systemd_unit.md) check failed, and is no longer failed | `web-3: failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service failed`, `web-3: failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service is ok again` |

A grouped message counts its nodes in every title, as the second row does:
`MMWatchdog cannot check myapp/http on 2 nodes`, `3 nodes stopped reporting to the MMWatchdog
fleet hub`, `maintenance on for 5 nodes`. A grouped recovery title counts
only the nodes covered by the grouped recovery message. A destination whose
service keeps incidents of its own, such as Pushover or
[MMHotline](../../reference/modules/mmhotline.md), learns when
the last node of a failure has recovered, and its closing alert says so:
`myapp/http is ok on every node again`
([one incident per check across the fleet](fleet.md#one-incident-per-check-across-the-fleet)).

**A title has at most 120 characters.** A title must fit an SMS and a phone
call, together with the words and the link that a notification channel
adds to it. A check group name and a check name have at most 64 characters
each, so a title with ordinary names stays far below the limit. When a title
would be longer:

- MMWatchdog shortens the longest name in the title: the node's name, check
  group's name, or check's name. MMWatchdog marks the cut with `…`.
  If the title is still too long, MMWatchdog shortens the name that is then the longest. MMWatchdog leaves at least
  12 characters of a name, with the mark, so a name stays recognizable.
- MMWatchdog never cuts the last words, which say what changed.
- The event carries `title_was_shortened: true`. The body and the event's
  `node`, `check_group`, and `check` fields keep every name in full.
- `mmwatchdog validate` names every check whose title would exceed 120
  characters on the node. The person who chose the names therefore learns
  of the cut before any alert goes out. The `mmwatchdog validate` command counts the characters in the check's
  longest possible title: a cannot-check incident raised to `sos`. An example:
  `web-3: MMWatchdog still cannot check myapp/http, now
  SOS`. Validation prints the title as MMWatchdog would send the title after
  shortening. The configuration stays valid.

The following failure title for `db-replica-03.frankfurt.example.com` would
have 147 characters:

```text
db-replica-03.frankfurt.example.com: nightly_backup_of_the_customer_database/the_restored_copy_of_last_night_has_as_many_rows_as_the_primary failed
```

The check's name is the longest name, so MMWatchdog cuts there and sends 120
characters:

```text
db-replica-03.frankfurt.example.com: nightly_backup_of_the_customer_database/the_restored_copy_of_last_night_has… failed
```

### The title with the alert's severity

A team may send every severity to one chat channel or one mailbox. A person
reading an alert in the chat channel or mailbox must see the urgency from
the alert's first words. So MMWatchdog writes the title a second time, with
a label in front, as the event's `title_with_alert_severity`:

```text
SOS: web-3: myapp/http failed
Resolved SOS: web-3: myapp/http is ok again
Info: web-3: maintenance on for the whole node
Warning: web-3 rebooted
```

| `alert_severity` | Label | Label of an alert that resolves an incident | Label of an alert that closes an incident without recovery |
| --- | --- | --- | --- |
| `info` | `Info` | `Resolved info` | `Closed info` |
| `warn` | `Warning` | `Resolved warning` | `Closed warning` |
| `sos` | `SOS` | `Resolved SOS` | `Closed SOS` |

- An alert resolves an incident when its type is `recovered` or
  `can_check_again`. An alert of the type `closed_without_recovery` closes an
  incident because its check was changed or removed or its node was
  forgotten, and says `Closed`: MMWatchdog does not know whether the problem
  is fixed. The `Resolved` and `Closed` labels name the highest severity
  that the incident reached. So the recovery of an incident raised to
  `sos` reads `Resolved SOS`.
- Every other alert has the plain label of its severity: a failure, a raise,
  a reminder, a maintenance alert, a reboot notice, and a
  [notice](#send-a-notice).
- The severity labels are the labels that MMHotline shows for an alert,
  so an alert reads the same in email, chat, and MMHotline. MMHotline receives a
  `closed_without_recovery` alert as a resolution, such as `resolved-sos`,
  and shows `Resolved SOS` before the title. The title still says the
  incident is closed and not known to be fixed. The labels use words
  without symbols, so every channel can show the labels.
- MMWatchdog writes `title_with_alert_severity`, and no module adds a label
  of its own.
- The label is not counted in the 120 characters of the title. The line with
  the label has at most 138 characters.
- The bare `title` stays in the event, for a receiver that shows the severity
  in its own way, such as MMHotline.

### The body

The body is readable text that MMWatchdog builds from the event's `message` and
`evidence`:

- the message of the confirming observation, such as a reading and its limit,
  or `expected 200, received 503`;
- since when the check has been failing, in UTC, and, for a check that failed
  on and off, how many of its last runs failed
  ([timing and confirmation](checks.md#timing-and-confirmation));
- the details that the check's module adds to its observation: for a
  machine check of memory, CPU, load, or swap, the five commands that use
  the most of that resource
  ([`node_resource`](../../reference/modules/node_resource.md#the-commands-that-use-the-most-of-a-resource)),
  and for a check of one program of a unit,
  the commands that the unit runs
  ([`systemd_unit_commands`](../../reference/modules/systemd_unit_commands.md));
- for a check that keeps other checks quiet while it fails, the names of
  those checks, as the line `no alerts and no recovery actions for these
  checks while this check fails: myapp/myapp_answers_health_request`
  ([one cause, one alert](checks.md#one-cause-one-alert));
- for a grouped message or an update from the hub, the nodes covered by the
  message ([fleet alert grouping](fleet.md#fleet-alert-grouping)). Below the node
  list, MMWatchdog shows the message, time, and evidence of the grouped
  message's first node, under `from app-1:`;
- a short excerpt of the evidence: the exit status and the first five lines
  of stdout and stderr.

A time is written as `14:02 UTC`, and with its date, `2026-09-29 14:02 UTC`,
when it is not on the day of the alert. For a check that MMWatchdog cannot run, the
body says `MMWatchdog has not been able to run the check since 14:02 UTC`. A
recovery's body is its message. The body of a [notice](#send-a-notice) is
the text that its sender gave with `--body`; without `--body` the notice has
no body. When a hub delivers a check alert late, the hub adds a line
stating the delay and the present state
([fleet alert grouping](fleet.md#fleet-alert-grouping)).

The `message` and `evidence` fields stay in the event, for a webhook receiver
or a module that wants the parts and not the finished text.

### What each destination sends

| Destination module | Sends |
| --- | --- |
| [`webhook`](../../reference/modules/webhook.md) | The whole event, with the title, the title with the alert's severity in front, and the body among its fields |
| [`email`](../../reference/modules/email.md) | The title with the alert's severity in front as the subject. The email's message text contains the body followed by the alert time and the alert's IDs |
| An [official extra module or a module of the project's own](#destination-modules) | What suits the module's channel: a module for chat sends the title with the alert's severity in front and the body, and a module for a short channel, such as SMS, sends that first line alone |
| [`system_log`](../../reference/modules/system_log.md) | One line with the title and the alert's severity in front |
| [`mmhotline`](../../reference/modules/mmhotline.md) | The title, without a label, as MMHotline's title and the body as MMHotline's message; MMHotline puts its own label in front, uses the title in notifications, and shows the body on the alert's page |

**A destination can ask for the title only.** A module knows its channel,
but some destinations cannot know that the receiver uses a short channel.
For example, a gateway can turn each email into an SMS, but MMWatchdog sees
an ordinary email address.
The email-to-SMS destination states
`send_only_the_alert_title: true`:

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  team_chat:
    remind_every: never
    slack:                                        # the official extra module for Slack
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]           # the team chat gets the title and the body
  on_call_sms:
    remind_every: never
    email:
      smtp_server: smtp.example.com:587
      smtp_connection_security: starttls
      from: mmwatchdog@example.com
      to: [15551234567@sms-gateway.example.com]   # a gateway that turns each email into an SMS
      login_user: mmwatchdog@example.com
      login_password_file: /etc/mmwatchdog/secrets/smtp
    send_only_the_alert_title: true               # this destination gets the title and no body
    alert_severities: [sos]
```

When the memory alert above goes out with severity `sos`, `team_chat` shows
the title with `SOS:` in front and, below it, the eight lines of the body.
`on_call_sms` gets an email containing only the title with `SOS:` in front, so
the SMS reads `SOS: app-1: machine_memory/memory_is_below_95_percent failed`.
With `send_only_the_alert_title: true`, the alert that MMWatchdog gives the
destination has no `body`, no `message`, and no `evidence`; `title`,
`title_with_alert_severity`, and the fields that identify the alert stay. An `mmhotline` destination rejects
`send_only_the_alert_title: true` as a validation error: MMHotline puts the title in its notifications
and shows the body on the alert's page. A destination asks for the title, never for a number of
characters: a length limit would cut the text in the middle, and the title is
written to be complete in one line.

## Alert severity

Every alert carries an `alert_severity`: `info`, `warn`, or `sos`, the words
MMHotline uses. `sos` means wake someone. `warn` means investigate the
monitored problem soon. `info` is for the record.
Severity decides where an alert goes, unless its
check names its destinations
([which alert goes to whom](#which-alert-goes-to-whom)): each destination
states the severities it takes, so `sos` can go to a pager and `info` to
email, with or without MMHotline.

- A check's alerts take the `alert_severity` written on the check, which
  every check that can alert must state; there is no default
  ([alert severity of a check](checks.md#alert-severity)).
- A cannot-check incident takes the check's current severity, including a
  raise of its open failure. If an `sos` check cannot run, MMWatchdog must
  alert at `sos` because MMWatchdog cannot observe an important monitored
  problem.
- A joined node's silence alert takes that node's
  `alert_severity_when_this_node_goes_silent`
  ([silent nodes](fleet.md#silent-nodes)).
- MMWatchdog's maintenance notices, `maintenance_on` and `maintenance_off`, are
  `info`.
- A `rebooted` notice takes the node's required
  `alert_severity_when_this_node_has_rebooted`
  ([reboots](checks.md#reboots)).
- A notice sent with [`mmwatchdog send-notice`](#send-a-notice) takes the severity
  that its command states.

**Raised, never lowered.** An open incident's severity can rise and never
falls back until the incident ends. Three things raise an open incident's
severity:

- time on a check: a check that has been failing for its
  [`raise_to_sos_after_failing_for`](checks.md#alert-severity) is raised to
  `sos`, and so is a check that MMWatchdog has not been able to run for that long;
- silence duration: the hub measures a joined node's silence and uses the
  joined node's
  [`raise_to_sos_after_this_node_is_silent_for`](fleet.md#silent-nodes).
  A [hub-outage alerter node](fleet.md#the-hub-outage-alerter-nodes) measures the time
  since the hub-outage alerter's last report reached the hub. The hub-outage alerter uses the
  hub's `raise_to_sos_after_this_node_is_silent_for`.
- on the hub, the number of nodes silent at once:
  [count or percentage of silent nodes](fleet.md#silent-nodes).

Escalation by level is not a raise: it is a second check with a higher limit
and a higher severity ([alert severity of a check](checks.md#alert-severity)).

Raising an announced incident sends a new alert for the same incident:
`failure_alert_severity_raised` or `cannot_check_alert_severity_raised`, with
the same `incident_id`, the higher `alert_severity`, and a message such as
`myapp/http has been failing for 30m, so this alert is now SOS
(raise_to_sos_after_failing_for: 30m)`, or for a cannot-check incident
`MMWatchdog has not been able to run myapp/http for 30m, so this alert is now
SOS (raise_to_sos_after_failing_for: 30m)`. The title of a raise ends with
the new severity, such as `web-3: myapp/http still failing, now SOS`. A destination that takes only `sos`
therefore gets the raise as its first message about the incident. An incident
raised before it was announced, such as one that began during a maintenance
window, is announced at its raised severity. During a maintenance window MMWatchdog
sends no raise for an announced incident; the raise goes out when the window
ends, if the incident is still open, and reminders wait in the same way.
Raising the settings in a reload raises open incidents to match. Lowering
the settings never lowers an open incident.

**A recovery goes where the incident went.**
`recovered`, `can_check_again`, and `closed_without_recovery` carry the
highest severity the incident reached as `alert_severity`.
The closing alerts carry every severity the incident's alerts went out with as
`alert_severities_sent_for_incident`, such as `["warn", "sos"]`.
The sender records which destinations each open incident's failure and
raises went to.
With a recipient record, MMWatchdog sends the closing alert to exactly the
recorded destinations, regardless of the destinations' current
`alert_severities`. For example, a destination selected for the `warn`
failure or only the `sos` raise hears that the incident ended.
A destination selected for neither the `warn` failure nor the `sos` raise
receives no closing alert, even if `alert_severities` changed during the
incident.

Where no recipient record exists, such as for a failure a joined node
delivered directly while the hub was down, the recovery goes to destinations
that take any severity in `alert_severities_sent_for_incident`.
An open incident's recipient record has no age limit.
MMWatchdog removes the recipient record only when the incident closes.

A [reminder](#reminders) carries the incident's current severity, and fleet
alert grouping never puts alerts of different severities in one message
([fleet alert grouping](fleet.md#fleet-alert-grouping)).

## Destinations

A destination is one place that MMWatchdog delivers alerts to: a chat channel, a
mailbox, a pager, or a program that the team wrote. Alert destinations live in
`alert_destinations.yml` in the configuration directory (default `/etc/mmwatchdog`),
beside `mmwatchdog_node_settings.yml` and the `check_groups/` directory, on a
standalone node or on the hub. Every command that reads configuration uses
`--config-dir DIR` to select the complete set. On a node with
`join_fleet_hub`, `alert_destinations.yml` must not exist: its presence is a
validation error, because the node gets destinations from the hub. MMX
distributes `alert_destinations.yml` with the remaining configuration.
MMWatchdog validates and activates `alert_destinations.yml` together with
the remaining configuration
([activation](operation.md#activate-a-configuration)).

Each named destination names exactly one [module](checks.md#modules), by the
module's name, and writes the module's inputs below that name, as a check
does:

| Module | Kind of module | Delivers to |
| --- | --- | --- |
| [`webhook`](../../reference/modules/webhook.md) | Core | Any receiver that reads MMWatchdog's alert event over HTTP |
| [`email`](../../reference/modules/email.md) | Core | Mailboxes, through a mail server |
| [`system_log`](../../reference/modules/system_log.md) | Core | The systemd journal on the machine delivering the alert |
| [`mmhotline`](../../reference/modules/mmhotline.md) | Core | MMHotline, the on-call program that a team runs on its own server |
| [`discord`](../../../official_extra_modules/discord/README.md), [`mattermost`](../../../official_extra_modules/mattermost/README.md), [`pushover`](../../../official_extra_modules/pushover/README.md), [`slack`](../../../official_extra_modules/slack/README.md), [`telegram`](../../../official_extra_modules/telegram/README.md) ([destination modules](#destination-modules)) | Official extra | The service named by the module |
| [A module of the project's own](#destination-modules) | The project's own | A team's own alarm system |

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  team_chat:
    remind_every: never
    slack:                                        # the official extra module for Slack
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]           # the team chat gets every alert
  on_call_pager:
    remind_every: never
    pushover:                                    # the official extra module for Pushover
      application_token_file: /etc/mmwatchdog/secrets/pushover-application-token
      user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-on-call-group-key
      repeat_sos_alerts_every: 5m
      stop_repeating_sos_alerts_after: 1h
    alert_severities: [sos]                       # the pager gets only sos alerts
  operations_mailbox:
    email:
      smtp_server: localhost:25                   # the mail server on this same machine
      smtp_connection_security: unencrypted
      from: mmwatchdog@app-1.example.com
      to: [operations@example.com]
    remind_every: 4h
    alert_severities: [warn, sos]
  machine_log:
    remind_every: never
    system_log: {}                                # one line in the system log for each alert
    alert_severities: [info, warn, sos]
  mail_administrator_chat:
    remind_every: never
    slack:                                        # the same module, another channel
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-mail-administrator
      mention: "<@U024BE7LH>"
    alert_severities: []                          # only checks that name this destination reach it
```

In the example, `team_chat` is a Slack channel that gets every alert, and
`on_call_pager` is Pushover, which gets only `sos` alerts.
`operations_mailbox` gets `warn` and `sos` alerts by email, through the mail
server on the machine delivering the alert. The mailbox hears every four
hours of an incident that is still open.
`machine_log` writes every alert to the system log. `mail_administrator_chat`
is a second Slack channel that no alert reaches by severity; it gets only the
alerts of the checks that name it.

Beside its module, a destination takes:

| Setting | Meaning |
| --- | --- |
| `alert_severities` | Required. The list of severities the destination takes from checks that name no destinations, such as `[sos]` or `[info, warn, sos]`. `[]` is valid: only checks that name the destination reach the destination ([which alert goes to whom](#which-alert-goes-to-whom)) |
| `time_limit_per_delivery_attempt` | The limit for one delivery attempt, default `5s`, and `30s` for an [`email`](../../reference/modules/email.md) destination |
| `remind_every` | Omitting `remind_every` is a validation error except on `mmhotline`. A positive duration or `never`. How often the destination hears again of an incident that is still open ([reminders](#reminders)) |
| `send_only_the_alert_title` | Optional. `true` gives the destination the title and no body ([title and body](#title-and-body)) |

Destination names follow the check-name rules. MMWatchdog reserves `hub`
for the fleet route.

Credentials live only in files, never in the configuration or in a URL
written in the configuration. A standalone node or hub reads credential
files for every delivery, so rotating a secret without changing the
credential file's path needs no reload on a standalone node. To supply fleet
settings, a hub also reads credential files when the hub starts and reloads.
The hub sends the credential contents to joined nodes, so reload the hub
after rotating a secret
([alerts in a fleet](fleet.md#alerts-in-a-fleet)). `mmwatchdog validate` checks that
every credential file a destination names is readable: the files of a
`webhook`, `email`, or `mmhotline` destination, and the file of each input
of a [destination module](#destination-modules) whose name ends in `_file`
([validation](operation.md#configuration-files-and-validation)). On a
joined node the fleet settings carry the credentials and the node keeps them in
its private state directory.

Secret files obey the
[credential permission rule](operation.md#credential-file-permissions).
Full validation and reload refuse exposed secrets. Startup logs each permission problem once and starts.

Runtime readers continue reading secrets after a file's mode changes.
Refusing a secret while MMWatchdog is running would drop alerts and check-ins.
The heartbeat sender warns on standard error and still sends the heartbeat.

Email and webhook connections verify server certificates by default.
`trusted_certificate_authority_file` replaces the system's authorities;
`dangerously_skip_server_certificate_check` explicitly disables identity
checks. Private authorities and protected networks need an explicit trust
choice. Email accepts `smtp_connection_security: unencrypted` with a login
to any host, since a private network can protect the email connection.

Email sends to accepted recipients and reports every refused address.
Retries send to every configured email recipient again, with the original
alert event's ID. A duplicate is preferable to losing an alert for healthy
mailboxes. Quiet email verification performs the SMTP conversation through
every recipient, then sends `QUIT` before `DATA`, so the server gets no
message to deliver.

Webhook verification only connects and checks the webhook's certificate
policy. Webhook verification sends no HTTP request and cannot verify the
path or token, since no standard webhook request is known to be free of
side effects. The webhook verification result states
that the webhook's path and token were not verified.

Sent bearer tokens have 1 to 4,096 bytes on one line: the receiving service
owns the token's format and strength. Heartbeat tokens retain the minimum
length because MMWatchdog authenticates incoming heartbeat requests
([heartbeats](checks.md#heartbeats)).

The page of each module says what the module sends, how a delivery fails,
and why it works that way.

### Destination modules

A destination module is a [module](checks.md#modules) that delivers alerts
to one service. A destination names the destination module like any module,
and MMWatchdog runs the destination module once for each alert that the
destination gets:

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  team_chat:
    remind_every: never
    slack:                                        # the module that delivers to Slack
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]
  on_call_phone:
    remind_every: never
    telegram:                                     # the module that delivers to Telegram
      bot_token_file: /etc/mmwatchdog/secrets/telegram-bot
      chat_id: "-1001234567890"
    alert_severities: [sos]
```

The mapping below a module's name holds the module's inputs, which the
[module description](checks.md#the-module-description) lists. What happens
when MMWatchdog has an `sos` alert, in order, on the standalone node or hub
that holds `/etc/mmwatchdog/alert_destinations.yml`:

1. `team_chat` and `on_call_phone` take `sos`, so MMWatchdog queues a
   delivery for each destination.
2. For `team_chat`, MMWatchdog reads the file that `webhook_url_file` names.
   MMWatchdog then runs `slack` with the alert event, `team_chat`'s inputs,
   and the contents of `/etc/mmwatchdog/secrets/slack-webhook-team`.
3. The `slack` module takes the webhook address from
   `/etc/mmwatchdog/secrets/slack-webhook-team`'s contents. The module posts
   the alert to Slack with the primitive `http_request` and answers
   "delivered".
4. For `on_call_phone`, MMWatchdog reads the file that `bot_token_file`
   names. MMWatchdog then runs `telegram` with the alert event,
   `on_call_phone`'s inputs, and the contents of
   `/etc/mmwatchdog/secrets/telegram-bot`. The module posts the alert to
   Telegram and answers "delivered".

One run of a module is one delivery attempt, under the destination's
`time_limit_per_delivery_attempt`. In a script, a primitive call that gets
no answer ends shortly before the destination's
`time_limit_per_delivery_attempt`. So when a service hangs, MMWatchdog
receives the script's own answer "failed" with the script's message
([the primitives](../../reference/writing-modules.md#primitives)).

A module that fails, runs past the time limit, or ends without the answer
"delivered" is a failed attempt. The [outbox](#delivery) retries the failed
attempt. The outbox schedules retries,
preserves delivery order, and records dropped deliveries. A module only
delivers.

A destination module is a Starlark script that defines
`deliver_alert`, or an executable
([the project's own modules](checks.md#the-projects-own-modules)).
The following example is a whole module for a team's own alarm system.
The team's alarm system takes alerts as JSON over HTTP and asks for a token.

```python
# mmwatchdog_external_modules/alarm_system/module.star
def deliver_alert(alert, inputs, file_contents):
    response = http_request(
        method = "POST",
        url = inputs["alarm_system_url"],
        headers = {
            "Content-Type": "application/json",
            "Authorization": "Bearer " + file_contents["api_token_file"],
        },
        body = json.encode({
            "title": alert["title"],
            "alert_severity": alert["alert_severity"],
            "body": alert.get("body", ""),
        }),
    )
    if response.error:
        return {"status": "failed", "message": "MMWatchdog could not reach the alarm system: " + response.error}
    if response.http_status_code // 100 != 2:
        return {"status": "failed", "message": "the alarm system answered HTTP %d" % response.http_status_code}
    return {"status": "delivered", "message": "the alarm system accepted the alert"}
```

MMWatchdog calls `deliver_alert` with three dictionaries:

- `alert` is the whole [alert event](#alert-events). `title` and
  `title_with_alert_severity` are always in the `alert` dictionary.
  `body`, `message`, and `evidence` are missing for a destination that states
  `send_only_the_alert_title: true`. `incident_id` and `fleet_incident_key`
  are missing from alerts without an incident, such as notices.
  A closing alert about a key across the fleet has `fleet_incident_key`
  but no `incident_id`.
- `inputs` are the destination's inputs as written.
- `file_contents` holds, under the input's name, what MMWatchdog read from each
  input whose name ends in `_file`: the file's text without the line break at
  its end, which a text editor adds and no credential has.

The function returns a dictionary with a `status` and a `message`:

- `status` is `delivered` or `failed`, and `message` is required; for
  `failed` it says why, such as `Slack answered 404: no_service. The address of the webhook does not work any more. Make a new incoming webhook in Slack and write its address into the file that webhook_url_file names.`.
- Anything else is a failed attempt: a script that ends with an error, a run
  past the time limit, or another result, also the answer `delivered`
  without a `message`.
- A failed attempt is retried like any failed delivery, and `mmwatchdog status` shows
  the destination with the module's `message`, in a line such as
  `Destination team_chat: 3 alerts are not delivered. MMWatchdog tries again at 14:05
  UTC. The latest attempt failed: the module slack reported: Slack answered 404: no_service. The address of the webhook does not work any more. Make a new incoming webhook in Slack and write its address into the file that webhook_url_file names.`
  ([status and diagnosis](operation.md#status-and-diagnosis)). Only a valid
  `delivered` result stops retries. No failure result stops retries.
  A module that said "do not try again" by mistake would lose an alert.
  A repeated attempt costs almost nothing, and the core destination modules try
  again after an error too.

An executable gets `alert`, `inputs`, and `file_contents` as one JSON
object on the executable's standard input. The JSON object also holds
`mmwatchdog_ends_the_module_after_milliseconds`, the time until MMWatchdog
ends the executable at `time_limit_per_delivery_attempt`.
The destination executable writes the answer as a single JSON object to
the executable's standard output, with exit status 0
([the JSON protocol](checks.md#executables-and-the-json-protocol)):

```json
{"protocol": 1,
 "alert": {"title": "app-1: myapp/myapp_answers_health_request failed",
           "title_with_alert_severity": "SOS: app-1: myapp/myapp_answers_health_request failed",
           "body": "expected 200, received 503\nfailing since 14:02 UTC", "alert_severity": "sos"},
 "inputs": {"alarm_system_url": "https://alarm.example.com/alerts", "api_token_file": "/etc/mmwatchdog/secrets/alarm-system-token"},
 "file_contents": {"api_token_file": "the-token-in-that-file"},
 "mmwatchdog_ends_the_module_after_milliseconds": 4987}
```

```json
{"protocol": 1, "status": "delivered", "message": "the alarm system accepted the alert"}
```

The example shows four fields of the alert event. For an executable, a
nonzero exit status or output outside the response schema is a failed
attempt. For a nonzero exit status, the error of the attempt
holds the first line that the executable wrote to its standard error.

The request is at most 1 MiB, which is also the most that the
[JSON helper](checks.md#json-helper-for-bash-executables) reads. Three
limits keep the largest alert, file contents, and destination inputs
within the size of the JSON request sent to the executable:

- Each file holds UTF-8 text of at most 16 KiB.
- The files of one destination hold at most 64 KiB together. JSON can write
  one byte of a text as six, so the files take at most 384 KiB of a request,
  and the largest alert takes less than 256 KiB.
- The inputs of one destination are at most 256 KiB as JSON. `mmwatchdog validate`
  refuses destination inputs whose JSON encoding exceeds 256 KiB.
- `mmwatchdog validate` refuses a destination whose files hold more than
  64 KiB together. So oversized files are found before a deploy, rather
  than at the first alert. A delivery attempt with files exceeding the
  combined file limit fails before MMWatchdog starts the module.

Rules for every destination module:

- **Verification must not deliver an alert or incur a bill.** Official extra
  destinations define `verify_destination(inputs, file_contents)`. Each
  module's page specifies a request that sends no alert and incurs no bill,
  and the responses known to mean a credential refusal.

  Connection and certificate failures and server errors fail verification.
  Unconfirmed responses pass to avoid false alerts. A service without a request
  known to send no alert and incur no bill gets only a connection and
  certificate check through
  `verify_connection(url)`. [Writing modules reference](../../reference/writing-modules.md#check-a-destination-without-a-message)
  owns the function contract.
- **Credentials are files, and MMWatchdog reads them.** An input names the path of a
  credential file, never the credential. An input whose name ends in `_file`
  names a file: MMWatchdog reads the file for every delivery and gives the module
  the file's content. The module never opens the file, so a credential is
  never in the configuration, on a command line, or in the environment.
- **A credential stays out of an error.** A module must never repeat a file's
  content in its `message` or on its standard error. If a module repeats file
  content, MMWatchdog writes `[content of webhook_url_file]` in place of the
  content before the text
  reaches `mmwatchdog status`, `mmwatchdog send-notice`, or a report to the hub. Every
  nonempty content is hidden, including a short credential, because length
  does not prove that a value is public. The primitive `http_request` never writes a
  web address into an error, because the address can be the secret.
- **The account.** A module runs in a separate
  process, as the unprivileged `mmwatchdog` account.
  The destination can override the account with `run_as_user_account`
  beside the module's inputs, as a check's module does
  ([how a module that is not core runs](checks.md#how-a-module-that-is-not-core-runs)).
  The module needs no access to the credential files, because MMWatchdog reads them.
- **In a fleet, the hub sends destination settings to joined nodes.**
  The fleet settings carry every destination to the joined nodes, including
  destinations with modules that are not core. The fleet settings contain
  each destination's module name, inputs, and credential file contents
  ([alerts in a fleet](fleet.md#alerts-in-a-fleet)). A
  joined node that delivers an alert without the hub runs the module with
  the module name, inputs, and credential contents received from the hub.
  The module's code does not come from the hub: every deploy installs the
  module's code on every node.
- **Recoveries.** A module for a service that keeps incidents of its own
  states `service_keeps_incidents: true` in the module description.
  The module opens an incident in the external service for a `failed`
  alert. The module closes the external incident for the alert that closes
  the MMWatchdog incident. The module matches the opening and closing
  alerts by `fleet_incident_key`, so the module keeps no state between
  deliveries: `pushover` stops the repeats of an `sos` alert.

  MMWatchdog sends one closing alert for each key to a destination whose
  module declares `service_keeps_incidents: true`. The closing alert follows
  closure of the last MMWatchdog incident for the key whose alert went to
  the destination, with or without recovery. So a check that fails on several
  nodes is a single incident in the external service.
  The external service also keeps a single incident when the
  [fleet hub](fleet.md#fleet-alert-grouping) groups failures into a single
  message ([one incident per check across the fleet](fleet.md#one-incident-per-check-across-the-fleet)).
- **No state.** A destination module keeps nothing between two deliveries:
  the outbox owns the retries, the order, and the gaps.

**Official extra modules.** The MMWatchdog project writes one module for each of
these services: [`discord`](../../../official_extra_modules/discord/README.md),
[`mattermost`](../../../official_extra_modules/mattermost/README.md),
[`pushover`](../../../official_extra_modules/pushover/README.md),
[`slack`](../../../official_extra_modules/slack/README.md), and
[`telegram`](../../../official_extra_modules/telegram/README.md).
Each official extra destination module is a Starlark script.
Every deploy installs all official extra modules of the release on every
node, so a destination uses an official extra module by naming the module.
Nothing is downloaded when an alert goes out
([official extra modules](checks.md#official-extra-modules)). The official
extra destination modules send with the primitive `http_request`.
A node needs nothing but MMWatchdog to deliver through official extra
destination modules. The page of each module says what it sends and where it is
tested.

**A team's own module.** A team that has its own alarm system writes its own
module, as the example above shows. MMWatchdog runs a team's own module
through the destination-module interface used for official extra modules.
A team's own module also lets the team choose the alert's wording.
The module receives the whole event and can write any text from the event's
fields. MMWatchdog has no message templates.

### No destination

MMWatchdog can run with no destination at all. MMWatchdog then runs its checks, restarts
units, runs recovery commands, and records every event, and tells nobody. The
person who wants MMWatchdog to run without alert destinations explicitly
writes `destinations: {}` in `/etc/mmwatchdog/alert_destinations.yml`:

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations: {}
```

- A missing `alert_destinations.yml` on a standalone node or on the hub is an
  error. The error message shows both choices: add a destination, or write
  `destinations: {}`. So a person who forgot the destinations finds out, and a
  person who wants only the restarts writes one line.
- A file without a `destinations:` mapping, such as an empty file, is an
  error too.
- `mmwatchdog validate` and `mmwatchdog status` print `This node sends alerts to nobody.`,
  and status JSON has `sends_alerts_to_nobody: true`.
- A node with `join_fleet_hub` must have no `alert_destinations.yml`, because
  the node's alerts go through the hub's destinations.

## Which alert goes to whom

Two rules decide which destinations get an alert.

**A check that names no destinations goes by severity.** Each destination
gets the alerts whose severity is in its `alert_severities`. Most checks
route alerts by severity: the check states the urgency, and the
destinations file states who hears of each severity.

**A check that names its destinations goes to exactly those.** The optional
setting `send_alerts_only_to_destinations` on a check lists destinations by
name. Every alert of a check with `send_alerts_only_to_destinations` goes to
the listed destinations and to no other, whatever the listed destinations'
`alert_severities` say. The list controls the failure, a raise, a
cannot-check alert, and every reminder. Recoveries and
`closed_without_recovery` follow the incident's recorded recipients,
not the current named list, as [alert severity](#alert-severity) describes.
A listed destination sends reminders at its own `remind_every`.

The mail-server example below has two checks that belong to one
administrator. Alerts from `postfix_service_is_running` and
`mail_certificate_is_valid_for_21_more_days` go to the mail administrator's
own chat channel and stay out of the team's channel:

```yaml
# /etc/mmwatchdog/check_groups/mail_server.yml
check_groups:
  mail_server:
    check_every: 1m
    time_limit_per_run: 5s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 1m
    checks:
      postfix_service_is_running:
        systemd_unit_state:
          systemd_unit: postfix.service
          fail_when: the_unit_is_not_active
        alert_severity: warn
        send_alerts_only_to_destinations: [mail_administrator_chat]
      mail_certificate_is_valid_for_21_more_days:
        tls:
          address: mail.example.com:465
          server_name: mail.example.com
          fail_when_certificate_expires_within: 21d
        alert_severity: warn
        send_alerts_only_to_destinations: [mail_administrator_chat]
```

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  team_chat:
    remind_every: never
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]           # every alert of a check that names no destinations
  on_call_pager:
    remind_every: never
    pushover:
      application_token_file: /etc/mmwatchdog/secrets/pushover-application-token
      user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-on-call-group-key
      repeat_sos_alerts_every: 5m
      stop_repeating_sos_alerts_after: 1h
    alert_severities: [sos]                       # the sos alerts of a check that names no destinations
  mail_administrator_chat:
    remind_every: never
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-mail-administrator
      mention: "<@U024BE7LH>"                     # the module puts this Slack mention into every message
    alert_severities: []                          # only checks that name this destination reach it
```

What happens, in order:

1. At 03:10 the unit `postfix.service` crashes. The check group has no
   restart, so MMWatchdog only watches.
2. At 03:15 `postfix_service_is_running` has been failing for five minutes.
   MMWatchdog sends the `warn` alert to `mail_administrator_chat`, with the
   mention. `team_chat` gets
   nothing, although `team_chat` takes `warn`.
3. At 03:40 an operator starts `postfix.service`.
4. At 03:41 `postfix_service_is_running` has been healthy for one minute.
   MMWatchdog sends the recovery to `mail_administrator_chat` only.

`send_alerts_only_to_destinations` also controls delivery to the team,
destinations for named checks only, silence, per-check reminder intervals,
and mentions. No separate setting is needed for each use:

- **The team as well.** The check names `team_chat` and
  `mail_administrator_chat`:
  `send_alerts_only_to_destinations: [team_chat, mail_administrator_chat]`.
- **A destination only for named checks.** The destination states
  `alert_severities: []`, as `mail_administrator_chat` does, so no check
  reaches it by severity.
- **A silent check.** The check names nobody:
  `send_alerts_only_to_destinations: []`. MMWatchdog still restarts the
  unit, runs the recovery command, and records every event in
  `mmwatchdog events`.
  MMWatchdog sends no new alert for the check with
  `send_alerts_only_to_destinations: []`.
  A closing alert still follows an announced incident's recipients.
- **A reminder interval for one check.** The check names a destination that
  has the required reminder interval in `remind_every`
  ([reminders](#reminders)).
- **A mention.** A person is mentioned through an input of the module on the
  destination, as `mention` above.

More rules:

- **The severity still counts.** A check that names its destinations still
  states its `alert_severity`. The severity is in every alert, `mmwatchdog status`
  shows it, and an `mmhotline` destination opens an MMHotline incident only
  for `sos`.
- **Alerts that belong to no check go by severity.** `maintenance_on`,
  `maintenance_off`, `rebooted`, and a notice belong to a check group or to
  the node, so the setting of a check does not apply to them. A notice whose
  command names one destination with `--destination` goes to that
  destination only ([send a notice](#send-a-notice)).
- **No destination is added at a raise.** A check that names its destinations
  cannot add the pager only when its alert is raised to `sos`, because the
  named destinations get every alert of the check. Two checks with two
  severities send to the chat first and add the pager later. The `warn`
  check names the chat, and the `sos` check, with a longer
  `alert_after_failing_for`, names the chat and the pager.
- **A wrong name never loses an alert.** On a standalone node and on the hub,
  a name that is not in `alert_destinations.yml` is a validation error. A
  node that has joined a hub uses the hub's destinations and knows their
  names from the fleet settings. Validation warns about names missing from
  the joined node's saved copy of the fleet settings. Validation includes
  the missing names in `checks_with_unknown_alert_destinations`.
  At run time MMWatchdog keeps every known name in a mixed list. Only a nonempty list
  with no known name goes by severity. Status also names the missing
  destinations as `alert_destinations_the_hub_does_not_have`.
- **A check that only records its readings names nobody.** A check that only
  records readings never alerts. Setting `send_alerts_only_to_destinations`
  or `alert_severity` on a check that only records readings is a validation
  error.
- **Where `send_alerts_only_to_destinations` is written.** A named check states
  `send_alerts_only_to_destinations` beside `alert_severity`. A direct check
  and a heartbeat state `send_alerts_only_to_destinations` on the check group.
  The destination list is no part of a check's identity. Changing the list
  keeps an open incident, and the next alert follows the new list.
- **In a fleet, the hub delivers.** The alert that a joined node sends through
  the hub carries the destination names of its check, as
  `send_alerts_only_to_destinations`, and the hub delivers the alert to
  exactly those destinations
  ([alerts in a fleet](fleet.md#alerts-in-a-fleet)). The hub never puts
  alerts of checks that name different destinations into one grouped
  message.

## Send a notice

A notice is a one-time message that a person or another program sends through
MMWatchdog's destinations. A notice serves two needs:

- **A test.** A notice goes the same way as a real alert, so it proves that
  the mail server, the chat channel, or the pager really gets MMWatchdog's alerts.
- **An event from another program.** A failover script, a deploy script, or a
  backup script announces what it did through the same channels as every
  alert.

```text
$ sudo mmwatchdog send-notice --alert-severity sos --title "Test of the alert destinations. Nothing is wrong."
Notice sent to every destination that takes sos:
  team_chat       delivered
  on_call_pager   delivered
```

| Option | Meaning |
| --- | --- |
| `--alert-severity SEVERITY` | Required: `info`, `warn`, or `sos`. The notice goes to every destination that takes the severity named by `--alert-severity` |
| `--title TEXT` | Required: the one line that says what happened |
| `--body TEXT` | Optional: more text, at most 8 KiB, for destinations that send the body. Without `--body` the notice has no body |
| `--destination NAME` | Optional: send the notice only to the destination named by `NAME`, whatever the named destination's `alert_severities` say |

- **The title.** MMWatchdog puts the node's name in front of the text, so the title
  reads `app-1: Test of the alert destinations. Nothing is wrong.` The whole
  title has at most [120 characters](#title-and-body). The command refuses a
  longer title with an error that states the length, and cuts nothing: the
  person who typed the title can shorten the title.
- **The result.** The command waits for the first delivery attempt to each
  destination and prints `delivered` or the failure, such as `on_call_pager
  failed: Pushover answered 400: application token is invalid, see
  https://pushover.net/api. Pushover does not know this application token.
  Copy it from pushover.net into the file that application_token_file
  names.` `mmwatchdog send-notice` exits 1 when a first attempt failed. A
  notice that no destination takes is recorded in `mmwatchdog events`; the command
  says so and exits 0.
- **A destination that is failing already.** A destination gets its alerts in
  order, so a notice waits behind the earlier alerts that MMWatchdog could not
  deliver to that destination, and their next attempt can be minutes away.
  `mmwatchdog send-notice` does not wait for delivery of the earlier alerts.
  `mmwatchdog send-notice` prints `on_call_pager not attempted
  yet: 3 earlier alerts are not delivered, and the latest attempt failed:
  Pushover answered 400: application token is invalid, see
  https://pushover.net/api. Pushover does not know this application token.
  Copy it from pushover.net into the file that application_token_file
  names.`, and exits 1: the test has shown that the
  destination does not get MMWatchdog's alerts now.
- **Delivery.** The notice is queued in the [outbox](#delivery) like every
  alert, so a failed first attempt is retried after the command has ended.
  The command exits 4 when the daemon refuses the notice or cannot save it;
  then nothing is sent.
- **No incident.** A notice is an alert event of the type `notice`. A notice
  has no check group, no incident, no recovery, and no reminder, like
  MMWatchdog's own `rebooted` notice.
  A [maintenance window](recovery.md#maintenance-windows)
  does not hold a notice back: a deploy script announces its deploy during
  the window that it turned on.
- **One destination by name.** With `--destination NAME` the notice goes to
  that destination and to no other, whatever its `alert_severities` say. A
  name that the node's destinations do not have is refused with an error
  that lists the destinations.
- **In a fleet.** On a node that has joined a hub, the notice goes through the
  hub like every alert, and the command prints whether the hub accepted it.
  To see the result for each destination, send the notice from the fleet hub:
  run the same `mmwatchdog send-notice` command there. A destination named
  with `--destination` travels with the notice, and the hub delivers the
  notice to the named destination. The node
  checks the name against
  its copy of the fleet settings, and refuses `--destination` before it has
  that copy. The hub never puts a notice into a grouped message
  ([fleet alert grouping](fleet.md#fleet-alert-grouping)).

A failover script announces its event with one line:

```sh
mmwatchdog send-notice --alert-severity warn --title "load balancer: this machine is now the active one"
```

The [command reference](operation.md#command-reference) lists the command with
MMWatchdog's other commands.

## Delivery

Delivery uses a durable local outbox. MMWatchdog commits an incident transition and
its outbox entries in one transaction, so a crash can neither lose an alert
for a committed incident nor send one for an uncommitted transition. Delivery
is asynchronous and never blocks check execution, recovery, or another
destination.

- **Retries.** A failed attempt is retried after one second, then with the
  delay doubling up to 256 seconds. Each retry keeps the event ID of the
  alert whose delivery failed, so receivers can deduplicate. Delivery is at
  least once, not exactly once.
- **Order.** Each destination receives events in order, one delivery at a
  time. A failure always precedes the failure's recovery, including when
  the destination was unavailable for the failure and recovery.
- **Independence.** Each destination proceeds on its own; an unavailable
  receiver never delays another.
- **Bounded queue.** The outbox is limited by age and size
  ([storage limits](operation.md#retained-state-and-storage-limits)). A
  delivery that expires or overflows is dropped with a durable `delivery_gap`
  event, and the gap count appears in status with the time of the first and
  of the newest drop. MMWatchdog never drops a delivery silently.
- **An `sos` goes first.** One rule orders each destination's queue:
  - The deliveries of each fleet incident key wait in a line, oldest
    first. The deliveries without a key, such as a
    [notice](#send-a-notice), wait in one line of their own, so a notice
    keeps its place among them.
  - A line's next delivery is its oldest, except that an `sos` passes
    deliveries of its own key that have had a failed attempt and wait to
    be retried, so a `warn` stuck behind a refusal or HTTP `429` never
    holds the `sos` back. It never passes a delivery of its key that was
    not attempted yet, so a healthy destination gets an incident's opening
    before its raise, and never a closing alert, so closing and opening
    alerts of one key keep their order.
  - Of the lines' next deliveries, the destination gets an `sos` first,
    else one that does not wait to be retried, else the oldest. So a
    delivery that keeps failing holds back no other key: another key's
    opening goes ahead of it, and that key's `sos` right after.
  - Nothing is removed: each delivery that waits goes after the `sos`,
    because it may be the only alert that names a node or an exact
    finding.
  - While the destination fails or limits the rate (HTTP `429` with
    `Retry-After`), the wait before the next attempt stays the same for
    the whole destination, and the `sos` goes first once the next attempt
    is allowed, so many keys cannot start a storm of retries.
- **Reminders never crowd out alerts.**
  The outbox holds at most one undelivered reminder for each incident and
  destination ([reminders](#reminders)).
- **Identity.** A destination's identity is its name, module, receiving
  address, and credential content. Moving an unchanged credential to another
  file keeps the queue. Command output, including JSON, and fleet settings
  never show a credential fingerprint: a reader could use an unsalted hash
  to test password guesses.

  MMHotline signing secrets do not change destination identity. Public trust
  bundles, named by inputs ending
  in `certificate_authority_file`, also do not change destination identity.
  Nor do the webhook module's `bearer_token_file` and the email module's
  `login_password_file`: they prove who sends, while the receiver is the URL,
  or the mail server and recipients. A new token or password for the same
  address must not discard queued alerts. The credential files of the other
  modules can name the receiver (a Pushover user key, a Slack webhook URL),
  so they stay in the identity.
  Rotating a signing secret or trust bundle must not discard queued alerts.
  Two readable, different credential contents cancel the destination's queue.
  An unreadable credential has no fingerprint and preserves queued alerts,
  so a mount that is temporarily unavailable cannot discard notifications.

  Address means the webhook URL, SMTP server and recipients, MMHotline alert
  source ID, or a destination module's recipient inputs. An alert queued
  under the MMHotline alert source for nodes sending without the hub has
  that source's ID as its address too: removing or replacing the source
  cancels it alone.
  Message presentation such as `mention`, delivery limits, reminders,
  severity filters, and module run settings do not change the receiver.

  Removing or changing the receiver cancels its queued deliveries atomically
  with activation, with a local `delivery_cancelled` event. Requests already
  in flight finish; cancelled alerts are not redirected.
- **Storage failure.** If the outbox cannot be written, status reports the
  storage error with exit code 3 until a write succeeds. Meanwhile the
  alerts that wait in memory for a commit go straight from memory to the
  destinations their routes select, with the same severity filters,
  `send_only_the_alert_title`, title, and body, and retries at the outbox's
  pace until the destination accepts each alert. A full disk so cannot
  silence the alert that reports the full disk. A joined node sends these
  alerts to the hub, and straight to the destinations of its copy of the
  fleet settings when the hub has not accepted an alert within
  `send_alert_without_hub_after_waiting_for`; a hub sends its own straight
  to its destinations, without fleet alert grouping. An `mmhotline`
  destination gets the failures and notices from memory, and their
  resolutions only from the outbox. So does a destination whose service
  keeps incidents of its own, on a hub and in a joined node's direct
  delivery: another node's incident may hold the key open. The commit that succeeds later puts the
  same alerts in the outbox, and each destination gets them a second time,
  with the same event ID.

On a joined node the hub is one more route with the outbox rules for
retries, order, independence, bounded queues, reminders, identity, and
storage failure described above. The section
[alerts in a fleet](fleet.md#alerts-in-a-fleet) defines delivery through the
hub and the direct fallback. A destination module that a joined node may
have to run itself, when the hub does not accept an alert in time, must be
installed on that node too, which every deploy
does.

### Destination verification and health

Reload verifies each new or changed destination through its quiet module
operation, without a message. Results name the destination and say what was
verified or why verification failed. Verification failure makes reload exit
1 after activation. Reload does not roll back configuration. Reload prints
the exact `send-notice` command for each changed destination so a person
can test the whole path on purpose. MMWatchdog never runs the printed
`send-notice` command or sends a test message automatically.
There is no test reminder to keep.

A daemon start performs no verification. Daily verification checks every
destination; failed verifications retry once a minute. Modules without a
quiet verification operation are reported as unsupported for verification,
not as a refused credential. An unchanged destination keeps its next
verification time, verification failure and latest error, and failure
notice state in the state database across a restart.
Deliveries do not move the verification schedule.

MMHotline check-ins use a separate schedule. The MMWatchdog fleet hub or
standalone node sends every `mmhotline` destination a check-in
every 30 seconds, at start, and at once after every reload.

For each destination, MMWatchdog tracks the oldest undelivered alert and
quiet verification failures. When the oldest alert for a destination has
waited ten minutes, or verification has failed for ten minutes, MMWatchdog
sends one `warn` notice. The notice goes to the node's other destinations,
naming the failed destination and latest error. The title is
`Destination NAME is failing.`

After a successful delivery to, or verification of, the failed destination,
MMWatchdog sends one `warn` notice: `Destination NAME works again.`
The recovery notice requires that no undelivered alert for the failed
destination has waited ten minutes. If verification failed, a later
verification of the failed destination must also succeed. The ten-minute
failure threshold and recovery requirements for queue age and successful
verification still apply after a restart. Recovery uses `warn`, the failure
notice's severity, so destinations that receive the failure notice can also
receive the recovery notice.

Verification and delivery prove different things:
a webhook connection check can succeed while the receiver still refuses
alerts. Delivering another alert
does not restart the oldest undelivered alert's ten-minute wait.

Destination failure and recovery messages are ordinary notices without
incidents or reminders.
MMWatchdog delivers destination failure and recovery notices directly on
the node so an unreachable fleet hub cannot block the notices.

`mmwatchdog status` exits 3 while a destination fails, unless a check has an
open failure incident: then it exits 1, because failure takes precedence over
health unknown. The output shows the failing destination in both cases. The [Commands reference](../../reference/commands.md#mmwatchdog-status)
owns the command output and JSON fields.

The fixed ten minutes lets temporary service errors and exponential delivery
retries clear before another destination alerts a person. Quiet verification
avoids messaging the team just because someone reloaded configuration.
Validation warns for each `warn` or `sos` check that reaches no destination.
Validation refuses a silent cause that would keep an alerting check quiet,
because the combination hides the only alert. Node validation and reload
use a joined node's saved fleet settings to validate alert reachability and
silent causes.
`mmwatchdog validate --config-only` reads no node state, so the command
skips the validations of alert reachability and silent causes.

## Reminders

A long failure should not be forgotten because one alert was missed. A
destination with `remind_every` receives a `reminder` event for each announced
incident that is still open and whose alerts it gets, every `remind_every`
after the failure was announced or raised, or after its previous reminder:

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  on_call_mailbox:
    email:
      smtp_server: smtp.example.com:587
      smtp_connection_security: starttls
      from: mmwatchdog@example.com
      to: [oncall@example.com]
      login_user: mmwatchdog@example.com
      login_password_file: /etc/mmwatchdog/secrets/smtp
    remind_every: 4h
    alert_severities: [warn, sos]
```

- `remind_every` takes any positive duration or `never`, such as `1m`, `4h`, or `1d`.
  `remind_every: never` sends no reminders to the destination. An `mmhotline`
  destination takes no `remind_every`.
- A reminder carries the incident ID and its evidence, and its message names
  when the incident opened and the latest result of the check, such as
  `myapp/http still failing since 14:02 UTC; latest result: expected 200,
  received 503`. Each reminder is its own event with its own ID. For a
  cannot-check incident, the message says `MMWatchdog has not been able to
  run myapp/http since 14:02 UTC; latest result: ...`.
- A reminder carries the latest reading of a check with a limit
  on a number, so a person told 80 percent learns the value is at
  95 percent.
- MMWatchdog makes a reminder only for a destination that gets it by the rule of
  [which alert goes to whom](#which-alert-goes-to-whom). A reminder carries
  the incident's current severity, so for a check that names no
  destinations it goes to a destination only while that destination takes
  that severity: before a raise to `sos`, a destination that takes only
  `sos` gets no reminders, and after it, a destination that takes only
  `warn` gets no more. A destination that the incident's check names
  receives every reminder, and a destination with `alert_severities: []`
  gets reminders only of the checks that name it. So the event history
  holds no reminder that was queued for no destination.
- A raise is the incident's newest alert, so the reminders count from it. A
  destination that takes only `sos` hears of the incident first through the
  raise, and its first reminder comes one `remind_every` later.
- An undelivered reminder is replaced by the next reminder for the same
  incident and destination. However short `remind_every` is and however long
  a destination is unreachable, reminders cannot fill the bounded outbox and
  push an alert out of it.
- During a maintenance window that covers the check group, MMWatchdog sends no
  reminders for it. After the window ends, reminders resume one interval
  later if the incident is still open and announced.
- Reminders stop when the incident resolves or closes without recovery
  (`closed_without_recovery`).
- The time of the last reminder per incident and destination is persisted, so
  a daemon restart neither repeats nor skips one.
- In a fleet the owning node sends each reminder through the hub, which
  delivers it to the destination it names and
  [groups](fleet.md#fleet-alert-grouping) matching reminders from other nodes.
  For a key with an [exact finding](checks.md#exact-findings), the hub
  keeps one reminder schedule per destination and key itself, and the
  nodes send no reminders while the hub is reachable
  ([one alert per exact finding](fleet.md#one-alert-per-exact-finding)).

**A reminder interval for one check.** `remind_every` belongs to a
destination. For more frequent reminders of a particular check, the check
names a destination whose `remind_every` states the interval required for
the check ([which alert goes to whom](#which-alert-goes-to-whom)).
In the configuration below, a check whose failure is urgent posts to the
team's chat channel again every minute:

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  team_chat:
    remind_every: never
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]
  team_chat_reminding_every_minute:
    slack:                                        # the same channel as team_chat
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    remind_every: 1m
    alert_severities: []                          # only checks that name this destination reach it
```

The check states
`send_alerts_only_to_destinations: [team_chat_reminding_every_minute]`. The
incidents of checks that do not name `team_chat_reminding_every_minute` get
no reminders from `team_chat_reminding_every_minute`, because the
destination takes no severity.

To quiet a known problem, turn on a
[maintenance window](recovery.md#maintenance-windows) for the check group; it
ends on its own, so the problem cannot stay silent by accident.

## Alert decisions

The alert decisions in the table are settled. Revisit an alert decision only
with a concrete workflow that the decision does not support.

| Decision | Reason |
| --- | --- |
| Delivery is at least once, not exactly once | Exactly-once delivery needs cooperation from every receiver. Stable event IDs let receivers detect and ignore duplicate deliveries. |
| One durable outbox serves every destination, whatever its module | Ordering, retries, and gaps behave the same whatever the channel. |
| Each destination is ordered and independent | Failure precedes recovery, and one broken receiver cannot delay the others. |
| An `sos` passes the waiting deliveries below `sos` of the same destination, while the wait before a retry stays destination-wide | A warning that waits for a retry must never delay a page, and one rate-limited destination must not get a retry for each key at once. |
| Every alert has a title and a body, written once by MMWatchdog | An alert reads the same on every channel. A short channel gets a complete line, and no module needs wording of its own. |
| A title has at most 120 characters, and MMWatchdog cuts inside the longest name | The title fits an SMS together with the words and the link that a service adds, and the words that say what changed survive the cut. |
| MMWatchdog writes the title a second time with the alert's severity in front, in the labels that MMHotline shows, and an email and a chat show that line | A team that sends every severity to one channel sees which message is urgent. A recovery reads `Resolved SOS`, which says the incident is over and how urgent the incident was. An incident closed without recovery reads `Closed SOS`, because MMWatchdog does not know whether the problem is fixed. A person must not read the closure as proof of recovery. MMWatchdog writes the title with severity once, so no module needs wording of its own. An alert reads the same in email, chat, and MMHotline. |
| A request to a destination module is at most 1 MiB, the files of one destination hold at most 64 KiB together, and its inputs are at most 256 KiB as JSON | A fixed size lets the helper and every module read a whole request. The file size limit ensures that the largest alert and the files always fit. Validation finds files that are too large before a deploy. |
| A destination can ask for the title only, never for a number of characters | A length limit cuts the text in the middle; the title is written to be complete in one line. |
| The core holds the destinations that speak a general protocol (`webhook`, `email`, `system_log`) and `mmhotline`; a service that one company runs is an official extra module, however many people use it | The services of single companies change with those companies, and a script can follow them without a new `mmwatchdog` program. A person's own alarm system receives alerts through a destination module, just as Slack does. MMHotline is the companion on-call program that a team runs on its own server, with a signed protocol of its own. |
| The official extra modules for chat send with the primitive `http_request` | A node needs nothing but MMWatchdog to deliver an alert: no `curl` and no other tool. |
| No message templates | A person who wants their own wording writes a module, which receives the whole event. |
| MMWatchdog runs with no destination only when `alert_destinations.yml` says `destinations: {}` | A person who wants only the restarts writes one line, and a person who forgot the destinations gets an error, not silence. |
| Severity decides where an alert goes, unless its check names its destinations | The common case needs one line on each check and one list on each destination, while a check that belongs to one person or one team says so on the check. |
| A check that names its destinations sends every alert to exactly those, whatever their `alert_severities` say | A reader of the check sees on one line who is told. Two filters at once would need a second look at every destination. |
| Routing is written on the check, not as a list of check groups on a destination | A reader of the check sees who is told, and a mistyped check group name on a destination would route nothing, with no error. |
| A wrong destination name is an error, or the alert goes by severity | No mistake in a name can lose an alert. |
| No separate settings on a check for a reminder interval, a mention, or silence | The destination list covers the reminder interval, a mention, and silence. One setting is easier to learn than four. |
| Every alert carries `info`, `warn`, or `sos`, MMHotline's words | Severity is the basis for routing, with or without MMHotline. |
| Every check states its `alert_severity`; a node states the severity of its own reboot notice; the maintenance notices are `info` | A reader under pressure sees the alert severity on the check. No check group or node default can hide a stopped unit's alert severity. |
| Every destination states its `alert_severities` | What a destination receives is written where it is configured. |
| A cannot-check incident takes the check's current severity | The operator still needs an SOS notification when MMWatchdog cannot run an SOS check. |
| An incident's severity only rises within the incident | An alert already sent stays true until the problem ends. Readings that repeatedly cross a limit cannot repeatedly raise and lower an incident's severity. |
| A raise is a new alert for the same incident, and a recovery goes where the incident's alerts went | A destination that takes only `sos` learns of the incident from the raise and hears when the incident ends. |
| One command, `mmwatchdog send-notice`, serves a test and an event from another program | A test notice and a notice from another program each send a one-time message through the real delivery path. A test that needs a real failure proves less and disturbs more. |
| No incident acknowledgement | A maintenance window quiets a known problem and ends on its own. |
| Every destination that takes reminders states a positive `remind_every` or `never` | A repeated alert is noise unless the operator asked for it. |
| A reminder is made only for a destination that gets the incident's alerts, and counts from the incident's newest alert | A reminder that no destination gets would push real events out of the event history, and `mmwatchdog events` would show it as sent. A destination that heard first of a raise could otherwise get a reminder right after it. |
| `remind_every` takes any positive duration or `never` | A team may want one urgent problem repeated every minute; replacing an undelivered reminder with the next reminder keeps reminders from crowding alerts out of the outbox, so no minimum is needed. |
| Redirects are not followed | A destination's identity is the URL the operator wrote. |
| Credentials live only in files | Credentials stay out of configuration, version control, URLs, and output. |
| While `state.db` cannot be written, alerts go from memory, and the later commit sends them again | A duplicate alert is better than a dropped one. Remembering which alerts went out would need the `state.db` that cannot be written. |
| Expired or dropped deliveries leave a visible gap | The operator always knows when an alert may have been missed. |
| Local bookkeeping events are not sent | Destinations receive changes a person acts on, not internal records. |
| A destination module answers `delivered` or `failed`, and no answer stops the retries | A module that said "do not try again" by mistake would lose an alert. A repeated attempt costs almost nothing, and the core destination modules try again after an error too. |

The reasons for the behavior of one destination module are on the module's
page, under "Why it works this way".

## Beyond release 1

- SMS, phone calls, on-call schedules, and escalation inside MMWatchdog.
  MMHotline provides on-call schedules and escalation through the
  [`mmhotline` destination](../../reference/modules/mmhotline.md). To deliver through
  another provider, MMWatchdog calls a
  [destination module](#destination-modules).
- Filtering a destination by the type of event, such as a destination that
  takes failures and no maintenance alerts.
