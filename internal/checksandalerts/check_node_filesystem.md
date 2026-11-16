# `node_filesystem`

`node_filesystem` measures one filesystem of the node: how much of its space
and of its inodes is in use, and how much space is free. The value of the
check is the absolute path where the filesystem is mounted, such as `/` or
`/srv/data`. MMWatchdog reads the filesystem counts through the kernel's
`statfs` call and runs no program to read the counts.

An inode is the filesystem's record of one file. A filesystem with many
small files can run out of inodes while space is left.

A `node_filesystem` check that states an `alert_severity` also fails while
nothing is mounted at its path, and while its filesystem is mounted
read-only, with or without a limit. So one check of a filesystem covers its
space, its inodes, and that it can be written.

To apply matching limits to every filesystem of the node, use
[`every_disk_and_network_filesystem`](check_every_disk_and_network_filesystem.md).
You do not need to name each filesystem to apply the limits.
To measure the disk itself, how busy it is and how much is read and
written, use [`node_disk_device`](check_node_disk_device.md).

## When the check fails, and when it cannot check

A check with an `alert_severity` fails at that severity:

- **when a reading passes a limit.** A reading strictly above its
  `fail_when_measurement_above` number, or strictly below its
  `fail_when_measurement_below` number, fails the check; an equal reading
  passes ([checks that measure a number](../../docs/reference/configuration-files.md#checks-that-measure)).
- **when nothing is mounted at the check's path,** with the message `no
  filesystem is mounted at /mnt/san`. The check reports no reading then.
- **when the filesystem at the check's path is mounted read-only.** The
  message starts with `/srv/data is mounted read-only;` and goes on with the
  readings, which MMWatchdog still records.

A check with `only_record_readings: true` only records its readings
([checks that only record](../../docs/how-to/look-back-at-measurements.md)).
The check with `only_record_readings: true` notes a read-only mount in the
check's message and never fails or alerts. A message can be `/srv/data
is mounted read-only; space_used_percent is 40; inodes_used_percent is 3;
space_free_gib is 12`. While nothing is mounted at the path of a check with
`only_record_readings: true`, the check is unknown because no readings are
available. The check with `only_record_readings: true` reports `no
filesystem is mounted at /srv/data` and opens no incident.

MMWatchdog cannot check, and the observation is unknown, when a condition in
the following list occurs. An unknown result of a check that can alert opens
a separate cannot-check incident after `alert_after_failing_for`
([scheduling and observed state](../../docs/changing-mmwatchdog/design/checks.md#scheduling-and-observed-state)).

- **The filesystem does not answer.** A filesystem on another machine can
  stop answering, for example when that machine is gone. See
  [a filesystem that does not answer](#a-filesystem-that-does-not-answer).
- **The kernel answers with an error,** such as a stale NFS file handle. The
  message is the kernel's error.
- **The check has a limit on `inodes_used_percent`, and the filesystem has
  no fixed inode count,** such as btrfs. The message is `/srv has no fixed
  inode count, so inodes_used_percent cannot be measured`. Leave out the
  `inodes_used_percent` limit on a filesystem without a fixed inode count.
  The filesystem's limits on space still work.
- **MMWatchdog cannot read the kernel's list of mounted filesystems,** with a
  message that starts with `MMWatchdog cannot read the kernel's list of mounted
  filesystems:`.

## Examples

### Limits on space, on inodes, and on free space

```yaml
# /etc/mmwatchdog/check_groups/root_filesystem.yml
check_groups:
  root_filesystem:
    check_every: 1m
    time_limit_per_run: 5s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    checks:
      root_filesystem_is_below_90_percent:
        node_filesystem: /
        fail_when_measurement_above:
          space_used_percent: 90
          inodes_used_percent: 90
        alert_severity: warn
      root_filesystem_has_10_gib_free:
        node_filesystem: /
        fail_when_measurement_below:
          space_free_gib: 10
        alert_severity: sos
```

Every minute, MMWatchdog on the node measures the filesystem at `/`.
`root_filesystem_is_below_90_percent` fails when more than 90 percent of the
space, or more than 90 percent of the inodes, is in use. The message of
`root_filesystem_is_below_90_percent` names the space and inode readings,
such as `space_used_percent is 62.5 (fails above 90);
inodes_used_percent is 12 (fails above 90)`. After 10 minutes of failing,
MMWatchdog sends a `warn` alert. `root_filesystem_has_10_gib_free` fails when less
than 10 GiB are free, and alerts at `sos`. A filesystem mounted read-only
fails `root_filesystem_is_below_90_percent` and
`root_filesystem_has_10_gib_free`. A [recovery command](../../docs/how-to/restart-a-service-automatically.md#steps-to-run-a-command-instead)
triggered by `root_filesystem_is_below_90_percent` or
`root_filesystem_has_10_gib_free` can clean up old files.

### A filesystem that must be mounted

A check with an `alert_severity` and no limit says "this filesystem must be
mounted, and writable":

```yaml
# /etc/mmwatchdog/check_groups/san_storage.yml
check_groups:
  san_storage:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 2m
    checks:
      san_storage_is_mounted:
        node_filesystem: /mnt/san
        alert_severity: sos
```

While a filesystem is mounted at `/mnt/san` and can be written, the check is
healthy and records its readings. When the filesystem is unmounted, the
check fails with `no filesystem is mounted at /mnt/san`, and after 5 minutes
MMWatchdog sends an `sos` alert. A [recovery command](../../docs/how-to/restart-a-service-automatically.md#steps-to-run-a-command-instead)
triggered by `san_storage_is_mounted` can mount the filesystem again. Once
the filesystem at `/mnt/san` has been mounted for 2 minutes,
MMWatchdog sends the recovery alert.

### A filesystem that must be writable

A filesystem that the kernel remounted read-only after I/O errors leaves
services running while every write to the affected filesystem fails:

```yaml
# /etc/mmwatchdog/check_groups/data_filesystem.yml
check_groups:
  data_filesystem:
    check_every: 1m
    time_limit_per_run: 5s
    alert_after_failing_for: 2m
    recovered_after_healthy_for: 1m
    node_filesystem: /srv/data
    fail_when_measurement_above:
      space_used_percent: 85
    alert_severity: sos
```

The `data_filesystem` check group holds one direct check,
`data_filesystem/node_filesystem`.
`data_filesystem/node_filesystem` fails when the filesystem at `/srv/data`
is more than 85 percent full, and also when the filesystem is mounted
read-only, even below 85 percent.

After 2 minutes MMWatchdog sends an `sos` alert whose message starts with
`/srv/data is mounted read-only;`. Write filesystem checks with an
`alert_severity` for the filesystems that are meant to be writable.

## How it works

Each run does three steps on the node:

1. MMWatchdog reads the kernel's list of mounted filesystems, `/proc/self/mountinfo`,
   and looks for a filesystem that is mounted exactly at the check's path.
   The path is the one where the filesystem is mounted, as `findmnt` lists
   it, not a directory inside the filesystem.
2. MMWatchdog asks the kernel for the counts of the filesystem mounted at
   the check's path, as `df` does.
3. MMWatchdog computes the readings, applies the limits, and notes a read-only
   mount.

| Measurement | Meaning |
| --- | --- |
| `space_used_percent` | The share of the filesystem's space that ordinary accounts cannot fill any more, 0 to 100. Space that the filesystem keeps for root counts as used |
| `inodes_used_percent` | The share of the filesystem's inodes that is in use, 0 to 100. MMWatchdog provides no inode-use percentage for a filesystem without a fixed inode count, such as btrfs |
| `space_free_gib` | The space that an ordinary account can still fill, in GiB |

MMWatchdog stores the readings from each scheduled run in the node's
[measurement history](../../docs/how-to/look-back-at-measurements.md), whether the
check has a limit or only records.

### A filesystem that does not answer

For a filesystem on another machine, the kernel holds the node's
`statfs` call without end while the remote machine is gone.
MMWatchdog cannot cancel the blocked system call. So:

- MMWatchdog waits for the kernel's answer no longer than `time_limit_per_run`. The
  check is then unknown with `the filesystem does not answer: the
  filesystem at /mnt/san gave no answer within the time limit of the run`.
- MMWatchdog starts no second `statfs` call for the check's path while the
  first `statfs` call remains blocked. Later runs wait for the pending
  call's answer and say when MMWatchdog started waiting:
  `the filesystem does not answer: MMWatchdog has waited for the filesystem at
  /mnt/san since 14:02:00 UTC`.
- Concurrent checks of the filesystem at the same path share one pending
  `statfs` call and receive the result of the shared call.
- Once the kernel returns a result, the next run starts a new `statfs` call.

## Errors and what they mean

Each validation error starts with the file, the line, and the path of the
setting, such as `check_groups.root_filesystem.checks.root_filesystem_is_below_90_percent.node_filesystem:`.

- **`must be the absolute path where the filesystem is mounted, such as
  /`.** The path is relative.
- **`this check has no such measurement; it measures space_used_percent,
  inodes_used_percent, space_free_gib`.** A limit names another
  measurement.
- **`expected a number from 0 to 100`** for a limit on a `_percent`
  measurement, and **`expected a number of 0 or more`** for a limit on
  `space_free_gib`.
- **A validation warning from `mmwatchdog validate`** when the filesystem of a
  check with a limit is missing from the `filesystems_to_ignore` of an
  `every_disk_and_network_filesystem` check: one full filesystem would send two
  alerts. The [`every_disk_and_network_filesystem`](check_every_disk_and_network_filesystem.md#errors-and-what-they-mean)
  page shows the warning.

The messages of an unknown run are listed under
[when MMWatchdog cannot check](#when-the-check-fails-and-when-it-cannot-check).
`/srv/data: filesystem counts unavailable or invalid` means that the kernel
gave counts that say nothing, such as a size of zero.
`/srv/data: inode counts invalid` makes a check with a limit on
`inodes_used_percent` unknown when the kernel reports more free inodes than
inodes.

## Tested on

- **Go tests** in `internal/checksandalerts`: `resources_test.go`
  (`TestSwapLoadAndFilesystemReadings`, `TestMeasurementLimitsAreOptionalAndStrict`)
  covers the readings, a read-only filesystem with and without an
  `alert_severity`, and a filesystem without a fixed inode count;
  `node_measurements_test.go` covers the kernel's list of mounts, a check of
  a path with nothing mounted, a filesystem that does not answer, and two
  questions about one filesystem at once. `resources_linux_test.go` in
  `internal/daemon` reads the filesystem at `/` on Linux.
- **On Linux virtual machines used for integration tests**, the suite
  `resources.py` records the space,
  the inodes, and the free space of `/`. The suite remounts a small tmpfs
  read-only. Checks of the tmpfs that state an `alert_severity` fail, with
  or without a limit, and keep the readings. The checks with an
  `alert_severity` recover once the tmpfs is writable. The check that only
  records notes the read-only mount and opens no incident.

  `node_scenarios.py` unmounts
  a tmpfs. The check that states an `alert_severity` fails and recovers when
  the tmpfs is mounted again. The check that only records is unknown and
  records no reading of the filesystem containing the directory exposed
  after the tmpfs is unmounted.
- The `resources.py` and `node_scenarios.py` suites and the Go tests
  passed on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)
  in the latest run of the whole catalog
  ([validation record](../../docs/changing-mmwatchdog/validation.md)). A filesystem that
  answers with an error, such as a stale NFS file handle, is covered by Go
  tests only.

## Why it works this way

- **Recording readings is an explicit choice.** State
  `only_record_readings: true` to record readings without alert
  notifications or recovery actions.
  A missing severity must not silently turn an intended alert into readings only.

- **A check with an `alert_severity` fails when nothing is mounted.**
  Without checking for a filesystem mounted exactly at the check's path,
  MMWatchdog would read the filesystem that holds the empty directory,
  usually `/`. The check would then report the containing filesystem's
  numbers. A check of a missing filesystem never reports
  another filesystem's numbers.
- **A check with an `alert_severity` fails on a read-only mount, without a
  limit.** A filesystem that is gone or cannot be written is a failure at
  any value. A full filesystem is a failure only above the limit
  that the operator sets
  ([check group decisions](../../docs/changing-mmwatchdog/design/checks.md#check-group-decisions)).
- **No new module for "is mounted".** One check of a filesystem covers its
  space, its inodes, and that it is mounted and writable, at the severity
  that the check states.
- **A check that only records never alerts,** also for a read-only mount.
  The check that only records notes the read-only mount in the check's
  message. The read-only mount remains visible to the reader of
  `mmwatchdog status`.
- **MMWatchdog keeps at most one question waiting for each path.** A check that
  runs every minute against a filesystem that does not answer would
  otherwise leave one more question waiting at each run.
