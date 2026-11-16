# A web service fleet

This example watches a small web service with MMWatchdog. The service runs on two
machines:

- the **application server** runs the web server, the web application, its
  caches, its databases, and its mail server, and holds the data;
- the **utility host** checks the application server over the network, as a
  visitor and an operator reach it, and watches itself.

Each machine is a standalone node: it runs its own MMWatchdog with its own
configuration and sends its own alerts. The two machines also watch each
other's SSH server.

The example started as a real company's production Monit setup, which ran a
web service for years. The example makes the Monit setup anonymous: the
example's names, domains, addresses, and services are invented. The parts
that served only the company's own products are left out. Where the setup
repeated one kind of check many times, such as the lag of a database replica
at five limits or seven Redis servers, the example keeps one check, or one
check with the MMWatchdog feature that replaces the repetition.
So each check shows a different kind of monitoring work.

A VM suite runs the example on two test guests and causes each failure for
real: see [Run the example as a test](#run-the-example-as-a-test).

## The files

```text
examples/web-service-fleet/
  application_server/          becomes /etc/mmwatchdog on the application server
    mmwatchdog_node_settings.yml
    alert_destinations.yml
    check_groups/
      web.yml                  the web server and the web application
      application.yml          the application's job queue, audio encoder, and version
      caches.yml               two Redis servers and memcached
      databases.yml            the primary MariaDB server and its replica
      dns_cache.yml            the local DNS cache
      node.yml                 memory, swap, processors, failed units, out-of-memory kills
      storage.yml              filesystems, a software RAID mirror, and a ZFS pool
      utility_host.yml         the utility host's SSH server
  utility_host/                becomes /etc/mmwatchdog on the utility host
    mmwatchdog_node_settings.yml
    alert_destinations.yml
    check_groups/
      application_server.yml   the application server, over the network
      utility_host.yml         the utility host itself
```

Install each configuration directory on the corresponding machine as the
[install on one node](../../docs/how-to/install-on-one-node.md) teaches.
The configuration directories can live under `files/application_server` and
`files/utility_host` in the deployment project `examples/deploy`.
Set each node's MMX inventory variable `mmwatchdog_config_source_dir` to
the node's configuration directory. The example's test copies each configuration
set to `files/hub_and_standalone` and deploys each machine separately.

The checks also read three files that the directories do not hold. On the
application server, two credentials, each readable by root only:

- `/etc/mmwatchdog/secrets/web-application-status-page-login`, the login of the
  status page, as one line `user:password`;
- `/etc/mmwatchdog/secrets/mariadb-replica-mmwatchdog-monitoring-password`, the password of
  the MariaDB account `mmwatchdog_monitoring` on the replica.

On the utility host, `/etc/mmwatchdog/web-service-test-certificate-authority.pem`
holds the certificate of the certificate authority that signed the site's
certificate. Provision the credentials through your normal secret
deployment, as [Install on one node](../../docs/how-to/install-on-one-node.md)
says for a webhook's token. Deploys never remove files under
`/etc/mmwatchdog/secrets/`. The example's test writes all
three files in the test's setup scripts.

The timings in the check groups are seconds, so that the test is quick.
[Timings in production](#timings-in-production) gives the values for a real
fleet.

## The application server

These services run on the application server:

- nginx, with the site `app.example.com`: a static front page, and the web
  application's `/health` and `/internal/status`, over HTTP and HTTPS. A
  default site answers every other host name with 404, as a load
  balancer's catch-all site does.
- The web application, `web-application.service`, behind nginx on
  127.0.0.1:8000, which runs as the account `webapp`. The web application's
  health command for the job queue is
  `/opt/web-application/bin/check-job-queue`. The web application's report
  job, `report-generator.service`, may use 32 MiB of memory.
  The web application uses the audio encoder `lame` for media.
- Two Redis servers from the Redis package's template unit:
  `redis-server@sessions.service` on port 6380 for the sessions, and
  `redis-server@job-queue.service` on port 6381 for the job queue; and
  memcached on port 11211.
- MariaDB: the primary on 127.0.0.1:3306, and a replica on 127.0.0.1:3307,
  `mariadb-replica.service`, which follows the primary.
- dnsmasq, a local DNS cache on 127.0.0.1:53.
- Postfix, the mail server, on port 25.
- Storage: swap space, `/srv/uploads` for the users' uploads, `/srv/shared`,
  a network share that must be mounted, a software RAID 1 mirror
  `/dev/md0`, and a ZFS pool `datapool`.

### `web.yml`: the web server and the web application

| Check | Why the check is included |
| --- | --- |
| `web_server/web_server_is_running`, `systemd_unit_state` of `nginx.service`, at `sos` | nginx serves the site. When nginx fails, MMWatchdog restarts it 3 seconds into the failure, after the alert. When an operator stops nginx with `systemctl stop`, MMWatchdog leaves it stopped: [a stopped unit stays stopped](../../docs/how-it-works.md#restarts-and-why-a-stopped-unit-stays-stopped). |
| `web_server/default_site_answers_not_found`, `http` that expects 404, at `warn` | Another answer than 404, such as the distribution's welcome page, means that a change of nginx's configuration went wrong. |
| `web_application/web_application_is_running`, `systemd_unit_state` of `web-application.service`, at `sos` | The application's process. When the application's process ends, MMWatchdog restarts `web-application.service`. |
| `web_application/web_application_answers_health_request`, `http` with the site's `Host` header, at `sos` | An application that hangs keeps the application's unit active, so only a request finds the hang. On a failure, MMWatchdog first runs a recovery command. The recovery command removes a maintenance flag that a deploy can leave behind. Only when the health request still fails after the command does MMWatchdog restart the unit: two recovery actions on one failure, in order. |
| `web_application/status_page_reports_no_stalled_workers`, `http` with `basic_auth_file` and `response_body_matches_regex`, at `warn` | A value from a status page behind a login: the application's count of stalled workers must be 0. When the login file is missing, MMWatchdog sends no request without the login file. MMWatchdog sends a cannot-check alert instead of a failure. |

The health request names the two unit checks in
`no_alerts_or_recovery_actions_while_these_checks_fail`.
The status page names the health request. So while nginx or the application's
unit is down, the unit's alert is the only alert:
[one cause, one alert](../../docs/how-it-works.md#one-cause-one-alert).

### `application.yml`: the application's own work

The three check groups in `application.yml` do real work, so the check groups
run every 5 seconds instead of every second. In production, the three check groups in `application.yml` run every 5 minutes.

| Check | Why the check is included |
| --- | --- |
| `job_queue/job_queue_health_command_succeeds`, `shell` with `run_as_user_account`, `run_from_directory`, and `env`, at `warn` | The application's health command runs as the application runs the command: as `webapp`, in the application's directory, and with the application's environment. The health command fails when more than 100 jobs wait. A command of the application's own is what a `shell` check is for. |
| `audio_encoder/audio_encoder_encodes_a_test_tone`, `shell`, at `warn` | A real piece of work proves that a tool works: the check encodes a test tone to MP3 with `lame`. A missing library or a broken package fails the audio-encoder check, which a look for the package would not notice. |
| `application_version/deployed_application_is_version_2_4_1`, `shell`, at `warn` | The deployed version must be the expected one. A deploy that installed another version fails the deployed-version check. No module compares versions, so a `shell` check does. The example writes the expected version directly in the check. |

### `caches.yml`: two Redis servers and memcached

| Check | Why the check is included |
| --- | --- |
| `sessions_redis/sessions_redis_is_running` and `sessions_redis/sessions_redis_answers_ping`, `systemd_unit_state` and `redis`, at `warn` | Each instance of one program has a check group of its own, so that an alert names the instance and MMWatchdog restarts only that instance. A Redis server that hangs keeps its unit active, so the PING finds it. |
| `job_queue_redis/job_queue_redis_is_running` and `job_queue_redis/job_queue_redis_answers_ping`, `systemd_unit_state` and `redis`, at `warn` | The job queue's Redis has a separate check group, so alerts name the job-queue instance and MMWatchdog restarts only the job-queue instance. A Redis server that hangs keeps the Redis unit active, so the PING detects the hang. |
| `memcached/memcached_is_running` and `memcached/memcached_answers_statistics_request`, `systemd_unit_state` and `memcached`, at `warn` | A service that MMWatchdog watches and does not restart: the check group has no `restart_systemd_unit`. MMWatchdog alerts, and a person decides. |

Each PING and the statistics request name the unit check of their own
check group, so a stopped server sends one alert.

### `databases.yml`: the primary MariaDB server and its replica

| Check | Why the check is included |
| --- | --- |
| `database_primary/connections_are_below_50_percent`, `mysql` through the local socket, with a limit on `connections_used_percent`, at `warn` | A measurement of the server past a limit. The server allows 20 connections, and an application that leaks connections uses them up. |
| `database_primary/reporting_database_answers_a_real_query`, `shell`, at `warn` | A real query with the expected result: the reports need at least one row of `reporting.daily_totals`. The `mysql` module asks the server fixed questions and takes no query of the user's, so a `shell` check runs the query. The query command logs in through the local socket as `mmwatchdog`, which MariaDB knows as `mmwatchdog@localhost`. The account `mmwatchdog@localhost` has read access to the database `reporting` only. |
| `database_replica/replica_is_less_than_5_seconds_behind`, `mysql` of the replica, with a limit of 5 on `replication_lag_seconds`, at `warn` | A replica that falls behind gets a warning first. |
| `database_replica/replica_is_less_than_15_seconds_behind`, `mysql` of the replica, with a limit of 15 on `replication_lag_seconds`, at `sos` | The replica gets an urgent alert when the replica falls further behind: two checks with two limits and two severities alert again as replication lag gets worse. A replica that stops replicating fails the 5-second and 15-second lag checks. MMWatchdog logs in to the replica as `mmwatchdog_monitoring`, with the password in a file. |

### `dns_cache.yml`: the local DNS cache

| Check | Why the check is included |
| --- | --- |
| `dns_cache/dns_cache_is_running`, `systemd_unit_state` of `dnsmasq.service`, at `info` | The DNS cache's process. |
| `dns_cache/dns_cache_answers_for_the_site`, `dns` over UDP with `expected_record_values`, at `info` | dnsmasq must answer `app.example.com` with `192.0.2.10`: a UDP service that answers a real request. |

The DNS cache's check group waits 10 seconds for failure and for recovery.
A failure must last 10 seconds before the failure counts, and a recovery
must last 10 seconds before the recovery counts.
So a brief DNS failure sends no alert, and a DNS cache that comes and goes
sends no recovery alert until the DNS cache stays healthy. The DNS cache's alerts are `info`, which
only the team's chat channel for information gets.

### `node.yml`: the machine itself

| Check | Why the check is included |
| --- | --- |
| `node_memory/memory_is_below_75_percent`, `node_resource: memory`, at `warn` | A fix when a resource of the whole machine runs out: the application leaks memory, so the check group restarts `web-application.service`, before the kernel starts to end processes. |
| `node_swap/swap_is_not_in_use_while_memory_is_nearly_full`, `shell`, at `warn` | Swap space in use after a past spike is harmless. Swap in use while memory is nearly full means the machine is about to struggle. A `node_resource` check has one measurement, and MMWatchdog has no rule that combines two measurements. So a `shell` check reads memory usage and swap usage from `/proc/meminfo`. The shell check fails when memory is above 60 percent and swap above 20 percent. |
| `node_processors/cpu_is_below_90_percent` and `node_processors/io_wait_is_below_20_percent`, `node_resource: cpu` and `io_wait`, at `warn` | The processors are busy, or wait for a slow disk. |
| `failed_systemd_units/machine_has_no_failed_systemd_unit`, `every_systemd_unit`, at `warn` | Any unit that systemd reports as failed, such as the report job, each as its own incident. The units with a check of their own are in `systemd_units_to_ignore`, so their failure sends one alert. |
| `out_of_memory_kills/out_of_memory_kills`, `out_of_memory_kills`, at `warn` | The kernel ended a process for lack of memory. One kill is enough: the out-of-memory-kills event check alerts at the first failed run and takes no `alert_after_failing_for`. |

### `storage.yml`: filesystems, RAID, and ZFS

| Check | Why the check is included |
| --- | --- |
| `uploads_filesystem/uploads_filesystem_is_below_90_percent`, `node_filesystem` of `/srv/uploads`, with limits on space and inodes, at `warn` | A warning while there is still time. |
| `uploads_filesystem/uploads_filesystem_is_below_98_percent`, `node_filesystem` of `/srv/uploads`, with limits of 98 percent on space and inodes, at `sos` | An urgent alert when the filesystem is nearly full: two levels on one filesystem. |
| `shared_storage/shared_storage_is_mounted`, `node_filesystem` of `/srv/shared` without a limit, at `warn` | A filesystem that must be mounted: the check fails while nothing is mounted at the path. MMWatchdog then runs the recovery command `mount /srv/shared`, which mounts the shared filesystem again from `/etc/fstab`. |
| `raid_mirror/mdadm_array`, `mdadm_array` of `/dev/md0`, at `warn` | A software RAID mirror that loses a disk. |
| `zfs_data_pool/data_pool_is_healthy_and_below_88_percent`, the official extra module [`zfs_pool`](../../official_extra_modules/zfs_pool/README.md), at `warn` | A ZFS pool that loses a device or fills up. |

### `utility_host.yml`: the other machine

| Check | Why the check is included |
| --- | --- |
| `utility_host/machine_answers_ssh`, `ssh` of `utility-host.web-service.internal:22`, at `warn` | The application server checks the utility host's SSH server, as the utility host checks the application server's. If the utility host stops answering SSH, the application server alerts. If the application server stops answering SSH, the utility host alerts. |

## The utility host

The utility host runs no service of the web service. The utility host checks the
application server from the network, by the site's public name
`app.example.com` and by the machine's name
`application-server.web-service.internal`.

### `application_server.yml`: the application server, over the network

| Check | Why the check is included |
| --- | --- |
| `application_server/https_port_accepts_connections`, `tcp` of `app.example.com:443`, at `sos` | The site's port. The site's other checks name it, so while the port refuses connections, its alert is the one alert of the site. |
| `application_server/site_answers_from_outside`, `http` of `https://app.example.com/`, at `warn` | The site as a visitor sees it. The site's certificate comes from a certificate authority of the example's own, so the check trusts that authority's certificate with `trusted_certificate_authority_file`; a site with a certificate of a public certificate authority needs no such file. |
| `application_server/site_certificate_is_valid_for_21_more_days`, `tls` with `fail_when_certificate_expires_within: 21d`, at `warn` | A warning three weeks before the certificate expires. |
| `application_server/site_certificate_is_valid_for_7_more_days`, `tls` with `fail_when_certificate_expires_within: 7d`, at `sos` | An urgent alert one week before: two levels on one certificate. |
| `application_server/machine_answers_ping`, `ping`, at `warn` | The machine answers on the network. |
| `application_server/machine_answers_ssh`, `ssh`, at `warn`, with `raise_to_sos_after_failing_for: 8s` | A machine that stops answering SSH is a warning at once. MMWatchdog sends reminders about the open SSH incident every 4 seconds to `team_chat_warn_and_sos` (`remind_every`). After 8 seconds of SSH failure, MMWatchdog raises the incident's severity to `sos`, so `on_call_pager` also receives an alert. |
| `application_server/mail_server_answers_smtp`, `smtp`, at `warn`, with `send_alerts_only_to_destinations: [mail_administrator]` | The mail server belongs to the mail administrator, and its alerts go to that person alone. |

### `utility_host.yml`: the utility host itself

| Check | Why the check is included |
| --- | --- |
| `utility_host_machine/memory_is_below_90_percent`, `node_resource: memory`, at `warn` | The utility host's own memory. |
| `utility_host_machine/root_filesystem_is_below_90_percent`, `node_filesystem` of `/`, at `warn` | The utility host's own disk space. |

## The destinations

Each check states an alert severity, and each destination states the
severities the destination takes, so severity decides who gets an alert.
The recovery alert goes to every destination that got the failure:
[which destination gets an alert](../../docs/how-to/send-alerts-somewhere.md#send-a-checks-alerts-to-chosen-destinations).

| Destination | Takes | Stands in for | In a real fleet |
| --- | --- | --- | --- |
| `team_chat_info` | `info` | The team's chat channel for information | A chat module, such as [`slack`](../../official_extra_modules/slack/README.md) or [`mattermost`](../../official_extra_modules/mattermost/README.md) |
| `team_chat_warn_and_sos` | `warn` and `sos` | The team's chat channel for warnings and urgent alerts. On the utility host, `team_chat_warn_and_sos` sends reminders for each open incident every 4 seconds (`remind_every: 4s`) | The chat module chosen for `team_chat_info`, for another channel |
| `on_call_pager` | `sos` | The on-call pager | [`pushover`](../../official_extra_modules/pushover/README.md) |
| `mail_administrator`, on the utility host only | No severity: only the SMTP check names `mail_administrator` | The person who runs the mail server | [`email`](../../docs/reference/modules/email.md) |

In the example, each destination is a `webhook` to a receiver on the node
that runs the check. Each destination uses a separate path, such as
`http://127.0.0.1:18080/events/on_call_pager`, so that the test sees which
destination got each alert. A real fleet replaces each webhook with the
destination module for the chat, paging, or email service receiving the alert.

The Monit setup had four failure-alert scripts, each with a separate script
that sent recovery notifications. The failure-alert scripts sent information
and warnings to two chat channels, urgent alerts to chat and a paging
service, and alerts through a script named after a person.

MMWatchdog provides the scripts' routing and recovery behavior through settings:

- Each check's alert severity marks information, warnings, or urgent alerts.
- Each destination's `alert_severities` selects the alerts for the chat
  channels and the paging service.
- MMWatchdog sends recovery alerts automatically, replacing the recovery
  scripts.
- `send_alerts_only_to_destinations` sends alerts only to the named person's
  destination, replacing the script named after a person.

## Timings in production

The example's timings are seconds. The Monit setup checked every 30
seconds, and these are its timings in MMWatchdog's form:

| Checks | In production |
| --- | --- |
| Local services and health requests | `check_every: 5m`, `alert_after_failing_for: 10m` |
| The unit checks of systemd services | `check_every: 30s`, `alert_after_failing_for: 5m` |
| CPU | `alert_after_failing_for: 10m` |
| I/O wait and memory | `alert_after_failing_for: 20m` |
| Swap while memory is nearly full | the `shell` check with swap above 10 percent while memory is above 95 percent |
| The recovery of the machine's resources | `recovered_after_healthy_for: 5m` |
| Disk space and inodes | limits of 90 and 98 percent, `alert_after_failing_for: 0s` |
| Memory that restarts the application | a limit of 95 percent, `restart_after_failing_for: 0s` |
| Failed systemd units | `alert_after_failing_for: 3m` |
| Software RAID | `alert_after_failing_for: 30m` |
| The DNS cache | `alert_after_failing_for: 2m30s` |
| SSH of the application server from the utility host, and SSH of the utility host from the application server | `alert_after_failing_for: 2m`, `remind_every: 1m` on the chat destination, `raise_to_sos_after_failing_for: 10m` |
| The site and its certificate from outside | `alert_after_failing_for: 2m`; `fail_when_certificate_expires_within: 21d` and `7d` |
| The mail server | `alert_after_failing_for: 30m`, `send_alerts_only_to_destinations` |
| The replica's lag | limits on `replication_lag_seconds` such as 1800 (`warn`) and 10800 (`sos`), with `remind_every` on the chat destination |
| The application's health command | `check_every: 5m`, `alert_after_failing_for: 10m` |
| The encoder test and the version check | `check_every: 5m` for the encoder test and the version check. `alert_after_failing_for: 5m` for the encoder test, and `alert_after_failing_for: 30m` for the version check. |

With the production table's timings, the default of 8 for
`max_concurrent_checks` serves the application server. The example raises
`max_concurrent_checks` to 16 because about 45 checks run every second.

## What the example leaves out

- **A DHCP server.** Like a DHCP check, the DNS cache check tests whether a
  UDP service answers a real request.
- **The services that only the company had**, such as its audio streaming
  servers. The example keeps the kinds of checks behind the company services:
  a value from a status page, and a measurement past a limit that alerts
  again as the measurement gets worse.
- **A central view, and a cron job that restarted the monitor.** MMWatchdog runs
  under systemd. A [fleet hub](../../docs/how-it-works.md#the-fleet) provides a central
  view. The hub's silence alert and hub-outage alerter node also notice a machine
  that stops answering. The example runs two standalone nodes, as the
  original company's production configuration ran two Monit instances.
- **The state change of keepalived on a pair of load balancers.** In MMWatchdog,
  keepalived's notify script runs
  [`mmwatchdog send-notice`](../../docs/reference/commands.md#mmwatchdog-send-notice).
- **Limits for each role and each machine, and checks generated for every
  machine of a group.** MMX's templates and inventory set role-specific and
  machine-specific limits, and generate checks for the machines of an
  inventory group.
  The example writes each node's files directly.

## Run the example as a test

The VM suite `web_service_fleet_example` runs the example on the two guests
of a job slot, as the
[testing guide](../../docs/changing-mmwatchdog/testing.md#the-web-service-fleet-example) describes. The guest `mmwatchdog-dev` is the application server,
and `mmwatchdog-peer` is the utility host. Each guest's `/etc/hosts` gives
`mmwatchdog-dev` the names
`application-server.web-service.internal` and `app.example.com`, and
`mmwatchdog-peer` the name `utility-host.web-service.internal`.

```sh
python3 tests/integration/incus.py --distribution ubuntu2404 --suite web_service_fleet_example
```

The suite's guests run Ubuntu 24.04 only. The application server's ZFS pool
needs ZFS's kernel module, and among MMWatchdog's target distributions only
Ubuntu's kernel packages hold it; the setup also installs Ubuntu's packages
by their names. The example's checks run the same MMWatchdog code on every
distribution, and the `node` and `services` suites test what differs
between distributions.

The test's own files are in
[`tests/integration/web_service_fleet_example/`](../../tests/integration/web_service_fleet_example/):
`setup/` holds the scripts that install and start each machine's services.
`step_commands/` holds helper commands that cause failures and read results.
`scenarios.json` is the scenario file. The driver
[`monitoring_setup_scenarios.py`](../../tests/integration/monitoring_setup_scenarios.py)
says what each part of the scenario file means. Each test run performs the
setup, the quiet check, and the failure scenarios in order:

1. The test driver runs the setup scripts on each guest, then deploys
   MMWatchdog through MMX and `examples/deploy`. Each guest receives the
   corresponding configuration directory from `examples/web-service-fleet`.
2. The quiet check: every check on the application server and the utility
   host becomes healthy, and then no alert arrives for 15 seconds.
3. Each scenario causes a failure for real, then waits for the expected alerts
   and the destination of each alert, then restores the pre-failure state.
   Every check must then be healthy again.
   Any unexpected alert fails the scenario, so a scenario also shows which
   checks stay quiet.

| Scenario | What the scenario shows |
| --- | --- |
| `web_application_killed` | `SIGKILL` leaves the application's unit failed. The `sos` alert reaches the chat and the pager. MMWatchdog restarts the unit 3 seconds into the failure, after the alert. The recovery alert reaches the chat and the pager. The health request, which names the unit check, sends nothing. |
| `web_application_hangs_and_gets_the_fix_command_then_a_restart` | `SIGSTOP` freezes the application while the application's unit stays active. The health request times out and alerts at `sos`. MMWatchdog runs the recovery command first, which does not help the frozen application. MMWatchdog restarts the unit only on a failure MMWatchdog sees after the recovery command. `mmwatchdog events` shows the order. The status page, which names the health request, sends nothing. |
| `web_server_stopped_by_an_operator` | `systemctl stop nginx`. MMWatchdog alerts and does not restart a unit that was stopped on request (`systemd_unit_restart_withheld`); nginx stays stopped until the scenario starts it. On the utility host, the port check sends the one alert of the site; the site and the two certificate checks stay quiet. On the application server, the health request, the status page, and the 404 check stay quiet. |
| `default_site_answers_200_instead_of_404` | The default site answers 200; the check that expects 404 sends a `warn` alert to the chat only. |
| `status_page_reports_stalled_workers` | The status page, behind its login, says `stalled_workers: 2`. |
| `status_page_login_file_missing` | The status-page login file is missing. MMWatchdog sends no request without the login file, so MMWatchdog cannot check: the `cannot_check` alert, and `can_check_again` once the login file is back. |
| `one_of_two_redis_instances_hangs` | `SIGSTOP` freezes the job queue's Redis. The job-queue Redis PING check alerts and MMWatchdog restarts the job-queue instance. The sessions' instance stays healthy and quiet. |
| `memcached_killed_and_left_alone` | memcached ends; MMWatchdog alerts and does nothing else, and memcached stays down until the scenario starts it. |
| `dns_cache_flaps_then_stops_answering` | dnsmasq stops answering for less than the 10 seconds of `alert_after_failing_for`, which sends nothing; then for longer, which sends the `info` alert to `team_chat_info` only. A healthy time shorter than `recovered_after_healthy_for` sends no recovery; a longer one does. |
| `report_job_killed_by_the_kernel` | The report job takes 256 MiB in a unit that allows 32 MiB, and the kernel ends the report job. Two alerts: `out_of_memory_kills`, and the failed unit `report-generator.service` reported by `every_systemd_unit`. The failed unit is a finding of `every_systemd_unit`, with a recovery of the failed unit after `systemctl reset-failed`. |
| `application_leaks_memory_and_gets_restarted` | The application takes memory until the machine's memory is 85 percent used. The memory check alerts and restarts the application, which frees the memory. |
| `swap_in_use_while_memory_is_free_and_then_nearly_full` | A program in a unit of 64 MiB holds 400 MiB. More than 25 percent of swap is in use while memory is free. The swap check stays healthy, which a plain limit on swap would not. A second program then fills memory to 67 percent, and the check alerts. |
| `processors_kept_busy` | Endless loops on every processor. |
| `programs_wait_for_a_slow_disk` | `dd` writes directly to the disk in a unit that systemd limits to 1 MiB per second, so the processors wait for the disk. |
| `uploads_filesystem_fills_up` | The uploads filesystem fills to 93 percent: the `warn` alert to the chat. Then the uploads filesystem fills to 99 percent: the `sos` alert to the chat and the pager. Removing the files recovers the warning and urgent checks. |
| `uploads_filesystem_runs_out_of_inodes` | The uploads filesystem fills to 93 percent inode usage: the `warn` alert to the chat. Then inode usage reaches 99 percent: the `sos` alert to the chat and pager. Removing the files recovers the warning and urgent checks. |
| `shared_storage_unmounted_and_mounted_again` | `umount /srv/shared`: the alert, then the recovery command mounts the share again, then the recovery. |
| `raid_mirror_loses_a_disk` | `mdadm --fail` of one device of the mirror; the check fails until the device is back and rebuilt. |
| `zfs_pool_loses_a_device` | `zpool offline` takes one half of the pool offline. The check fails until the offline half is back. |
| `replica_stops_replicating` | The replica stops receiving changes. The 5-second and 15-second lag checks fail. |
| `replica_falls_further_and_further_behind` | The replica applies changes 30 seconds late while orders are written every half second, so the replica falls behind second by second. The `warn` alert comes above 5 seconds, then the `sos` alert above 15 seconds. |
| `database_connections_nearly_used_up` | 12 idle connections to a server that allows 20. |
| `reporting_query_fails` | The table that the reports read is renamed. |
| `job_queue_backs_up` | The scenario first verifies the health command's healthy output from a run as `webapp`. Then the scenario places 500 jobs in the queue. |
| `audio_encoder_loses_its_library` | `libmp3lame.so.0` is moved away, so `lame` cannot start. |
| `deployed_application_version_differs` | Version 2.5.0 is deployed while the check expects 2.4.1. |
| `application_server_ssh_unreachable_from_the_utility_host` | The application server drops new SSH connections from the utility host. The `warn` alert reaches the chat only; a reminder follows; after 8 seconds of failing, the raise to `sos` reaches the chat and the pager; the recovery reaches both. |
| `application_server_does_not_answer_ping` | The application server drops ping requests from the utility host. |
| `utility_host_ssh_unreachable_from_the_application_server` | The utility host drops SSH from the application server, and the application server's check of the utility host alerts. |
| `site_certificate_expires_in_10_days_and_then_in_3` | A certificate with 10 days until expiry fails the 21-day check (`warn`) and passes the 7-day check. A certificate with 3 days until expiry fails the 7-day check (`sos`). A certificate with 90 days until expiry makes the 21-day and 7-day checks recover. |
| `mail_server_down` | Postfix stops; the SMTP check's failure and recovery reach `mail_administrator` and no other destination. |

How the test causes its failures:

- **The application and memcached have `Restart=no`**, so that a killed
  program stays down until MMWatchdog restarts it, or until the scenario starts it.
  With `Restart=always`, systemd would restart a killed application or
  memcached process before MMWatchdog saw the failure.
  The Redis servers keep their `Restart=always`: the scenario
  freezes Redis with `SIGSTOP`, so the unit stays active while Redis does
  not answer.
- **A scenario that needs a restart kills or freezes the program**, because
  MMWatchdog leaves a unit that was stopped on request stopped. The scenario
  signals the main process only (`systemctl kill --kill-whom=main`).
- **Where an action of MMWatchdog must come after an alert**, the recovery
  action waits 3 seconds and the alert 2 seconds. The DNS cache's
  10 seconds are far longer than the scenario's own steps.
  The scenario controls the duration of a short failure, so the duration
  does not depend on the machine's speed.
- **A `shell` check's message describes the command's exit code**, such as
  `command exited 1`. What the script prints is the run's standard output,
  which the alert's body shows and which the step commands read.
- **The utility host's reminders may come in any scenario of the utility host's checks**,
  including reminders MMWatchdog sends while confirming recovery.
  The scenario file therefore allows the utility host's reminders.
