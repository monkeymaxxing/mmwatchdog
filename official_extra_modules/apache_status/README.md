# `apache_status`

`apache_status` reads the status page of an Apache web server and measures
how many of Apache's workers are busy. `apache_status` is an
[official extra module](../../docs/reference/modules/README.md): a
Starlark script that the MMWatchdog project writes. Every deploy with the
release's MMX workflows installs `apache_status` on every node. A check
names the module and the address of the page. MMWatchdog runs the module
once for each run of the check, on the node.

A **worker** is one process or thread of Apache that serves one request at a
time. When every worker is busy, Apache makes new requests wait. The status
page is the page that Apache's own `mod_status` serves. The module reads
the `mod_status` page in the form for programs, whose address ends with
`?auto`, and sends the request itself, through MMWatchdog.

Choose `apache_status` to see how close Apache is to its limit of workers.
To check that a site answers with the right status code, use the core module
[`http`](../../docs/reference/modules/http.md).

## Set up

- **Apache's `mod_status`,** with the status page allowed for the machine
  that MMWatchdog runs on. On Debian and Ubuntu, installing Apache enables
  `mod_status` and allows requests for the status page from the machine
  that runs Apache:

  ```apache
  <Location /server-status>
      SetHandler server-status
      Require local
  </Location>
  ```

- **Nothing else.** The module sends the request itself and needs no
  program on the node. The check runs as the
  unprivileged account `mmwatchdog`.

## Inputs

| Input | Meaning | When omitted |
| --- | --- | --- |
| `url` | The address of Apache's status page in the form for programs, which ends with `?auto`, such as `http://127.0.0.1/server-status?auto` | Required: `mmwatchdog validate` refuses the check |

## Measurements

| Measurement | Unit | Meaning |
| --- | --- | --- |
| `busy_workers` | workers | The workers that serve a request now |
| `idle_workers` | workers | The workers that wait for a request |
| `busy_workers_percent` | percent | The busy workers as a share of the most workers that Apache starts, 0 to 100. At 100, Apache makes new requests wait |

