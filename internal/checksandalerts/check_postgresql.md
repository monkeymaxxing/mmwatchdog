# `postgresql`

`postgresql` asks a PostgreSQL server a question through the server's own
client program, `psql`, as MMX's module for PostgreSQL does. The check is
healthy when the server answers and the conditions below hold. A streaming
replica with `primary_conninfo` set and no row in `pg_stat_wal_receiver`
fails without a limit. A standby that replays copied WAL
stays healthy without a streaming receiver. The check also measures how far
a replica is behind its primary (`replication_lag_seconds`), how many of
the server's connections are in use (`connections_used_percent`), how
many of its block reads its shared buffers served (`cache_hit_percent`),
how long its write-ahead log waits to be archived
(`wal_archive_lag_seconds`), and how far its transaction IDs are toward
wraparound (`transaction_ids_used_before_wraparound_percent`). The check
only asks: it sends one `SELECT` statement, twice on its first run, and
changes nothing.

A check without inputs, `postgresql: {}`, asks the server on the node
itself and needs no account, no password, and no setup. A check of a server
on another machine names it in `database_server_host` and logs in there.
For a MySQL or a MariaDB server, use [`mysql`](check_mysql.md), which has
the inputs of `postgresql`, with `login_user` in place of
`login_role`.

## Set up

**The server on the node itself** needs nothing: a node that has the server
has `psql`, and the check asks as the server's own administrator.

**A check that logs in**, of a server on another machine or of the server on
the node, needs:

- `psql` on the node that checks, when the server is on another machine:
  the distribution's PostgreSQL client package, such as `postgresql-client`
  on Debian and Ubuntu and `postgresql` in the Red Hat family.
- An account inside the server that can log in and nothing more. In
  PostgreSQL an account is a role. Run on the database server:

  ```sh
  sudo -u postgres createuser mmwatchdog_monitoring
  sudo -u postgres psql --command "SET password_encryption = 'scram-sha-256'" --command '\password mmwatchdog_monitoring'
  ```

  `createuser` makes a role that can log in and has no other right.
  `\password` asks for the role's password twice, so the password stays out
  of the shell's history and of the process list. PostgreSQL stores the password in the form
  `scram-sha-256`, which the line in `pg_hba.conf` below needs.
  PostgreSQL 13 would otherwise keep the password as `md5`. The check
  connects to the database `postgres`, which every role may connect to
  unless someone took that right away.
- For a replica checked through a login, `mmwatchdog_monitoring` also needs
  `pg_read_all_settings` to read whether `primary_conninfo` is set:

  ```sh
  sudo -u postgres psql --command "GRANT pg_read_all_settings TO mmwatchdog_monitoring"
  ```

  MMWatchdog uses the setting to distinguish a lost streaming connection from a
  standby that only replays copied WAL. Membership in
  `pg_read_all_settings` also lets the role read the server's other
  restricted settings. A primary check does not need membership in
  `pg_read_all_settings`. A check through the local socket already runs as the server's administrator.
- A line in the server's `pg_hba.conf` that lets the role log in with a
  password from the node's address. PostgreSQL uses the first line that
  fits a connection, so put this line above the others:

  ```text
  hostssl  postgres  mmwatchdog_monitoring  192.0.2.10/32  scram-sha-256
  ```

  `192.0.2.10` is the address of the node that checks. `hostssl` takes only
  encrypted connections, which `verified_tls` makes. For
  a check with `connection_security: unencrypted`, write `host`; for the
  server on the node, write `127.0.0.1/32`. Debian and Ubuntu already have
  such a line for `127.0.0.1` and `::1`; the Red Hat family does not, and
  its `pg_hba.conf` is in the data directory, `/var/lib/pgsql/data`. Then let the server read the file again, with `sudo -u postgres
  psql --command 'SELECT pg_reload_conf()'`. A server on another machine
  must also listen on an address that the node reaches: `listen_addresses`
  in `postgresql.conf` is `localhost` unless someone changed it.
