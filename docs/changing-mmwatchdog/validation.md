# Validation

This record covers the current build only. Rerun the relevant suites in
[testing.md](testing.md) after a change and replace this record with the new
results.

## Current build

The release checks for the first release (2026-11-16), with Go 1.27.4 and
the MMX code released with MMWatchdog:

| Run or check | Result |
| --- | --- |
| Static analysis, `scripts/static-analysis.sh` | `go vet`, `staticcheck`, `errcheck`, `exhaustive`, and `nilaway` report nothing; `govulncheck`, on the upstream versions of the copies, finds no vulnerability that MMWatchdog's code reaches. With Go 1.27.3 it found GO-2026-6859 in `net/http` (fixed in Go 1.27.4), and `exhaustive` found a switch on the distribution family without the Debian family |
| Go tests, also with the race detector, on a Mac | All passed |
| Every suite on every [target distribution](testing.md#every-target-distribution), AMD64, Incus on the test servers | All passed on all nine. Every earlier failure was in the test, not in MMWatchdog. In a first run, under a heavy load from other test runs on the same servers, InnoDB counted only 147 to 295 of the 300 changes that grow its history list (the scenario now asserts only the check's limit of 100), `mongosh` took 4.8 seconds of a 5-second limit, and the web service example's status page answered after its 500 ms limit; the last two passed unchanged at a normal load. In the next run, `zpool wait -t resilver` returned before the resilver started, and Postfix greeted after the `ssh` check's read had ended; both scenarios now wait for that state, and passed |
| ARM64, Hetzner `cax11` servers | `native`, `deployment`, and `node` passed on Debian 13; `native` and `deployment` passed on Ubuntu 26.04. On Ubuntu 26.04 the `node` suite's Ubuntu Pro step ran longer than 4 minutes; see below |
| [Fuzz tests](testing.md#fuzz-tests-for-a-release), 10 minutes each on an AMD64 test server | All passed, with no failing input: `FuzzCheckGroupConfigurationFile`, 192,011 inputs; `FuzzNodeSettingsFile`, 753,506; `FuzzModuleDescription`, 4,580,739; `FuzzExecutableModuleOutput`, 15,717,002; `FuzzStarlarkModuleResult`, 7,978,534; `FuzzJoinRequestOnTheHub`, 7,428,716; `FuzzNodeStatusReportOnTheHub`, 2,657,571; `FuzzMMHotlineReceipt`, 11,897,340 |
| `TestRealServiceMMHotline` against MMHotline's code released with MMWatchdog, with MMHotline's fake sending services | Passed: the `info` alert and its retry made one alert, the `sos` alert opened an incident, and the `resolved-sos` resolved it two minutes later |
| Release archives from a fresh clone, with `scripts/release.py 0.1.0 --output-dir .` | Both archives and `SHA256SUMS` built, and `sha256sum --check` passed |

The Ubuntu Pro client on ARM64: `pro security-status --format json` took
37 to 43 seconds on a Hetzner `cax11` server (2 ARM64 cores, Ubuntu 26.04,
156 waiting updates), as root and as the account `mmwatchdog`, with and
without network access, and also two runs at once. On a fresh `cax11`
server, `mmwatchdog check-now --without-daemon` ran
`ubuntu_pro_security_fixes` in 43 seconds and reported `ok`. Inside the
`node` suite on the same kind of server, the same run did not end within
4 minutes; the cause is not found. The real alert destinations
(Slack, Telegram, Discord, and Pushover) were not run again: the four
modules have not changed since the run under
[real alert destinations](#real-alert-destinations).

A server that systemd does not run (2026-11-14): on an Alpine Linux 3.22
container with OpenRC, `mmwatchdog daemon` refused to start, with exit code 4
and the message `this node does not run systemd: the directory
/run/systemd/system, which exists on every machine that systemd started, is
not there. ...`. The native and deployment suites, which start the daemon
under systemd, passed on Debian 13 and Fedora 45 with the same build.

MMWatchdog on Fedora 45 (2026-11-14): dnf 5.4, in an Incus VM with SELinux
enforcing. Every suite passed on Fedora 45, and in one run of all
suites on all nine target distributions (Debian 12 and 13, Ubuntu 24.04 and
26.04, Rocky Linux 9, AlmaLinux 10, Fedora 45, Arch Linux, and openSUSE Leap
16.1). The node suite ran against the real dnf 5: the security listing with
and without a waiting update, a refresh that failed with the servers blocked
(dnf 5 then touches no package data, and the check stays unknown), the limits
of each repository (6 hours, 7 days, and 14 days of `metadata_expire`) with
package data aged to 20 days, `dnf5-plugins` taken away and installed again,
the real reboot answer after the C library was installed again, and
`SUPPORT_END`. The services suite ran on Fedora with Dovecot 2.4, Valkey,
Docker from `moby-engine`, and MySQL's own client with `openssl3-libs`. Not
run on Fedora 45: pgBackRest, because its repositories have none. Not
verified: how good the vulnerability data of `known_exploited_vulnerabilities_in_installed_packages`
is for Fedora (the check stays unknown there), that the `mmwatchdog` account
reads dnf 5's cache with no home directory, and Amazon Linux (unknown by design).

MMWatchdog on the Red Hat family (2026-11-14): Rocky Linux 9.8 and AlmaLinux
10, in Incus VMs with SELinux enforcing. Every suite passed
on both, and on Debian 12 and 13, Ubuntu 24.04 and 26.04, and Arch Linux, in
the one run of all suites on all eight target distributions at that time. The node suite
ran against the real `dnf`: a package chosen at run time and installed in the
version before its security update, package data aged to nine days, a refresh
that failed with the servers blocked (dnf touches its stamp then, and the check
stays unknown), the real reboot hint after the C library was installed again,
and `SUPPORT_END` of `/usr/lib/os-release`. The mapping from each enabled
repository to its cache directory is compared on the guest. Not run on Rocky
Linux 9 and AlmaLinux 10: Docker, the MySQL 8.4 and MongoDB scenarios that use
it, and pgBackRest, because their repositories have none of them. Not verified:
CentOS Stream and RHEL themselves, `dnf-automatic`, and ARM64 (later steps).

SUSE checks (2026-11-14): `zypper_security_patches` and the SUSE parts of
`reboot_after_updates` and `distribution_release_security_support` passed the
node suite on all six target distributions, and every suite passed on
openSUSE Leap 16.1 and Debian 13. On openSUSE Leap the node suite ran against
the real `zypper`, a package chosen at run time, the real reinstall of the
kernel package, and the real product file. Not verified: a SUSE node with a
repository that zypper skips (exit code 106) in a real run (Go tests only),
and SUSE Linux Enterprise.

Arch Linux and openSUSE Leap (2026-11-14): every suite
passed on all six target distributions, Debian 12 and 13, Ubuntu 24.04 and
26.04, Arch Linux, and openSUSE Leap 16.1, in Incus VMs
(`web_service_fleet_example` on Ubuntu 24.04 only, as always). On Arch Linux
and openSUSE Leap, the checks of the distributions' own servers ran against
PostgreSQL 18.6, MariaDB 13.0 and 12.3, Dovecot 2.4, Postfix, and Valkey in
the place of Redis. `verified_tls` without
`database_server_certificate_authority_file` used `/etc/ssl/ca-bundle.pem`
on openSUSE Leap and failed for a certificate of an unknown authority. The
checks that did not know those distributions were unknown there, each with
its reason. Not run there: pgBackRest on openSUSE Leap, whose pgBackRest
2.53.1 does not support its PostgreSQL 18, and MariaDB's client 12.3 of
openSUSE Leap against MySQL 8.4. Arch Linux and openSUSE Leap ran on AMD64
only: Hetzner Cloud has no image of either. Before this date, "every target
distribution" below means the four Debian and Ubuntu releases.

The fixes of 2026-11-07 for alerts that could be lost or wrong passed the
node and fleet suites on every target distribution. They cover: alerts sent
from memory while `state.db` cannot be written; a panic in a check or a
delivery; a new webhook token or email password that keeps the queue; a
filesystem that failed and is no longer mounted; a cause that reaches no
destination; a machine without a host name; and a hub's settings with a
field this node does not know. In the fleet suite, a new webhook token on the
hub reached the joined node and was not counted as another destination. The
cases themselves are proved by unit tests, each of which failed on the
earlier code. No guest test fills a disk.

The [`postgresql`](../reference/modules/postgresql.md) reading
`transaction_ids_used_before_wraparound_percent` (2026-11-05) passed the
services suite on every target distribution, against the distribution's own
PostgreSQL server. The reading was present, in percent and between 0 and 20,
for a check through the local socket, for a check with `login_role`, and on
a streaming replica. A check with the limit 0 failed and a check with the
limit 50 was healthy. A server near wraparound is not tested: the suite
cannot make a server use about a billion transaction IDs.

The [`distribution_release_security_support`](../reference/modules/distribution_release_security_support.md)
check with `security_support` (2026-11-03) passed the node suite on every
target distribution. For `regular` and for the distribution's longer
support, `debian_lts` or `ubuntu_pro`, the check gave the status and the
date that the suite read from the guest's own `distro-info-data`, including
the ended regular support of Debian 12, and the value of the other
distribution was unknown.

The [`ubuntu_pro_security_fixes`](../reference/modules/ubuntu_pro_security_fixes.md)
check (2026-11-03) passed the node suite on every target distribution. On
Ubuntu 24.04 and 26.04, with `ubuntu-pro-client` 37.2 and no Ubuntu Pro, the
suite installed a package whose newer version only ESM Apps has, and the
check reported the package's source package as a finding; after the suite
removed the package, the check was healthy. On Debian 12 and 13 the check
was unknown. `pro security-status --format json` ran as the account
`mmwatchdog` in about 2 seconds, also without network access. A node
attached to Ubuntu Pro is not tested.

Until 2026-10-25, the target distributions also held Rocky Linux 9 and
AlmaLinux 10, and a run of a commit before that date names them. MMWatchdog
now runs on Debian and Ubuntu only
([every target distribution](testing.md#every-target-distribution)).

The [`mmhotline` destination](../reference/modules/mmhotline.md) (2026-10-23) sends
to one address, alert source ID, and signing secret for each team's
MMHotline. The Go tests pass against a fake MMHotline built from
MMHotline's alert intake contract. The fleet suite runs a fake MMHotline
over HTTP on the hub guest. The fleet suite
passed on every target distribution, and the acceptance suite on Debian 13
and Rocky Linux 9. The fleet suite's first run on Debian 12,
Ubuntu 24.04 and 26.04, and AlmaLinux 10 failed in the suite itself.
The fleet suite took the first check-in listing the joined node,
but the check-in still listed maintenance from the preceding deploy.
The fleet suite now waits until a check-in lists the
maintenance window opened by the fleet suite. `TestRealServiceMMHotline`
([real services before a release](testing.md#real-services-before-a-release))
passed on a Mac against a real MMHotline, built from MMHotline's
development build of 2026-10-23 and run on loopback with a test alert source: MMHotline accepted
the signed `info` alert and its retry as one alert, at `info` and without an
incident. MMHotline's alert source page showed the check-in. An alert
without a title got the signed `400`, and a wrong signing secret the `401`.
With the SOS step, `TestRealServiceMMHotline` passed on 2026-10-24
against MMHotline's development build of 2026-10-24
on loopback, with the real Pushover service and one person on call. The
`sos` alert opened an incident and reached the person's phone as a Pushover
emergency alert titled
`[SOS] mmwatchdog-real-service-test: real_service_test/mmhotline_sos failed`,
which sounded with the phone on silent and asleep. The person's
acknowledgement in Pushover claimed the incident 12 seconds later. The
`info` alert, MMHotline's claim result, and the `[Resolved SOS] ...` of the
`resolved-sos` two minutes later arrived without a sound, and the
`resolved-sos` resolved the incident opened by the test's `sos` alert.
The check-ins every 30
seconds kept MMHotline from warning during the test. Pushover's repeat of
an emergency alert every 30 seconds did not show, because the person
acknowledged it at once.

Since a change of 2026-10-22, MMWatchdog prints no time for a check that has not run yet in
`mmwatchdog fleet`, which showed `0001-01-01T00:00:00Z` right after a join
when MMX deployed MMWatchdog on two test servers. The Go tests of
`cmd/mmwatchdog` and `internal/incidents` pass.

On 2026-10-22, MMWatchdog moved to Go 1.27.3, which fixes `GO-2026-6809`
in Go's `crypto/x509`. A change just before the move to Go 1.27.3 checks the close
of the hub-outage alerter cursor, which `errcheck` found. With Go 1.27.3, the Go tests pass,
also with the race detector for `internal/fleet`, `internal/daemon`,
`internal/config`, and `cmd/mmwatchdog`; static analysis reports nothing;
and every suite passed on Debian 13 and Rocky Linux 9 at the first run, in
at most 549 seconds on one server.

Two changes of 2026-10-22 run the email scenarios against
Mailpit 1.31.6 on loopback in each guest, in place of the stand-in mail
server, and remove the official extra modules `pagerduty` and
`jira_service_management`; the fleet suite's scenario of one incident per
check across the fleet now uses `pushover`. With both changes, the Go tests and
the Python unit tests pass, and `acceptance` and `fleet` passed on all six
target distributions at the first run: 12 suite runs, at most 250 seconds
on one server. Before these successful suite runs, the Mailpit change
failed twice on the reference distributions, each time in the test code.
The reader looked for Mailpit's receive time in the wrong part of Mailpit's answer.
Rocky Linux and AlmaLinux guests have no `tar`. On Rocky Linux and AlmaLinux,
SELinux lets systemd run no program from `/var/tmp`, so the Mailpit binary
now goes to `/usr/local/bin`.

A change of 2026-10-22 adds the second hub-outage alerter node,
`send_silence_alerts_of_this_node_only_to_destinations`, and the read of
the fleet view by another program on the hub.
With that change, the Go tests pass,
also with the race detector for `internal/fleet`, `internal/daemon`,
`internal/config`, and `cmd/mmwatchdog`, and `fleet` passed on all six
target distributions at the first run.

A change of 2026-10-22 adds the dependency check script, continuous integration, and
two test fixes. With that change, every suite passed on all six target
distributions,
and `web_service_fleet_example` passed on Ubuntu 24.04: 62 suite runs, at
most 929 seconds on one server. In the first run, `node` failed on all six,
because an alert title in the filesystem scenario reached 121 characters.
After the fix, `node` passed on all six distributions. `services`
failed once on Debian 13, where the MongoDB container with a login exited
before answering, and passed on rerun. The scenario now prints the log
of the MongoDB container with a login when the container does not answer.
Static analysis reported nothing
except `GO-2026-6809` in Go's `crypto/x509`, which the move to Go 1.27.3
above fixes.

The interface review of 2026-10-19 brought required settings
without hidden defaults, explicit names for certificate authorities,
logins, and the skipped server certificate check, `mmwatchdog validate
--config-only`, quiet destination verification and destination health
notices, the permission rule for secret files, fleet incident keys scoped
by the hub's certificate, `validate_inputs` for modules, and renamed JSON
fields. Before the last names round of the interface review, static analysis reports nothing and the Go
tests pass. Under the stress script, 727 Go tests passed 40 runs each, 4
at once, with the race detector. Every suite except `soak` passed on all
six target distributions, and `web_service_fleet_example` passed on Ubuntu
24.04: 49 suite runs, at most 645 seconds on one distribution. The last
names round changed the security-update check, the severity of
one notice, and three JSON names. After the last names round, static analysis reports nothing,
the Go tests pass, `node` passed on all six distributions, and
`acceptance`, `fleet`, and `diagnostics` passed on Debian 13 and Rocky
Linux 9.

A change of 2026-10-20 applies the env, config, and dir naming table and deploys
with MMX's development build of 2026-10-19, which applies the table too.
With that change, static analysis reports
nothing and the Go tests pass. Every suite except `soak` passed on Debian
13 and Rocky Linux 9, and `web_service_fleet_example` passed on Ubuntu
24.04. `acceptance`, `resources`, and `deployment` first failed on a test
helper that still wrote the old key of a module description's example.
After the test helper was fixed, `acceptance`, `resources`, and `deployment` passed.

Validated on 2026-10-16, with Go 1.26.8. The 2026-10-16 validation is step 2
of the release plan: the release-checklist parts needing no outside account.
The [testing guide](testing.md) holds the commands for the 2026-10-16
validation runs.

A later change of 2026-10-16 moved MMWatchdog to Go 1.26.9, which fixes 10 advisories of
Go 1.26.8's standard library that MMWatchdog's code reaches. With Go 1.26.9, static
analysis reports nothing, the Go tests pass, and every suite passed on
Debian 13 and Rocky Linux 9 at the first try, in 518 seconds.
The other runs below used Go 1.26.8.

Every Go dependency is a copy with unused files removed, in `third_party/`
(since 2026-10-16).
The symbols of every program and test binary matched before and after
pruning, except unused initializers. The
[implementation rules](implementation-rules.md#go-dependencies) own the
dependency rule; [the measured sizes below](#the-size-of-the-mmwatchdog-program)
show the reduction in the release programs.

The runs also found faulty tests, fixed without a product change: four
in static analysis, and two under stress, an
attempt counter that hashed the remaining time and a reused port. Test
time limits were raised so a busy machine never reaches the limits.
The load run's verdict was corrected.
The load run now fails on any refused alert.

| Run or check | Result |
| --- | --- |
| Static analysis, `scripts/static-analysis.sh`, on the commit that rewrote alert, status, and guide texts for a first-time reader (2026-10-27), with Go 1.27.3 | `go vet`, `staticcheck`, `errcheck`, `exhaustive`, and `nilaway` report nothing. The rewrite had moved two reads that NilAway reported. The two reads keep the reviewed exception. Reviewed exceptions are in `errcheck_excluded_functions.txt`, `staticcheck.conf`, and comments beside the code. `govulncheck`, on the upstream versions of the copies, finds no vulnerability that MMWatchdog's code reaches |
| [Fuzz tests](testing.md#fuzz-tests-for-a-release), 10 minutes each on the AMD64 test servers | All passed: `FuzzCheckGroupConfigurationFile`, 136,123 inputs; `FuzzNodeSettingsFile`, 195,255; `FuzzModuleDescription`, 2,081,485; `FuzzExecutableModuleOutput`, 4,464,171; `FuzzStarlarkModuleResult`, 1,451,100; `FuzzJoinRequestOnTheHub`, 1,422,829; `FuzzNodeStatusReportOnTheHub`, 374,922. `FuzzJoinRequestOnTheHub` and `FuzzNodeStatusReportOnTheHub` ran after the two tests were made to share one hub |
| Go tests under stress, [`tests/run_go_tests_under_stress.sh`](testing.md#tests-that-never-depend-on-the-machines-speed), on the 2026-10-16 build | 618 tests, 40 runs of every test, 4 at once, with the race detector on a loaded Mac: 0 failures and no race report |
| Every suite on every [target distribution](testing.md#every-target-distribution), AMD64, Incus on the test servers, on the commits that called the message to MMHotline a check-in, logged each attempt to deliver an alert, and rewrote alert, status, and guide texts for a first-time reader (2026-10-27) | All nine suites passed on Debian 12 and 13 and Ubuntu 24.04 and 26.04, and the example suite `web_service_fleet_example` passed on Ubuntu 24.04. In the fleet suite, the hub-outage alerter's journal named the direct alert and resolution with the destination that accepted each. The first run's `resources` suite failed on Debian 12 and 13 and Ubuntu 24.04 and 26.04: the new end of a closed incident's title, "not known to be fixed", made one long title cut the unit's name as well as the check's. With the shorter "maybe not fixed" the title keeps the unit, and `resources` passed on Debian 12 and 13 and Ubuntu 24.04 and 26.04 |
| The node, fleet, and native suites on every [target distribution](testing.md#every-target-distribution), AMD64, Incus on the test servers, on the commits that made another program read the fleet view through a socket unit and made `known_exploited_vulnerabilities_in_installed_packages` read CISA's list and OSV.dev itself (2026-10-27) | All passed on Debian 12 and 13 and Ubuntu 24.04 and 26.04. In the fleet suite, an account read `mmwatchdog fleet --json` through a socket unit whose service runs as root without capabilities, and could not forget a node. In the node suite, the check asked the stand-in with the guest's real source package names only, page by page, in the release's ecosystems (both Ubuntu ecosystems on Ubuntu), left out a vendor package, and found the flaws built from the guest's own packages and the running kernel. The node suite's first run on Ubuntu 26.04 failed in the test, not the check. The reload and the test's own check had each run the check, so every recorded query appeared twice. The test now compares the queries as a set, and the node suite then passed on Debian 12 and 13 and Ubuntu 24.04 and 26.04 |
| The node suite on every [target distribution](testing.md#every-target-distribution), AMD64, Incus on the test servers, on the commit that added `distribution_release_security_support` (2026-10-26) | Passed on Debian 12 and 13 and Ubuntu 24.04 and 26.04 at the first try, in 143 seconds. On each guest the check read the real `distro-info-data` and gave the date that the test independently read by the column's name |
| `known_exploited_vulnerabilities_in_installed_packages` against the real CISA list and the real OSV.dev, on the change that made the check read them itself (2026-10-27), with `mmwatchdog check-now --without-daemon` on the lab's two Debian 13 servers and its Ubuntu 24.04 server | Healthy on all three: 296 source packages of `Debian:13` in 97 and 104 seconds, and 382 of `Ubuntu:24.04:LTS` and `Ubuntu:Pro:24.04:LTS` in 54 seconds. Before the check read CISA's list from CISA's copy on GitHub, `www.cisa.gov` answered HTTP 403 on all three servers |
| `known_exploited_vulnerabilities_in_installed_packages` against real CISA and OSV.dev with `mmwatchdog check-now --without-daemon`, on a Mac, with stand-ins for `dpkg-query` and `apt-cache` listing seven source packages of an early Debian 13 server | The whole run took 91 seconds, spent almost entirely on OSV.dev's pages of `linux`. The check found 7 flaws on CISA's list. CVE-2025-32463 affected `sudo` 1.9.16p2-1, and CVE-2025-48384 affected `git` 1:2.47.2-0.2. Corrected versions of `sudo` and `git` were available for installation. Five flaws affected the running kernel `linux` 6.12.38-1, with fixes in 6.12.48-1 to 6.12.95-1. A separate script read OSV.dev's records of the 35 flaws of `linux` on CISA's list and compared fixed versions by number. The five kernel flaws reported by the check affect 6.12.38-1. `openssh` 1:10.0p1-7, `bash`, `glibc`, and `openssl` were healthy |
| The services suite on every [target distribution](testing.md#every-target-distribution), AMD64, Incus on the test servers, on the commit that added `cache_hit_percent` and `wal_archive_lag_seconds` to `postgresql` and the module `pgbackrest` (2026-10-27) | Passed on Debian 12 and 13 and Ubuntu 24.04 and 26.04 at the first try, in 428 seconds, with each distribution's own PostgreSQL and pgBackRest. On each guest `cache_hit_percent` read from 0 to 100, through the local socket and through a login. With the server archiving through pgBackRest, `wal_archive_lag_seconds` stayed below its limit of 20 seconds, passed it while pgBackRest could not use its repository's archive, and fell below it again after the repair; with `archive_mode` off the check with that limit was unknown, and a check with `login_role` had no such reading. `pgbackrest` gave the age of a full backup taken minutes before, and failed with pgBackRest's own reason for a stanza that the repository does not have and for a repository whose archive pgBackRest could not read |
| ARM64, pairs of Hetzner `cax11` servers, on the 2026-10-16 build | `native`, `deployment`, and `node` passed on Debian 13 and Rocky Linux 9, in 393 seconds. Cleanup was verified |
| [Sustained soak](testing.md#release-and-sustained-test-evidence), one Hetzner AMD64 server, Debian 13, on the 2026-10-16 build | Passed in 138 seconds: 32 checks, 84 rounds, 4,064 events, 2 deliberate restarts, 3 completed incidents, 56 deliveries accepted, 8 rejected, 0 duplicates, and 0 queued at the end. Steady state RSS was 32.40 to 32.57 MB, storage 4.68 to 4.70 MB, descriptors and threads 14 throughout |
| [Large-fleet load run](testing.md#large-fleet-load-run), from the Mac, on the 2026-10-16 build | Passed with a Hetzner `ccx23` hub (4 CPUs, 15.6 GiB) and a generator server. 10,000 synthetic nodes joined (join p50 4.2 ms, p99 20.4 ms) and reported every 30 seconds (p50 about 5 ms, p99 17 to 26 ms in every phase, about 100,000 reports in the steady phase). No node failed two reports in a row, and the hub's daemon did not restart. Each change of 1,000 nodes arrived as 2 grouped messages: silence, return, `app/http` failed, recovered. The hub accepted all 1,000 alert posts of each failing phase. Hub CPU median 56%, max 70%; RSS max 307 MiB; cgroup memory max 858 MiB; `fleet.db` 31 MiB, `state.db` 17 MiB. Command medians: `mmwatchdog fleet` 271 ms, `mmwatchdog fleet --json` 346 ms, `mmwatchdog status --json` 274 ms. The first run failed: the generator's alerts lacked `fleet_incident_key`, required by the hub for one incident per check across the fleet, so the hub refused them with HTTP 400. The generator was then fixed. The servers from both runs were deleted, and the deletion was verified |
| Release archives from a clean checkout of the 2026-10-16 build | The two release archives built, and `SHA256SUMS` verifies the archives. `build.json` records the revision, `modified: false`, and Go 1.26.8. Each archive holds `manifest.json` and the license notices of Go and all 11 dependency modules. The [release checklist](design/README.md#release-checklist) owns the remaining tagged-build checks |
| Fleet suite with scenarios for hub storage failure, refused reports, and alert grouping across five synthetic nodes, on the 2026-10-16 build | The scenarios passed on Debian 13 and Rocky Linux 9, also within the run of every suite above |

### Real alert destinations

The [real service test](testing.md#real-services-before-a-release) ran on
a Mac on 2026-10-22 against Slack, Telegram, Discord, and Pushover, with
the test accounts of the Monkeymaxxing lab, and passed at its first run,
through the modules [`slack`](../../official_extra_modules/slack/README.md),
[`telegram`](../../official_extra_modules/telegram/README.md),
[`discord`](../../official_extra_modules/discord/README.md), and
[`pushover`](../../official_extra_modules/pushover/README.md).
A person looked at the Slack channel and at the phone. What the services
showed:

- **Messages.** Every service took the failure alert, its recovery, and
  the long alert.
  Telegram and Discord stored exactly the text that the
  module sent. In Slack the first line is plain text with the mention, and
  the check's output is a code block that shows `&`, `<`, `>`, `*`, `_`,
  backticks, and `~` as they are.
- **Limits count characters.** Telegram took 4,096 characters with 192
  emoji, 4,288 UTF-16 code units, in one try; Discord took 2,000
  characters with 91 emoji, 2,091 UTF-16 code units, in one try; Pushover
  took 1,024 characters with 49 emoji, 1,073 UTF-16 code units.
  Telegram, Discord, and Pushover count characters (code points), as the
  modules do. Slack took the long message, which the
  module cut to 4,000 characters and marked with `…`.
- **Mentions.** On Discord, the input `mention: "@here"` notified the
  channel, and `@everyone` in a check's output notified nobody. In Slack,
  `<!here>` showed as a mention.
- **Wrong keys.** Each service refused a wrong key, and the module's advice
  fits its answer: Slack `403 invalid_token`; Telegram `401 Unauthorized`,
  and `400 Bad Request: chat not found` for an unknown chat; Discord
  `401 Invalid Webhook Token (code 50027)`; Pushover `400 application
  token is invalid`. Each verification accepted the real keys,
  refused the wrong keys, and sent no message.
- **Pushover's emergencies across a fleet.** Two nodes sent an `sos` alert
  of one check, with the same tag. Pushover repeated the emergency alerts
  from the two nodes after 30 seconds. The closing alert's stop by tag
  answered `"canceled":2`. The receipts for the two nodes' emergency alerts
  then showed `expired: 1`. Neither emergency alert from the two nodes
  was delivered again in the next 40 seconds. A stop by a tag that no message has
  answers HTTP 200 with `"canceled":0`, and the closing message after it
  is delivered.

### Real use

Since 2026-10-22, MMWatchdog has watched a fleet of three servers beyond
the test guests: two Debian 13 dedicated servers, one of them the hub, and
an Ubuntu 24.04 virtual server. The two joined servers are the hub's
hub-outage alerters. MMX deploys all three with the release's playbooks, and the
alerts go to Slack and Telegram. What the fleet has shown so far:

- **Every check gives a real result** on the servers:
  three RAID 1 arrays,
  processor and drive temperatures, every filesystem on a disk (also a
  btrfs storage pool on a loop device), memory, processor, the SSH server,
  the clock, failed systemd units, out-of-memory kills, pending reboots,
  and waiting security updates. All three servers needed a reboot for new
  kernels; the hub sent the three `info` alerts to Slack with no delivery
  failure.
- **apt's success stamp on Debian.** On the Debian servers nothing wrote
  `/var/lib/apt/periodic/update-success-stamp`, although the daily refresh
  ran, so the security update check stayed unknown with advice that was
  already followed. Fixed on 2026-10-22: the advice names Ubuntu's package
  and the hook line for Debian. With the hook, `apt-get update` wrote the
  stamp, and the check found 3 waiting security updates. A server that
  got the hook after apt's last refresh had no stamp until that
  `apt-get update`, because the daily refresh waits a day after the last
  one; the check stayed unknown in the meantime, as the module page says.
- **The shipped deploy project.** The `examples/deploy` project's
  `join_hub` playbook gave every joined
  node the same configuration set, and every unchanged rerun reported
  three changes. Fixed on 2026-10-22: `mmwatchdog_config_source_dir` and
  exact staging copies. An unchanged rerun of the hub
  then reported no change and did not restart MMWatchdog.
  The deploy that restarted the hub opened a maintenance window on the hub
  right before the restart and closed the window after
  `mmwatchdog config` answered, and the joined servers, still on the
  earlier build, kept reporting to the updated hub.
- **Different configuration sets in one inventory.** MMX first refused the
  second node's directory inside the playbook's task block; a change to MMX on 2026-10-22
  fixed the refusal of the second node's configuration directory.
  Then the two joined servers got their own sets in one deploy,
  a test server with its RAID arrays and temperatures and a Git server with
  the Git server's Tailscale, each server in the server's node group. An unchanged rerun of
  `join_hub` reported no change.
- **Reboots for the new kernels.** Each server rebooted once, and every
  alert came as designed:
  - The Ubuntu server, a joined node, was back in about 22 seconds.
    The Ubuntu server sent a `rebooted` notice and resolved the pending
    reboot incident. The hub delivered the notice and resolution.
  - The joined Debian server rebooted under a whole-node maintenance
    window. The hub recorded `rebooted` and the resolved pending reboot
    without sending them, and sent the resolution once when the window
    closed. Right after boot, the clock check failed for about a minute
    until the time server answered, within the check group's 3-minute
    `alert_after_failing_for`.
  - The hub rebooted with no window and was silent for about 90 seconds.
    When the hub started, the hub sent a `rebooted` warning to Slack
    and Telegram. The two hub-outage alerters sent nothing, because the silence stayed
    under the hub-outage alerters' 3 minutes. No notification was queued or lost.
- **An outage of the hardware, and its charts.** During a long test run,
  the NVMe drives of the joined Debian server stayed above their 70 °C
  limit for 29 minutes, at most 80.85 °C. The hub sent the warning when
  the check had failed for the configured 5 minutes, and the recovery
  once the drives had been cool for 5 minutes, and recorded no queued
  notification and no
  delivery error. On the
  hub, `--chart` drew the hour containing the 29-minute temperature failure
  from the hub's real measurement history,
  for the joined server alone and for its node group, with the 30 readings
  beyond the limit as one continuous sequence on the chart. The chart
  also showed one processor reading of
  91 °C, beyond its limit of 90, which sent no alert. The chart of a
  30-day span printed at once. The
  [chart in Look back at measurements](../how-to/look-back-at-measurements.md#read-the-readings) shows the
  drive and processor temperature readings from the hour containing the failure.
- **Still to come:** the running time that exercises ordinary outages,
  deploys, and reminders, as the
  [release checklist](design/README.md#release-checklist) asks.

## Beta validation with real-world Monit work

Validated on 2026-10-16. The question was
whether MMWatchdog does the monitoring work that people do with Monit in real
setups. The material was 198 public Monit sources and one company's
production Monit setup. Of the public sources, 183, by 169 authors, are
configurations that run on servers, with 2,570 `check` statements.
Reading the source configurations produced 203 monitoring requirements,
written as scenarios. A scenario is one need, such as "restart
nginx when it stops answering on its port, and alert when it stays down".

Each scenario was first matched, on paper, against MMWatchdog's suites, examples,
and Go tests at the start of the 2026-10-16 validation. The
[coverage index](testing.md#real-world-monit-work) lists what proves each
kind of work. The comparison proved 121 scenarios.
Another 29 scenarios were uncommon work that a `shell` check or a module
of the project's own does, which counts as covered.
Another 22 scenarios were work that MMWatchdog leaves out by design,
such as a web page that starts and stops services.
Another 25 scenarios were built, but no suite showed the 25 scenarios on a real Linux system.
Among all the scenarios, 19 used MMWatchdog in ways the suites do not try, such as unusual combinations.

Five scenarios were common work that MMWatchdog did not do.
Each of the five scenarios is built now.
Among the five scenarios, `file` takes a directory, and `zfs_pool` and
`mysql` measure `days_since_last_scrub` and `innodb_history_list_length`.
The scenario requesting charts of measurement history was a question of scope.
The readings commands now draw charts with
[`--chart`](../how-to/look-back-at-measurements.md#read-the-readings).

The 25 scenarios that no suite showed, and the unusual scenarios, ran on
guests in two packs. Whole projects ran too, converted from the sources: the
company's setup first, then three public deployments. The suites test one feature at a time, with inputs of the suites' own.
A real project combines many features, and mistakes hide in the combinations. The guests were Incus
VMs on the test machines. The driver is now
[`monitoring_setup_scenarios.py`](testing.md#the-web-service-fleet-example)
in the test harness. The scenario driver deployed MMWatchdog on each
guest as [Install on one node](../how-to/install-on-one-node.md) teaches:
through MMX, with the example deployment project `examples/deploy`. Each
project and each pack then ran in steps. In the first step, every check had
to be healthy, and no alert could come for a stated time. Each later step
caused one failure for real, such as a killed process, a full filesystem, or
a stopped replica. The driver waited at a webhook receiver for each alert and the
destination that got the alert, then for
the recovery action, the recovery, and the
recovery alert. An alert that no step expected failed the step, and so did a
reboot that no step accounted for. The company's setup is now the example
[`web-service-fleet`](../../examples/web-service-fleet/README.md), and the
suite `web_service_fleet_example` runs the company's setup. On 2026-10-16,
`web_service_fleet_example` passed on Ubuntu 24.04, the initial healthy period without alerts and all 31
scenarios, in 400 seconds, of which 331 went to the scenarios.
The three public deployments and the two scenario packs stay
outside this repository. Each project ran on the distribution of the project's
source, and the packs ran on Debian 13 and Rocky Linux 9; the suites cover
all six target distributions.

| Project | Distribution | Result |
| --- | --- | --- |
| A company's web service fleet, from its production Monit setup, made anonymous, with one check of each kind that the setup has. Two guests: an application and data server, and a utility server that checks it over the network | Ubuntu 24.04 | All 32 steps passed. The steps cover a killed process that MMWatchdog starts again, two Redis servers, and a memcached server watched without restarts. The steps cover failed units and an out-of-memory kill by the kernel. Stopping nginx produces one alert for one cause. HTTP scenarios cover a `Host` header, a 404, basic authentication, and a status page. The steps cover a cannot-check alert and DNS answers that disappear and return. A memory leak causes an application restart. Resource scenarios cover CPU and I/O wait, and disk space and inodes at two levels. Storage scenarios cover a mount with a fix command, an `mdadm` mirror losing a disk, and a ZFS pool losing a device. Database scenarios cover a MariaDB replica that stops replicating, or falls further and further behind, database connections, and a real query. Application scenarios run a health command as the application's account, encode sound with the application's audio encoder, and check the deployed version. The steps cover SSH on the second server, raised to `sos`, with `warn` and `sos` reminders going to different destinations. Network scenarios cover ping and the two servers watching each other. TLS scenarios check a certificate at 21 and 7 days. SMTP check alerts go to one person |
| cedricpim/ansible-server: one person's server for mail, a DNS resolver, CalDAV, and file sync | Debian 13 | All 14 steps passed after the fix of equal waiting times below, which the cedricpim/ansible-server scenarios found. Before the equal-wait fix, 13 of 14 steps passed. The scenarios include a calendar site that loses its login, an nginx that uses too much CPU, a CalDAV server that hangs, Postfix killed while it carries the alert email, changed Postfix files, a broken resolver configuration that uses up its restarts, and the mail servers in planned maintenance |
| openforis/sepal: services as Docker containers | Debian 13 | All 8 steps passed. MMWatchdog failed a container in a restart loop while Docker still reported the container as running. The source's Monit check could not detect the restart loop. While Docker itself was down, MMWatchdog sent one alert |
| lsst-camera-dh/ccs-infrastructure: an observatory's control and data servers | Rocky Linux 9, with SELinux enforcing | All 12 steps passed, in each of two runs: CPU, load per core, swap, memory, the notice of a reboot, inodes through `every_disk_and_network_filesystem`, an NFS cluster filesystem that is full, unmounted, and not answering, ping, a link speed that drops from 1000 to 100 Mbit/s, and a saturated link |
| A pack of 28 scenarios of services, recovery, and alerts that no suite showed, or that were unusual | Debian 13 and Rocky Linux 9 | All 32 steps passed on Debian 13 and Rocky Linux 9 after the reminder fix below. The 28-scenario pack found the reminder defect. Before the reminder fix, 31 of 32 steps passed |
| A pack of 21 scenarios of the node, disks, files, hardware, operation, and the fleet that no suite showed, or that were unusual | Debian 13 and Rocky Linux 9 | All 23 steps passed on Debian 13 and Rocky Linux 9 |

The paper comparison and scenario runs found the defects listed below. Each listed defect now has a fix and a test:

- A Starlark module that called a server which accepts the connection and
  then sends nothing, through `http_request`, `tcp_exchange`, or
  `udp_exchange`, never got an error back. A module also got no error when the module's
  `run_command` program did not end. MMWatchdog ended the module at the time limit,
  so the run was unknown, and an unknown run never drives recovery: a hung
  service was never restarted. Now each request to a module that is not
  core carries `mmwatchdog_ends_the_module_after_milliseconds`, and each such call
  ends before the module's time limit by a tenth of the limit, at most one second before.
  The call returns the error "MMWatchdog ended the call shortly before the time limit of the run
  (time_limit_per_run)". The Starlark script then decides, so `apache_status` fails
  for an Apache that hangs. The last address of a network call
  now takes the run's own time limit: once on Debian 13, a race gave the
  error in Go's words instead of MMWatchdog's.
- `reboot_after_updates` on Debian and Ubuntu was healthy when nothing writes
  `/run/reboot-required`. Only a kernel hook writes `/run/reboot-required`.
  A minimal Debian has no kernel hook, so the node stayed healthy after every kernel update. Now
  the check is unknown on a node without a kernel hook, with a message to install
  `unattended-upgrades`; MMWatchdog knows the four hooks that write the file.
- `memcached` did not measure what the release plan requires. The `memcached` check now
  measures `cached_items`, and `cache_hits_per_second`,
  `cache_misses_per_second`, `cache_evictions_per_second`, and
  `cache_hit_percent` over the time since the previous `memcached` run, so a cold cache
  and a warm cache look different.
- Ten places where the documents did not say what the build does.
  The discrepancies include the `tls` measurement `expires_in`, whose name
  lacked its unit.
  The `tls` measurement is
  now `certificate_expires_in_seconds`, and the example of the `shell`
  module no longer uses `grep -q`, which can fail a healthy run under
  `pipefail`.
- With `restart_after_failing_for` equal to `alert_after_failing_for`, a
  restart could come at the run of another check, before the failing
  check's own run had opened its incident. When the restart fixed the
  check, no alert ever went out. A recovery command with
  `run_after_failing_for` equal to `alert_after_failing_for` also fixed
  the check before an incident opened, so no alert went out.
  Now MMWatchdog decides from the failing
  check's own latest run, so the action comes with the run that opens the
  incident, and the alert goes out. The `diagnostics` suite runs the equal-wait case as
  the check group `cache_equal_waits`.
- MMWatchdog made a reminder for every open incident and every destination with
  `remind_every`, also a destination that does not get the incident's
  alerts, such as one whose `alert_severities` lack the incident's
  severity. No destination received the reminders for destinations excluded from
  alerts, but the reminders filled the
  event history: about 40 of the last 100 events in one run. Now reminders
  go only to destinations selected for the incident's alerts, including
  on the hub for a silent node. Raising an incident's severity restarts
  the reminder timer from the raise.
- The `mysql` check read the first line of what the client program wrote
  about a refused statement. MariaDB's client writes the refused statement
  in front of the error, framed by lines of dashes. When a replica-status
  statement failed for a reason other than missing privileges, MMWatchdog
  incorrectly reported no replica and 0 seconds of lag.
  The MySQL and MariaDB clients also
  stopped at the first refused statement, although the check passes
  `--force`. Now the check sends its statements on the client's standard
  input, so `--force` continues after a failed statement. The check uses
  the line number in each client error to identify the failed SQL
  statement.
- Three more pages say what users met in the runs. The
  [`mysql`](../reference/modules/mysql.md) page says that `connections_used_percent` can
  pass 100 while root uses the server's extra connection. The
  [`node_resource`](../reference/modules/node_resource.md) page says that the kernels of
  Rocky Linux 9 and AlmaLinux 9 come with memory pressure off, and how to
  turn it on. The message of a stopped MySQL replica says
  plainly which part stopped, receiving or applying the changes.

With the fixes, the suite runs below passed:

- `node`, with the fix of `reboot_after_updates`, on Debian 13, Rocky Linux
  9, and Ubuntu 26.04;
- every suite with the module-deadline, reboot-hook, memcached-measurement,
  and documentation fixes above, on Debian 13 and Rocky Linux 9: 18
  of 18 suite runs;
- every suite with the fixes of equal waiting times and of reminders: 9 of
  9 on Rocky Linux 9, and 8 of 9 on Debian 13, where `native` failed once
  from the race that the fix of the last address of a network call above
  fixes, and passed after that fix;
- `services` with the message of a stopped MySQL replica, on Debian 13 and Rocky Linux 9; and
- with the message of a stopped MySQL replica, the changed suites `node`, `services`, and `diagnostics` on
  all six target distributions (Debian 12 and 13, Ubuntu 24.04 and 26.04,
  Rocky Linux 9, and AlmaLinux 10): 18 of 18 suite runs at the first try,
  in about 6.5 minutes.

Five small behaviors that the runs noticed stay as they are:

- A reminder that falls due while its check is healthy again, before
  `recovered_after_healthy_for` has passed, says `still failing since ...;
  latest:` with the healthy reading. The incident is still open, the
  reminder shows the latest reading, and the recovery alert follows within
  `recovered_after_healthy_for`.
- The error for a `check_groups/` directory without a check group names
  `mmwatchdog_node_settings.yml`. The error says `expected at least one
  named check group`. An operator meets the error once, at a first setup.
- A `space_free_gib` reading below 0.01 shows as 0 in an alert's message.
  Only the message rounds. The limit applies to the exact reading,
  and measurement history stores the exact reading.
- `mongodb` says "The check needs that program on this node" also when the
  time limit ended `mongosh`, with a `time_limit_per_run` under 10 seconds.
  The `mongodb` error names the time limit that ended `mongosh`.
  The `mongodb` page asks for a `time_limit_per_run` above the 10 seconds of `mongosh`.
- MMWatchdog resets a recovery action's attempt counter after
  `reset_attempts_after_healthy_for`, at the run of any check, so up to one
  check interval early.
  The triggering checks must have been healthy for the whole period.

### What was left out

- Hardware that no guest has: the inlet temperature that a server's
  management controller reports, and real sensors for `node_temperature`.
- A link speed or a duplex mode that a real network card agrees on with its
  switch. A guest's virtual network card reports no speed, so the
  lsst-camera-dh/ccs-infrastructure project set a link speed with `ethtool`; a real
  change needs an emulated network card.
- `mmwatchdog fleet forget` of a node that is already silent.
  The forget command requires a hub. Forgetting the node retires the node's
  check without claiming recovery. The `fleet` suite forgets a node that runs.
- MMX writing check groups from its inventory, so that one machine checks
  every machine of a role: that is MMX's work, not MMWatchdog's.

## The size of the `mmwatchdog` program

Measured on 2026-10-22 with Go 1.27.3 and the release build's flags (`CGO_ENABLED=0`,
`-trimpath`, `-ldflags '-s -w'`):

| Program | Size |
| --- | --- |
| `mmwatchdog` for Linux AMD64 | 16,760,992 bytes |
| `mmwatchdog` for Linux ARM64 | 15,663,264 bytes |

Go 1.27.3 makes the program 565,246 bytes larger on AMD64 and 524,286
bytes larger on ARM64 than the same MMWatchdog code built with Go 1.26.9.
Most of the increase comes from Go's standard library.
`encoding/json`, which Go 1.27 builds on the new `encoding/json/v2`, adds
about 180 KB. Go's new support for ML-DSA, a digital-signature algorithm,
adds about 44 KB to the cryptography code included in the program.

The pages of the core modules, 569,769 bytes of Markdown, are built in as
plain text, so that `mmwatchdog describe-module NAME` prints a module's page on the
node, where a check fails ([the module description](design/checks.md#the-module-description)).

On 2026-10-16, with Go 1.26.9, the
[pruned copies of the dependencies](implementation-rules.md#go-dependencies)
made the program 20,480 bytes smaller on AMD64 (from 15,814,818) and 65,536
bytes smaller on ARM64 (from 14,811,298). Removing unused dependency files
also removed initialization code that populated unused data tables.
All symbols except the unused table initializers stayed unchanged.

## Not verified for this build

- Two MySQL configurations: a replica with several sources, and a member
  of a replication group. Go tests cover multi-source and group
  replication with answers written by hand.
  The `mysql` check through a MySQL server's local socket, as root, remains
  unverified. The `mysql` check ran through a local socket as root against MariaDB,
  and against MySQL through a login.
- A MongoDB replica set with a login, and a MongoDB server that is reached
  over TLS.
- `zfs_pool` against a real pool on Debian: Ubuntu is the one target
  distribution whose own packages hold ZFS ready to use
  ([every target distribution](testing.md#every-target-distribution) says
  why).
- Shell checks, modules that are not core, and recovery commands as another
  account than `mmwatchdog` through `run_as_user_account`, or in another
  directory than `/` through `run_from_directory`: no suite sets
  `run_as_user_account` or `run_from_directory`. So on the guests every
  such program ran as `mmwatchdog` in `/`, including the check that states `env`.
  The Go tests run shell checks, modules that are not core, and recovery
  commands as the test's own account, in a test directory.
- A run stuck for reasons other than an unreachable NFS server, such as
  a program the kernel cannot end, remains unverified on deployed nodes.
  A `log_lines` check reading a file on a stuck filesystem also remains
  unverified. Go tests simulate stuck runs with a test module that ends
  only when the test permits.
- The following cases have Go-test coverage, but no deployed-node coverage:
  a log that `logrotate` rotates with `copytruncate`;
  a log file with the same inode and different content after MMWatchdog starts;
  a filesystem that answers with an error such as a stale
  NFS handle; an owner or a group from a directory service; a DNS answer
  that comes over TCP; a mail server that sends without end; a `tcp`, `redis`,
  or `memcached` server that answers a part and then waits; and a database
  server that is named by an address of IP version 6.

- On deployed nodes, titles that MMWatchdog cuts
  remain unverified, as do a hub's grouped title and body and
  `send_only_the_alert_title`. Go tests cover shortened titles, grouped
  messages, and `send_only_the_alert_title`.
- Measurement history lacks deployed-node coverage for the cases below.
  Storage cases include an unusable history file on a node or hub, and a
  hub reaching the history size limit. Storage cases also include lowering
  the size limit of a file hundreds of megabytes large, which MMWatchdog
  shrinks in steps. Synchronization cases include restored older node
  history, a node clock ahead of the hub, and readings exceeding one report.
  Node-removal cases include forgetting a node with long history.
  Further cases include forgetting a node or replacing a node with another
  node of the same name while a report is in flight.
  Simultaneous catch-up by many nodes after a hub outage also remains
  unverified. Go tests cover every case except simultaneous catch-up by many
  nodes, with small files.
  The large-fleet load run above measured the cost of readings for a hub
  with 10,000 synthetic nodes.
- A `state.db` that was restored from an older copy, on a node and on the
  hub, and a reply or a report with a sequence number above 2^53: the Go
  tests cover them.
- For node groups and a silent node's severity increase by time, on real nodes: a
  fleet of more than two nodes; a node in 100 node groups; a raise held by
  a node's whole-node window or `mmwatchdog maintenance on node_reports`; the
  raise by time and `raise_to_sos_when_silent_nodes_reach_percent` reached at the same
  moment; a restart of the hub-outage alerter node while the hub is silent; a node
  that replaces a silent node of the same name; the raise when every node of
  a node group is silent. Go tests cover the listed node-group and
  silence-severity cases;
  the fleet suite now covers a report that the hub cannot save.
- For one cause, one alert, on real nodes: a daemon start, a maintenance
  window, and a reload inside a quiet time; an incident that was announced
  before the quiet time; a quiet check that MMWatchdog cannot run; a late or a
  missed run of the named check; a chain; a heartbeat and an
  `every_systemd_unit` check as the quiet check or the named check; a recovery command; and a
  joined node, whose report gives the hub the note for `mmwatchdog fleet NODE` and
  whose alert can be part of a hub's grouped message. Go tests cover the listed quiet-time cases; the VM scenario runs two shell checks and a restart on a standalone
  node.
- A check's named destinations in a real fleet: a name the hub does not
  have, and the wrong name in a joined node's status, are covered by Go
  tests; the VM scenario for a check runs on a standalone node. Delivery by
  name through the hub is covered in the fleet scenario by a notice.
- A notice to an `mmhotline` destination, a joined node's notice while the hub
  does not answer, and a notice on a node with `destinations: {}`, on real
  nodes: the Go tests cover them.
- For the modules that are not core, on real nodes: a module that reports
  findings; a script that runs past its time limit, and the program that the script
  started; the limits of the copy for `mmwatchdog check-now`;
  a `module_state_dir` that MMWatchdog did not make; and
  `mmwatchdog validate` on a script with an
  error. Go tests cover findings, script timeouts and the program that the
  script started, state-copy limits, state directories not created by MMWatchdog, and script validation errors.
  The Go tests also run natively on the Linux guest as root.
  Not covered anywhere: a module's account that changes its state directory
  at the very moment MMWatchdog copies the state directory.
  MMWatchdog opens every path in the state-directory copy in a
  way that cannot leave the source directory. Two independent reviews read
  the state-directory copying implementation.
- For the official extra modules `slack`, `telegram`, `discord`, and
  `pushover`, the [real service test](#real-alert-destinations) did not
  reach: each service's rate limits; whether a Discord webhook may notify a
  role or `@everyone` (it notified `@here`); how a phone's notification of
  Discord shows the backslashes of the first line;
  whether one person's acknowledgement stops Pushover's repeats for every
  member of a Pushover delivery group, a list of recipients (the
  test group has one member); and how many refused requests make Pushover
  block a node, which no test should provoke. `mattermost` runs against a
  real Mattermost server. Posts and push notifications
  displayed by Mattermost's apps remain unverified.
- A step of the system clock by more than a minute on a real node, and a
  step while MMWatchdog is stopped: Go tests with a fake clock cover them. The VM
  scenarios step a guest's clock by 30 and 60 seconds only, because the
  certificates of the suites are minutes old when the steps run.
- For destination modules, on real nodes: a module that runs as another
  account than `mmwatchdog`.
- A deploy interrupted during the installation task block, and
  `mmx resume` after the interruption. The deployment suite covers a module
  directory that was left moved aside, and no other interrupted state.
- For the joined-node playbook: a refused deploy because of a module.
  The module tasks have identical text in the joined-node and
  hub-and-standalone playbooks. The deployment suite tests module refusals
  with the hub-and-standalone playbook.
- Distributions without GNU `find` and `diff`: the install task uses `find
  -printf` and `diff -r`.
- For `every_systemd_unit`, on real nodes: maintenance windows,
  reminders, raises, and MMHotline keys of a unit's incident, and a joined
  node that sends a unit's incident straight to MMHotline while the hub is
  down, which the Go tests cover; and many failed units at once. Each failed
  unit is its own alert, with no upper limit.
- An email through a real mail service: the Go tests and VM scenarios
  use stand-in mail servers. On a real node with a stand-in mail server,
  the following cases are
  not covered and the Go tests cover them: a mail server that refuses a
  sender, a recipient, or the message, one that offers no STARTTLS or no
  login that MMWatchdog uses, a certificate that MMWatchdog does not trust,
  the time limit,
  reminders to an email destination, and a reload that changes an email
  destination's identity.
- A reminder that replaces an undelivered one, on a real node: the Go tests
  cover it; the VM scenario's webhook accepts every reminder.
- Real fleets beyond two guests and the synthetic load, node power loss, and
  hardware I/O faults.
- A real MMWatchdog fleet hub that sends to a real MMHotline: the real
  service test sends through the destination's code, and the fleet suite
  through a fake MMHotline.

Module pages that rely on the "Not verified for this build" section:
[`tcp`](../reference/modules/tcp.md),
[`mmhotline`](../reference/modules/mmhotline.md),
[`mongodb`](../../official_extra_modules/mongodb/README.md),
[`discord`](../../official_extra_modules/discord/README.md),
[`mattermost`](../../official_extra_modules/mattermost/README.md),
[`pushover`](../../official_extra_modules/pushover/README.md),
[`slack`](../../official_extra_modules/slack/README.md),
[`telegram`](../../official_extra_modules/telegram/README.md).
