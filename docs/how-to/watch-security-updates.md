# Watch for security updates

**When to use this page:** you want an alert when a node has security
updates that wait to be installed, runs a package with a flaw that
attackers are known to use, must reboot to use updates that are already
installed, or runs a release whose security support ends soon. Or such an
alert arrived, and you want to clear it.

**Who acts:** you, or your coding agent. Each step runs on your controller
machine, the machine where you run `mmx`, except where the step says "on the
node". The reboot in [Clear an alert](#clear-an-alert) stops every
application on the node while it restarts: on a production node, a coding
agent asks the person first.

**What this page does:** you prepare the node so that its package manager
knows which updates exist, add a check group with the security checks for
the node's distribution, and install it with MMX. Then you read what each
alert says and clear it by installing the updates or rebooting the node.

**Terms:** a **security update** is a new version of an installed package
that fixes a security flaw, published by the node's distribution. A **known
exploited flaw** is a flaw on the
[list of known exploited vulnerabilities](https://www.cisa.gov/known-exploited-vulnerabilities-catalog)
that CISA, the US cybersecurity agency, publishes: attackers are known to
use each flaw on the list. A **check group** is a named set of checks with
shared timings. A **module** is the code that does one kind of check; each
check names one module.

## Choose the checks

Each distribution family has its own checks. Find the node's distribution
with `ssh deploy@app01 grep PRETTY_NAME /etc/os-release`, then add the checks
of its column:

| Question | Debian and Ubuntu | Red Hat family: Rocky Linux, AlmaLinux, Fedora | SUSE: openSUSE Leap | Arch Linux |
|---|---|---|---|---|
| Does a security update wait to be installed? | [`apt_security_updates`](../reference/modules/apt_security_updates.md) | [`dnf_security_updates`](../reference/modules/dnf_security_updates.md) | [`zypper_security_patches`](../reference/modules/zypper_security_patches.md) | no check |
| Must the node reboot to use installed updates? | [`reboot_after_updates`](../reference/modules/reboot_after_updates.md) | `reboot_after_updates` | `reboot_after_updates` | no check |
| Does the release's security support end soon? | [`distribution_release_security_support`](../reference/modules/distribution_release_security_support.md) | `distribution_release_security_support` | `distribution_release_security_support` | no check: a rolling release has no end of support |
| Does an installed package have a known exploited flaw, also one with no fix yet? | [`known_exploited_vulnerabilities_in_installed_packages`](../reference/modules/known_exploited_vulnerabilities_in_installed_packages.md) | no check | no check | no check |
| Does a security fix wait that only Ubuntu Pro delivers? | [`ubuntu_pro_security_fixes`](../reference/modules/ubuntu_pro_security_fixes.md), on Ubuntu without Ubuntu Pro | no check | no check | no check |

"No check" means that the check runs there but stays `unknown`, with a
message that gives the reason. Leave it out of the check group.
[Limits on each distribution](../reference/limits-on-each-distribution.md) says what
each distribution lacks, why, and what you can do.

| Your node runs | Follow |
|---|---|
| Debian or Ubuntu | [Set up the checks on Debian or Ubuntu](#set-up-the-checks-on-debian-or-ubuntu) |
| Rocky Linux, AlmaLinux, Fedora, or openSUSE Leap | [Set up the checks on the Red Hat family or SUSE](#set-up-the-checks-on-the-red-hat-family-or-suse) |
| An alert about updates, a reboot, a flaw, or the end of support arrived | [Clear an alert](#clear-an-alert) |

## Before you start

- MMWatchdog runs on the node, installed with MMX from the MMX project on
  your controller machine. If not, follow
  [Install MMWatchdog on one node](install-on-one-node.md) first.
- Open a terminal in the MMX project: the directory that holds
  `mmx_project_settings.yml`.
- The node can reach its distribution's package repositories. The check
  for known exploited flaws also needs HTTPS, on port 443, to
  `raw.githubusercontent.com` and `api.osv.dev`.
- An alert destination takes `warn` alerts, such as the Slack channel of
  [Send alerts where your team sees them](send-alerts-somewhere.md).

## Set up the checks on Debian or Ubuntu

1. **Have apt refresh its package lists every day, and record each refresh
   that worked.** The checks never refresh the lists themselves: they read
   what apt already knows, and they trust only a refresh that reached every
   repository.

   - On Ubuntu 24.04 and 26.04, the package `update-notifier-common` does
     both. Ubuntu's server installation and cloud images include it.
   - On Debian 12 and 13, on the node, write the file
     `/etc/apt/apt.conf.d/15update-stamp`, then refresh once:

     ```sh
     ssh deploy@app01 'sudo tee /etc/apt/apt.conf.d/15update-stamp' <<'EOF'
     APT::Update::Post-Invoke-Success {"touch /var/lib/apt/periodic/update-success-stamp";};
     APT::Periodic::Update-Package-Lists "1";
     EOF
     ssh deploy@app01 sudo apt-get update
     ```

     Reason: the first line has apt write the stamp
     `/var/lib/apt/periodic/update-success-stamp` after each refresh in
     which every list arrived. The second line turns on the daily refresh,
     which `apt-daily.timer` runs.

   **Check:** on the node, the stamp exists and is from today:

   ```sh
   ssh deploy@app01 ls -l /var/lib/apt/periodic/update-success-stamp
   ```

   ```text
   -rw-r--r-- 1 root root 0 Nov 15 07:05 /var/lib/apt/periodic/update-success-stamp
   ```

2. **Install the program that marks a needed reboot.** When a new kernel is
   installed, a hook writes `/run/reboot-required`, which the reboot check
   reads. The hook comes with a package such as `unattended-upgrades`, and
   many nodes have it already: on Ubuntu 26.04, every node. First look:

   ```sh
   ssh deploy@app01 ls /etc/kernel/postinst.d/
   ```

   ```text
   initramfs-tools
   zz-update-grub
   ```

   This node has no hook: the list names none of `unattended-upgrades`,
   `update-notifier`, and `reboot-notifier`. Install `unattended-upgrades` with its automatic
   updates turned off:

   ```sh
   ssh deploy@app01 'echo "unattended-upgrades unattended-upgrades/enable_auto_updates boolean false" | sudo debconf-set-selections'
   ssh deploy@app01 sudo apt-get install -y unattended-upgrades
   ```

   Reason for turning automatic updates off: with this page's check group,
   the alert tells you that updates wait, and you install them. A node that
   installs its updates by itself needs another timing; see
   [If something is different](#if-something-is-different).

   **Check:** the hook is now in the kernel's hook directory:

   ```sh
   ssh deploy@app01 ls /etc/kernel/postinst.d/
   ```

   ```text
   initramfs-tools
   unattended-upgrades
   zz-update-grub
   ```

3. **Install the release dates.** The check of the release's security
   support reads them from the package `distro-info-data`, which some
   cloud images and server installations lack:

   ```sh
   ssh deploy@app01 sudo apt-get install -y distro-info-data
   ```

   **Check:** `ssh deploy@app01 ls /usr/share/distro-info/` lists
   `debian.csv` and `ubuntu.csv`.

4. **Write the check group.** Create
   `files/hub_and_standalone/check_groups/node_software.yml` in the MMX
   project:

   ```yaml
   # files/hub_and_standalone/check_groups/node_software.yml
   check_groups:
     node_software:
       check_every: 1h                     # run each check once an hour
       time_limit_per_run: 2m              # a package manager can be slow on a busy node
       alert_after_failing_for: 0s         # alert at the first run that finds a problem
       recovered_after_healthy_for: 0s     # the problem is over at the first healthy run
       checks:
         no_security_update_waits:
           apt_security_updates:
             fail_when: a_security_update_waits_to_be_installed
           alert_severity: warn
         node_needs_no_reboot:
           reboot_after_updates:
             fail_when: the_node_must_reboot_to_use_installed_updates
           alert_severity: warn
         release_gets_security_fixes:
           distribution_release_security_support:
             security_support: regular
             fail_when_security_support_ends_within: 180d
           alert_severity: warn
     known_exploited_flaws:
       check_every: 6h                     # each run downloads CISA list and asks OSV.dev
       time_limit_per_run: 5m              # a run takes a few minutes
       alert_after_failing_for: 5h         # a finding alerts when the next run still finds it
       recovered_after_healthy_for: 0s
       checks:
         no_installed_package_has_a_known_exploited_flaw:
           known_exploited_vulnerabilities_in_installed_packages:
             fail_when: an_installed_package_is_affected
           alert_severity: warn
   ```

   - `warn` sends each alert to the destinations that take `warn`: a
     person should look today. A waiting update rarely needs a person at
     night. To have a known exploited flaw wake someone, give that check
     `alert_severity: sos`.
   - `check_every: 1h`: apt refreshes the package lists once a day, so a
     run every hour finds a new update soon after the refresh. After you
     install updates, you can run the checks at once, as
     [Clear an alert](#clear-an-alert) shows.
   - `fail_when_security_support_ends_within: 180d` gives you about six
     months to upgrade the release. `security_support: regular` counts down
     to the free security support. For a Debian node with Debian LTS, write
     `debian_lts`; for an Ubuntu node with Ubuntu Pro, write `ubuntu_pro`.
   - The check for known exploited flaws is in a check group of its own,
     because each run asks two public services. Reason for
     `alert_after_failing_for: 5h`: a finding alerts when the next run, 6
     hours later, still finds it, and an outage of CISA's list or OSV.dev
     shorter than 6 hours sends no cannot-check alert.
   - On Ubuntu without Ubuntu Pro, add the check of
     [The check group on Ubuntu without Ubuntu Pro](#the-check-group-on-ubuntu-without-ubuntu-pro).

   **Check:** the file holds a `check_groups:` mapping, and every check has
   one module, its `fail_when`, and an `alert_severity`.

5. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists your new file as a change to
   `/etc/mmwatchdog`:

   ```text
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked input /etc/mmwatchdog/check_groups/node_software.yml would change
   ```

   The dryrun of this project exits with code 2: something would change.

6. **Install the checks:**

   ```sh
   mmx run hub_and_standalone
   ```

   The run checks the whole configuration on the node before it changes
   anything, and restarts MMWatchdog inside a 15-minute maintenance window.
   After the restart, each check runs at once.

   **Check:** the run ends with `Run ...: succeeded`. If it ends with
   `failed`, the node still runs its earlier configuration; see
   [If something is different](#if-something-is-different).

7. **Read the result of each check.** On the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status node_software
   ```

   ```text
   node_software/no_security_update_waits failed       2 security updates wait to be installed: curl, libcurl4t64
     Config: /etc/mmwatchdog/check_groups/node_software.yml:9:7
     Rule: check every 1h, time limit per run 2m, alert after failing for 0s, recovered after healthy for 0s
     Alert severity: warn
   ...
   node_software/node_needs_no_reboot ok           this node needs no reboot
   ...
   node_software/release_gets_security_fixes ok           this node runs Debian GNU/Linux 13 (trixie): Debian's regular security support ends on 2028-08-09, in 20 months
   ...
   ```

   The first run of the check for known exploited flaws ends a few
   minutes after the restart. Then:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status known_exploited_flaws
   ```

   ```text
   known_exploited_flaws/no_installed_package_has_a_known_exploited_flaw confirming_failure 1 known exploited flaw affects installed packages: CVE-2026-123158 openssh
     CVE-2026-123158 openssh    confirming_failure CVE-2026-123158 in openssh 1:10.0p1-7+deb13u4: no fix yet (CISA: OpenBSD OpenSSH Unexpected Status Code or Return Value Vulnerability; https://security-tracker.debian.org/tracker/CVE-2026-123158)
     Config: /etc/mmwatchdog/check_groups/node_software.yml:28:7
     Rule: check every 6h, time limit per run 5m, alert after failing for 5h, recovered after healthy for 0s
   ...
   ```

   **Check:** each check shows `ok`, `failed`, or `confirming_failure`,
   with a message about the node. A check that shows `failed` has sent its
   alert; follow [Clear an alert](#clear-an-alert). A finding that shows
   `confirming_failure` sends its alert if the next run, 6 hours later,
   still finds it; until then, `status` exits with 0, as for any check
   that has not failed for `alert_after_failing_for` yet. A check that shows `unknown` cannot
   check, and its message says what the node lacks; see
   [If something is different](#if-something-is-different).

## Set up the checks on the Red Hat family or SUSE

1. **Prepare the node,** as the Set up section of each module page says:
   - The security-update check needs a daily refresh of the package data:
     on the Red Hat family, `dnf-makecache.timer`, which Rocky Linux,
     AlmaLinux, and Fedora turn on by default
     ([`dnf_security_updates`](../reference/modules/dnf_security_updates.md#set-up));
     on openSUSE Leap, a systemd timer of your own
     ([`zypper_security_patches`](../reference/modules/zypper_security_patches.md#set-up)).
   - The reboot check on Rocky Linux and AlmaLinux needs the package
     `dnf-plugins-core`; on SUSE it needs nothing
     ([`reboot_after_updates`](../reference/modules/reboot_after_updates.md#set-up)).
   - The check of the release's security support needs nothing.

   **Check:** the module page's Set up says how to confirm each part.

2. **Write the check group.** Create
   `files/hub_and_standalone/check_groups/node_software.yml`. On the Red
   Hat family:

   ```yaml
   # files/hub_and_standalone/check_groups/node_software.yml
   check_groups:
     node_software:
       check_every: 1h                     # run each check once an hour
       time_limit_per_run: 2m              # a package manager can be slow on a busy node
       alert_after_failing_for: 0s         # alert at the first run that finds a problem
       recovered_after_healthy_for: 0s     # the problem is over at the first healthy run
       checks:
         no_security_update_waits:
           dnf_security_updates:
             fail_when: a_security_update_waits_to_be_installed
           alert_severity: warn
         node_needs_no_reboot:
           reboot_after_updates:
             fail_when: the_node_must_reboot_to_use_installed_updates
           alert_severity: warn
         release_gets_security_fixes:
           distribution_release_security_support:
             security_support: regular
             fail_when_security_support_ends_within: 180d
           alert_severity: warn
   ```

   On openSUSE Leap, the security-update check uses zypper's module, which
   names patches, not packages:

   ```yaml
   # files/hub_and_standalone/check_groups/node_software.yml
   check_groups:
     node_software:
       check_every: 1h                     # run each check once an hour
       time_limit_per_run: 2m              # a package manager can be slow on a busy node
       alert_after_failing_for: 0s         # alert at the first run that finds a problem
       recovered_after_healthy_for: 0s     # the problem is over at the first healthy run
       checks:
         no_security_patch_waits:
           zypper_security_patches:
             fail_when: a_security_patch_waits_to_be_installed
           alert_severity: warn
         node_needs_no_reboot:
           reboot_after_updates:
             fail_when: the_node_must_reboot_to_use_installed_updates
           alert_severity: warn
         release_gets_security_fixes:
           distribution_release_security_support:
             security_support: regular
             fail_when_security_support_ends_within: 180d
           alert_severity: warn
   ```

   The check group has no check for known exploited flaws: that check is
   `unknown` on these distributions. There, the security-update check is
   the one that alerts on each waiting security fix.

   **Check:** every check has one module, its `fail_when`, and an
   `alert_severity`.

3. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists
   `/etc/mmwatchdog/check_groups/node_software.yml` as `would_change`, and
   the dryrun exits with code 2.

4. **Install the checks:**

   ```sh
   mmx run hub_and_standalone
   ```

   **Check:** the run ends with `Run ...: succeeded`.

5. **Read the result of each check.** On the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status node_software
   ```

   **Check:** each check shows `ok` or `failed`. A check that shows
   `unknown` names what the node lacks; the module page's Set up says how to
   add it.

## Clear an alert

Each alert names the node, the check, and what the check found.

| The alert says | What clears it | Follow |
|---|---|---|
| `security updates wait to be installed` | Install the updates | [Install the waiting updates](#install-the-waiting-updates) |
| `this node needs a reboot` | Reboot the node | [Reboot the node](#reboot-the-node) |
| A CVE in a package, with `fixed version ... waits to be installed` | Install the updates, as for a waiting update | [Install the waiting updates](#install-the-waiting-updates) |
| A CVE in the running kernel, with `the fixed kernel ... runs after a reboot` | Reboot the node | [Reboot the node](#reboot-the-node) |
| A CVE with `no fix yet` or `fix needs Ubuntu Pro` | Protect the service until a fix arrives | [A known exploited flaw has no fix yet](#a-known-exploited-flaw-has-no-fix-yet) |
| `security support ends on ...` | Upgrade the node to a newer release | [The release's security support ends](#the-releases-security-support-ends) |
| `MMWatchdog cannot check ...` | Add what the message names | [If something is different](#if-something-is-different) |

### Install the waiting updates

1. **Read the alert.** It names the packages:

   ```text
   Warning: app01: node_software/no_security_update_waits failed
   2 security updates wait to be installed: curl, libcurl4t64
   failing since 07:06 UTC
   ```

   The message names 20 packages at most, and counts the rest, such as
   `, and 4 more`.

2. **Install the updates.** On the node:

   ```sh
   ssh deploy@app01 sudo apt-get upgrade -y
   ```

   ```text
   ...
   2 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.
   ...
   Setting up libcurl4t64:amd64 (8.14.1-2+deb13u7) ...
   Setting up curl (8.14.1-2+deb13u7) ...
   ```

   On the Red Hat family and on openSUSE Leap, install the updates with
   `dnf` or `zypper` in the same way.

3. **Run the checks at once,** instead of waiting up to an hour for the
   next run:

   ```sh
   ssh deploy@app01 sudo mmwatchdog check-now node_software --count-as-scheduled-run
   ```

   ```text
   node_software/no_security_update_waits ok       no security update waits to be installed
     Observed: 2026-11-15T07:09:23.214627146Z
   node_software/node_needs_no_reboot ok       this node needs no reboot
   ...
   The runs counted as scheduled runs for check group node_software; run mmwatchdog status node_software to see the resulting check status.
   ```

   Reason for `--count-as-scheduled-run`: without it, `check-now` only
   shows what the checks find, and the open alert stays open until the
   next scheduled run.

   **Check:** the destinations get the recovery:

   ```text
   Resolved warning: app01: node_software/no_security_update_waits is ok again
   no security update waits to be installed
   ```

   If the check now says `this node needs a reboot`, an update needs one:
   follow [Reboot the node](#reboot-the-node). A program that started
   before the update can still run the old code of a library until the
   program restarts; a tool such as `needrestart` lists the services to
   restart.

### Reboot the node

1. **Read the alert.** On Debian and Ubuntu, it names the packages that
   need the reboot:

   ```text
   Warning: app01: node_software/node_needs_no_reboot failed
   this node needs a reboot for 1 package: linux-image-6.12.117+deb13-amd64
   failing since 07:09 UTC
   ```

2. **Reboot the node** at a time that suits its users:

   ```sh
   ssh deploy@app01 sudo systemctl reboot
   ```

   MMWatchdog starts with the node, and each check runs at once.

   **Check:** the destinations get the reboot notice and the recovery:

   ```text
   Warning: app01 rebooted
   app01 rebooted at 07:10 UTC; MMWatchdog last ran at 07:10 UTC
   ```

   ```text
   Resolved warning: app01: node_software/node_needs_no_reboot is ok again
   this node needs no reboot
   ```

   To reboot without the alerts of the applications that stop meanwhile,
   open a maintenance window first, as
   [Pause alerts during planned work](pause-alerts-during-planned-work.md#pause-every-check-group)
   shows.

### A known exploited flaw has no fix yet

1. **Read the alert.** Each affected package and CVE is a finding with its
   own alert:

   ```text
   Warning: app01: known_exploited_flaws/no_installed_package_has_a_known_exploited_flaw: CVE-2026-123158 openssh failed
   CVE-2026-123158 in openssh 1:10.0p1-7+deb13u4: no fix yet (CISA: OpenBSD OpenSSH Unexpected Status Code or Return Value Vulnerability; https://security-tracker.debian.org/tracker/CVE-2026-123158)
   failing since 07:07 UTC
   ```

   The reason after the version is one of four: `fixed version VERSION
   waits to be installed`, `the fixed kernel VERSION is installed and runs
   after a reboot`, `fix needs Ubuntu Pro, version VERSION`, or
   `no fix yet`.

2. **Open the link of the security tracker** in the message. It says
   which versions are affected and, often, how to protect the service until
   the fix arrives, such as a setting to turn off.

3. **Protect the service,** such as by limiting who can reach it. For a
   `fix needs Ubuntu Pro` finding, you can attach the node to Ubuntu Pro
   with `pro attach`; the fix then becomes an ordinary waiting update.

   **Check:** none from MMWatchdog until the fix arrives. The finding stays
   one open alert; a destination with `remind_every` reminds you of it.
   When the distribution publishes
   the fix, the reason changes to `fixed version ... waits to be
   installed`; then follow
   [Install the waiting updates](#install-the-waiting-updates). The
   finding's alert clears at the first run after the fixed version is
   installed.

### The release's security support ends

The alert names the release and the date:

```text
this node runs Debian GNU/Linux 13 (trixie): Debian's regular security support ends on 2028-08-09, in 20 months
```

Upgrade the node to a newer release, or move its applications to a new
node, before that date. Your distribution's release notes describe the
upgrade. The alert clears at the first run on a release whose security
support ends more than `fail_when_security_support_ends_within` away.

## The check group on Ubuntu without Ubuntu Pro

Ubuntu delivers many security fixes of its Universe packages only through
Ubuntu Pro, Canonical's subscription. On a node without Ubuntu Pro,
`apt_security_updates` cannot see those fixes. Add the check
`no_fix_waits_that_only_ubuntu_pro_delivers`, which does, to the check
group of step 4:

```yaml
# files/hub_and_standalone/check_groups/node_software.yml
check_groups:
  node_software:
    check_every: 1h                     # run each check once an hour
    time_limit_per_run: 2m              # a package manager can be slow on a busy node
    alert_after_failing_for: 0s         # alert at the first run that finds a problem
    recovered_after_healthy_for: 0s     # the problem is over at the first healthy run
    checks:
      no_security_update_waits:
        apt_security_updates:
          fail_when: a_security_update_waits_to_be_installed
        alert_severity: warn
      no_fix_waits_that_only_ubuntu_pro_delivers:
        ubuntu_pro_security_fixes:
          fail_when: the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers
        alert_severity: warn
      node_needs_no_reboot:
        reboot_after_updates:
          fail_when: the_node_must_reboot_to_use_installed_updates
        alert_severity: warn
      release_gets_security_fixes:
        distribution_release_security_support:
          security_support: regular
          fail_when_security_support_ends_within: 180d
        alert_severity: warn
  known_exploited_flaws:
    check_every: 6h                     # each run downloads CISA list and asks OSV.dev
    time_limit_per_run: 5m              # a run takes a few minutes
    alert_after_failing_for: 5h         # a finding alerts when the next run still finds it
    recovered_after_healthy_for: 0s
    checks:
      no_installed_package_has_a_known_exploited_flaw:
        known_exploited_vulnerabilities_in_installed_packages:
          fail_when: an_installed_package_is_affected
        alert_severity: warn
```

The node needs the Ubuntu Pro client, as
[`ubuntu_pro_security_fixes`](../reference/modules/ubuntu_pro_security_fixes.md#set-up)
says. Each affected source package sends its own alert. The module page
says what you can do about a finding.

## If something is different

- **A check is `unknown`, and an alert says `MMWatchdog cannot check`.**
  The message names what the node lacks. The messages of a Debian node
  without the setup of steps 1 to 3:

  ```text
  Warning: app01: MMWatchdog cannot check node_software/no_security_update_waits
  apt has no usable successful-refresh stamp at /var/lib/apt/periodic/update-success-stamp, so MMWatchdog cannot know which updates wait: have apt write this stamp after each successful refresh; the module page apt_security_updates, section "Set up", shows how (mmwatchdog describe-module apt_security_updates)
  MMWatchdog has not been able to run the check since 07:05 UTC
  ```

  ```text
  no program on this node writes /run/reboot-required when a new kernel is installed, so MMWatchdog cannot know whether the node needs a reboot: install the package unattended-upgrades
  ```

  ```text
  MMWatchdog cannot read /usr/share/distro-info/debian.csv, which holds the dates of the releases: install the package distro-info-data
  ```

  Do the step that the message names, then run
  `sudo mmwatchdog check-now node_software --count-as-scheduled-run` on the
  node. The destinations get `MMWatchdog can check ... again`.
- **The security-update check became `unknown` after weeks of working.**
  The last refresh that worked is more than 7 days old, and the message
  gives its age. Reason: after a week, "no update waits" would be a guess.
  Check that the daily refresh runs:
  `systemctl status apt-daily.timer` on Debian and Ubuntu, and that the
  node still reaches its repositories with `sudo apt-get update`.
- **The node installs its security updates by itself,** such as with
  `unattended-upgrades` and automatic updates turned on. Then a waiting
  update is normal for up to a day. Give the update check its own waiting
  time, longer than the time between automatic installations:

  ```yaml
        no_security_update_waits:
          apt_security_updates:
            fail_when: a_security_update_waits_to_be_installed
          alert_severity: warn
          alert_after_failing_for: 3d     # the automatic installation has stopped working
  ```
- **The known exploited flaws check is `unknown`.** The node runs neither
  Debian nor Ubuntu, or it cannot reach `raw.githubusercontent.com` or
  `api.osv.dev`; the message says which. On a node behind a firewall that
  allows hosts by name, allow those two hosts. For one run, the check is
  also `unknown`, with `has not run yet`, for a few minutes after
  MMWatchdog starts, until its first run ends.
- **The check for known exploited flaws sends the node's package names.**
  Each run sends OSV.dev the names of the node's source packages and the
  release, such as `Debian:13`, but no installed versions. CISA's list is
  downloaded whole. So neither service learns whether the node is patched.
- **The run failed at `validate_config`.** The output names the file, the
  line, and the fault. A module written without its `fail_when` reads:

  ```text
  mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/node_software.yml:10:9: check_groups.node_software.checks.no_security_update_waits.apt_security_updates: expected a mapping, found nothing
  ```

  Fix the file and run `mmx run hub_and_standalone` again. Nothing changed
  on the node.
- **The project's nodes run different distributions.** Each node gets
  every file in `files/hub_and_standalone/check_groups/`, and a check for
  another distribution stays `unknown` and sends cannot-check alerts. Give
  each distribution its own configuration directory, such as
  `files/rocky_nodes/`, and select it with `mmwatchdog_config_source_dir`
  on the node or on an MMX inventory group, as
  [Deploy a fleet](set-up-a-fleet.md) shows.
- **The node runs Arch Linux.** No security check of MMWatchdog works
  there. [Limits on each distribution](../reference/limits-on-each-distribution.md#arch-linux)
  says why and what you can do.
- **A severe flaw has a fix at another distribution, but not yet at
  yours.** No check alerts until your distribution publishes its fix,
  unless the flaw is on CISA's list.
  [Limits on each distribution](../reference/limits-on-each-distribution.md#on-every-distribution)
  gives a measured example.
- **You change a node that MMX did not install.** Write the check group in
  `/etc/mmwatchdog/check_groups/` on the node, then run
  `sudo mmwatchdog validate` and `sudo mmwatchdog reload`.

## Related pages

- [Add a check](add-a-check.md)
- [Decide when a check alerts](decide-when-a-check-alerts.md)
- [Pause alerts during planned work](pause-alerts-during-planned-work.md),
  for the reboot.
- [Limits on each distribution](../reference/limits-on-each-distribution.md)
- The module pages of each check, listed in [Choose the checks](#choose-the-checks).
