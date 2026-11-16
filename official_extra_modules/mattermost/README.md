# `mattermost`

`mattermost` delivers alerts to a channel in Mattermost, a chat server that
many teams run themselves. `mattermost` is an
[official extra module](../../docs/reference/modules/README.md): a
Starlark script that the MMWatchdog project writes, and that every deploy with the
release's MMX workflows installs on every node. An alert destination names
the module, and MMWatchdog runs the module once for each alert of that destination.

The module posts the alert to a Mattermost **incoming webhook**: a web
address that Mattermost makes for one channel, such as
`https://chat.example.com/hooks/9xuqwrwgstrb3mzrxb83nb357a`. Whatever is
posted to the address appears as a message in the webhook's channel.

Choose `mattermost` for a team that reads its alerts in Mattermost.
Mattermost also takes messages in Slack's format, but the module
[`slack`](../slack/README.md) writes Slack's notation for mentions and
formatting, which Mattermost reads in another way. For a receiver of your
own that reads MMWatchdog's alert event as JSON, use the core module
[`webhook`](../../docs/reference/modules/webhook.md) instead.

## Set up

Secret files follow the
[credential permission rule](../../docs/reference/configuration-files.md#secret-files).
Place permission settings beside `mattermost`, where `alert_severities` stands.

The node that delivers the alert needs:

- **Nothing but MMWatchdog.** The module sends the message through MMWatchdog itself, so the
  node needs no other program.
- **A way out to the Mattermost server over HTTPS.** The address of the
  webhook names the server.
- **Trust in the server's certificate.** MMWatchdog checks the certificate against
  the system trust store of the node. A certificate of the company's own
  certificate authority works when that authority is in the node's trust
  store.
- **In a fleet, the module on every node, not only on the MMWatchdog fleet hub.** A
  joined node that delivers an alert without the hub runs its own copy
  ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)). The MMX
  workflows of the MMWatchdog release install every official extra module on every
  node, in `/usr/local/lib/mmwatchdog/official_extra_modules`.

