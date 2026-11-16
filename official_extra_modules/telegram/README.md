# `telegram`

`telegram` delivers alerts to one Telegram chat: a group, a channel, or one
person. `telegram` is an
[official extra module](../../docs/reference/modules/README.md): a
Starlark script that the MMWatchdog project writes. Every deploy with the release's
MMX workflows installs the module on every node. An alert destination names
the module, and MMWatchdog runs the module once for each alert of the destination.

The module sends the alert as a **bot**: a Telegram account that a program
controls. The module uses the method `sendMessage` of the
[Telegram Bot API](https://core.telegram.org/bots/api#sendmessage).

Choose `telegram` for a team that reads its alerts in Telegram. For
on-call schedules and escalation, use
[`mmhotline`](../../docs/reference/modules/mmhotline.md), a core module built into
the `mmwatchdog` program. For a receiver of your own, use
[`webhook`](../../docs/reference/modules/webhook.md).

## Set up

Secret files follow the
[credential permission rule](../../docs/reference/configuration-files.md#secret-files).
Place permission settings beside `telegram`, where `alert_severities` stands.

The node that delivers the alert needs:

- **Nothing but MMWatchdog.** The module sends the message through MMWatchdog itself, so the
  node needs no other program.
- **A way out to `api.telegram.org`, or to the server in
  `bot_api_server_url`, over HTTPS.**
- **In a fleet, the module on every node, not only on the MMWatchdog fleet
  hub.**
  A joined node that delivers an alert without the hub runs the node's own copy
  ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)). The MMX
  workflows of the MMWatchdog release install every official extra module
  on every node, in `/usr/local/lib/mmwatchdog/official_extra_modules`.

The bot's token is a secret: anybody who has it can send messages as the
bot. So the token lives in a file that only root reads (mode `0600`), never
in `alert_destinations.yml`. To set up a bot and a chat:

1. In Telegram, talk to `@BotFather` and send `/newbot`. `@BotFather` answers
   with the bot's token. Write the token into the file, on the standalone
   node or on the MMWatchdog fleet hub:

   ```sh
   sudo install -m 0600 /dev/null /etc/mmwatchdog/secrets/telegram-bot
   sudo nano /etc/mmwatchdog/secrets/telegram-bot    # paste the token, one line
   ```

2. Add the bot to the group or the channel that should get the alerts. For
   alerts to one person, that person sends the bot a message first: a bot
   cannot write to a person who has never written to it.
3. Find the chat's number. Send a message in the chat, then open
   `https://api.telegram.org/bot<token>/getUpdates` in a browser, with the
   bot's token in place of `<token>`. The number is the `id` under `chat`.
   The number of a group or a channel is negative. Write the chat's number
   in quotes in `chat_id`.

MMWatchdog reads the token file for every delivery and gives the module
the file's content without the final line break. The module needs no
access to the token file. A new token in the token file needs no reload
on the machine reading the file. In a fleet, reload the hub after changing
the token file. The hub sends the file's content to joined nodes and reads
the file again only at startup and reload.

