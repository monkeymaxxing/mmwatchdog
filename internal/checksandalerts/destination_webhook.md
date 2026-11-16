# `webhook`

`webhook` posts each alert to a web address that you name, as MMWatchdog's own alert
event in JSON over HTTP or HTTPS. A receiver at the configured web address
reads the event and decides how to act. The receiver can be a small
program of the team's own or a relay to another service.

Choose `webhook` for a receiver that reads MMWatchdog's event as it is. A service
with a format of its own, such as Slack or Telegram, takes a
[destination module](../../docs/reference/writing-modules.md#destination-modules) that speaks that
format, such as the official extra module
[`slack`](../../official_extra_modules/slack/README.md). A destination names
its module and states the settings that every destination takes, as [the alert destinations file](../../docs/reference/configuration-files.md#the-alert-destinations-file) describes.

## Set up

HTTPS verifies the server certificate and name by default.
`trusted_certificate_authority_file` replaces the system's authorities with
PEM certificates from `trusted_certificate_authority_file`. `dangerously_skip_server_certificate_check:
true` encrypts without checking the certificate or identity.

The token file holds one token of 1 to 4,096 bytes on one line. MMWatchdog sends the
token to another service, which decides what token is valid; MMWatchdog imposes no
minimum beyond one byte. Secret files follow the
[credential permission rule](../../docs/reference/configuration-files.md#secret-files).

Place permission settings beside `webhook`, where `alert_severities` stands.

## What it sends

For each alert, MMWatchdog sends one `POST` request to `url` with the alert event as
its JSON body:

- `Content-Type: application/json`.
- `Idempotency-Key`, which holds the event ID, matching the body's `id`.
  Each retry preserves the original POST's event ID in the header and body,
  so the receiver can recognize an alert already received.
- `Authorization: Bearer TOKEN` when the destination states
  `bearer_token_file`. Without `bearer_token_file`, the request has no
  `Authorization` header.

The request body contains the whole alert event. The table lists the
event's fields. The [alert events](../../docs/changing-mmwatchdog/design/alerts.md#alert-events)
section of the design explains the event fields:

| Field | Field contents |
| --- | --- |
| `id`, `incident_id` | The event ID, and the incident that the event belongs to. A [notice](../../docs/reference/commands.md#mmwatchdog-send-notice) and a [reboot notice](../../docs/how-it-works.md#reboot-notices) have no incident |
| `fleet_incident_key` | On every alert about an incident: 32 lowercase hexadecimal digits, the same for one check on every node of a fleet and on a standalone node, so a receiver can take the alerts of one check on many nodes for one incident ([one incident per check across the fleet](../../docs/changing-mmwatchdog/design/fleet.md#one-incident-per-check-across-the-fleet)). A notice, a reboot notice, and a maintenance alert have none |
| `node`, `check_group`, `check` | The node's name, and the check group and the check that the alert is about |
| `finding` | On an alert about one failed unit of a [`every_systemd_unit`](check_every_systemd_unit.md) check, that unit, such as `backup.service` |
| `type` | The change: `failed`, `recovered`, `cannot_check`, `can_check_again`, `failure_alert_severity_raised`, `cannot_check_alert_severity_raised`, `closed_without_recovery`, `maintenance_on`, `maintenance_off`, `rebooted`, `reminder`, or `notice` |
| `alert_severity` | `info`, `warn`, or `sos`. A recovery or a retirement carries the highest severity that the incident reached |
| `alert_severities_sent_for_incident` | On a recovery or a retirement, every severity that the incident's alerts went out with |
| `at` | When the change happened |
| `title`, `title_was_shortened`, `title_with_alert_severity`, `body`, `message` | The alert's [title and body](../../docs/reference/alert-format.md), and the readable explanation of the observation |
| `evidence` | On an incident's alerts, the observation that opened the incident ([evidence](../../docs/how-it-works.md#the-outbox-and-delivery)) |
| `no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails`, `number_of_checks_with_no_alerts_or_recovery_actions_while_this_check_fails` | On a failure, its raise, and its reminders: at most 20 checks that MMWatchdog keeps quiet while the alert's check fails, and the total number of quiet checks ([one cause, one alert](../../docs/how-it-works.md#one-cause-one-alert)) |
| `check_groups`, `maintenance` | On a maintenance alert: the check groups of the maintenance command, or `every_check_group` for the whole node, and the window's `until`, `reason`, and `user`. When the command names one check group, `check_group` holds the check group's name too. A `maintenance_off` alert says whether the window was turned off, expired, or ended because the window's check group was removed |
| `nodes`, `node_count` | On a message in which the MMWatchdog fleet hub grouped the alerts of several nodes: up to 20 nodes, and the total number of nodes ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)) |
| `destination` | On a reminder, the destination that the reminder is for |
| `send_alerts_only_to_destinations` | The destinations that the alert's check names ([which destination gets an alert](../../docs/how-to/send-alerts-somewhere.md#send-a-checks-alerts-to-chosen-destinations)), or the one destination that `mmwatchdog send-notice --destination` names |

A field without a value is left out, except `check`, which is an empty text
on an alert that is about no check. A destination with
[`send_only_the_alert_title: true`](../../docs/reference/alert-format.md)
gets an event without `body`, `message`, and `evidence`; `title`,
`title_with_alert_severity`, and the fields that identify the alert stay.

A receiver can get a recovery or a retirement whose failure it never got,
after a gap in delivery. Match the alerts of one incident by `incident_id`.

## When a delivery fails

An answer with a status code from `200` to `299` acknowledges the delivery.
Everything else is a failed attempt:

- any status code outside `200` to `299`, such as `401` or `503`, which
  MMWatchdog reports as `webhook returned HTTP 503`;
- a redirect, such as `301` or `307`: MMWatchdog does not follow it;
- no answer within `time_limit_per_delivery_attempt`, which is `5s` unless
  the destination states another time;
- a connection that does not open, or a certificate that MMWatchdog does not trust;
- a token file that MMWatchdog cannot read, or whose content is not one token.

MMWatchdog tries a failed alert again as it does for every destination: after one
second, then with the delay doubling up to 256 seconds, with an unchanged event
ID on each retry. The alerts of one destination go out in order, so the later
alerts wait behind the one that fails, and other destinations do not wait
for the failed alert. The [delivery](../../docs/how-it-works.md#the-outbox-and-delivery)
section of How MMWatchdog works has the rules for every destination, and
`mmwatchdog status` shows the destination as
[a destination that is failing](../../docs/when-something-fails.md#a-destination-is-failing).

## Examples

### Every alert to a receiver of the team's own

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  operations:
    webhook:
      url: https://alerts.example.com/mmwatchdog
      bearer_token_file: /etc/mmwatchdog/secrets/webhook   # holds one token; MMWatchdog reads it for each delivery
    remind_every: 4h
    alert_severities: [info, warn, sos]
```

The check `http` in the check group `myapp` fails on the standalone node
`web-3`. When the alert is due, MMWatchdog on `web-3`:

1. reads the token from `/etc/mmwatchdog/secrets/webhook`;
2. posts the event to `https://alerts.example.com/mmwatchdog`, with the token in the
   `Authorization` header and the event ID in `Idempotency-Key`;
3. counts the alert as delivered when the receiver answers `204`, or tries
   again after one second when it answers `503`.

While the incident is open, the receiver gets a `reminder` every four hours.
When the check recovers, the receiver gets a `recovered` event whose
`incident_id` matches the opening `failed` event's `incident_id`.

### A second receiver for `sos` alerts only

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  operations:
    remind_every: never
    webhook:
      url: https://alerts.example.com/mmwatchdog
      bearer_token_file: /etc/mmwatchdog/secrets/webhook
    time_limit_per_delivery_attempt: 5s
    alert_severities: [info, warn, sos]
  pager:
    remind_every: never
    webhook:
      url: https://pager.example.com/mmwatchdog   # takes no token
    alert_severities: [sos]
```

Every alert goes to `operations`. Only `sos` alerts go to `pager`, and the
recovery of an incident goes to `pager` when `pager` got the incident's
failure alert or an alert reporting the incident's severity increase to
`sos`. Each destination has its own queue: when `pager` does not answer,
`operations` still gets its alerts at once.

### A receiver that gets the title only

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  on_call_sms:
    remind_every: never
    webhook:
      url: https://sms-relay.example.com/mmwatchdog
    send_only_the_alert_title: true     # this destination gets the title and no body
    alert_severities: [sos]
```

The relay turns each alert into an SMS. The relay gets each `sos` alert
without `body`, `message`, and `evidence`, and sends
`title_with_alert_severity`, such as `SOS: web-3: myapp/http failed`.

## Test the destination

Quiet verification checks only the connection and server certificate policy. MMWatchdog sends
no HTTP request, so verification tests neither the URL path nor the bearer
token. Send a notice to test whether the receiver accepts a real alert.


Send a [notice](../../docs/reference/commands.md#mmwatchdog-send-notice) to the destination alone,
on the standalone node or on the MMWatchdog fleet hub that holds the destination.

```text
$ sudo mmwatchdog send-notice --alert-severity info --title "Test of the pager. Nothing is wrong." --destination pager
Notice sent to the destination pager:
  pager   delivered
```

The receiver gets an event of the type `notice` with the title
`web-3: Test of the pager. Nothing is wrong.` A failed attempt prints the
reason, such as `pager   failed: webhook returned HTTP 401`, and the command
exits 1. The notice stays queued, and MMWatchdog tries again.

## How it works

- **The token file is read for each delivery.** A new token in the configured token file
  needs no reload. The file holds one token of 1 to 4,096 bytes on one
  line; white space at the token's start and end does not belong to the
  token. `sudo mmwatchdog validate` reads the token file and names the file when the file is
  missing or holds anything else.
- **`url` takes `http://` or `https://`,** and never a user name or a
  password. With `https://`, MMWatchdog checks the receiver's certificate against
  the system trust store of the node. With `http://`, the event and the
  token cross the network unencrypted. Use `http://` only for a receiver
  on the node that sends the POST, or in a network that you trust.
- **In a fleet,** the destination is in the hub's `alert_destinations.yml`,
  and the hub posts the alerts of every node. A joined node posts an alert
  itself only when the hub did not accept it in time, with the token from
  its copy of the fleet settings and from its own place in the network
  ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)). After a new
  token, reload the hub, so the joined nodes get the new token.
- **A change of `url` or of the token contents in `bearer_token_file`** makes the
  destination a new one: a reload cancels the alerts queued for the destination
  ([delivery](../../docs/how-it-works.md#the-outbox-and-delivery)).

## Errors and what they mean

`trusted_certificate_authority_file` must be absolute. Certificate settings
have no use with an `http://` URL: remove them or choose `https://`.

| Error | What to do |
| --- | --- |
| `destinations.operations.webhook.url: expected HTTP(S) URL without credentials and with a host, such as https://example.com/health` | Write the address with `http://` or `https://`, and put a token in `bearer_token_file` instead of a user name and a password in the address |
| `destinations.operations.webhook.bearer_token_file: must be absolute` | Write the whole path, starting with `/` |
| `destinations.operations.webhook.bearer_token_file: /etc/mmwatchdog/secrets/webhook: expected a single token of 1..4096 bytes` | From `mmwatchdog validate` or a delivery attempt: put one token of 1 to 4,096 bytes on one line into the file |
| `failed: webhook returned HTTP 401` | The receiver refused the alert. Check the token in the file named by `bearer_token_file`, and the token expected by the receiver |
| `failed: MMWatchdog could not post the alert to the webhook: ...` | No answer: the name does not resolve, nothing listens at the address, the certificate is not trusted, or `time_limit_per_delivery_attempt` passed |

MMWatchdog writes the file and the line of the setting in front of a validation
error, such as `/etc/mmwatchdog/alert_destinations.yml:5:7:`.

## Tested on

- Go tests on macOS and natively on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution):
  `TestWebhookSenderContract` in `notify_test.go` (the `POST`, the
  `Content-Type`, `Idempotency-Key`, and `Authorization` headers, `2xx` as
  delivery, `503` and a redirect as failures, no redirect followed, a
  cancelled delivery), `TestWebhookSlowDestinationDoesNotBlockAnother`, and
  the tests of delivery, identity, and token files in `store_test.go`,
  `notify_test.go`, and `destination_test.go`.
- VM suites on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution):
  the acceptance, diagnostics, and fleet suites deliver failures,
  recoveries, reminders, and notices to a webhook receiver in the guest,
  and the fleet suite delivers through the hub and directly from a joined
  node with the token from its copy of the fleet settings.

## Why it works this way

- **Permission refusals belong to validation and reload.** Startup warns
  once per file and continues. Deliveries and check-ins keep reading
  secrets after a mode change, because refusing then would drop alerts.
  The [credential permission rule](../../docs/reference/configuration-files.md#secret-files)
  owns the settings and the full behavior.

- **Quiet verification sends no HTTP request.** A generic webhook has no
  standard request that tests its path or token without triggering work.
- **Certificate checks stay enabled by default.** A private authority can
  replace the system store; skipping identity checks requires an explicit
  dangerous setting.
- **Sent tokens can be one byte.** The receiving service owns the token's
  format and strength; MMWatchdog only bounds the file size and keeps one line.

- **A generic JSON webhook with a stable event ID and an `Idempotency-Key`
  header.** Any receiver or relay can read the JSON alert event and use
  the event ID to recognize a retry.
- **Redirects are not followed.** The destination is the address that the
  operator wrote, and a redirect could send the alert and its token
  elsewhere.
- **The token lives only in a file.** The token stays out of the
  configuration, version control, the address, and every output of
  MMWatchdog.
- **`webhook` is a core module.** `webhook` speaks a general protocol, so the module never
  needs a new `mmwatchdog` program when a company changes its service.