In Mattermost, make the incoming webhook
([Mattermost's guide](https://docs.mattermost.com/integrations-guide/incoming-webhooks.html)):

1. Open the product menu, choose **Integrations**, then **Incoming
   Webhooks**, then **Add Incoming Webhook**. Without **Integrations** in the
   menu, an administrator has turned incoming webhooks off, or allows them
   to administrators only: the setting **Enable Incoming Webhooks** is in the
   System Console, under **Integrations > Integration Management**.
2. Give the webhook a name, choose the channel, and select **Lock to this
   channel**. Save. Mattermost shows the webhook's address.
3. Keep the account used to create the webhook in the channel. The webhook
   posts as the account used to create the webhook, with the label `BOT`
   beside the account's name. The webhook stops working in a private channel
   that the account used to create the webhook leaves.

The address of the webhook is a secret: anybody who has it can post to the
channel. So the address lives in a file that only root reads (mode `0600`),
never in `alert_destinations.yml`. Write the address into the file, on the
standalone node or on the MMWatchdog fleet hub:

```sh
sudo install -m 0600 /dev/null /etc/mmwatchdog/secrets/mattermost-webhook-team
sudo nano /etc/mmwatchdog/secrets/mattermost-webhook-team    # paste the address, one line
```

MMWatchdog reads the file named by `webhook_url_file` for every delivery and gives the module the file's content,
without the final line break. The module needs no access to the secret file.
A new address in the secret file needs no reload on the delivering machine.
In a fleet, reload the hub after you change the secret file. The hub sends
the secret file's content to joined nodes. The hub reads the secret file
only when the hub starts and reloads. The address is in no command's arguments and in no
message that the module or MMWatchdog writes about a delivery.
[Destination modules](../../docs/reference/writing-modules.md#destination-modules) has the rules
that every such file follows.

## Inputs

| Input | Meaning | When omitted |
| --- | --- | --- |
| `webhook_url_file` | The absolute path of a file that holds the address of the incoming webhook, such as `https://chat.example.com/hooks/9xuqwrwgstrb3mzrxb83nb357a`, and nothing else | Required: `mmwatchdog validate` refuses the destination |
| `mention` | A Mattermost mention starts every message sent to the destination configured with `mattermost`. Write mentions in Mattermost's notation and in quotes. Use `"@barbara"` for a person by user name, or `"@sre-team"` for a user group. Use `"@channel"` or `"@all"` for every channel member, or `"@here"` for online members. One text can hold several mentions, such as `"@barbara @robert"` | The message starts with the alert's title and notifies nobody in particular |

A text that starts with `@` needs quotes in YAML. Without quotes around the text, `mmwatchdog validate` refuses the YAML file. Beside the inputs, a
destination can state the run settings that every destination module takes,
such as `run_as_user_account`
([destination modules](../../docs/reference/writing-modules.md#destination-modules)).
`mmwatchdog describe-module mattermost` prints the inputs on a node.

## What it sends

The first line of the message is the alert's title with the alert's severity
in front, such as `SOS: app-1: myapp/http failed`
([title and body of an alert](../../docs/reference/alert-format.md)).
Below the first line comes the alert's body, in a **code block**: text
between two lines of backticks. Mattermost shows the code block in a box,
as plain text. MMWatchdog writes the
first line and the body, so the alert reads in Mattermost as it reads
everywhere else. With `mention`, the mention starts the first line.

The example below shows the message that `mattermost` sends for a failed
check with `alert_severity: sos` on the node `app-1`:

````text
@barbara SOS: app-1: myapp/http failed
```
expected HTTP 200, received 503
failing since 14:02 UTC
```
````

Mattermost shows the first line as it is, highlights `@barbara`, and
notifies Barbara. Every alert is a post of its own, with its first line and
its body:

| Alert | First line, for example |
| --- | --- |
| A check failed, or MMWatchdog raised the failure alert | `SOS: app-1: myapp/http failed`, `SOS: app-1: myapp/http still failing, now SOS` |
| MMWatchdog cannot run a check, or MMWatchdog raised the cannot-check alert | `Warning: app-1: MMWatchdog cannot check myapp/http` |
| A check recovered, MMWatchdog can run it again, or MMWatchdog closed its incident without a recovery | `Resolved SOS: app-1: myapp/http is ok again`, `Resolved warning: app-1: MMWatchdog can check myapp/http again`, `Closed SOS: app-1: myapp/http incident closed, maybe not fixed` |
| A reminder of an open incident | `SOS: app-1: myapp/http still failing` |
| A maintenance window was turned on or off | `Info: app-1: maintenance on for the whole node` |
| The node rebooted | `Info: app-1 rebooted` |
| A [notice](../../docs/reference/commands.md#mmwatchdog-send-notice) | `Info: app-1: Test of the team chat. Nothing is wrong.` |
| The MMWatchdog fleet hub grouped the alerts of several nodes | `SOS: myapp/http failed on 7 nodes` |

- **A recovery is a new post.** Mattermost keeps no incidents, so the label
  `Resolved` says which failure the post ends.
- **A destination with `send_only_the_alert_title: true`** gets the first
  line, with the mention if it has one, and no code block.
- **The code block keeps the body as it is.** Nothing in the code block
  formats, links,
  or notifies anybody: `*`, `_`, `#`, a web address, and `@channel` in the
  output of a check stay text. The lines of backticks around the body are
  longer than every row of backticks in the body, so no line of the body can
  end the code block: a body that holds three backticks in a row gets four
  around it.
- **The first line shows the title as it is.** Mattermost reads the first line as Markdown, so that the
  mention notifies. The module therefore puts
  a backslash in front of `` ` ``, `*`, `~`, `[`, and `]`, and in front of
  `_` where it does not stand between two letters or digits, as in a
  check's name. Mattermost displays the escaped backticks, asterisks,
  tildes, square brackets, and underscores as text without formatting.
  A backslash gets a backslash too, except before an ASCII letter or
  digit, where it escapes nothing: the failed unit `mnt-data\x2d1.mount`,
  in whose name systemd writes `-` as `\x2d`, shows with one backslash and
  can be copied into `systemctl`.
- **Only `mention` notifies.**
  An `@` of the title, such as `@channel` in
  the title of a notice, gets an invisible character after it, the zero
  width space, so the `@` shows and mentions nobody. An `@` after a letter
  or a digit, as in `getty@tty1.service`, mentions nobody anyway and stays
  as it is. Mattermost turns Slack's `<!channel>` into a mention, so its `!`
  gets a backslash.
- **Mattermost rewrites Slack's notation in every message,** in the code
  block too: `<!channel>` shows as `@channel`, and
  `<https://example.com|the page>` as `[the page](https://example.com)`.
  Nothing in the code block notifies anybody. Mattermost rewrites Slack
  notation after the module's backslashes, so a title that holds
  `<address|text>` shows as a link.
- **A message has at most 16,383 characters,** the most that Mattermost
  keeps in one post. Mattermost splits a longer text of a webhook into
  several posts, which would break the code block apart, so the module cuts
  a longer body and marks the cut with `…`. The first line and the end of
  the code block always arrive. `sudo mmwatchdog events` on the node has the whole
  alert.

**When a mention notifies.** Mattermost lets the message of a webhook
mention, as the message of a person does:

- `"@barbara"` notifies Barbara when she is a member of the channel, also
  when Barbara made the webhook.
- `"@channel"` and `"@all"` notify every member of the channel, and
  `"@here"` the members who are online. The mentions `@channel`, `@all`, and
  `@here` work only if the webhook's creator has permission to use channel
  mentions. An administrator can turn channel mentions off for a channel
  under **User Management > Channels** in the System Console.
  The mentions `@channel`, `@all`, and `@here` notify nobody when channel
  membership reaches **Max notifications per channel** or more.
  The setting defaults to 1,000 members unless an administrator changes
  **Max notifications per channel**. Each member can choose to ignore
  notifications from `@channel`, `@all`, and `@here` in one channel.
- A user group, such as `"@sre-team"`, notifies its members where the
  Mattermost server has user groups, which not every plan of Mattermost has.
- A person's own **words that trigger mentions**, such as a first name in
  Mattermost's settings, notify that person when the first line holds that
  word, as in any message. The code block notifies nobody.

## When a delivery fails

Mattermost answers an accepted message with the HTTP status code 200 and the
text `ok`. Every other answer, and every failure to reach Mattermost, is a
failed attempt. The module then answers `failed` with a message that says
what Mattermost answered and what to do. MMWatchdog keeps the alert and tries again,
as for every destination: after one second, then with the delay doubling up
to 256 seconds ([destination modules](../../docs/reference/writing-modules.md#destination-modules)).
Each attempt runs the module again.

Mattermost writes a refusal as JSON with an id, such as
`web.incoming_webhook.general.app_error`, which is the same in every
language, a message in the language of the server, and the status code.
Recent versions of Mattermost answer every refusal of a webhook with the
same id and message, and the message names the webhook by its key: the last
part of its address, and the secret in it. So the module reports the id and
not the message, and tells the refusals apart by their status code. An
answer without an id and a status code comes from another server, such as a
proxy or a gateway in front of Mattermost. The module reports the
server's `message` as the server's words.

MMWatchdog keeps the first 300 bytes of a module's message. The module writes the module's own text in full.
The module shortens the words of Mattermost or another server, without the webhook's key,
to the bytes left, marking the cut with `…`. So the advice at the end always arrives whole.

While the attempts fail, `mmwatchdog status` shows the destination with the
module's message after the words `the module mattermost reported:`
([a destination that is failing](../../docs/when-something-fails.md#a-destination-is-failing)).
[Errors and what they mean](#errors-and-what-they-mean) lists each message.

## Examples

### Every alert to the team's channel, with a mention

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    mattermost:                                                   # the module
      webhook_url_file: /etc/mmwatchdog/secrets/mattermost-webhook-team   # the file that holds the webhook's address
      mention: "@barbara"                                         # starts every message, in quotes
    alert_severities: [info, warn, sos]
```

The destination `team_chat` names the module `mattermost` and writes the
module's inputs below that name. What happens when a check fails, on the
standalone node or on the hub that holds
`/etc/mmwatchdog/alert_destinations.yml`:

1. The alert's severity is in `alert_severities`, so MMWatchdog queues one delivery
   for `team_chat`.
2. MMWatchdog reads `/etc/mmwatchdog/secrets/mattermost-webhook-team` and runs the module
   `mattermost` with the alert, the inputs, and the content of that file.
3. The module posts the message to the webhook: the mention, the title with
   the severity in front, and the body in a code block. Mattermost notifies
   Barbara.
4. Mattermost answers 200 and `ok`, and the module answers `delivered`.

### The whole channel for `sos` alerts only

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    mattermost:
      webhook_url_file: /etc/mmwatchdog/secrets/mattermost-webhook-team
    alert_severities: [info, warn, sos]
  on_call_channel:
    remind_every: never
    mattermost:
      webhook_url_file: /etc/mmwatchdog/secrets/mattermost-webhook-on-call
      mention: "@channel"                                         # notifies every member of the channel
    alert_severities: [sos]
```

Two destinations use the module, each with the webhook of its own channel.
An `info` or `warn` alert goes to `team_chat` alone, and notifies nobody in
particular. An `sos` alert goes to `team_chat` and `on_call_channel`:
MMWatchdog runs the module once for each
destination, and in the channel of `on_call_channel` the message starts with
`@channel`, which notifies every member.

### One administrator's channel, only for the checks that name it

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    mattermost:
      webhook_url_file: /etc/mmwatchdog/secrets/mattermost-webhook-team
    alert_severities: [info, warn, sos]
  mail_administrator_chat:
    remind_every: never
    mattermost:
      webhook_url_file: /etc/mmwatchdog/secrets/mattermost-webhook-mail-administrator
      mention: "@barbara"
    alert_severities: []                                          # only checks that name this destination reach it
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
of Barbara. `team_chat` gets nothing, although `team_chat` takes `warn`, because the
check names `mail_administrator_chat`. The recovery goes to the channel of `mail_administrator_chat`.
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

The channel then shows `Info: app-1: Test of the team chat. Nothing is
wrong.`, after the mention if the destination has one. When the delivery
fails, the command prints the module's message and keeps the notice queued:

```text
Notice sent to the destination team_chat:
  team_chat   failed: the module mattermost reported: Mattermost answered 400: web.incoming_webhook.general.app_error. Mattermost does not know this webhook: it was deleted, or its channel was archived. Make a new incoming webhook and write its address into the file that webhook_url_file names.
mmwatchdog: the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again
```

### Verify without a message

MMWatchdog posts text to the deliberately invalid channel `mmwatchdog!verify`. Mattermost
looks up the webhook before the channel and never posts the text. HTTP 400
means the webhook is invalid and fails verification; HTTP 404 means the
webhook passed and the channel did not exist, so verification passes.
Posting to `mmwatchdog!verify` checks the webhook key, not the webhook's
usual channel or posting rights.

A connection or certificate failure, or any HTTP 5xx answer, also fails
verification. Every other answer passes, including a rate limit or a reply
whose meaning has not been confirmed. Passing verification for rate limits
and unconfirmed responses avoids a false credential failure when the
response does not prove the credential is bad. A successful
verification does not replace a test notice for the full delivery path.

## How it works

One delivery attempt is one run of the script `module.star`, under the
destination's `time_limit_per_delivery_attempt`:

1. MMWatchdog reads the file that `webhook_url_file` names and starts the script as
   the destination's `run_as_user_account`, `mmwatchdog` unless stated.
2. The module checks that the file's content is one web address that starts
   with `https://` or `http://`, without a space, a control character, a
   quote, or a backslash, and that ends with `/hooks/` and the key, which is
   letters and digits.
3. The module writes the message as
   [what it sends](#what-it-sends) says.
   The module calls `http_request`, MMWatchdog's function for sending HTTP
   requests, to send an HTTP `POST` of the JSON `{"text": "..."}` to the
   webhook's address. In MMWatchdog, a function such as `http_request` is
   a **primitive**.
   The request states no channel, user name, or picture, so the webhook's
   own settings in Mattermost hold. The primitive follows no redirect and
   never writes the address into an error. An error can name the host, such as
   `chat.example.com`, but never the path behind it, which holds the secret.
4. The module answers `delivered` for the status code 200 with the text
   `ok`, and `failed` for everything else: with the id of Mattermost's
   answer, or Mattermost's message where the id is empty, or the `message` of
   another server, each without the webhook's key and shortened to the
   bytes that the advice leaves of 300.

## Errors and what they mean

| Message | What the message means and what to do |
| --- | --- |
| `destinations.team_chat.mattermost.channel: unknown field "channel": the inputs of the module mattermost are webhook_url_file, mention` | `mmwatchdog validate`: the destination states an input that the module does not have. Remove the `channel` input from `destinations.team_chat.mattermost`. Mattermost chooses the channel by the webhook |
| `destinations.team_chat.mattermost.webhook_url_file: is required: The absolute path of a file that holds the address of the Mattermost incoming webhook, and nothing else. ...` | `mmwatchdog validate`: the destination leaves out `webhook_url_file` |
| `/etc/mmwatchdog/alert_destinations.yml: yaml: line 5: found character that cannot start any token` | `mmwatchdog validate`: a mention without quotes. Write `mention: "@barbara"` |
| `Mattermost answered 400: web.incoming_webhook.general.app_error. Mattermost does not know this webhook: ...` | The webhook was deleted, or its channel was archived, which deletes the channel's webhooks. In the services suite, Mattermost Team Edition 11.11.1 answered a deleted webhook with HTTP 400. Mattermost answers HTTP 404 when the channel is gone. For HTTP 400 and HTTP 404, the module advises creating a new incoming webhook. Make a new incoming webhook and write the new webhook's address into the file named by `webhook_url_file` |
| `Mattermost answered 403: web.incoming_webhook.general.app_error. Mattermost does not let this webhook post to its channel: the account that made it may have left the channel or its team. ...` | Make a new incoming webhook with an account that is a member of the channel |
| `Mattermost answered 501: web.incoming_webhook.general.app_error. An administrator of Mattermost has turned off incoming webhooks. ...` | Ask an administrator of Mattermost to turn on **Enable Incoming Webhooks** |
| `Mattermost answered 429: limit exceeded. Mattermost takes no more requests from this node for 3 seconds.` | Nothing to do. An administrator of Mattermost limits how many requests the server takes from one address, and MMWatchdog tries again |
| `Mattermost answered 413: api.context.request_body_too_large.app_error. The message is larger than Mattermost, or a proxy in front of it, takes.` | Ask the administrator of Mattermost, or of the proxy, to allow larger requests. A message of the module is at most 16,383 characters |
| `Mattermost answered 500: Internal Server Error.` | Mattermost had an error. Nothing to do unless the delivery failure lasts: MMWatchdog tries again. A Mattermost server with `EnableHardenedMode` answers every error of 500 or more with `Internal Server Error` and an empty id, also when incoming webhooks are turned off. When the delivery failure lasts, ask an administrator of Mattermost whether **Enable Incoming Webhooks** is on |
| `the server at the webhook's address answered 502 without the answer of Mattermost. Check the address in the file that webhook_url_file names, and what stands between this node and Mattermost.` | Something other than Mattermost answered, such as a proxy whose Mattermost is down, or the address names another server. The module reports `without the answer of Mattermost` also for an answer 200 without `ok`. A server that answers with JSON and a `message` has its words after a colon, such as `answered 403 without the answer of Mattermost: Forbidden.` |
| `the server at the webhook's address answered 301, a redirect, and MMWatchdog follows no redirect. Copy the webhook's address from Mattermost again into the file that webhook_url_file names.` | The address is an old one, often with `http://` where the server wants `https://` |
| `MMWatchdog could not send the message to Mattermost: dial tcp: lookup chat.example.com: no such host` | The node did not reach the server. The text after `Mattermost:` says why: in the example error, the node could not look up the name `chat.example.com` |
| `the file that webhook_url_file names must hold one address of a Mattermost incoming webhook, such as https://chat.example.com/hooks/9xuqwrwgstrb3mzrxb83nb357a, and nothing else` | The file holds something other than the address of the webhook. Write the address into the file, on one line |
| `MMWatchdog cannot read the file of the input webhook_url_file of the module mattermost: open /etc/mmwatchdog/secrets/mattermost-webhook-team: no such file or directory` | MMWatchdog could not read the file, so it did not start the module. Write the file as [set up](#set-up) shows. `sudo mmwatchdog validate` finds a missing file before a deploy |

After you correct the file named by `webhook_url_file`, the next attempt delivers the queued alerts.
In a fleet, reload the hub first, as [set up](#set-up) says. The errors that
every destination module can meet, such as a run past the time limit or a
module that is not installed on the node, are in
[destination modules](../../docs/reference/writing-modules.md#destination-modules).

## Tested on

- **Go tests against a stand-in for Mattermost** (`tests/official_extra_modules/mattermost_test.go`).
  The Go tests build `mmwatchdog` and run the script through
  `mmwatchdog run-starlark-module`, as MMWatchdog does. The script sends
  requests to an HTTP server inside the test that answers as Mattermost
  does. The Go tests cover:

  - The exact JSON sent for failures, recoveries, reminders, notices, hub
    messages that combine several alerts, and alerts with only a title.
    The message cases include alerts with and without a mention.
  - Backslashes and the invisible character in the first line, including a
    backslash before a letter as in `\x2d`, and Slack link notation.
  - Backticks, mentions, and Markdown in a body enclosed by lines of
    backticks longer than every row of backticks in the body.
  - A Mattermost installation whose URL includes a path prefix.
  - The cut at 16,383 characters.
  - A deleted webhook, using the response from the real Mattermost server
    in the services suite below and the response of older versions.
  - A missing channel, posting without channel permission, and disabled
    incoming webhooks.
  - Rate limits with and without a specified wait, oversized messages, and
    Mattermost errors.
  - A response with no error ID whose message contains the webhook's key.
  - A page and JSON from another server, and server text shortened to fit
    within the 300-byte error message.
  - HTTP 200 without `ok`, redirects that are not followed, an address
    where nothing listens, and a file without a valid webhook address.
  - Input validation and the absence of the webhook's key from all output.

  The Go tests ran with `go test` on macOS on ARM64.
- **The services suite on every target distribution**
  (`tests/integration/mattermost_scenarios.py`, see
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)).
  A real Mattermost Team Edition 11.11.1 server runs in the suite's
  test virtual machine. The suite sets up
  Mattermost with `mmctl` and reads posts through Mattermost's REST API.
  The module is installed in
  `/usr/local/lib/mmwatchdog/official_extra_modules` and runs as the account
  `mmwatchdog`.

  The module delivers a check's failure and recovery as exactly the posts that the module
  sent: with the mention, to a destination that takes only the title, and
  with a body that holds `@channel`, Markdown, and three backticks.
  The configured mention notifies the named person. Nothing in a title or body notifies
  anybody, including `@channel` after a line of three backticks or three
  tildes, and the notice title `@channel *x* <!here> getty@tty1.service`.

  A message cut to 16,383 characters arrives as one post. A deleted webhook
  fails with Mattermost's answer 400 in `mmwatchdog send-notice` and `mmwatchdog status`,
  and the queued notice arrives once the file holds a working webhook,
  without a reload. No output of MMWatchdog holds a webhook's key.

Not verified: how Mattermost's web, desktop, and mobile apps show the post,
including the escaped characters of the first line and the code block, and
the text of push notifications. The services suite reads the posts back
through Mattermost's REST API
([validation record](../../docs/changing-mmwatchdog/validation.md#not-verified-for-this-build)).

## Why it works this way

- **Verification sends no alert.** The invalid channel prevents a post. Mattermost checks the webhook first, so a channel refusal verifies the key without a message.

- **`mattermost` is its own module, not `slack`.** Mattermost takes Slack's
  format, but it reads mentions as `@barbara` and `@channel`, formats with
  its own Markdown, has backslash escapes, and keeps 16,383 characters in a
  post. A module that writes Mattermost's notation shows the alert as it is.
- **The body goes in a code block, and the first line is Markdown.** A code
  block shows the output of a check as it is and notifies nobody. The first
  line must be Markdown, so that the mention notifies, and the module
  escapes what the title could format or mention.
- **The lines of backticks around the body are longer than every row of
  backticks in the body.** Mattermost ends a code block only at a line of
  at least as many backticks as the line that opened it. So the body
  arrives unchanged, without a character replaced.
- **The module cuts at 16,383 characters.** Mattermost would split a longer
  text into several posts. The second post would begin in the middle of the
  body, outside the code block, where the output of a check could format and
  mention.
- **The request holds the text alone.** The webhook in Mattermost chooses
  the channel, the name, and the picture, and Mattermost ignores a name or a
  picture of a request unless an administrator allows them. One input fewer
  is one mistake fewer.
- **MMWatchdog writes the first line and the body; the module only places them.** An
  alert reads the same in Mattermost, in an email, and in MMHotline.
  No module writes wording of its own. A team that wants other wording writes a
  module of its own, which receives the whole alert event.
- **`mattermost` is an official extra module, not a core module.** A
  Mattermost server changes with its releases. The MMWatchdog project can
  update the script to support Mattermost changes in a new MMWatchdog
  release without a new `mmwatchdog` program.
- **The module sends with MMWatchdog's primitive `http_request`.** A node needs
  nothing but MMWatchdog to deliver an alert: no `curl` and no other tool.
- **The address of the webhook lives in a file that MMWatchdog reads, and its key
  stays out of every message.** The address is the credential, so it stays
  out of the configuration, version control, command lines, and output.
  Mattermost's message about a refusal names the key, so the module
  reports Mattermost's error ID instead of Mattermost's refusal message.
- **Only 200 with `ok` is delivered.** HTTP 200 with the text `ok` is
  Mattermost's success response. A proxy or another server that answers
  200 has not delivered the alert. A second post of the same alert is
  better than an alert that never arrives.
- **The module keeps nothing between two deliveries.** MMWatchdog's outbox owns the
  retries, the order, and the gaps. No answer of the module stops the
  retries: a module that said "do not try again" by mistake would lose an
  alert. So a recovery has a separate post, not a reply to the failure:
  a reply needs the post of the failure, which the webhook does not return.
