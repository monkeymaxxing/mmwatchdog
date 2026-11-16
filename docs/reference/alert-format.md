# Alert format reference

This page shows what an alert says: the title and the body of each kind of
alert, the severity label in front of the title, what each destination
module sends, the JSON that a `webhook` destination receives, and the
fields of the events that `mmwatchdog events --json` prints.

Read this page when you write a receiver for alerts, such as a webhook
endpoint or a [destination module of your own](writing-modules.md), or
when you want to know what a person will read on the phone. To choose
where alerts go, see
[Send alerts where your team sees them](../how-to/send-alerts-somewhere.md).

Every alert is one **event**: a record of something that happened, such as
a check that failed. MMWatchdog writes two texts for each alert, once, when
the alert is queued, and every destination gets the same two texts:

- **The title** is one short line that is complete without the body. The
  title names the node, the check group, the check, and what changed.
- **The body** holds everything else a person needs to act.

## An alert, from title to body

The check `queue_is_short` in the check group `myapp` on the node `web02`
runs a command that exits with code 7. After the check has been failing for
the check group's `alert_after_failing_for`, the alert goes out. A chat
destination, such as `slack`, shows the title with the severity label in
front, then the body:

```text
Warning: web02: myapp/queue_is_short failed
```

```text
command exited 7
failing since 10:30 UTC
exit code: 7
stdout:
  queue has 120 jobs
stderr:
  worker 3 stopped
```

- `Warning:` is the label of the severity `warn`, so a reader sees how
  urgent the alert is from the first word.
- `web02: myapp/queue_is_short failed` is the title: the node, the check
  group and the check, and what changed.
- The body starts with the message of the run that failed, then says since
  when the check has been failing, then gives a short excerpt of the
  evidence: the exit code and the first lines of the command's output.

## The title

**A title has at most 120 characters,** so that the title fits an SMS and a
phone call. A title that would be longer is shortened:

1. MMWatchdog shortens the longest name in the title: the node, the check
   group, the check, or a failed systemd unit. The cut is marked with `…`.
2. If the title is still too long, MMWatchdog shortens the name that is now
   the longest. MMWatchdog leaves at least 12 characters of each name.
3. MMWatchdog never cuts the last words, which say what changed.

The event of a shortened title carries `title_was_shortened: true`, and the
event's `node`, `check_group`, and `check` fields keep every name in full.

`sudo mmwatchdog validate` names every check whose longest title would be
cut on the node, and prints that title as MMWatchdog would send it. The
configuration stays valid:

```text
$ sudo mmwatchdog validate
Valid configuration: 1 check group, 1 alert destination
An alert title has at most 120 characters. For node web02, MMWatchdog will shorten the alert titles listed below; shorten the check group name or check name to keep the full title:
  nightly_backup_of_the_customer_database/the_restored_copy_of_last_night_has_as_many_rows_as_the_primary: the check's longest alert title has 149 characters and will be sent as: web02: MMWatchdog still cannot check nightly_backup_of_the_customer_database/the_restored_copy_of_last_night_h…, now SOS
```

The longest title that a check can get is the raised alert of a check that
MMWatchdog cannot run, so `validate` counts that title.
`sudo mmwatchdog validate --json` lists the same checks in
`alert_titles_that_will_be_shortened`, each with `check_group`, `check`,
`characters`, and `title`.

## The severity label

A person who reads an alert in an email or a chat should see from its
first words how urgent the alert is. So MMWatchdog writes the title a
second time with a label in front, as the event's
`title_with_alert_severity`:

| `alert_severity` | Label | Label of an alert that resolves an incident | Label of an alert that closes an incident without recovery |
| --- | --- | --- | --- |
| `info` | `Info` | `Resolved info` | `Closed info` |
| `warn` | `Warning` | `Resolved warning` | `Closed warning` |
| `sos` | `SOS` | `Resolved SOS` | `Closed SOS` |

- An **incident** is a problem that has lasted `alert_after_failing_for`.
  An alert of type `recovered` or `can_check_again` resolves an incident.
- An alert of type `closed_without_recovery` closes an incident whose check
  was changed or removed. The label says `Closed`, because MMWatchdog does
  not know whether the problem is fixed.
