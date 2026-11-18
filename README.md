# MMWatchdog

**Agent-first server monitoring, maxxed for monkey/human understanding.**

MMWatchdog watches your Linux servers and alerts you when something is
wrong. You list what to watch on each server, such as a service, a web
page, a disk, or a database, in plain YAML files. MMWatchdog runs those
checks, can restart a service that fails, and sends each alert to the places
you name, such as email or [Slack](https://slack.com/).

Your coding agent, such as [Claude Code](https://claude.com/claude-code) or [Codex](https://openai.com/codex/), can write the YAML files
and run the commands, or you can do both yourself. You don't need to have
used a monitoring tool before. If you have used
[Monit](https://mmonit.com/monit/), much of MMWatchdog will be familiar, and
[If you've used Monit](#if-youve-used-monit) lists the differences.

**To start,** paste the prompt in
[Set it up with your coding agent](#set-it-up-with-your-coding-agent) into
your agent, or follow [Install](#install) and [First run](#first-run)
yourself.

MMWatchdog runs on each server that it watches. Such a server is called a
**node**. One `mmwatchdog` process on the node runs the node's checks,
restarts services, and sends the node's alerts, so a node needs no other
machine.

If you have several servers, one node can also be the **fleet hub**. The
other nodes report to the fleet hub, and `mmwatchdog fleet` on the fleet hub
shows every node in one place.

You install MMWatchdog on your nodes with
[MMX](https://github.com/monkeymaxxing/mmx), the Monkeymaxxing tool that
sets up servers. You run MMX on your **controller machine**: usually your
laptop.

A node must run [systemd](https://systemd.io/), the service manager of most current Linux
distributions. MMWatchdog is tested on Debian 12 and 13, Ubuntu 24.04 and
26.04, Rocky Linux 9, AlmaLinux 10, Fedora 45, Arch Linux, and openSUSE Leap
16.1. [Limits on each distribution](docs/reference/limits-on-each-distribution.md)
lists what is limited on each one, and why.

The [examples](examples/) are working configurations, and the
[Monkeymaxxing Stack](https://github.com/monkeymaxxing/monkeymaxxing-stack)
is an example of MMWatchdog watching a whole system.

## The Monkeymaxxing tools

MMWatchdog is one of the free, open-source Monkeymaxxing tools for running
simple server systems.

| Tool | What it does |
|---|---|
| [MMX](https://github.com/monkeymaxxing/mmx) | Sets up your servers, and makes every later change to them |
| **MMWatchdog** (this tool) | Watches your servers, restarts what fails, and alerts you |
| [MMHotline](https://github.com/monkeymaxxing/mmhotline) | Gets each urgent alert to the person on call, and can let your coding agent investigate first |
| [Monkeymaxxing Stack](https://github.com/monkeymaxxing/monkeymaxxing-stack) | Instructions for your coding agent that build and run a whole production system with the three tools above |

You install MMWatchdog with MMX. You do not need MMHotline or the
Monkeymaxxing Stack to use MMWatchdog.

## Why MMWatchdog

### Why a monitoring tool instead of a cron script

The first monitoring on many servers is a cron job: a script that requests
a page of the application every minute, and sends an email when the request
fails. A coding agent will often write such a script too, because the script
is quick to write:

```sh
#!/bin/sh
# cron runs this script every minute
if ! curl --fail --silent --max-time 5 http://127.0.0.1:8080/health > /dev/null
then
  echo "myapp does not answer on $(hostname)" | mail -s "myapp is down" team@example.com
fi
```

The script does not remember what it found one minute ago. So the script
cannot wait to see whether a failure lasts, cannot tell a new problem from
a problem that it already reported, and cannot tell you when the problem is
over. While `myapp` is down, the script sends 60 emails an hour. One slow
answer at night sends an email too. And the script does nothing to fix
`myapp`.

MMWatchdog is a monitoring tool. It keeps watching, remembers what it saw,
and acts on what you told it:

- **Watch.** MMWatchdog runs each check as often as you set. Is the service
  running? Does the page answer? Is the disk filling up? Is a security
  update waiting?
- **Confirm.** A check must fail for as long as you set before MMWatchdog
  alerts, so one slow answer sends no alert. One problem gives one alert, a
  reminder while the problem lasts, and a message when the problem is over.
- **Repair.** MMWatchdog can restart the service, or run a command of
  yours, up to a number of attempts that you set.
- **Alert.** Alerts go to email, Slack, [Telegram](https://telegram.org/), a webhook, and more. When
  a destination cannot be reached, MMWatchdog keeps the alert and tries
  again.
- **Look back.** MMWatchdog records what happened and when. It also keeps
  the measurements of each check, such as how full a disk is, so you can
  see what led to a failure.

In MMWatchdog, you write the same job as a **check group**: a file that
lists the checks of one application.

```yaml
# /etc/mmwatchdog/check_groups/myapp.yml
check_groups:
  myapp:
    check_every: 30s                    # run each check every 30 seconds
    time_limit_per_run: 5s              # one run of a check may take 5 seconds
    alert_after_failing_for: 1m         # alert after 1 minute of failing
    recovered_after_healthy_for: 30s    # the problem is over after 30 healthy seconds

    checks:
      service_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active
        alert_severity: sos

      health_page_answers:
        http:
          url: http://127.0.0.1:8080/health
          expected_http_status_codes: [200]
        alert_severity: sos

    restart_systemd_unit:               # restart myapp when the health page fails
      systemd_unit: myapp.service
      triggered_by_checks: [health_page_answers]
      restart_after_failing_for: 1m
      min_time_between_attempts: 1m
      max_attempts: 3
      reset_attempts_after_healthy_for: 10m
```

Each entry in `checks` is a **check**: one thing that MMWatchdog watches.
`systemd_unit_state` and `http` are **modules**: each module knows how to
do one kind of check. `alert_severity` says how serious a failure of the
check is. The three severities are `info`, `warn`, and `sos`, and each alert
destination names the severities that it takes.

On the node, `sudo mmwatchdog status` shows every check and what its latest
run found. Part of its output:

```text
myapp/health_page_answers    ok           HTTP 200 and configured content match
myapp/service_is_running     ok           systemd unit myapp.service is active
```

Now suppose `myapp` hangs: the service still runs, but the health page no
longer answers. Within a minute, the status line of the check changes:

```text
myapp/health_page_answers    confirming_failure Get "http://127.0.0.1:8080/health": no answer within 5s (time_limit_per_run)
```

`confirming_failure` says that MMWatchdog has seen the failure and is
waiting, because the failure has not lasted one minute yet. After one
minute, MMWatchdog restarts `myapp.service` and sends the alert. The message
in Slack reads:

```text
SOS: app01: myapp/health_page_answers failed
Get "http://127.0.0.1:8080/health": no answer within 5s (time_limit_per_run)
failing since 23:05 UTC
```

In this example the restart fixes `myapp`. When the health page has answered
for 30 seconds, MMWatchdog sends a second message:

```text
Resolved SOS: app01: myapp/health_page_answers is ok again
HTTP 200 and configured content match
```

Afterwards, `sudo mmwatchdog events --last 15m` lists what happened, with
the newest event first:

```text
2026-11-14T23:07:32Z recovered            myapp/health_page_answers HTTP 200 and configured content match
2026-11-14T23:06:01Z systemd_unit_restart_result myapp/health_page_answers systemd unit restart finished. The next check runs will show whether the check has recovered
2026-11-14T23:06:01Z systemd_unit_restart_requested myapp/health_page_answers restart systemd unit myapp.service, attempt 1
2026-11-14T23:06:01Z failed               myapp/health_page_answers Get "http://127.0.0.1:8080/health": no answer within 5s (time_limit_per_run)
```

[The whole configuration](#the-whole-configuration) shows the other files
that this check group needs.

### If you've used Monit

MMWatchdog covers the everyday work of Monit: it watches services, files,
and resources on a server, restarts what fails, and alerts. MMWatchdog
differs in these ways:

- **Times, not cycles.** Monit counts a failure in cycles of its polling
  interval, such as "for 3 cycles". MMWatchdog's settings state times, such
  as `alert_after_failing_for: 1m`.
- **Restarts go through systemd.** In Monit, you write the start command and
  the stop command of each service. In MMWatchdog, you name the systemd
  unit, and MMWatchdog asks systemd to restart the unit. MMWatchdog never
  starts a unit that systemd reports as stopped.
- **More alert destinations ship with it.** Monit sends its own alerts by
  email, and reaches another destination through a script that you write.
  MMWatchdog delivers to email, a webhook, Slack, Telegram, [Discord](https://discord.com/),
  [Mattermost](https://mattermost.com/), and [Pushover](https://pushover.net/) today, and your coding agent can
  [add another destination](docs/reference/writing-modules.md) as a module of your
  own.
- **The fleet view is part of the same program.** The view of many Monit
  servers is [M/Monit](https://mmonit.com/), a separate product with a paid license. In MMWatchdog,
  one node takes the fleet hub role.
- **YAML, with names that say what happens.** Monit has a configuration
  language of its own. MMWatchdog uses YAML, and `mmwatchdog validate`
  checks every check against the description of its module.

## What you get

### Maxxed for monkey/human understanding

Even if your coding agent writes the files, you may still want to read
them, sometimes during an outage at 3 AM. So MMWatchdog's settings have
names that say exactly what they do:

- `alert_after_failing_for: 1m`
- `recovered_after_healthy_for: 30s`
- `fail_when: the_unit_is_not_active`
- `no_alerts_or_recovery_actions_while_these_checks_fail: [postgres/service_is_running]`

Each setting is written on the thing that the setting affects: the timings
of a check group are in the check group. `sudo mmwatchdog config` shows the
values that are in effect on the node.

### Your coding agent writes and runs it

A coding agent can write the YAML files and run the commands, and a person
checks the result. These parts of MMWatchdog are made for that:

- `mmwatchdog validate --config-only --config-dir DIR` checks a copy of the
  configuration on any machine, without a node.
- `mmwatchdog describe-module NAME` prints a module's whole page, with its
  inputs, examples, and errors, so the agent doesn't have to guess.
- `sudo mmwatchdog check-now CHECK_GROUP` runs the checks of one check group
  at once and prints what each check found. It sends no alert and restarts
  nothing.
- `--json` gives the agent structured results, so the agent doesn't have to
  parse output written for people.

### One problem, one alert

An alert that comes too often gets ignored. MMWatchdog sends one alert when
a problem has lasted long enough, a reminder as often as the destination
states, and one message when the problem is over.

- When one failure causes others, a check can stay quiet while another
  check fails. For example, the checks of an application stay quiet while
  the check of its database fails.
- `sudo mmwatchdog maintenance on myapp --for 30m` keeps the check group
  `myapp` quiet during planned work. The maintenance ends by itself.
- When a destination cannot be reached, the node keeps the alert for the
  time that the node's settings state, and tries again.

### It repairs what it can

`restart_systemd_unit` restarts a systemd unit when the checks that you name
have failed for the time that you set. `recovery_command` runs a command of
yours instead. Each has a limit on attempts, so MMWatchdog does not restart
a broken service forever. A restart is only an attempt: the alert closes
when the check is healthy again, not when the restart finishes.

### The common checks are built in

More than 40 **core modules** are built into MMWatchdog and cover the
common checks:

- **Services:** systemd units, HTTP, TCP, TLS certificates, DNS, and mail
  servers.
- **Data:** PostgreSQL, MySQL and MariaDB, Redis, memcached, [SQLite](https://sqlite.org/), and
  backups made with pgBackRest.
- **The server:** CPU, memory, disks, filesystems, network interfaces,
  temperature, and the clock.
- **Security:** waiting security updates, a needed reboot, and the end of a
  distribution release's security support.
  [Limits on each distribution](docs/reference/limits-on-each-distribution.md) says
  which of these checks work on which distribution.
- **Everything else:** files, log lines, Docker containers, and a `shell`
  check that runs a script of yours.

Eight **official extra modules** ship with MMWatchdog: five alert
destinations (Slack, Telegram, Discord, Mattermost, and Pushover) and three
checks (MongoDB, ZFS, and Apache). The
[module pages](docs/reference/modules/README.md) list every module of both kinds.

### Your agent can write a missing module

When no module fits, your agent can write one for your project in [Starlark](https://github.com/bazelbuild/starlark),
a language that reads like Python. A module of your own can be a check or
an alert destination. `mmwatchdog validate` checks each use of your module
against the inputs that the module declares.
[Writing modules reference](docs/reference/writing-modules.md) is the guide your agent follows.

### A scheduled job can report in

A backup or another scheduled job can tell MMWatchdog each time it
succeeds, with `mmwatchdog heartbeat`. When the job has not reported in
time, MMWatchdog alerts. So you hear about the backup that silently stopped
running.

### Measurements you can look back at

Some checks measure a number, such as how full a disk is. The node keeps
every reading for the time and up to the size that you set.
`sudo mmwatchdog readings CHECK_GROUP --chart` draws the readings in the
terminal.

### One place to see every node

With a fleet hub, `mmwatchdog fleet` on the fleet hub shows every node and
its failing checks. The fleet hub alerts when a node stops reporting. Each
node still runs its own checks. When the fleet hub is down, a node sends its
alerts directly, after a wait that you set.

If every node is down, no node can send an alert. MMHotline, the
Monkeymaxxing on-call program that a team runs on its own server, covers
that case: the fleet hub, or a node without a fleet hub, checks in with
MMHotline every 30 seconds, and MMHotline alerts when the check-ins stop. MMHotline is optional, and
MMWatchdog is complete without it.

### Ruthlessly simple

MMWatchdog is one program on each node. It needs no database server and no
other service: the node keeps its state in SQLite files under
`/var/lib/mmwatchdog`.
It sends its alerts itself, so you need no paid alerting service.

## When another tool fits better

Choose another tool when you need one of these:

- **Dashboards and queries over metrics.** [Prometheus](https://prometheus.io/) with [Grafana](https://grafana.com/) stores
  metrics from many servers for a long time and lets you query and graph
  them. MMWatchdog keeps the readings of its checks and draws simple charts
  in the terminal.
- **Log search, request tracing, or error tracking.** MMWatchdog can alert
  on lines in a log, but it is not a place to search logs.
- **Windows servers, or Linux without systemd.** MMWatchdog watches only
  Linux servers that run systemd. Monit runs on more systems.

## The whole configuration

The configuration of a node is the directory `/etc/mmwatchdog`. The check
group from
[Why a monitoring tool instead of a cron script](#why-a-monitoring-tool-instead-of-a-cron-script)
needs two more files, and a file with the Slack address:

```text
/etc/mmwatchdog/
  mmwatchdog_node_settings.yml
  alert_destinations.yml
  check_groups/
    myapp.yml
    machine.yml
  secrets/
    slack-webhook-team
```

```yaml
# /etc/mmwatchdog/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h                   # keep an alert that could not be delivered, and try again, for 24 hours
undelivered_alerts_content_size_limit: 8MiB        # and keep at most 8 MiB of such alerts

alert_severity_when_this_node_goes_silent: sos     # used by a fleet hub when this node stops reporting
alert_severity_when_this_node_has_rebooted: warn   # the severity of the notice after a reboot of this node

keep_measurement_history_for: 7d                   # keep every reading for 7 days
measurement_history_size_limit: 256MiB             # and never more than 256 MiB of readings
```

Every node states these six settings.

```yaml
# /etc/mmwatchdog/alert_destinations.yml
destinations:
  team_slack_channel:
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]   # this destination takes alerts of every severity
    remind_every: 4h                      # remind of each open problem every 4 hours
```

The file `secrets/slack-webhook-team` holds one line: the address of a
Slack incoming webhook. The
[`slack` module page](official_extra_modules/slack/README.md) says how to
get the address. In the MMX project, the address goes in MMX's encrypted
vault, and each install writes the file on the node, readable by root only
(mode `0600`). [First run](#first-run) shows the two steps.

A second check group, `machine`, watches the node's disk:

```yaml
# /etc/mmwatchdog/check_groups/machine.yml
check_groups:
  machine:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m

    checks:
      root_filesystem_is_below_90_percent:
        node_filesystem: /
        fail_when_measurement_above:
          space_used_percent: 90
        alert_severity: warn
```

Run the commands on the node:

```sh
sudo mmwatchdog validate          # check the configuration, and read the files that it names
sudo mmwatchdog reload            # make the running MMWatchdog use the configuration
sudo mmwatchdog status            # show every check and what its latest run found
sudo mmwatchdog check-now myapp   # run the checks of myapp now, without alerts or restarts
```

[How MMWatchdog works](docs/how-it-works.md) explains the parts of
MMWatchdog and follows a failure from the first failed run to the recovery.

## Set it up with your coding agent

You can have your coding agent set up MMWatchdog. The prompt below tells
the agent to follow this README, check each step before the next, and ask
you before it changes anything on a server.

Paste this into your agent. Put your own servers in place of the two example
servers, your own login account in place of `root`, your own application
in place of `myapp`, and where you want alerts in place of the Slack
channel, such as your email address and your mail provider's SMTP server:

```text
Set up MMWatchdog to watch my servers. Follow the README at
https://github.com/monkeymaxxing/mmwatchdog, including its "For coding
agents" checklist.

My servers:
- app01 at 203.0.113.11
- app02 at 203.0.113.12

I log in to them as root with my current SSH setup. My application runs as
the systemd unit myapp.service and answers at http://127.0.0.1:8080/health.
Send alerts to my Slack channel. When you need a secret, such as the
webhook address or a mail password, have me type it at the hidden prompt of
`mmx secrets set mmwatchdog_secret_files NAME` myself, and keep it out of
the project's plain files and out of our chat. Tell me what the vault passphrase is, and have me run
`mmx secrets write-passphrase-file` myself, in my own terminal. Then run
`mmx dryrun` and `mmx run` yourself.

Start with one server, and run each step's check before the next step. Show
me the MMX dryrun output and wait for my yes before every mmx run. Then
prove the alert path: make one check fail and recover, and ask me whether
both messages arrived. Answer each of my questions before you go on. Keep
your messages short. When I must choose, give your recommendation and a
one-line reason, so I can just say yes. At the
end, show me how to run `sudo mmwatchdog status` on the server myself, to see
every check.
```

The sections below show the steps, if you want to follow along or do them
yourself.

## Install

You install MMWatchdog from a release archive, with MMX. The archive holds
the `mmwatchdog` program for the nodes, a starter configuration, and an MMX
project that installs both. First install
[MMX](https://github.com/monkeymaxxing/mmx#install) on your controller
machine.

The block below asks your node for its processor kind, downloads the
matching archive and its checksum file into the current folder, checks the
download, and unpacks it. Put your node's SSH login in the first line:

```sh
(
set -eu
node=root@203.0.113.11
version=0.1.3
case "$(ssh "$node" uname -m)" in
  x86_64)  platform=linux-amd64 ;;
  aarch64) platform=linux-arm64 ;;
  *) echo "MMWatchdog has no release for $(ssh "$node" uname -m) nodes."; exit 1 ;;
esac
release=https://github.com/monkeymaxxing/mmwatchdog/releases/download/v$version
curl -fsSLO "$release/mmwatchdog-$version-$platform.tar.gz"
curl -fsSLO "$release/SHA256SUMS"
if command -v sha256sum >/dev/null; then sha256sum --check --ignore-missing SHA256SUMS
else shasum -a 256 --check --ignore-missing SHA256SUMS; fi
tar -xzf "mmwatchdog-$version-$platform.tar.gz"
echo "Unpacked the MMX project: cd mmwatchdog-$version-$platform"
)
```

Then go into the folder that the last line names. That folder is the MMX
project, and [First run](#first-run) works in it.

The checksum finds a damaged download. To check also that GitHub built the
archive from this repository, run
`gh attestation verify mmwatchdog-0.1.3-linux-amd64.tar.gz --repo monkeymaxxing/mmwatchdog`
(with your archive's name) with the [GitHub CLI](https://cli.github.com/).

**By hand.** `uname -m` on the node prints `x86_64` for the archive
`linux-amd64`, and `aarch64` for `linux-arm64`. One project serves nodes of
one kind: for x86-64 nodes and ARM64 nodes together, unpack both archives
and use two projects.

**Upgrade.** Run the block again with the new version, and copy your
own files from the old project into the new one;
[Update MMWatchdog](docs/how-to/update-mmwatchdog.md) has the steps.

**Build from source.** To make the archives yourself, you need Git, Python
3, and Go 1.21 or newer; by default, Go then downloads the exact Go version
that MMWatchdog needs. [Install on one node](docs/how-to/install-on-one-node.md)
has the commands, from GitHub or from the GitLab mirror.

## First run

In the unpacked MMX project:

1. Put your node's address and SSH account in
   `inventories/hub_and_standalone.yml`. The playbook
   `hub_and_standalone` sets up a node that watches itself and sends its
   own alerts: your one server, or the fleet hub of several.
2. Say what to watch in `files/hub_and_standalone/check_groups/`. The
   starter check group, `machine`, records CPU use and alerts on memory use
   and disk use.
3. Say where alerts go in `files/hub_and_standalone/alert_destinations.yml`.
   The starter configuration has no alert destination until you add one.
4. Put the secret of the alert destination, such as the Slack address, in
   MMX's encrypted vault:

   The first time, choose the vault passphrase, in your own terminal:

   ```sh
   mmx secrets write-passphrase-file
   ```

   MMX asks for a new passphrase twice. The passphrase is the one key to
   the encrypted secrets file: make it new, and save it in your password
   manager. MMX creates the vault, saves the passphrase in a file that only
   you can read, and prints the line to use, such as
   `MMX_VAULT_PASSWORD_FILE=~/.config/mmx/vault-passphrase-mmx_example`. Run
   `export` with that line, or delete the file and type the passphrase at
   each prompt. Then store the address:

   ```sh
   mmx secrets set mmwatchdog_secret_files slack-webhook-team
   ```

   Type the address at the hidden prompt. To change the address later, add
   `--replace` to the command.

   Then list the file in `inventories/hub_and_standalone.yml`, under
   `vars`: `mmwatchdog_secret_files: [slack-webhook-team]`. Each install
   writes each listed file to `/etc/mmwatchdog/secrets/` on the node.
5. Install:

   ```sh
   mmx validate                       # check the project files, without contacting a node
   mmx dryrun hub_and_standalone      # show what would change on the node
   mmx run hub_and_standalone         # install MMWatchdog and start it
   ```

   On a node without MMWatchdog, the dry run marks many steps
   `not_checked`, some with an error such as `exit status 127`: those
   steps depend on files and accounts that the run itself creates first.
   The line `Run ...: succeeded` near the end says that the run can go ahead.

6. On the node, `sudo mmwatchdog status` shows every check.
7. Make a check fail and recover, and confirm that both messages arrive.
   [Install MMWatchdog on one node](docs/how-to/install-on-one-node.md#steps)
   has every step, with its commands and the output to expect.

**What the install does to the node.** MMX copies the `mmwatchdog` program
to `/usr/local/bin`, the configuration to `/etc/mmwatchdog`, and the modules
to `/usr/local/lib/mmwatchdog`. MMX starts MMWatchdog as the systemd
service `mmwatchdog.service`, which runs as root. MMX also creates the
system account `mmwatchdog`, which runs the checks that need no root.
MMWatchdog keeps its state in `/var/lib/mmwatchdog`.

[Install on one node](docs/how-to/install-on-one-node.md) covers each step in full, and
also [fleets](docs/how-to/set-up-a-fleet.md),
[updates](docs/how-to/update-mmwatchdog.md), and
[removal](docs/how-to/remove-mmwatchdog.md).

## For coding agents

When you set up MMWatchdog for someone, reach each outcome below and run its
check before moving on. Use one server first, then the rest.

| Step | Outcome | Check |
| --- | --- | --- |
| 1 | MMX is installed on the controller machine. | `mmx version` prints a version. |
| 2 | The release archive for the node's architecture is unpacked. | The directory holds `mmx_project_settings.yml` and the `mmwatchdog` program. |
| 3 | The inventory names the node, the check groups say what to watch, and an alert destination exists. | `mmx validate` prints `Valid:` and the project name. |
| 4 | The person approved the changes. | You showed the person the alert destination in `alert_destinations.yml` and the output of `mmx dryrun hub_and_standalone`, and they said yes. |
| 5 | MMWatchdog runs on the node. | `mmx run hub_and_standalone` ends with no failed node, and `sudo mmwatchdog status` on the node lists every check. |
| 6 | The alert path works. | The person saw the failure message and the recovery message at the destination. |
| 7 | The person can see for themselves that MMWatchdog runs. | The person ran `ssh NODE sudo mmwatchdog status` and saw every check. |

- Talk to the person in their own words: your server, your laptop, the
  monitoring program, the list of your servers, a group of checks. Use a
  name from MMX's or MMWatchdog's files, such as node, controller,
  inventory, check group, daemon, unit, `hub_and_standalone`, or
  `mmx-https-api.service`, only inside a command or file the person will
  type or open, or after one plain line that says what it is.
- When a step is done, report it in at most five short lines about what now
  works, not paths, versions, or checksums.
- Answer each question the person asks before you go on to the next step.
- Keep each message short: the person skims long ones. When the person must
  choose, such as port 7443 or `connection: ssh_only`, or how to test the
  alert path, give your recommendation and a one-line reason, so that the
  person can say yes.
- Look up each module with `mmwatchdog describe-module NAME` before using
  it. Use a core module when one fits. Use `shell` for a short script.
- Give each check a name that says what is healthy, such as
  `health_page_answers`, because the name appears in every alert.
- Never put a secret's value in a plain project file, a command line, or
  the chat. Have the person run `mmx secrets set mmwatchdog_secret_files NAME` and type
  the value at the hidden prompt. Each install
  writes it to a file under `/etc/mmwatchdog/secrets` that only root can
  read, and the configuration names the file.
- To unlock the vault, follow the bullet "To unlock the vault" in the
  ["For coding agents" section of MMX's README](https://github.com/monkeymaxxing/mmx#for-coding-agents).
  In short: tell the person what the vault passphrase is ("the one key to
  the encrypted secrets file; make it new, and save it in your password
  manager"), and have them run `mmx secrets write-passphrase-file` once, in
  their own terminal. Then run `MMX_VAULT_PASSWORD_FILE=... mmx dryrun` and
  `mmx run` yourself, with the line that command prints. Never read the
  file or ask for the passphrase in the chat. The person still types each
  secret's value at the hidden prompt of `mmx secrets set`.
- Before the first `mmx run`, tell the person in plain words that MMX
  starts its own service `mmx-https-api.service` on the node, on TCP port
  7443 on every network address. The service makes later runs faster, and
  only SSH keys that can already log in to the node can use it. With
  `connection: ssh_only` on the node in the inventory, MMX opens no port
  and works over SSH, a little slower. Let them choose before the run.
- Before the alert test, tell the person that each `mmx run` that restarts
  a running MMWatchdog sends two notices, `NODE: maintenance on for the
  whole node` and `NODE: maintenance off for the whole node`: the run
  pauses alerts while MMWatchdog restarts. The first install sends neither,
  because no MMWatchdog runs yet. Adding `--without-maintenance-on-and-off-alerts`
  to the `maintenance on` command of the `start_maintenance` task in the
  playbooks turns the two notices off.
- Do not set `alert_after_failing_for: 0s` to make alerts faster without
  asking the person. A short failure then sends an alert.

## When a check fails

Three commands on the node say what is wrong:

```sh
sudo mmwatchdog status myapp      # the state of each check of myapp, and why
sudo mmwatchdog events myapp      # what happened and when: failures, restarts, recoveries
sudo mmwatchdog check-now myapp   # run the checks now, without alerts or restarts
```

The states that you will see most often in `status`:

| State | What it means |
| --- | --- |
| `ok` | The check is healthy. |
| `confirming_failure` | The check has failed, but not yet for `alert_after_failing_for`. No alert was sent. |
| `failed` | The check has failed for `alert_after_failing_for`. MMWatchdog sent the alert, unless maintenance or another failing check keeps this check quiet. |
| `confirming_recovery` | The latest run was healthy, but not yet for `recovered_after_healthy_for`. |
| `unknown` | MMWatchdog could not tell whether the check is healthy, for example because the check cannot run on this distribution. |

When MMWatchdog has used every restart attempt, it stops restarting the
unit. Fix the cause, then run `sudo mmwatchdog reset-recovery-attempts myapp`.

## How it works

On each node, systemd runs one `mmwatchdog` process. That process runs each
check when the check is due, and remembers the result. When a check has
failed for long enough, the process records the failure, restarts the
systemd unit if the check group says so, and puts the alert in a queue. The
process sends each queued alert and tries again when a destination cannot be
reached.

The commands `mmwatchdog status`, `mmwatchdog events`, and the others talk
to that process on the same node. MMX only installs and updates MMWatchdog;
MMX takes no part in the monitoring.

[How MMWatchdog works](docs/how-it-works.md) explains each part.

## Documentation

| Guide | Read it when you want to |
|---|---|
| [How MMWatchdog works](docs/how-it-works.md) | Understand the parts of MMWatchdog, where each one runs, and how checks, alerts, and the fleet hub work together |
| [Commands](docs/reference/commands.md) and [Configuration files](docs/reference/configuration-files.md) | Look up a command, a setting, or a check group |
| [Module pages](docs/reference/modules/README.md) | Look up a module: what it checks or where it delivers, its inputs, examples, and errors. `mmwatchdog describe-module NAME` prints the same page |
| [Install on one node](docs/how-to/install-on-one-node.md), [Set up a fleet](docs/how-to/set-up-a-fleet.md), and [Update MMWatchdog](docs/how-to/update-mmwatchdog.md) | Install and update MMWatchdog through MMX, and set up a fleet |
| [Examples](examples/) | Start from working configuration files, or see a whole worked example: the [web service fleet](examples/web-service-fleet/README.md) |
| [Write your own check or destination](docs/how-to/write-your-own-check-or-destination.md) | Add a check or an alert destination of your own |
| [Limits on each distribution](docs/reference/limits-on-each-distribution.md) | See what MMWatchdog cannot do on each tested distribution, and why |

## Contributing

MMWatchdog's code is on GitHub at
[github.com/monkeymaxxing/mmwatchdog](https://github.com/monkeymaxxing/mmwatchdog),
with the release archives. Report issues and send changes on GitHub. For
when GitHub is down, a mirror at
[gitlab.com/monkeymaxxing/mmwatchdog](https://gitlab.com/monkeymaxxing/mmwatchdog)
has the code and the release tags but no release archives: from the
mirror, build from source.

[CONTRIBUTING.md](CONTRIBUTING.md) says how to report a bug or propose a
change. [AGENTS.md](AGENTS.md) is the starting point for changing MMWatchdog
itself. It links the [release plan](docs/changing-mmwatchdog/design/README.md), the
[acceptance contracts](docs/changing-mmwatchdog/acceptance.md), the
[validation record](docs/changing-mmwatchdog/validation.md), the [testing guide](docs/changing-mmwatchdog/testing.md),
the [implementation rules](docs/changing-mmwatchdog/implementation-rules.md), and the
[documentation map](docs/changing-mmwatchdog/documentation-map.md).

## License

Apache License 2.0. Included dependencies keep their own licenses.