Telegram requires the token inside the address of every request. The token
appears in no command's arguments. MMWatchdog omits request addresses from
errors, so delivery messages contain no token.
[Destination modules](../../docs/reference/writing-modules.md#destination-modules) has the rules
that every such file follows.

## Inputs

`mmwatchdog validate` and `mmwatchdog reload` check `bot_api_server_url`
before contacting the service. An invalid address is refused at the
`bot_api_server_url` input line. MMWatchdog checks credential file contents
when using the credentials.

| Input | Meaning | When omitted |
| --- | --- | --- |
| `bot_token_file` | The absolute path of a file that holds the bot's token, such as `123456789:AAE-x1y2z3`, and nothing else | Required: `mmwatchdog validate` refuses the destination |
| `chat_id` | The chat that gets the alerts: the number of a chat in quotes, such as `"-1001234567890"`, or the name of a public channel, such as `"@mychannel"` | Required: `mmwatchdog validate` refuses the destination |
| `bot_api_server_url` | The address of the Bot API server, for a team that runs [its own](https://core.telegram.org/bots/api#using-a-local-bot-api-server) | The module uses Telegram's server, `https://api.telegram.org` |

A `chat_id` without quotes is a number, not text, so
`mmwatchdog validate` refuses the value. Beside the inputs, a destination can state
the run settings that every destination module takes, such as
`run_as_user_account`
([destination modules](../../docs/reference/writing-modules.md#destination-modules)).
`mmwatchdog describe-module telegram` prints the inputs on a node.

## What it sends

The first line of the message is the alert's title with the alert's severity
in front, and below it comes the alert's body
([title and body of an alert](../../docs/reference/alert-format.md)).
MMWatchdog writes the title and body, so Telegram shows MMWatchdog's alert
wording, as do the remaining destinations.
This is the message of a failed check with `alert_severity: sos` on the
node `app-1`:

```text
SOS: app-1: myapp/http failed
expected HTTP 200, received 503
failing since 14:02 UTC
```

The recovery alert for `myapp/http` on `app-1` starts with
`Resolved SOS: app-1: myapp/http is ok again`.

- **A recovery, a reminder, a [notice](../../docs/reference/commands.md#mmwatchdog-send-notice),
  and every other alert** are sent as the title with severity, followed by
  the body.
- **A destination with `send_only_the_alert_title: true`** gets the first
  line alone.
- **The module sends plain text.** Characters that Telegram could read as
  formatting, such as `*` and `_`, stay those characters. Telegram shows no
  preview of a web address in the alert.
- **A message has at most 4,096 characters,** the most that Telegram takes.
  The module cuts longer text at 4,096 characters and marks the cut
  with `…`. Telegram counts characters as the module does, also emoji.
  Should Telegram still
  answer that the message is too long, the module sends the first 2,048
  characters. `sudo mmwatchdog events` on the node has the whole alert.

## When a delivery fails

Telegram answers an accepted message with `"ok": true`. Every other answer,
and every failure to reach Telegram, is a failed attempt. The module then
answers `failed` with a message that says what Telegram answered and what to
do. MMWatchdog keeps the alert and tries again, as for every destination:
after one second, then with the delay doubling up to 256 seconds
([destination modules](../../docs/reference/writing-modules.md#destination-modules)). Each
attempt runs the module again.

While the attempts fail, `mmwatchdog status` shows the destination with the
module's message after the words `the module telegram reported:`
([a destination that is failing](../../docs/when-something-fails.md#a-destination-is-failing)).
[Errors and what they mean](#errors-and-what-they-mean) lists each message.

## Examples

### Every `sos` alert to the on-call group

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  on_call_phone:
    remind_every: never
    telegram:                                        # the module
      bot_token_file: /etc/mmwatchdog/secrets/telegram-bot   # the file that holds the bot's token
      chat_id: "-1001234567890"                      # the chat that gets the alerts
    alert_severities: [sos]
```

The destination `on_call_phone` names the module `telegram` and writes the
module's inputs below that name. What happens when a check fails with
`alert_severity: sos`, on the standalone node or hub that holds
`/etc/mmwatchdog/alert_destinations.yml`:

1. The alert's severity is in `alert_severities`, so MMWatchdog queues one delivery
   for `on_call_phone`. Alerts at `info` and `warn` do not reach the group.
2. MMWatchdog reads `/etc/mmwatchdog/secrets/telegram-bot` and runs the module `telegram`
   with the alert, the inputs, and the content of that file.
3. The module sends the title with the severity in front and the body to
   the chat `-1001234567890`, as the bot.
4. Telegram answers `"ok": true`, and the module answers `delivered`.

### Every alert to a public channel, title only

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  status_channel:
    remind_every: never
    telegram:
      bot_token_file: /etc/mmwatchdog/secrets/telegram-bot
      chat_id: "@mychannel"                          # a public channel, by its name
    alert_severities: [info, warn, sos]
    send_only_the_alert_title: true                  # one line for each alert
```

The bot is an administrator of the public channel `@mychannel`. Each alert
arrives in `@mychannel` as one line, such as
`Warning: app-1: myapp/http failed`, without the body. The whole alert stays
in `sudo mmwatchdog events` on the node.

### A team's own Bot API server

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  on_call_phone:
    remind_every: never
    telegram:
      bot_token_file: /etc/mmwatchdog/secrets/telegram-bot
      chat_id: "-1001234567890"
      bot_api_server_url: https://bot-api.example.com  # the team's own Bot API server
    alert_severities: [sos]
```

The module sends each `sos` alert to `https://bot-api.example.com` and not
to `api.telegram.org`. The node needs an outbound HTTPS connection only to
`https://bot-api.example.com` for Telegram delivery. The team's Bot API
server forwards the message to Telegram.

## Test the destination

`mmwatchdog send-notice` sends a message through the destination as a real alert
goes, and prints what happened. Run the following `mmwatchdog send-notice`
command on the standalone node or on the hub:

```sh
sudo mmwatchdog send-notice --alert-severity info --title "Test of the on-call phone. Nothing is wrong." --destination on_call_phone
```

```text
Notice sent to the destination on_call_phone:
  on_call_phone   delivered
```

The chat `-1001234567890` of `on_call_phone` then shows
`Info: Test of the on-call phone. Nothing is wrong.` The example destination
`on_call_phone` takes only `sos`. But a notice naming `on_call_phone`
reaches `on_call_phone` regardless of the destination's `alert_severities`.
When the delivery fails, the command prints the module's message after
`failed:` and keeps the notice queued.

### Verify without a message

MMWatchdog calls `getChat` with the bot token and `chat_id`, without sending
a message.
HTTP 401 with `ok: false`, `error_code: 401`, and `description: Unauthorized`
fails verification. HTTP 400 with `ok: false`, `error_code: 400`, and
`description: Bad Request: chat not found` also fails. A successful read
does not prove that the bot has permission to post in the chat.

A connection or certificate failure, or any HTTP 5xx answer, also fails
verification. Every other answer passes, including a rate limit or a reply
whose meaning has not been confirmed. Accepting responses other than the
listed failures avoids rejecting credentials without evidence of a bad
credential. A successful verification does not replace a test notice for
the full delivery path.

## How it works

One delivery attempt is one run of the script `module.star`, under the
destination's `time_limit_per_delivery_attempt`:

1. MMWatchdog reads the file that `bot_token_file` names and starts the script as
   the destination's `run_as_user_account`, `mmwatchdog` unless stated.
2. The module checks that the content supplied for `bot_token_file` has
   the form of a bot token: digits, a colon, and then letters, digits,
   underscores, and hyphens.
   Configuration validation has already checked `bot_api_server_url`.
3. The module sends the message with `http_request`, a function MMWatchdog
   provides to modules for HTTP requests. The module sends an HTTP `POST`
   of JSON to `<server>/bot<token>/sendMessage`: the `chat_id`,
   the `text`, and `link_preview_options` with `is_disabled: true`. The
   request names no parse mode, so Telegram formats nothing.
   `http_request` never writes the address, and so the token, into an
   error. An error can name the host, such as `api.telegram.org`, but never
   the path, which holds the token.
4. When Telegram answers that the message is too long, the module sends the
   first 2,048 characters in a second request.
5. The module answers `delivered` for `"ok": true`, and `failed` for every
   other answer, with the first 200 characters of Telegram's `description`.

## Errors and what they mean

| Message | What the message means and what to do |
| --- | --- |
| `destinations.on_call_phone.telegram.chat_id: expected a string` | `mmwatchdog validate`: `chat_id` is written without quotes. Write `chat_id: "-1001234567890"` |
| `destinations.on_call_phone.telegram.bot_token_file: is required: The absolute path of a file that holds the bot's token, ...` | `mmwatchdog validate`: the destination leaves out `bot_token_file`. A missing `chat_id` gets an `is required` error naming `chat_id` |
| `destinations.on_call_phone.telegram.token: unknown field "token": the inputs of the module telegram are bot_token_file, chat_id, bot_api_server_url` | `mmwatchdog validate`: the destination states an input that the module does not have. The token belongs in the file of `bot_token_file` |
| `Telegram answered 401: Unauthorized. Telegram does not know this bot token. Get the token of the bot from @BotFather in Telegram and write it into the file that bot_token_file names.` | The token in the file is wrong or was replaced. Telegram answers a wrong token with 401 or with 404. Get the token from `@BotFather` and write it into the file |
| `Telegram answered 400: Bad Request: chat not found. Check chat_id, and that the bot is a member of the chat.` | Correct `chat_id`, or add the bot to the chat |
| `Telegram answered 403: Forbidden: ... The bot may not write to this chat. Add the bot to the group or the channel. A person must first send the bot a message.` | Add the bot to the group or channel again. A person must send the bot a message first |
| `Telegram answered 429: Too Many Requests: retry after 35. Telegram takes no more messages from this bot for 35 seconds.` | Nothing to do. Telegram limits how fast a bot sends, and MMWatchdog tries again |
| `Telegram answered 400: Bad Request: group chat was upgraded to a supergroup chat. The group became a supergroup, which has a new number. Set chat_id to "-1001234567890".` | Set `chat_id` to the number in the message, and reload |
| `the Bot API server answered 502 without the JSON that Telegram answers with. Check bot_api_server_url, and what stands between this node and that server.` | Something between the node and the server answered, such as a proxy, or `bot_api_server_url` names another server. Check `bot_api_server_url` |
| `MMWatchdog could not send the message to Telegram: dial tcp: lookup api.telegram.org: no such host` | The node did not reach the server. The text after `Telegram:` says why: the node could not look up the name `api.telegram.org` |
| `the file that bot_token_file names must hold one bot token, which looks like 123456789:AAE-x1y2z3, and nothing else` | The file holds something other than the token. Write the token into the file, on one line |
| `bot_api_server_url: use one web address starting with https:// or http://, such as https://api.telegram.org; this is the service's API address` | Correct `bot_api_server_url` in the destination, and reload |
| `MMWatchdog cannot read the file of the input bot_token_file of the module telegram: open /etc/mmwatchdog/secrets/telegram-bot: no such file or directory` | MMWatchdog could not read the file, so it did not start the module. Write the file as [set up](#set-up) shows. `sudo mmwatchdog validate` finds a missing file before a deploy |

After you correct the token file, the next attempt delivers the queued
alerts. In a fleet, reload the hub first, as [set up](#set-up) says.
The errors that every destination module can meet, such as
a run past the time limit or a module that is not installed on the node,
are in [destination modules](../../docs/reference/writing-modules.md#destination-modules).

## Tested on

- **Go tests against a stand-in for Telegram** (`tests/official_extra_modules/modules_test.go`).
  The Go tests build `mmwatchdog` and run the script through
  `mmwatchdog run-starlark-module`, as MMWatchdog does. The script sends
  requests to an HTTP server inside the test that answers as the Bot API
  does. The Go tests check the exact `sendMessage`
  JSON for failure and recovery alerts, using numeric chat IDs in quotes
  and channel names. The Go tests check that the module uses
  Telegram's server unless `bot_api_server_url` names a custom server.

  The length tests check the cut at 4,096 characters. When Telegram refuses
  text as too long, the length tests check a second request limited to
  2,048 characters. The failure tests check wrong tokens, unknown chats,
  missing bot membership, rate limits with wait times, and groups changed
  to supergroups. Further failure cases cover another Telegram refusal,
  a response from a different server, and an address where nothing listens.

  The Go tests cover a file that holds no token and an invalid Bot API
  server address. The Go tests also check input validation and the
  absence of the token from output. The Go tests ran with `go test ./...`
  on macOS on ARM64.
- **The acceptance suite on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)**
  (`tests/integration/official_extra_destination_module_scenarios.py`).
  The test installs the module in
  `/usr/local/lib/mmwatchdog/official_extra_modules` in the test's Linux
  virtual machine. The module runs as the account `mmwatchdog`. A stand-in
  for the Bot API runs in the test's Linux virtual machine
  (`tests/integration/destination_services_stand_in.py`). The test sets
  `bot_api_server_url` to reach the stand-in over HTTPS.

  The acceptance suite verifies delivery of a check's failure and recovery,
  and a notice. A chat that Telegram does not know fails with Telegram's
  answer. No output of MMWatchdog and no line of the MMWatchdog journal
  holds the bot's token.

- **The real service test against Telegram itself**
  (`tests/official_extra_modules/real_services_test.go`), which runs on a
  Mac before each release
  ([real services before a release](../../docs/changing-mmwatchdog/testing.md#real-services-before-a-release)).
  Telegram stored a failure, its recovery, and a text of 4,096 characters
  with 192 emoji, each at the first attempt and with exactly the text that
  the module sent. A wrong token got `401 Unauthorized`, and a chat that
  Telegram does not know got `400 Bad Request: chat not found`.
  Each error included the module's advice for the error. Verification
  accepted the real bot and chat and refused the wrong token and unknown
  chat, without a message
  ([validation record](../../docs/changing-mmwatchdog/validation.md#real-alert-destinations)).

Not verified: Telegram's rate limits ([validation record](../../docs/changing-mmwatchdog/validation.md#not-verified-for-this-build)).

## Why it works this way

- **Input values are checked before delivery.** Configuration validation
  catches an invalid API address before an alert needs it. The module checks
  file contents only when MMWatchdog supplies them at runtime.

- **Verification sends no alert.** Reading the chat checks the token and chat address without sending a message or using a message quota.

- **MMWatchdog writes the first line and the body; the module only sends them.** An
  alert uses MMWatchdog's title and body wording in Telegram, email, and
  MMHotline. No module writes alert wording of the module's own. A team
  that wants different wording writes a module of the team's own, which
  receives the whole alert event.
- **Plain text, without a parse mode.** The request names no parse mode, so
  Telegram formats nothing, and the output of a check arrives as MMWatchdog wrote it.
- **2,048 characters at a second attempt.**
  In the real service test, Telegram counted a text with emoji as the
  module does. Should Telegram ever count a text as longer, half of the
  limit fits, and the alert arrives instead of failing at every attempt.
- **`bot_api_server_url` exists.** Telegram lets a team run its own Bot API
  server. The input `bot_api_server_url` also lets MMWatchdog's tests reach
  a stand-in, because the address of Telegram's own server is fixed.
- **`telegram` is an official extra module, not a core module.** Telegram is
  a service that one company runs, and Telegram changes with the company.
  The script can be updated for Telegram API changes in a new release
  without a new `mmwatchdog` program.
- **The module sends with MMWatchdog's primitive `http_request`.** A node needs
  nothing but MMWatchdog to deliver an alert: no `curl` and no other tool.
- **The token lives in a file that MMWatchdog reads.** The token is the credential,
  so it stays out of the configuration, version control, command lines, and
  output. The module needs no access to the file.
- **The module keeps nothing between two deliveries.** MMWatchdog's outbox owns
  the retries, the order, and the gaps. No answer from the module stops
  retries: a mistaken "do not try again" answer would lose an alert.
