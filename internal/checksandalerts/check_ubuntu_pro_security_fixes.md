# `ubuntu_pro_security_fixes`

`ubuntu_pro_security_fixes` fails while the node misses a published security
fix of an installed package, because only an Ubuntu Pro service delivers the
fix and the node does not get that service. Add the check to Ubuntu nodes
without Ubuntu Pro. The check reads the Ubuntu Pro client's data on the node
and downloads nothing. The check is opt-in: MMWatchdog runs only the checks
that a check group names, and no example setup names
`ubuntu_pro_security_fixes`.

Ubuntu Pro is Canonical's subscription for Ubuntu, free for a few personal
machines. A node is attached to Ubuntu Pro after someone runs `pro attach`
with the subscription's token. Two Ubuntu Pro services deliver security fixes
that Ubuntu's free archive does not have:

- **ESM Apps** (Expanded Security Maintenance for Applications) fixes the
  packages of Ubuntu's Universe component, such as Podman and pgBackRest.
  Many security fixes of Universe packages are in ESM Apps only.
- **ESM Infra** (Expanded Security Maintenance for Infrastructure) fixes the
  packages of Ubuntu's Main component after the release's free standard
  support ends.

Each source package is one finding and one incident. A source package holds
the source code from which Ubuntu builds one or more installable binary
packages. For example, the source package `python-wheel` builds the binary
packages `python3-wheel` and `python-wheel-common`.

Use [`apt_security_updates`](check_apt_security_updates.md) beside
`ubuntu_pro_security_fixes` to report security fixes that apt on the node can
already install.

## Set up

Install `ubuntu-pro-client` on the Ubuntu node. Ubuntu 24.04 and 26.04 include
the client. As root, run:

```sh
apt-get install ubuntu-pro-client update-notifier-common
apt-get update
systemctl enable --now apt-daily.timer
```

`update-notifier-common` configures apt's daily refresh and the successful-refresh
stamp `/var/lib/apt/periodic/update-success-stamp`. Root's `apt-get update`
also starts the Ubuntu Pro client's `esm-cache.service`, which refreshes the
ESM package lists. MMWatchdog never refreshes package lists.

To confirm the setup, wait until `systemctl show esm-cache.service
--property=ActiveState,Result` shows `ActiveState=inactive` and
`Result=success`. Then `sudo -H -u mmwatchdog pro security-status --format
json` must print JSON, which is what the check reads.

## When the check fails, and when it cannot check

- **The check fails** when `pro security-status --format json` lists an
  installed package under `esm-apps` or `esm-infra` with a status other than
  `upgrade_available`. The failed message names the source packages. Each
  finding names the needed service, binary packages, and fixed versions.
- **Each finding names its exact finding:** the release, the source
  package, its installed version, and each service with the fixed versions
  that it delivers, such as `ubuntu 24.04 python-wheel 0.42.0-1; esm-apps
  fixed version: 0.42.0-2ubuntu0.1~esm1`. The release is `ID` and
  `VERSION_ID` of `/etc/os-release`, and the installed version is the lowest
  installed version of the source package. Two nodes with the same exact
  finding miss the same fix, so a fleet hub sends one alert for all of
  them. A new installed version or a new fixed version is a new exact
  finding: the incident stays open, and MMWatchdog sends
  `exact_finding_changed`.
- **The check is healthy** with `this node misses no security fix that only
  Ubuntu Pro delivers` when no matching entry exists. Entries with
  `upgrade_available` belong to `apt_security_updates`, because apt can
  install those fixes now. Standard security updates are also left out.
- **The check is unknown** on Debian or another distribution without Ubuntu
  in `ID` or `ID_LIKE`; when apt has no package lists or no usable successful-refresh
  stamp; when the stamp is older than 7 days; when a command cannot run or
  fails; or when the Pro client's JSON has no packages array or attachment
  boolean. An unattached node with no nonempty ESM package list is also
  unknown, even when `pro` reports no fixes.

A team can do one of three things about a finding:

