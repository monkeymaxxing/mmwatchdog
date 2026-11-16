# `mongodb`

`mongodb` checks a MongoDB server. The `mongodb` module is an
[official extra module](../../docs/reference/modules/README.md): a
Starlark script that the MMWatchdog project writes. Every deploy with the
release's MMX workflows installs the script on every node. A check names the module, and MMWatchdog
runs the module once for each run of the check, on the node.

The module asks the server through MongoDB's own shell, `mongosh`, whether
the server answers and how many connections are in use. For a replica-set
member, the module also asks whether the member is healthy and how far
the member is behind.

A **replica set** is a group of MongoDB servers that hold the same data.
One member, the **primary**, takes the changes, and the other members,
the **secondaries**, follow the primary.
An **arbiter** is a member that holds no data and only votes.

Choose `mongodb` for a MongoDB server. To check only that something listens
on the server's port, the core module [`tcp`](../../docs/reference/modules/tcp.md) is
enough.

## Set up

- **The `mongosh` program,** MongoDB's own shell. A node with a MongoDB
  server usually has `mongosh`. On a node that checks a server on another
  machine, install the package `mongodb-mongosh`.
- **For a server with access control, an account with the role
  `clusterMonitor`,** and a file with the account's connection string.
  Without a connection-string file, the check asks the server on the node
  itself. Unless access control is on, the local server accepts a
  connection from the node running the check without a login.
- **The connection string can hold a password,** so it is in a file and
  never in the configuration. MMWatchdog reads the file as root and gives its
  contents to the module. The check runs as the Linux account `mmwatchdog`
  by default, unless the check states `run_as_user_account`.
- **Keep the file in `/etc/mmwatchdog/secrets`, owned by root with mode `0600`.**
  The account of the check does not need access to `/etc/mmwatchdog/secrets`.

```sh
sudo install -d -o root -m 0700 /etc/mmwatchdog/secrets
sudo install -o root -m 0600 /dev/null /etc/mmwatchdog/secrets/mongodb-connection-string
sudo nano /etc/mmwatchdog/secrets/mongodb-connection-string    # one line: mongodb://mmwatchdog_monitoring:PASSWORD@127.0.0.1:27017/
```

- **Encode separating characters in the user name and password before
  writing the connection string,** as MongoDB requires.
  Replace `@` with `%40`, `:` with `%3A`, and `/` with `%2F`.
  The module refuses unencoded `/`, `@`, `?`, `#`, `[` or `]` in the user
  name or password, and unencoded colons in the password.
- **`mongosh` keeps files of its own.** For each start, `mongosh` writes a
  log file into `.mongodb/mongosh/` in the home directory of the check's
  Linux account. The `mongosh` program removes old shell log files by
  `mongosh`'s own rule.
  MongoDB's shell also sends usage data to MongoDB unless usage reporting
  is turned off. The module automatically turns off usage reporting for
  the account of the check (`disableTelemetry()`). The file
  `/etc/mongosh.conf` can turn off usage reporting for the whole node.

## Inputs

| Input | Meaning | When omitted |
| --- | --- | --- |
| `connection_string_file` | The absolute path of a file that holds one MongoDB connection string, such as `mongodb://mmwatchdog_monitoring:PASSWORD@127.0.0.1:27017/`, and nothing else. MMWatchdog reads the file as root and gives its contents to the module | The check asks the server at `mongodb://127.0.0.1:27017/` without a login |

