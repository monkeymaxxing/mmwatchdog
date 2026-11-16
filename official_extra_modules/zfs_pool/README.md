# `zfs_pool`

`zfs_pool` checks one ZFS pool on the node. `zfs_pool` is an
[official extra module](../../docs/reference/modules/README.md): a
Starlark script written by the MMWatchdog project. Every deploy with the
release's MMX workflows installs `zfs_pool` on every node. A check names the module
and the pool, and MMWatchdog runs the module once for each run of the check, on the
node.

The module asks the `zpool` program whether the pool is healthy, how full it
is, how fragmented its free space is, and how many days ago it was last
scrubbed. A scrub is ZFS's own check of a pool: a scrub reads every block
and repairs blocks that do not match the blocks' checksum from another copy.
The check fails while the pool is not healthy. A measurement limit can
make the check fail when the pool is too full or has gone too long without a scrub.

Choose `zfs_pool` for the health and the space of a whole pool. The core
module [`node_filesystem`](../../docs/reference/modules/node_filesystem.md) measures
one mounted filesystem, such as one ZFS dataset, and knows nothing of the
pool's devices.

## Set up

- **The programs `zpool` and `zfs`,** which every node with ZFS has in the
  same package, such as `zfsutils-linux` on Ubuntu.
- **No special account.** The check runs as the unprivileged account `mmwatchdog`,
  which may list a pool, read its status, and read when it was created.
- **Something that scrubs the pool,** for a limit on `days_since_last_scrub`.
  Ubuntu's and Debian's package `zfsutils-linux` scrubs every pool on the
  second Sunday of each month. Elsewhere, enable OpenZFS's monthly timer
  for each pool, such as `systemctl enable --now zfs-scrub-monthly@tank.timer`.

## Inputs

| Input | Meaning | When omitted |
| --- | --- | --- |
| `pool_name` | The name of the ZFS pool, as `zpool list` prints it, such as `tank` | Required: `mmwatchdog validate` refuses the check |

## Measurements

| Measurement | Unit | Meaning |
| --- | --- | --- |
| `space_used_percent` | percent | The share of the pool's space that is in use, 0 to 100 |
| `free_space_fragmentation_percent` | percent | How fragmented the free space of the pool is, which `zpool list` prints in its column `FRAG`. The number says nothing about how fragmented the files are. A pool without a number in `FRAG` has no `free_space_fragmentation_percent` measurement |
| `days_since_last_scrub` | days | The time since the last scrub of the pool ended, such as `14.5`. The next table says what `days_since_last_scrub` counts from while a scrub has not ended |

