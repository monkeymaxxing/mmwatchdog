# `dns`

`dns` checks a DNS server. The `dns` module asks one DNS server for records
of one type for one name, such as the `A` records of `www.example.com`.
Without `expected_record_values`, an answer with at least one record makes
the check healthy. But the check fails if the answer
says that the domain takes no mail or that the service is not available.
With `expected_record_values`, the answer must hold exactly the configured
values.

MMWatchdog writes the DNS question itself and sends it to the server that the
check names, and to no other. The node's resolver, `/etc/hosts`, and
resolver settings take no part, so the answer is always the server's.
Choose `dns` to check a DNS server. To check only that
a machine answers under its name, an [`http`](check_http.md),
[`tcp`](check_tcp.md), or [`ping`](check_ping.md) check of that name is
enough.

## When the check fails, and when it cannot check

For a healthy run, the server must answer within the check group's
`time_limit_per_run`
([timings of a check group](../../docs/how-to/decide-when-a-check-alerts.md)).
The answer must contain at least one record. With
`expected_record_values`, the answer must hold exactly the configured
values. The check's message gives the answer, with the values in sorted
order: `the A record of www.example.com at the DNS server 10.0.0.2:53 is
192.0.2.10, 192.0.2.11`.

The run fails when:

- **The answer holds other values than `expected_record_values`.** Another
  value, a value too many, and a value too few all fail the check, with the
  answer and what was expected: `the A record of www.example.com at the DNS
  server 10.0.0.2:53 is 192.0.2.10, 192.0.2.11, expected 192.0.2.10`.
- **The server has no such record,** because the name does not exist or
  has no record of the requested type:
  `the DNS server 10.0.0.2:53 has no MX record
  of www.example.com`.