- The `Resolved` and `Closed` labels name the highest severity that the
  incident reached: a check that alerted with `warn` and was raised to
  `sos` recovers with `Resolved SOS`.
- Every maintenance alert has the severity `info`, also the alert at the
  end of maintenance, which closes no incident.
- The label is not counted in the 120 characters of a title, so the line
  with the label has at most 138 characters.
- The labels are those that MMHotline shows, so an alert reads the same in
  an email, in a chat, and in MMHotline.

## Titles and bodies of each kind of alert

The examples come from real alerts of MMWatchdog 0.1.1, on the nodes
`web02` and `app01`. Each shows the line with the label, then the body.

**A check failed** (`failed`):

```text
Warning: web02: myapp/health_page_answers failed
```

```text
expected HTTP 200, received 503
failing since 10:30 UTC
```

**A check that failed on and off.** When some runs between the first
failure and the alert were healthy, the body says how many of the last runs
failed. This example is a raised alert:

```text
SOS: app01: myapp/health_page_answers still failing, now SOS
```

```text
myapp/health_page_answers has been failing for 1m, so this alert is now SOS (raise_to_sos_after_failing_for: 1m)
failing on and off since 04:32 UTC; 4 of the last 7 runs failed
```

**The alert was raised to `sos`** (`failure_alert_severity_raised`), after
the check's `raise_to_sos_after_failing_for`:

```text
SOS: web02: myapp/health_page_answers still failing, now SOS
```

```text
myapp/health_page_answers has been failing for 30s, so this alert is now SOS (raise_to_sos_after_failing_for: 30s)
failing since 10:30 UTC
```

**A reminder** (`reminder`), after the destination's `remind_every`. The
first line says since when, and gives the latest result:

```text
Warning: web02: myapp/queue_is_short still failing
```

```text
myapp/queue_is_short still failing since 10:30 UTC; latest result: command exited 7
exit code: 7
stdout:
  queue has 120 jobs
stderr:
  worker 3 stopped
```

**A check recovered** (`recovered`). The body is the message of the run
that ended the failure:

```text
Resolved SOS: web02: myapp/health_page_answers is ok again
```

```text
HTTP 200 and configured content match
```

**MMWatchdog cannot run a check** (`cannot_check`), such as a run that took
longer than `time_limit_per_run`:

```text
Info: web02: MMWatchdog cannot check myapp/slow_check
```

```text
the run took longer than 2s (time_limit_per_run), so MMWatchdog ended it
MMWatchdog has not been able to run the check since 10:31 UTC
```

**MMWatchdog can run the check again** (`can_check_again`):

```text
Resolved info: web02: MMWatchdog can check myapp/slow_check again
```

```text
MMWatchdog can run the check again
```

**An incident closed without recovery** (`closed_without_recovery`),
because the check was changed or removed:

```text
Closed SOS: app01: nightly_backup/heartbeat incident closed, maybe not fixed
```

```text
the check was changed in the configuration; MMWatchdog does not know whether the problem is fixed
```

**A check that keeps other checks quiet.** When other checks name the
failing check in their `no_alerts_or_recovery_actions_while_these_checks_fail`,
the body lists them:

```text
SOS: app01: postgres/postgres_service_is_running failed
```

```text
systemd unit postgresql.service is stopped. systemd reports no failure: the unit was stopped on request, or its program ended normally. MMWatchdog did not start the unit.
failing since 07:11 UTC
no alerts and no recovery actions for these checks while this check fails: myapp/health_page_answers
```

**Maintenance on and off** (`maintenance_on`, `maintenance_off`), always
`info`:

```text
Info: web02: maintenance on for myapp
```

```text
maintenance until 2026-11-15 10:42 UTC, turned on by deploy: database upgrade
```

```text
Info: web02: maintenance off for myapp
```

```text
maintenance turned off by deploy: database upgrade
```

The title says `for the whole node` for whole-node maintenance, names up to
three check groups, such as `maintenance on for worker_1, worker_2`, and
counts more, such as `maintenance on for 5 check groups`.

**A notice** (`notice`), sent with `mmwatchdog send-notice`. The title is
the node's name and the text of `--title`. The body is the text of
`--body`, or nothing:

```text
Info: web02: Test of the alert destinations. Nothing is wrong.
```