A check can set a limit on each measurement, with `fail_when_measurement_above` or
`fail_when_measurement_below` beside the module. MMWatchdog applies the limit
([checks that measure a number](../../docs/reference/configuration-files.md#checks-that-measure)).
The most workers that Apache can start is the length of the page's
`Scoreboard`, with one character for each worker that Apache can start.
Without a scoreboard, `apache_status` uses the sum of busy and idle workers
instead. When that number is 0, the run has no
`busy_workers_percent`. A check with a limit on `busy_workers_percent` then
becomes unknown. Unknown means MMWatchdog cannot determine whether the
check is healthy or failed.

## When the check fails, and when it cannot check

| What happens | The run | Message |
| --- | --- | --- |
| Apache answers with its status page | Healthy | `Apache answers; 3 of at most 150 workers are busy` |
| More workers are busy than the limit allows | Failed | `busy_workers_percent is 93.33 (fails above 90); the module reported: Apache answers; 140 of at most 150 workers are busy` |
| Apache does not answer | Failed | `Apache did not answer the request for its status page: ` and the reason |
| Apache answers with another status code than 200 | Failed | `Apache answered the request for its status page with HTTP 403; the page of mod_status answers with 200` |
| The answer is another page, such as the status page for people | Unknown | `the answer is not the status page of Apache in the form for programs: the address must be the one of mod_status and end with ?auto` |

A check with `apache_status` therefore also fails when Apache is stopped or
hangs ([how it works](#how-it-works)). With a limit, the message of a
healthy run starts with the reading and its limit, such as
`busy_workers_percent is 2 (fails above 90); the module reported: Apache
answers; 3 of at most 150 workers are busy`. MMWatchdog
sends the alert when the check has failed for `alert_after_failing_for`.
If the check stays unknown for `alert_after_failing_for`, MMWatchdog opens
a cannot-check incident. A cannot-check incident records that MMWatchdog
cannot determine the check's health
([the failing time](../../docs/how-to/decide-when-a-check-alerts.md)).

## Examples

### Apache answers and has free workers

```yaml
# /etc/mmwatchdog/check_groups/web_server.yml
check_groups:
  web_server:
    check_every: 30s
    time_limit_per_run: 10s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 2m
    checks:
      apache_answers_and_has_free_workers:
        apache_status:                                  # the module
          url: http://127.0.0.1/server-status?auto      # the status page, in the form for programs
        fail_when_measurement_above:
          busy_workers_percent: 90
        alert_severity: warn
```

Every 30 seconds MMWatchdog runs the module on the node, as the account
`mmwatchdog`. The module requests the status page from Apache on the node
that runs MMWatchdog. The check fails when Apache does not answer the
request. The check also fails when more than 90 percent of the workers
that Apache can start are busy. MMWatchdog sends the `warn` alert when the
check has failed for five minutes, so a short rush of requests sends
nothing.

### Record the workers and alert when Apache stops answering

```yaml
# /etc/mmwatchdog/check_groups/web_server.yml
check_groups:
  web_server:
    check_every: 30s
    time_limit_per_run: 10s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 2m
    checks:
      apache_workers_are_recorded:
        apache_status:
          url: http://127.0.0.1/server-status?auto
        alert_severity: info
```

Without a limit, the check keeps the measurements that each run produces in the
node's measurement history, to see how busy Apache gets over a week. Every
check with a module that is not core states `alert_severity`, because
the module can fail by itself. In the example, MMWatchdog sends an `info`
alert when Apache stops answering.

## How it works

Each run of the check is one run of the script `module.star`, as the account
`mmwatchdog`, under the check's `time_limit_per_run`:

1. The module sends an HTTP `GET` to `url` with `http_request`, a primitive:
   a function that MMWatchdog provides to the script. A request without an
   answer ends shortly before `time_limit_per_run`, and the run fails.
2. The page for programs has one line for each value, with the name, a
   colon, and the value. The module reads `BusyWorkers`, `IdleWorkers`, and
   the length of `Scoreboard`.

## Errors and what they mean

| Message | What the message means and what to do |
| --- | --- |
| `check_groups.web_server.checks.apache_answers_and_has_free_workers.apache_status.url: is required: ...` | `mmwatchdog validate`: the check leaves out `url` |
| `Apache did not answer the request for its status page: ...` | Failed. Nothing answered at `url`. The text after the colon says why, such as a refused connection, or `MMWatchdog ended the call shortly before the time limit of the run (time_limit_per_run)` for an Apache that accepts the connection and hangs. Start or restart Apache, or correct `url` |
| `Apache answered the request for its status page with HTTP 403; the page of mod_status answers with 200` | Failed. Apache does not allow the page for the node that runs MMWatchdog, or `url` names another page. Allow the page as [set up](#set-up) shows |
| `Apache answered the request for its status page with HTTP 404; the page of mod_status answers with 200` | Failed. `mod_status` is off, or `url` names another path. Turn on `mod_status`, or correct `url` |
| `the answer is not the status page of Apache in the form for programs: the address must be the one of mod_status and end with ?auto` | Unknown. The answer has no `BusyWorkers` and `IdleWorkers`, such as the page for people. Add `?auto` to the address of the `mod_status` page in `url` |
| `the result has no measurement named busy_workers_percent, which this check has a limit on` | Unknown. The page counted no worker at all |

The errors that every module that is not core can meet, such as a run past
`time_limit_per_run`, are in
[checks with a module that is not core](../../docs/reference/configuration-files.md#run-settings-of-a-shell-check-and-of-a-module-that-is-not-core).

## Tested on

- **On every target distribution, against Apache as the distribution installs it**
  (`tests/integration/service_scenarios.py`, in the services suite). The
  check reads the status page of the real Apache server and measures its
  busy and idle workers. The check is unknown for the page for people.
  The check fails for a missing page with HTTP 404.
  The check also fails when Apache is stopped and when Apache hangs,
  because the signal `SIGSTOP` paused Apache's processes.
  The scenario turns on each distribution's status page its own way; the
  module's code is the same on every distribution
  ([every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)).
- **Go tests** (`tests/official_extra_modules/check_modules_test.go`,
  `TestApacheStatus`) run the script through `mmwatchdog run-starlark-module`
  against a stand-in web server: the page for programs, the page for people,
  another status code, a server that is gone, and a server that accepts the
  connection and never answers. The `TestApacheStatus` tests ran with
  `go test ./...` on macOS on ARM64.

## Why it works this way

- **The module reads `mod_status`.** The `mod_status` page reports Apache's
  own worker counts. Installing Apache on Debian and Ubuntu enables
  `mod_status`.
- **The module sends the request itself, with MMWatchdog's primitive
  `http_request`.** The node needs no program to send the request.
- **The page for people makes the check unknown, never healthy.** The
  module cannot read the workers from Apache's status page for people,
  and a module that cannot tell says so.
- **The share of busy workers counts against the scoreboard.** The
  scoreboard has one place for each worker that Apache can start, so
  100 percent is the point where new requests wait.
- **`apache_status` is an official extra module, not a core module.**
  Monitoring Apache's status page is uncommon work. A new release can update
  the script for Apache's status page without a new `mmwatchdog` program.
