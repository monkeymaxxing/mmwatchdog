# `shell`

A `shell` check runs a Bash script on the node with `pipefail`.
With `pipefail`, a pipeline succeeds only when every command in the pipeline
succeeds. The check is healthy when the script ends with an exit code that
counts as success. A one-line command and a script of many lines run alike, and pipes, redirection, variables, and ordinary Bash syntax work.

No wrapper script, JSON, or module written by the team is needed:
`shell: test -f
/var/lib/myapp/ready` is a whole check.

A `shell` check passes or fails and reports no number.
A check of a team's own application may need a number with a limit,
application-specific inputs, or several findings. Such a check uses a
[module of the project's own](../../docs/reference/configuration-files.md#run-settings-of-a-shell-check-and-of-a-module-that-is-not-core).

## When the check fails, and when it cannot check

- **The check is healthy** when the script ends with an exit code in
  `success_exit_codes`, which is `[0]` without the input. An explicit list
  replaces the default.
- **The check fails** when the script ends by itself with any other exit
  code. The message is `command exited 1`, with the exit code.
- **MMWatchdog cannot check, and the run is unknown,** when Bash cannot find
  or run a command of the script.

  MMWatchdog identifies Bash's error by the script's exit code and the last
  line of the script's standard error. Exit code 127 means Bash cannot find
  the command or script. Examples are
  `bash: line 1: /usr/local/bin/check-queue: No such file or directory`
  and `bash: line 1: myapp-cli: command not found`.

  Exit code 126 means Bash found the command or script but cannot execute
  the command or script. For example, `...: Permission denied` can mean
  the script is not executable.

  The message of the run adds Bash's line:
  `command exited 127, which Bash uses when it
  cannot find a command: bash: line 1: myapp-cli: command not found`. An
  exit code in `success_exit_codes` stays healthy.
- **A program that ends with 127 or 126 by itself fails the check** unless
  the exit code is in `success_exit_codes`, like any other exit code.
  The dynamic loader ends a program whose shared library is missing with
  127 and writes `error while loading shared
  libraries`: the program cannot work, which is what the check is for.
- **MMWatchdog also reports an unknown run** when the script cannot start,
  runs past `time_limit_per_run`, or ends because of a signal.
  A script that writes more than 64 KiB to its standard output also makes
  the run unknown. The accepted exit codes cannot make a run healthy after
  a start error, timeout, signal, or standard output above 64 KiB.
- **The exit code, the standard output, and the standard error** are kept
  in the check's latest observation and in the evidence of the check's incident.
  The standard error keeps 16 KiB, followed by `[stderr truncated]` when
  the script wrote more ([built-in limits](../../docs/reference/configuration-files.md#built-in-limits)).

## Examples

### A one-line command

```yaml
# /etc/mmwatchdog/check_groups/myapp_scripts.yml
check_groups:
  myapp_selftest:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 1m
    shell: myapp-cli selftest | grep 'all tests passed' > /dev/null
    alert_severity: warn
```

Every minute, MMWatchdog on the node runs the command as the unprivileged account
`mmwatchdog`, in `/`. `grep` ends with exit code 0 when the self-test printed
`all tests passed`, and the check is healthy. When `myapp-cli` itself fails,
`pipefail` makes the whole pipe fail, even though `grep` ran. The example
uses `grep ... > /dev/null` and not `grep -q`: `grep -q` stops reading at
the first match, so a self-test that prints more after it can end with a
broken pipe, and `pipefail` would then fail a healthy run. When the check
has failed for 5 minutes, MMWatchdog sends the `warn` alert. The check is written
directly on the check group, so status shows it as `myapp_selftest/shell`.

### A script with its own account, directory, and variables

```yaml
# /etc/mmwatchdog/check_groups/myapp_scripts.yml
check_groups:
  queue:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 1m
    shell:
      script: /usr/local/bin/check-queue
      success_exit_codes: [0, 2]
      run_as_user_account: myapp
      run_from_directory: /srv/myapp
      env: {QUEUE: default}
    alert_severity: warn
```

MMWatchdog on the node runs `/usr/local/bin/check-queue` every minute, as the
account `myapp`, in the directory `/srv/myapp`, with the variable
`QUEUE=default` beside the standard ones. The script's exit codes 0 and 2
are both healthy, so a script that uses 2 for "the queue is busy, but fine"
does not alert. Any other exit code fails the check.

## How it works

- **MMWatchdog runs `/bin/bash --noprofile --norc -o pipefail -c SCRIPT`.** Bash
  runs noninteractively, without profile or rc files. MMWatchdog does not change
  the script; Bash interprets it when the check runs. `set -e` and `set -u`
  are not added: a script that wants them sets them itself.
- **The script runs in a separate process.** With `run_as_user_account`,
  MMWatchdog runs the script as the named account. Without
  `run_as_user_account`, the script runs as the unprivileged account
  `mmwatchdog`. With `run_from_directory`, the script starts in the named
  directory. Without `run_from_directory`, the script starts in `/`.

  The account `mmwatchdog` is shared: a script that runs as it can read
  what that account's other checks use, including the password of a
  `postgresql` or `mysql` check that logs in. Give a database check its own
  low-rights monitoring login.

  `run_as_user_account: root` gives the script all the access of root.
  The account is the only boundary, and MMWatchdog gives the script no
  sandbox. The settings `run_as_user_account`, `run_from_directory`, and
  `env` also serve a module that is not core
  ([check groups and checks](../../docs/reference/configuration-files.md#check-group-files),
  [required settings and defaults](../../docs/reference/configuration-files.md#node-settings)).
- **The script inherits no environment of the daemon.**
  MMWatchdog gives the script
  `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, the
  account's `HOME` and `USER`, `LANG=C.UTF-8`, and the variables in `env`.
  `BASH_ENV` and `ENV` cannot be set, so no file runs before the script.
- **The run ends at `time_limit_per_run`:** MMWatchdog ends the script and every
  program that it started. The one exception is a program that makes
  itself independent of the script, as a daemon does when it starts.
- **`mmwatchdog check-now CHECK_GROUP` runs the script once and shows** its exit
  code, its standard output, and its standard error
  ([operate](../../docs/reference/commands.md)).
- **A [recovery command](../../docs/how-to/restart-a-service-automatically.md#steps-to-run-a-command-instead) runs in the
  same way** as a shell check.

## Errors and what they mean

Validation errors name the check, such as `check_groups.queue.shell.success_exit_codes: ...`:

| Message | What the message means, and what to do |
| --- | --- |
| `script: cannot be empty` | Write the script, or the command alone as the value of `shell`. |
| `success_exit_codes: cannot be empty` | Leave the input out for `[0]`, or list the codes. |
| `success_exit_codes: expected distinct integers 0..255` | An exit code is a whole number from 0 to 255, and each is listed once. |
| `run_from_directory: must be absolute` | Write the directory's absolute path. |
| `env.BASH_ENV: unsupported environment variable` | In the displayed error, `BASH_ENV` is forbidden. MMWatchdog also refuses `ENV`, `MMWATCHDOG_BIN`, and `MMWATCHDOG_MODULE_HELPER`. For another variable, the error can mean an invalid variable name or a value containing a zero byte. |
| `the module shell reports no measurement, so its check takes no limit` | A shell check takes no `fail_when_measurement_above` or `fail_when_measurement_below`. |

`sudo mmwatchdog validate` on the node also checks that the account and
the directory exist ([validation](../../docs/reference/commands.md#mmwatchdog-validate)), with
messages such as `run_as_user_account: user: unknown user myapp` and
`run_from_directory: /srv/myapp is not a directory`.

Messages of a run:

| Message | What the message means, and what to do |
| --- | --- |
| `command exited 3` | The script ended with an exit code that is not in `success_exit_codes`: the check failed. The script's standard output and standard error are in the evidence. |
| `command exited 127, which Bash uses when it cannot find a command: bash: line 1: myapp-cli: command not found` | The script names a command or a file that is not on the node, or not in the script's `PATH`. MMWatchdog cannot check, and runs no recovery action. Install the program, or correct the name or the path. |
| `command exited 127` | A program of the script ended with 127 by itself, such as one whose shared library is missing. The program's standard error in the evidence says why. The check failed. |
| `command exited 126, which Bash uses when it cannot run a command: bash: line 1: /usr/local/bin/check-queue: Permission denied` | The script names a file that the check's account cannot run, often because the file is not executable. Make `/usr/local/bin/check-queue` executable (`chmod 0755`), or let the check's account read and run `/usr/local/bin/check-queue`. |
| `the run took longer than 5s (time_limit_per_run), so MMWatchdog ended it` | The script ran past `time_limit_per_run` (here 5 seconds) and MMWatchdog ended it. The check is `unknown`, and the evidence shows no exit code, because the script wrote none. Make the script faster, or give the check group a longer `time_limit_per_run`. |
| `check process was terminated by a signal` | A signal ended the script, for example the kernel's out-of-memory killer. |
| `check standard output exceeds 65536 bytes` | The script wrote more than 64 KiB to its standard output. Write less, for example by sending the output of a command that the check only tests to `/dev/null`. |
| `run_as_user_account "myapp": user: unknown user myapp` | The account does not exist on the node. |
| `running as the user account myapp requires root` | MMWatchdog runs without root, as with `mmwatchdog check-now --without-daemon` by a normal user, and cannot change to another account. |

## Tested on

- **Go tests:** `TestShellContract` in `check_test.go` checks a pipe that
  fails with `pipefail`, exit codes inside and outside
  `success_exit_codes`, and an unset variable without `set -u`.

  `TestShellCheckThatCannotFindOrRunACommandIsUnknown` in `check_test.go`
  checks unknown results for a missing script, a command not installed,
  and a script not executable. The test checks failure when a program
  ends with 127 for a missing shared library, or a script exits with
  127 or 126 itself. The test also checks that 127 listed in
  `success_exit_codes` is healthy.

  `TestTimeoutAndOutputLimits` in `check_test.go` checks unknown results
  for a script exceeding the time limit, endless output, and more than
  64 KiB of standard output.

  `TestRunCommandRunsAsMMWatchdogItselfOrAsACheck` in `primitives_test.go`
  checks the account, directory, and use of only the environment that
  MMWatchdog gives the script.

  `TestConfigStrict` and `TestValidateRuntimeChecksExecution` in
  `config_test.go` cover the validation of every input, `BASH_ENV` refused,
  and a missing account or directory.
- **The acceptance suite** (`acceptance.py`), on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution):
  `false | cat` fails with exit code 1, `exit 2` with `success_exit_codes:
  [0, 2]` is healthy and keeps its exit code, and `id -un` prints `mmwatchdog`.

  The acceptance, diagnostics, and one-cause scenarios use shell checks for
  failures, incidents, recovery commands, and alerts throughout, and the
  soak suite runs 24 shell checks that flip every 500 ms.
- The latest run passed on every target distribution
  ([validation record](../../docs/changing-mmwatchdog/validation.md)).

## Why it works this way

- **`shell` is Bash only.** One shell to learn, and Bash can call any
  interpreter.
- **A shell check passes or fails and reports no number.** A
  number with a limit is the job of a module. A second way to report a
  number would do one job twice.
- **The script runs as the unprivileged account `mmwatchdog` by default.** The
  account's home is apart from the daemon's state, which only root can
  read, so a check cannot read or change incidents, credentials, or the
  alert queue.
- **The script inherits no environment of the daemon.** The script's result
  then does not depend on the daemon's environment. No file runs before the
  script through `BASH_ENV`.
- **MMWatchdog does not change the script.** Configuration is data, with no
  expressions or templates, so Bash runs the script text from the YAML
  configuration without changes.
- **A command that Bash cannot find or run is unknown, not failed.** Such
  a script did not look at the service. Reporting the run as failed would
  blame a service that may be fine. If the check triggers a unit's restart,
  reporting the run as failed would restart that healthy unit.
  An unknown run never starts a recovery
  action.

  MMWatchdog goes by Bash's own message and not by the exit code alone,
  because a program uses 127 for other faults too. The dynamic loader ends
  a program whose shared library is missing with 127. A program with a
  missing shared library is broken, so the program's check fails.
