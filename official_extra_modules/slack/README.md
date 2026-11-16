# `slack`

`slack` delivers alerts to a channel in Slack. `slack` is an
[official extra module](../../docs/reference/modules/README.md): a
Starlark script that the MMWatchdog project writes. Every deploy with the
release's MMX workflows installs `slack` on every node. An alert destination
names the module, and MMWatchdog runs the module once for each alert of that
destination.

The module posts the alert to a Slack **incoming webhook**: a web address
that Slack makes for one channel. Whatever is posted to the address appears
as a message in the webhook's channel. The module works for every service that takes Slack's incoming webhook format.

Choose `slack` for a team that reads its alerts in Slack. For a receiver of
your own that reads MMWatchdog's alert event as JSON, use the core module
[`webhook`](../../docs/reference/modules/webhook.md) instead.

## Set up

Secret files follow the
[credential permission rule](../../docs/reference/configuration-files.md#secret-files).
Place permission settings beside `slack`, where `alert_severities` stands.

The node that delivers the alert needs:

- **Nothing but MMWatchdog.** The module sends the message through MMWatchdog itself, so the
  node needs no other program.
- **A way out to the webhook's address over HTTPS.**
- **In a fleet, the module on every node, not only on the MMWatchdog fleet hub.** A
  joined node that delivers an alert without the hub runs its own copy
  ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)). The MMX
  workflows of the MMWatchdog release install every official extra module on every
  node, in `/usr/local/lib/mmwatchdog/official_extra_modules`.

