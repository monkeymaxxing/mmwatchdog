# `smtp`

`smtp` checks a mail server that accepts mail. MMWatchdog talks SMTP with the
mail server: MMWatchdog waits for the server's greeting and greets the server
back. When the check's `connection_security` requires encryption, MMWatchdog
switches to encryption before leaving. The check sends no mail and logs in
nowhere.

For a mail server that serves mailboxes, use [`imap`](check_imap.md) or
[`pop3`](check_pop3.md). To be warned before the mail server's certificate
expires, add a [`tls`](check_tls.md) check on port 465, encrypted
from the first byte. A `tls` check does not negotiate STARTTLS.
The `smtp` check verifies certificate validity unless explicitly disabled.

`smtp`, `imap`, and `pop3` connect to a mail server in the same way:
`connection_security`, the verification of the server's certificate, and
`trusted_certificate_authority_file` work alike for all three, and the configuration files
reference describes them in
[the connection to a mail server](../../docs/reference/configuration-files.md#connection-to-a-mail-server-smtp-imap-and-pop3).
The `smtp` module page describes what is particular to SMTP.

## When the check fails, and when it cannot check

A run of the check is healthy when the whole conversation ends within the
check group's `time_limit_per_run`
([timings of a check group](../../docs/how-to/decide-when-a-check-alerts.md)).
The message says `the SMTP server at mail.example.com:587 answers, and its
certificate is valid`, or `the SMTP server at 127.0.0.1:25 answers` with
`connection_security: unencrypted`.

The run fails when:

- **The connection does not open.** The message is the reason that the
  node's system gives, such as `dial tcp 127.0.0.1:25: connect: connection
  refused`, or `the connection to 127.0.0.1:25 did not open within the time limit of
  the run`.
- **The encrypted connection does not start** with
  `connection_security: implicit_tls`, for example because the certificate
  is not valid: `the encrypted connection to mail.example.com:465 did not
  start:` and the reason.
- **A step of the conversation fails.** The message names the step, such
  as `the SMTP server at mail.example.com:587: its greeting: no answer
  within the time limit of the run`. The steps are `its greeting`, `its
  answer to MMWatchdog's greeting`, `the switch to encryption`, and `its answer to
  QUIT`. After the step comes the server's answer, or the reason, such as a
  certificate that is not valid.
- **The server does not offer STARTTLS** with
  `connection_security: starttls`: `the SMTP server at
  mail.example.com:25 does not offer STARTTLS, which connection_security:
  starttls requires`.
- **The server sends more than MMWatchdog reads,** 64 KiB in one run: the message
  of the step ends with `the server sent more data than MMWatchdog reads from the server`.

The check is unknown when MMWatchdog cannot read the file that
`trusted_certificate_authority_file` names. The message names the file and the
reason, such as `open /etc/mmwatchdog/secrets/mail-ca.pem: no such file or
directory`. A check that stays unknown for `alert_after_failing_for` sends
a cannot-check alert
([scheduling and observed state](../../docs/changing-mmwatchdog/design/checks.md#scheduling-and-observed-state)).

Every failed run counts toward the check's
[failing time](../../docs/how-to/decide-when-a-check-alerts.md), which decides when the
alert goes out.

## Examples

### The submission port of a mail server

```yaml
# /etc/mmwatchdog/check_groups/mail_server.yml
check_groups:
  mail_server:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 3m
    recovered_after_healthy_for: 1m
    checks:
      mail_server_accepts_mail:
        smtp:
          address: mail.example.com:587
          connection_security: starttls
        alert_severity: sos
```

Once a minute, MMWatchdog on the node connects to port 587 of
`mail.example.com`. Port 587 accepts mail from programs and people.
MMWatchdog waits for the greeting and greets the server.
MMWatchdog asks the server to switch to encryption and verifies the server's
certificate for the name `mail.example.com`. Then MMWatchdog says goodbye
with `QUIT`. When the check has been failing for 3 minutes, MMWatchdog opens
an incident and sends the `sos` alert.

### The mail server on the node itself, and a private certificate authority

```yaml
# /etc/mmwatchdog/check_groups/local_mail.yml
check_groups:
  local_mail:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 3m
    recovered_after_healthy_for: 1m
    checks:
      local_mail_server_accepts_mail:
        smtp:
          address: 127.0.0.1:25
          connection_security: unencrypted
        alert_severity: sos
      relay_accepts_mail:
        smtp:
          address: relay.internal.example.com:465
          connection_security: implicit_tls
          trusted_certificate_authority_file: /etc/mmwatchdog/secrets/internal-ca.pem
        alert_severity: warn
```

The first check talks to the mail server of the node itself, on port 25,
without encryption: the conversation never leaves the node. The second
check connects to a relay on another machine with encryption from the
first byte. The `relay_accepts_mail` check trusts the certificates in
`/etc/mmwatchdog/secrets/internal-ca.pem` instead of the node's system
certificates, because a private certificate authority signed the relay's
certificate. MMWatchdog reads `/etc/mmwatchdog/secrets/internal-ca.pem` again
for each run.

## How it works

1. MMWatchdog opens a TCP connection to the address, written as host:port. With
   `implicit_tls`, MMWatchdog starts the encrypted connection at once and verifies
   the certificate.
2. MMWatchdog waits for the server's greeting, a line that starts with `220`.
3. MMWatchdog greets the server with `EHLO` and the node's host name, which some
   mail servers require in place of `localhost`. A host name with
   characters that a greeting cannot hold is replaced with `localhost`.
4. With `starttls`, MMWatchdog looks in the server's answer for the offer of
   `STARTTLS`, sends `STARTTLS`, and verifies the certificate.
5. MMWatchdog sends `QUIT` and closes the connection.

The usual ports are 25 for mail between servers and 587 for mail from a
program, both with `starttls`, and 465 with `implicit_tls`.

## Errors and what they mean

The validation errors of `address`, `connection_security`, and
`trusted_certificate_authority_file` are the same for `smtp`, `imap`, and `pop3`.
The configuration files reference lists the validation errors for the mail server inputs in
[the connection to a mail server](../../docs/reference/configuration-files.md#connection-to-a-mail-server-smtp-imap-and-pop3).
The messages of a run are under
[when the check fails](#when-the-check-fails-and-when-it-cannot-check).

## Tested on

- **Go tests:** `TestMailServerChecks` in
  `internal/checksandalerts/protocol_checks_test.go` runs the check against a stand-in
  mail server with each `connection_security`: a certificate that only
  `trusted_certificate_authority_file` makes valid, a file that is missing at a run
  and at `mmwatchdog validate`, a server of another protocol, a silent
  server, and a server that does not offer STARTTLS. `TestMailServerChecks` also tests the
  validation errors. `TestChecksOfServersThatDoNotAnswerAsExpected` sends
  lines of a greeting without end, and the check stops reading at the reading
  limit. `TestMailServerChecks` and `TestChecksOfServersThatDoNotAnswerAsExpected` run on the machine that runs the suites and on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution).
- **On a node:** the services scenarios (`tests/integration/service_scenarios.py`)
  check Postfix as each distribution installs it, on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution): port 25 with `unencrypted` and `starttls`, and port 465 with
  `implicit_tls`, each healthy; a certificate that the node does not trust
  fails the switch to encryption; a stopped Postfix fails the checks; and
  Postfix with its encryption turned off fails the check with `starttls`
  and answers the check with `unencrypted`.
- **Not verified on a node:** a mail server that sends without end. The Go
  tests cover a mail server that sends without end.

MMWatchdog speaks SMTP itself and runs no program of the node, so the distribution of the mail server changes
nothing for the check. The [validation record](../../docs/changing-mmwatchdog/validation.md)
has the latest results.

## Why it works this way

- **The check sends no mail and logs in nowhere.** A mail server that
  greets, answers, and switches to encryption accepts connections; a check
  that needs no password leaves no credential on the node.
- **`connection_security` is required, with no default,** as on an
  [email destination](destination_email.md). A reader sees on each check
  how the connection is protected
  ([protocols and common server software](../../docs/changing-mmwatchdog/design/checks.md#protocols-and-common-server-software)).
- **MMWatchdog verifies certificates by default.** A private authority can be named
  with `trusted_certificate_authority_file`.
  `dangerously_skip_server_certificate_check: true` permits encryption
  without server authentication when the operator explicitly chooses it.
- **The address has no default port.** A reader sees on each check where
  it connects.
- **A failed run names the step.** The person who reads the alert knows at
  once whether the server did not answer, refused MMWatchdog, or failed the
  encryption.
- **MMWatchdog reads at most 64 KiB in one run.** A server that sends without end
  cannot make the check take the node's memory.
- **`smtp` is a core module.** Checking that a mail server accepts mail is common work, so the check is built into `mmwatchdog` and tested
  with `mmwatchdog`.