`mmwatchdog validate` and `mmwatchdog reload` enforce the
[credential permission rule](../../docs/reference/configuration-files.md#secret-files).
Place permission settings beside `mongodb`, where `alert_severity` stands.

## Measurements

| Measurement | Unit | Meaning |
| --- | --- | --- |
| `connections_used_percent` | percent | The open connections as a share of the connections that the server allows, 0 to 100 |
| `replication_lag_seconds` | seconds | For a member of a replica set: how far the member is behind the primary. The primary is 0 seconds behind. A server outside a replica set has no lag measurement |

A check can set limits on `connections_used_percent` and
`replication_lag_seconds`, with `fail_when_measurement_above` or
`fail_when_measurement_below` beside the module. MMWatchdog applies the
limits ([checks that measure a number](../../docs/reference/configuration-files.md#checks-that-measure)).
If a check limits `replication_lag_seconds` but the run has no lag
measurement, the check is unknown. For example, a server outside a replica
set has no lag measurement, nor does a member whose replica set has no
primary.

## When the check fails, and when it cannot check

| What the server says | The run | Message |
| --- | --- | --- |
| The MongoDB server answers, and is in no replica set | Healthy | `the MongoDB server answers; it is a server of its own, in no replica set` |
| The MongoDB server answers as the primary, a secondary, or an arbiter of a replica set | Healthy | `the MongoDB server answers; it is a member of the replica set rs0 in the state SECONDARY` |
| The MongoDB server is a member in another state, such as `RECOVERING` or `STARTUP2` | Failed | A message saying the server answers and naming the replica set and member state, followed by `, which is neither PRIMARY, SECONDARY, nor ARBITER` |
| The MongoDB server cannot report the server's replica set status, including a refused `replSetGetStatus` command | Unknown | `MongoDB could not report its replica set status: ` and the reason, without the connection string or password |
| The checked MongoDB server cannot reach another member of the checked server's replica set | Failed | A message saying the server answers and naming the replica set and member state, followed by `; this member cannot reach db-2:27017` |
| It was started for a replica set and is a member of none: nobody set the replica set up, or the server was removed from it | Failed | `the MongoDB server answers and takes no changes: it was started for a replica set and is a member of none`, and the server's own words in parentheses |
| The MongoDB server does not answer, or refuses the login | Failed | `the MongoDB server did not answer the check: ` with the shell's reason, or `mongosh ended with exit code N` when the shell names none |
| A measurement is above its limit | Failed | `replication_lag_seconds is 42 (fails above 30); the module reported: ...` |
| The file cannot be read or holds no valid connection string, or the node has no `mongosh` | Unknown | See [errors and what they mean](#errors-and-what-they-mean) |

- **A server that is gone fails the check, with the shell's reason, and is
  not unknown,** as long as the check's `time_limit_per_run` is above the
  time for which the shell looks for the server. The shell's server search
  lasts 10 seconds, unless the connection string states a different
  `serverSelectionTimeoutMS`.
- **Neither the connection string nor its password appears in a message.**
  The module replaces the connection string with `the server's address`
  and the password with `(the password)`.

MMWatchdog sends the alert when the check has failed for `alert_after_failing_for`,
and opens a cannot-check incident when it has been unknown that long
([the failing time](../../docs/how-to/decide-when-a-check-alerts.md)).

## Examples

### The server on this node, and a member that keeps up

```yaml
# /etc/mmwatchdog/check_groups/mongodb.yml
check_groups:
  mongodb:
    check_every: 1m
    time_limit_per_run: 20s
    alert_after_failing_for: 3m
    recovered_after_healthy_for: 1m
    checks:
      mongodb_answers:
        mongodb: {}                   # the server on this node, without a login
        alert_severity: sos
      mongodb_member_is_less_than_30_seconds_behind:
        mongodb:
          connection_string_file: /etc/mmwatchdog/secrets/mongodb-connection-string
        fail_when_measurement_above:
          replication_lag_seconds: 30
        alert_severity: warn
```

Every minute MMWatchdog runs `mongodb_answers` and
`mongodb_member_is_less_than_30_seconds_behind` on the node, as the account
`mmwatchdog`. The `mongodb_answers` check asks the server on the node
itself, without a login. The `mongodb_member_is_less_than_30_seconds_behind`
check logs in with the connection string from
`/etc/mmwatchdog/secrets/mongodb-connection-string`. The lag check fails
when the member is more than 30 seconds behind the primary.

Every member of a replica set can use the lag check: the primary is 0
seconds behind, and which member is the primary changes by itself.
A server that stops answering sends the `sos` alert after three minutes.
The 20-second run limit exceeds the shell's 10-second server search, so
the alert includes the shell's reason.

### Connections in use on a server with a login

```yaml
# /etc/mmwatchdog/check_groups/mongodb.yml
check_groups:
  mongodb:
    check_every: 1m
    time_limit_per_run: 20s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 5m
    checks:
      mongodb_has_free_connections:
        mongodb:
          connection_string_file: /etc/mmwatchdog/secrets/mongodb-connection-string
        fail_when_measurement_above:
          connections_used_percent: 80
        alert_severity: warn
```

The `mongodb_has_free_connections` check logs in as the connection
string's account, which has the role `clusterMonitor`. The check fails
when the server answers but more than 80 percent of the allowed
connections are open for five minutes. The check also fails when the
server does not answer. The check works on a server outside a replica set
because the check sets no limit on `replication_lag_seconds`.

## How it works

Each run of the check is one run of the script `module.star`, as the account
of the check, under the check's `time_limit_per_run`:

1. MMWatchdog reads `connection_string_file` as root, when the check states it,
   and gives its contents to the module. The module checks that the contents
   hold one connection string that starts with `mongodb://` or `mongodb+srv://`.
2. The module adds `serverSelectionTimeoutMS=10000` to the connection
   string, unless the string states its own.
3. The module starts `mongosh --nodb --quiet --norc` and gives the
   connection string to the shell's standard input, inside the questions.
   The connection string never appears as a program argument, because
   program arguments show in the process list.
4. The shell asks the server `hello` and `serverStatus`, and for a member of
   a replica set also `replSetGetStatus`. The `mongosh` shell writes the
   server's answers on one line that the module reads.

Each run starts `mongosh`, which takes about one second of processor time.
With `check_every` set to a minute, as in the examples, starting `mongosh`
uses little processor time. Many MongoDB checks that run every few seconds
keep a processor busy, and their runs take longer.

## Errors and what they mean

| Message | What the message means and what to do |
| --- | --- |
| `the MongoDB server did not answer the check: MongoNetworkError: connect ECONNREFUSED 127.0.0.1:27017` | Failed. Nothing listens at the address. Start the server, or correct the connection string |
| `the MongoDB server did not answer the check: MongoServerError: Authentication failed.` | Failed. The server refused the login. Correct the user name or the password in the file |
| `the MongoDB server did not answer the check: MongoServerError: Command serverStatus requires authentication` | Failed. The server asks for a login, and the check states no `connection_string_file`. Add the connection-string file |
| `MMWatchdog cannot read the file of the input connection_string_file of the module mongodb: ...` | Unknown. MMWatchdog could not read the file as root. Check that the file exists at the configured path |
| `MongoDB could not report its replica set status: ...` | Unknown. `replSetGetStatus` failed, so the module cannot establish replica-set health. The reason says whether to correct the monitoring account or the server |
| `the file that connection_string_file names must hold one MongoDB connection string that starts with mongodb:// or mongodb+srv://, and nothing else` | Unknown. Write one connection string into the file, on one line |
| `the connection string in the file that connection_string_file names has a user name or a password with a character that must be written in its percent form there, such as %40 for @, %3A for a colon, and %2F for a slash` | Unknown. Write that character of the user name or the password in its percent form |
| `MMWatchdog cannot run mongosh, the shell of MongoDB: .... The check needs that program on this node` | Unknown. Install `mongosh`, from the package `mongodb-mongosh` |
| `the result has no measurement named replication_lag_seconds, which this check has a limit on` | Unknown. The server is in no replica set, or its replica set has no primary now. Remove the limit for a standalone MongoDB server outside a replica set |

The errors that every module that is not core can meet, such as a run past
`time_limit_per_run`, are in
[checks with a module that is not core](../../docs/reference/configuration-files.md#run-settings-of-a-shell-check-and-of-a-module-that-is-not-core).

## Tested on

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what MMWatchdog cannot do on each distribution, or did not test there, and why.

- **On every target distribution except Rocky Linux 9 and AlmaLinux 10, whose repositories have no Docker (it comes from Docker's own repository), against MongoDB 8.0 itself, and 8.2 on
  Linux 6.19 and newer** (`tests/integration/service_scenarios.py`, in the
  services suite). Debian has no MongoDB package, so the scenario runs five servers
  in containers of MongoDB's own image `mongo:8.0`.
  The scenario copies `mongosh` from the image onto the guest.
  MongoDB 8.0 refuses to start on Linux 6.19 and newer.
  On such a kernel, such as Linux 7.0 of Ubuntu 26.04, the image is `mongo:8.2`.

  The check runs on the guest and asks:

  - A server outside a replica set.
  - A server with a login whose password is in percent form.
  - A wrong password, which never shows.
  - A connection string that the module refuses.
  - A replica set of two members and an arbiter, before and after setup.
    The primary is 0 seconds behind.
    The secondary falls behind while the secondary applies nothing.
    Members name the member that the members cannot reach.
    The secondary takes the place of a primary that is gone.

  A guest without the shell is unknown
  ([every target distribution](../../docs/changing-mmwatchdog/testing.md#every-target-distribution)).
- **Go tests** (`tests/official_extra_modules/check_modules_test.go`,
  `TestMongoDB`) run the script through `mmwatchdog run-starlark-module`
  with a stand-in for `mongosh`. The Go tests cover:

  - The answer behind the shell's prompts.
  - A server outside a replica set.
  - A secondary with lag, a primary with lag of 0, and an arbiter.
  - A member in another state, and a member that cannot reach another member.
  - A server started for a replica set but belonging to none.
  - A failed replica-status command.
  - The root-read connection string on standard input, in no argument and no message.
  - The time after which the shell gives up.
  - A password with a character that separates connection-string parts.
  - A server that does not answer, and a missing `mongosh`.

  The Go tests ran with `go test ./...` on macOS on ARM64.

Not verified: a replica set with a login, and a server that is reached over
TLS ([validation record](../../docs/changing-mmwatchdog/validation.md#not-verified-for-this-build)).

## Why it works this way

- **The module asks through `mongosh`.** The `mongosh` program is MongoDB's
  own shell, and a node with a MongoDB server usually has `mongosh`.
- **The connection string reaches the shell on its standard input.** The
  arguments of a program show in the process list, where other accounts
  could read a password.
- **The module refuses a separating character in the user name or the
  password.** The shell would take a piece of the password for another part
  of the connection string, and then show that piece in an error.
- **The shell looks for the server for 10 seconds.** By itself, `mongosh`
  gives up after 30 seconds. A shorter time lets the check say that the
  server did not answer, within the check's time limit.
- **The primary is 0 seconds behind.** Which member is the primary changes
  by itself, so a check with a lag limit fits every member.
- **MMWatchdog reads the connection string as root.** The file can stay in
  `/etc/mmwatchdog/secrets` with mode `0600`, and the module needs no root account.
- **A failed replica-status command makes the run unknown.** A server that
  answers `hello` has not proved its replica set healthy. The reason helps
  correct the failure without exposing the connection string or password.
- **`mongodb` is an official extra module, not a core module.** A check of
  MongoDB is uncommon work. A script can follow `mongosh` in a new release
  without a new `mmwatchdog` program.
