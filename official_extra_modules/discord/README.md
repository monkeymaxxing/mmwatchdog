# `discord`

`discord` delivers alerts to a channel on a Discord server. `discord` is an
[official extra module](../../docs/reference/modules/README.md): a
Starlark script that the MMWatchdog project writes, and that every deploy with the
release's MMX workflows installs on every node. An alert destination names
the module, and MMWatchdog runs the module once for each alert of that destination.

The module posts the alert to a Discord **webhook**: a web address that
Discord makes for one channel. Whatever is posted to the address appears as
a message in that channel, under the name that the webhook has in Discord.
The module uses the method
[Execute Webhook](https://docs.discord.com/developers/resources/webhook#execute-webhook)
of Discord's API.

Choose `discord` for a team that reads its alerts in Discord. For Slack, use
[`slack`](../slack/README.md); for a receiver of your own that reads MMWatchdog's
alert event as JSON, use the core module
[`webhook`](../../docs/reference/modules/webhook.md).

## Set up

Secret files follow the
[credential permission rule](../../docs/reference/configuration-files.md#secret-files).
Place permission settings beside `discord`, where `alert_severities` stands.

The node that delivers the alert needs:

- **Nothing but MMWatchdog.** The module sends the message through MMWatchdog itself, so the
  node needs no other program.
- **Outbound HTTPS access to `discord.com`.**
- **In a fleet, the module on every node, not only on the MMWatchdog fleet hub.** A
  joined node that delivers an alert without the hub runs its own copy
  ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)). The MMX
  workflows of the MMWatchdog release install every official extra module on every
  node, in `/usr/local/lib/mmwatchdog/official_extra_modules`.

The address of the webhook is a secret: anybody who has it can post to the
channel, and can delete the webhook. So the address lives in a file that
only root reads (mode `0600`), never in `alert_destinations.yml`. To make a
webhook, you need the permission Manage Webhooks on the Discord server:

