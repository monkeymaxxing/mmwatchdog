# `node_temperature`

`node_temperature` measures the temperature of one kind of hardware of the
machine, such as its processors or its NVMe drives. The value of the check
is the name of the kind of sensor, as the kernel names it, such as
`coretemp`. MMWatchdog reads the kernel's files below `/sys/class/hwmon` and
needs no `sensors` program to measure temperatures.

A machine has several sensors of one name: one for each core, or one
for each drive. The check measures the highest reading among the sensors
of the configured name as `highest_temperature_celsius`. Different hardware
has different limits, so each kind of sensor gets its own check.

## When the check fails, and when it cannot check

- **The check fails** at the check's `alert_severity` when
  `highest_temperature_celsius` passes a limit set for the check.
  A reading strictly above the check's `fail_when_measurement_above`
  number, or strictly below the check's `fail_when_measurement_below`
  number, fails the check. An equal reading passes
  ([checks that measure a number](../../docs/reference/configuration-files.md#checks-that-measure)).
- **A check with `only_record_readings: true` only records** its readings, never fails, and
  takes no `alert_severity`
  ([checks that only record](../../docs/how-to/look-back-at-measurements.md)).
- **MMWatchdog cannot check** when the machine has no sensor of the name
  configured in `node_temperature`, which is common on a virtual machine.
  The observation is then unknown, and the message lists the names of the
  sensors that the machine has: `this
  machine has no temperature sensor named coretemp; the kernel knows these
  kinds of sensor: acpitz, nvme`. A machine without any sensor gives `this
  machine has no temperature sensor named coretemp; it has no temperature
  sensor that the kernel knows`. An unknown result of a check that can
  alert opens a separate cannot-check incident after `alert_after_failing_for`
  ([scheduling and observed state](../../docs/changing-mmwatchdog/design/checks.md#scheduling-and-observed-state)).

An unreadable sensor of the requested kind makes the run unknown unless
a readable sensor already fails a limit. The result message names the
unreadable sensor's file and the reason. If a readable sensor fails a
limit, the run stays failed, and the result message also names unreadable
sensors.

Negative temperature limits are valid, for hardware that can operate below zero.

## Examples

```yaml
# /etc/mmwatchdog/check_groups/temperature.yml
check_groups:
  temperature:
    check_every: 1m
    time_limit_per_run: 5s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 5m
    checks:
      processors_are_below_85_degrees:
        node_temperature: coretemp
        fail_when_measurement_above:
          highest_temperature_celsius: 85
        alert_severity: warn
      drives_are_below_60_degrees:
        node_temperature: nvme
        fail_when_measurement_above:
          highest_temperature_celsius: 60
        alert_severity: warn
```

Every minute, MMWatchdog on the node reads every sensor named `coretemp`, which are
those of the Intel processors, and every sensor named `nvme`. The first
check fails when the hottest processor sensor is above 85 degrees Celsius,
the second when the hottest NVMe drive is above 60. Each check that has
failed for 5 minutes sends a `warn` alert. Each check recovers after
5 minutes below the check's limit. The check's result message names
the hottest sensor, such as
`highest_temperature_celsius is 61 (fails above 85); the hottest of 9 sensors of
coretemp is Package id 0`.

## How it works

| Name of the sensor | Hardware |
| --- | --- |
| `coretemp` | The processors of Intel |
| `k10temp` | The processors of AMD |
| `nvme` | NVMe drives |
| `drivetemp` | SATA drives |
| `acpitz` | The mainboard |

- **The names** are those that the `sensors` program prints in front of the
  first hyphen, such as `coretemp` in `coretemp-isa-0000`. The kernel shows
  the sensor names in the files `/sys/class/hwmon/*/name`. Several devices
  can have the same name, such as one for each NVMe drive, and the check
  reads all of them.
- **Each reading** comes from a file such as `temp1_input`, which holds
  thousandths of a degree Celsius. A sensor is named in the message by its
  label, such as `Package id 0` from `temp1_label`. For a sensor without a
  label, the message uses the sensor's file, such as `temp1`. An unreadable reading makes the run unknown
  and names its file, unless a readable sensor already fails a limit.
  When a readable sensor fails a limit and another sensor cannot be read,
  the run stays failed and also names the unreadable file.
- **A reading below zero is valid,** such as that of a sensor in a cold
  room. A limit may also be below zero.

## Errors and what they mean

Each validation error starts with the file, the line, and the path of the
setting, such as `check_groups.temperature.checks.processors_are_below_85_degrees.node_temperature:`.

- **`must be the name of a kind of temperature sensor, as the sensors
  program prints it in front of the first hyphen, such as coretemp,
  k10temp, or nvme`.** The value holds a character other than letters,
  digits, `_`, `.`, and `-`, such as a space, or is longer than 64
  characters.
- **`this check has no such measurement; it measures highest_temperature_celsius`.**
- **`a node_temperature check needs a limit to alert`.** Add a measurement limit,
  or replace the severity with `only_record_readings: true`.

- **An unknown check with `this machine has no temperature sensor named
  ...`.** The message lists the names that the machine has; use one of
  them, or remove the check from a machine without sensors. A value such
  as `coretemp-isa-0000` passes validation but names no sensor: it is the
  whole name of one device, as `sensors` prints it; write `coretemp`.

## Tested on

- **Go tests** in `internal/checksandalerts/server_checks_test.go`:
  `TestNodeTemperatureCheck` covers the hottest of several sensors, several
  devices of one name, a sensor without a label, a reading below zero, no
  sensor of that name, and no sensor at all.
- **On virtual machines running MMWatchdog**, the suite `node_scenarios.py`
  checks `coretemp` on a guest without a `coretemp` sensor. The check is
  unknown with
  `this machine has no temperature sensor named coretemp;`, followed by
  what the guest has. The guests are virtual machines, so
  no test reads a real temperature sensor.
- The suite and the Go tests passed on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)
  in the latest run of the whole catalog
  ([validation record](../../docs/changing-mmwatchdog/validation.md)).

## Why it works this way

- **Unreadable sensors do not prove that hardware is cool.** A known failed
  limit still proves failure when another sensor cannot be read.
- **Temperatures and their limits may be negative.** Cold hardware can be
  below zero Celsius. Allowing negative temperatures does not change the
  allowed limits for non-temperature measurements.

- **Recording readings is an explicit choice.** State
  `only_record_readings: true` to record readings without alerts or recovery.
  A missing severity must not silently turn an intended alert into readings only.

- **One kind of sensor for each check.** Different hardware has different
  limits: a processor runs hotter than a drive.
- **The highest reading counts.** A single hot core or drive is the one
  that is in trouble; an average would hide it.
- **MMWatchdog reads the kernel's files.** The node needs no `sensors` program, and
  the names are those that `sensors` prints, so a reader who knows that
  program knows the names.
- **A machine without the sensor is unknown, not healthy.** A check that
  cannot read a temperature must not say that the hardware is cool.
