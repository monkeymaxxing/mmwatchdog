# `tls`

`tls` opens a TLS connection from the node to an address and port, verifies
the server's certificate, and fails when the certificate expires soon.
The `tls` check completes the
handshake and sends nothing else, so the check works for any service that
speaks TLS, not only for HTTPS.

Choose `tls` to be warned before a certificate expires, and for a TLS port
that is not HTTP, such as IMAP on port 993. An [`http`](check_http.md)
check of an HTTPS address already verifies the certificate, so a separate
`tls` check is needed only for the warning before expiry or for a port that
is not HTTP. The connection is encrypted from its first byte, so `address`
must contain a host and a port that speaks TLS at once. Examples of TLS
ports are 443, 465, 993, and 995. A mail port that switches to encryption
with STARTTLS, such as 587, needs
[`smtp`](check_smtp.md), which verifies the certificate too.

## When the check fails, and when it cannot check

A run is healthy when the handshake completes, the certificate is valid,
and it expires later than the required `fail_when_certificate_expires_within`
time from now. The message is `TLS certificate is valid`.

The run fails when:

- **the certificate expires within `fail_when_certificate_expires_within`:**
  `TLS certificate expires at
  2026-11-20T12:00:00Z`, the time of expiry in UTC;
- **the handshake fails or the certificate is not valid,** such as a
  certificate that expired, one for another name, or one that no trusted
  certificate authority signed. The message is the cause in Go's words, such
  as `tls: failed to verify certificate: x509: certificate signed by unknown
  authority`;
- **the connection does not open,** or does not complete the handshake
  within the check group's `time_limit_per_run`.

PEM is a text format for certificates, with a `-----BEGIN CERTIFICATE-----`
line.

