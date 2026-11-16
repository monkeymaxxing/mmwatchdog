# `every_disk_and_network_filesystem`

`every_disk_and_network_filesystem` applies the limits of its check to every
filesystem of the node that is mounted from a disk device or from another
machine, without listing each filesystem. A filesystem that nobody listed is the filesystem
that fills up. MMWatchdog reads the kernel's list of mounts and
asks the kernel for the counts of each filesystem, as `df` does. MMWatchdog
runs no external program to list mounts or measure filesystem usage.

Each filesystem that fails is a separate finding: one of several things
that a run reports as failed. Each finding has a separate incident, alert,
and recovery
([findings](../../docs/changing-mmwatchdog/design/checks.md#findings)).

The limits and the read-only rule are those of
[`node_filesystem`](check_node_filesystem.md), which measures one filesystem
that it names. Use `node_filesystem` for a filesystem whose readings should
be recorded, for a filesystem that must be mounted, and for a filesystem of
a kind that `every_disk_and_network_filesystem` leaves out.

## When the check fails, and when it cannot check

The check always states an `alert_severity`. The
`every_disk_and_network_filesystem` check fails while at least one
filesystem is a finding. A filesystem is a finding:

- **when a reading of the filesystem passes a limit.** The finding's message is,
  for example, `/var: space_used_percent is 93 (fails above 90)`.
- **when the filesystem is mounted read-only,** with or without a limit,
  just as a read-only filesystem fails a `node_filesystem` check. The
  message starts with `/srv/data is mounted
  read-only;`.
- **when it does not answer,** such as a filesystem on another machine that
  is gone: `the filesystem does not answer: the filesystem at /mnt/nfs gave
  no answer within the time limit of the run`. MMWatchdog asks about all
  filesystems at once, so one that does not answer takes no time from the
  others.
- **when it answers with an error in place of its size,** as a network
  filesystem does when its server restarted or went away: `the filesystem
  at /mnt/nfs gives an error in place of its size: stale NFS file handle`.

A permission error or an unknown observation of a filesystem also becomes
a finding, with the path and reason.

Each finding identifies the path where the failing filesystem is mounted
and follows the check's timings independently. The finding's incident opens
after `alert_after_failing_for`. The finding recovers when runs stop
reporting the finding for `recovered_after_healthy_for`, with the message
`the filesystem at /var no longer fails this check`. A second filesystem
that fails while the first filesystem's incident is open gets a separate
alert.

A filesystem that failed in the run before and is no longer mounted is
not recovered: it stays a finding, with the message `the filesystem at
/mnt/backup is no longer mounted`, until it is mounted and healthy again or
is named in `filesystems_to_ignore`. A healthy filesystem that is unmounted
just leaves the check.

MMWatchdog cannot check when it cannot read the kernel's list of mounts. The
observation is then unknown, with a message that starts with `MMWatchdog cannot
read the kernel's list of mounted filesystems:`, and after
`alert_after_failing_for` the check opens one cannot-check incident
([scheduling and observed state](../../docs/changing-mmwatchdog/design/checks.md#scheduling-and-observed-state)).
An open incident of a finding stays open while MMWatchdog cannot check.

A mount path longer than 255 bytes is checked in the same way as shorter
paths. The long path's finding uses `filesystem_path_sha256:` followed by
the SHA-256 checksum of the path as a stable identity. Shorter paths use
the mount path as the finding identity.

Every finding message, including recovery, names the mount path. MMWatchdog
saves the recovery wording with the finding's incident, so a daemon restart
does not replace the mount path with a checksum.

## Examples

```yaml
# /etc/mmwatchdog/check_groups/filesystems.yml
check_groups:
  filesystems:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    checks:
      every_filesystem_is_below_90_percent:
        every_disk_and_network_filesystem:
          filesystems_to_ignore: [/boot/efi]
        fail_when_measurement_above:
          space_used_percent: 90
        alert_severity: warn
```

Every minute, MMWatchdog on the node lists the mounted filesystems and
measures the covered disk and network filesystems, except the filesystem
at `/boot/efi`. While all covered filesystems are within the limit, the
message is, for example, `3 filesystems are within the limits (not counting
1 in filesystems_to_ignore: /boot/efi)`.

When the filesystem at `/var` is 93 percent full for 10 minutes, the node
`web-3` sends the `warn` alert `web-3:
filesystems/every_filesystem_is_below_90_percent: /var failed`, with the
message `/var: space_used_percent is 93 (fails above 90)`. The check's own
message is then `1 filesystem fails: /var`. If `/srv` passes the limit an
hour later, it gets an alert of its own. Each filesystem gets a separate
recovery alert after the filesystem stays below the limit for 5 minutes.

`mmwatchdog status` shows the check with one line for each filesystem that fails,
and JSON status lists the failing filesystems as `findings`.

## How it works

### Which filesystems the check covers

- **Filesystems from a disk device:** every filesystem whose source, in the
  kernel's list of mounts, is below `/dev/`.
- **Filesystems from another machine or from a ZFS pool:** the kinds `nfs`,
  `nfs4`, `cifs`, `smb3`, `ceph`, `9p`, `virtiofs`, `lustre`, `afs`,
  `fuse.sshfs`, `fuse.glusterfs`, `fuse.ceph`, and `zfs`.
- **Left out:** what the kernel provides in memory, such as `/proc`, `/run`,
  and every `tmpfs`, and an image that is read-only by its nature, such as
  a CD or the package of a snap (`squashfs`, `iso9660`, `udf`, `erofs`,
  `cramfs`, `romfs`). A filesystem of another kind, such as an overlay of a
  container or `fuse.rclone`, gets its own `node_filesystem` check.
- **A filesystem that is mounted at several paths** is covered once, and
  named by the path that the kernel lists first.
- **Skipped without a finding or an error message:** a filesystem unmounted
  between reading the mount list and asking the kernel for filesystem usage.
  MMWatchdog also skips a filesystem when the kernel gives no size.
  A path that cannot fit a finding identity uses the path's SHA-256 checksum.
  Permission errors remain findings.

### The limits

- You can set limits on the measurements of `node_filesystem`:
  `space_used_percent`, `inodes_used_percent`, and `space_free_gib`
  ([checks that measure a number](../../docs/reference/configuration-files.md#checks-that-measure)).
- A limit on `inodes_used_percent` applies to the filesystems that count
  their inodes. A filesystem that does not, such as btrfs, still has its
  limits on space.
- The check records no readings in the node's
  [measurement history](../../docs/how-to/look-back-at-measurements.md). To record
  the readings of one filesystem, give it a `node_filesystem` check.

### `filesystems_to_ignore`

- **The list is required,** so a reader sees whether any filesystem is left
  out. An empty list, `[]`, leaves none out.
- **Each entry is a path where a filesystem is mounted.** For a filesystem
  that is mounted at several paths, any of them leaves it out. The order of
  the list does not matter: a reload that only reorders it changes nothing.
- **A filesystem that has its own `node_filesystem` check with a limit**
  belongs in the list, or one full filesystem sends two alerts. `mmwatchdog
  validate` prints a [validation warning](../../docs/reference/commands.md#validation-warnings)
  that names every such filesystem that is missing from the list.

## Errors and what they mean

Each validation error starts with the file, the line, and the path of the
setting, such as `check_groups.filesystems.checks.every_filesystem_is_below_90_percent.every_disk_and_network_filesystem.filesystems_to_ignore:`.

- **`is required: the paths where the filesystems are mounted that this
  check leaves out, such as [/boot/efi], or [] to leave none out`.** The
  check has no `filesystems_to_ignore`.
- **`expected distinct absolute paths where filesystems are mounted, such
  as [/boot/efi, /mnt/backup]`.** An entry is relative, is not written in
  plain form (such as `/boot/efi/`), or appears twice.
- **`alert_severity: required: info, warn, or sos`.** The check states no
  `alert_severity`; this check always alerts.
- **`this check has no such measurement; it measures space_used_percent,
  inodes_used_percent, space_free_gib`.** A limit names another
  measurement.
- **`expected a number from 0 to 100`** for a limit on a `_percent`
  measurement, and **`expected a number of 0 or more`** for a limit on
  `space_free_gib`.

The validation warning for a filesystem with a check of its own is one
line, followed by one line for each such filesystem:

```text
An every_disk_and_network_filesystem check alerts for every filesystem beyond the check's limits and outside the check's filesystems_to_ignore list. The filesystems listed below also have a node_filesystem check with a limit, so a full filesystem would send two alerts. Add each listed filesystem to filesystems_to_ignore:
  filesystems/every_filesystem_is_below_90_percent: /srv/data is missing from filesystems_to_ignore; data_filesystem/node_filesystem also checks the filesystem
```

`mmwatchdog validate --json` lists the affected filesystems in
`filesystems_missing_from_filesystems_to_ignore`.
The configuration stays valid. Add each mount path named in the warning to
`filesystems_to_ignore`.

## Tested on

`TestFilesystemFindingsKeepLongPathsAndUnknownObservations` checks stable
identities for long mount paths and findings for invalid filesystem counts.
`TestLongFilesystemFindingRecoveryNamesPathAfterRestart` in
`internal/daemon/failed_units_test.go` verifies failure, daemon restart, and
recovery with the full mount path.

- **Go tests** in `internal/checksandalerts/node_measurements_test.go` check
  coverage of disk and network filesystems once each, with memory
  filesystems, overlays, and inherently read-only images excluded. The tests
  check findings when a filesystem passes a limit, is read-only, does not
  answer, or answers with an error. The tests check ignored filesystems in
  the message and inode limits skipped where no inodes are counted. The
  tests also check inputs and the validation warning.
  `cmd/mmwatchdog/main_test.go` covers the warning in the output of
  `mmwatchdog validate`.
- **On real guests**, the suite `node_scenarios.py` mounts three
  filesystems of 32 MiB on loop devices. A filesystem is filled past the
  limit, and another filesystem is remounted read-only. Each failing
  filesystem gets a separate alert and recovery, while the guest's
  remaining filesystems stay healthy. The full filesystem in
  `filesystems_to_ignore` sends nothing.

  The suite also checks that the count of covered filesystems matches the
  guest's filesystems on disk devices, and the warning of `mmwatchdog validate`.
- The suite and the Go tests passed on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)
  in the latest run of all integration test suites
  ([validation record](../../docs/changing-mmwatchdog/validation.md)). A filesystem that does
  not answer or answers with an error is covered by Go tests only.

## Why it works this way

- **Unreadable filesystems remain visible as findings.** Silence about a
  permission error or unknown measurement would look like confirmed health.
- **Long paths use a stable checksum for identity.** The finding protocol's
  size limit must not exclude a filesystem from monitoring.

- **The check covers filesystems without a list.** A filesystem that
  nobody listed is the filesystem that fills up.
- **Each filesystem is its own incident.** A second full filesystem gets
  an alert at once. Each repaired filesystem reports a
  separate recovery, as each failed unit does in
  [`every_systemd_unit`](check_every_systemd_unit.md)
  ([check group decisions](../../docs/changing-mmwatchdog/design/checks.md#check-group-decisions)).
- **A filesystem that does not answer, or answers with an error, fails the
  check,** where a [`node_filesystem`](check_node_filesystem.md) check of
  that filesystem is unknown. It is one finding among the others: one
  filesystem that MMWatchdog cannot measure must not make the whole check unknown
  and hide the other filesystems.
- **`filesystems_to_ignore` is required.** A reader sees at once whether any
  filesystem is left out.
- **A filesystem with a separate check belongs in `filesystems_to_ignore`.** MMWatchdog warns instead of
  automatically excluding the separately checked filesystem. Silently
  excluding separately checked filesystems would make
  `filesystems_to_ignore` untrue, but a warning keeps the configuration
  valid.
- **The check always states an `alert_severity` and records no readings.**
  The `every_disk_and_network_filesystem` check alerts for any filesystem.
  The readings of a filesystem that matters belong to a separate
  `node_filesystem` check.
- **Memory filesystems and images are left out.** Memory filesystems such
  as `tmpfs` and `/proc` are not storage that fills up. An
  image such as a snap is always full and always read-only.
