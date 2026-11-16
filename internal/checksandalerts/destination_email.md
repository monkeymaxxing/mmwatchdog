# `email`

`email` sends each alert as one email. MMWatchdog hands the email over SMTP to a
mail server: the program that accepts email and passes it on to the mailboxes
of the recipients, also called an SMTP server. MMWatchdog delivers to no mailbox
itself, so the destination needs a mail server that takes email from the
node, such as the mail service of the team or a mail server on the node
itself.

Choose `email` for people who read their alerts in a mailbox, and for an
address that turns each email into an SMS. For a chat, use an official extra
module such as [`slack`](../../official_extra_modules/slack/README.md); for a
program of the team's own, use [`webhook`](destination_webhook.md). A
destination names its module and states the settings that every destination
takes, as [the alert destinations file](../../docs/reference/configuration-files.md#the-alert-destinations-file)
describes.

## Set up

The destination states how its connection to the mail server is protected,
in `smtp_connection_security`. The setting is required and has no default,
so a reader sees on the destination how the connection is protected:

| `smtp_connection_security` | The connection to the mail server |
| --- | --- |
| `starttls` | Starts in plain text and then switches to encryption. A mail server that does not offer the switch fails the attempt |
| `implicit_tls` | Is encrypted from the first byte, as mail servers offer on port 465 |
| `unencrypted` | Is not encrypted |

`starttls`, `implicit_tls`, and `unencrypted` are also the values that the
[`smtp`](check_smtp.md), [`imap`](check_imap.md), and [`pop3`](check_pop3.md)
checks take as `connection_security`.

- **With `starttls` and `implicit_tls`, MMWatchdog checks the certificate and host
  name** with TLS 1.2 or later. The default uses the system's certificate
  authorities. `trusted_certificate_authority_file` replaces those authorities
  with the PEM certificates in that file. `dangerously_skip_server_certificate_check:
  true` disables certificate and identity checks; encryption remains.
- **A login needs `login_user` and `login_password_file` together.** MMWatchdog
  refuses `login_user` without `login_password_file`, and refuses
  `login_password_file` without `login_user`. The password file holds one
  line with the password:
  UTF-8 text of at most 1,024 bytes, without a tab or another control
  character. A space at the start or at the end of that line belongs to the
  password; only the line break at the end does not. MMWatchdog reads the file for
  every delivery, so a new password in the same file needs no reload.
  `sudo mmwatchdog validate` reads the file too, and names it when it is
  missing or holds anything other than one line.
- **MMWatchdog logs in with the method that mail servers call PLAIN,** or with the
  method LOGIN when the mail server offers only that. These are the two
  methods that mail servers offer for a name and a password.
- **`unencrypted` accepts a login to any mail server.** The password crosses
  the connection in plain text. Use a login over `unencrypted` only when the
  network itself protects the connection, such as a private WireGuard network.
- **Secret files follow the [credential permission rule](../../docs/reference/configuration-files.md#secret-files).**
  Place permission settings beside `email`, where `alert_severities` stands.
- **Each address is written plain and in ASCII,** such as `mmwatchdog@example.com`,
  with at most 254 characters. A display name is refused:
  `from: "MMWatchdog <mmwatchdog@example.com>"` is a validation error. `to` lists at least
  one address and no address twice.

## What it sends

The subject is the alert's
[title with the alert's severity in front](../../docs/reference/alert-format.md),
so a person sees in the list of emails how urgent each one is. The text is
the alert's body, then an empty line, then three lines that identify the
alert.

The check `http` belongs to the check group `myapp`, which has
`alert_severity: sos`. The check fails on the node `web-3` at 14:02 UTC, and the alert goes
out at 14:07. Each recipient of the destination `on_call_mailbox` gets
the following email:

```text
Subject: SOS: web-3: myapp/http failed

expected HTTP 200, received 503
failing since 14:02 UTC

Time of the alert: 2026-09-29 14:07:05 UTC
Incident ID: 9f2c4e6a8b0d1f3a5c7e9b1d3f5a7c9e
Event ID: 4b1d7f3a9c5e2b8d6f0a4c8e2b6d0f4a
```

- `Time of the alert` is when the alert happened, in UTC. The date of the
  email itself is the time of the delivery attempt, which is later when MMWatchdog
  had to try more than once.
- `Incident ID` is the same in the failure, each raise, each reminder, and
  the recovery of one incident. An alert without an incident, such as a
  [notice](../../docs/reference/commands.md#mmwatchdog-send-notice) or a
  [reboot notice](../../docs/how-it-works.md#reboot-notices), has no such line.
- `Event ID` identifies the individual alert event. The two IDs are the
  `incident_id` and the `id` that `mmwatchdog events --json` shows.
- The event ID is also the email's `Message-ID`, in the example
  `<4b1d7f3a9c5e2b8d6f0a4c8e2b6d0f4a@example.com>`, with the domain of
  `from`. Delivery is at least once, so an email can arrive twice after a
  retry, and a mail program can recognize a duplicate email by the event ID
  in the `Message-ID` header.
- The email goes out once, to all addresses in `to`, and each recipient sees
  the others in the `To` header.
- The email says that a program wrote it (`Auto-Submitted: auto-generated`),
  so a mailbox with an out-of-office reply does not answer it.
- The text is UTF-8. A subject with a character outside ASCII is encoded so
  that no line is longer than a mail server takes.

With [`send_only_the_alert_title: true`](../../docs/reference/alert-format.md),
the email's subject is still the alert's title with the severity in front,
but the email has no text at all, not even the three closing lines. The
`send_only_the_alert_title` setting is for an address that turns each email
into an SMS.

## When a delivery fails

**When an attempt counts as delivered.** The mail server accepting the email
acknowledges delivery. MMWatchdog knows that the mail server took the email; what
the mail server does with it afterwards, MMWatchdog cannot see.

`time_limit_per_delivery_attempt` limits the whole conversation with the
mail server, from connecting to the accepted email. For an email destination
`time_limit_per_delivery_attempt` is `30s` unless the destination states
another time; other destinations have `5s`. A mail server may take seconds
to accept an email, for example while it scans the email for spam. If
MMWatchdog gave up while the mail
server was still accepting the email, the mail server would still deliver
the email, MMWatchdog would send it again, and the recipients would get the
same alert once for each attempt.

**When an attempt fails.** Every failure is a failed attempt, and MMWatchdog tries
again as for every destination: after one second, then with the delay
doubling up to 256 seconds
([delivery](../../docs/how-it-works.md#the-outbox-and-delivery)). A failure
is a connection that does not open, a certificate that MMWatchdog does not trust, a
refused login, a refused sender, recipient, or message, the time limit, or a
password file that MMWatchdog cannot read.

A refused sender stops the attempt. A refused recipient does not block the
accepted recipients: MMWatchdog sends their email and reports every refused address
as the attempt's error. A retry sends to all recipients again, so an
accepted recipient can get a duplicate with the same event ID. Sending to
accepted recipients despite a refused address avoids losing an alert for
healthy mailboxes because one address is wrong.

When the mail server answered with a refusal, the error says what the mail
server refused and ends with the mail server's own answer. An error without
a refusal from the mail server, such as a connection failure, names the SMTP
step where the attempt failed. The error is one line, cut after 300 bytes.
`mmwatchdog send-notice` prints the error, and `mmwatchdog status` shows the
error in the line of the destination
([a destination that is failing](../../docs/when-something-fails.md#a-destination-is-failing)).
The [errors](#errors-and-what-they-mean) below list each one.

## Examples

### A mail service with a login

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  on_call_mailbox:
    email:
      smtp_server: smtp.example.com:587          # the mail server: its host name and its port
      smtp_connection_security: starttls         # how the connection to the mail server is protected
      from: mmwatchdog@example.com                       # the sender of every email
      to: [oncall@example.com, ops@example.com]  # each address gets every alert of on_call_mailbox
      login_user: mmwatchdog@example.com                   # the name that MMWatchdog logs in to the mail server with
      login_password_file: /etc/mmwatchdog/secrets/smtp        # the file that holds the password of mmwatchdog@example.com
    remind_every: 4h
    alert_severities: [warn, sos]
```

For each `warn` and `sos` alert, MMWatchdog on the standalone node:

1. reads the password from `/etc/mmwatchdog/secrets/smtp`;
2. connects to `smtp.example.com` on port 587, switches to encryption, and
   checks that the certificate is valid for `smtp.example.com`;
3. logs in as `mmwatchdog@example.com`, names the sender and the recipients
   `oncall@example.com` and `ops@example.com`, and sends one email.

While an incident is open, `oncall@example.com` and `ops@example.com` get a
reminder every four hours.

### The mail server on the same machine

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  operations_mailbox:
    remind_every: never
    email:
      smtp_server: localhost:25                  # the mail server on the MMWatchdog node
      smtp_connection_security: unencrypted
      from: mmwatchdog@app-1.example.com
      to: [operations@example.com]
    alert_severities: [warn, sos]
```

MMWatchdog hands each email to the mail server on the node itself, without
encryption and without a login, because nothing crosses the network. The
mail server on the node passes the email on to `operations@example.com`.

### An address that turns each email into an SMS

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  on_call_sms:
    remind_every: never
    email:
      smtp_server: smtp.example.com:587
      smtp_connection_security: starttls
      from: mmwatchdog@example.com
      to: [15551234567@sms-gateway.example.com]  # a gateway that turns each email into an SMS
      login_user: mmwatchdog@example.com
      login_password_file: /etc/mmwatchdog/secrets/smtp
    send_only_the_alert_title: true              # on_call_sms gets the title and no text
    alert_severities: [sos]
```

For each `sos` alert, the gateway gets an email with a subject such as
`SOS: web-3: myapp/http failed` and no text, and sends that line as an SMS.

## Test the destination

Quiet verification connects, applies the certificate policy, logs in when
configured, names the sender and every recipient, and sends `QUIT` before
`DATA`. It sends no email. A successful verification does not prove final
mailbox delivery; send a notice for that test.


Send a [notice](../../docs/reference/commands.md#mmwatchdog-send-notice) to the new destination
alone, on the standalone node or on the MMWatchdog fleet hub that holds the
destination. In the following test, one address in `to` is wrong:

```text
$ sudo mmwatchdog send-notice --alert-severity info --title "Test of the on-call mailbox. Nothing is wrong." --destination on_call_mailbox
Notice sent to the destination on_call_mailbox:
  on_call_mailbox   failed: the mail server smtp.example.com:587 refused the recipient ops@example.com: 550 5.1.1 User unknown
mmwatchdog: the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again
```

Correct the address and run `sudo mmwatchdog reload`. A change of the recipients
makes the destination a new one, so the reload cancels the waiting notice
([delivery](../../docs/how-it-works.md#the-outbox-and-delivery)); send the
notice again. When the line says `delivered`, the mail server has taken the
email. Look in each mailbox for it.

## How it works

- **One conversation for each delivery attempt.** MMWatchdog connects to
  `smtp_server`, starts the encrypted connection at once for
  `implicit_tls`, waits for the mail server's greeting, and greets it with
  the node's host name, which some mail servers require in place of
  `localhost`. With `starttls`, MMWatchdog switches to encryption. With a login, MMWatchdog
  logs in. Then MMWatchdog names the sender and each recipient, sends the email,
  and says goodbye. The email is delivered when the mail server has
  accepted it, whatever happens after that.
- **MMWatchdog names the sender without asking for anything more.** Every line of
  the email is ASCII or encoded, so it needs no feature that only some mail
  servers have, and a mail server that passes it on to another one can
  always deliver it.
- **MMWatchdog reads at most 1 MiB from a mail server** in one delivery attempt. No
  mail server needs more to take one email.
- **What makes the destination a new one** is its mail server, its list of
  recipients, and the path of its password file. The order of the
  recipients does not matter, and a change of `from`, `login_user`, or
  `smtp_connection_security` keeps the alerts that wait. A reload that
  changes the identity cancels the alerts that wait for the destination
  ([delivery](../../docs/how-it-works.md#the-outbox-and-delivery)).
- **In a fleet,** the email destination is in the hub's
  `alert_destinations.yml`, and the hub sends the emails. A joined node
  sends an email itself only when it delivers an alert without the hub
  ([alerts in a fleet](../../docs/how-to/set-up-a-fleet.md#where-alerts-go-in-a-fleet)). The node then
  connects to the mail server in `smtp_server` from its own place in the
  network, with the password from its copy of the fleet settings:
  - The mail server must be reachable from every node, and accept the login
    from every node.
  - `smtp_server: localhost:25` means the mail server on the node that sends
    the email: the hub's own for an alert that goes through the hub, and
    each joined node's own for an alert that the node delivers without the
    hub. A joined node without a mail server of its own fails the attempt
    to deliver through `localhost:25` and tries again. With `unencrypted`
    and a login, a joined node sends the password to the mail server on
    the joined node's machine.
  - The hub reads the password file when it starts and reloads, and gives
    the password to the joined nodes with its settings. After a change of
    the password, reload the hub.

## Errors and what they mean

`trusted_certificate_authority_file` must be absolute. Certificate settings
have no use with `smtp_connection_security: unencrypted`: remove them or
choose `starttls` or `implicit_tls`.

The validation errors, from `mmwatchdog validate`, `mmwatchdog reload`, and a deploy. MMWatchdog
writes the file and the line of the setting in front of each one:

| Error | What to do |
| --- | --- |
| `destinations.on_call_mailbox.email.smtp_server: expected the mail server's host name and port, such as smtp.example.com:587` | Write the port too: it has no default |
| `destinations.on_call_mailbox.email.smtp_connection_security: required: starttls, implicit_tls, or unencrypted` | State how the connection is protected |
| `destinations.on_call_mailbox.email.from: expected one email address in ASCII, such as mmwatchdog@example.com` | Write the address alone, without a display name such as `MMWatchdog <...>`. The error `expected one email address in ASCII, such as mmwatchdog@example.com` at `to.0`, `to.1`, and so on names an address in `to` |
| `destinations.on_call_mailbox.email.to: expected a list of at least one email address, such as [oncall@example.com]` | List at least one address |
| `destinations.on_call_mailbox.email.to.1: ops@example.com is listed twice` | Remove the second `ops@example.com` entry from `to` |
| `destinations.on_call_mailbox.email.login_password_file: required with login_user: the file that holds the password of mmwatchdog@example.com` | Set `login_user` and `login_password_file` together, or omit `login_user` and `login_password_file` |
| `destinations.on_call_mailbox.email.login_user: required with login_password_file: the name that MMWatchdog logs in to the mail server with` | Set `login_user` and `login_password_file` together, or omit `login_user` and `login_password_file` |
| `destinations.on_call_mailbox.email.login_user: expected one line of text of at most 256 bytes` | Shorten the login name, or remove control characters from the login name |
| `destinations.on_call_mailbox.email.login_password_file: must be absolute` | Write the whole path, starting with `/` |

The errors of a delivery attempt, from `mmwatchdog send-notice` and `mmwatchdog status`:

| Error | What happened |
| --- | --- |
| `the conversation with the mail server smtp.example.com:587 failed while MMWatchdog was connecting: ...` | MMWatchdog could not connect: nothing listens at `smtp.example.com:587`, the name does not resolve, or a firewall is in the way |
| `the mail server smtp.example.com:587 does not offer STARTTLS, which smtp_connection_security: starttls requires` | The mail server has no switch to encryption on port 587 at `smtp.example.com`. MMWatchdog does not go on without encryption |
| `the conversation with the mail server smtp.example.com:587 failed while MMWatchdog was switching to encryption: tls: failed to verify certificate: ...` | MMWatchdog does not trust the mail server's certificate, or the certificate names another host. With `implicit_tls` the step reads `starting the encrypted connection` |
| `the mail server smtp.example.com:587 offers no login on this connection, and the destination states login_user and login_password_file` | The mail server offers no login on the SMTP connection MMWatchdog opened to `smtp.example.com:587`. Some mail servers offer a login only on an encrypted connection |
| `the mail server smtp.example.com:587 offers no login that MMWatchdog can use: MMWatchdog uses PLAIN or LOGIN, and the mail server offers CRAM-MD5, GSSAPI` | The mail server offers only other login methods |
| `the mail server smtp.example.com:587 refused the login of mmwatchdog@example.com: 535 5.7.8 Authentication failed` | The name or the password is wrong |
| `the mail server smtp.example.com:587 refused the sender mmwatchdog@example.com: ...` | The mail server does not accept the address in `from` as a sender |
| `the mail server smtp.example.com:587 refused the recipient ops@example.com: ...` | The mail server does not take email for `ops@example.com` |
| `the mail server smtp.example.com:587 refused the message: ...` | The mail server took the addresses and then refused the email itself |
| `the mail server smtp.example.com:587 did not finish within 30s (time_limit_per_delivery_attempt); MMWatchdog was logging in` | The time limit passed. The last words say which step MMWatchdog was at: `connecting`, `starting the encrypted connection`, `waiting for the mail server's greeting`, `greeting the mail server`, `switching to encryption`, `logging in`, `naming the sender and the recipients`, or `sending the message` |
| `/etc/mmwatchdog/secrets/smtp: expected one line with the password: UTF-8 text of at most 1,024 bytes, without a tab or another control character` | The password file is empty, has more than one line, is too long, or holds a character that a password cannot have. `mmwatchdog validate` also reports the displayed password-format error |

No error holds the password.

## Tested on

- **Go tests,** on macOS and natively on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution),
  against a mail server inside the test that speaks SMTP and records what MMWatchdog
  sends (`internal/daemon/email_test.go`): `TestEmailSenderContract` (one
  message to every recipient over `starttls`, `implicit_tls`, and
  `unencrypted`; a PLAIN login, a LOGIN login, and no login; the subject,
  the text, the `Message-ID`, and `Auto-Submitted`), `TestEmailMessageText`
  (no text with `send_only_the_alert_title`, a subject outside ASCII, no
  line over 78 bytes, a notice without an incident line),
  `TestEmailSenderFailures` (each error above, a certificate that MMWatchdog does
  not trust with both kinds of encryption, a cancelled delivery; nobody gets
  a message, and the password is in no error), `TestReadSMTPPassword`,
  `TestEmailSettingsAreValidated`, `TestValidateRuntimeReadsTheEmailPasswordFile`,
  `TestEmailDestinationIdentity`, `TestTheDaemonDeliversAlertsByEmail`,
  `TestAJoinedNodeSendsEmailWithoutTheHub`, and
  `TestFleetSettingsCarryEmailPasswords`.
- **VM suites,** on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution),
  against three loopback-only Mailpit servers in each guest: STARTTLS with
  a required login, TLS from the first byte with a required login, and
  unencrypted SMTP without a login. The guest trusts their localhost
  certificate through its system trust store. The scenarios verify a check's
  failure and recovery
  over STARTTLS after a login, as one message for both recipients; an email
  without text for `send_only_the_alert_title: true`; `unencrypted` to
  `localhost` without a login; `mmwatchdog send-notice` over `implicit_tls` and with
  a title outside ASCII; a wrong password that fails the first attempt with
  the mail server's answer, after which the queued notice arrives once the
  file is right, without a reload; and `mmwatchdog validate` naming a
  missing password file. The fleet suite has a joined node that sends the
  email itself while the hub is down.
- **Not verified:** internet delivery and spam filtering. These cases
  are covered by the Go tests only: a refused sender, recipient, or message,
  a mail server without STARTTLS or without a login that MMWatchdog uses, a
  certificate that MMWatchdog does not trust, the time limit, reminders, and a
  reload that changes the destination's identity
  ([validation record](../../docs/changing-mmwatchdog/validation.md)).

## Why it works this way

- **Permission refusals belong to validation and reload.** Startup warns
  once per file and continues. Deliveries and check-ins keep reading
  secrets after a mode change, because refusing then would drop alerts.
  The [credential permission rule](../../docs/reference/configuration-files.md#secret-files)
  owns the settings and the full behavior.

- **Every email destination states `smtp_connection_security`.** The reader
  sees on the destination how the connection is protected. A mail server on
  the MMWatchdog node needs no encryption, so `unencrypted` stays possible.
- **`unencrypted` with a password is allowed to any host.** The explicit
  connection setting lets operators use a protected network without adding TLS.
- **Certificate checks stay enabled by default.** A private authority can
  replace the system store; skipping identity checks requires
  `dangerously_skip_server_certificate_check: true`, whose name makes the
  danger explicit, so the trust decision remains readable.
- **Quiet verification stops before `DATA`.** MMWatchdog logs in, names the sender
  and every recipient, and sends `QUIT`. The server checks the addresses
  without receiving a message to deliver.
- **The time limit is `30s`, not `5s`.** An attempt that gives up while the
  mail server still accepts the email sends a second copy at every retry.
- **Accepted recipients receive the message even when another address fails.**
  A duplicate on retry is preferable to an alert lost for every recipient.
- **MMWatchdog logs in with PLAIN or LOGIN, written in MMWatchdog itself.** These are the
  two methods that mail servers offer for a name and a password, and Go's
  own PLAIN method refuses a connection that is encrypted from the first
  byte.
- **MMWatchdog names the sender without parameters.** Go's mail client asks for
  SMTPUTF8, the feature for addresses outside ASCII, whenever a mail server
  offers it. A mail server that must then pass the email on to one without
  SMTPUTF8 accepts the email and cannot deliver the email. MMWatchdog's
  addresses are ASCII, so MMWatchdog needs no SMTPUTF8.
- **Plain addresses in ASCII, without a display name.** MMWatchdog writes each
  address as it is into the SMTP commands and into the headers.
- **The event ID is the `Message-ID`, and the IDs are in the text.**
  Delivery is at least once, and a person or a mail program can recognize a
  repeated email.
- **The text ends with the time of the alert.** The date of the email is the time of the
  attempt, which a retry makes later.
- **No text at all with `send_only_the_alert_title`.** An SMS gateway sends
  the subject alone, and the closing lines would only cost it characters.
- **The password lives only in a file, read for every delivery.** It stays
  out of the configuration, and a new password needs no reload.
