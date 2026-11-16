# Limits on each distribution

MMWatchdog is tested on the
[target distributions](../changing-mmwatchdog/testing.md#every-target-distribution). Most checks work
the same on all of them. This page is for a person who runs one of them: it
says what MMWatchdog cannot do there, why, what would change it, and what you
can do now. It does not advise which distribution to choose. Each cause is
named as it was measured, with the date and the source. A cause on
MMWatchdog's own side says so. On ARM64, the tests run on Debian and Ubuntu
only so far; the other distributions are tested on AMD64.

The page covers the limits that remain on a distribution. Which check answers
which question on each distribution is in
[Security checks on each distribution](../how-to/watch-security-updates.md).

## On every distribution

### While a fix has not been published, no check alerts: one example

- **Limited:** while a distribution has not published a fix, no check of
  MMWatchdog alerts. [`apt_security_updates`](modules/apt_security_updates.md),
  [`dnf_security_updates`](modules/dnf_security_updates.md), and
  [`zypper_security_patches`](modules/zypper_security_patches.md) stay
  healthy, because no fixed version waits to be installed.
  [`known_exploited_vulnerabilities_in_installed_packages`](modules/known_exploited_vulnerabilities_in_installed_packages.md)
  fails only for a flaw on CISA's list of known exploited vulnerabilities.
- **Cause, one dated example:** CVE-2026-98432, a flaw in the Linux kernel
  with a CVSS score of 9.8, which Ubuntu's CVE page rates critical and dates
  2026-10-15. Each distribution's own advisory published its fixed kernel on
  these dates, read on 2026-11-14:

  | Distribution | Advisory | Published |
  |---|---|---|
  | Debian 13 | DSA-6568-1 | 2026-10-16 |
  | Debian 12 | DLA-4841-1 | 2026-10-17 |
  | Fedora 45 | FEDORA-2026-e96bbfedfe (kernel 7.2.11, which contains the upstream fix of 7.2.10) | 2026-10-16 |
  | Rocky Linux 9 | RLSA-2026:93412 | 2026-11-03 |
  | AlmaLinux 10 | ALSA-2026:93413 | 2026-11-09 |
  | openSUSE Leap 16.1 | openSUSE-SU-2026:22309-1 | 2026-11-09 |
  | Ubuntu 24.04 and 26.04 | USN-9022-1 and USN-9018-1 | 2026-11-10 |
  | Arch Linux | none: the tracker has no entry; Arch's kernel follows upstream releases | none |

  On 2026-11-14, the flaw was not on CISA's list, so on each distribution no
  check of MMWatchdog alerted until that distribution's fix was published:
  18 days after Debian's on Rocky Linux 9, 24 days on AlmaLinux 10 and
  openSUSE Leap 16.1, and 25 days on Ubuntu. Canonical's post about the flaw
  on its own blog says that Ubuntu's fixes waited for its kernel
  certification, tests "on more than 2,000 certified servers and devices",
  and were in the `-proposed` pocket before that. One flaw is one example,
  not a measure of how fast any distribution ships fixes in general.
- **What would change it:** an earlier fix from the distribution, or the flaw
  joining CISA's list, after which
  `known_exploited_vulnerabilities_in_installed_packages` fails while the
  node is affected, with `no fix yet` as the reason.
- **What you can do now:** read your distribution's security notices for
  critical flaws, and apply a mitigation while the fix waits; for this flaw,
  Ubuntu's CVE page gave one.

## Debian 12 and 13

No known limit beyond the one
[on every distribution](#on-every-distribution), as of 2026-11-14.

## Ubuntu 24.04 and 26.04

### Some security fixes come only with Ubuntu Pro

- **Limited:** [`apt_security_updates`](modules/apt_security_updates.md)
  sees only the fixes that apt on the node can install. Many security fixes
  of the packages of Ubuntu's Universe component are in Ubuntu Pro's ESM Apps
  service only, so a node without Ubuntu Pro never gets them, and
  `apt_security_updates` stays healthy.
- **Cause:** Ubuntu delivers those fixes through Ubuntu Pro, Canonical's
  subscription, not through its free archive.
- **What would change it:** the fixes reaching Ubuntu's free archive.
- **What you can do now:** add
  [`ubuntu_pro_security_fixes`](modules/ubuntu_pro_security_fixes.md), which
  fails while a node misses such a fix, or attach the node to Ubuntu Pro.

### A severe fix waited until 2026-11-10: one example

Ubuntu published its fix of CVE-2026-98432 on 2026-11-10, 25 days after
Debian's. See [While a fix has not been published, no check alerts](#while-a-fix-has-not-been-published-no-check-alerts-one-example).

## Rocky Linux 9

### A severe fix waited until 2026-11-03: one example

Rocky Linux 9 published its fix of CVE-2026-98432 on 2026-11-03, 18 days after
Debian's. See [While a fix has not been published, no check alerts](#while-a-fix-has-not-been-published-no-check-alerts-one-example).

### No check for known exploited flaws

- **Limited:**
  [`known_exploited_vulnerabilities_in_installed_packages`](modules/known_exploited_vulnerabilities_in_installed_packages.md)
  is unknown.
- **Cause:** the vulnerability data that Rocky Linux publishes through
  [OSV.dev](https://osv.dev), measured on 2026-11-14 through OSV.dev's API and
  downloadable files:
  - It holds advisories only: every affected range ends with a fixed version,
    and no record says "affected, no fix yet". So the check could never
    report a flaw without a fix, and its findings would be the waiting fixes
    that [`dnf_security_updates`](modules/dnf_security_updates.md) already
    alerts on.
  - The data of `Rocky Linux:9` had no record of three flaws on CISA's list:
    CVE-2024-6387 in `openssh`, CVE-2023-4911 in `glibc`, and CVE-2024-1086
    in `kernel`. The data of `AlmaLinux:9` had a record of each:
    ALSA-2024:4312, ALSA-2023:5453, and ALSA-2024:2394.
  - Rocky Linux 9.8 and 9.9 share the ecosystem `Rocky Linux:9`. Advisory
    RLSA-2026:85958 fixed CVE-2026-59997, CVE-2026-59998, and CVE-2026-60000
    in `openssh` `9.9p1-13.el9_8.rocky.0.1` for Rocky Linux 9.8, and
    RLSA-2026:97866 lists the same flaws with the 9.9 fix. Asked about
    `openssh-clients` `9.9p1-13.el9_8.rocky.0.1`, OSV.dev answered that
    RLSA-2026:97866 affects it, although the 9.8 fix is installed.
- **What would change it:** data that records each flaw of each release,
  also the unfixed ones.
- **What you can do now:** let `dnf_security_updates` alert on every waiting
  security update, and install the updates soon; keep the node on the current
  minor release, so that the fixes of the release reach it.

### Docker, MySQL 8.4, MongoDB, and pgBackRest are not tested

- **Limited:** [`docker_container`](modules/docker_container.md), the MySQL
  8.4 part of [`mysql`](modules/mysql.md), the official extra module
  [`mongodb`](../../official_extra_modules/mongodb/README.md), and
  [`pgbackrest`](modules/pgbackrest.md) are not tested on this distribution.
  They read the same programs as elsewhere and are expected to work.
- **Cause:** the distribution's repositories have no Docker and no
  pgBackRest, and the tests run MySQL and MongoDB in Docker.
- **What would change it:** tests that install these programs from their
  makers' own repositories.
- **What you can do now:** nothing more than on another distribution; the
  checks report what they find.

## AlmaLinux 10

### A severe fix waited until 2026-11-09: one example

AlmaLinux 10 published its fix of CVE-2026-98432 on 2026-11-09, 24 days after
Debian's. See [While a fix has not been published, no check alerts](#while-a-fix-has-not-been-published-no-check-alerts-one-example).

### No check for known exploited flaws

- **Limited:**
  [`known_exploited_vulnerabilities_in_installed_packages`](modules/known_exploited_vulnerabilities_in_installed_packages.md)
  is unknown.
- **Cause:** the vulnerability data that AlmaLinux publishes through
  OSV.dev holds advisories only, measured on 2026-11-14: every affected range
  ends with a fixed version. So the check could never report a flaw without a
  fix, and its findings would be the waiting fixes that
  [`dnf_security_updates`](modules/dnf_security_updates.md) already alerts
  on.
- **What would change it:** data that records each flaw of each release,
  also the unfixed ones.
- **What you can do now:** let `dnf_security_updates` alert on every waiting
  security update, and install the updates soon.

### Docker, MySQL 8.4, MongoDB, and pgBackRest are not tested

The same as on [Rocky Linux 9](#docker-mysql-84-mongodb-and-pgbackrest-are-not-tested).

## Fedora 45

### No check for known exploited flaws

- **Limited:**
  [`known_exploited_vulnerabilities_in_installed_packages`](modules/known_exploited_vulnerabilities_in_installed_packages.md)
  is unknown.
- **Cause, on MMWatchdog's side:** the check is unknown on the whole Red Hat
  family, whose measured data is described under
  [Rocky Linux 9](#rocky-linux-9). Fedora's own data was not measured.
- **What would change it:** a measurement that shows Fedora's data records
  each flaw of each release, also the unfixed ones.
- **What you can do now:** let
  [`dnf_security_updates`](modules/dnf_security_updates.md) alert on every
  waiting security update, and install the updates soon.

### pgBackRest is not tested

- **Limited:** [`pgbackrest`](modules/pgbackrest.md) is not tested on this
  distribution, and is expected to work.
- **Cause:** Fedora's repositories have no pgBackRest.
- **What would change it:** a test that installs pgBackRest from its makers'
  repository.
- **What you can do now:** nothing more than on another distribution.

## Arch Linux

### No security-update check

- **Limited:** MMWatchdog has no check of waiting security updates on Arch
  Linux, and
  [`known_exploited_vulnerabilities_in_installed_packages`](modules/known_exploited_vulnerabilities_in_installed_packages.md)
  is unknown.
- **Cause:** pacman has no security data to read. Measured on 2026-11-14:
  pacman's package databases, such as `core.db`, hold for each package its
  name, version, description, sizes, checksums, signature, and dependencies,
  and no field about security; pacman has no option about security. Arch
  Linux's security data lives apart, in the Arch Security Team's tracker at
  `security.archlinux.org`, which the tool `arch-audit` reads. The tracker's
  groups referenced 2,789 CVEs from 2021, but 45 from 2023, 13 from 2024, 79
  from 2025, and 5 from 2026, so a check built on it would miss most recent
  flaws.
- **What would change it:** pacman, or a maintained Arch Linux source,
  recording the flaws of current packages.
- **What you can do now:** upgrade often, with `pacman -Syu`: a rolling
  release ships its fixes as upgrades of the whole system. If you want the
  tracker's signal anyway, run `arch-audit -u`, which lists the installed
  packages that have a tracked flaw and an upgrade, from a
  [`shell`](modules/shell.md) check.

### No reboot check

- **Limited:** [`reboot_after_updates`](modules/reboot_after_updates.md) is
  unknown.
- **Cause, on MMWatchdog's side:** MMWatchdog knows how Debian, Ubuntu, the
  Red Hat family, and SUSE say that a node needs a reboot, and has no reader
  for Arch Linux.
- **What would change it:** a reader for a signal that Arch Linux gives.
- **What you can do now:** reboot after an upgrade that brings a new kernel.

[`distribution_release_security_support`](modules/distribution_release_security_support.md)
is unknown too, which is no limit: a rolling release has no end of support.

## openSUSE Leap 16.1

### A severe fix waited until 2026-11-09: one example

openSUSE Leap 16.1 published its fix of CVE-2026-98432 on 2026-11-09, 24 days after
Debian's. See [While a fix has not been published, no check alerts](#while-a-fix-has-not-been-published-no-check-alerts-one-example).

### No check for known exploited flaws

- **Limited:**
  [`known_exploited_vulnerabilities_in_installed_packages`](modules/known_exploited_vulnerabilities_in_installed_packages.md)
  is unknown.
- **Cause:** the vulnerability data that openSUSE publishes through
  OSV.dev, measured on 2026-11-14: the ecosystem `openSUSE:Leap 16.1` held 3
  advisories, while `openSUSE:Leap 16.0` held 1,242. The data of 16.0 does not
  fit 16.1, whose packages are other builds. It also holds advisories only:
  every affected range ends with a fixed version.
- **What would change it:** OSV.dev's data for Leap 16.1 filling in, or a
  reader for SUSE's own OVAL files at
  `ftp.suse.com/pub/projects/security/oval/`, which cover Leap 16.1, also the
  flaws without a fix, in another format and about 10 MB to download for each
  run.
- **What you can do now:** let
  [`zypper_security_patches`](modules/zypper_security_patches.md) alert on
  every waiting security patch, and install the patches soon.

### pgBackRest with PostgreSQL 18 is not tested

- **Limited:** [`pgbackrest`](modules/pgbackrest.md) is not tested on this
  distribution.
- **Cause:** openSUSE Leap 16.1's pgBackRest 2.53.1 does not support its
  PostgreSQL 18, measured on 2026-11-14 in the test.
- **What would change it:** a newer pgBackRest in openSUSE Leap.
- **What you can do now:** use a pgBackRest that supports your PostgreSQL.

### MariaDB's client against MySQL 8.4 is not tested

- **Limited:** a [`mysql`](modules/mysql.md) check of a MySQL 8.4 server
  through MariaDB's client is not tested; MySQL's own client is.
- **Cause:** openSUSE Leap 16.1's MariaDB client 12.3 lacks the plugin
  `caching_sha2_password`, which MySQL 8.4 logins use, measured on 2026-11-14
  in the test.
- **What would change it:** that plugin in openSUSE's MariaDB client.
- **What you can do now:** install MySQL's own client on nodes that check a
  MySQL 8.4 server.
