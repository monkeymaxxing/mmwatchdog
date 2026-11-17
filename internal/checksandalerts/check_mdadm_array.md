# `mdadm_array`

`mdadm_array` watches one software RAID array of Linux, the kind that the
`mdadm` program manages. The value of the check is the path of the
array, such as `/dev/md0`. The check is healthy while the array has all its
devices. The check fails when a device is missing or when the node has not
put the array together at all.

MMWatchdog reads the state of the array from the kernel's own files below
`/sys/block`, and needs no `mdadm` program for it. For a ZFS pool, use
[`zfs_pool`](../../official_extra_modules/zfs_pool/README.md).

MMWatchdog provides no check for hardware storage controllers.
A [`shell`](../../docs/reference/modules/shell.md) check or a module of the team's own
can ask the vendor's tool.

## When the check fails, and when it cannot check

The check states an `alert_severity` and fails at that severity:

- **when the array misses a device:** `/dev/md0 (raid1) is degraded: 1 of 2
  devices work; the kernel reports as faulty: sdb1`. After a faulty device
  was removed, the message ends after `1 of 2 devices work`.
- **while a new device is rebuilt.** The array misses a device until the
  rebuild is done, and the message says how far it is: `/dev/md0 (raid1) is
  degraded: 1 of 2 devices work; the array is being rebuilt, 50 percent
  done`.
- **when the node has not put the array together:** `/dev/md0  is not an assembled software RAID array on the node`. The message about
  an array the node has not put together also appears for a stopped array
  and for a path that names no array.
- **when the kernel reports that the array does not work,** such as an
  array that is put together but not started: `/dev/md0 does not work: the
  kernel reports the array as inactive`.

A check of the array that the kernel runs from time to time is no failure.
The healthy check's message reports the kernel's array check: `/dev/md0
(raid1) has all expected devices: 2 of 2 devices work; the kernel runs a check of the array,
25 percent done`.

A device path that cannot resolve fails and names the full configured path.
MMWatchdog never guesses an array from the path's last component. A missing array
fails; an array whose state or `degraded` count cannot be read is unknown.
An empty, invalid, or negative `degraded` count is unknown, never zero.
The check's incident follows the check group's
[failing time](../../docs/how-to/decide-when-a-check-alerts.md).

## Examples

```yaml
# /etc/mmwatchdog/check_groups/storage.yml
check_groups:
  storage:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 1m
    checks:
      data_array_has_all_its_devices:
        mdadm_array: /dev/md0
        alert_severity: sos
```

Every minute, MMWatchdog on the node reads the state of
`/dev/md0`. While the RAID 1 array's two devices work, the check is healthy with
`/dev/md0 (raid1) has all expected devices: 2 of 2 devices work`.

When the kernel marks a device as faulty, the check fails.
Because `alert_after_failing_for` is `0s`, MMWatchdog sends the `sos` alert at
once. The check stays failed while the replacement device is rebuilt. Once the array has been whole for 1
minute, MMWatchdog sends the recovery alert.

## How it works

- **The kernel exposes the array's state** in the directory
  `/sys/block/NAME/md`. A path that is a link to an array, such as
  `/dev/md/data`, names the array that the link leads to, such as `md0`.
- **A working array** is in one of the states `clean`, `active`,
  `active-idle`, `write-pending`, `readonly`, or `read-auto`, as the file
  `array_state` says. Every other state, such as `inactive`, fails the
  check.
- **A device works** when the kernel reports it as `in_sync`. The message
  counts devices marked `in_sync` against the number of devices that the
  array should have (`raid_disks`), and names each device that the kernel
  reports as `faulty`. The array misses a device while the kernel's count
  of missing devices (`degraded`) is above 0. A `raid0` or `linear` array
  has no spare copy of its data, so the kernel keeps no `degraded` count for
  it: such an array that loses a device has the state `broken`, which fails
  the check.
- **The progress** of a rebuild or of a kernel check comes from
  `sync_completed`. The kernel reports the sectors processed and the total
  sectors to process for the current rebuild or kernel check.

## Errors and what they mean

- **`alert_severity: required: info, warn, or sos`.** A validation error:
  the check states no `alert_severity`.
- **`must be the path of a software RAID array, such as /dev/md0`.** A
  validation error: the path does not start with `/dev/`, or is not written
  in plain form.
  The validation error starts with the configuration file's path, the line
  number in the configuration file, and the setting's YAML key path, such
  as `check_groups.storage.checks.data_array_has_all_its_devices.mdadm_array:`.
- **`is not an assembled software RAID array on the node`** right
  after a start of the machine means that the array was not assembled. Look
  at `cat /proc/mdstat` and the configuration of `mdadm` on the node. On a
  node that never had the array, the path in the check is wrong.
- **`does not work: the kernel reports the array as ...`.** The array exists but
is not in use. `mdadm --detail /dev/md0` says more.

## Tested on

- **Go tests** in `internal/checksandalerts/server_checks_test.go`: `TestMdadmArrayCheck`
  covers an array that is whole, one that the kernel checks, one that is
  degraded with its faulty device, one that is being rebuilt, one that is
  inactive, one that the node has not put together, and the validation of
  the path.
- **In virtual machines (VMs)**, the suite `node_scenarios.py` installs
  `mdadm` from the distribution's packages and builds a real RAID 1 array
  on two loop devices. The check is healthy while the array is whole. When
  a device fails, the check fails and names the faulty device in the result
  message.
  The check stays failed while the device is missing, recovers after the
  rebuild, and fails when the array is stopped. A check of an array never
  present in the test VM fails with a message about the missing array.
- The suite and the Go tests passed on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)
  in the latest run of the whole test catalog
  ([validation record](../../docs/changing-mmwatchdog/validation.md)). The guests are VMs,
  because the suite loads the kernel's module for RAID.

## Why it works this way

- **A missing array differs from unreadable array data.** Absence proves a
  failure; unreadable data cannot prove that an array is whole.
- **The configured device path must resolve.** An unrelated array with
  the configured device path's last component must not make the check healthy.

- **Each check names its array, and MMWatchdog has no check of "every array that is
  there".** An array that is missing after a start of the machine is the
  worst case. A check of only present arrays would report an overall
  healthy result because the missing array would never be checked.
- **The check stays failed during a rebuild.** Until the rebuild is done,
  the array still lacks the redundancy of the missing device.
- **MMWatchdog reads the kernel's files and runs no `mdadm`.** The check needs
  no program installed on the node beyond MMWatchdog. The kernel files read by the
  check do not differ between distributions.
- **Software RAID is a core module.** Arrays of `mdadm` are common on Linux
  servers. Checks for common server needs are built into the `mmwatchdog`
  program as core modules and tested with MMWatchdog
  ([check group decisions](../../docs/changing-mmwatchdog/design/checks.md#check-group-decisions)).
