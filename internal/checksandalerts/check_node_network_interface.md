# `node_network_interface`

`node_network_interface` measures one network interface of the node: the
speed of its link, and the data that it receives and sends. The value of the
check is the name of the interface, such as `eth0`, as `ip link` lists it.
MMWatchdog measures the interface by reading the kernel's files in
`/sys/class/net`, without running another program.

A `node_network_interface` check that states an `alert_severity` also fails
while its interface has no link, with or without a limit. So a check with an
`alert_severity` and no limit says "the cable of this interface must be in,
and the link up".

To check that another machine answers over the network, use
[`ping`](check_ping.md).

## When the check fails, and when it cannot check

A check with an `alert_severity` fails at that severity:

- **when a reading passes a limit.** A reading strictly above its
  `fail_when_measurement_above` number, or strictly below its
  `fail_when_measurement_below` number, fails the check; an equal reading
  passes ([checks that measure a number](../../docs/reference/configuration-files.md#checks-that-measure)).
- **while the interface has no link,** with a message that starts with
  `network interface eth0 has no link`. An interface without a link reports
  no speed, so a limit on `link_speed_mbit_per_second` does not make such a
  check unknown: it fails for the missing link.

A check with `only_record_readings: true` only records its readings
([checks that only record](../../docs/how-to/look-back-at-measurements.md)).
The check that only records notes a missing link in the check's message,
such as `network interface eth1 has no link; received_mbit_per_second is 0;
sent_mbit_per_second is 0 over 30.000s`, and never fails or alerts.

MMWatchdog cannot check, and the observation is unknown with the reason,
in the following cases. An unknown result of a check that can alert opens
a separate cannot-check incident after `alert_after_failing_for`
([scheduling and observed state](../../docs/changing-mmwatchdog/design/checks.md#scheduling-and-observed-state)).

- **The interface does not exist:** `network interface eth9 does not
  exist`.
- **The check has a limit on `link_speed_mbit_per_second`, and the
  interface has a link but reports no speed,** as the interfaces of many
  virtual machines do: `network interface eth0 reports no link speed, so
  link_speed_mbit_per_second cannot be measured`. On a machine that reports
  no link speed, leave out the limit on `link_speed_mbit_per_second`.
- **The counters of the interface cannot be read,** such as `network
  interface eth0: the count of received bytes is unavailable:` followed by
  the reason.

## Examples

```yaml
# /etc/mmwatchdog/check_groups/network.yml
check_groups:
  network:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 2m
    recovered_after_healthy_for: 1m
    checks:
      eth0_link_is_up_at_1000_mbit:
        node_network_interface: eth0
        fail_when_measurement_below:
          link_speed_mbit_per_second: 1000
        alert_severity: sos
      eth1_traffic_is_recorded:
        only_record_readings: true
        node_network_interface: eth1               # records only
```

Every 30 seconds, MMWatchdog on the node reads `eth0` and `eth1`.
`eth0_link_is_up_at_1000_mbit` fails in each of the following cases:

- When the cable of `eth0` is out, the message is
  `network interface eth0 has no link`.
- When the link comes up at only 100 megabits per second, the message is
  `link_speed_mbit_per_second is 100 (fails below 1000) over 30.000s`.

After `eth0_link_is_up_at_1000_mbit` has been failing for 2 minutes,
MMWatchdog sends an `sos` alert. After `eth0_link_is_up_at_1000_mbit` has
been healthy for 1 minute, MMWatchdog sends the recovery alert.
`eth1_traffic_is_recorded` stores the speed and the traffic of `eth1` in the
node's [measurement history](../../docs/how-to/look-back-at-measurements.md), and
never alerts.

## How it works

| Measurement | Meaning |
| --- | --- |
| `link_speed_mbit_per_second` | The speed agreed between the interface and the device at the cable's other end, in megabits per second. The speed comes from the file `speed`. If the interface reports no speed, the speed measurement is unavailable |
| `received_mbit_per_second` | Data received through the interface, in megabits per second, from the counter `statistics/rx_bytes` |
| `sent_mbit_per_second` | Data sent through the interface, in megabits per second, from the counter `statistics/tx_bytes` |

- **The link** is up while the kernel writes `1` into the file `carrier` of
  the interface. An interface
  that is turned off has no answer in `carrier`, and counts as without a
  link.
- **The two traffic measurements cover the time since the check's run
  before.** The first run takes a fresh sample of 200 ms.
  A run after a gap of more than `2 × check_every + time_limit_per_run`
  also takes a fresh sample. A run after a counter decreases below the
  previous reading also takes a fresh sample. So the check group's
  `time_limit_per_run` must be longer than `200ms`.

  The message ends with the time that the reading covers, such as
  `over 30.000s`. The [CPU sampling](../../docs/reference/configuration-files.md#readings-over-a-time)
  section of the configuration files reference gives the rules that every module that
  measures a rate shares.
- **The message** names each measurement that has a limit, with its reading
  and its limit; a check without a limit names every reading.

## Errors and what they mean

Each validation error starts with the file, the line, and the path of the
setting, such as `check_groups.network.checks.eth0_link_is_up_at_1000_mbit.node_network_interface:`.

- **`must be the name of a network interface, such as eth0`.** The name is
  longer than 15 characters, is `.` or `..`, or holds a character other
  than letters, digits, `_`, `.`, `:`, `@`, and `-`, such as a slash or a
  space.
- **`this check has no such measurement; it measures link_speed_mbit_per_second,
  received_mbit_per_second, sent_mbit_per_second`.** A limit names another
  measurement.
- **`expected a number of 0 or more`.** A limit is negative.
- **`time_limit_per_run: must exceed 200ms for the initial sample of the
  network interface`.**

The messages of an unknown run are listed under
[when MMWatchdog cannot check](#when-the-check-fails-and-when-it-cannot-check).
`the kernel's counters changed while sampling; awaiting a fresh sample`
means that a counter's second reading is smaller than the first reading
during the fresh sample, for example because the interface was removed and
added again. The next run takes a new sample.

## Tested on

- **Go tests** in `internal/checksandalerts/node_measurements_test.go`:
  `TestNodeNetworkInterfaceCheck` verifies the following outcomes:

  - The check measures the speed and the traffic.
  - Missing speed makes the observation unknown only when the check has
    a limit on the speed.
  - A missing link fails a check with an `alert_severity`, with or without
    a limit.
  - A check that only records notes the missing link without failing.
  - The test covers validation.

  `TestCounterReadingsOfACheck` covers the rate from two readings.
- **On real guests**, the suite `node_scenarios.py` measures the interface
  that carries the guest's default route, and compares its speed with the
  kernel's, or finds no speed where the kernel reports none.
  `node_scenarios.py` turns a dummy interface off and on. The check that
  states an `alert_severity` fails with `has no link` and recovers with the
  link. The check that only records notes the missing link and opens no
  incident. An interface that does not exist is unknown with its message.
- The `node_scenarios.py` suite and the Go tests passed on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)
  in the latest run of the whole catalog
  ([validation record](../../docs/changing-mmwatchdog/validation.md)).

## Why it works this way

- **Recording readings is an explicit choice.** State
  `only_record_readings: true` to record readings without alerts or
  recovery actions.
  A missing severity must not silently turn an intended alert into readings only.

- **A check with an `alert_severity` fails without a link, without a
  limit.** A cable that is out is a failure regardless of the speed or
  traffic readings. Busy traffic is a failure only above the limit that the operator sets
  ([check group decisions](../../docs/changing-mmwatchdog/design/checks.md#check-group-decisions)).
- **A missing speed is unknown, never zero.** Many virtual machines report
  no speed at all. A zero would fail every limit on the speed.
- **No link fails a check with a limit on the speed, and does not make it
  unknown.** The missing link is the reason that the speed is missing, and
  it is the failure that the check is there for.
