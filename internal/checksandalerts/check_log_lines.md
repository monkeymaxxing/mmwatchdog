# `log_lines`

A `log_lines` check reads the lines that a program adds to a log on the
node, from a file or from a systemd unit's journal. The journal is the
system log that systemd keeps for every unit; `journalctl -u myapp.service`
shows the journal's lines. A run reads only the lines that are new since the run
before. A run fails when a new line contains the configured text or matches
a regular expression, or when the source got no new line for a stated time.

To look at a file as a whole, such as its age, size, or checksum, use
[`file`](check_file.md).

## When the check fails, and when it cannot check

A check states exactly one source, `log_file_path` or
`journal_of_systemd_unit`, and exactly one condition,
`fail_when_a_new_line_contains`, `fail_when_a_new_line_matches_regex`, or
`fail_when_no_new_line_for`.

- **A check of new lines fails only in the run that reads a matching
  line,** with a message such as `1 new line in
  /var/log/myapp/application.log contains "FATAL"`. The next run without a
  matching line is healthy. The first matching run opens the incident.

  The check refuses `alert_after_failing_for` and ignores the check group's
  value. The `recovered_after_healthy_for` timer starts at the first run
  that finds no new matching line. The recovery timer can start up to one
  `check_every` after the last run that reports a matching event
  ([the failing time](../../docs/how-to/decide-when-a-check-alerts.md)).
- **The alert's body shows the first matching lines,** at most five, and
  counts the rest, such as `and 3 more`. A line is cut after 200 bytes.
  MMWatchdog replaces characters that can change how a terminal or mail program
  shows surrounding text with spaces: control characters, Unicode line separators,
  and text-direction characters.
- **A healthy run of a check of new lines** says how many new lines it
  read, such as `none of 4 new lines in /var/log/myapp/application.log
  contains "FATAL"`.
- **A check of silence fails** when the source got no new line for the
  stated time. The silence check counts from the newest line that MMWatchdog
  has seen, or from the check's first run when MMWatchdog has seen no line, and the
  message says which:
  `the journal of myapp-worker.service got no new line for 12m0s since the
  newest line (fails after 10m)`, or `... since MMWatchdog started watching the log`. A healthy run says how long the log has had no new line: `the journal of
  myapp-worker.service received 3 new lines; no new line in the last 2m0s (fails
  after 10m)`.
- **The first run of a check starts at the end of the log** and is healthy,
  so earlier log lines cause no alert. The first run's message
  is `MMWatchdog starts watching new lines in /var/log/myapp/application.log from now on`.
  A check changed by a reload is a new check and starts at the end again.
- **MMWatchdog cannot check, and the run is unknown,** when the file does not
  exist, when something else than a regular file is under the name, when
  systemd does not know the unit, or when MMWatchdog cannot read the journal.
- **A run that cannot read all new lines** is unknown, and the next run
  continues where it stopped. One run processes at most 16 MiB of unread file
  data, plus the backward lookup for a silence check. A journal batch holds
  at most 8 MiB of what `journalctl` writes, which is the message,
  the place, and the time of each entry; a run that reaches its
  `time_limit_per_run` stops in the same way. A run that already found a
  matching line fails instead. A check of silence uses the time of the
  newest complete line; reading an old backlog does not prove recent activity.
  For rotated logs, MMWatchdog inspects both the old file and the current file before
  it drains the backlog. File modification times date their complete lines;
  journal entries carry their own timestamps.
  A matching line already found is still reported when a later read fails;
  the failure message also names the read problem. Without a match, a read
  error makes the run unknown and keeps the position for the next attempt.
  An oversized line checks its first 64 KiB and advances past its remainder.
  A matching prefix fails the check. An unmatched prefix makes the run unknown,
  naming the file and byte offset, or the journal's unit and cursor.