1. In Discord, open the settings of the channel, then **Integrations**, then
   **Webhooks**, and make a new webhook
   ([Discord's guide](https://support.discord.com/hc/en-us/articles/228383668-Intro-to-Webhooks)).
2. Give the webhook a name that says where its messages come from, such as
   `MMWatchdog`. Discord shows the name above every alert.
3. **Copy Webhook URL** copies the address, such as
   `https://discord.com/api/webhooks/123456789012345678/AbC-dEf_123`. Write
   the address into the file, on the standalone node or on the MMWatchdog fleet hub:

   ```sh
   sudo install -m 0600 /dev/null /etc/mmwatchdog/secrets/discord-webhook-team
   sudo nano /etc/mmwatchdog/secrets/discord-webhook-team    # paste the address, one line
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

**To notify somebody,** write a mention into the input `mention`. A mention
names a person or a role by its number, the ID, which Discord shows once
**Developer Mode** is on (User Settings, then Advanced). Then right-click a
person and choose **Copy User ID**, or right-click a role in the server's
settings, under Roles, and choose **Copy Role ID**. A webhook may notify
only whom the channel lets everybody notify: for `@everyone` and `@here`,
the role `@everyone` needs the permission "Mention @everyone, @here, and All
Roles" in the channel, and a role is notified when that permission is there
or the role's setting "Allow anyone to @mention this role" is on.

## Inputs

| Input | Meaning | When omitted |
| --- | --- | --- |
| `webhook_url_file` | The absolute path of a file that holds the address of the webhook, such as `https://discord.com/api/webhooks/123456789012345678/AbC-dEf_123`, and nothing else | Required: `mmwatchdog validate` refuses the destination |
| `mention` | A Discord mention that starts every message of this destination, in quotes: `"<@123456789012345678>"` for a person by user ID, `"<@&165511591545143296>"` for a role by role ID, `"@everyone"` for every member of the channel, or `"@here"` for the members who are online. Only the configured mention notifies anybody | The message starts with the alert's title and notifies nobody in particular |

Write the mention in quotes: YAML refuses a value that starts with `@`.
Beside the inputs, a destination can state the run settings that every
destination module takes, such as `run_as_user_account`
([destination modules](../../docs/reference/writing-modules.md#destination-modules)).
`mmwatchdog describe-module discord` prints the inputs on a node.

## What it sends

The first line of the message is the alert's title with the alert's severity
in front, such as `SOS: app-1: myapp/http failed`
([title and body of an alert](../../docs/reference/alert-format.md)).
Below the first line comes the alert's body, in a **code block**: the text
between two lines of three backticks. Discord shows the body unchanged in a box. MMWatchdog writes the first line and the body, so the alert reads in
Discord as it reads
everywhere else. With `mention`, the mention starts the first line.

For a failure of `myapp/http` with `alert_severity: sos` on the node
`app-1`, the module sends:

````text
<@123456789012345678> SOS: app-1: myapp/http failed
```
expected HTTP 200, received 503
failing since 14:02 UTC
```
````

When the user ID is Barbara's, Discord shows the first line as
`@Barbara SOS: app-1: myapp/http failed` and notifies Barbara. A
notification on a phone shows the first line. The recovery of `myapp/http`
starts with `Resolved SOS: app-1: myapp/http is ok again`.

- **A recovery, a reminder, a [notice](../../docs/reference/commands.md#mmwatchdog-send-notice),
  a message in which the MMWatchdog fleet hub grouped the alerts of several nodes,
  and every other alert** are sent as the alert's first line and body.
- **A destination with `send_only_the_alert_title: true`** gets the first
  line, with the mention if it has one, and no code block.
- **The code block keeps the body as it is:** `*`, `_`, `~`, `#`, and `>` in
  the output of a check stay those characters and do not turn into
  formatting.
- **Three backticks in a row** would end the code block early, so the module
  sends them as three single quotes.
- **The first line is ordinary text,** so that the mention notifies. Discord
  reads formatting in the first line, so the module puts a backslash in
  front of each of `\`, `*`, `_`, `~`, `` ` ``, `|`, `<`, `>`, and `[`. Discord
  does not show the backslash: `my_app` stays `my_app`, and `<@999>` in the title of a
  notice stays that text and mentions nobody.
- **Only the mention notifies.** The module generates `allowed_mentions`,
  a field in the JSON message that tells Discord which mentions may notify
  people. The field permits only the configured person, role, or members,
  so a `<@...>` in the output of a check notifies nobody. An `@everyone` or
  `@here` in the title or the body gets an invisible character, a zero-width
  space, after its `@`, so it is no mention.
- **No preview.** Discord shows no preview of a web address in the alert.
- **A message has at most 2,000 characters,** the most that Discord takes.
  The module cuts a longer body and marks the cut with `…`. The first line
  and the end of the code block always arrive. Discord counts characters as
  the module does, also emoji. Should Discord still answer that the message
  is too long, the module sends the first 1,000 characters, so that the
  alert arrives. `sudo mmwatchdog events` on the node has the whole alert.

## When a delivery fails

Discord answers a message that Discord saved with HTTP status code 200 and
the message. Every other answer, and every failure to reach Discord, is a
failed attempt. The module then answers `failed` with a message that says
what Discord answered and what to do. MMWatchdog keeps the alert and tries again, as
for every destination: after one second, then with the delay doubling up to
256 seconds ([destination modules](../../docs/reference/writing-modules.md#destination-modules)).
Each attempt runs the module again.

While the attempts fail, `mmwatchdog status` shows the destination with the
module's message after the words `the module discord reported:`
([a destination that is failing](../../docs/when-something-fails.md#a-destination-is-failing)).
[Errors and what they mean](#errors-and-what-they-mean) lists each message.

## Examples

### Every alert to the team's channel, with a mention

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    discord:                                                   # the module
      webhook_url_file: /etc/mmwatchdog/secrets/discord-webhook-team   # the file that holds the webhook's address
      mention: "<@123456789012345678>"                         # starts every message
    alert_severities: [info, warn, sos]
```

The destination `team_chat` names the module `discord` and writes the
module's inputs below that name. What happens when a check fails, on the
standalone node or on the hub that holds
`/etc/mmwatchdog/alert_destinations.yml`:

1. The alert's severity is in `alert_severities`, so MMWatchdog queues one delivery
   for `team_chat`.
2. MMWatchdog reads `/etc/mmwatchdog/secrets/discord-webhook-team` and runs the module
   `discord` with the alert, the inputs, and the content of that file.
3. The module posts the message to the webhook: the mention, the title with
   the severity in front, and the body in a code block. Discord notifies the
   person with the user ID `123456789012345678`.
4. Discord answers 200 with the message that it saved, and the module
   answers `delivered`.

### The on-call role for `sos` alerts only

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    discord:
      webhook_url_file: /etc/mmwatchdog/secrets/discord-webhook-team
    alert_severities: [info, warn, sos]
  on_call_channel:
    remind_every: never
    discord:
      webhook_url_file: /etc/mmwatchdog/secrets/discord-webhook-on-call
      mention: "<@&165511591545143296>"                        # the role of the people on call
    alert_severities: [sos]
```

Two destinations use the module, each with the webhook of its own channel.
An `info` or `warn` alert goes to `team_chat` alone, and notifies nobody in
particular. An `sos` alert goes to `team_chat` and `on_call_channel`:
MMWatchdog runs the module once for each destination, and in the channel
of `on_call_channel` the message starts
with the role's mention, which notifies every member of the role.

### Every alert as one line

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  status_channel:
    remind_every: never
    discord:
      webhook_url_file: /etc/mmwatchdog/secrets/discord-webhook-status
    alert_severities: [info, warn, sos]
    send_only_the_alert_title: true                            # one line for each alert
```

Each alert arrives in the channel as one line, such as
`Warning: app-1: myapp/http failed`, without the body. The whole alert
stays in `sudo mmwatchdog events` on the node.

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
  team_chat   failed: the module discord reported: Discord answered 404: Unknown Webhook (code 10015). The webhook was deleted, or the address in the file is wrong. Write the address of a webhook from Discord into the file that webhook_url_file names.
mmwatchdog: the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again
```

### Verify without a message

MMWatchdog reads the webhook with `GET /api/v10/webhooks/{id}/{token}`. The request
sends no message. MMWatchdog discards the response body because the webhook object
contains the webhook's token. A 404 with Discord error code `10015`, or
error code `50027` for an invalid token, fails verification. Successful
verification does not prove that a forum webhook has a thread in which it
can post.

A connection or certificate failure, or any HTTP 5xx answer, also fails
verification. Every other answer passes, including a rate limit or a reply
whose meaning has not been confirmed. Accepting rate limits and responses
whose meaning has not been confirmed avoids reporting a bad credential
without evidence. A successful verification does not replace a test
notice for the full delivery path.

## How it works

One delivery attempt is one run of the script `module.star`, under the
destination's `time_limit_per_delivery_attempt`:

1. MMWatchdog reads the file that `webhook_url_file` names and starts the script as
   the destination's `run_as_user_account`, `mmwatchdog` unless stated.
2. The module checks that the file's content is one web address that starts
   with `https://` or `http://` and holds `/webhooks/`, without a space, a
   control character, a quote, or a backslash. Configuration validation has
   already checked that `mention`, when stated, is one of Discord's four forms.
3. The module asks for version 10 of Discord's API: it writes
   `/api/v10/webhooks/` in place of `/api/webhooks/` in the address. An
   address on `discordapp.com`, `ptb.discordapp.com`, or
   `canary.discordapp.com` goes to the corresponding host under today's
   name, such as `discord.com`. Those three `discordapp.com` hosts refuse
   version 10. The module leaves unlisted hosts as the webhook file states.
   The module adds `wait=true` to the address, so Discord answers only after saving the message.
   Discord answers with the saved message.
4. The module writes the message as [what it sends](#what-it-sends) says,
   and sends the message with MMWatchdog's primitive `http_request`, a function
   for HTTP requests, as an HTTP `POST` of JSON to the address: the `content`,
   the `allowed_mentions`, and the flag
   `SUPPRESS_EMBEDS` that turns the previews off. The header
   `User-Agent: MMWatchdog discord module` names the program, as Discord asks. The
   primitive follows no redirect and never writes the address into an
   error. An error can name the host, such as `discord.com`, but never the
   path behind it, which holds the secret.
5. When Discord answers that the content is too long, the module sends the
   first 1,000 characters in a second request.
6. The module answers `delivered` for a status code that starts with 2 and
   the saved message, and `failed` for everything else, with Discord's
   `message` and `code`, or with the title or the first line of a page that
   is not Discord's answer, and what to do.
   A server that is not Discord, such as a proxy, can repeat the address
   in its answer, so the module writes `[address of the webhook]` and
   `[token of the webhook]` in place
   of the address and of its token in every text of an answer. MMWatchdog shows at
   most 300 bytes of the module's message, so the module shortens the
   response text while keeping the full corrective advice.

## Errors and what they mean

| Message | What the message means and what to do |
| --- | --- |
| `destinations.team_chat.discord.username: unknown field "username": the inputs of the module discord are webhook_url_file, mention` | `mmwatchdog validate`: the destination states an input that the module does not have. Remove `username`. Discord shows the name of the webhook, which you change in Discord, and a webhook posts to its own channel |
| `destinations.team_chat.discord.webhook_url_file: is required: The absolute path of a file that holds the address of the Discord webhook, and nothing else. ...` | `mmwatchdog validate`: the destination leaves out `webhook_url_file` |
| `destinations.team_chat.discord.mention: expected a string` | `mmwatchdog validate`: the mention is a number. Write the `mention` value in Discord's notation and in quotes, such as `"<@123456789012345678>"` |
| `/etc/mmwatchdog/alert_destinations.yml: yaml: line 5: found character that cannot start any token` | `mmwatchdog validate`: a value starts with `@`, such as `mention: @here`. Write the `mention` value in quotes: `mention: "@here"` |
| `Discord answered 404: Unknown Webhook (code 10015). The webhook was deleted, or the address in the file is wrong. Write the address of a webhook from Discord into the file that webhook_url_file names.` | The webhook was deleted in Discord. Make a new webhook and write the new webhook's address into the file that `webhook_url_file` names |
| `Discord answered 401: Invalid Webhook Token (code 50027). The webhook was deleted, or the address in the file is wrong. ...` | The address in the file is not whole or not the webhook's. Copy the webhook address in Discord again and write the copied webhook address into the file that `webhook_url_file` names |
| `Discord answered 400: Webhooks posted to forum channels must have a thread_name or thread_id (code 220001). The webhook is in a forum channel, where a message needs a thread. Make a webhook for a text channel.` | Make a webhook for a text channel, and write its address into the file |
| `Discord answered 429: You are being rate limited. Discord takes no more messages from this webhook for 1 second.` | Nothing to do. Discord limits how fast one webhook posts, and MMWatchdog tries again |
| `Discord answered 429: You are being rate limited. Discord takes no requests from this node's network address for 65 seconds. The limit counts the requests of every program there.` | Discord's global limit counts every request from the node's network address, including requests from other programs using the node's network address. MMWatchdog tries again. If the global rate limit continues, find what else sends requests to Discord from the node's network address |
| `Discord answered 403: internal network error (code 40333). Cloudflare, in front of Discord, is blocking the request; Discord says that a proper User-Agent often resolves this. The module sends one, so check that no proxy on the way changes it.` | Discord documents the code 40333 as "Cloudflare is blocking your request. This can often be resolved by setting a proper User Agent". The module sends a `User-Agent` header, so check whether a proxy between the node and Discord replaces it. Otherwise the block ends after a time |
| `the server at the webhook's address answered 429 without Discord's JSON: Access denied \| discord.com used Cloudflare to restrict access. Cloudflare, in front of Discord, answers so while it blocks this node's network address, for a time after many requests.` | Cloudflare returned a web page saying requests from the node's network address were blocked. A 403 response with a Cloudflare block page also means Cloudflare blocked requests from the node's network address. The Cloudflare block ends after a time. If the Cloudflare block continues, find what else sends requests to Discord from the node's network address |
| `the server at the webhook's address answered 502 without Discord's JSON: discord.com \| 502: Bad gateway. Discord, or Cloudflare in front of it, had a fault. MMWatchdog tries again.` | Nothing to do unless the HTTP 5xx failure continues: Discord asks to wait a bit and try again. For any status code from 500 to 599, the module advises waiting and retrying, including responses with Discord's own JSON (`Discord had a fault. MMWatchdog tries again.`) |
| `the server at the webhook's address answered 404 without Discord's JSON: 404 Not Found. Check the address in the file that webhook_url_file names, and any proxy between this node and Discord.` | Something other than Discord answered, such as a proxy, or the file holds the address of another server |
| `MMWatchdog could not send the message to Discord: dial tcp: lookup discord.com: no such host` | The node did not reach Discord. The text after `Discord:` says why: here the node could not look up the name `discord.com` |
| `the file that webhook_url_file names must hold the address of one Discord webhook, which looks like https://discord.com/api/webhooks/123456789012345678/AbC-dEf_123, and nothing else` | The file holds something other than the address of a Discord webhook, such as the address of a Slack webhook. Write the webhook's address into the file, on one line |
| `mention: set mention to one Discord mention: "<@USER_ID>" for a person, "<@&ROLE_ID>" for a role, "@everyone", or "@here"` | The mention is none of the four forms, such as `"@Barbara"` or the old form `"<@!123456789012345678>"`. `mmwatchdog validate` and reload refuse the value. Set `mention` to `"<@USER_ID>"` for a person, `"<@&ROLE_ID>"` for a role, `"@everyone"`, or `"@here"`, replacing `USER_ID` or `ROLE_ID` with the Discord ID |
| `MMWatchdog cannot read the file of the input webhook_url_file of the module discord: open /etc/mmwatchdog/secrets/discord-webhook-team: no such file or directory` | MMWatchdog could not read the file, so it did not start the module. Write the file named by `webhook_url_file` as [set up](#set-up) shows. `sudo mmwatchdog validate` finds a missing file before a deploy |

After you correct the file named by `webhook_url_file`, the next attempt delivers the queued alerts.
In a fleet, reload the hub first, as [set up](#set-up) says. The errors that
every destination module can meet, such as a run past the time limit or a
module that is not installed on the node, are in
[destination modules](../../docs/reference/writing-modules.md#destination-modules).

## Tested on

- **Go tests against a stand-in for Discord** (`tests/official_extra_modules/discord_test.go`).
  The Go tests build `mmwatchdog` and run the script as MMWatchdog does,
  through `mmwatchdog run-starlark-module`, against an HTTP server inside
  the test that answers as Discord's API does.

  The Go tests check the exact JSON and HTTP request: API version 10,
  `wait=true`, replacement of the `discordapp.com` hosts, and the
  `User-Agent` request header. The message cases include a failure, a
  recovery, a reminder, a grouped message from the hub, a notice, and an
  alert with only a title. The mention cases include a person, a role,
  `@everyone`, `@here`, and no mention.

  The Go tests check that only the destination's mention notifies
  people, even when `@everyone`, `@here`, `<@...>`, and `<@&...>` appear in
  the title and body. Formatting checks cover backslashes in the first
  line, the body preserved in a code block, and replacement of three
  backticks. Length checks cover shortening to 2,000 characters and a
  second request limited to 1,000 characters after a length rejection.

  Failure checks cover a deleted webhook, a wrong token, a forum channel,
  webhook and global rate limits, Cloudflare blocks as JSON, a page, and
  text, another refusal, and a Discord error. Response checks cover fault
  pages from Cloudflare and another server, another server's page, and a
  response without the saved message. The Go tests check that redirects
  are not followed. Failure cases also include nothing listening at the
  address, a secret file holding no webhook address, and an invalid mention.

  Secret handling checks cover another server repeating the webhook
  address or path in a response, and the absence of the webhook address
  and token from output. The Go tests check that long response text is
  shortened while the full advice fits within the 300 bytes that MMWatchdog
  shows. The Go tests also check input validation.
  The Go tests ran with
  `go test ./tests/official_extra_modules/` on macOS on ARM64.
- **The acceptance suite on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)**
  (`tests/integration/official_extra_destination_module_scenarios.py`). The
  module is installed in `/usr/local/lib/mmwatchdog/official_extra_modules`, runs as
  the account `mmwatchdog`, and posts over HTTPS to a stand-in for Discord
  in the test's Linux virtual machine
  (`tests/integration/destination_services_stand_in.py`). A real
  check's failure and recovery go to
  `/api/v10/webhooks/<id>/<token>?wait=true` with the exact content,
  `allowed_mentions`, and flags 4, and `mmwatchdog send-notice` prints `delivered`.
  A deleted webhook fails with Discord's answer in `mmwatchdog send-notice` and
  `mmwatchdog status`, and the queued notice arrives once the file holds a working
  webhook, without a reload.
  No output of MMWatchdog and no line of its journal
  holds the webhook's token.

- **The real service test against Discord itself**
  (`tests/official_extra_modules/real_services_test.go`), which runs on a
  Mac before each release
  ([real services before a release](../../docs/changing-mmwatchdog/testing.md#real-services-before-a-release)).
  Discord stored a failure with the mention `@here`, which notified the
  channel. Discord also stored the recovery with `@everyone` and `@here` in a check's output,
  which notified nobody, and a text of 2,000 characters with 91 emoji.
  Each message arrived at the first attempt with exactly the content that the module
  sent. A wrong token got `401 Invalid Webhook Token (code 50027)` and the
  module's advice. The verification accepted the real webhook and refused
  the wrong token, without a message
  ([validation record](../../docs/changing-mmwatchdog/validation.md#real-alert-destinations)).

Not verified: whether a webhook may notify a role and `@everyone`, how a
phone's notification shows the backslashes of the first line, and
Discord's rate limits ([validation record](../../docs/changing-mmwatchdog/validation.md#not-verified-for-this-build)).

## Why it works this way

- **A malformed mention is refused during configuration validation.** A
  mention written in another service's notation cannot make every alert
  fail only when MMWatchdog tries to send it.

- **Verification sends no alert.** Reading the webhook checks its address and token without posting a message. The response object contains the token and must never appear in output.

- **The message is text, not an embed.** Discord's embeds are boxes with a
  title, a color, and fields. A mention notifies only from the text of a
  message, a notification on a phone shows that text, and a person who turns
  off link previews in Discord may see no embed at all. Discord also refuses
  an embed for more reasons, such as an empty field.
- **The body goes in a code block, and the first line is escaped.** A code
  block shows the output of a check as it is. The first line stays ordinary
  text, so that the mention notifies, and the backslashes keep the title as
  MMWatchdog wrote it.
- **Only the destination's mention notifies.** The output of a check can
  hold any text, also `@everyone`. The message names its one mention in
  `allowed_mentions`, and the zero-width space keeps `@everyone` and `@here`
  of an alert from being a mention, whatever Discord makes of the text.
- **`wait=true`.** Without `wait=true`, Discord answers 204 also for a
  message that Discord did not save. With `wait=true`, the module answers
  `delivered` only for a message that Discord saved.
- **Version 10 of Discord's API.** The address that Discord shows names no
  version, and reaches version 6, which Discord marks as deprecated.
  Version 6 answers a refused message with less detail, such as
  `{"embeds": ["0"]}`.
  Version 10 is the one that Discord's documentation describes. The hosts of
  Discord's former name, `discordapp.com`, answer version 10 with
  `Invalid API version` (code 50041), so the module asks `discord.com`, the
  same service.
- **No text of an answer holds the address.** The address is the secret,
  and a proxy or another server at the address can repeat it, or its path
  with the token, in its answer. MMWatchdog takes out only the secret file's whole
  content, which the address with version 10 no longer matches.
- **No input for the name or the thread.** Discord stores the webhook's
  name and channel, and a person changes the name and channel in Discord.
  One destination is one channel, as one webhook is.
- **`discord` is a module of its own.** Discord also takes Slack's format,
  at another address, but Discord's mentions, formatting, and limits are not
  Slack's, so `slack` would send them wrongly.
- **MMWatchdog writes the first line and the body; the module only places them.** An
  alert reads the same in Discord, in an email, and in MMHotline, and no
  module writes wording of its own. A team that wants other wording writes a
  module of its own, which receives the whole alert event.
- **`discord` is an official extra module, not a core module.** Discord is a
  service that one company runs, and it changes with that company. A script
  can support changes to Discord in a new release without a new
  `mmwatchdog` program.
- **The module sends with MMWatchdog's primitive `http_request`.** A node needs
  nothing but MMWatchdog to deliver an alert: no `curl` and no other tool.
- **The address of the webhook lives in a file that MMWatchdog reads.** The address
  is the credential, so it stays out of the configuration, version control,
  command lines, and output. The module needs no access to the file.
- **Each failed answer carries Discord's own words and what to do.** The
  person who reads `mmwatchdog status` at night learns what to fix without reading
  Discord's documentation.
- **The module keeps nothing between two deliveries.** MMWatchdog's
  outbox is the stored queue of undelivered alerts. MMWatchdog uses the
  outbox to manage retries, delivery order, and gaps in delivery. No
  answer from the module stops the retries: a module that said
  "do not try again" by mistake would lose an alert. Discord asks programs
  not to keep posting to a webhook that answers
  404, and blocks a network address after 10,000 refused requests in ten
  minutes. MMWatchdog's retries slow down to one every 256 seconds for each
  destination, far below 10,000 refused requests in ten minutes.