- A file on the node with the password on one line, which only root can
  read. MMWatchdog reads `login_password_file` as root. The second command
  below reads the password from the terminal: type
  the password, press Enter, and then Ctrl-D.

  ```sh
  sudo install -d -m 0700 /etc/mmwatchdog/secrets
  sudo install -m 0600 /dev/stdin /etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring
  ```

  `mmwatchdog validate` refuses a password file that its group or other
  accounts can read without explicit permission ([credential files that other accounts can use](../../docs/reference/configuration-files.md#secret-files)).

For an exact `connections_used_percent`, the role also needs the right to
read the statistics of all sessions:
`sudo -u postgres psql --command 'GRANT pg_read_all_stats TO mmwatchdog_monitoring'`.
Membership in `pg_read_all_stats` also shows the role the statement that
every session runs, which can hold the application's data. Without
membership in `pg_read_all_stats`, the reading is a little high
(see [connections in use](#connections-in-use)).

## When the check fails, and when it cannot check

- **The check fails** when the server does not answer, when it refuses the
  login, and when the answer does not arrive within the check group's
  `time_limit_per_run`. The check also fails when `psql` does not accept the
  server's certificate or the protection of the connection (see
  [how the connection is protected](#how-the-connection-is-protected)), and
  when a reading passes a limit. A streaming replica with `primary_conninfo` set and no row in
  `pg_stat_wal_receiver` fails even without a limit.
  A standby that replays copied WAL, with no `primary_conninfo`, stays
  healthy without a streaming receiver. When the client reports an error,
  the message holds the first line of what `psql` says.
- **MMWatchdog cannot check,** and the run is unknown, when:
  - a replica has no streaming receiver and the login role cannot read
    `primary_conninfo`; grant `pg_read_all_settings` as described in
    [Set up](#set-up), so MMWatchdog can tell whether streaming is required;
  - the node has no `psql`, or MMWatchdog cannot run it as the account that the
    check needs;
  - MMWatchdog cannot read the password file;
  - with `verified_tls`, the node has no file of certificate authorities,
    or the account `mmwatchdog` cannot read `database_server_certificate_authority_file`;
  - `psql` gives an answer that MMWatchdog does not understand;
  - a limit names `replication_lag_seconds`, and the server is a replica
    that has not applied a change of its primary yet, so MMWatchdog has no number;
  - a limit names `cache_hit_percent`, and the server read no block since
    the run before ([the cache hit ratio](#the-cache-hit-ratio));
  - a limit names `wal_archive_lag_seconds`, and at least one of the
    following conditions holds: the check logs in with `login_role`, or
    the server is a primary with `archive_mode` off
    ([how long the write-ahead log waits](#how-long-the-write-ahead-log-waits-to-be-archived)).

A failed run starts or continues the check's
[failing time](../../docs/how-to/decide-when-a-check-alerts.md). Unknown runs open a
separate cannot-check incident after the check's waiting times, as the design's
[failing time](../../docs/changing-mmwatchdog/design/checks.md#the-failing-time)
describes. The measurements take a limit in the same way as every
measurement does
([checks that measure a number](../../docs/reference/configuration-files.md#checks-that-measure)).

## Examples

### The server on this node, and how far behind it is

```yaml
# /etc/mmwatchdog/check_groups/postgresql.yml
check_groups:
  postgresql:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 3m
    recovered_after_healthy_for: 1m
    checks:
      postgresql_answers:
        postgresql: {}                            # the server on this node
        alert_severity: sos
      postgresql_replica_is_less_than_60_seconds_behind:
        postgresql: {}
        fail_when_measurement_above:
          replication_lag_seconds: 60
        alert_severity: warn
```

Every minute, MMWatchdog on the node runs `psql` as the operating system account
`postgres` and asks the server on the node through its local socket. The
first check is healthy whenever the server answers, with a message such as
`the PostgreSQL server answers; it is a primary`. When the server does not
answer for 3 minutes, MMWatchdog sends an `sos` alert.

The second check also fails when the server is a replica that is more than
60 seconds behind its primary, and sends a `warn` alert after 3 minutes. A
primary is 0 seconds behind, so the check with the replication-lag limit
fits every server of a primary and its replicas, and stays right when a
replica is made the primary.

### The database cache, and the archive of the write-ahead log

```yaml
# /etc/mmwatchdog/check_groups/postgresql_capacity.yml
check_groups:
  postgresql_every_hour:
    check_every: 1h
    time_limit_per_run: 10s
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    checks:
      hot_data_fits_in_the_shared_buffers:
        postgresql: {}
        fail_when_measurement_below:
          cache_hit_percent: 99                   # of the blocks read in the hour since the run before
        alert_severity: warn
      transaction_ids_are_less_than_half_way_to_wraparound:
        postgresql: {}
        fail_when_measurement_above:
          transaction_ids_used_before_wraparound_percent: 50
        alert_severity: warn
  postgresql_archive:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 1m
    checks:
      write_ahead_log_is_archived:
        postgresql: {}
        fail_when_measurement_above:
          wal_archive_lag_seconds: 300            # how long the oldest finished segment has waited
        alert_severity: warn
```

Every hour, MMWatchdog asks the server on the node how many blocks it read
since the run before, and how many blocks the server's shared buffers held. When
fewer than 99 percent of the hour's reads were in the buffers, the data
that the application uses most no longer fits in the server's memory, and
the cache check sends a `warn` alert at once: the reading already covers
the hour. The transaction-ID check warns above 50 percent, leaving about
a billion transactions for vacuum to finish. Every minute, the archive
check sends a `warn` alert when a
finished segment of the write-ahead log has waited more than 5 minutes to
be archived, such as by `pgbackrest archive-push`. A server without writes
finishes no segment, so the server stays healthy on a quiet day.

### A server on another machine

```yaml
# /etc/mmwatchdog/check_groups/database_machine.yml
check_groups:
  database_machine:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 3m
    recovered_after_healthy_for: 1m
    checks:
      postgresql_on_the_database_machine_answers:
        postgresql:
          database_server_port: 5432
          database_server_host: db-1.internal.example.com
          connection_security: verified_tls
          login_role: mmwatchdog_monitoring
          login_password_file: /etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring
        fail_when_measurement_above:
          connections_used_percent: 80
        alert_severity: sos
```

Every minute, MMWatchdog on the node reads the password file and runs `psql` as the
unprivileged account `mmwatchdog`. `psql` opens an encrypted connection to the
server on `db-1.internal.example.com` at the standard port 5432, verifies
that the server's certificate is valid for that name, and logs in as the
role `mmwatchdog_monitoring`. The check fails when the server does not answer, when
the certificate is not valid, and when more than 80 percent of the server's
connections are in use. When the check has failed for 3 minutes because
the server does not answer, the certificate is invalid, or connection use
is above 80 percent, MMWatchdog sends an `sos` alert.

### The server on this node through a login

As Debian and Ubuntu install PostgreSQL, the local
socket logs an account of the operating system in as the role of the same
name (`peer`), so a login with a password works over a network address
only. A check of the server on the
node that logs in therefore states the address `127.0.0.1`, and
`connection_security: unencrypted`, because that connection stays on the
node. The server's `pg_hba.conf` needs a line for `127.0.0.1`, as
[set up](#set-up) says:

```yaml
# /etc/mmwatchdog/check_groups/postgresql_login.yml
check_groups:
  postgresql_login:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 3m
    recovered_after_healthy_for: 1m
    checks:
      application_role_can_log_in:
        postgresql:
          database_server_port: 5432
          database_server_host: 127.0.0.1
          connection_security: unencrypted
          login_role: mmwatchdog_monitoring
          login_password_file: /etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring
        alert_severity: warn
```

MMWatchdog on the node runs `psql` as the account `mmwatchdog`. `psql`
connects to the server on the node at `127.0.0.1`, port 5432, and logs in as
`mmwatchdog_monitoring`. The check fails when the role cannot log in, for example
after the password of `mmwatchdog_monitoring` changed on the server but
`/etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring` still holds the
old password.

### A certificate of your own certificate authority

A server whose certificate a certificate authority of your own signed
needs that authority's certificate on the node. `psql` reads the file as
the account `mmwatchdog`, so keep it outside `/etc/mmwatchdog/secrets`, such as in
`/etc/mmwatchdog`:

```yaml
# /etc/mmwatchdog/check_groups/internal_database.yml
check_groups:
  internal_database:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 3m
    recovered_after_healthy_for: 1m
    checks:
      internal_database_answers:
        postgresql:
          database_server_port: 5432
          database_server_host: db-1.internal.example.com
          connection_security: verified_tls
          database_server_certificate_authority_file: /etc/mmwatchdog/database-authority.pem
          login_role: mmwatchdog_monitoring
          login_password_file: /etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring
        alert_severity: sos
```

MMWatchdog on the node first asks the operating system whether the account `mmwatchdog` can
read `/etc/mmwatchdog/database-authority.pem`. `psql` then trusts only the
certificates in `/etc/mmwatchdog/database-authority.pem` for the server's
certificate.

## How it works

### Through the local socket, without a login

A check without `database_server_host` uses the local socket of the server
on the node, as the server's own administrator. PostgreSQL takes the operating system account `postgres`
for its role `postgres` without a password, so MMWatchdog runs `psql` as
the operating system account `postgres`.

- **Through the local socket, nothing is encrypted,** and the check states
  no `connection_security`
  ([PostgreSQL, MySQL, and MariaDB checks](#how-the-connection-is-protected)).
- **`database_server_port` alone** names a second server on the node: the
  local socket of PostgreSQL holds the port in its name, such as
  `/var/run/postgresql/.s.PGSQL.5432`.

### With a login

A check with `login_role` and `login_password_file` logs in with a password,
and MMWatchdog runs `psql` as the unprivileged account `mmwatchdog`, which needs no rights
on the node. MMWatchdog uses the account `mmwatchdog` for a server on
another machine, with `database_server_host`, and for the server on the
node without `database_server_host`.

- **MMWatchdog reads the password file for each run,** so a changed password needs
  no reload. MMWatchdog gives the password to `psql` in the environment
  (`PGPASSWORD`), never as an argument: the arguments of a program show in
  the process list.
- **`psql` never asks for a password** (`--no-password`). A server that
  requires a password that the check does not have fails the check.
- **`psql` reads no file of its own settings** (`--no-psqlrc`) and connects
  to the database `postgres`.
- **Without `database_server_port`,** `psql` uses the standard port, 5432.

### How the connection is protected

A check with `database_server_host` states `connection_security`. The
rules below are the same for `postgresql` and [`mysql`](check_mysql.md).

- **`connection_security` has no default.** Without explicit settings,
  `psql`, `mariadb`, and `mysql` each protect the connection in a different
  way. So MMWatchdog always tells the client program how to protect the
  connection, and the check shows the choice.
- **`database_server_host` takes a name, or an IPv4 or IPv6 address** such
  as `2001:db8::5`, without a port.
- **With certificate checking on, `verified_tls` trusts the node's own
  certificate authorities,** unless the check states
  `database_server_certificate_authority_file`. The node's file is the
  first of these that exists: `/etc/ssl/certs/ca-certificates.crt`
  (Debian, Ubuntu, and Arch Linux), `/etc/ssl/ca-bundle.pem` (openSUSE),
  and `/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem` (Rocky Linux,
  AlmaLinux, and the rest of the Red Hat family).

The following table says what `psql` gets.

MMWatchdog tells `psql` the value in its environment:

| `connection_security` | What `psql` gets |
| --- | --- |
| `verified_tls` | `PGSSLMODE=verify-full`, and `PGSSLROOTCERT` with the file of certificate authorities |
| `verified_tls` with `dangerously_skip_server_certificate_check: true` | `PGSSLMODE=require`, with a root-certificate path that cannot exist so libpq loads no default certificate file |
| `unencrypted` | `PGSSLMODE=disable` |

- **With `database_server_host`, `psql` also gets `PGGSSENCMODE=disable`.**
  For an account with a Kerberos ticket, `psql` would otherwise take the
  encryption of Kerberos in the place of TLS, and verify no certificate.
- **With certificate verification enabled, MMWatchdog names a file of certificate authorities
  to `psql`:** the configured `database_server_certificate_authority_file`,
  or the node's own file.
  `psql` knows the node's file by itself only from PostgreSQL 16.
- **`psql` refuses a server that does not keep to the value** before it
  logs in: with `verify-full` a certificate that is not valid for the name,
  with `require` a server without encryption. The errors from `psql` are in
  [errors and what they mean](#errors-and-what-they-mean).

### How far a replica is behind

`replication_lag_seconds` is how far a replica is behind its primary, in
seconds.

- **A primary is 0 seconds behind:** it is behind no server. One check with
  a limit on the lag then fits the primary and its replicas, and stays right
  when a replica is made the primary. The message says whether the server
  is a primary or a replica.
- **A replica that has applied every change that it received, and that is
  connected to its primary,** is 0 seconds behind, however long ago the
  primary made its last change.
- **Otherwise the reading is the age of the last change that the replica
  has applied.** So a replica that lost its primary falls further behind
  with every second, and a limit catches it. The message then adds `and it
  is not connected to its primary now`. A replica with `primary_conninfo`
  set and no row in `pg_stat_wal_receiver` fails even without a lag limit.
  A standby that only replays copied WAL (`primary_conninfo` empty) does
  not fail for having no receiver.
- **A new replica that has not applied a change yet has no reading,** never
  the 0 of a primary. A check with a limit on the lag is then unknown, with
  `the PostgreSQL replica has not applied a change of its primary yet`.

### Connections in use

`connections_used_percent` is the open connections as a percent of the
server's `max_connections`, 0 to 100. With the administrator or a member
of `pg_read_all_stats`, MMWatchdog counts only client connections because
`max_connections` limits client connections. With a role that cannot read
all sessions' statistics, the count also includes the server's own workers. The extra worker processes
make the reading a little high.

### The cache hit ratio

`cache_hit_percent` is the percentage of block requests served from the
server's shared buffers between consecutive runs, 0 to 100. MMWatchdog
reads PostgreSQL's counters `blks_hit` and `blks_read` of `pg_stat_database`,
summed over the server's databases, because the buffers are one for the
whole server.

- **`cache_hit_percent` covers the time since the previous run,** as the hit
  ratio of [`memcached`](check_memcached.md) does, so `check_every` sets the
  window: a check every hour measures the last hour. The first run after
  MMWatchdog starts samples the counters twice, 0.2 seconds apart.
- **A block from outside the buffers is not always read from the disk:**
  the page cache of the operating system may hold it. A ratio below about
  99 percent on a busy server still means that the data that the
  application uses most no longer fits in the buffers.
- **PostgreSQL adds a session's reads to the counters when its transaction
  ends,** at most once a second. The reads of a statement that runs for an
  hour count in the run after the statement ends. A longer `check_every`
  smooths the effect of delayed reads.
- **After the server starts, its buffers are empty,** so the ratio is low
  until the application has read its data again.
- **The SQL statement that MMWatchdog sends reads from the buffers too.**
  On a server without other work, each run's connection adds a few hundred
  hits and no reads, so an idle server reads close to 100 percent.
- **A time in which the server read no block has no ratio,** never one of 0
  or 100. A check with a limit on `cache_hit_percent` is then unknown,
  with `the PostgreSQL
  server read no blocks in the last 0.200s, so cache_hit_percent cannot be
  measured`.
- **Counters that fell,** after `pg_stat_reset()` or a crash of the server,
  start a new sample, as on a first run.

### How long the write-ahead log waits to be archived

`wal_archive_lag_seconds` is how long the oldest finished segment of the
write-ahead log has waited to be archived, in seconds, and 0 when none
waits. PostgreSQL marks each finished segment that waits with a file in
`pg_wal/archive_status`, which `pg_ls_archive_statusdir()` lists with the
time it was made. A segment waits until `archive_command`, such as
`pgbackrest archive-push`, has succeeded for it.

- **A server without writes finishes no segment,** so nothing waits, and
  the reading is 0. The time since the last archived segment would grow on
  such a server and warn on every quiet day. `wal_archive_lag_seconds`
  grows only while a segment waits.
- **After three failed attempts, PostgreSQL waits up to a minute** before it
  tries again, so the reading can stay above 0 for up to a minute after the
  archive works again.
- **A replica is 0 seconds behind** unless its
  `archive_mode` is `always`, because its primary archives. The check with
  a `wal_archive_lag_seconds` limit stays right when a replica is made the
  primary. A former primary that became a replica can still hold segments
  that waited to be archived at that moment, and no archiver runs on a
  replica to send them, so they do not count until
  it is the primary again. With `always`, the replica archives too, and
  its own wait counts.
- **A primary with `archive_mode` off has no reading:** it archives nothing,
  so nothing waits, and a 0 would hide that. A check with a limit on
  `wal_archive_lag_seconds` is unknown, with
  `archive_mode is off, so the PostgreSQL server archives no
  write-ahead log and wal_archive_lag_seconds cannot be measured`.
- **A check with `login_role` does not measure
  `wal_archive_lag_seconds`.**
  PostgreSQL lists the waiting segments only to its administrators and to
  members of `pg_monitor`, and it refuses the whole statement of a role
  without that right before it runs any of it. The archive belongs to the
  server on its own node, which a check without a login asks as the
  administrator.

### How far the transaction IDs are from wraparound

PostgreSQL gives each writing transaction a 32-bit number, called a
transaction ID. Rows keep transaction IDs so PostgreSQL can tell which
changes a reader may see. The counter eventually wraps around and reuses
numbers. Vacuum must freeze old rows, marking those rows as visible to
all transactions, before the reused numbers could make old rows appear new.

`transaction_ids_used_before_wraparound_percent` measures the oldest
unfrozen transaction ID across every database, including template databases:
`100 * max(age(datfrozenxid)) / 2147483648`, from `pg_database`.
The reading is a percentage from 0 to 100. Near 100, PostgreSQL refuses
writes until a vacuum finishes, while reads keep working.

PostgreSQL starts its own emergency vacuum near 9 percent:
`autovacuum_freeze_max_age` defaults to 200 million transactions.
A healthy server reads about 5 to 10. A reading that keeps rising means
vacuum is falling behind. A limit of 50 leaves about a billion
transactions. On a busy server, that can mean weeks, giving a vacuum
that may take hours time to finish. The time left depends on how quickly
the server uses transaction IDs.

Every check measures the age, with or without `login_role`, because every
role may read `pg_database`. A replica reads the number copied from its
primary. No limit applies unless the check states one. An age that cannot
be parsed has no reading.

PostgreSQL's [wraparound guide](https://www.postgresql.org/docs/current/routine-vacuuming.html#VACUUM-FOR-WRAPAROUND)
explains freezing and the refusal of writes. Its
[vacuum settings](https://www.postgresql.org/docs/current/runtime-config-vacuum.html#GUC-AUTOVACUUM-FREEZE-MAX-AGE)
explain the emergency vacuum threshold.

### Messages

- **A healthy run says what the server is:** `the PostgreSQL server
  answers; it is a primary`, or `the PostgreSQL server answers; it is a
  replica, 2.5 seconds behind its primary`. With a limit, the message names
  each reading that has a limit before the message about the server's role
  and replication lag.
- **A server that does not answer, and a server that refuses the login,**
  fail the check with the first line of what `psql` says, such as `the
  PostgreSQL server did not answer the check: psql: error: connection to
  server on socket "/var/run/postgresql/.s.PGSQL.5432" failed: No such file
  or directory`. `psql` puts the reason at the end of its line, so of a line
  of more than 300 bytes the message keeps the first 100 and the last 200
  bytes, with ` ... ` between them.

## Errors and what they mean

The validation errors name the setting, such as
`check_groups.database_machine.checks.postgresql_on_the_database_machine_answers.postgresql.connection_security`.
In the list below, validation errors start at the module's input name
and omit the check group and check path:

- **`connection_security: required with database_server_host: write connection_security: verified_tls to encrypt the connection, or connection_security: unencrypted to send without encryption`.** State how the connection to the server
  is protected. Use `verified_tls` wherever the server has a certificate
  that a certificate authority signed.
- **`database_server_host: a check of a server on another machine logs in
  with a password: state login_role and login_password_file`.**
- **`connection_security: has no use without database_server_host: the
  check uses the server's local socket on this node, which has no
  encryption and needs none`,** and the same for
  `database_server_certificate_authority_file`. Remove the setting, or add
  `database_server_host`.
- **`database_server_certificate_authority_file: has a use only with connection_security:
  verified_tls, which verifies the server's certificate`.**
- **`database_server_host: expected the name or the address of the database
  server, without a port`.** Write the port in `database_server_port`.
- **`database_server_port: expected a port number from 1 to 65535`.**
- **`login_role: required with login_password_file: the account that the
  check logs in as`,** and **`login_role: expected one word of at most 256
  bytes`.**
- **`login_password_file: must be absolute`,** and the same for
  `database_server_certificate_authority_file`. Write the whole path.
- **`login_password_file: open /etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring: no such file or directory`,**
  from `mmwatchdog validate`. A run is unknown with
  `open /etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring: no such file or directory`,
  without `login_password_file:`. Create
  `/etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring`.
  A file that does not hold exactly one line gives `/etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring: expected one line with the
  password: UTF-8 text of at most 1,024 bytes, without a tab or another
  control character`.
- **`connection_security: verified_tls needs the certificate authorities
  that this node trusts, and this node has no file of them: MMWatchdog looked for
  ...`.** Install the distribution's package of certificate authorities,
  `ca-certificates`, or state `database_server_certificate_authority_file`.
- **`database_server_certificate_authority_file: the account mmwatchdog, which runs the client
  program of the database server, cannot read
  /etc/mmwatchdog/secrets/database-authority.pem. A certificate is no secret: give
  every account the permission to read the file and to pass through the
  directories above it`.** Move the file to a directory
  that every account can pass through, such as `/etc/mmwatchdog`, and let every
  account read it. A file without certificates gives `contains no PEM
  certificates`.
- **`SSL error: certificate verify failed`.** `psql` does not know the
  certificate authority of the server's certificate. State
  `database_server_certificate_authority_file`, or check that the server shows the right
  certificate.
- **`server certificate for "db-1.internal.example.com" does not match host
  name "db-1"`.** The certificate is for another name. Write in
  `database_server_host` a name that the certificate holds.
- **`MMWatchdog cannot run psql, the client program of PostgreSQL: executable file
  not found in $PATH. The check needs that program on this node`.** Install
  `psql`.
- **`MMWatchdog cannot run psql as the account mmwatchdog: ...`.** The program exists, and
  the operating system did not start `psql` as `mmwatchdog`, for example
  because the node has no account `mmwatchdog`. The run is unknown.
- **`the PostgreSQL server did not answer the check: psql ended with exit code 2`.**
  The client program failed and wrote nothing. The check fails.
- **`the PostgreSQL server did not answer within the time limit of the
  run`.** The server, or the network to it, is too slow for the check
  group's `time_limit_per_run`.
- **`psql gave an answer that MMWatchdog does not understand: ...`.** The run is
  unknown. The answer follows in the message.
- **`a check with login_role does not measure wal_archive_lag_seconds: ...`.**
  The run is unknown because a limit names the measurement. Check the
  server on its own node without `login_role`.
- **`archive_mode is off, so the PostgreSQL server archives no write-ahead
  log and wal_archive_lag_seconds cannot be measured`.** The run is
  unknown because a limit names the measurement. Turn on `archive_mode`,
  or remove the limit from a server that does not archive.

## Tested on

- **Go tests** in `server_checks_test.go`, with a stand-in for `psql`:
  `TestPostgreSQLCheck` covers the arguments and the environment of `psql`
  for the local socket and for a login, a primary that is 0 seconds behind,
  a replica that is level, behind, without its primary, and without a
  change yet, the transaction-ID age as a percentage with and without a
  login, a limit on that percentage, an unreadable age, the other limits,
  a refusing server, an answer that MMWatchdog does not
  understand, a password file that cannot be read, and a node without
  `psql`. `TestADatabaseCheckProtectsItsConnectionAsItStates` covers what
  `psql` gets for each `connection_security`, the node's file of
  certificate authorities and the check's own, a node without such a file,
  a file without certificates, and a file that the account of the client
  cannot read, in a run and in validation on the node. The tests record as
  which account MMWatchdog runs `psql`: `postgres` without a login, `mmwatchdog` with one.
  `TestPostgreSQLCheckMeasuresCacheHitsAndTheArchiveWait` covers the cache
  hit ratio of a first run without reads, of 99 and of 50 percent since the
  run before against a limit, a segment that has waited 412.5 seconds, a
  primary with `archive_mode` off, a replica, and a check that logs in.
  The statements of the check with and without a login ran against a real
  PostgreSQL 15 server, as its administrator and as a role without
  `pg_monitor`.
- **Real servers:** the `services` suite (`service_scenarios.py`) asks
  PostgreSQL as each distribution installs it, through the local socket with
  no setup and through a login with a password, stopped and started. With a
  certificate that a certificate authority of the test signed, it asks with
  each `connection_security`: a certificate that the check trusts, one of
  an authority that the guest does not know, one for another name,
  encryption without verification, a server with its encryption turned
  off, a server that takes only encrypted connections, and a file of
  certificate authorities that the account `mmwatchdog` cannot read, for the check
  and for `mmwatchdog validate`. A second server follows the first as its
  replica: level, behind while its replay is paused, without its primary,
  and made a primary. The latest run passed on [every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution),
  with `psql` 13 to 18 ([validation record](../../docs/changing-mmwatchdog/validation.md)).
- **Not verified on a real node:** a database server named by an address of
  IP version 6.

## Why it works this way

- **Permission refusals belong to validation and reload.** Startup warns
  once per file. A running check continues
  after a permissions mode change so the permission rule cannot stop
  monitoring. The
  [credential permission rule](../../docs/reference/configuration-files.md#secret-files)
  owns the settings and the reasons.

- **A disconnected streaming replica fails without a limit.** The replica
  cannot receive new changes; a copied-WAL standby has no streaming
  connection to require.

- **Certificate checking is the default.**
  `dangerously_skip_server_certificate_check: true` makes the choice to
  trust any server explicit, while TLS still prevents people who can only
  observe network traffic from reading the connection's contents.

- **A network server's port is required.** With `database_server_host`,
  state `database_server_port` so the connection address is visible even
  when the server uses its usual port.

- **The check asks through `psql`,** as MMX's module for PostgreSQL does,
  and has MMX's names for the connection. MMWatchdog needs no library for
  PostgreSQL, and a node that has the server has the program.
- **Without a login, the check asks as the server's own administrator
  through the local socket.** PostgreSQL accepts the operating system
  account `postgres` as the database role `postgres` without a
  password, so the most common check, of the server on the node itself,
  needs no account, no password, and no setup. The check sends only a
  statement that reads.
- **With a login, `psql` runs as the unprivileged account `mmwatchdog`.** The check
  then needs no rights on the node.
- **A check with `database_server_host` must state `connection_security`.** `psql` decides by itself when nothing tells
  it, and PostgreSQL as Debian installs it makes a certificate for itself
  that no certificate authority signed. The design explains the reasons for explicit connection protection
  and for accounting for an unsigned certificate for every check of a
  common service that encrypts, in
  [protocols and common server software](../../docs/changing-mmwatchdog/design/checks.md#protocols-and-common-server-software).
- **`unencrypted` is allowed with a password.** A login to the server on
  the node through `127.0.0.1` stays on the node. Any host is accepted,
  including across a private tunnel; the setting makes the choice visible.
- **MMWatchdog names a file of certificate authorities when certificate checking is enabled.** `psql` knows the node's file by itself only from
  PostgreSQL 16.
- **MMWatchdog checks itself whether `mmwatchdog` can read `database_server_certificate_authority_file`.**
  When `mmwatchdog` cannot read the certificate-authority file, `psql`
  gives no reason for the failure.
- **A primary is 0 seconds behind,** so that one check with a limit fits
  every server of a replication.
- **A replica without a number has no reading, never 0.** A 0 would say
  that the replica is level when MMWatchdog does not know it.
- **The cache hit ratio covers the time since the run before,** not the
  time since the server started. A server that has run for weeks would
  otherwise show a change in the data that the application reads most
  often only weeks later.
- **The archive's wait is the age of the oldest segment that waits,** not
  the time since the last archived segment, which grows on a server
  without writes. A team's first servers often have no traffic for weeks.
- **A primary with `archive_mode` off has no archive reading, never 0.** A
  0 would say that nothing waits when the server archives nothing.
- **The oldest database decides the server's wraparound risk, including
  templates,** because one database with old unfrozen rows can stop writes
  across the server. The denominator is 2^31 (2147483648), half the 32-bit
  range, because PostgreSQL compares transaction IDs within half that range
  to tell older transactions from newer transactions.
- **The message keeps the end of a long line of `psql`.** The reason is at
  the end of the line, behind the name and the address of the server.