- **A log file on a filesystem of another machine that is gone** cannot be
  read: the kernel holds the question without end. The check is then
  unknown, and its run gives its place back, so the other checks of the
  node go on ([a run that does not end](../../docs/how-it-works.md#a-run-that-does-not-end)).

## Examples

```yaml
# /etc/mmwatchdog/check_groups/myapp_log.yml
check_groups:
  myapp_log:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 0s             # send the alert after the first run that fails
    recovered_after_healthy_for: 30m        # 30 minutes of health from the first run with no new match
    checks:
      application_log_has_no_fatal_lines:
        log_lines:
          log_file_path: /var/log/myapp/application.log
          fail_when_a_new_line_contains: "FATAL"
        alert_severity: warn
      worker_still_writes_log_lines:
        log_lines:
          journal_of_systemd_unit: myapp-worker.service
          fail_when_no_new_line_for: 10m
        alert_severity: sos
```

The first check watches the application's log file for lines with `FATAL`.
On the node:

1. At 14:02:10 the application writes the line `FATAL: cannot open database`.
2. The run at 14:03 finds one new line that contains `FATAL` and fails,
   with the message `1 new line in /var/log/myapp/application.log contains
   "FATAL"`. MMWatchdog sends the `warn` alert at once. The alert's body shows the line.
3. No further line with `FATAL` arrives. The first healthy run is at 14:04.
   At 14:34 the check has been healthy for 30 minutes, and MMWatchdog sends
   "recovered".

The second check watches the journal of `myapp-worker.service`. When the
worker has written no line for 10 minutes, the run fails, and MMWatchdog sends the
`sos` alert at once. A program that hangs often stops writing, so a check of
silence can also be listed in the `triggered_by_checks` of a
[restart](../../docs/how-to/restart-a-service-automatically.md#steps-to-restart-a-systemd-unit).

### A regular expression

```yaml
# /etc/mmwatchdog/check_groups/myapp_log.yml
check_groups:
  myapp_log:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 30m
    checks:
      worker_log_has_no_errors:
        log_lines:
          journal_of_systemd_unit: myapp-worker.service
          fail_when_a_new_line_matches_regex: '^(ERROR|PANIC):'
        alert_severity: warn
```

A run fails when a new line of the worker's journal starts with `ERROR:` or
`PANIC:`, with a message such as `2 new lines in the journal of
myapp-worker.service match the regular expression ^(ERROR|PANIC):`. The
regular expression is in single quotes, so that YAML takes every character
as written.

## How it works

### Where a run starts reading

- **MMWatchdog saves the reading position in the check's state,** including
  across a daemon restart. For a file, the saved reading position includes
  the inode, the number of bytes read, and a checksum of at most the first
  512 bytes. For the journal, the saved position is the cursor: the journal's
  name for the position of the last entry MMWatchdog read.
- **The first run of a journal check** notes the place of the unit's newest
  entry. A unit that has written nothing yet is read from the time of the
  first run.
- **`mmwatchdog check-now` reads from where the scheduled runs stopped**
  and leaves the saved reading position unchanged, so inspecting a check
  never hides a line from the next scheduled run.

### Log files that are rotated or emptied

- **A log file that was rotated loses no line.**
  A rotated log is one where the program, or `logrotate`, renamed the old
  file and started a new file at the original path.
  MMWatchdog keeps the file of the previous run open, reads
  that old file to its end first, and then reads the new file from its
  start. So the last words of a program before it started a new log, such
  as `FATAL: out of disk, shutting down`, reach the alert.
- **MMWatchdog holds the old file open until the next run,** so the disk space of a
  log that was rotated and removed is free one check interval later.
- **After a restart of MMWatchdog,** the check has only the name of the file. It
  knows the file again by its inode and by the checksum of its first bytes,
  and not by the number of its device, which can change when the machine
  starts. It then reads on from where it stopped. Any other file under the
  name is read from its start. Lines that an old file got while MMWatchdog was
  stopped, before the program started a new one, are not read.
- **A file that was emptied in place** is read from its start, and so is a
  file whose first bytes are no longer the ones of the previous run. The
  unread lines removed when the file was emptied or rewritten are lost,
  so no program can read the removed contents. `logrotate` empties
  a file in place with `copytruncate`; a log that MMWatchdog watches is better
  rotated by renaming.

### Lines

- **A line counts once the program has written its end.** MMWatchdog leaves a line
  that the program is still writing for the next run. In a file that is
  finished, because a new one took its name, the last line counts without
  its end. An oversized line is the exception: MMWatchdog can check its prefix before
  its end arrives, then discards the remainder through that end.
- **MMWatchdog matches at most the first 64 KiB of each line,** in a file and in the
  journal. A longer line with a matching prefix counts as a match. Otherwise
  the run is unknown, unless another line already matched. MMWatchdog names the
  oversized line's position and skips its remainder, so the next lines can
  still be checked. A file line longer than one run's read budget is skipped
  across runs; its prefix is checked once.
- **A run keeps no line that does not match.** MMWatchdog checks each new
  line while reading and keeps only the count and the first five matching
  excerpts. Memory use stays bounded as the number of lines increases.
- **The regular expression** uses
  [RE2 syntax](https://github.com/google/re2/wiki/Syntax).
  The `http` input `response_body_matches_regex` also uses RE2 syntax.
- **MMWatchdog opens the file once and reads from that open file,** so a named pipe
  under the name cannot hold a run up.
- **MMWatchdog reads the journal with `journalctl`,** as root: `journalctl
  --no-pager --quiet --output=json --all --output-fields=MESSAGE
  --unit=UNIT`, after the cursor of the run before. A message that is no
  valid text is read with each invalid byte replaced. If one journal entry
  exceeds the batch budget, MMWatchdog obtains its cursor without its message and
  reads a bounded message prefix. The next run starts after that cursor.
  The [journalctl reference](https://www.freedesktop.org/software/systemd/man/252/journalctl.html)
  documents the cursor and output options.

## Errors and what they mean

Validation errors name the check, such as
`check_groups.myapp_log.checks.application_log_has_no_fatal_lines.log_lines: ...`:

| Message | What the message means, and what to do |
| --- | --- |
| `states exactly one source: log_file_path or journal_of_systemd_unit` | The check must state exactly one of `log_file_path` and `journal_of_systemd_unit`. Write one check for each source. |
| `states exactly one condition: fail_when_a_new_line_contains, fail_when_a_new_line_matches_regex, or fail_when_no_new_line_for` | The check names no condition or several. Write one check for each condition. |
| `log_file_path: must be the absolute path of the log file` | The path is relative or not in the shortest form, such as `/var/log/myapp/`. |
| `journal_of_systemd_unit: must be the name of a systemd unit with its kind, such as myapp.service` | Write the unit's kind, such as `.service` or `.timer`. |
| `fail_when_a_new_line_contains: must be a text on one line that is not empty` | The text is empty or has a line break. |
| `fail_when_a_new_line_matches_regex: cannot be empty`, `fail_when_a_new_line_matches_regex: is no valid regular expression: ...` | Fix the regular expression; the message says what is wrong with it. |
| `fail_when_no_new_line_for: expected a duration longer than 0s, such as 10m` | Write a duration longer than `0s`. |

Messages of a run:

| Message | What the message means, and what to do |
| --- | --- |
| `/var/log/myapp/application.log does not exist` | No file exists at `/var/log/myapp/application.log`. Runs remain unknown while the configured path is missing. |
| `/var/log/myapp is a directory, not a regular file` | The path names something else than a file. |
| `MMWatchdog cannot look at /var/log/myapp/application.log: ...` | The kernel refused to open the file, and the message gives its error. |
| `systemd unit myapp-worker.service not found` | systemd does not know `myapp-worker.service`. Check the configured unit's name with `systemctl status`. Every check of an unknown systemd unit reports a unit-not-found message ([checks of a systemd unit](../../docs/reference/modules/systemd_unit_state.md)). |
| `MMWatchdog cannot read the journal of myapp-worker.service: ...` | `journalctl` failed, and the message gives what it wrote. |
| `/var/log/myapp/application.log has more new lines than MMWatchdog reads in one run, or than MMWatchdog could read within the time limit; the next run continues where the current run stopped` | The log grows faster than a run reads. Run the check more often, or give the check a longer `time_limit_per_run`. |
| `the line at byte ... exceeds 64 KiB; its first 64 KiB did not match, and MMWatchdog skips the rest of that line` | The unchecked remainder could contain a match. The run is unknown unless another line matched. MMWatchdog continues after the oversized line. Write shorter lines to check every byte. |
| `read error at byte ...: ...` | MMWatchdog could not read from the byte offset named in the message. A match already found still fails the check. Without a match, the run is unknown. The next run retries from the saved byte offset named in the read-error message. |

## Tested on

- **Go tests** in `node_checks_test.go` cover the following behavior:

  - `TestLogLinesCheckOfAFile`: the first run starts at the log's end, and
    later runs read only new lines. The test covers incomplete lines,
    five alert excerpts with a count of remaining matches, and replacement
    of control characters. The test also covers `mmwatchdog check-now`
    leaving the saved reading position unchanged, rotation, and emptying
    the file. Further cases cover a run ending with unread lines and
    regular-expression matching.
  - `TestLogLinesCheckOfASilentLog`: silence timing starts at the first
    run or at the newest line.
  - `TestLogLinesCheckOfAJournal`: runs continue after a saved journal
    cursor. The test covers a unit without entries, read from the first
    run's time, and a message containing invalid text. Further cases cover
    an unknown unit and a journal that cannot be read.
  - `TestTheInputsOfALogLinesCheck`.
  - `TestLogLinesCheckOfAFileThatIsReplaced`: the old file's last lines
    are read after renaming or removal, including a last line without an end. The
    test covers removal without a replacement at the
    configured path and contents replaced in place and grown past the saved byte
    offset. Restart cases cover resuming the previously read file, a new
    file at the configured path, and changed contents in the previously
    read file. The test checks that the saved `log_lines` reading state
    contains no device number and covers two million lines in a run.
    Further cases cover a line longer than a file-read piece and
    a named pipe.
  - `TestADetailLineHoldsNoCharacterThatChangesHowTextIsShown`: the alert body's lines.
  - `TestAProgramEndsAtTheLimitOfItsStandardOutput`: the limit on what
  `journalctl` writes.
- **Go reading checks:** `TestLogFileOversizedLinesAreCheckedAndSkipped`,
  `TestLogJournalOversizedEntriesAreCheckedAndSkipped`,
  `TestLogJournalReportsMatchesBeforeAReadError`, and
  `TestLogReadingRetriesFromTheReadErrorPosition`,
  `TestLogReadReportsCompleteMatchesBeforeUnexpectedEnd`,
  `TestOversizedLogLineSpanningRunsChecksItsPrefixOnce`, and
  `TestLogJournalSkipsAnEntryLargerThanItsBatchBudget` cover matches, positions,
  and read errors. `TestLogSilenceUsesTheTimeOfAnOldFileBacklog`,
  `TestLogSilenceReadsCurrentFileBeforeRotatedBacklog`, and
  `TestLogJournalSilenceUsesTheNewestEntryBeyondABacklog` cover silence.
- **The node suite** (`node_scenarios.py`), on
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution):
  a real log file with a matching line, a line that was written while MMWatchdog
  was stopped, and the last lines of a rotated log before the first lines
  of the new file; the real journal of a unit with a line that matches a
  regular expression, a line of 6000 characters, and a unit that falls
  silent and recovers with its next line; and a unit that systemd does not
  know.
- The latest run passed on every target distribution ([validation
record](../../docs/changing-mmwatchdog/validation.md)). Go tests
  only, not on a real node: a log that `logrotate` rotates with
  `copytruncate`; a log file
  with the saved inode but different content after MMWatchdog starts; and a
  `log_lines` check of a file on a filesystem
  of another machine that is gone.

## Why it works this way

- **A new event opens its incident at once.** The event happened even when
  the next run is healthy; a waiting time must not hide it. Lasting health
  closes the incident after `recovered_after_healthy_for`, counted from the
  first run with no new matching line. MMWatchdog counts observed health, so the
  incident can stay open for `recovered_after_healthy_for` plus up to one
  `check_every` after the last run that reports a matching event.
- **A timing-only reload keeps the module's state.** Changing when a check
  runs must not discard an event not yet read.
- **A match is reported even before a later read problem.** Advancing the position
without
  reporting the match would lose the alert.
- **MMWatchdog checks oversized lines' prefixes and advances the reading
  position past the remainder.** A matching prefix proves failure.
  An unmatched prefix cannot prove health, but one long line
  must not prevent every subsequent line from being checked.
- **MMWatchdog preserves the byte offset where a read failed.** An unread
  byte does not prove the log ended normally. The next run must retry
  reading from the saved byte offset where the previous read failed.
- **Silence follows the newest complete line's time.** An old backlog is
  evidence of past activity, not proof that a worker still writes.
- **The first run starts at the end of the log.** File lines or journal
  entries written before the check began watching cause no alert.
- **The old file of a rotated log is read to its end.** The last words of a
  program before it starts a new log are the lines that matter most.
- **The file is known again by its inode and its first bytes, not by its
  device.** A device can get another number when the machine starts.
- **A run keeps only the count and five matching lines.** A burst of lines then
costs MMWatchdog no memory.
- **The characters that change how text is shown are replaced.** A line of
  a log cannot change an operator's screen or the look of an alert.
