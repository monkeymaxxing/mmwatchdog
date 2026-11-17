# When something fails

**Who acts:** you, or your coding agent, on the node, logged in with an
account that can use `sudo`. Some fixes run `mmx` on your controller
machine: the machine where you run `mmx`, such as your laptop.

**When to use this page:** an alert did not arrive, a check or a
destination looks wrong, an `mmx run` of MMWatchdog failed, or a message
of `mmwatchdog` is not clear to you. Search this page for a few words of
the message, such as `time_limit_per_run` or `Storage:`.

**What this page does:** for each symptom, the page says what it means,
how to confirm it with a command, what to do, and which page explains the
topic.

**Terms:**

- A **node** is a server that runs MMWatchdog. A **check group**, such as
  `myapp`, holds **checks**, such as `myapp/health_page_answers`.
- A **run** is one execution of a check. Its result is `ok`, `failed`, or
  `unknown`. `unknown` means that MMWatchdog could not find out whether
  the check passes.
- An **incident** opens when a check has failed for its check group's
  `alert_after_failing_for`, and the incident's alert goes out then.
- A **destination** is a place that gets alerts, such as a Slack channel.
- In a **fleet**, the nodes report to one node with the hub role, the
  **hub**. The hub holds the destinations and delivers the alerts of every
  node.

The examples use the check group `myapp`, the joined node `web02`, and the
hub at `203.0.113.10`.

Find the part of this page for what you see:

| What you see | Go to |
|---|---|
| A check failed, but no alert arrived | [No alert arrived](#no-alert-arrived) |
| `status` has a line `Destination NAME: ... not delivered`, or the alert `Destination NAME is failing.` | [A destination is failing](#a-destination-is-failing) |
| A check is `unknown`, or an alert says `MMWatchdog cannot check` | [A check is unknown](#a-check-is-unknown) |
| A message names `time_limit_per_run`, or says that a run did not finish | [A check shows its time limit](#a-check-shows-its-time-limit) |
| A check failed, and the service was not restarted | [The restart did not happen](#the-restart-did-not-happen) |
| `status` or `fleet` has a line that starts with `Storage:`, or MMWatchdog does not start because `state.db` uses another format | [A reading or the node's state cannot be saved](#a-reading-or-the-nodes-state-cannot-be-saved) |
| The hub lists a node as `silent`, or an alert says `stopped reporting to the MMWatchdog fleet hub` | [The hub shows a node silent](#the-hub-shows-a-node-silent) |
| An alert says `cannot reach the MMWatchdog fleet hub` | [The hub is down](#the-hub-is-down) |
| An alert arrived late, twice, or without a reason that you know | [Alerts arrive late, twice, or unexpected](#alerts-arrive-late-twice-or-unexpected) |
| `mmx run` failed at a node task of MMWatchdog | [`mmx run` fails on an MMWatchdog step](#mmx-run-fails-on-an-mmwatchdog-step) |
| `mmwatchdog validate` or `mmwatchdog reload` refuses the configuration | [Validation refuses the configuration](#validation-refuses-the-configuration) |
| An `mmwatchdog` command ends with `dial unix` | [The `mmwatchdog` command cannot reach MMWatchdog](#the-mmwatchdog-command-cannot-reach-mmwatchdog) |

## What `mmwatchdog status` tells you first

Most symptoms start with `sudo mmwatchdog status` on the node. The exit
code of `status` sums up the node, so check it first:

| Exit code | Meaning |
|---|---|
| 0 | Every check is healthy, or is failing but has not alerted yet. |
| 1 | At least one check has an open failure incident: its alert has gone out. |
| 3 | No failure incident is open, but something needs attention: a check is `unknown`, a destination is failing, or MMWatchdog cannot save something. A failure incident takes precedence, so the exit code is 1 when both are true. |
| 4 | The command could not run, such as when MMWatchdog is not running. |

`sudo mmwatchdog events myapp` shows what happened and when.
`sudo mmwatchdog check-now myapp` runs the checks of `myapp` once more and
shows the result, without alerts or restarts.
[See what is happening](how-to/see-what-is-happening.md) explains the
three commands.

## No alert arrived

An alert goes out only when a check has failed for its
`alert_after_failing_for`, and only to the destinations that take the
alert's severity. Find the reason with `sudo mmwatchdog status myapp` on
the node:

| What `status` shows | What it means and what to do |
|---|---|
| The check's state is `confirming_failure` | The check fails, but not yet for `alert_after_failing_for`, which the `Rule:` line shows. The alert goes out when that time has passed. To alert sooner, see [Decide when a check alerts](how-to/decide-when-a-check-alerts.md). |
| A line `Maintenance on: myapp until ...` | A maintenance window holds the alerts of the check group. The alerts come back when the window ends. See [Pause alerts during planned work](how-to/pause-alerts-during-planned-work.md). |
| A line such as `myapp/health_page_answers is failing; no alert and no recovery action while postgres/postgres_service_is_running fails` | MMWatchdog keeps the check quiet, because the check that the line names fails, and that check sent the alert. See [Stop alerts that follow another failure](how-to/stop-alerts-that-follow-another-failure.md). |
| A line `Destination NAME: ... not delivered` | MMWatchdog could not deliver to that destination, and keeps trying. See [A destination is failing](#a-destination-is-failing). |
| `Alert deliveries:` counts alerts as `dropped` | An alert waited longer than `keep_undelivered_alerts_for`, or did not fit into `undelivered_alerts_content_size_limit`, and MMWatchdog gave up. See [When a destination cannot be reached](how-to/send-alerts-somewhere.md#when-a-destination-cannot-be-reached). |
| `This node sends alerts to nobody.` | The node has no destination. See [Send alerts where your team sees them](how-to/send-alerts-somewhere.md). |
| The `Fleet hub:` line of a joined node ends with `no settings from the hub yet, so alerts wait for the hub` | The node has not yet received the hub's destinations, so the node cannot send alerts itself. The alerts wait until the hub accepts them. See [Set up a fleet](how-to/set-up-a-fleet.md#if-something-is-different) for a node that cannot reach the hub. |
| A line `The MMWatchdog fleet hub has not accepted 2 alerts. ...` on a joined node | The node cannot hand its alerts to the hub. The node sends them to the destinations itself after `send_alert_without_hub_after_waiting_for`, such as 30 seconds with `send_alert_without_hub_after_waiting_for: 30s`. See [The hub is down](#the-hub-is-down). |

This is how a check looks while it fails but has not alerted yet. The
check runs `false`, which always fails, with `alert_after_failing_for: 1h`:

```text
myapp/health_page_answers confirming_failure command exited 1
  Config: /etc/mmwatchdog/check_groups/myapp.yml:8:7
  Rule: check every 10s, time limit per run 2s, alert after failing for 1h, recovered after healthy for 30s
  Alert severity: sos
  Observed: 2026-11-15T10:30:25.457391537Z
  Exit code: 1
```

The `Rule:` line says that the alert waits for one hour of failing. The
`Config:` line names the file and the line of the check.

When `status` shows none of these lines, check the destinations:

- **No destination takes the alert's severity.** `sudo mmwatchdog
  validate` warns about it, such as `machine/memory_is_below_90_percent: no
  destination receives alerts with alert_severity warn; ...`. Add the
  severity to a destination's `alert_severities`, as
  [Send alerts where your team sees them](how-to/send-alerts-somewhere.md#if-something-is-different)
  says. In a fleet, the destinations are on the hub, so run `validate` on
  the hub.
- **The destination accepted the alert, but nobody saw it.**
  `journalctl -u mmwatchdog` on the node that delivers, the hub in a fleet,
  has one line for each delivery, such as `delivered the alert "..." to
  alert destination team_slack_channel, which accepted it`. Then the alert
  left MMWatchdog. Look in the destination's service, such as the Slack
  channel's settings, or the mail server's spam folder.

## A destination is failing

MMWatchdog keeps each alert that a destination did not take, and tries
again: after 1 second, then after twice as long each time, up to every 256
seconds. Each destination has its own queue, so a failing destination
delays no other. `sudo mmwatchdog status` shows the queue at once, with
the latest error:

```text
Alert deliveries: 1 waiting, 0 dropped
Destination on_call_receiver: 1 alert is not delivered. MMWatchdog tries again at 04:33 UTC. The latest attempt failed: MMWatchdog could not post the alert to the webhook: dial tcp 127.0.0.1:18081: connect: connection refused
```

- **What it means:** the latest error comes from the destination's
  module, such as a refused connection, `webhook returned HTTP 401` for a
  wrong token, or `Slack answered 404: no_service` for a Slack webhook
  that was removed.
- **What happens next:** when an alert has waited for 10 minutes, every
  other destination of the node gets the `warn` notice `Destination
  on_call_receiver is failing.` When the destination takes alerts again,
  they get `Destination on_call_receiver works again.` MMWatchdog also
  verifies every destination once a day without sending a message. A
  verification that keeps failing for 10 minutes sends the same notice.
  A node with no other destination sends the notice to nobody: only
  `sudo mmwatchdog events` shows it.
- **What to do:** fix the cause that the error names, such as the
  address or the token in the vault. The next attempt then delivers every
  waiting alert.
  [When a destination cannot be reached](how-to/send-alerts-somewhere.md#when-a-destination-cannot-be-reached)
  shows the whole case, and the
  [module pages](reference/modules/README.md) list each module's errors.

`sudo mmwatchdog status` exits with code 3 while a destination fails, or
with 1 when a check has also failed; the output names the failing
destination in both cases. In
`sudo mmwatchdog status --json`, `delivery.failing_destinations` lists
each failing destination with its error and its next attempt.

## A check is unknown

A run is `unknown` when MMWatchdog could not find out whether the service
works, such as when the check's own command is missing. An `unknown`
check never calls for a restart. When a check stays `unknown` for
`alert_after_failing_for`, MMWatchdog sends an alert whose title says
`MMWatchdog cannot check myapp/queue_tool_runs`.

To see why, run the check once more on the node. `check-now` shows the
standard output, the standard error, and the exit code of a `shell` check
or of a module:

```sh
sudo mmwatchdog check-now myapp
```

```text
...
myapp/queue_tool_runs unknown  command exited 127, which Bash uses when it cannot find a command: /bin/bash: line 1: myapp-cli: command not found
  Observed: 2026-11-15T10:33:39.3817403Z
  Exit code: 127
  stderr: "/bin/bash: line 1: myapp-cli: command not found\n"
mmwatchdog: one or more checks need attention
```

The message gives the cause. Common causes:

| Message | What to do |
|---|---|
| `command exited 127, which Bash uses when it cannot find a command: ...` | The `shell` check runs a command that the node does not have, or that is not on the check's `PATH`. Install the command, or write its full path. |
| `command exited 126, which Bash uses when it cannot run a command: ...` | The command exists but cannot run, such as a script without the executable bit. Run `chmod +x` on the script. |
| `the run took longer than 5s (time_limit_per_run), so MMWatchdog ended it` | See [A check shows its time limit](#a-check-shows-its-time-limit). |
| A message from a module, such as a refused login to a database | The module's page lists its messages and what each means: see the [module pages](reference/modules/README.md). |

A check of a module that is not core, or a `shell` check, runs as the
account `mmwatchdog` by default. A command that works for you with `sudo`
can fail for that account. `run_as_user_account` beside the check's module
changes the account, as
[Add a check](how-to/add-a-check.md#if-something-is-different) says.

## A check shows its time limit

Each run may take as long as its check group's `time_limit_per_run`, 5
seconds in the starter settings. MMWatchdog ends a run that takes longer.
What the run's result then is depends on the kind of check. In this
example, the check group `myapp` has `time_limit_per_run: 2s`, and
`check-now` runs its checks:

```sh
sudo mmwatchdog check-now myapp
```

```text
myapp/database_port_opens failed   the connection to 203.0.113.250:5432 did not open within 2s (time_limit_per_run)
  Observed: 2026-11-15T10:33:35.378938293Z
myapp/health_page_answers failed   Get "http://127.0.0.1:18099/health": no answer within 2s (time_limit_per_run)
  Observed: 2026-11-15T10:33:37.37936286Z
myapp/queue_script_succeeds unknown  the run took longer than 2s (time_limit_per_run), so MMWatchdog ended it
  Observed: 2026-11-15T10:33:39.380445572Z
...
```

- **A check that asks a server over the network,** such as `http` or
  `tcp`, has `failed`: a server that does not answer in time counts as
  down.
- **A check that runs a program,** such as `shell`, is `unknown`:
  MMWatchdog ended the program, so the program never said whether the
  service works. The run shows no exit code.

What to do:

- **The service is slow, but works.** Raise `time_limit_per_run` in the
  check group, such as to `time_limit_per_run: 15s`.
  [Decide when a check alerts](how-to/decide-when-a-check-alerts.md)
  explains the timings of a check group.
- **The service hangs.** Fix the service. For a server that does not
  answer, the check's alert is the right result.
- **The check itself waits for something,** such as a script that waits
  for input. Fix the script, then try it with `sudo mmwatchdog check-now
  myapp`.

Some runs cannot be ended at all, such as a run that reads a file on a
network filesystem whose server is gone: the kernel holds the read
without end. MMWatchdog waits 5 seconds after the time limit, then stops
waiting for that run:

- The run's result is `unknown`, with a message that names the check
  group's `time_limit_per_run`, such as `the check run did not finish
  within the time limit of 5s. MMWatchdog stopped waiting for the run to
  finish`.
- The run no longer counts against the node's `max_concurrent_checks`, so
  the other checks of the node go on.
- Until that run ends, MMWatchdog starts no new run of the same check.
  Each due run is `unknown` at once, with `the check run that started at
  14:02 UTC has not finished. MMWatchdog will start no new run of the check
  until the earlier run finishes`. So a check that cannot end keeps only
  one run waiting.

When the stuck run ends, MMWatchdog drops its result, and the next run is an ordinary
run. A reload that changes the check starts the check over.

## The restart did not happen

MMWatchdog restarts a unit only when every condition of
`restart_systemd_unit` holds. `sudo mmwatchdog status myapp` shows the
attempts, such as:

```text
Systemd unit restarts myapp.service: 2 of 2 attempts used
```

| What you find | What it means and what to do |
|---|---|
| Every attempt is used, as above | The restarts did not help. MMWatchdog keeps running the checks and alerting. Fix the cause, then give the attempts back with `sudo mmwatchdog reset-recovery-attempts myapp`. |
| `sudo mmwatchdog events myapp` shows `systemd_unit_restart_withheld ... MMWatchdog never starts a stopped unit` | Someone or something stopped the unit on purpose, so MMWatchdog leaves the unit stopped. Start the unit with `sudo systemctl start myapp.service`. |
| The check is `unknown` | An `unknown` check never calls for a restart. See [A check is unknown](#a-check-is-unknown). |
| A maintenance window covers the check group, or the check is kept quiet | Neither alerts nor restarts happen then. See [No alert arrived](#no-alert-arrived). |
| `systemctl status myapp.service` says that systemd refuses to start the unit | systemd's start limit stops the unit, and MMWatchdog never clears that limit. Fix the cause that `systemctl status` names. |

[Restart a service automatically](how-to/restart-a-service-automatically.md#if-something-is-different)
gives every condition of a restart, and what a recovery command does when
it fails.

## A reading or the node's state cannot be saved

MMWatchdog keeps its state, its alerts that wait for delivery, and the
readings of its measurements in files under `/var/lib/mmwatchdog/`. A file
that MMWatchdog cannot write, such as on a full disk, is a storage problem.
`sudo mmwatchdog status` then has a line that starts with `Storage:` and
exits with code 3, or with 1 when a failure incident is open.

**A reading that cannot be stored** changes nothing for the check: its
state, its alerts, and its restarts do not depend on the measurement
history. The `Storage:` line names `measurement history write`, with the
error. The line goes away when a reading is stored again, such as after
you free space on the disk.

**A measurement history file that MMWatchdog cannot open,** such as a
damaged file, leaves the node without readings. MMWatchdog still starts,
runs the checks, and sends alerts. The `Storage:` line says what to do:

```text
Storage: measurement history write: cannot save readings in measurement history file /var/lib/mmwatchdog/measurement_history.db: file is not a database (26); move the measurement history file aside and restart MMWatchdog to create an empty measurement history
```

On the node, move the file aside and restart MMWatchdog, which creates an empty history:

```sh
sudo mv /var/lib/mmwatchdog/measurement_history.db /var/lib/mmwatchdog/measurement_history.db.damaged
sudo systemctl restart mmwatchdog
```

On a hub, the same can happen to
`/var/lib/mmwatchdog/measurement_history_of_nodes.db`, which holds the
readings that the nodes send. The hub still takes reports and alerts, and
`sudo mmwatchdog fleet` shows the `Storage:` line and exits with code 3,
or 1 when a check of a node failed. Move that file aside in the same way and restart
MMWatchdog on the hub. Each node still keeps its own readings.

**MMWatchdog does not start, and the journal says that `state.db` uses
another format,** such as `/var/lib/mmwatchdog/state.db uses state format
7; the running MMWatchdog build uses format 8. Move the database file
aside to start with empty state`. The file was written by another release,
which stores its state in another way. Go back to that release, or move
the file aside and start MMWatchdog again. Empty state loses the open
incidents, the alerts that wait for delivery, and the event history.

**The state cannot be saved,** such as on a full disk. MMWatchdog then
sends alerts straight from memory, so the alerts still go out. When the
disk has room again, MMWatchdog saves the same alerts and sends them a
second time. A duplicate alert is better than a lost one. Free space on
the disk. [Installed files](reference/installed-files.md#the-state) says
which files MMWatchdog keeps.

## The hub shows a node silent

A joined node sends a report to the hub at each `report_every`, such as
every 30 seconds with `report_every: 30s`. When three reports in a row are
missing, the hub alerts, such as `Warning: web02
stopped reporting to the MMWatchdog fleet hub`. `sudo mmwatchdog fleet`
on the hub lists the node as `silent`, with the time of its last report:

```text
web02                    silent     exit 1  last report 2026-11-15T08:36:47Z (2m25s ago)
```

The node, its MMWatchdog, or the network between the node and the hub
stopped. Log in to the node: `systemctl is-active mmwatchdog` says whether
MMWatchdog runs, and the last line of `sudo mmwatchdog status` gives the
latest error of the reports. [When a node goes silent](how-to/watch-a-fleet.md#when-a-node-goes-silent)
walks through the case, and how to plan a reboot without the alert.

## The hub is down

The nodes keep running their checks and restarts without the hub. Each
node sends its own alerts to the destinations, from its copy of the hub's
settings, when the hub has not taken them for
`send_alert_without_hub_after_waiting_for`. Two joined nodes, the
**hub-outage alerter nodes**, alert you about the hub when they have not
reached the hub for `alert_after_hub_unreachable_for`, such as 3 minutes
with `alert_after_hub_unreachable_for: 3m`. In a fleet of two nodes, the
only joined node is the hub-outage alerter node. The alert reads:

```text
SOS: web01 cannot reach the MMWatchdog fleet hub
no report from this node has reached the MMWatchdog fleet hub since 08:28 UTC; mmwatchdog status on this node shows the latest error
failing since 08:31 UTC
```

Each hub-outage alerter node sends its own alert, so you may get two. The
network error is left out of the alert, because the alert may show on a
phone's lock screen. The last line of `sudo mmwatchdog status` on
`web01` gives the error:

```text
Fleet hub: 203.0.113.10:8445 as web01; last report 2026-11-15T08:28:20Z; error at 2026-11-15T08:32:20Z: Post "https://203.0.113.10:8445/v1/report": dial tcp 203.0.113.10:8445: connect: connection refused
```

`connection refused` means that the hub's server answers, but MMWatchdog
does not run on the hub, or a firewall rejects the port. Log in to the
hub and run `systemctl status mmwatchdog`. A message such as `i/o timeout` means that the hub's server
or the network does not answer. When the hub runs again, the nodes send
the events that it missed. [When the hub stops](how-to/watch-a-fleet.md#when-the-hub-stops)
gives the whole case.

When the hub and both hub-outage alerter nodes fail together, nothing
alerts until one of them runs again. An `mmhotline` destination on the hub
covers that case, as
[Watch a fleet](how-to/watch-a-fleet.md#if-something-is-different) says.

## Alerts arrive late, twice, or unexpected

MMWatchdog prefers a duplicate alert to a lost one, so a few cases send an
alert twice.

- **An alert arrives late.** The destination did not take the alert at
  first, and MMWatchdog kept trying, as in
  [A destination is failing](#a-destination-is-failing). In a fleet, an
  alert that the hub delivers more than 2 minutes after the event ends with
  the present state, such as `(alert delivered late; the event happened at
  14:02 UTC. Now web02 is reporting to the hub)`.
- **The same alert arrives twice during a restart.** An alert that is on
  its way to a destination when MMWatchdog restarts can arrive twice, if
  the destination takes longer than 5 seconds to answer. MMWatchdog cuts
  that delivery off after 5 seconds and sends the alert again after the
  restart, so no alert is lost. A deploy with `mmx run` restarts
  MMWatchdog, so a slow destination may get the deploy's `maintenance on`
  message twice.
- **The same alert arrives twice in a fleet.** When the hub saved an
  alert but its answer to the node was lost, the node also sends the alert
  directly. Both carry the same event ID.
- **Two alerts about the hub.** Each of the two hub-outage alerter nodes
  sends one. See [The hub is down](#the-hub-is-down).
- **The same failure again and again.** A destination with
  `remind_every`, such as `remind_every: 4h`, gets a reminder while the
  incident stays open, such as `myapp/http still failing since 14:02 UTC;
  latest result: expected HTTP 200, received 503`. Set `remind_every:
  never` to stop the reminders. See
  [Send alerts where your team sees them](how-to/send-alerts-somewhere.md).
- **`maintenance on` and `maintenance off` at each deploy.** A run of
  `mmx run hub_and_standalone` that changes something opens a whole-node
  window for its restart, and the destinations get the two `info`
  messages. See [Deploys through MMX](how-to/pause-alerts-during-planned-work.md#deploys-through-mmx).
- **A recovery without its failure.** A destination that was down may get
  the recovery of a failure whose alert it never got. Match the two by the
  incident in the alert.

## `mmx run` fails on an MMWatchdog step

MMX names the node task that failed, and the line after it gives the
reason. Most failures come before MMX changes anything on the node, so
the node keeps its configuration and its running MMWatchdog.

| The failed node task | What it means and what to do |
|---|---|
| `validate_config` | The new configuration is not valid. The output names the file, the line, and the fault: see [Validation refuses the configuration](#validation-refuses-the-configuration). The path in the output, such as `/usr/local/lib/mmwatchdog/candidate_config/check_groups/myapp.yml`, is the copy that the run checks on the node, and the line and column are those of your file in `files/`. |
| `validate_config`, with `the official extra modules in ... were shipped with MMWatchdog 0.1.0, and this mmwatchdog program is MMWatchdog 0.1.1` | The project holds the program of one release and the modules of another. Use one release's whole project: see [Update MMWatchdog](how-to/update-mmwatchdog.md). |
| `write_secret_files`, with `value is unavailable at /secrets/mmwatchdog_secret_files/NAME` | The inventory lists a secret file that the vault does not hold. Add it with `mmx secrets set mmwatchdog_secret_files NAME`, or fix the name. See [Install MMWatchdog on one node](how-to/install-on-one-node.md#if-something-is-different). |
| Before any node task: `mmx: vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)` | A coding agent ran `mmx` without the passphrase file. The person runs `mmx secrets write-passphrase-file` once, in their own terminal. The agent then runs the command again with the line that command prints, such as `MMX_VAULT_PASSWORD_FILE=~/.config/mmx/vault-passphrase-PROJECT`. |
| `install_monitoring_on_a_joined_node`, with `duplicate service configuration path /etc/mmwatchdog/alert_destinations.yml` | `files/join_hub/` holds an `alert_destinations.yml`. Delete it: in a fleet, the destinations belong on the hub. See [Set up a fleet](how-to/set-up-a-fleet.md#if-something-is-different). |
| `wait_for_inspection_socket` or `verify_daemon` | MMWatchdog did not start within 10 seconds after the restart. `journalctl -u mmwatchdog -n 50` on the node says why. The run's maintenance window ends by itself within 15 minutes. |

A dryrun does not check your new configuration: it checks the copy that
the last run left on the node. Only `mmx run` checks the new files.

## Validation refuses the configuration

`sudo mmwatchdog validate` on the node checks the whole configuration in
`/etc/mmwatchdog/`, and `mmx run` runs the same check before it installs
anything. A fault prints the file, the line, the column, the setting, and
what is wrong, and the command exits with code 1:

```sh
sudo mmwatchdog validate
```

```text
mmwatchdog: /etc/mmwatchdog/check_groups/myapp.yml:5:5: check_groups.myapp.alert_after_failing_for: expected a duration such as 30s, 5m, 26h, or 7d, found the string "1 minute"
```

Fix the line that the message names, here to `alert_after_failing_for:
1m`, and run `validate` again. A valid configuration prints a line such as
`Valid configuration: 2 check groups; alert destinations come from the
hub`, and exits with code 0.

`sudo mmwatchdog reload` uses a new configuration only when all of it is
valid. A configuration that is not valid changes nothing: MMWatchdog keeps
the configuration that it has, and `reload` prints the fault.

A **validation warning** is not a fault: the configuration is valid, but
it may not do what you expect, such as a severity that no destination
takes. `validate` and `reload` print the warnings below their result.

The page of each job lists the faults of its settings, under "If something
is different". The [configuration files reference](reference/configuration-files.md)
lists every setting, and `sudo mmwatchdog describe-module NAME` lists the
inputs of a module.

## The `mmwatchdog` command cannot reach MMWatchdog

The commands that read or change the running MMWatchdog, such as
`status`, `events`, `check-now`, `reload`, and `maintenance`, reach it
through the socket `/run/mmwatchdog/mmwatchdog.sock`. When a command
cannot reach the socket, the command exits with code 4:

| Message | What it means and what to do |
|---|---|
| `mmwatchdog: Get "http://localhost/status": dial unix /run/mmwatchdog/mmwatchdog.sock: connect: permission denied` | You ran the command without `sudo`. Run `sudo mmwatchdog status`. |
| `mmwatchdog: Get "http://localhost/status": dial unix /run/mmwatchdog/mmwatchdog.sock: connect: no such file or directory` | MMWatchdog does not run on the node. `systemctl status mmwatchdog` and `journalctl -u mmwatchdog -n 50` say why. Start MMWatchdog with `sudo systemctl start mmwatchdog`. |

## Where to go next

- [See what is happening](how-to/see-what-is-happening.md): `status`,
  `events`, and `check-now` on a node.
- [Watch a fleet](how-to/watch-a-fleet.md): the fleet, a silent node, and
  a hub that stopped.
- [Send alerts where your team sees them](how-to/send-alerts-somewhere.md):
  destinations, and a destination that cannot be reached.
- [Commands reference](reference/commands.md): every command, its output
  as JSON, and its exit codes.
