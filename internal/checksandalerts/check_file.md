# `file`

A `file` check looks at one file or directory on the node: whether it
exists, who owns it, what its permissions are, and how old it is, and for a
regular file also what its content is and how large it is. MMWatchdog asks the
kernel about the path and, for a file's content, reads it, as root. The
check fails when nothing is at the path, unless it states
`must_not_exist: true`. With `must_not_exist: true`, an existing path fails
the check, and a missing path passes.

To watch the lines that a program adds to a log file, use
[`log_lines`](check_log_lines.md). A job can write a new file name on each
run, such as a backup with the date in the name. For jobs with changing
output file names, a [heartbeat](../../docs/how-to/report-from-a-scheduled-job.md) sent by the
job when the job finishes is better.

## When the check fails, and when it cannot check

- **A missing file fails the check,** with the message
  `/var/backups/database.sql.gz does not exist`, unless the check states
  `must_not_exist: true`. With `must_not_exist: true`, the check fails
  while the checked file exists, with
  `/srv/myapp/maintenance.flag exists`, and is
  healthy with `/srv/myapp/maintenance.flag does not exist`.
- **The path names a regular file or a directory.** Anything else at the
  path fails the check, and the message says what is there, such as
  `/run/myapp/queue is a named pipe, not a regular file or a directory`. A
  symbolic link stands for the file or directory that it names.
- **A directory has no content to compare and no size.** A check of a
  directory with `expected_sha256_checksum` fails with `/etc/cron.d is a
  directory, and expected_sha256_checksum checks the content of a regular
  file`, and one with a limit on `file_size_mib` fails with `/var/backups is
  a directory, and the limit on file_size_mib in fail_when_measurement_below
  checks the size of a regular file`. Directory checks with a checksum or
  size limit stay failed until corrected or until a regular file replaces
  the directory.
- **Each `expected_` input that differs is named** with what the file has
  and what was expected, such as `/etc/ssh/sshd_config has permissions mode
  0644, expected 0600`. Several differences are joined in one message,
  separated by `;`.