MMWatchdog cannot check, and the run is unknown, when it cannot read the
`trusted_certificate_authority_file`, or the file holds no PEM certificate, such as
`/etc/mmwatchdog/secrets/internal-ca.pem: contains no PEM certificates`. An unknown
run opens no failure incident. Unknown runs open a cannot-check incident of
their own after the same waiting times
([unknown results](../../docs/changing-mmwatchdog/design/checks.md#the-failing-time)).

A failed run starts or continues the check's
[failing time](../../docs/how-to/decide-when-a-check-alerts.md).

## Examples

### A mail server's certificate is valid for 30 more days

```yaml
# /etc/mmwatchdog/check_groups/mail_certificate.yml
check_groups:
  mail_certificate:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 15m
    recovered_after_healthy_for: 5m
    tls:
      address: mail.example.com:465
      server_name: mail.example.com
      fail_when_certificate_expires_within: 30d   # alert before the certificate expires
    alert_severity: warn
```

The check is written directly on the check group, so status shows it as
`mail_certificate/tls`. Every 5 minutes, MMWatchdog on the node connects to port 465
of `mail.example.com`, completes the TLS handshake, and verifies that the
certificate is valid for `mail.example.com` and signed by a certificate
authority that the node trusts. The run fails when the certificate expires
within 30 days. After 15 minutes of failing, MMWatchdog sends the `warn` alert.

Once the new certificate is installed, the check recovers after 5 healthy minutes.

### A server with a certificate of a private certificate authority

```yaml
# /etc/mmwatchdog/check_groups/internal_api_certificate.yml
check_groups:
  internal_api_certificate:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 15m
    recovered_after_healthy_for: 5m
    tls:
      address: 10.0.0.5:8443
      server_name: app.example.com
      trusted_certificate_authority_file: /etc/mmwatchdog/secrets/internal-ca.pem
      fail_when_certificate_expires_within: 14d
    alert_severity: warn
```

MMWatchdog connects to the address `10.0.0.5`, and verifies the certificate for
the name `app.example.com` and against the certificates of
`/etc/mmwatchdog/secrets/internal-ca.pem` only. The run fails when the certificate
expires within 14 days.

## How it works

- **The address is host:port,** with a port from 1 to 65535 and no default
  port.
- **`server_name` is the name that MMWatchdog sends to the server and verifies in
  the certificate.** Without `server_name`, the name is the host in `address`.
  State `server_name` when `address` contains an IP address or a host name
  different from the name in the certificate.
- **Verification is never skipped.** MMWatchdog trusts the system's certificate
  authorities, or, with `trusted_certificate_authority_file`, only the PEM
  certificates in `trusted_certificate_authority_file`. MMWatchdog reads
  `trusted_certificate_authority_file`, of at most 1 MiB, for each
  run. The connection uses TLS 1.2 or later.
- **The time left is the measurement `certificate_expires_in_seconds`:**
  the time until the server's own certificate expires. MMWatchdog records `certificate_expires_in_seconds` in the
  [measurement history](../../docs/how-to/look-back-at-measurements.md) like every
  measurement. A `tls` check takes no limit on `certificate_expires_in_seconds`:
  `fail_when_certificate_expires_within` is the limit.
- **A certificate that expires exactly at the end of
  `fail_when_certificate_expires_within`** fails the check.
- **`mmwatchdog validate` on the node reads `trusted_certificate_authority_file`**
  and refuses one that it cannot read or that holds no PEM certificate
  ([validation](../../docs/reference/commands.md#mmwatchdog-validate)).

## Errors and what they mean

`mmwatchdog validate` and `mmwatchdog reload` print each error after the file, the line, and
the column, which the table leaves out. The examples are for the check
`mail_certificate/tls` above.

| Message | What the message means, and what to do |
| --- | --- |
| `check_groups.mail_certificate.tls.address: address mail.example.com: missing port in address` | The address has no port. Write `address` as host:port, such as `mail.example.com:465` |
| `check_groups.mail_certificate.tls.address: port must be a number from 1 to 65535` | The port is not a number from 1 to 65535 |
| `check_groups.mail_certificate.tls.trusted_certificate_authority_file: must be absolute` | The path does not start with `/` |
| `check_groups.mail_certificate.tls.fail_when_certificate_expires_within: expected a duration of 0s or more, such as 30s, 5m, 26h, or 7d, found the string "-1d"` | The time is negative. Write the time before expiry at which the check fails, such as `30d` |
| `check_groups.mail_certificate.tls.fail_when_certificate_expires_within: expected a duration such as 30s, 5m, 26h, or 7d, found the string "a month"` | The time is not a number with a unit. Write the time as a number with a unit, such as `30d` |
| `check_groups.mail_certificate.fail_when_measurement_below: the module tls records its measurements but takes no limit on them` | The check states `fail_when_measurement_below` or `fail_when_measurement_above`. A `tls` check takes no such limit, also not on `certificate_expires_in_seconds`. Use `fail_when_certificate_expires_within` for the time before expiry |
| `check_groups.mail_certificate.tls.trusted_certificate_authority_file: /etc/mmwatchdog/secrets/internal-ca.pem: contains no PEM certificates` | On the node: the file holds no certificate in PEM form, the text that starts with `-----BEGIN CERTIFICATE-----` |
| `check_groups.mail_certificate.tls.trusted_certificate_authority_file: open /etc/mmwatchdog/secrets/internal-ca.pem: no such file or directory` | On the node: `/etc/mmwatchdog/secrets/internal-ca.pem` is missing, or MMWatchdog cannot read `/etc/mmwatchdog/secrets/internal-ca.pem` |

## Tested on

- **Go tests:** `TestTLSCAFile` in `internal/checksandalerts` (a certificate
  that only `trusted_certificate_authority_file` makes valid, the system's
  certificate authorities, and a file that is missing) and
  `TestRuntimeCredentialValidation` in `internal/daemon` (the certificate authority file at
  `mmwatchdog validate`). They ran on macOS ARM64 and, as the native
  suite, on the AMD64 guest of [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution).
- **The acceptance suite** (`tests/integration/acceptance.py`), on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution): two checks of a test server whose
  certificate is valid for one day and trusted only through
  `trusted_certificate_authority_file`; with `fail_when_certificate_expires_within:
  1h` the check is healthy, and with `48h` it fails.

## Why it works this way

- **The certificate warning time is required.** Certbot commonly renews with
  30 days left, while other certificates have shorter lifetimes. State
  `fail_when_certificate_expires_within` for the certificate being checked.

- **Verification is never skipped.** `trusted_certificate_authority_file` covers a
  server with a private certificate authority without teaching an insecure
  habit.
- **The check fails before expiry at the stated warning time.**
  An expired certificate is an outage; the warning leaves time to renew it.
- **An unreadable `trusted_certificate_authority_file` makes the run unknown,
  not failed.** MMWatchdog learned nothing about the server certificate being
  checked.