- **Attach the node to Ubuntu Pro.** Attaching does not install the fix.
  Once the node gets the needed service, apt can install the fix, the
  finding's incident resolves, and `apt_security_updates` reports the fix as
  a security update that waits until apt installs the fix.
- **Remove or replace the affected package.** The finding's incident
  resolves at the next run.
- **Live with the risk.** MMWatchdog has no action to acknowledge a finding:
  the finding stays one open incident, and a newly affected source package
  still opens its own incident and sends its own alert.

An unknown result opens a cannot-check incident after
`alert_after_failing_for`, as explained in
[the failing time](../../docs/how-to/decide-when-a-check-alerts.md). An unknown result
never confirms recovery of an open finding.

## Examples

### Add the check to an Ubuntu node without Ubuntu Pro

```yaml
# /etc/mmwatchdog/check_groups/ubuntu_pro.yml
check_groups:
  ubuntu_pro:
    check_every: 1h
    time_limit_per_run: 2m
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    checks:
      security_fixes_that_only_ubuntu_pro_delivers:
        ubuntu_pro_security_fixes:
          fail_when: the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers
        alert_severity: warn
```

Once an hour, MMWatchdog asks the Ubuntu Pro client about security fixes.
The Ubuntu Pro client is slow on small servers, so the example allows it 2
minutes: on 2026-11-16, `pro security-status --format json` took about 37
seconds on a server with 2 ARM64 cores and 156 waiting updates (Hetzner
`cax11`, Ubuntu 26.04), and about 2 seconds on the AMD64 test guests.
The first failed run opens a `warn` incident for each source package. If a
team lives with one finding, that finding stays one open incident; a newly
affected source package still gets an alert at once. The first healthy
run for a source package resolves that source package's incident.

## How it works

1. MMWatchdog reads `/etc/os-release` and requires Ubuntu in `ID` or
   `ID_LIKE`.
2. MMWatchdog requires nonempty apt package lists and the successful-refresh
   stamp `/var/lib/apt/periodic/update-success-stamp`, no more than 7 days old.
3. MMWatchdog runs `pro security-status --format json` as the unprivileged
   account `mmwatchdog`, from `/`, with `LC_ALL=C`. The command uses the
   account's home `/var/lib/mmwatchdog-checks` and can write its client log
   under `~/.cache/ubuntu-pro/`. The command reads local data and downloads
   nothing.
4. For an unattached node, MMWatchdog requires a nonempty file matching
   `/var/lib/ubuntu-advantage/apt-esm/var/lib/apt/lists/*_Packages`.
5. MMWatchdog runs `dpkg-query --show` with the format
   `${db:Status-Status}\t${Package}\t${source:Package}\t${source:Version}\n`,
   under the account, directory, and environment used for `pro`. Installed
   binary packages are grouped by source package, and the lowest source
   version is the source package's installed version. A binary absent from
   the inventory keeps its own name as the source package name, and its
   exact finding says `installed version unknown`.
6. Each source with an ESM Apps or ESM Infra fix whose status is not
   `upgrade_available` becomes one finding. Source names and binary names
   are sorted. The check's summary names at most 20 sources and counts the
   remaining sources.

## Errors and what they mean