```text
Sent by Barbara after the firewall change.
```

**The node rebooted** (`rebooted`), at the severity of the node's
`alert_severity_when_this_node_has_rebooted`. The title is `web02 rebooted`,
and the body has the form
`web02 rebooted at 03:12 UTC; MMWatchdog last ran at 03:10 UTC`. The first
start of MMWatchdog on a node sends no reboot notice. This page shows the
format from the code, because no lab node rebooted during the capture.

**One failed systemd unit** of an
[`every_systemd_unit`](modules/every_systemd_unit.md) check is a separate
incident, and the title names the unit after the check, such as
`web02: failed_systemd_units/machine_has_no_failed_systemd_unit: backup.service failed`
and `... backup.service is ok again`.

### What the body holds

The body of an alert that opens or raises an incident holds, in this
order:

1. the message of the run, such as `expected HTTP 200, received 503`.
2. since when: `failing since 14:02 UTC`, or
   `failing on and off since 12:00 UTC; 5 of the last 10 runs failed`, or
   for a check that MMWatchdog cannot run,
   `MMWatchdog has not been able to run the check since 14:02 UTC`. A time
   on another day than the alert carries its date, such as
   `2026-09-29 14:02 UTC`.
3. the checks that the failing check keeps quiet, at most 20 by name,
   then `, and N more`.
4. the details that the check's module adds, such as
   [the commands that use the most memory](modules/node_resource.md),
   at most 12 lines.
5. an excerpt of the evidence: `exit code: N`, then the first five lines
   of standard output and of standard error, each under its own heading,
   each line cut at 200 characters, and `…` when more lines follow.

A reminder's body starts with the reminder's message, which already says
since when, then the quiet checks and the evidence excerpt. The body of a
recovery, a closed incident, and a maintenance alert is the message alone.

## Fleet messages

On a fleet, the hub delivers the alerts of every joined node. **Fleet alert
grouping** sends the first alert at once, and joins alerts of the same kind
that arrive from several nodes soon after into one message. A grouped
message counts its nodes in the title and names up to 20 of them in the
first line of the body.

A real grouped message from the lab hub:

```text
Info: maintenance on for 2 nodes
```

```text
maintenance started on 2 nodes: web01, web02 (by root: mmwatchdog deploy)
```

For a grouped alert about a check, the first line of the body names the
nodes, and the lines below are the alert of the first node:

```text
myapp/health_page_answers failed on 2 nodes: web01, web02
from web01:
  expected HTTP 200, received 503
  failing since 14:02 UTC
```

The titles of fleet messages, as the code writes them (`web01` is a joined
node):

| Alert | Title for one node | Title for several nodes | First line of a grouped body |
| --- | --- | --- | --- |
| A check failed | `web01: myapp/http failed` | `myapp/http failed on 7 nodes` | `myapp/http failed on 7 nodes: web01, web02, ...` |
| A check recovered | `web01: myapp/http is ok again` | `myapp/http is ok again on 3 nodes` | `myapp/http is ok again on 3 nodes: ...` |
| The node rebooted | `web01 rebooted` | `3 nodes rebooted` | `3 nodes rebooted: ...` |
| A joined node stopped reporting | `web01 stopped reporting to the MMWatchdog fleet hub` | `3 nodes stopped reporting to the MMWatchdog fleet hub` | `3 nodes stopped reporting: web01, web02, web03` |
| The node is still silent, raised | `web01 still not reporting to the MMWatchdog fleet hub, now SOS` | `3 nodes still not reporting to the MMWatchdog fleet hub, now SOS` | `3 nodes that stopped reporting are now SOS: ...` |
| The node reports again | `web01 reports to the MMWatchdog fleet hub again` | `3 nodes report to the MMWatchdog fleet hub again` | `3 nodes are reporting again: ...` |
| A hub-outage alerter node cannot reach the hub | `web01 cannot reach the MMWatchdog fleet hub` | `2 nodes cannot reach the MMWatchdog fleet hub` | `2 nodes cannot reach the MMWatchdog fleet hub: ...` |
| The hub can be reached again | `web01 reaches the MMWatchdog fleet hub again` | `2 nodes reach the MMWatchdog fleet hub again` | |