- **The server answers that the domain takes no mail, or that the service
  is not available,** and the check has no `expected_record_values`. A
  domain that takes no mail publishes the null MX: one `MX` record whose
  host is the root name, written `.`. An `SRV` record whose
  target is `.` says that the service is not available at the queried domain.
  The messages:
  `the DNS server 10.0.0.2:53 answers that example.com takes no mail: its
  MX record is the null MX (.)`, and `the DNS server 10.0.0.2:53 answers
  that the service _sip._tcp.example.com is not available: its SRV record
  names no host (its target is .)`. To watch a domain that must take no
  mail, expect the value `.`
  ([a domain that takes no mail](#a-domain-that-takes-no-mail)).
- **The server refuses the question,** as a server does for a name that it
  does not serve: `MMWatchdog got no A record of www.elsewhere.example.org at the
  DNS server 10.0.0.2:53: the DNS server refused the query`. Other errors
  of the server also fail the run: `the DNS server reported a failure while processing the query`,
  `the DNS server returned error` and the error's name,
  `the DNS server sent an invalid response or a response to a different query`,
  and `the DNS server sent a response that MMWatchdog cannot read`.
- **No answer arrives within `time_limit_per_run`:** `the DNS server
  10.0.0.2:53 did not answer within the time limit of the run`.
- **The node's system reports that no program listens** on the server's
  machine at the port in `dns_server`:
  `no DNS server listens at 127.0.0.1:5353`.
- **The node cannot find the address of the server,** when `dns_server` is
  a host name: `MMWatchdog got no A record of ... at the DNS server ...:` and the
  resolver's reason.

MMWatchdog can always run a `dns` check, so the `dns` module never reports
an unknown observation for a module-specific reason. A `dns` check is
unknown only for reasons shared by every check,
such as a run that does not end
([scheduling and observed state](../../docs/changing-mmwatchdog/design/checks.md#scheduling-and-observed-state)).
Every failed run counts toward the check's
[failing time](../../docs/how-to/decide-when-a-check-alerts.md), which decides when the
alert goes out.

## Examples

### The internal DNS server knows the application

```yaml
# /etc/mmwatchdog/check_groups/internal_dns.yml
check_groups:
  internal_dns:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 3m
    recovered_after_healthy_for: 1m
    checks:
      internal_dns_knows_the_application:
        dns:
          dns_server: 10.0.0.2:53
          record_name: app.internal.example.com
          record_type: A
          expected_record_values: [10.0.0.5]
        alert_severity: sos
      internal_dns_knows_the_name_of_the_address:
        dns:
          dns_server: 10.0.0.2:53
          record_name: 10.0.0.5
          record_type: PTR
          expected_record_values: [app.internal.example.com]
        alert_severity: warn
```

Once a minute, MMWatchdog on the node asks the DNS server at `10.0.0.2`, port 53,
two questions. The first check asks for the address of
`app.internal.example.com` and is healthy only when the answer is
`10.0.0.5` and nothing else. The second check asks for the name of the
address `10.0.0.5`: for `record_type: PTR`, `record_name` is the address.
MMWatchdog asks DNS under the name where DNS keeps the address,
`5.0.0.10.in-addr.arpa`. When a check has been failing for 3 minutes, MMWatchdog
opens its incident and sends its alert.

### The mail exchanger of a domain, and the node's own resolver

```yaml
# /etc/mmwatchdog/check_groups/dns_of_the_domain.yml
check_groups:
  dns_of_the_domain:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 15m
    recovered_after_healthy_for: 5m
    checks:
      domain_has_a_mail_exchanger:
        dns:
          dns_server: ns1.example.com:53
          record_name: example.com
          record_type: MX
          expected_record_values: [mail.example.com]
        alert_severity: warn
      resolver_of_this_node_answers:
        dns:
          dns_server: 127.0.0.53:53
          record_name: www.example.com
          record_type: A
        alert_severity: warn
```

Every 5 minutes, MMWatchdog on the node asks two questions. The first check
asks the name server `ns1.example.com` for the mail exchanger of
`example.com`. The node's resolver finds the address of
`ns1.example.com`, and the question then goes only to `ns1.example.com`.
When `ns1.example.com` has several addresses, MMWatchdog asks each address
in turn until an address answers.

The second check asks the node's own resolver, which systemd-resolved runs
at `127.0.0.53:53`. The second check has no
`expected_record_values`, because the addresses of `www.example.com` may
change, as for a name behind a content delivery network. An answer with at
least one record is healthy. When a check has been failing for 15
minutes, MMWatchdog opens its incident and sends its `warn` alert.

### A domain that takes no mail

```yaml
# /etc/mmwatchdog/check_groups/dns_of_the_domain.yml
check_groups:
  dns_of_the_domain:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 15m
    recovered_after_healthy_for: 5m
    checks:
      domain_takes_no_mail:
        dns:
          dns_server: ns1.example.com:53
          record_name: static.example.com
          record_type: MX
          expected_record_values: ["."]
        alert_severity: warn
```

The domain `static.example.com` serves a website and takes no mail. The
name server of `static.example.com` publishes the null MX for
`static.example.com`, so that mail servers refuse mail to the domain at
once instead of trying to deliver it for days. Every 5
minutes, MMWatchdog on the node asks `ns1.example.com` for the `MX` records of
`static.example.com`. The check is healthy while the answer is the null MX
alone, with the message `the MX record of static.example.com at the DNS
server ns1.example.com:53 is .`. When someone gives the domain a mail
server, or removes the null MX, the check fails, and after 15 minutes MMWatchdog
sends its `warn` alert. YAML does not need the quotes around the dot, but
the quotes show the reader that the dot is a value. For an `SRV` record of
a service that the domain must not offer, expect `.` too. The quotes around
the `SRV` value `.` are also optional.

## How it works

1. MMWatchdog writes the question for the records of `record_type` for
   `record_name`. The name is taken as complete: MMWatchdog adds no search domain
   of the node to it, so the check never gets the answer for another name.
2. MMWatchdog sends the question in one UDP packet to `dns_server`. When
   `dns_server` is a host name with several addresses, MMWatchdog asks each
   address in turn and gives each address a share of the remaining time.
3. When the server says that its answer does not fit a packet, MMWatchdog asks
   again over TCP.
4. MMWatchdog takes records matching `record_type` from the answer and skips
   records of different types. For example, an `A` answer can contain a
   `CNAME` record of a name before the address of the name represented by
   the `CNAME`.

A value is written in the form of its kind of record: an address for `A`
and `AAAA`, a host name for `CNAME`, `MX`, `NS`, and `PTR`, the text for
`TXT`, and host:port for `SRV`, such as `sip.example.com:5060`. The root
name is written `.`, never as an empty value, including the host of the null
MX and the target of an `SRV` record. An `SRV` value with target `.` has no port.

A `TXT` record with several text pieces is one value. When MMWatchdog
compares the answer with `expected_record_values`, host name case and a
final dot make no difference. The two ways of writing an IPv6 address
also make no difference. The values may be written in any order. A reload
that only reorders `expected_record_values` keeps the existing check.

`record_name` must be a name that DNS can hold: ASCII characters, parts of
1 to 63 characters between dots, and at most 253 characters in all. A name
with non-ASCII characters is written in the name's `xn--` form, such as
`xn--bcher-kva.example.com` for `bücher.example.com`. A name with a star,
such as `*.example.com`, is asked for as it is written.

## Errors and what they mean

The table below lists validation errors. `mmwatchdog validate` and
`mmwatchdog reload` print each validation error after the file, line, and
column, with the check's path in front, such as
`check_groups.internal_dns.checks.internal_dns_knows_the_application.dns.`

| The error | Meaning and action |
| --- | --- |
| `dns_server: is required: the DNS server that the check asks, as host:port, such as 10.0.0.2:53` | `dns_server` is missing, or it has no port or a port that is not a number from 1 to 65535. |
| `record_type: must be one of A, AAAA, CNAME, MX, NS, TXT, PTR, SRV` | `record_type` is missing or names another kind. Write the kind in capital letters. |
| `record_name: is required: the name that the check asks for, such as www.example.com` | `record_name` is missing or empty. |
| `record_name: holds a space, which a DNS name cannot hold` | The name holds a space, a tab, a line break, or a slash; the message names which. A URL such as `https://www.example.com/` is no DNS name: write the host name alone. |
| `record_name: has 261 characters: a DNS name has at most 253 characters` | The name is too long. A final dot in the name does not count. |
| `record_name: must be an address for record_type: PTR, such as 192.0.2.10: a PTR record gives the name of an address` | With `record_type: PTR`, write the address whose name the check asks for. |
| `record_name: each part of a DNS name between dots must have 1 to 63 characters` | A part of the name is empty, as in `www..example.com`, or longer than 63 characters. |
| `record_name: a DNS name must contain only printable ASCII characters without spaces; use the xn-- form for a name with non-ASCII characters` | Write the name in its `xn--` form. |
| `expected_record_values: cannot be empty: list the values that the answer must hold, or remove expected_record_values` | The list is empty. List the values, or omit `expected_record_values` to accept any answer with a record. |
| `expected_record_values: expected distinct values that are not empty` | A value is empty or written twice. Two ways of writing one value, such as `Mail.example.com.` and `mail.example.com`, count as the same value. |
| `expected_record_values: expected a list, found the string "10.0.0.5"` | `expected_record_values` is written as one value, such as `expected_record_values: 10.0.0.5`. Write the values as a list, also a single one: `[10.0.0.5]`. |

A run without `expected_record_values` fails with a message below when
the server answers that nothing is available:

| The message | Meaning and action |
| --- | --- |
| `the DNS server 10.0.0.2:53 answers that example.com takes no mail: its MX record is the null MX (.)` | The domain publishes the null MX, which says that the domain takes no mail. If the domain should take mail, name the domain's mail servers in DNS instead of the null MX. If `example.com` must take no mail, write `expected_record_values: ["."]`. |
| `the DNS server 10.0.0.2:53 answers that example.com takes no mail: one of its MX records is the null MX (.); its MX records are ., mail.example.com` | The domain publishes the null MX beside other `MX` records, which RFC 7505 forbids. Mail servers may treat the domain as one that takes no mail. Remove the null MX or the `MX` records that name mail servers. |
| `the DNS server 10.0.0.2:53 answers that the service _sip._tcp.example.com is not available: its SRV record names no host (its target is .)` | The `SRV` record says that the domain does not offer the service. If the domain should offer the service, name the host and the port in the record. If the domain must not offer the service, write `expected_record_values: ["."]`. |

## Tested on

- **Go tests:** `TestDNSCheck` in `internal/checksandalerts/protocol_checks_test.go`
  asks a stand-in DNS server for the eight record types. The configurations
  vary the order and spelling of expected values and cover an extra value
  and a missing value. Separate conditions cover a name without a
  record, no search domain added to the requested name, a refusal, and a
  server that does not answer.

  `TestDNSCheck` also covers a name in the hosts file of the machine
  running `TestDNSCheck`, a name with a star, and IPv6 input for `PTR`.
  `TestDNSCheck` covers an answer over TCP, a server host name with two
  addresses, names that DNS cannot hold, and validation errors.
  `TestDNSCheckOfANullMXAndOfAnSRVRecordOfNoService`
  in `internal/checksandalerts/protocol_checks_test.go` covers the null MX,
  an `MX` answer that holds the null MX beside another record, and an
  `SRV` record whose target is `.`: each fails without
  `expected_record_values` and is healthy with the value `.`.
  `TestDNSCheck` and `TestDNSCheckOfANullMXAndOfAnSRVRecordOfNoService`
  run on the machine that runs the suites
  and on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution).
- **On a node:** the services scenarios (`tests/integration/service_scenarios.py`)
  run dnsmasq, a DNS server program, with records supplied by the tests, on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution).
  The checks ask for `A`, `MX`, `TXT`, `CNAME`, `PTR`, and `SRV` records.
  The checks accept expected values in any order and any way of writing
  the values. The checks fail for another value, for
  one value of two, and for a name without a record. The checks also fail
  for a name that the server refuses, a port where no server listens, and
  a server that is stopped.
- **Not verified on a node:** an answer that comes over TCP. `TestDNSCheck`
  covers an answer over TCP.

The [validation record](../../docs/changing-mmwatchdog/validation.md) has the latest results.

## Why it works this way

- **TXT values compare exactly, including case and a final dot.** TXT is
  arbitrary text. Case and a final dot are ignored only in host names,
  including the host part of an SRV value.

- **The check asks the server that it names, and no other.** A check of a
  DNS server must not get its answer from the machine that asks: the
  node's hosts file or its resolver could answer for a server that is down
  ([protocols and common server software](../../docs/changing-mmwatchdog/design/checks.md#protocols-and-common-server-software)).
- **The name is taken as complete.** A search domain of the node would
  make the check ask for another name than the one it shows.
- **With `expected_record_values`, the answer must hold exactly those
  values, in any order.** A record that should be gone is as wrong as one
  that is missing, and DNS servers give records in any order.
- **`expected_record_values` is optional.** A name whose addresses change,
  such as a name behind a content delivery network, can only be checked
  for an answer.
- **Without `expected_record_values`, the null MX fails the check.** A
  check of an `MX` record without expected values asks where the domain's
  mail goes, and the null MX says nowhere. A domain that should take mail
  would otherwise lose it while its check stays healthy. A domain that
  must take no mail states that with the expected value `.`. An `SRV`
  record whose target is `.` says that the service is unavailable. Without
  `expected_record_values`, an `SRV` answer containing `.` fails the check.
  A check of a service that must not be offered expects `.`.
- **The root name is written `.`.** An empty value in a message reads as
  if the server had said nothing, and the dot is how DNS itself writes the
  root name.
- **`dns_server` has no default port.** A reader sees on each check where
  MMWatchdog connects.
- **`dns` is a core module.** Checking a DNS server is common work, so the
  check is built into `mmwatchdog` and tested with it.