| Message | Meaning of the message, and what to do |
| --- | --- |
| `ubuntu_pro_security_fixes.fail_when: is required: ...` | Write `fail_when: the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers` below `ubuntu_pro_security_fixes`. No other value is allowed. |
| `Ubuntu Pro exists only on Ubuntu, and this node runs ...` | Add the check only to Ubuntu nodes. |
| `apt has no lists of available packages ...` | Run `apt-get update` as root and enable daily refreshes. |
| `apt has no usable successful-refresh stamp ...` | Install `update-notifier-common`, then run `apt-get update` as root. |
| `the stamp ... is 9 days old ...` | Restore daily apt refreshes and run `apt-get update` as root. |
| `MMWatchdog cannot run pro, the command of Ubuntu's package ubuntu-pro-client: ...: install ubuntu-pro-client` | Install `ubuntu-pro-client` and ensure `pro` is in the standard command path. |
| `MMWatchdog cannot ask pro which updates wait: ...` | Run `pro security-status --format json` as the account `mmwatchdog` and inspect the client's error. |
| `MMWatchdog cannot read pro security-status JSON: ...` | The client's answer is unusable. Check the installed client and the client's log. |
| `the Ubuntu Pro client has no lists of ESM fixes in /var/lib/ubuntu-advantage/apt-esm ...` | Run `apt-get update` as root and check that `esm-cache.service` finishes successfully. A successful `pro` command alone proves no list freshness. |
| `MMWatchdog cannot read installed packages with dpkg-query: ...` | MMWatchdog could not read the list of installed packages. Run `dpkg-query --show` as the account `mmwatchdog` and read the error. |

## Tested on

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what MMWatchdog cannot do on each distribution, or did not test there, and why.

- **Go tests:** `TestUbuntuProSecurityFixesCheck` covers source grouping,
  fixed versions, the exact findings, ESM Apps and ESM Infra, ignored installable and standard
  fixes, command arguments and accounts, Debian, missing ESM lists,
  attachment state, unusable JSON, command failures, and apt's stamp rule.
- **The node suite** (`node_scenarios.py`) has a scenario for
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution).
  On Ubuntu, the scenario selects and installs a small package with an ESM
  Apps fix, checks the source finding, removes the package, and checks
  health again. On Debian, Arch Linux, and openSUSE Leap, the scenario
  checks the unknown result and its message.
- **Not tested: a node attached to Ubuntu Pro.** On such a node with the
  needed service enabled, the Ubuntu Pro client is expected to report the
  fix as `upgrade_available`, which the check leaves to
  `apt_security_updates`. apt is expected to name the service's archive,
  such as `UbuntuESMApps:24.04/noble-apps-security`, which
  `apt_security_updates` counts because the name contains `security`.

## Why it works this way

- **A separate module keeps separate rules.** `apt_security_updates`
  reports the fixes that apt can install now, and this check the fixes that
  only Ubuntu Pro delivers, which a team may decide to live with. Both
  report one incident per source package; another `fail_when` of the apt
  module would mix a team's choice about Ubuntu Pro into the updates that it
  must install.
- **The check is opt-in because most people are not Pro subscribers.** A
  team that decided against Ubuntu Pro might not want an open incident for
  every fix that only Ubuntu Pro delivers, so each team chooses. No example
  setup names `ubuntu_pro_security_fixes`.
- **The Pro client owns the ESM rules.** Calling `pro` uses Ubuntu's own
  interpretation of service access and available fixes. Reading the ESM
  package lists ourselves would duplicate the client's package and service
  rules.
- **Source packages keep findings useful.** Several binary packages can
  come from one source and need the same fix. One source finding avoids
  repeated incidents. A newly affected source still gets an alert while a
  finding that the team lives with stays open.
- **Missing ESM lists cannot mean health.** An unattached client's command
  exits successfully with no ESM fixes when the ESM lists are missing.
  MMWatchdog returns unknown to refuse that false healthy result.
- **apt's stamp is the refresh evidence.** Root's `apt-get update` starts
  `esm-cache.service`, so the check shares apt's successful-refresh rule.
  An ESM `InRelease` file's `Date:` describes the archive release, not the
  node's refresh. The successful-refresh stamp does not independently prove
  that the asynchronous ESM refresh succeeded; the check also requires ESM
  package data on unattached nodes.
- **A failed ESM refresh can go unnoticed.** When apt's refresh succeeds and
  the Ubuntu Pro client's ESM refresh fails, for example because a firewall
  lets the node reach Ubuntu's archive but not `esm.ubuntu.com`, the ESM
  lists stay old, and the check misses fixes that arrived after the last ESM
  refresh. The ESM lists carry no time of the node's last refresh, so
  MMWatchdog cannot tell old ESM lists from current ones.