A check can set limits on `space_used_percent`,
`free_space_fragmentation_percent`, and `days_since_last_scrub`.
Write `fail_when_measurement_above` or `fail_when_measurement_below` beside
the module, and MMWatchdog applies the limits
([checks that measure a number](../../docs/reference/configuration-files.md#checks-that-measure)).
A check with a limit on `free_space_fragmentation_percent` is unknown for a
pool that has no such number.

### What `days_since_last_scrub` counts from

The module reads the line `scan:` of `zpool status tank`. When the line
says a scrub completed, MMWatchdog keeps the completed scrub's date between runs.
A **resilver** copies data onto a disk that was replaced or came back.
A resilver is not a scrub: it does not read every block of the whole pool.
ZFS replaces the previous scan line when another scan starts. MMWatchdog's saved
completed scrub date stays unchanged until another scrub completes.

| What MMWatchdog has observed | `days_since_last_scrub` counts from | The message of a healthy run ends with |
| --- | --- | --- |
| The scan line says `scrub repaired ... on Sun Oct 11 00:36:02 2026` | The end of the scrub named in the scan line. MMWatchdog saves the date | `; its last scrub ended on Sun Oct 11 00:36:02 2026` |
| A scrub runs, is paused or canceled, or a resilver replaces the scan line | The saved date of the last completed scrub | `; its last completed scrub observed by MMWatchdog ended on Sun Oct 11 00:36:02 2026` |
| MMWatchdog has no saved completed scrub date, and the scan line shows none | The pool's creation | `; MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation` |

Without a saved completed scrub date, the pool's creation is a conservative
starting point. A completed scrub that MMWatchdog never observed cannot be recovered
from a later scan line. zpool writes each date in the node's own time zone,
and the module reads the date in that same time zone.

## When the check fails, and when it cannot check

| The pool | The run | Message |
| --- | --- | --- |
| Is `ONLINE` and `zpool status -x` calls the pool healthy | Healthy | `the ZFS pool tank is ONLINE and healthy` |
| Is in another state, such as `DEGRADED` or `FAULTED` | Failed | `the ZFS pool tank is DEGRADED`, and after a colon what `zpool status` says is wrong |
| Is `ONLINE` and has a problem, such as a device with errors | Failed | `the ZFS pool tank is ONLINE and has a problem`, and after a colon what `zpool status` says is wrong |
| Is unavailable to programs on the node because the node did not import the pool | Failed | `this node has no ZFS pool named tank` |
| Is above a limit on a measurement | Failed | `space_used_percent is 83 (fails above 80); the module reported: the ZFS pool tank is ONLINE and healthy; its last scrub ended on Sun Oct 11 00:36:02 2026` |
| Has gone longer without a scrub than a limit allows | Failed | `days_since_last_scrub is 40.5 (fails above 35); the module reported: the ZFS pool tank is ONLINE and healthy; its last scrub ended on Sun Sep 6 00:31:12 2026` |
| Cannot be asked or listed, or `zpool` cannot say whether the pool has a problem | Unknown | See [errors and what they mean](#errors-and-what-they-mean) |

- **A pool that is missing fails the check on purpose:** a pool that was not
  imported at the start of the machine is not there for the programs that
  use it.
- **What `zpool status` says is wrong** is the text after its heading
  `status:`, which `zpool` writes on several lines. The module joins the
  text after `status:` into one line.
- **The module reports the measurements with every run that lists the
  pool,** also with a failed one.
- **MMWatchdog applies a limit only to a healthy run.** A failed run stays failed
  with the module's message. With a limit, the message of a healthy run
  starts with the reading and its limit, such as `space_used_percent is 42
  (fails above 80); the module reported: the ZFS pool tank is ONLINE and
  healthy; its last scrub ended on Sun Oct 11 00:36:02 2026`.
- **A resilver that runs fails the check by itself:** `zpool status -x`
  calls the pool's state a problem until the resilver ends, and the
  message says `the ZFS pool tank is ONLINE and has a problem: One or more
  devices is currently being resilvered.`

MMWatchdog sends the alert when the check has failed for `alert_after_failing_for`.
When the check has been unknown for `alert_after_failing_for`, MMWatchdog
opens a cannot-check incident
([the failing time](../../docs/how-to/decide-when-a-check-alerts.md)).

## Examples

### A pool that is healthy and below 80 percent

```yaml
# /etc/mmwatchdog/check_groups/storage.yml
check_groups:
  storage:
    check_every: 1m
    time_limit_per_run: 20s
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 1m
    checks:
      pool_tank_is_healthy_and_below_80_percent:
        zfs_pool:                     # the module
          pool_name: tank             # the pool, as zpool list prints it
        fail_when_measurement_above:
          space_used_percent: 80
        alert_severity: sos
```

Every minute MMWatchdog runs the module on the node, as the account `mmwatchdog`. The check
fails while the pool `tank` is not healthy, and when the pool is more than
80 percent full. A ZFS pool gets slow long before it is full, so a limit of
80 percent is a common choice. When one disk of a mirrored pool fails, the pool
turns `DEGRADED`, and MMWatchdog sends the `sos` alert at once, with what
`zpool status` says is wrong.
After the disk is replaced and the pool is healthy for a minute,
MMWatchdog sends the "ok again".

### Two levels of space, and fragmentation

```yaml
# /etc/mmwatchdog/check_groups/storage.yml
check_groups:
  storage:
    check_every: 5m
    time_limit_per_run: 20s
    alert_after_failing_for: 15m
    recovered_after_healthy_for: 15m
    checks:
      pool_tank_is_below_80_percent:
        zfs_pool:
          pool_name: tank
        fail_when_measurement_above:
          space_used_percent: 80
          free_space_fragmentation_percent: 50
        alert_severity: warn
      pool_tank_is_below_90_percent:
        zfs_pool:
          pool_name: tank
        fail_when_measurement_above:
          space_used_percent: 90
        alert_severity: sos
```

The first check sends a `warn` alert when the pool has been more than 80
percent full, or its free space more than 50 percent fragmented, for 15
minutes. The second check sends an `sos` alert above 90 percent. Each check
also fails while the pool is not healthy, so a degraded pool sends the
`warn` and `sos` alerts.

### A pool that goes too long without a scrub

```yaml
# /etc/mmwatchdog/check_groups/storage.yml
check_groups:
  storage:
    check_every: 1h
    time_limit_per_run: 20s
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    checks:
      pool_tank_was_scrubbed_within_35_days:
        zfs_pool:
          pool_name: tank
        fail_when_measurement_above:
          days_since_last_scrub: 35   # a monthly scrub, with a few days to spare
        alert_severity: warn
```

Every hour MMWatchdog runs the module on the node. The pool `tank` is scrubbed once
a month, by the scrub of Ubuntu's or Debian's package or by OpenZFS's
monthly timer. When no scrub has ended for 35 days, because the timer is
off or a scrub never ends, the check fails.
MMWatchdog then sends a `warn` alert.

The check also fails while the pool is not healthy. A scrub that runs or is
canceled keeps the reading counting from the last completed scrub. A new
pool counts from creation, so the new pool sends no alert in the first
35 days. After the next scrub ends, the check is healthy again and
MMWatchdog sends the "ok again".

## How it works

Each run of the check is one run of the script `module.star`, as the account
`mmwatchdog`, under the check's `time_limit_per_run`:

1. The module runs `zpool list -H -p -o name,health,capacity,fragmentation tank`.
   `-H` writes one line without a heading, with a tab between the columns,
   and `-p` writes plain numbers: `42` and not `42%`.
2. The module runs `zpool status tank`, for what is wrong with the pool and
   for its scan line. A completed scrub updates the date kept with
   `write_state`; other scan lines use the date from `read_state`.
   `date --file=- +%s` converts the completed date and `now` into seconds
   since 1970. With no known completed scrub, the module gets the pool's
   creation from `zfs get -H -p -o value creation tank`.
   The module subtracts the last completed scrub's timestamp, or the pool's
   creation timestamp, from the current timestamp.
   Dividing the elapsed seconds by 86,400, the seconds in a day, gives
   `days_since_last_scrub`.
3. A pool that is not `ONLINE` fails, with what `zpool status` says is
   wrong.
4. For a pool that is `ONLINE`, the module runs `zpool status -x tank`,
   which says `is healthy` for a pool without a problem. Another answer
   fails, with what `zpool status` says is wrong.

## Errors and what they mean

| Message | What the message means and what to do |
| --- | --- |
| `check_groups.storage.checks.pool_tank_is_healthy_and_below_80_percent.zfs_pool.pool_name: is required: ...` | `mmwatchdog validate`: the check leaves out `pool_name` |
| `check_groups.storage.checks.pool_tank_is_healthy_and_below_80_percent.fail_when_measurement_above.used_percent: this check has no such measurement; it measures ...` | `mmwatchdog validate`: a limit names a measurement that the module does not report. Use `space_used_percent` or `free_space_fragmentation_percent` |
| `this node has no ZFS pool named tank` | Failed. The pool is not imported on this node. Import the pool with `zpool import tank`, or correct `pool_name` |
| `the ZFS pool tank is ONLINE, and zpool could not say whether it has a problem: ...` | Unknown. `zpool status -x` ended with an error, and the text after the colon says why |
| `zpool could not list the pool tank: ...` | Unknown. `zpool list` failed for another reason than a missing pool, and the text after the colon is zpool's own words |
| `zpool gave an answer about the pool tank that the module does not understand: ...` | Unknown. The answer of `zpool list` has another form than the module reads, and the text after the colon is its first line |
| `MMWatchdog cannot run the zpool program: ...` | Unknown. The node has no `zpool` program, or the account `mmwatchdog` cannot run it. Install ZFS, or remove the check from a node without ZFS |
| `the result has no measurement named free_space_fragmentation_percent, which this check has a limit on` | Unknown. The pool has no fragmentation number. Remove the limit on `free_space_fragmentation_percent` for this pool |
| `the result has no measurement named days_since_last_scrub, which this check has a limit on` | Unknown. The module could not measure the time since the last scrub. Run `sudo mmwatchdog check-now storage`; the message of a check without a limit on `days_since_last_scrub` ends with one of the texts below |
| `...; MMWatchdog could not read when it was last scrubbed: zpool status ended with ...` | `zpool status tank` failed, and the text after `ended with` is zpool's own words |
| `...; MMWatchdog could not read the date Sun Oct 11 00:36:02 2026 with the date program: ...` | The `date` program did not read the date of the scan line. The text after the colon is the `date` program's own words |
| `...; MMWatchdog has not observed a completed scrub and could not read when the pool was created: zfs get creation ended with ...` | No completed scrub date is known, and `zfs get creation` failed, for example because the node has no `zfs` program |

The errors that every module that is not core can meet, such as a run past
`time_limit_per_run`, are in
[checks with a module that is not core](../../docs/reference/configuration-files.md#run-settings-of-a-shell-check-and-of-a-module-that-is-not-core).

## Tested on

- **On Ubuntu 24.04 and 26.04, against a real pool**
  (`tests/integration/service_scenarios.py`, in the services suite). The
  scenario installs Ubuntu's package `zfsutils-linux`, and loads the ZFS
  kernel module that comes with Ubuntu's kernel. The pool mirrors two
  files. The check finds the pool healthy and measures its use and its
  fragmentation, fails with zpool's own reason while one of the two files
  used as mirrored devices is offline, and fails when the pool is exported.
  A pool that is not there fails.

  A check with a limit on `days_since_last_scrub` reads the real scan line
  in each form: a new pool that was never scrubbed, a scrub that ended, a
  scrub that runs, is paused, and is canceled, and the resilver after a
  mirrored device came back.
  OpenZFS's setting `zfs_scan_suspend_progress` suspends scrub progress
  while ZFS still reports the scrub as running.

  Ubuntu 24.04 has OpenZFS 2.2.2, and Ubuntu 26.04 has 2.4.1.
  OpenZFS 2.1 and 2.3, which target distributions outside Ubuntu can install,
  write the scan line with the words used by OpenZFS 2.2.2 and 2.4.1.
- **On every other target distribution, on a node without ZFS:** Debian 12
  and 13, Rocky Linux 9, AlmaLinux 10, Fedora 45, Arch Linux, and openSUSE Leap
  ([every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)).
  The check is unknown, with `MMWatchdog cannot run the zpool program: `.

  Ubuntu is the one target distribution whose own packages hold ZFS ready
  to use. Debian and the others build the kernel module on the node, and the Red Hat
  family needs the OpenZFS project's own package source. MMWatchdog's
  tests build no kernel module and add no package source. The module only reads what
  `zpool` prints, so its code is the same on every distribution.
- **Go tests** (`tests/official_extra_modules/check_modules_test.go`,
  `TestZFSPool`) run the script through `mmwatchdog run-starlark-module` with
  stand-ins for `zpool`, `zfs`, and `date` that write answers in their
  output formats. Pool health cases cover healthy, degraded with zpool's
  whole reason, online with a problem, a missing pool, a `zpool` that
  cannot report health, and a missing `zpool` program.
  Missing-measurement cases cover a missing fragmentation number and a
  date that `date` cannot read.

  Scrub-age cases cover each scan-line form in the scrub-age table,
  including a day of the month below 10. Separate module runs test retention
  of a completed scrub date through a running, paused, or canceled scrub
  and a resilver. The Go tests ran with `go test ./...` on macOS on ARM64.

## Why it works this way

- **The module asks `zpool` and reads its plain numbers.** `zpool` is the
  program that every node with ZFS has, and `-H -p` gives one line that a
  script reads without guessing.
- **For an `ONLINE` pool, the module also runs `zpool status -x`.** A pool
  can be `ONLINE` and still have a problem, such as a device with errors,
  and only `zpool status -x` says so.
- **A missing pool fails, and is not unknown.** The pool is not there for the
  programs that use it, which is a failure that someone must fix.
- **The module measures the time since the last scrub.** In the beta
  validation with real-world Monit work, a company's production setup
  checked ZFS pools with a shell script of 147 lines. The script also
  alerted on a pool that had gone 90 days without a scrub. A limit on
  `days_since_last_scrub` alerts on overdue scrubs without a script.
- **Only a completed scrub resets the reading.** Starting or canceling a
  scrub and replacing a disk do not prove the whole pool was checked.
  Keeping the completed date prevents a started or canceled scrub, or a
  disk replacement's resilver, from hiding an overdue scrub.
- **Without a known completed scrub, the reading counts from creation.**
  ZFS keeps only its latest scan line, so an older completed scrub may no
  longer be visible when MMWatchdog first runs. Counting from creation is
  conservative and does not let a canceled scrub make an old pool healthy.
- **The module asks the `date` program for the time.** A script has no
  clock of its own. The `date` program reads zpool's date in the node's own time zone, in which zpool writes the date.
- **The measurement is named `free_space_fragmentation_percent`.** `FRAG`
  measures the free space only, and the name says so, so that nobody reads
  it as the fragmentation of the files.
- **The module needs no root.** The account `mmwatchdog` may list a pool and read
  its status.
- **`zfs_pool` is an official extra module, not a core module.** ZFS is
  uncommon work on a Linux node. A script can follow `zpool` in a new
  release without a new `mmwatchdog` program.
