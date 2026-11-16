# `pushover`

`pushover` delivers alerts to the phones and other devices of one person, or
of a group of people, through Pushover. `pushover` is an
[official extra module](../../docs/reference/modules/README.md): a
Starlark script that the MMWatchdog project writes, and that every deploy with the
release's MMX workflows installs on every node. An alert destination names
the module, and MMWatchdog runs the module once for each alert of the destination.

[Pushover](https://pushover.net) is a service that shows notifications in
its app on phones, tablets, and desktops. The module sends each alert
through Pushover's [Message API](https://pushover.net/api) to a Pushover
**user**, one person, or to a **delivery group**, several people such as an
on-call team. An `sos` alert goes out at Pushover's **emergency priority**:
Pushover repeats the emergency message, with sound, until somebody
acknowledges the message in the app.
When the check has recovered on every node, MMWatchdog sends one closing alert,
which stops the repeats.

Choose `pushover` for alerts that must wake a person on a phone. For
on-call schedules and escalation, use the core module
[`mmhotline`](../../docs/reference/modules/mmhotline.md); for a team's chat, use
[`slack`](../slack/README.md) or [`telegram`](../telegram/README.md).

## Set up

Secret files follow the
[credential permission rule](../../docs/reference/configuration-files.md#secret-files).
Place permission settings beside `pushover`, where `alert_severities` stands.

The node that delivers the alert needs:

- **Nothing but MMWatchdog.** The module sends the message through MMWatchdog itself, so the
  node needs no other program.
- **A way out to `api.pushover.net` over HTTPS.**
- **In a fleet, the module on every node, not only on the MMWatchdog fleet hub.** A
  joined node that delivers an alert without the hub runs its own copy
  ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)). The MMX
  workflows of the MMWatchdog release install every official extra module on every
  node, in `/usr/local/lib/mmwatchdog/official_extra_modules`.

Pushover needs an application API token and a user or group key from
MMWatchdog. The application API token and the user or group key are secrets:

- **The API token of a Pushover application.** An application is the sender
  that Pushover shows above each message, with its name and its icon.
  Log in at [pushover.net](https://pushover.net), register an application
  at [pushover.net/apps/build](https://pushover.net/apps/build), and name it
  so that the person sees where an alert comes from, such as `MMWatchdog`. Pushover
  then shows the application's API token, 30 letters and digits. Anybody who
  has the token can send messages in the name of the application.
- **The key of the person or of the group that gets the alerts.** The user
  key of a person is on the front page of pushover.net after that person
  logs in. For several people, make a delivery group at
  [pushover.net/groups/build](https://pushover.net/groups/build), add the
  people, and take the group key instead. A user key and a group key look
  alike, 30 letters and digits. Anybody with the key and an application of
  the sender's own can send messages to the named user or delivery group's devices.
  Pushover asks to keep the key private.

On the standalone node or MMWatchdog fleet hub, save the application token
and user or group key in separate files. Only root reads the
credential files (mode `0600`). Never write the application API token or
user or group key into `alert_destinations.yml`:

```sh
sudo install -m 0600 /dev/null /etc/mmwatchdog/secrets/pushover-application-token
sudo nano /etc/mmwatchdog/secrets/pushover-application-token    # paste the application's API token, one line
sudo install -m 0600 /dev/null /etc/mmwatchdog/secrets/pushover-on-call-group-key
sudo nano /etc/mmwatchdog/secrets/pushover-on-call-group-key    # paste the user key or the group key, one line
```

Each person installs the Pushover app on the phone and logs in with their
own Pushover account: the account whose user key is in the file, or one of
the accounts of the delivery group. After a trial of 30 days, the app is a
one-time purchase for each platform, such as Android or iOS
([Pushover's prices](https://pushover.net/pricing)).

MMWatchdog reads the application-token and user-or-group-key files for
every delivery. MMWatchdog gives the module the file contents without the
final line break, so the module needs no access to the credential files.
A new key in the credential file needs no reload on the machine reading
the file. In a fleet, reload the hub after changing a credential file.
The hub sends the credential files' contents to joined nodes and reads
the files again only at startup and reload. The module sends the
application API token and user or group key in request bodies to Pushover,
never in request addresses. No command argument or delivery message from
the module or MMWatchdog contains the application API token or user or group key.
[Destination modules](../../docs/reference/writing-modules.md#destination-modules) has the rules
that every such file follows.

### Make an alert wake the person

A notification that arrives does not prove that it wakes anybody. On a
muted phone, a Pushover alert arrives without a sound, and Pushover's
default sound is a soft bell. MMWatchdog sends every `sos` alert at
Pushover's emergency priority, so each person sets up the Pushover app on
their phone to sound loudly for emergency-priority messages.

On an iPhone, in the Pushover app:

1. In the app's settings, turn on **Critical Alerts** for emergency
   priority. iOS then asks whether Pushover may send critical alerts:
   grant Pushover permission to send critical alerts. After granting the
   iOS permission, nothing needs changing in the iPhone's own Settings app.
2. Leave Critical Alerts for high priority as they are. MMWatchdog sends no
   alert at high priority: `info` goes at low priority, `warn` at normal
   priority, and `sos` at emergency priority.
3. In the app's sound settings, choose an alarming sound and raise its
   volume. Pushover plays a critical alert at the volume set in the app,
   not at the phone's own volume.

On an Android phone, Pushover's article
[Overriding Do Not Disturb on Android](https://support.pushover.net/i258-overriding-do-not-disturb-on-android)
shows how to let the app sound through Do Not Disturb. The steps differ
from one phone maker to another. Choose an alarming sound in the Pushover
app on the Android phone too.

Then test each phone at `sos`, once muted and once with Do Not Disturb on,
as [Test the destination](#test-the-destination) shows.

## Inputs

`mmwatchdog validate` and reload check the optional API address before contacting
the service. An invalid address is refused at its input line. Credential
file contents are checked when MMWatchdog uses those files.

| Input | Meaning | When omitted |
| --- | --- | --- |
| `application_token_file` | The absolute path of a file that holds the API token of the Pushover application, such as `azGDORePK8gMaC0QOYAMyEEuzJnyUi`, and nothing else | Required: `mmwatchdog validate` refuses the destination |
| `user_or_group_key_file` | The absolute path of a file that holds the user key of the person or the group key of the delivery group that gets the alerts, such as `uQiRzpo4DXghDmr9QzzfQu27cmVRsG`, and nothing else | Required: `mmwatchdog validate` refuses the destination |
| `repeat_sos_alerts_every` | How often Pushover repeats an `sos` alert on the devices until somebody acknowledges it, such as `5m`. At least `30s` | Required: `mmwatchdog validate` refuses the destination |
| `stop_repeating_sos_alerts_after` | How long Pushover repeats an `sos` alert that nobody acknowledges, such as `1h`. At most `3h`, and at least as long as `repeat_sos_alerts_every` | Required: `mmwatchdog validate` refuses the destination |
| `api_server_url` | The address of a server that answers as Pushover's API server does, such as a stand-in in a test | The module uses Pushover's own server, `https://api.pushover.net` |

Write `repeat_sos_alerts_every` and `stop_repeating_sos_alerts_after` as
durations, like `remind_every: 4h`. MMWatchdog accepts durations such as
`90s`, `5m`, `1.5h`, `1h30m`, or `1d`. `mmwatchdog validate` refuses a value
that is no time, such as `300`. Pushover takes whole seconds, so the
module drops a fraction of a second.

The module checks Pushover's repeat interval and expiration limits at every
delivery, whatever the alert's severity. A test with an `info` notice shows
a value outside Pushover's repeat limits before the first `sos` alert.
Pushover repeats a message at most 50 times: with
`repeat_sos_alerts_every: 30s`, the repeats end after 25 minutes, whatever
`stop_repeating_sos_alerts_after` says.

Beside the inputs, a destination can state the run settings that every
destination module takes, such as `run_as_user_account`
([destination modules](../../docs/reference/writing-modules.md#destination-modules)).
`mmwatchdog describe-module pushover` prints the inputs on a node.

## What it sends

Each alert is one Pushover message. The Pushover message's title is the
alert's title with the alert's severity in front, and the message below it
is the alert's body
([title and body of an alert](../../docs/reference/alert-format.md)).
MMWatchdog writes the title and body, so the alert reads in Pushover as
the alert reads everywhere else.
This is the message of a failed check with `alert_severity: sos` on the
node `app-1`:

```text
SOS: app-1: myapp/http failed
expected HTTP 200, received 503
failing since 14:02 UTC
```

The first line is the message's title, which Pushover shows above the
message. The recovery of `myapp/http` on `app-1` has the title
`Resolved SOS: app-1: myapp/http is ok again`.

The alert's severity sets the message's
[priority](https://pushover.net/api#priority), which decides how the
device announces the message:

| Alert | Priority | What the device does |
| --- | --- | --- |
| Every `sos` alert: a failure, a raise to `sos`, a check that MMWatchdog cannot run, a reminder, a reboot notice, and a [notice](../../docs/reference/commands.md#mmwatchdog-send-notice) | Emergency | The device plays a sound, also in the person's quiet hours, and repeats the message every `repeat_sos_alerts_every` until somebody acknowledges the message in the app, or until `stop_repeating_sos_alerts_after` has passed |
| The closing alert of an `sos` check, which MMWatchdog sends when the check has recovered on every node: a recovery, `MMWatchdog can check again`, or an incident that MMWatchdog closed without knowing whether the problem is fixed | Normal, after the alert has stopped the repeats | The device plays the configured sound, except in the person's quiet hours |
| Every `warn` alert | Normal | The device plays the configured sound, except in the person's quiet hours |
| Every `info` alert, including every maintenance alert | Low | The device shows the message without a sound and without vibration |

**The closing alert stops the repeats.** Every emergency message about a
check carries the tag of the check, which Pushover keeps with the message:
`mmwatchdog_fleet_` and the alert's `fleet_incident_key`, such as
`mmwatchdog_fleet_5925e47138c3e039dd7d115ca91fefaf`. MMWatchdog gives every alert about one check
the same `fleet_incident_key`, 32 hexadecimal digits: on every node of a
fleet, on a standalone node, and in a message where the fleet hub grouped
alerts from several nodes. A check's failures, cannot-check incidents, and
each finding, such as one failed systemd unit, have separate keys.

The module's `module_description.yml` says `service_keeps_incidents: true`.
So MMWatchdog sends a `pushover` destination the failures, the raises, and the
reminders of every node, and the end of the check's incidents only once for
each key: when the check has recovered on every node whose alerts went to
the destination. When an `sos` alert of the
check went to the destination, the module first asks Pushover to stop
repeating every message with the check's tag. Then the module sends the
closing message, such as
`Resolved SOS: app-1: myapp/http is ok again`. When several nodes
failed, the closing alert is about the check, with the title
`Resolved SOS: myapp/http is ok on every node again`, and its body names the
nodes, such as
`myapp/http is ok on every node again (was failing on app-2, app-3)`.
A person who has not yet acknowledged the failure hears no more repeats, and
reads that the check is ok again.

- **A raise to `sos`** of a `warn` failure is the incident's first
  emergency message, and the closing alert stops it.
- **A reminder of an `sos` incident,** for a destination with
  `remind_every`, starts repeats of its own, with the check's tag, so a
  person who acknowledged the failure is alerted again while the incident
  lasts. The reminder stops nothing: it goes to one destination, and a stop
  by tag would also silence every other destination with the same
  application token. While the failure message still repeats, Pushover
  repeats the failure message and the reminder independently. The closing
  alert stops the repeats of the failure message and the reminder.
- **A notice and a reboot notice at `sos`** have no incident. The repeats
  of a notice or reboot notice end when somebody acknowledges the message,
  or after `stop_repeating_sos_alerts_after`.
- **The MMWatchdog fleet hub's message about several nodes,** such as
  `SOS: myapp/http failed on 7 nodes`, carries the tag of the check too
  ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)). The closing
  alert stops the hub's grouped message from repeating, along with every
  other message about the check.
- **A node that recovers while another node still fails** sends the
  destination nothing, so the repeats of the check's messages go on while
  any node fails, until somebody acknowledges them or
  `stop_repeating_sos_alerts_after` has passed.
- **When Pushover does not stop the repeats,** the module still sends the
  recovery and answers `delivered`. Nothing reports the failed stop: MMWatchdog
  shows no text of a delivered alert. The repeats then end when somebody
  acknowledges the earlier message in the app, or after
  `stop_repeating_sos_alerts_after`.
- **The stop reaches every destination with the same application token.**
  So the closing alert stops the repeats only when an `sos` alert of the
  check went to its own destination: MMWatchdog lists in the closing alert's
  `alert_severities_sent_for_incident` the severities that went to that
  destination alone. Take two destinations with one token, `on_call_warnings`
  with `alert_severities: [warn]` and `on_call_phones` with `[sos]`. On
  `app-1` the check fails at `warn` and is raised to `sos`; on `app-2` it
  fails at `sos`. When `app-1` recovers, `on_call_warnings` gets its closing
  alert and stops nothing, so the emergency message of `app-2` on
  `on_call_phones` repeats until `app-2` recovers too. Each destination that
  got alerts about the check gets a closing alert. The person or delivery
  group named by each destination receives the message that the check is
  ok again.

More about the message:

- **A destination with `send_only_the_alert_title: true`,** and a notice
  without a body, send the title as the message, and Pushover shows the
  application's name above it, such as `MMWatchdog`. Pushover takes no message
  without text.
- **The body is in Pushover's monospace font,** which shows the output of a
  check with its columns, as it is. Pushover reads no formatting in such a
  message, so `<b>`, `*`, and `_` in the output of a check stay those
  characters. A web address in the body becomes a link that the person can
  tap. The notification on the lock screen shows the text in the device's
  ordinary font; the app shows the monospace font when the person opens the
  message.
- **Nobody is mentioned.** A message reaches the person or every member of
  the delivery group, whatever the text says.
- **A message has at most 1,024 characters, and a title at most 250,** the
  most that Pushover takes. The module cuts a longer body to 1,024
  characters and marks the cut with `…`. An alert's title with its severity
  has at most 138 characters, so the title is never cut.
  `sudo mmwatchdog events` on the node has the
  whole alert.

## When a delivery fails

Pushover answers a message that it took with the HTTP status code 200 and
`"status": 1`. Every other answer, and every failure to reach Pushover, is a
failed attempt. The module then answers `failed` with a message that says
what Pushover answered and what to do. MMWatchdog keeps the alert and tries again,
as for every destination: after one second, then with the delay doubling up
to 256 seconds ([destination modules](../../docs/reference/writing-modules.md#destination-modules)).
Each attempt runs the module again.

- **A refusal of Pushover,** with a status code that starts with 4, continues
  until the file or input is correct. MMWatchdog still tries again, so
  queued alerts go out once the credential file holds the right key,
  without a reload. **Correct the credential file soon:** Pushover blocks
  a machine that sends many refused requests within a few minutes. The
  first block lasts an hour, with the answer 429. Each later block lasts
  longer. While the block lasts, no alert of the blocked node reaches
  Pushover. Test a new destination with `mmwatchdog send-notice`, as
  [test the destination](#test-the-destination) shows, so a wrong key shows
  at once.
- **The monthly limit.** A Pushover account sends 10,000 messages a month,
  and a team account 25,000; the applications of the account share them.
  Beyond the limit, Pushover answers 429 until the first day of the next
  month, 00:00 Central Time, or until more messages are bought. A stop of
  the repeats is no message.
- **An error of Pushover,** with a status code that starts with 5, passes.
  MMWatchdog tries again.

While the attempts fail, `mmwatchdog status` shows the destination with the
module's message after the words `the module pushover reported:`
([a destination that is failing](../../docs/when-something-fails.md#a-destination-is-failing)).
[Errors and what they mean](#errors-and-what-they-mean) lists each message.

## Examples

### Every `warn` and `sos` alert to the on-call group

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  on_call_phones:
    remind_every: never
    pushover:                                                                # the module
      application_token_file: /etc/mmwatchdog/secrets/pushover-application-token    # the file that holds the application's API token
      user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-on-call-group-key    # the file that holds the delivery group's key
      repeat_sos_alerts_every: 5m                                           # an sos alert sounds again every five minutes
      stop_repeating_sos_alerts_after: 1h                                   # for at most one hour
    alert_severities: [warn, sos]
```

The destination `on_call_phones` names the module `pushover` and writes the
module's inputs below that name. What happens when a check with
`alert_severity: sos` fails at 03:10, on the standalone node or on the hub
that holds `/etc/mmwatchdog/alert_destinations.yml`:

1. The alert's severity is in `alert_severities`, so MMWatchdog queues one delivery
   for `on_call_phones`.
2. MMWatchdog reads `/etc/mmwatchdog/secrets/pushover-application-token`
   and `/etc/mmwatchdog/secrets/pushover-on-call-group-key`.
   MMWatchdog runs `pushover` with the alert, inputs, and credential file
   contents.
3. The module sends the message at emergency priority, with the tag of the
   check. Pushover answers `"status": 1`, and the module answers
   `delivered`.
4. The phones of every member of the delivery group sound, at 03:10, 03:15,
   and every five minutes after, until one person acknowledges the message
   in the app. A delivery group member's acknowledgement of the message
   stops the repeats for the whole delivery group.
5. When the check has recovered, MMWatchdog runs the module again. The module stops
   the repeats of the check's messages, if they still go on, and sends
   `Resolved SOS: app-1: myapp/http is ok again` at normal priority.

A `warn` alert reaches the group at normal priority, once. `info` alerts do
not reach the group.

### One person's phone for every alert, with reminders

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  barbaras_phone:
    pushover:
      application_token_file: /etc/mmwatchdog/secrets/pushover-application-token
      user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-barbara-user-key     # Barbara's user key
      repeat_sos_alerts_every: 2m
      stop_repeating_sos_alerts_after: 30m
    remind_every: 4h                                                        # an incident that lasts is sent again every four hours
    alert_severities: [info, warn, sos]
```

Barbara's phone gets every alert: an `info` alert without a sound, a `warn`
alert with a sound, and an `sos` alert every two minutes for half an hour,
until she acknowledges it. While an `sos` incident lasts, MMWatchdog sends a reminder
every four hours, and the reminder repeats as the failure did, so Barbara is
alerted again even after she acknowledged the failure. The recovery stops
the repeats of the failure and of every reminder. The application token can
be the same for several destinations.

### The title only

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  on_call_watch:
    remind_every: never
    pushover:
      application_token_file: /etc/mmwatchdog/secrets/pushover-application-token
      user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-on-call-group-key
      repeat_sos_alerts_every: 1m
      stop_repeating_sos_alerts_after: 20m
    send_only_the_alert_title: true                                         # one line for each alert
    alert_severities: [sos]
```

Each `sos` alert arrives as one line, such as
`SOS: app-1: myapp/http failed`, below the application's name, for a person
who reads the alerts on a watch. The whole alert stays in `sudo mmwatchdog events` on
the node.

## Test the destination

`mmwatchdog send-notice` sends a message through the destination as a real alert
goes, and prints what happened. Run `mmwatchdog send-notice` on the
standalone node or on the hub:

```sh
sudo mmwatchdog send-notice --alert-severity info --title "Test of the on-call phones. Nothing is wrong." --destination on_call_phones
```

```text
Notice sent to the destination on_call_phones:
  on_call_phones   delivered
```

The devices then show `Info: app-1: Test of the on-call phones. Nothing is
wrong.` below the application's name, without a sound. The destination
takes only `warn` and `sos`, but a notice that names the destination reaches
it whatever its `alert_severities` say. A test at `info` also checks the two
times of the repeats.

To test that an `sos` alert wakes each person, send the notice with
`--alert-severity sos`, twice: once while the phone is muted (Silent Mode
on an iPhone), and once while it has Do Not Disturb on.

```sh
sudo mmwatchdog send-notice --alert-severity sos --title "Test of the on-call phones at night. Nothing is wrong." --destination on_call_phones
```

Each time, check that the phone sounds loudly enough to wake the person,
then acknowledge the test notice in the app. Until somebody acknowledges
the test notice, the phone sounds again every `repeat_sos_alerts_every`.
A notice has no incident, so nothing else stops the notice's repeats.
A message that arrives without a sound, or with a quiet sound, fails the test.
Set up the phone again as
[Make an alert wake the person](#make-an-alert-wake-the-person) says.

When the delivery fails, the command prints the module's message after
`failed:` and keeps the notice queued.

### Verify without a message

MMWatchdog posts the application token and user or group key as a form to
`/1/users/validate.json`. This is Pushover's user-validation request, not a
message, so it uses none of the monthly message quota. A 4xx response with
`status: 0` and `token: invalid` or `user: invalid` fails verification.
Pushover also checks whether the user has an active device.

A connection or certificate failure, or any HTTP 5xx answer, also fails
verification. Every other answer passes, including a rate limit or a reply
whose meaning has not been confirmed. The verification rules avoid a false
failure from a service response that does not prove a bad credential.
A successful verification does not replace a test notice for the full delivery path.

## How it works

One delivery attempt is one run of the script `module.star`, under the
destination's `time_limit_per_delivery_attempt`:

1. MMWatchdog reads the files that `application_token_file` and
   `user_or_group_key_file` name and starts the script as the destination's
   `run_as_user_account`, `mmwatchdog` unless stated.
2. The module checks that each file's content has the form of a Pushover
   key: 30 letters and digits. During configuration validation, MMWatchdog
   has already checked `repeat_sos_alerts_every`,
   `stop_repeating_sos_alerts_after`, and `api_server_url`.
3. For a closing alert whose `alert_severities_sent_for_incident` holds
   `sos`, or, without that list, whose `alert_severity` is `sos`, the module
   first sends an HTTP `POST` to
   `<server>/1/receipts/cancel_by_tag/<tag>.json`, a form with the
   application's token. The tag is `mmwatchdog_fleet_` and the alert's
   `fleet_incident_key`. What Pushover answers does not decide the delivery.
4. The module uses `http_request`, MMWatchdog's function for making HTTP
   requests. The module sends an HTTP `POST` of JSON to
   `<server>/1/messages.json`. The JSON contains the application's token,
   the key, the title, the message, `monospace`, and the priority. An
   emergency message also carries `retry` and `expire`, the two times in
   seconds, and, when the alert has a `fleet_incident_key`, `tags`, the tag
   of its check. `http_request` follows no redirect.
5. The module answers `delivered` for the status code 200 with
   `"status": 1`, and `failed` for every other answer, with Pushover's
   `errors` on one line. MMWatchdog shows a module's message as one line of
   at most 300 bytes. Pushover's error text is limited to 200 characters
   and to the bytes remaining after the module's own words within the
   300-byte message.

MMWatchdog runs the module for the end of a check's incidents only once for
each key, when the check has recovered on every node, because
`module_description.yml` says `service_keeps_incidents: true`. The module
asks nothing and keeps nothing. Each alert's type, key, and severity
determine the module's requests.

## Errors and what they mean

| Message | What the message means and what to do |
| --- | --- |
| `destinations.on_call_phones.pushover.repeat_sos_alerts_every: is required: How often Pushover repeats an sos alert on the devices until somebody acknowledges it, ...` | `mmwatchdog validate`: the destination leaves out an input. Every input but `api_server_url` is required |
| `destinations.on_call_phones.pushover.repeat_sos_alerts_every: expected a duration such as 30s, 5m, 2h, or 7d` | `mmwatchdog validate`: the time has no unit, such as `300`. Write `5m` |
| `destinations.on_call_phones.pushover.sound: unknown field "sound": the inputs of the module pushover are application_token_file, user_or_group_key_file, repeat_sos_alerts_every, stop_repeating_sos_alerts_after, api_server_url` | `mmwatchdog validate`: the destination states an input that the module does not have. Each person chooses the sound in the Pushover app |
| `Pushover answered 400: application token is invalid, see https://pushover.net/api. Pushover does not know this application token. Copy it from pushover.net into the file that application_token_file names.` | The token in the file is wrong, or the application was deleted. Copy the token from the application's page on pushover.net |
| `Pushover answered 400: user identifier is invalid. Pushover does not know this user or group key, or its user has no active device. Copy the key from pushover.net into the file that user_or_group_key_file names.` | The key in the file is wrong, the group was deleted, or the person has no device with the app. Check the key, and that the app is installed and logged in |
| `Pushover answered 400: message cannot be blank.` | Another refusal: the text after `400:` is Pushover's own words |
| `Pushover answered 429: application has exceeded its monthly limit of 10000 messages, see https://support.pushover.net/i13. The account has used up its messages of this month. Pushover takes messages again on the 1st of next month, or after more are bought.` | The account has used its messages of the month. Buy more on pushover.net, or wait for the first day of the next month. MMWatchdog tries again |
| `Pushover answered 429: ... Pushover takes no messages for now: the account used up its messages of this month, or Pushover blocks this node after many refused requests.` | Pushover blocks the node after many refused requests, or the account reached its monthly limit; Pushover's words after `429:` say which. Correct what Pushover refused before, such as a key, and wait for the block to end |
| `the API server answered 503 without Pushover's JSON: Service Unavailable. Pushover may have a temporary problem; if it lasts, check api_server_url and what stands between this node and that server.` | Pushover, or something between the node and Pushover such as a proxy, answered with an error. The text after the status code is the first line of the HTTP 503 response. Nothing to do unless the HTTP 503 error lasts |
| `MMWatchdog could not send the message to Pushover: dial tcp: lookup api.pushover.net: no such host` | The node did not reach Pushover. The text after `Pushover:` says why: the node could not look up the name `api.pushover.net` |
| `the file that application_token_file names must hold one API token of a Pushover application, 30 letters and digits such as azGDORePK8gMaC0QOYAMyEEuzJnyUi, and nothing else` | The file holds something other than the token. Write the token into the file, on one line. `user_or_group_key_file` has a matching message for the key |
| `repeat_sos_alerts_every: set repeat_sos_alerts_every to at least 30s; Pushover repeats a message no more often than every 30 seconds` | `mmwatchdog validate` and reload refuse the input. Set repeat_sos_alerts_every to at least `30s` |
| `stop_repeating_sos_alerts_after: set stop_repeating_sos_alerts_after to at most 3h; Pushover repeats a message for at most 3 hours` | `mmwatchdog validate` and reload refuse the input. Set stop_repeating_sos_alerts_after to at most `3h` |
| `stop_repeating_sos_alerts_after: set stop_repeating_sos_alerts_after to at least repeat_sos_alerts_every, or Pushover never repeats an sos alert` | `mmwatchdog validate` and reload refuse the input. Make stop_repeating_sos_alerts_after at least repeat_sos_alerts_every |
| `api_server_url: use one web address starting with https:// or http://, such as https://api.pushover.net; this is the service's API address` | Correct `api_server_url`, or leave it out, and reload |
| `MMWatchdog cannot read the file of the input user_or_group_key_file of the module pushover: open /etc/mmwatchdog/secrets/pushover-on-call-group-key: no such file or directory` | MMWatchdog could not read the file, so it did not start the module. Write the file as [set up](#set-up) shows. `sudo mmwatchdog validate` finds a missing file before a deploy |

After you correct a file, the next attempt delivers the queued alerts. In a
fleet, reload the hub first, as [set up](#set-up) says. After you correct an
input, reload. The errors that every destination module can meet, such as a
run past the time limit or a module that is not installed on the node, are
in [destination modules](../../docs/reference/writing-modules.md#destination-modules).

## Tested on

- **Go tests against a stand-in for Pushover**
  (`tests/official_extra_modules/pushover_test.go`). The stand-in Go tests
  build `mmwatchdog` and run the script through
  `mmwatchdog run-starlark-module`, as MMWatchdog does. An HTTP server
  inside the tests answers as Pushover's API server does.

  The stand-in Go tests check the exact JSON for `sos`, `warn`, and `info`
  alerts, a raise to `sos`, a cannot-check alert, a notice and reboot notice
  at `sos`, a maintenance alert, the hub's grouped message with the check's
  tag, a reminder, an alert with only a title, and an `sos` failure without
  `fleet_incident_key`, which has no tag.

  Cancellation coverage checks that repeats stop by tag before recovery,
  `MMWatchdog can check again`, and closure without knowing whether the
  problem is fixed. The tests also check cancellation before a closing
  alert after the last of several nodes recovers. The multi-node closing
  alert has no node and no incident. The tests check
  that a reminder sends no stop request, and that a closing alert sends
  none to a destination that got only `warn` alerts of the check, even
  after the check reached `sos` elsewhere.

  The stand-in Go tests check delivery of a recovery message when
  cancellation fails, and failed recovery delivery after cancellation
  succeeds. Text coverage includes quotes, `&`, `<`, characters outside
  ASCII, and truncation at 1,024 message characters and 250 title
  characters.

  Error coverage includes a wrong token, a wrong key, another refusal,
  the monthly limit, a node block, and a Pushover error. The tests also
  cover a page from another server, an unfollowed redirect, and an address
  with no server listening. Every message fits MMWatchdog's single
  line of 300 bytes, including messages with long Pushover error text.

  Input coverage includes a file with no key, every form of MMWatchdog's
  duration notation, durations outside Pushover's limits, and an invalid
  server URL. The tests check Pushover's default server, the override with
  `api_server_url`, and input validation. The tests also check that no
  output contains the application token or user or group key. The stand-in
  Go tests ran with `go test ./...` on macOS on ARM64.
- **The acceptance suite on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)**
  (`tests/integration/official_extra_destination_module_scenarios.py`). The
  module is installed in `/usr/local/lib/mmwatchdog/official_extra_modules`, runs as
  the account `mmwatchdog`, and sends over HTTPS to a stand-in for Pushover
  in the Linux virtual machine running the acceptance suite
  (`tests/integration/destination_services_stand_in.py`): a real
  check's `sos` failure sends the emergency message with the tag
  `mmwatchdog_fleet_<fleet_incident_key>` from the real event, and its recovery
  stops the repeats by that tag and then sends the recovery message.
  `mmwatchdog send-notice` prints `delivered`. A refused application token fails
  with Pushover's answer in `mmwatchdog send-notice` and `mmwatchdog status`, and the queued
  notice arrives once the file holds the right token, without a reload. No
  output of MMWatchdog and no line of its journal holds the token or the user or
  group key.
- **The phone setup** of
  [Make an alert wake the person](#make-an-alert-wake-the-person), on an
  iPhone. Without Critical Alerts, an alert arrived without a sound on the
  muted phone. With Critical Alerts on for emergency priority, an
  emergency-priority message sounded with Silent Mode on, and another with
  Do Not Disturb on. The Android setup comes from Pushover's article and was
  not tried.

- **The real service test against Pushover itself**
  (`tests/official_extra_modules/real_services_test.go`), which runs on a
  Mac before each release
  ([real services before a release](../../docs/changing-mmwatchdog/testing.md#real-services-before-a-release)).
  Two nodes sent an `sos` alert of one check with the same tag, and
  Pushover repeated both emergency messages after 30 seconds. The closing
  alert stopped the emergency messages from the two nodes with a single
  cancellation request by tag. Pushover answered the cancellation request
  with `"canceled":2`, and neither emergency message was delivered again.
  A cancellation request for a tag attached to no message returned HTTP
  200 with `"canceled":0`. The closing message after the cancellation
  request was delivered. Pushover took a message of 1,024 characters with
  49 emoji, at low priority. A wrong application token got `400 application
  token is invalid` and the module's advice. The verification accepted the
  real keys and refused the wrong token, without a message
  ([validation record](../../docs/changing-mmwatchdog/validation.md#real-alert-destinations)).

Not verified: that one person's acknowledgement stops the repeats for a
whole delivery group, because the test group has one member; how many
refused requests make Pushover block a node, which no test should provoke;
and Pushover's rate limits ([validation record](../../docs/changing-mmwatchdog/validation.md#not-verified-for-this-build)).

## Why it works this way

- **Input values are checked before delivery.** Configuration validation
  catches an invalid API address before an alert needs it. The module checks
  file contents only when MMWatchdog supplies them at runtime.

- **Verification sends no alert.** The user-validation request checks the keys and active devices without sending or charging for a message.

- **`sos` is Pushover's emergency priority.** `sos` means wake someone, and
  only the emergency priority keeps sounding until a person answers. `warn`
  is normal and `info` low, so a phone that gets every severity sounds only
  for what needs a person.
- **The repeat times are required.** Pushover requires
  `repeat_sos_alerts_every` and `stop_repeating_sos_alerts_after` for an
  emergency message. How long a phone should keep sounding depends on the
  team. A person who sleeps through a sound needs another sound soon. A
  team with a second person on call may stop sooner.
- **The closing alert stops the repeats by the check's tag.** Pushover can
  stop every message with a tag, so the module keeps no receipt of Pushover
  between two deliveries. The tag is `mmwatchdog_fleet_` and the alert's
  `fleet_incident_key`.
  The key is the same for every alert about the check, on every node, in
  the hub's grouped messages, and in every way an alert goes. The key holds
  only hexadecimal digits, so no node or check name can make Pushover
  refuse the tag.
- **MMWatchdog, not the module, sends the closing alert when the last node
  recovers.** The MMWatchdog fleet hub knows the incidents of every node, and the
  module sees one alert. A stop by the check's tag at the recovery of one
  node would silence the messages of nodes that still fail, so the module's
  description says `service_keeps_incidents: true`.
- **A failure to stop the repeats never fails the recovery.** A recovery
  that waited for the stop would hold back every later alert of the
  destination, and a phone that keeps sounding costs a person one tap.
- **Only the closing alert stops the repeats, and only when an `sos` alert
  went to its destination.** A stop by tag reaches every destination with
  the same application token, and each destination that got alerts about
  the check gets the closing alert. A reminder that stopped the repeats
  would silence the others while the incident lasts, and so would the
  closing alert of a destination that got only `warn` alerts while another
  destination still repeats an `sos` alert of a node that fails. Two
  repeating messages of one incident are a duplicate, which is better than
  a person who is no longer alerted.
- **The recovery is a message of its own, at normal priority.** A person who
  heard the repeats should also read that the incident is over.
- **The body is in the monospace font.** The alert body preserves the
  check output's columns, and Pushover reads no formatting in the alert
  body. Pushover's HTML would need every `<` of a check's output written
  another way.
- **MMWatchdog writes the title and body. The module copies the title and
  body into Pushover's corresponding message fields.** An
  alert reads the same in Pushover, in an email, and in MMHotline, and no
  module writes wording of its own. A team that wants other wording writes a
  module of its own, which receives the whole alert event.
- **No input for a sound or a device.** An emergency message already
  sounds in the person's quiet hours and repeats, with the sound that the
  person chose in the Pushover app, and a message reaches every device of
  the person, so no alert misses the one device that the person carries.
- **`api_server_url` exists.** Pushover's server has a fixed address, and the
  input lets MMWatchdog's tests reach a stand-in.
- **The user key or group key lives in a file, like the token.** With the key and an application of the sender's own,
  anybody can send messages to the named user or
  delivery group's devices, including emergency messages. So the user or
  group key stays out of configuration, version control, command lines,
  and output.
- **`pushover` is an official extra module, not a core module.** Pushover is
  a service that one company runs, and Pushover changes with the company.
  A script can follow Pushover in a new release without a new
  `mmwatchdog` program.
- **The module sends with MMWatchdog's primitive `http_request`.** A node needs
  nothing but MMWatchdog to deliver an alert: no `curl` and no other tool.
- **The module keeps nothing between two deliveries.** MMWatchdog's outbox owns
  the retries, the order, and the gaps. No answer from the module stops
  retries: a mistaken "do not try again" answer would lose an alert.