- A **hub-outage alerter node** is one of the two joined nodes that alert
  when the hub stops answering. The node sends that alert to the
  destinations directly, because the hub is down.
- A grouped message lists at most 20 node names, then `, and N more`, and
  carries every node count in `node_count`.
- The hub's alert about a silent node is a check of the built-in check
  group `node_reports`, named after the silent node.

**One closing alert for the fleet.** A destination whose service keeps
incidents of its own, such as [`mmhotline`](modules/mmhotline.md), opens
one incident for each check across the fleet. The hub closes that incident
with one alert, once no node has the problem any more, with a title such as
`myapp/http is ok on every node again` or
`Every node is reporting to the MMWatchdog fleet hub again`. When only one
node had the problem, the title is that node's own, such as
`web01: myapp/http is ok again`.

## What each destination sends

| Destination module | Sends |
| --- | --- |
| [`webhook`](modules/webhook.md) | The whole event as JSON: see [The webhook request](#the-webhook-request) |
| [`email`](modules/email.md) | The title with the label as the subject. The text is the body, then the alert's time and IDs |
| [`system_log`](modules/system_log.md) | One line with the title and the label in front |
| [`mmhotline`](modules/mmhotline.md) | The title without a label as MMHotline's title, and the body as MMHotline's message. MMHotline puts its own label in front, shows the title in notifications, and shows the body on the alert's page. MMHotline gets a `closed_without_recovery` alert as a resolution, such as `resolved-sos` |
| Official extra modules, such as [`slack`](../../official_extra_modules/slack/README.md) and [`pushover`](../../official_extra_modules/pushover/README.md) | The title with the label as the first line or the message title, and the body below |
| A [module of your own](writing-modules.md) | The whole event, in the module's request |

**A destination can ask for the title only.** Some receivers take only a
little text, such as an email address that turns each email into an SMS.
Such a destination states `send_only_the_alert_title: true`:

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  on_call_sms:
    webhook:
      url: https://sms-relay.example.com/mmwatchdog
    send_only_the_alert_title: true     # this destination gets the title and no body
    alert_severities: [sos]
    remind_every: never
```

MMWatchdog then queues the alert without `body`, `message`, `evidence`,
and the list of quiet checks. The title, the title with the label, and the
fields that identify the alert stay. This real request went to a
title-only `webhook` destination:

```json
{
  "alert_severity": "sos",
  "at": "2026-11-15T10:30:33.982236846Z",
  "check": "health_page_answers",
  "check_group": "myapp",
  "fleet_incident_key": "f970a8b34b85fbc16bbf6d82d654ab10",
  "id": "3d8269c7a64d9ea896d2f0d05219ec10",
  "incident_id": "880bc318ffded0bbba93a9562fa61e5b",
  "node": "web02",
  "title": "web02: myapp/health_page_answers still failing, now SOS",
  "title_with_alert_severity": "SOS: web02: myapp/health_page_answers still failing, now SOS",
  "type": "failure_alert_severity_raised"
}
```

An `email` destination with the setting sends the title with the label as
the subject and no text. Changing the setting applies to alerts queued
after the change. An `mmhotline` destination takes no
`send_only_the_alert_title`.

## The webhook request

A `webhook` destination sends each alert as one HTTP `POST` request to its
`url`:

- **Headers:** `Content-Type: application/json`, and
  `Idempotency-Key` with the event's `id`. With `bearer_token_file`, also
  `Authorization: Bearer TOKEN`.
- **Body:** the event as JSON, with the fields in
  [Event fields](#event-fields). A field without a value is left out,
  except `check`, which is empty for an alert about no check.
- **Success:** any status code from 200 to 299. Any other code, or no
  answer within `time_limit_per_delivery_attempt`, is a failed attempt, and
  MMWatchdog tries again later with the same `id`. So a receiver that sees
  an `id` twice can drop the second request.

The real body of a failure alert (the path in `source.file` is shown as on
a node with the default configuration directory):

```json
{
  "alert_severity": "warn",
  "at": "2026-11-15T10:30:13.984945499Z",
  "body": "command exited 7\nfailing since 10:30 UTC\nexit code: 7\nstdout:\n  queue has 120 jobs\nstderr:\n  worker 3 stopped",
  "check": "queue_is_short",
  "check_group": "myapp",
  "evidence": {
    "failing_since": "2026-11-15T10:30:03.884856553Z",
    "incident_id": "4b9350202210b1bad9076d15756a9b86",
    "observation": {
      "at": "2026-11-15T10:30:13.984945499Z",
      "duration_ns": 1736898,
      "exit_code": 7,
      "message": "command exited 7",
      "protocol": 1,
      "status": "failed",
      "stderr": "worker 3 stopped\n",
      "stdout": "queue has 120 jobs\n"
    },
    "source": {
      "column": 7,
      "file": "/etc/mmwatchdog/check_groups/myapp.yml",
      "line": 13
    },
    "timing": {
      "alert_after_failing_for": "10s",
      "check_every": "5s",
      "recovered_after_healthy_for": "10s",
      "time_limit_per_run": "2s"
    }
  },
  "fleet_incident_key": "45100a4d8c63e7f3b08aba2dec734f86",
  "id": "ff1d8cb26f858dca0ea31bd5e3e9b3e8",
  "incident_id": "4b9350202210b1bad9076d15756a9b86",
  "message": "command exited 7",
  "node": "web02",
  "title": "web02: myapp/queue_is_short failed",
  "title_with_alert_severity": "Warning: web02: myapp/queue_is_short failed",
  "type": "failed"
}
```

The recovery of the same incident carries
`"alert_severities_sent_for_incident": ["warn"]`, the same `incident_id`,
and the evidence captured when the incident opened.

## Event fields

The webhook body, the request to a destination module, and each entry of
`mmwatchdog events --json` have the same fields:

| Field | Meaning |
| --- | --- |
| `id` | The event's ID, 32 hexadecimal digits. Every destination and every retry of the alert get the same ID |
| `type` | What happened. See [Event types](#event-types) |
| `node` | The host name of the node that the alert is about. On a grouped message of the hub, empty when the nodes differ |
| `check_group`, `check` | The check group and the check, such as `myapp` and `health_page_answers`. `check` is empty for an alert about no check. A `rebooted` and a `notice` event have no `check_group` |
| `finding` | On an alert about one finding of a check, the finding's ID, such as the failed unit `backup.service` of an `every_systemd_unit` check |
| `check_groups` | On a maintenance event, the check groups of the maintenance command, or `["every_check_group"]` for the whole node |
| `alert_severity` | `info`, `warn`, or `sos`. On a recovery or a closed incident, the highest severity that the incident reached |
| `alert_severities_sent_for_incident` | On a recovery or a closed incident, every severity that the incident's alerts went out with, so the recovery goes where those alerts went |
| `at` | When the change happened, in RFC 3339 with nanoseconds, in UTC |
| `title` | The title, at most 120 characters |
| `title_was_shortened` | `true` when MMWatchdog shortened the title. Left out otherwise |
| `title_with_alert_severity` | The title with the severity label in front |
| `body` | The body, as lines separated by `\n` |
| `message` | The message of the run, such as `expected HTTP 200, received 503`. For a grouped message, the nodes. For a notice, the text of `--title` without the node's name |
| `incident_id` | The incident that the event belongs to. Match a recovery to its failure by `incident_id`: after a delivery gap, a destination can get a recovery without the failure |
| `fleet_incident_key` | 32 hexadecimal digits, the same for one check, or one finding, and one kind of incident, on every node of a fleet and on a standalone node. A notice, a reboot, and a maintenance alert have none |
| `evidence` | On an incident's alerts, the run that confirmed the incident. See [Evidence fields](#evidence-fields) |
| `maintenance` | On a maintenance event: `until`, `reason`, `user`, and `without_maintenance_on_and_off_alerts` |
| `nodes`, `node_count` | On a grouped message or a fleet closing alert: up to 20 node names, and the count of all nodes |
| `destination` | On a `reminder`, the one destination that the reminder is for |
| `send_alerts_only_to_destinations` | The destinations that the alert's check names, or the one destination that `mmwatchdog send-notice --destination` names |
| `no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails` | On a failure alert, its raise, and its reminders: up to 20 checks that the failing check keeps quiet, each as `CHECK_GROUP/CHECK` |
| `number_of_checks_with_no_alerts_or_recovery_actions_while_this_check_fails` | How many checks the failing check keeps quiet in all |
| `seq` | Only in `mmwatchdog events --json`: the event's position in the node's event history |

### Evidence fields

The evidence is frozen when the incident opens, so every later alert of the
incident, the reminders, the recovery, and the closing alert carry the same
evidence. One evidence object is at most 16 KiB as JSON: MMWatchdog
shortens the standard output and error first, then removes details and
measurements, then shortens the message, and marks the evidence with
`truncated: true`.

| Field | Meaning |
| --- | --- |
| `incident_id` | The incident |
| `observation` | The run that confirmed the incident: `status` (`ok`, `failed`, or `unknown`), `message`, `at`, `duration_ns`, and when present `exit_code`, `stdout`, `stderr`, `measurements` (each with `value` and `unit`), `details`, `findings`, and `sample_window_ns`. `protocol` is the version of the observation's format, `1` |
| `source` | Where the check is written: `file`, `line`, and `column` |
| `timing` | The check's timings when the incident opened. Empty for a heartbeat |
| `failing_since` | When the failing time began, for a failure incident |
| `cannot_check_since` | When MMWatchdog first could not run the check, for a cannot-check incident |
| `last_runs_failed`, `last_runs_counted` | For a check that failed on and off: how many of the counted runs failed. Left out when every run failed |
| `truncated` | `true` when MMWatchdog shortened the evidence |

## Event types

These types go to destinations:

| Type | Sent when |
| --- | --- |
| `failed`, `recovered` | A failure incident opens, or resolves |
| `cannot_check`, `can_check_again` | MMWatchdog cannot run a check for `alert_after_failing_for`, or can run the check again |
| `failure_alert_severity_raised`, `cannot_check_alert_severity_raised` | An open incident is raised to a higher severity |
| `reminder` | An incident is still open after the destination's `remind_every` |
| `closed_without_recovery` | A configuration change, or a node that the hub forgets, closes an incident without a recovery |
| `maintenance_on`, `maintenance_off` | A maintenance window starts or changes, and ends, expires, or ends because its check group was removed |
| `rebooted` | MMWatchdog starts for the first time after the node rebooted |
| `notice` | A person or a program ran `mmwatchdog send-notice` |

`mmwatchdog events` also shows events that only the node records and no
destination gets. The last three types are in the hub's history of the
nodes, which `mmwatchdog fleet NODE` shows:

| Type | Recorded when |
| --- | --- |
| `systemd_unit_restart_requested`, `systemd_unit_restart_result`, `systemd_unit_restart_withheld` | MMWatchdog asks systemd to restart a unit, learns the result, or holds a restart back |
| `recovery_command_started`, `recovery_command_result` | A recovery command starts, and ends |
| `recovery_attempts_reset` | `mmwatchdog reset-recovery-attempts` set the count of attempts back to zero |
| `delivery_gap` | Alerts were dropped, because they waited longer than `keep_undelivered_alerts_for` or the queue grew past its size, or because MMWatchdog could not save them |
| `delivery_cancelled` | Deliveries were cancelled, because their destination was removed or changed |
| `history_gap` | On the hub, a joined node deleted events to stay within its event history limits before the events reached the hub |
| `node_joined`, `node_replaced` | On the hub, a node joined, or a new node of the same name replaced a silent node |

A recorded-only event has `id`, `type`, `node`, `at`, `message`, and, when
the event is about a check, `check_group` and `check`. The event has no title and
no body. For example:

```json
{
  "at": "2026-11-15T07:19:15.697756301Z",
  "check": "health_page_answers",
  "check_group": "myapp",
  "id": "29c6e3628d3d40d84098a0cedce09b1a",
  "message": "restart systemd unit myapp.service, attempt 1",
  "node": "app01",
  "seq": 96,
  "type": "systemd_unit_restart_requested"
}
```

`mmwatchdog events --json` prints a list of events, newest first.
[Commands](commands.md) describes the command and its flags.

## Related pages

- [Send alerts where your team sees them](../how-to/send-alerts-somewhere.md):
  add a destination and choose which alerts it gets.
- [Configuration files](configuration-files.md): every setting of a
  destination.
- [Module pages](modules/README.md): the inputs of each destination module.
- [Writing a module](writing-modules.md): the request that a destination
  module of your own reads.