The address of the webhook is a secret: anybody who has it can post to the
channel. So the address lives in a file that only root reads (mode `0600`),
never in `alert_destinations.yml`. To get an address, make an incoming
webhook for the channel in Slack
([Slack's guide](https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks)).
Then write the address into `/etc/mmwatchdog/secrets/slack-webhook-team`,
on the standalone node or on the MMWatchdog fleet hub:

```sh
sudo install -m 0600 /dev/null /etc/mmwatchdog/secrets/slack-webhook-team
sudo nano /etc/mmwatchdog/secrets/slack-webhook-team    # paste the address, one line
```

MMWatchdog reads the webhook file for every delivery and gives the module
the file's content, without the final line break. The module needs no
access to the webhook file. A new address in the webhook file needs no
reload on the machine that reads the file.
In a fleet, reload the hub after changing the webhook file. The hub sends
the file's content to joined nodes and reads the file again only at startup
and reload. The address is in no command's arguments and in no
message that the module or MMWatchdog writes about a delivery.
[Destination modules](../../docs/reference/writing-modules.md#destination-modules) has the rules
that every such file follows.

## Inputs

| Input | Meaning | When omitted |
| --- | --- | --- |
| `webhook_url_file` | The absolute path of a file that holds the address of the incoming webhook, such as `https://hooks.slack.com/services/YOUR/WEBHOOK/ADDRESS`, and nothing else | Required: `mmwatchdog validate` refuses the destination |
| `mention` | A Slack mention that starts every message sent through the destination whose `slack` inputs include `mention`. Use Slack's notation: `<@U024BE7LH>` for a person by member ID. Use `<!subteam^SAZ94GDB8>` for a user group. Use `<!channel>` or `<!here>` for the channel | The message starts with the alert's title and notifies nobody in particular |

Beside the inputs, a destination can state the run settings that every
destination module takes, such as `run_as_user_account`
([destination modules](../../docs/reference/writing-modules.md#destination-modules)).
`mmwatchdog describe-module slack` prints the inputs on a node.

## What it sends

The first line of the message is the alert's title with the alert's severity
in front, such as `SOS: app-1: myapp/http failed`
([title and body of an alert](../../docs/reference/alert-format.md)).
The alert's body appears below the first line, in a **code block**.
A code block is text between two lines of three backticks, which Slack
shows unchanged in a box. MMWatchdog writes the first line and the body,
so the alert reads in Slack as it reads
everywhere else. With `mention`, the mention starts the first line.

The following message shows what the module sends for a failed check with
`alert_severity: sos` on the node `app-1`:

````text
<@U024BE7LH> SOS: app-1: myapp/http failed
```
expected HTTP 200, received 503
failing since 14:02 UTC
```
````

When the member ID is Barbara's, Slack shows the first line as
`@Barbara SOS: app-1: myapp/http failed` and notifies Barbara. The recovery
of `myapp/http` on `app-1` starts with
`Resolved SOS: app-1: myapp/http is ok again`.

- **A recovery, a reminder, a [notice](../../docs/reference/commands.md#mmwatchdog-send-notice),
  and every other alert** use the title with the severity in front.
  The body follows in a code block. With `mention`, the mention starts the
  first line.
- **A destination with `send_only_the_alert_title: true`** gets the first
  line, with the mention if it has one, and no code block.
- **The code block keeps the body as it is:** `*`, `_`, and `~` in the
  output of a check stay those characters and do not turn into bold or
  italic text.
- **Three backticks in a row** would end the code block early, or start one
  in the first line, so the module sends them as three single quotes.
- **`&`, `<`, and `>` of an alert** go out as `&amp;`, `&lt;`, and `&gt;`,
  which Slack shows as the three characters. The mention keeps its `<` and
  `>`.
- **The first line is ordinary text,** so that the mention notifies. Slack
  therefore formats the first line. A title with `*` or `_` around a word,
  such as a notice's title, can show the word in bold or italic.
- **Slack asks for messages of at most 4,000 characters,** counted as
  Slack gets the characters, with `&` as the five characters `&amp;`.
  The module cuts a longer body and marks the cut with `…`. The first line
  and the end of the code block always arrive.
  `sudo mmwatchdog events` on the node has the whole alert.

## When a delivery fails

Slack answers an accepted message with HTTP 200 and the text `ok`. The
module requires a 2xx status and `ok` after trimming white space. Every other
answer, and every failure to reach Slack, is a failed attempt. The module
then answers `failed` with a message that says what Slack answered and what
to do. MMWatchdog keeps the alert and tries again, as for every destination: after
one second, then with the delay doubling up to 256 seconds
([destination modules](../../docs/reference/writing-modules.md#destination-modules)). Each
attempt runs the module again.

While the attempts fail, `mmwatchdog status` shows the destination with the
module's message after the words `the module slack reported:`
([a destination that is failing](../../docs/when-something-fails.md#a-destination-is-failing)).
[Errors and what they mean](#errors-and-what-they-mean) lists each message.

## Examples

### Every alert to the team's channel, with a mention

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    slack:                                                   # the module
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team   # the file that holds the webhook's address
      mention: "<@U024BE7LH>"                                # starts every message
    alert_severities: [info, warn, sos]
```

The destination `team_chat` names the module `slack` and writes the
module's inputs below that name. What happens when a check fails, on the
standalone node or on the hub that holds
`/etc/mmwatchdog/alert_destinations.yml`:

1. The alert's severity is in `alert_severities`, so MMWatchdog queues one delivery
   for `team_chat`.
2. MMWatchdog reads `/etc/mmwatchdog/secrets/slack-webhook-team` and runs the module `slack`
   with the alert, the inputs, and the content of that file.
3. The module posts the message to the webhook: the mention, the title with
   the severity in front, and the body in a code block. Slack notifies the
   person with the member ID `U024BE7LH`.
4. Slack answers 200 with `ok`, and the module answers `delivered`.

### The whole channel for `sos` alerts only

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]
  on_call_channel:
    remind_every: never
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-on-call
      mention: "<!channel>"                                  # notifies every member of the channel
    alert_severities: [sos]
```

Two destinations use the module, each with the webhook of its own channel.
An `info` or `warn` alert goes to `team_chat` alone. An `sos` alert goes to
`team_chat` and `on_call_channel`. MMWatchdog runs the module once for each
destination. In the channel of `on_call_channel`, the message starts with
`@channel`, which notifies every member.

### One administrator's channel, only for the checks that name it

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]
  mail_administrator_chat:
    remind_every: never
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-mail-administrator
      mention: "<@U024BE7LH>"
    alert_severities: []                                     # only checks that name this destination reach it
```

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
```

When `postfix.service` has been stopped for five minutes, MMWatchdog on the node
sends the `warn` alert to `mail_administrator_chat` alone, with the mention
of the administrator. `team_chat` gets nothing, although it takes `warn`,
because the check names its destination. The recovery goes to the channel
of `mail_administrator_chat`.
[Which destination gets an alert](../../docs/how-to/send-alerts-somewhere.md#send-a-checks-alerts-to-chosen-destinations)
has the rules.

## Test the destination

`mmwatchdog send-notice` sends a message through the destination as a real alert
goes, and prints what happened. Run `mmwatchdog send-notice` on the
standalone node or on the hub:

```sh
sudo mmwatchdog send-notice --alert-severity info --title "Test of the team chat. Nothing is wrong." --destination team_chat
```

```text
Notice sent to the destination team_chat:
  team_chat   delivered
```

The channel then shows `Info: Test of the team chat. Nothing is wrong.`,
after the mention if the destination has one. When the delivery fails, the
command prints the module's message and keeps the notice queued:

```text
Notice sent to the destination team_chat:
  team_chat   failed: the module slack reported: Slack answered 404: no_service. The address of the webhook does not work any more. Make a new incoming webhook in Slack and write its address into the file that webhook_url_file names.
mmwatchdog: the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again
```

### Verify without a message

For `hooks.slack.com`, MMWatchdog posts the text `mmwatchdog verify` with the content type
`application/json`. The body is not JSON and cannot become a message. A
400 `invalid_payload` is accepted. A 404 `no_team`, or a 400, 403, or 404
with `no_service`, `invalid_token`, `no_active_hooks`, or `team_disabled`,
fails verification. Other hosts receive only a connection and certificate
check, with no HTTP request; their credential and posting rights are not
checked.

A connection or certificate failure, or any HTTP 5xx answer, also fails
verification. Every other answer passes, including a rate limit or a reply
whose meaning has not been confirmed. Accepting responses that do not prove
a bad credential avoids a false verification failure. A successful
verification does not replace a test notice for the full delivery path.

## How it works

One delivery attempt is one run of the script `module.star`, under the
destination's `time_limit_per_delivery_attempt`:

1. MMWatchdog reads the file that `webhook_url_file` names and starts the script as
   the destination's `run_as_user_account`, `mmwatchdog` unless stated.
2. The module checks that the file's content is one web address that starts
   with `https://` or `http://`, without a space, a control character, a
   quote, or a backslash.
3. The module writes the message as [what it sends](#what-it-sends) says,
   and sends it with MMWatchdog's primitive `http_request` as an HTTP `POST` of the
   JSON `{"text": "..."}` to the webhook's address. The primitive follows
   no redirect and never writes the address into an error. An error can
   name the host, such as `hooks.slack.com`, but never the path behind it,
   which holds the secret.
4. The module answers `delivered` for a 2xx status with the text `ok` after
   trimming white space. Every other answer is `failed`.
   Before showing the first 200 characters, the module hides the webhook's
   full path and credential segments, even when a proxy returns only a
   path fragment.

## Errors and what they mean

| Message | What the message means and what to do |
| --- | --- |
| `destinations.team_chat.slack.channel: unknown field "channel": the inputs of the module slack are webhook_url_file, mention` | `mmwatchdog validate`: the destination states an input that the module does not have. Remove the `channel` input. Slack chooses the channel by the webhook |
| `destinations.team_chat.slack.webhook_url_file: is required: The absolute path of a file that holds the address of the Slack incoming webhook, and nothing else. ...` | `mmwatchdog validate`: the destination leaves out `webhook_url_file` |
| `destinations.team_chat.slack.mention: expected a string` | `mmwatchdog validate`: the mention is not text. Put the `mention` value in quotes, such as `"<@U024BE7LH>"` |
| `Slack answered 404: no_service. The address of the webhook does not work any more. Make a new incoming webhook in Slack and write its address into the file that webhook_url_file names.` | The webhook was removed or turned off. Slack can also say `no_active_hooks`, `no_service_id`, `invalid_token`, `no_team`, or `team_disabled`. For any of the listed errors, make a new incoming webhook. Write the new webhook's address into the file named by `webhook_url_file` |
| `Slack answered 410: channel_is_archived. The channel of the webhook does not exist any more or is archived. Make a new incoming webhook for another channel.` | Slack can also say `channel_not_found`. Make a new incoming webhook for another channel |
| `Slack answered 403: action_prohibited. An administrator of the Slack workspace does not let this webhook post to its channel.` | Slack can also say `posting_to_general_channel_denied`. Ask an administrator of the Slack workspace |
| `Slack answered 429: ... Slack takes no more messages from this webhook for a short time.` | Nothing to do. Slack takes about one message a second from one webhook, and MMWatchdog tries again |
| `Slack answered 503: no text.` | Slack, or the service that takes Slack's format, answered with an error and no text. Nothing to do unless the service error lasts: MMWatchdog tries again |
| `MMWatchdog could not send the message to Slack: dial tcp: lookup hooks.slack.com: no such host` | The node did not reach Slack. The text after `Slack:` says why: here the node could not look up the name `hooks.slack.com` |
| `the file that webhook_url_file names must hold one web address that starts with https:// or http://, and nothing else` | The file holds something other than the address of the webhook. Write the address into the file, on one line |
| `MMWatchdog cannot read the file of the input webhook_url_file of the module slack: open /etc/mmwatchdog/secrets/slack-webhook-team: no such file or directory` | MMWatchdog could not read the file, so it did not start the module. Write the file as [set up](#set-up) shows. `sudo mmwatchdog validate` finds a missing file before a deploy |

After you correct the webhook file, the next attempt delivers the queued alerts.
The errors that every destination module can meet, such as a run past the
time limit or a module that is not installed on the node, are in
[destination modules](../../docs/reference/writing-modules.md#destination-modules).

## Tested on

- **Go tests against a stand-in for Slack** (`tests/official_extra_modules/modules_test.go`).
  The Go tests build `mmwatchdog` and run the script through
  `mmwatchdog run-starlark-module`, as MMWatchdog does.
  An HTTP server inside the test answers as Slack does. The Go tests cover:

  - Message format: the exact JSON of a failure, a recovery, a reminder,
    and an alert with only the title, with and without a mention.
  - Escaping and length: `&`, `<`, `>`, and three backticks are escaped.
    Messages are cut at 4,000 characters, never inside `&amp;`.
  - Slack failures: a removed or revoked webhook, an archived or restricted
    channel, a rate limit, and a Slack error.
  - Unexpected responses: a page from another server and a redirect that
    the module does not follow.
  - Connection failures: nothing listening, and a server that accepts the
    connection but never answers.
  - Validation: a file that holds no web address, and input validation.
  - Secrecy: the webhook's address appears in no output.

  The Go tests ran with `go test ./...` on macOS on ARM64.
- **The acceptance suite on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)**
  (`tests/integration/official_extra_destination_module_scenarios.py`). The module is installed in
  `/usr/local/lib/mmwatchdog/official_extra_modules`, runs as the account `mmwatchdog`, and
  delivers over HTTPS to a stand-in for Slack in the test virtual machine
  (`tests/integration/destination_services_stand_in.py`): a check's failure and recovery, a
  destination that takes only the title, and a notice. A webhook that Slack
  no longer knows fails with Slack's answer in `mmwatchdog send-notice` and
  `mmwatchdog status`, and the queued notice arrives once the webhook file
  holds a working webhook,
  without a reload. No output of MMWatchdog and no line of its journal
  holds the webhook's address.
- **The fleet suite on every target distribution**
  (`tests/integration/fleet.py`): while the hub is down, a joined node runs
  its own installed `slack` with the webhook's address from its copy of the
  fleet settings.

- **The real service test against Slack itself**
  (`tests/official_extra_modules/real_services_test.go`), which runs on a
  Mac before each release
  ([real services before a release](../../docs/changing-mmwatchdog/testing.md#real-services-before-a-release)).
  Slack accepted a failure with the mention `<!here>`, its recovery, and an
  alert that the module cut to 4,000 characters. In the channel, the first
  line was plain text with the mention, the check's output was a code block
  with `&`, `<`, `>`, `*`, `_`, backticks, and `~` as they are, and the long
  message ended with `…` and a closed code block. A wrong webhook got
  `403 invalid_token` and the advice to make a new webhook. The
  verification accepted the real webhook and refused the wrong webhook,
  without a message
  ([validation record](../../docs/changing-mmwatchdog/validation.md#real-alert-destinations)).

Not verified: Slack's rate limits ([validation record](../../docs/changing-mmwatchdog/validation.md#not-verified-for-this-build)).

## Why it works this way

- **Verification sends no alert.** A malformed request cannot become a Slack
  message. Other services that accept Slack messages may handle a malformed
  verification request differently, so only Slack gets the malformed request.

- **The body goes in a code block, and Slack's formatting stays on.** A code
  block shows the output of a check as it is, and with formatting on, a
  mention is sure to notify.
- **MMWatchdog writes the first line and body. The module only places the
  first line and body.** An alert reads the same in Slack, in an email,
  and in MMHotline. No module writes wording of its own. A team that wants
  other wording writes a module of its own, which receives the whole alert event.
- **`slack` is an official extra module, not a core module.** Slack is a
  service that one company runs, and it changes with that company. A script
  can follow Slack's changes in a new release without a new
  `mmwatchdog` program.
- **The module sends with MMWatchdog's primitive `http_request`.** A node needs
  nothing but MMWatchdog to deliver an alert: no `curl` and no other tool.
- **The address of the webhook lives in a file that MMWatchdog reads.** The address
  is the credential, so it stays out of the configuration, version control,
  command lines, and output. The module needs no access to the file.
- **Delivery needs the acknowledgement `ok`.** A proxy can return a login
  page with HTTP 200. Treating the proxy's login page as delivered would
  silently lose the alert. A 2xx status alone cannot acknowledge delivery.
- **Each failed answer carries Slack's own word and what to do.** The person
  who reads `mmwatchdog status` at night learns what to fix without reading Slack's
  documentation.
- **The module keeps nothing between two deliveries.** MMWatchdog's outbox owns
  the retries, the order, and the gaps. No answer from the module stops
  retries: a mistaken "do not try again" answer would lose an alert.