- **A limit on a measurement** fails the check as for every module with
  measurements ([limits](../../docs/reference/configuration-files.md#checks-that-measure)).
  The message names each reading that has a limit, in front of what the
  check found: `file_age_hours is 30.5 (fails above 26); /var/backups/database.sql.gz exists`.
- **A healthy check says** `/etc/ssh/sshd_config is as expected`, or
  `/var/backups/database.sql.gz exists` for a check without an `expected_`
  input, after the readings that have a limit.
- **MMWatchdog cannot check, and the run is unknown,** when MMWatchdog
  cannot look at the path or read the file, for example a very large file with
  `expected_sha256_checksum` that MMWatchdog cannot read within
  `time_limit_per_run`. A file that was last changed in the future has no
  age, so a check with a limit on its age is then unknown, and its message
  gives the file's last modification time.
- **A file on a network filesystem whose server is gone** cannot be
  examined: the kernel holds the request without end. No time limit ends
  the blocked request. The check is then unknown, and the run releases
  the run's place so the node's remaining checks can run
  ([a run that does not end](../../docs/how-it-works.md#a-run-that-does-not-end)).
  A [`node_filesystem`](check_node_filesystem.md) or an
  [`every_disk_and_network_filesystem`](check_every_disk_and_network_filesystem.md) check
  says in plain words that such a filesystem does not answer.

## Examples

### A backup that is fresh and not empty

```yaml
# /etc/mmwatchdog/check_groups/files.yml
check_groups:
  files:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    checks:
      database_backup_is_fresh_and_not_empty:
        file:
          path: /var/backups/database.sql.gz
        fail_when_measurement_above:
          file_age_hours: 26          # fails when the file was last changed more than 26 hours ago
        fail_when_measurement_below:
          file_size_mib: 1            # fails when the file is smaller than 1 MiB
        alert_severity: sos
```

Every 5 minutes, MMWatchdog on the node looks at `/var/backups/database.sql.gz`. The
check fails when the file is missing, when it was last changed more than 26
hours ago, or when it is smaller than 1 MiB. When the check has failed for 10
minutes, MMWatchdog sends the `sos` alert. A nightly backup that runs late
by more than 2 hours therefore alerts, as does a backup that wrote an
almost empty file.

### A settings file that must not change

```yaml
# /etc/mmwatchdog/check_groups/files.yml
check_groups:
  files:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    checks:
      ssh_server_settings_are_unchanged:
        file:
          path: /etc/ssh/sshd_config
          expected_owner: root
          expected_group: root
          expected_permissions_mode: "0600"
          expected_sha256_checksum: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
        alert_severity: warn
```

The check fails when the settings file of the SSH server has another owner,
another group, other permissions, or other content than stated. `sha256sum
/etc/ssh/sshd_config` on the node prints the checksum to write. The
permissions are in quotes, because YAML would read `0600` without quotes as
a number.

### A directory with its owner and permissions

```yaml
# /etc/mmwatchdog/check_groups/files.yml
check_groups:
  files:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    checks:
      cron_jobs_directory_is_as_expected:
        file:
          path: /etc/cron.d
          expected_owner: root
          expected_group: root
          expected_permissions_mode: "0755"
        alert_severity: warn
```

The check fails when `/etc/cron.d` is missing, has another owner or group,
or other permissions, such as `0777`, which lets every account add a job
that cron runs as root. The check reads only the directory itself, not the
files in it.

### A file that must be gone

```yaml
# /etc/mmwatchdog/check_groups/files.yml
check_groups:
  files:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    checks:
      maintenance_flag_is_gone:
        file:
          path: /srv/myapp/maintenance.flag
          must_not_exist: true        # the check fails while this file exists
        alert_severity: warn
```

The check fails while the file `/srv/myapp/maintenance.flag` exists. When
someone forgets to remove `/srv/myapp/maintenance.flag` after maintenance,
MMWatchdog sends the `warn` alert after 10 minutes.

## How it works

- **MMWatchdog looks at the file as root, so MMWatchdog can check any file.**
- **For the checksum, MMWatchdog opens the file at `path` once.** MMWatchdog
  takes the owner, permissions, size, and content from the open file.
  Replacing the file at the path during the run cannot combine properties
  from the original file and the replacement file. MMWatchdog opens `path` so
  that a named pipe at `path` cannot hold up the run.
- **The owner and the group** are compared by numeric ID first, so
  `expected_owner: "0"` also works. An owner or group absent from the
  node's own files, such as an account of a directory service, is found
  through the node's `getent` program. MMWatchdog runs `getent` as the
  unprivileged account `mmwatchdog`. An unknown ID is shown as a number.
- **The permissions** include the set-user-ID, set-group-ID, and sticky
  bits, as `chmod` writes them. Mode `"4755"` makes an executable run with
  the file owner's user ID.
- **The age** is the time since the file's content was last changed.
  The age has three units so that a limit reads naturally. A directory's age is the
  time since an entry was last added, removed, or renamed in it; a change
  of the content of a file in it does not count.

## Errors and what they mean

Validation errors name the check, such as
`check_groups.files.checks.ssh_server_settings_are_unchanged.file.expected_permissions_mode: ...`:

| Message | What the message means, and what to do |
| --- | --- |
| `path: is required: the absolute path of the file or directory that the check looks at` | `path` is missing, relative, or not in its shortest form, such as `/var//log` or `/var/log/`. |
| `expected_owner: a check with must_not_exist: true looks only at whether the file exists; remove expected_owner` | A check with `must_not_exist: true` rejects every `expected_` input, including `expected_owner`. |
| `fail_when_measurement_above: a file check with must_not_exist: true looks only at whether the file exists; remove fail_when_measurement_above` | A missing file has no age or size, so `must_not_exist: true` rejects `fail_when_measurement_above` and `fail_when_measurement_below`. |
| `expected_group: cannot be empty and holds no white space` | An `expected_` input is empty or holds a space. |
| `expected_permissions_mode: expected the permissions as chmod writes them, in quotes, such as "0600"` | Write three or four octal digits in quotes. |
| `expected_sha256_checksum: expected a SHA-256 checksum: 64 hex characters in lower case, as sha256sum prints them` | Copy the checksum from `sha256sum`. |

Messages of a run:

| Message | What the message means, and what to do |
| --- | --- |
| `/etc/ssh/sshd_config is owned by the user account myapp, expected root` | The owner differs from `expected_owner`. Messages for group, permissions, and checksum differences also give the actual and expected values: `belongs to the group ...`, `has permissions mode 0644, expected 0600`, `has the SHA-256 checksum ..., expected ...`. |
| `/etc/cron.d is a directory, and expected_sha256_checksum checks the content of a regular file` | A checksum needs a regular file. Remove `expected_sha256_checksum`, or point `path` at a file. |
| `/var/backups is a directory, and the limit on file_size_mib in fail_when_measurement_below checks the size of a regular file` | A directory has no size. Remove the limit, or point `path` at a file. |
| `/run/myapp/queue is a named pipe, not a regular file or a directory` | Something else than a file or a directory is at the path. |
| `MMWatchdog could not read all of /srv/big.img within the time limit of the run` | The file is too large to read for its checksum within `time_limit_per_run`. Give the check group a longer `time_limit_per_run`. |
| `MMWatchdog cannot look at PATH: ...`, `MMWatchdog cannot read PATH: ...` | The kernel refused, and the message gives its error. |
| `PATH was last changed at 2027-01-01 00:00 UTC, which is in the future, so its age cannot be measured` | The file's time of its last change is wrong. `touch` the file, or fix the program that wrote it. |

## Tested on

- **Go tests** in `node_checks_test.go` cover the following cases:
  - `TestFileCheck` covers a missing file with and without `must_not_exist`,
    and age and size with limits above and below. The test checks owner,
    group, permissions with a set-user-ID bit, and checksum. The test checks
    that each difference is named and multiple differences are joined.
    Further cases cover numeric account IDs, a file unreadable
    within the time limit, a file from the future, and a symbolic link.
    The test also covers a named pipe at the path and validation of every
    input.
  - `TestFileCheckOfADirectory` covers a directory's owner, group,
    permissions, and age, and an entry added to the directory. The test
    checks that a checksum and a size limit fail, and covers `must_not_exist`.
  - `TestAccountNamesOfADirectoryService` covers names from `getent`.
- **The node suite** (`node_scenarios.py`), on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution):
  a backup file that is missing, fresh, too small, and too old; a settings
  file whose permissions and content change; a directory whose permissions
  change; a flag file that must not exist; and eight files on an NFS filesystem whose server stops, whose
  checks say that their run did not end and are healthy again when the
  server is back.
- The latest run passed on every target distribution
  ([validation record](../../docs/changing-mmwatchdog/validation.md)). Go tests only, not on a
  real node: an owner or a group of a directory service, and a filesystem
  that answers with an error such as a stale NFS handle. A stale NFS handle
  means the NFS server no longer recognizes the client's reference to a file.

## Why it works this way

- **`file` is a core module.** A file that must exist or must not exist,
  its owner and permissions, its checksum, its size, and its age are among
  the most common checks of real monitoring configurations. Checks of file
  existence, owner, permissions, checksum, size, and age are useful on any
  Linux server.
- **The limits on age and size are ordinary limits on measurements.** One
  way to write a limit serves every module, and the readings reach the
  measurement history.
- **The age has three units.** `file_age_hours: 26` reads more naturally
  than `file_age_minutes: 1560`.
- **`must_not_exist: true` takes no other input and no limit.** A missing
  file has no owner, age, or size.
- **A directory is accepted.** Of the Monit checks collected for beta
  validation, 167 looked at a directory, mostly at the directory's owner,
  group, and permissions. Without the `file` check, each directory check would need
  a shell check.
  A checksum or a size limit on a directory fails at the run and is not
  refused when the configuration loads, because only the node knows what
  is at the path. A `file` check fails when the path names a named pipe,
  a socket, or a device. The collection of Monit checks used for the
  comparison had 4 checks of a named pipe, and a socket has its
  [`unix_socket`](check_unix_socket.md) check.
- **The checksum check reads one open file.** A file that another file
  replaces during the run then shows one file only, never the owner of one
  and the content of the other.
