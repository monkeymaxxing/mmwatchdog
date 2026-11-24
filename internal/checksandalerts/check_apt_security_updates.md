# `apt_security_updates`

`apt_security_updates` fails while a security update waits to be
installed on the node, and its message names the packages. The check asks
the node's own package manager, `apt-get` on Debian and Ubuntu. The check
downloads nothing and reads only what the package manager already knows.
On a node of the Red Hat family, such as Rocky Linux, AlmaLinux, and Fedora, use
[`dnf_security_updates`](check_dnf_security_updates.md) instead. On a SUSE node,
such as openSUSE Leap, use
[`zypper_security_patches`](check_zypper_security_patches.md) instead.
Set `fail_when` to `a_security_update_waits_to_be_installed` to state when
the check fails.

The check sees only security fixes that apt on the node can install. An
Ubuntu node without Ubuntu Pro should also add
[`ubuntu_pro_security_fixes`](check_ubuntu_pro_security_fixes.md) for the
security fixes of Universe packages that only Ubuntu Pro delivers.

The update and release-support checks report waiting updates, a
required restart, and the end of a release's security support. The update and
release-support checks do not look for known exploited flaws.

To learn whether the node must restart to use updates that are already
installed, such as a new kernel, use [`reboot_after_updates`](check_reboot_after_updates.md).
To learn when the release's security support ends, use
[`distribution_release_security_support`](check_distribution_release_security_support.md).

To find known exploited flaws in installed packages, including flaws with no
fix yet, use [`known_exploited_vulnerabilities_in_installed_packages`](check_known_exploited_vulnerabilities_in_installed_packages.md), which reads CISA's list and OSV.dev.

## Set up

The node must refresh its lists of available packages by itself. MMWatchdog
never refreshes the node's lists of available packages.

- **Debian and Ubuntu:** apt must refresh its lists every day, and write
  the stamp `/var/lib/apt/periodic/update-success-stamp` after each
  refresh in which every list arrived.
  - Ubuntu 24.04 and 26.04: the package `update-notifier-common` sets up
    daily refreshes and writes the stamp after each refresh in which every
    list arrived. Ubuntu's server installation and cloud images include
    `update-notifier-common`.
  - Debian 12 and 13: no Debian package writes the stamp only after a
    successful refresh. Write `/etc/apt/apt.conf.d/15update-stamp` with the
    line from Ubuntu's package. Then turn on daily refreshes.
    The package `unattended-upgrades` also turns on daily refreshes:

    ```text
    // /etc/apt/apt.conf.d/15update-stamp
    APT::Update::Post-Invoke-Success {"touch /var/lib/apt/periodic/update-success-stamp";};
    APT::Periodic::Update-Package-Lists "1";
    ```

    Do not install Debian's package `apt-config-auto-update` to write the
    successful-refresh stamp. The package's
    `/etc/apt/apt.conf.d/15update-stamp` touches the stamp after every
    refresh, including a failed refresh.

  apt runs the hook after the daily refresh and after a plain
  `apt-get update`, so `apt-get update` writes the stamp at once. The
  daily refresh runs from `apt-daily.timer`; enable it with
  `systemctl enable --now apt-daily.timer` if it is off.

## When the check fails, and when it cannot check

- **The check fails** while at least one package has a security update
  that waits, with a message such as `3 security updates wait to be
  installed: curl, libssl3, openssl`. The message names 20 packages at most
  and counts the rest, such as `, and 4 more`. Each package counts once,
  however many security advisories name it.
- **All waiting updates are one finding,** `package security`, with one
  incident, one alert, and one recovery, and the check's message as the
  finding's message. Its exact finding names the node's release, `ID` and
  `VERSION_ID` of `/etc/os-release`, and what waits, such as
  `debian 13: security updates wait`. So every node of
  a release that waits for updates has the same exact finding, whichever
  updates wait, and a fleet hub sends one alert for all of them. A further
  update that starts to wait changes no exact finding and sends no alert.
  When nothing waits any more, the finding recovers with
  `no security update waits to be installed`.
