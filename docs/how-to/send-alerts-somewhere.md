# Send alerts where your team sees them

**When to use this page:** you want MMWatchdog's alerts in one more place,
such as a mailbox, a chat channel, a receiver of your own, or the phone of
the person on call; or you want a place to get only some of the alerts.

**Who acts:** you, or your coding agent. Each step runs on your controller
machine, the machine where you run `mmx`, except where the step says "on
the node". A coding agent shows the person the output of `mmx dryrun` and
waits for their yes before `mmx run`, and asks the person to type each
password or token into the vault themselves.

**What this page does:** you add an **alert destination** to the MMX
project that installed MMWatchdog, put its password or token in MMX's
vault, install it on the node with MMX, and prove with a test message that
alerts arrive.

**Terms:** an **alert destination** is one named place that gets alerts,
such as `team_slack_channel`. Each destination names one **module**, the
code that delivers to one kind of service, such as `slack` or `email`. Each
alert has an **alert severity**: `info` for news that needs no action,
`warn` for a problem that someone should look at soon, and `sos` for a
problem that needs a person now. A **notice** is a one-time message, such
as a test or "deploy finished", that is not about a check.

## Choose what to do

| You want | Section |
|---|---|
| Alerts in a new place | [Add a destination](#add-a-destination) |
| One check's alerts only in some places, such as one person's channel | [Send a check's alerts to chosen destinations](#send-a-checks-alerts-to-chosen-destinations) |
| A one-time message, such as a test or "deploy finished" | [Send a notice](#send-a-notice) |
| To stop sending alerts to one place | [Remove a destination](#remove-a-destination) |
| The checks and restarts, but no alerts | [Run with no destination](#run-with-no-destination) |
| To know which messages come without a failing check | [Messages that are not about a check](#messages-that-are-not-about-a-check) |
| To know what happens when a destination cannot be reached | [When a destination cannot be reached](#when-a-destination-cannot-be-reached) |

## Choose the destination

| Alerts go to | Module | The secret you put in the vault | The module's page |
|---|---|---|---|
| A [Slack](https://slack.com/) channel | `slack` | The address of the channel's incoming webhook | [`slack`](../../official_extra_modules/slack/README.md) |
| Mailboxes, through a mail server | `email` | The password of the mail server's login, if it has one | [`email`](../reference/modules/email.md) |
| A receiver of your own, which reads each alert as JSON over HTTP | `webhook` | A bearer token, if the receiver takes one | [`webhook`](../reference/modules/webhook.md) |
| A [Telegram](https://telegram.org/) chat, through a bot | `telegram` | The bot's token | [`telegram`](../../official_extra_modules/telegram/README.md) |
| A [Discord](https://discord.com/) channel | `discord` | The address of the channel's webhook | [`discord`](../../official_extra_modules/discord/README.md) |
| A [Mattermost](https://mattermost.com/) channel | `mattermost` | The address of the channel's incoming webhook | [`mattermost`](../../official_extra_modules/mattermost/README.md) |
| Phones, through [Pushover](https://pushover.net/), which repeats an `sos` alert until somebody acknowledges it | `pushover` | The application's token and the user or group key | [`pushover`](../../official_extra_modules/pushover/README.md) |
| The person on call, through [MMHotline](https://github.com/monkeymaxxing/mmhotline), the on-call program that a team runs on its own server | `mmhotline` | The signing secret | [`mmhotline`](../reference/modules/mmhotline.md), and [MMHotline as a destination](#mmhotline-as-a-destination) below |
| The node's system log, the systemd journal, for a log collector that carries it further | `system_log` | None | [`system_log`](../reference/modules/system_log.md) |

`email`, `webhook`, `mmhotline`, and `system_log` are built into the
`mmwatchdog` program. The other modules ship with MMWatchdog, and every
`mmx run` installs them on the node. For another service, a receiver of
your own behind `webhook` can pass the alerts on, or you can
[write a destination module](write-your-own-check-or-destination.md).
Each module's page gives its inputs and how to get its address or token;
on the node, `mmwatchdog describe-module NAME` prints the same page.

## Before you start

- MMWatchdog runs on the node, installed with MMX from the MMX project on
  your controller machine. If not, follow
  [Install MMWatchdog on one node](install-on-one-node.md) first.
- Open a terminal in the MMX project: the directory that holds
  `mmx_project_settings.yml`.
- You have the address and the secret of the destination, such as the
  receiver's address and its token. The module's page says how to get them.
- The node can reach the destination over the network, such as a web
  address over HTTPS, or a mail server on its port.

## Add a destination

This example adds `on_call_receiver`: a receiver of the team's own at
`https://alerts.example.com/mmwatchdog`, which takes a bearer token and
gets only `sos` alerts. The project already has the Slack destination of
[Install MMWatchdog on one node](install-on-one-node.md). If your project
has no destination and no vault yet, the steps are the same: step 1
creates the vault, and step 3 adds the first destination; write Slack's
inputs from the whole `alert_destinations.yml` of that page. For another
module, write its inputs from its page in step 3; the other steps stay the
same.

1. **Put the secret in MMX's vault.** MMX keeps secrets in the encrypted
   file `secrets_vault.age` in the project, and writes them to the node
   during each run. Open the vault in your editor; MMX asks for the vault's
   passphrase:

   ```sh
   mmx secrets edit
   ```

   Under `mmwatchdog_secret_files`, add one line: a file name of your
   choice, and the token. Save, and close the editor:

   ```yaml
   mmwatchdog_secret_files:
     slack-webhook-team: "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
     webhook-token-on-call: "the receiver's token"
   ```

   A coding agent asks the person to run `mmx secrets edit` and type the
   token themselves, so the token never passes through the chat.

   **Check:** `mmx secrets edit` ends with `Saved encrypted vault:
   secrets_vault.age`. If it reports an error instead, it kept the vault as
   it was: run it again and fix the line.

2. **List the file in the inventory,** in
   `inventories/hub_and_standalone.yml`, under `vars`:

   ```yaml
   vars:
     mmwatchdog_config_source_dir: files/hub_and_standalone
     mmwatchdog_secret_files: [slack-webhook-team, webhook-token-on-call]
   ```

   Each run writes each listed file to `/etc/mmwatchdog/secrets/` on the
   node, readable only by root. Reason: the secret then appears in no
   configuration file, command line, or message; MMWatchdog reads the file
   for each delivery.

   **Check:** each name in the list is a name under
   `mmwatchdog_secret_files` in the vault, spelled the same.

3. **Add the destination** to
   `files/hub_and_standalone/alert_destinations.yml`, below the
   destinations that are there:

   ```yaml
   destinations:
     team_slack_channel:
       slack:
         webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
       alert_severities: [info, warn, sos]
       remind_every: 4h
     on_call_receiver:                     # the name of the destination: your choice, except hub
       webhook:                            # the module
         url: https://alerts.example.com/mmwatchdog
         bearer_token_file: /etc/mmwatchdog/secrets/webhook-token-on-call   # the file of step 2
       alert_severities: [sos]             # only the alerts that need a person now
       remind_every: 1h                    # remind of each open problem every hour
   ```

   - `alert_severities` lists the severities that the destination takes.
     A check's alerts go to every destination whose list holds the check's
     `alert_severity`. Here `team_slack_channel` gets every alert and
     `on_call_receiver` gets only `sos` alerts. `[]` is allowed: the
     destination then gets only the alerts of checks that name it, as
     [Send a check's alerts to chosen destinations](#send-a-checks-alerts-to-chosen-destinations)
     shows.
   - `remind_every` is how often the destination gets a reminder while a
     problem lasts, such as `30m`, `4h`, or `1d`, or `never`. A reminder
     names the problem and when it began, such as `myapp/health_page_answers
     still failing since 04:35 UTC; latest result: expected HTTP 200,
     received 404`. Every destination states it, except `mmhotline`, which
     does not take it.
   - The module's inputs, here `url` and `bearer_token_file`, are on its
     page. An input whose name ends in `_file` names a file of step 2.

   **Check:** `mmx validate` prints `Valid: mmwatchdog`. `mmx validate`
   checks the MMX project, not the inputs of the destination: the run in
   step 5 checks those on the node before it installs the configuration.

4. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists the new secret file and the changed
   configuration, such as:

   ```text
     write_secret_files / node / secret_files[1]: would create "/etc/mmwatchdog/secrets/webhook-token-on-call"
   ...
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked inputs /etc/mmwatchdog/alert_destinations.yml and 1 more would change
   ```

   The dryrun of this project exits with code 2: something would change.
   It does not check the new destination: `validate_config` checks only the
   copy of the configuration that the last run left on the node. The run
   checks it before it installs the configuration.

5. **Install the destination:**

   ```sh
   mmx run hub_and_standalone
   ```

   ```text
   Run ba427d5a: succeeded
   1 finished
   ```

   The run restarts MMWatchdog inside a short maintenance window, so each
   destination that takes `info`, here Slack, gets the two messages
   `maintenance on for the whole node` and `maintenance off for the whole
   node`.

   **Check:** the run ends with `succeeded`. If it ends with `failed`, the
   node still runs its earlier configuration; see
   [If something is different](#if-something-is-different).

6. **Prove that the destination gets alerts.** On the node, send a test
   notice to the new destination alone:

   ```sh
   sudo mmwatchdog send-notice --alert-severity info --title "Test of the destination on_call_receiver. Nothing is wrong." --destination on_call_receiver
   ```

   ```text
   Notice sent to the destination on_call_receiver:
     on_call_receiver   delivered
   ```

   The notice goes the same way as a real alert, with the token and to the
   same address. `--destination` sends it to this destination only, even
   though the destination takes no `info` alerts.

   **Check:** the line says `delivered`, and the receiver shows the
   message. The receiver of this example got this JSON event:

   ```text
   {"id": "dec3668b43e5a5c020ee0e9fc8036fb0", "node": "app01", "check": "", "type": "notice", "alert_severity": "info", "at": "2026-11-15T04:33:12.353335757Z", "title": "app01: Test of the destination on_call_receiver. Nothing is wrong.", "title_with_alert_severity": "Info: app01: Test of the destination on_call_receiver. Nothing is wrong.", "message": "Test of the destination on_call_receiver. Nothing is wrong.", "send_alerts_only_to_destinations": ["on_call_receiver"]}
   ```

   A chat shows `title_with_alert_severity`, such as `Info: app01: Test of
   the destination on_call_receiver. Nothing is wrong.` If the line says
   `failed:`, it gives the reason; see
   [If something is different](#if-something-is-different).

### The files you changed, whole

```yaml
# inventories/hub_and_standalone.yml
# The directory of this project whose files each node gets as its
# MMWatchdog configuration in /etc/mmwatchdog. A node or a group of
# another role sets its own directory under files/ in its vars.
vars:
  mmwatchdog_config_source_dir: files/hub_and_standalone
  # The secret files that each node gets in /etc/mmwatchdog/secrets, with
  # mode 0600. List the file names here, and put each file's content in the
  # MMX vault under the secret mmwatchdog_secret_files with mmx secrets edit:
  #   mmwatchdog_secret_files:
  #     slack-webhook-team: "https://hooks.slack.com/services/..."
  # A node of this inventory gets the whole mmwatchdog_secret_files secret,
  # and writes the files listed here.
  mmwatchdog_secret_files: [slack-webhook-team, webhook-token-on-call]

nodes:
  node:
    address: 203.0.113.11
    ssh_user: deploy
```

```yaml
# files/hub_and_standalone/alert_destinations.yml
destinations:
  team_slack_channel:
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]   # every alert
    remind_every: 4h
  on_call_receiver:
    webhook:
      url: https://alerts.example.com/mmwatchdog
      bearer_token_file: /etc/mmwatchdog/secrets/webhook-token-on-call
    alert_severities: [sos]               # only the alerts that need a person now
    remind_every: 1h
```

The vault, as `mmx secrets edit` shows it:

```yaml
mmwatchdog_secret_files:
  slack-webhook-team: "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
  webhook-token-on-call: "the receiver's token"
```

### Destinations of the other modules

Each example goes under `destinations:` in
`files/hub_and_standalone/alert_destinations.yml`. Each file that an input
ending in `_file` names is a file of the vault and the inventory, as in
steps 1 and 2. The module's page explains each input.

```yaml
destinations:
  on_call_mailbox:
    email:
      smtp_server: smtp.example.com:587           # the mail server: its host name and its port
      smtp_connection_security: starttls          # starttls, implicit_tls, or unencrypted
      from: mmwatchdog@example.com
      to: [oncall@example.com, ops@example.com]   # each address gets every alert of this destination
      login_user: mmwatchdog@example.com
      login_password_file: /etc/mmwatchdog/secrets/smtp-password
    alert_severities: [warn, sos]
    remind_every: 4h
  on_call_telegram:
    telegram:
      bot_token_file: /etc/mmwatchdog/secrets/telegram-bot-token
      chat_id: "-1001234567890"                   # in quotes
    alert_severities: [sos]
    remind_every: 1h
  team_discord_channel:
    discord:
      webhook_url_file: /etc/mmwatchdog/secrets/discord-webhook-team
      mention: "@here"                            # optional; in quotes
    alert_severities: [warn, sos]
    remind_every: 4h
  team_mattermost_channel:
    mattermost:
      webhook_url_file: /etc/mmwatchdog/secrets/mattermost-webhook-team
    alert_severities: [warn, sos]
    remind_every: 4h
  on_call_phones:
    pushover:
      application_token_file: /etc/mmwatchdog/secrets/pushover-application-token
      user_or_group_key_file: /etc/mmwatchdog/secrets/pushover-group-key
      repeat_sos_alerts_every: 5m                 # Pushover repeats an sos alert until somebody acknowledges it
      stop_repeating_sos_alerts_after: 1h
    alert_severities: [sos]
    remind_every: never                           # Pushover repeats the alert itself
  mmhotline_on_call:
    mmhotline:
      mmhotline_url: https://hotline.example.com
      mmhotline_alert_source_id: mmwatchdog_app01
      mmhotline_signing_secret_file: /etc/mmwatchdog/secrets/mmhotline-signing-secret
    alert_severities: [sos]                       # no remind_every: MMHotline escalates by itself
  machine_log:
    system_log: {}                                # no inputs
    alert_severities: [info, warn, sos]
    remind_every: never
```

## Send a check's alerts to chosen destinations

By default, a check's alerts go by severity. To send one check's alerts
only to some destinations, such as the channel of the one person who looks
after a service, the check names them. This example sends the alerts of
`health_page_answers` only to `on_call_receiver`; Slack no longer gets
them, although Slack takes `sos`.

1. **Name the destinations on the check,** beside its `alert_severity`, in
   the check group's file:

   ```yaml
   # files/hub_and_standalone/check_groups/myapp.yml
   check_groups:
     myapp:
       check_every: 30s
       time_limit_per_run: 5s
       alert_after_failing_for: 1m
       recovered_after_healthy_for: 30s
       checks:
         service_is_running:
           systemd_unit_state:
             systemd_unit: myapp.service
             fail_when: the_unit_is_not_active
           alert_severity: sos
         health_page_answers:
           http:
             url: http://127.0.0.1:8080/health
             expected_http_status_codes: [200]
           alert_severity: sos
           send_alerts_only_to_destinations: [on_call_receiver]   # these destinations, and no other
   ```

   Every alert of the check goes to the named destinations and to no
   other, whatever their `alert_severities` say: the failure, each
   reminder, and the recovery. The `alert_severity` still counts: it is in
   every alert, and an `mmhotline` destination opens an incident only for
   `sos`.

   **Check:** each name is a destination in `alert_destinations.yml`.

2. **Install the change:**

   ```sh
   mmx dryrun hub_and_standalone
   mmx run hub_and_standalone
   ```

   **Check:** the run ends with `succeeded`. A name that
   `alert_destinations.yml` does not have stops the run at
   `validate_config`, before anything changes on the node:

   ```text
   mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/myapp.yml:18:9: check_groups.myapp.checks.health_page_answers.send_alerts_only_to_destinations: alert_destinations.yml has no destination named "on_call"
   ```

3. **Confirm the route.** On the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status myapp
   ```

   ```text
   myapp/health_page_answers    ok           HTTP 200 and configured content match
     Config: /etc/mmwatchdog/check_groups/myapp.yml:13:7
     Rule: check every 30s, time limit per run 5s, alert after failing for 1m, recovered after healthy for 30s
     Alert severity: sos
     Sends alerts only to: on_call_receiver
   ...
   ```

   **Check:** the check has the line `Sends alerts only to:` with your
   destinations.

Other uses of the same setting:

- **The team as well:** name both,
  `send_alerts_only_to_destinations: [team_slack_channel, on_call_receiver]`.
- **A destination only for the checks that name it:** give the destination
  `alert_severities: []`.
- **A check that alerts nobody,** but still restarts its service:
  `send_alerts_only_to_destinations: []`. `sudo mmwatchdog status` then
  shows `Sends alerts to no destination (send_alerts_only_to_destinations:
  [])` under the check.
- **Another reminder interval for one check:** name a destination whose
  `remind_every` is the interval that the check needs.
- **A check group with one check written directly on the group,** without
  `checks`: write the setting on the check group. A check group with
  `checks` refuses the setting on the group, because it would apply to
  none of the checks: write it on each check.

## Send a notice

A notice goes through the destinations the same way as an alert: use it to
test the destinations, or to let a script of yours announce an event, such
as a finished deploy or a failover. On the node:

```sh
sudo mmwatchdog send-notice --alert-severity warn --title "deploy of myapp 1.4 finished" --body "Deployed by the release script from commit 3f9e2a1."
```

```text
Notice sent to every destination that takes warn:
  team_slack_channel   delivered
```

The channel shows:

```text
Warning: app01: deploy of myapp 1.4 finished
Deployed by the release script from commit 3f9e2a1.
```

| Option | Meaning |
|---|---|
| `--alert-severity info`, `warn`, or `sos` | Required. The notice goes to every destination whose `alert_severities` hold it |
| `--title TEXT` | Required. One line that says what happened. MMWatchdog puts the node's name in front, and the whole title has at most 120 characters |
| `--body TEXT` | Optional. More text, at most 8 KiB |
| `--destination NAME` | Optional. Only this destination gets the notice, whatever its `alert_severities` say |

**Check:** each line says `delivered`, and the command exits with 0. A
line that says `failed:` gives the reason, the command exits with 1, and
MMWatchdog keeps the notice and tries again, as
[When a destination cannot be reached](#when-a-destination-cannot-be-reached)
says.

- A notice has no recovery and gets no reminder.
- A maintenance window does not hold a notice back, so a deploy script can
  announce its deploy during the window that it opened.
- A name in `--destination` that `alert_destinations.yml` does not have is
  refused, and the error lists the destinations:
  `mmwatchdog: alert_destinations.yml has no destination named "pager";
  its destinations are: on_call_receiver, team_slack_channel`.
- When no destination takes the severity, MMWatchdog records the notice in
  `sudo mmwatchdog events`, sends it to nobody, and exits with 0.
- A script that must not fail when a chat is down ignores the exit code
  1: `mmwatchdog send-notice ... || true`. Exit code 4 means that
  MMWatchdog did not take the notice at all, such as when the
  `mmwatchdog` service is not running.
- An `sos` notice to an `mmhotline` destination opens an MMHotline
  incident that a person closes in MMHotline, because a notice has no
  recovery.

## Remove a destination

1. Delete the destination's entry from
   `files/hub_and_standalone/alert_destinations.yml`, and its name from
   any check group's `alert_destinations` list.
2. If the destination read a secret file, such as `webhook-token-on-call`,
   delete that name from `mmwatchdog_secret_files` in
   `inventories/hub_and_standalone.yml`, and its entry from the vault
   with `mmx secrets edit`.
3. The run in step 4 writes the listed secret files and does not delete
   one that is no longer listed. Delete the file on each node by hand:

   ```sh
   ssh deploy@203.0.113.11 sudo rm /etc/mmwatchdog/secrets/webhook-token-on-call
   ```

4. Install the change with `mmx run hub_and_standalone`.

**Check:** on the node, `sudo mmwatchdog status` no longer names the
destination, and `sudo ls /etc/mmwatchdog/secrets` no longer lists the
file.

## Run with no destination

MMWatchdog runs without alerts: it then runs the checks, restarts
services, runs recovery commands, and records every event, and tells
nobody. Write the empty mapping in
`files/hub_and_standalone/alert_destinations.yml`:

```yaml
# files/hub_and_standalone/alert_destinations.yml
destinations: {}
```

Install it with `mmx run hub_and_standalone`.

**Check:** on the node, `sudo mmwatchdog status` ends with `This node
sends alerts to nobody.` Reason for the explicit `{}`: a missing or empty
`alert_destinations.yml` is a validation error, so a forgotten destination
does not go unnoticed. `sudo mmwatchdog validate` on the node also prints
one validation warning for each check that alerts, such as
`myapp/service_is_running: no destination receives alerts with
alert_severity sos; ...`. With no destination, the
warnings are expected; they do not make the configuration invalid.

## Messages that are not about a check

Besides the alerts of the checks, the destinations get these messages.
Each goes by its severity, to every destination that takes it, except the
two messages about a failing destination.

| Message | When | Severity |
|---|---|---|
| `maintenance on for myapp`, `maintenance off for myapp` | A maintenance window opens or ends, such as during each `mmx run` that changes the configuration. [Pause alerts during planned work](pause-alerts-during-planned-work.md) says how to send them, or not | `info` |
| `app01 rebooted`, with `app01 rebooted at 04:54 UTC; MMWatchdog last ran at 04:54 UTC` | MMWatchdog starts after a reboot of the node. A restart of MMWatchdog alone sends nothing, and a reboot during a maintenance window of the whole node sends nothing | `alert_severity_when_this_node_has_rebooted` in `files/hub_and_standalone/mmwatchdog_node_settings.yml`, such as `warn` |
| A notice | Somebody runs `mmwatchdog send-notice` | The notice's own |
| `Destination on_call_receiver is failing.` and `Destination on_call_receiver works again.` | An alert has waited 10 minutes for the destination, and later the destination takes alerts again. Every other destination of the node gets the message, whatever its `alert_severities` say | `warn` |

Choose the reboot severity per node: `info` on one of many web servers,
`warn` or `sos` on a database server whose unplanned reboot someone must
look at.

## When a destination cannot be reached

MMWatchdog keeps each alert that a destination did not take, and tries
again: after 1 second, then after twice as long each time, up to every 256
seconds. Each destination has its own queue, so a destination that is down
delays no other. A destination gets its alerts in order.

The test notice of a destination that is down shows the reason:

```text
Notice sent to every destination that takes sos:
  on_call_receiver     failed: MMWatchdog could not post the alert to the webhook: dial tcp 127.0.0.1:18081: connect: connection refused
  team_slack_channel   delivered
mmwatchdog: the notice was not delivered at the first attempt to 1 of 2 destinations. MMWatchdog keeps the notice queued and tries again
```

`sudo mmwatchdog status` on the node shows what waits:

```text
Alert deliveries: 1 waiting, 0 dropped
Destination on_call_receiver: 1 alert is not delivered. MMWatchdog tries again at 04:33 UTC. The latest attempt failed: MMWatchdog could not post the alert to the webhook: dial tcp 127.0.0.1:18081: connect: connection refused
```

When the destination is back, the next attempt delivers every waiting
alert, and `status` says `Alert deliveries: 0 waiting`. After an alert has
waited 10 minutes, the other destinations get the notice `Destination
on_call_receiver is failing.`, and later `Destination on_call_receiver
works again.`

`journalctl -u mmwatchdog` on the node has one line for each attempt:

```text
could not deliver the alert "SOS: app01: Test while the receiver is down. Nothing is wrong." to alert destination on_call_receiver. MMWatchdog will try again in 4s: MMWatchdog could not post the alert to the webhook: dial tcp 127.0.0.1:18081: connect: connection refused
delivered the alert "SOS: app01: Test while the receiver is down. Nothing is wrong." to alert destination on_call_receiver, which accepted it
```

How long MMWatchdog keeps trying is in
`files/hub_and_standalone/mmwatchdog_node_settings.yml`:

```yaml
keep_undelivered_alerts_for: 24h                 # keep an alert that could not be delivered, and try again, for 24 hours
undelivered_alerts_content_size_limit: 8MiB      # and keep at most 8 MiB of such alerts
```

An alert that waited longer, or that did not fit, is dropped, and counts in
the `dropped` number of `status`. A destination that was down may also get
the recovery of a failure whose alert it never got: match the two by the
incident in the alert.

## MMHotline as a destination

[MMHotline](https://github.com/monkeymaxxing/mmhotline) is the on-call
program of Monkeymaxxing: a team runs it on its own server, and it calls
the person on call until somebody acknowledges an `sos` alert. MMWatchdog
works fully without it.

- An `mmhotline` destination opens an MMHotline incident for each `sos`
  alert, and the recovery closes it. MMHotline escalates by itself, so the
  destination takes no `remind_every`.
- The node that has the destination also sends MMHotline a check-in every
  30 seconds, so MMHotline alerts when the node, or MMWatchdog on it,
  stops.
- The destination needs MMHotline's address, an alert source ID, and a
  signing secret that MMHotline's settings also list. The
  [`mmhotline` page](../reference/modules/mmhotline.md) sets them up, step by step.
  The signing secret goes into the vault and the inventory, as in
  [Add a destination](#add-a-destination).
- `sudo mmwatchdog status` shows one line for each MMHotline destination
  that does not answer, such as:

  ```text
  MMHotline mmhotline_on_call: The check-in to MMHotline at 04:53 UTC failed: MMHotline did not answer (Post "https://hotline.example.com/check-in/mmwatchdog_app01": dial tcp: lookup hotline.example.com on 127.0.0.53:53: no such host).
  ```

- In a fleet, the destination is on the fleet hub, and its address must be
  one that every node reaches. The hub sends one MMHotline incident for the
  same failure on many nodes;
  [MMHotline in a fleet](../reference/modules/mmhotline.md) explains it.

## If something is different

- **The test notice says `failed:`.** The line gives the module's reason,
  such as `webhook returned HTTP 401` for a wrong token, or `Slack answered
  404: no_service` for a webhook that Slack removed. Fix the secret with
  `mmx secrets edit` and run `mmx run hub_and_standalone`; a changed secret
  file restarts MMWatchdog. Then send the test notice again. The module's
  page lists its errors.
- **The run fails at `validate_config`.** The output names the file, the
  line, and the fault, such as an input that the module does not have, or
  a required input that is missing. Fix it and run again. Nothing changed
  on the node.
- **The run fails at `write_secret_files`** with `value is unavailable at
  /secrets/mmwatchdog_secret_files/NAME`. The inventory lists a file that
  the vault does not hold: add it with `mmx secrets edit`, or fix the
  spelling.
- **`sudo mmwatchdog validate` warns that no destination receives some
  alerts,** such as
  `machine/memory_is_below_90_percent: no destination receives alerts with
  alert_severity warn; add the severity to a destination's
  alert_severities, or change the check's
  send_alerts_only_to_destinations.` The configuration is valid, but the
  alerts of that check reach nobody. Add the severity to a destination, or
  accept it on purpose.
- **A secret file must be readable by a group, or by every account.**
  MMWatchdog refuses a secret file that another account can read, unless
  the destination allows it with `secret_readable_by_group: true` or
  `dangerously_let_every_account_read_secret: true` beside the module,
  or inside the `mmhotline` mapping for an MMHotline destination. The
  files that MMX writes are readable by root alone and need neither.
  [Credential files that other accounts can use](../reference/configuration-files.md#secret-files)
  gives the rule.
- **A destination should get the title of each alert only,** such as a
  relay that sends each alert as an SMS. Add `send_only_the_alert_title:
  true` to the destination.
- **A delivery takes longer than 5 seconds,** such as to a slow mail
  server. Set `time_limit_per_delivery_attempt: 15s` on the destination.
- **The nodes report to a fleet hub.** The destinations are in the hub's
  `alert_destinations.yml`, and the hub delivers the alerts of every node.
  A joined node has no `alert_destinations.yml`. Send test notices on the
  hub. [Deploy a fleet](set-up-a-fleet.md) says how.
- **You change a node that MMX did not install.** Write the secret into a
  file that only root reads, such as with
  `sudo install -m 0600 /dev/null /etc/mmwatchdog/secrets/webhook-token-on-call`
  and an editor. Edit `/etc/mmwatchdog/alert_destinations.yml`, then run
  `sudo mmwatchdog validate` and `sudo mmwatchdog reload`. `reload` checks
  each new or changed destination without sending a message, and prints
  the `send-notice` command that tests it:

  ```text
  The configuration is active.
  Destination machine_log: verified connection to the system log socket
  Destination machine_log is new or changed. To send a test notice, run:
  sudo mmwatchdog send-notice --alert-severity info --title "Test of the destination machine_log. Nothing is wrong." --destination machine_log
  ```
- **You remove a destination.** Delete it from `alert_destinations.yml`,
  and its file from the inventory's `mmwatchdog_secret_files` and from the
  vault, then run `mmx run hub_and_standalone`. The run leaves the secret
  file on the node: delete it there with
  `sudo rm /etc/mmwatchdog/secrets/webhook-token-on-call`. Alerts that
  waited for the removed destination are dropped, and `sudo mmwatchdog
  events` shows `delivery_cancelled`.
- **You change a destination's address or credential** while alerts wait
  for it. MMWatchdog drops the waiting alerts of that destination, because
  they were meant for the old receiver. A new `bearer_token_file` of a
  `webhook`, or a new password of an `email` destination, keeps them.

## Related pages

- [Install MMWatchdog on one node](install-on-one-node.md), which adds the
  first destination.
- [Pause alerts during planned work](pause-alerts-during-planned-work.md)
- [See what is happening](see-what-is-happening.md)
- [Decide when a check alerts](decide-when-a-check-alerts.md), which helps you
  choose each check's severity.
- The [module pages](../reference/modules/README.md), one for each module.