- **The check is healthy** with `no security update waits to be installed`.
- **MMWatchdog cannot check, and the check is unknown,** when any condition
  below applies. The message explains why MMWatchdog cannot check. An
  unknown result is never healthy:
  - The successful-refresh stamp is missing or cannot be read, even when
    package lists exist. For apt, the message names
    `/var/lib/apt/periodic/update-success-stamp`, Ubuntu's package that
    writes it, and the line that writes it on Debian.
  - The successful-refresh stamp is more than 7 days old. The message names
    the stamp and its age, and gives the refresh instructions in [Set up](#set-up).
  - The package manager holds no nonempty lists yet. A stamp without package
    data is not enough. The message names the package manager and gives the
    refresh instructions.
  - The package manager cannot be run, or it ends with an error.
  - The node runs a distribution that MMWatchdog does not know, or the Red
    Hat family, or SUSE. On those, the message says to use
    `dnf_security_updates` or `zypper_security_patches`.

An unknown result opens a cannot-check incident once it has lasted for the
check's `alert_after_failing_for`, as for every check
([the failing time](../../docs/how-to/decide-when-a-check-alerts.md)).

## Examples

### Tell the team when a security update waits

```yaml
# /etc/mmwatchdog/check_groups/node_software.yml
check_groups:
  node_software:
    check_every: 1h
    time_limit_per_run: 2m
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    checks:
      no_security_update_waits:
        apt_security_updates:
          fail_when: a_security_update_waits_to_be_installed
        alert_severity: warn
```

Once an hour, MMWatchdog on the node asks the package manager which security
updates wait. `alert_after_failing_for: 0s` sends the `warn` alert after the
first run that finds a waiting security update. The alert message names the
packages. After the updates are installed, the next run is healthy, and
`recovered_after_healthy_for: 0s` sends the recovery at once. A run may take
2 minutes, because a package manager can be slow on a busy node.

### A node that installs its security updates by itself

```yaml
# /etc/mmwatchdog/check_groups/node_software.yml
check_groups:
  node_software:
    check_every: 1h
    time_limit_per_run: 2m
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    checks:
      automatic_security_updates_still_work:
        apt_security_updates:
          fail_when: a_security_update_waits_to_be_installed
        alert_severity: warn
        alert_after_failing_for: 3d
```

On a node where `unattended-upgrades` installs security
updates every day, the check fails only between the day an update appears
and the next automatic installation. The check states
`alert_after_failing_for: 3d`, which is longer than the wait until the next
daily automatic installation. So MMWatchdog on the node sends no alert for
an update that the next installation takes care of. When the check has
failed for 3 days, the alert says that the automatic installation has
stopped working.

## How it works

MMWatchdog reads `/etc/os-release` on the node. MMWatchdog recognizes Debian and
Ubuntu when the distribution's `ID` or any name in `ID_LIKE` is `debian`
or `ubuntu`. Other distributions make the check unknown; a node of the Red
Hat family is told to use [`dnf_security_updates`](check_dnf_security_updates.md), and a
SUSE node [`zypper_security_patches`](check_zypper_security_patches.md).

1. MMWatchdog requires nonempty package lists in `/var/lib/apt/lists/*_Packages`
   or their compressed files, and a successful-refresh stamp at
   `/var/lib/apt/periodic/update-success-stamp`. The stamp's time alone decides
   freshness. Missing lists, a missing stamp, or a stamp older than 7 days
   makes the check unknown. Directory times, list times, and `update-stamp`
   do not prove success.
2. MMWatchdog runs `apt-get --simulate --quiet dist-upgrade` with `LC_ALL=C`, as
   the unprivileged account `mmwatchdog`, in `/`. The simulation changes nothing,
   and the account `mmwatchdog` cannot change anything anyway.
3. MMWatchdog counts each package that the upgrade would install from a security
   archive: an archive whose name contains `security`, such as Debian's
   `Debian-Security` and Ubuntu's archives that end with `-security`.

## Errors and what they mean

| Message | Meaning of the message, and what to do |
| --- | --- |
| `apt_security_updates.fail_when: is required: The check fails while a security update waits to be installed; write a_security_update_waits_to_be_installed, the only value.` | Write `fail_when: a_security_update_waits_to_be_installed` below `apt_security_updates`. |
| `apt_security_updates.fail_when: expected one of a_security_update_waits_to_be_installed` | Use `a_security_update_waits_to_be_installed`, the only allowed value. |
| `the stamp /var/lib/apt/periodic/update-success-stamp, which apt writes after a successful refresh, is 9 days old, so MMWatchdog cannot know which updates wait: ...` | The last successful refresh is too old. Enable the daily refresh in [Set up](#set-up). |
| `apt has no usable successful-refresh stamp at /var/lib/apt/periodic/update-success-stamp, so MMWatchdog cannot know which updates wait: have apt write this stamp after each successful refresh; the module page apt_security_updates, section "Set up", shows how (mmwatchdog describe-module apt_security_updates)` | The message is short on purpose; the configuration is in this page. Have apt write the stamp, as [Set up](#set-up) shows: `update-notifier-common` on Ubuntu, the file `/etc/apt/apt.conf.d/15update-stamp` on Debian. Then `apt-get update` writes the stamp at once. `unattended-upgrades` alone writes no such stamp. |
| `MMWatchdog cannot run apt-get: ...` | `apt-get` is not installed, or not in the standard path. |
| `MMWatchdog cannot ask apt-get which updates wait: ...` | The package manager ended with an error, and the message gives what it wrote. Run the command of "How it works" by hand to see the whole answer. |
| `MMWatchdog does not know the package manager of NAME: it knows only that of Debian and Ubuntu (apt_security_updates), that of the Red Hat family (dnf_security_updates), and that of SUSE (zypper_security_patches)` | The distribution is not one of MMWatchdog's targets. |
| `this node runs NAME, whose package manager is dnf, not apt: use the check dnf_security_updates` | The node runs the Red Hat family. Use [`dnf_security_updates`](check_dnf_security_updates.md). |
| `this node runs NAME, whose package manager is zypper, not apt: use the check zypper_security_patches` | The node runs SUSE. Use [`zypper_security_patches`](check_zypper_security_patches.md). |

## Tested on

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what MMWatchdog cannot do on each distribution, or did not test there, and why.

- **Go tests** in `server_checks_test.go`: `TestNodeDistributionFamily`
  checks `ID` and `ID_LIKE`. `TestUpdateChecksOnUnknownDistributions`
  checks the unknown result and its distribution name.
  `TestAptSecurityUpdatesCheck` covers the simulated upgrade of
  `apt-get` with Debian and Ubuntu archives, the one finding with the
  release as its exact finding, the account and arguments,
  missing or old lists, the refresh stamp, and a package manager that fails.
  `TestSecurityUpdateCheckRequiresSuccessfulRefreshStamp` refuses fresh lists
  without a success stamp before it queries apt.
  `TestSecurityUpdateRefreshStampTakesPrecedenceOverFreshLists` refuses a
  stamp older than seven days even when package lists are fresh.
- **The node suite** (`node_scenarios.py`) runs the check against the real
  package manager on the Debian and Ubuntu releases of
  [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution).
  On Arch Linux, the suite checks only that the check is
  unknown and that its message names the distribution. On openSUSE Leap,
  [`zypper_security_patches`](check_zypper_security_patches.md) takes the
  check's place.
  The node suite first checks the unknown result without the success stamp.
  Where the guest has no refresh hook, the suite writes
  `/etc/apt/apt.conf.d/15update-stamp` as [Set up](#set-up) shows.
  The suite runs `apt-get update`, which writes the stamp, and compares
  security updates with its own package count. The node suite also tests a
  stamp made nine days old.

## Why it works this way

- **Usable lists must exist before refresh times count.** An empty cache
  directory or a success stamp without package data cannot prove no updates wait.
- **Only a success stamp proves a successful refresh.** A failed refresh
  leaves old lists in place and can update one repository while failing on
  the security repository. Even the newest list cannot prove that every
  repository refreshed successfully. Without the stamp, MMWatchdog must report
  unknown. Directory times and failed-attempt stamps cannot prove success.
- **apt's `update-stamp` is not a success stamp.** apt's daily job writes
  `/var/lib/apt/periodic/update-stamp` whenever `apt-get update` ends with
  exit code 0, and `apt-get update` does that also when a list could not be
  fetched because of a network error: it warns "Some index files failed to
  download" and keeps the old list. apt runs a `Post-Invoke-Success` hook
  only when every list arrived. On a Debian 13 server on 2026-10-22, only
  `update-stamp` existed after the daily refresh. An `apt-get update`
  run on 2026-10-22 used a source whose host name did not resolve.
  The `apt-get update` run ended with exit code 0 but did not run the
  `Post-Invoke-Success` hook.
- **On Debian the administrator writes the hook.** Debian's package
  `apt-config-auto-update` 2.2, in Debian 12 and 13, writes the stamp with
  `APT::Update::Post-Invoke`, which apt runs after every refresh, so its
  stamp cannot prove success. Ubuntu's line is one line of apt's own
  configuration and needs no program.
- **The check downloads nothing.** MMWatchdog reads only what the package
  manager already knows, so a run changes nothing on the node that
  MMWatchdog watches.
- **A success stamp older than 7 days makes the check unknown.** After a
  week, the package manager does not know the updates of the last days, and "no
  update waits" would be a guess.
- **`apt-get` runs as the unprivileged account `mmwatchdog`.** The simulation
  needs no root privileges, so MMWatchdog runs `apt-get` without root
  privileges.
- **Each package counts once.** The update that waits for a package is one,
  the newest, whatever the number of advisories that name it.
- **One finding for all waiting updates, keyed to the release.** The
  remedy is the same for every waiting update, to install the updates, and a
  node that waits for one usually waits for several. A finding for each
  update would send a node one alert for each, such as 27 for an old
  kernel, and a new update across a fleet would send another alert. The
  message still names the packages.
- **A check of MMWatchdog 0.1.x had one incident without a finding.** At
  the upgrade to 0.2.0, that incident closes once with the message that the
  check's module now reports each finding as an incident of its own, and the
  finding `package security` opens as a new incident.
