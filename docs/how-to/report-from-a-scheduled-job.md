# Report from a scheduled job

**When to use this page:** a backup, or another job that a systemd timer
or cron starts, must tell you when it stops succeeding: when it fails, and
also when it stops running at all, such as after someone removed its timer.

**Who acts:** you, or your coding agent. Each step runs on your controller
machine, the machine where you run `mmx`, except where the step says "on the
node". A coding agent can do every step: `openssl` on the controller
machine makes the values that go into the vault, so nobody types a secret.

**What this page does:** you add a heartbeat check group to the MMX project
that installed MMWatchdog. After each success, the job sends MMWatchdog a
short message, a **heartbeat**. When no heartbeat arrives in the time that
you choose, MMWatchdog alerts, whatever the reason: the job failed, ran too
long, or did not run. The next heartbeat ends the problem.

**Terms:** the **heartbeat socket** is the file
`/run/mmwatchdog/heartbeat.sock` on the node, a Unix socket: a job on the
same node sends its heartbeats to MMWatchdog through it, with no
certificate, port, or address. The **token** is a random text that the job
sends with each heartbeat, so that MMWatchdog knows that the heartbeat
comes from your job. The **deadline** is the time by which the next
heartbeat must arrive.

## Choose the route

| Your situation | Section |
|---|---|
| The job runs on the node, and it is the first job there that sends heartbeats | [Steps](#steps) |
| The node already has a heartbeat check group, and you add another job | [Add another job](#add-another-job) |
| The job runs as an account other than root | [Steps](#steps), then [If something is different](#if-something-is-different) |
| The job runs on another machine | [Report from another machine](#report-from-another-machine) |

## Before you start

- MMWatchdog runs on the node, installed with MMX from the MMX project on
  your controller machine, with an alert destination such as a Slack
  channel. If not, follow
  [Install MMWatchdog on one node](install-on-one-node.md) first.
- The job exits with code 0 only when it succeeded. Reason: the heartbeat
  goes out after an exit code of 0, so a job that ends with 0 after a
  failure still reports success.
- `openssl` is installed on the controller machine: `openssl version`
  prints its version.
- Open a terminal in the MMX project: the directory that holds
  `mmx_project_settings.yml`.

## Steps

This page uses a nightly backup of the application `myapp`, run on the
node `app01` by the systemd service `backup-myapp.service` and its timer,
and a check group named `nightly_backup`.

1. **Make the job's token:**

   ```sh
   openssl rand -hex 32
   ```

   ```text
   58ba5ed9832159481c44241f0fe1d3fc923362d7e7ea0f507c2bbbad09785b93
   ```

   Reason: anyone who has the token can report success for this job, so
   the token is random and long. Each heartbeat check group needs a token
   of its own.

   **Check:** the output is one line of 64 characters. MMWatchdog takes a
   token of 16 to 4096 bytes on one line.

2. **Put the token in MMX's vault.** MMX keeps secrets in the encrypted
   file `secrets_vault.age` in the project, and writes them to the node
   during each install. Open the vault in your editor:

   ```sh
   mmx secrets edit
   ```

   Under `mmwatchdog_secret_files`, beside the entries that are there, add
   your token:

   ```yaml
   mmwatchdog_secret_files:
     slack-webhook-team: "https://hooks.slack.com/services/YOUR/WEBHOOK/ADDRESS"
     nightly-backup-heartbeat: "58ba5ed9832159481c44241f0fe1d3fc923362d7e7ea0f507c2bbbad09785b93"
   ```

   Save, and close the editor.

   **Check:** `mmx secrets edit` prints `Saved encrypted vault:
   secrets_vault.age`.

3. **List the token's file in the inventory.** In
   `inventories/hub_and_standalone.yml`, under `vars`:

   ```yaml
   vars:
     mmwatchdog_config_source_dir: files/hub_and_standalone
     mmwatchdog_secret_files: [slack-webhook-team, nightly-backup-heartbeat]
   ```

   Each install writes each listed file to `/etc/mmwatchdog/secrets/` on
   the node, readable only by root.

   **Check:** each name in the list is a name under
   `mmwatchdog_secret_files` in the vault.

4. **Write the heartbeat check group.** Create
   `files/hub_and_standalone/check_groups/nightly_backup.yml`:

   ```yaml
   # files/hub_and_standalone/check_groups/nightly_backup.yml
   check_groups:
     nightly_backup:
       heartbeat:
         max_time_between_heartbeats: 26h   # alert when no heartbeat came for 26 hours
         expected_bearer_token_file: /etc/mmwatchdog/secrets/nightly-backup-heartbeat
       alert_severity: sos
   ```

   - `max_time_between_heartbeats` is the time from one heartbeat to the
     deadline of the next. Choose the time between two runs of the job,
     plus the longest that a run may take, plus some margin. For a nightly
     job, `26h` leaves 2 hours for a slow night.
   - `expected_bearer_token_file` names the token's file on the node, from
     step 3.
   - `alert_severity` stands on the check group, beside `heartbeat`.
   - A heartbeat check group holds only the heartbeat. It takes none of the
     four timings of other check groups, such as `check_every`: the deadline
     is its only rule. Alerts go out at the deadline and end at the next
     heartbeat.

   With this check group, the node gets the heartbeat socket. The socket
   has no setting: it exists whenever the configuration has at least one
   heartbeat check group.

   **Check:** the file has `heartbeat` with its two settings, and an
   `alert_severity` of `info`, `warn`, or `sos`.

5. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output has a line ending in
   `would create "/etc/mmwatchdog/secrets/nightly-backup-heartbeat"`,
   and:

   ```text
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked input /etc/mmwatchdog/check_groups/nightly_backup.yml would change
   ```

   The dryrun exits with code 2: something would change. It does not check
   the new configuration: step 6 does.

6. **Install the change:**

   ```sh
   mmx run hub_and_standalone
   ```

   **Check:** the run ends with `Run ...: succeeded`. If it ends with
   `failed`, see [If something is different](#if-something-is-different).
   Then, on the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status nightly_backup
   ```

   ```text
   nightly_backup/heartbeat     waiting_for_first_heartbeat no heartbeat has arrived yet
     Config: /etc/mmwatchdog/check_groups/nightly_backup.yml:4:5
     Rule: next heartbeat due by 2026-11-16T10:39:09Z
     Alert severity: sos
   Alert deliveries: 0 waiting, 0 dropped
   ```

   The first deadline counts from the install: if no heartbeat arrives
   within `max_time_between_heartbeats`, here by 10:39 UTC the next day,
   MMWatchdog alerts.

   The heartbeat socket is there too:

   ```sh
   ssh deploy@app01 ls -l /run/mmwatchdog/
   ```

   ```text
   total 0
   srw-rw-rw- 1 root root 0 Nov 15 08:39 heartbeat.sock
   srw------- 1 root root 0 Nov 15 08:39 mmwatchdog.sock
   ```

   Every account on the node may connect to `heartbeat.sock`, and every
   heartbeat still needs the token.

7. **Make the job send a heartbeat after each success.** On the node, add
   an `ExecStartPost` line to the job's service, here
   `/etc/systemd/system/backup-myapp.service`:

   ```ini
   [Unit]
   Description=Back up myapp

   [Service]
   Type=oneshot
   ExecStart=/usr/local/bin/backup-myapp
   ExecStartPost=/usr/local/bin/mmwatchdog heartbeat nightly_backup --bearer-token-file /etc/mmwatchdog/secrets/nightly-backup-heartbeat
   ```

   - With `Type=oneshot`, systemd runs `ExecStartPost` only when every
     `ExecStart` command exited with code 0. So a failed backup sends no
     heartbeat.
   - Without `--receiver-url`, the command sends the heartbeat through the
     heartbeat socket. Before it sends the token, it checks that root owns
     the socket and its directory. Reason: then no other account can have
     put its own socket there to collect the token.
   - The service runs as root, so the command can read the token's file in
     `/etc/mmwatchdog/secrets/`. A job of another account needs its own
     copy: see [If something is different](#if-something-is-different).
   - The job's timer, here `backup-myapp.timer`, needs no change. A job
     that has no timer yet gets one, such as
     `/etc/systemd/system/backup-myapp.timer` for 02:00 each night; turn
     it on with `sudo systemctl daemon-reload` and
     `sudo systemctl enable --now backup-myapp.timer`:

     ```ini
     [Unit]
     Description=Run backup-myapp each night

     [Timer]
     OnCalendar=*-*-* 02:00:00
     Persistent=true

     [Install]
     WantedBy=timers.target
     ```

   For a cron job, join the two commands with `&&`, so that the heartbeat
   follows only a success, such as in `/etc/cron.d/backup-myapp`:

   ```text
   0 2 * * * root /usr/local/bin/backup-myapp && /usr/local/bin/mmwatchdog heartbeat nightly_backup --bearer-token-file /etc/mmwatchdog/secrets/nightly-backup-heartbeat
   ```

   Then load the changed service:

   ```sh
   sudo systemctl daemon-reload
   ```

   **Check:** `sudo systemd-analyze verify /etc/systemd/system/backup-myapp.service`
   prints nothing.

8. **Run the job once, and see the heartbeat arrive.** On the node:

   ```sh
   sudo systemctl start backup-myapp.service
   sudo journalctl -u backup-myapp.service -n 5
   ```

   ```text
   Nov 15 08:39:19 app01 systemd[1]: Starting backup-myapp.service - Back up myapp...
   Nov 15 08:39:19 app01 backup-myapp[16508]: backup done
   Nov 15 08:39:19 app01 mmwatchdog[16509]: Heartbeat accepted: nightly_backup
   Nov 15 08:39:19 app01 systemd[1]: backup-myapp.service: Deactivated successfully.
   Nov 15 08:39:19 app01 systemd[1]: Finished backup-myapp.service - Back up myapp.
   ```

   Then:

   ```sh
   sudo mmwatchdog status nightly_backup
   ```

   ```text
   nightly_backup/heartbeat     ok           heartbeat received
     Config: /etc/mmwatchdog/check_groups/nightly_backup.yml:4:5
     Rule: next heartbeat due by 2026-11-16T10:39:19Z
     Last heartbeat: 2026-11-15T08:39:19Z
     Alert severity: sos
   ...
   ```

   **Check:** the journal shows `Heartbeat accepted: nightly_backup`, and
   `status` shows `ok`, with the next deadline 26 hours after the last
   heartbeat. If the journal shows an error instead, see
   [If something is different](#if-something-is-different).

## What happens when the job stops succeeding

At the deadline, MMWatchdog alerts at once, at the check group's
`alert_severity`. The alert names the last heartbeat and the deadline. To
see the alert without waiting a day, set `max_time_between_heartbeats: 3m`,
install the change, run the job once, and wait three minutes without
running it again. This alert comes from such a test:

```text
SOS: app01: nightly_backup/heartbeat failed
heartbeat overdue: last heartbeat 2026-11-15T08:43:13Z; expected within 3m; deadline 2026-11-15T08:46:13Z
failing since 08:46 UTC
```

If no heartbeat came since the install, the alert says so:
`heartbeat overdue: no heartbeat since monitoring started 2026-11-15T08:39:48Z; expected within 3m; deadline 2026-11-15T08:42:48Z`.

The next heartbeat ends the problem at once, and each destination that got
the alert gets:

```text
Resolved SOS: app01: nightly_backup/heartbeat is ok again
heartbeat received
```

While the problem lasts, each destination's `remind_every` sends
reminders, as for any other check. On the node,
`sudo mmwatchdog events nightly_backup` lists each missed deadline and each
recovery, newest first:

```text
2026-11-15T08:46:13Z failed               nightly_backup/heartbeat heartbeat overdue: last heartbeat 2026-11-15T08:43:13Z; expected within 3m; deadline 2026-11-15T08:46:13Z
  Incident: ec73a244a7399a5dbca926455de7c7a2
  ...
2026-11-15T08:43:13Z recovered            nightly_backup/heartbeat heartbeat received
  Incident: 9ee98951e1a91669a84aba14af3a736a
  ...
```

Put the real `max_time_between_heartbeats` back after the test. A changed
time makes a new check, so an alert of the test that is still open closes
with this message instead of `Resolved`:

```text
Closed SOS: app01: nightly_backup/heartbeat incident closed, maybe not fixed
the check was changed in the configuration; MMWatchdog does not know whether the problem is fixed
```

## Add another job

The node already has the heartbeat socket. Each job gets its own check
group and its own token:

1. **Make the token:** `openssl rand -hex 32`.
   **Check:** one line of 64 characters.
2. **Put the token in the vault** with `mmx secrets edit`, under
   `mmwatchdog_secret_files`, such as
   `weekly-report-heartbeat: "..."`.
   **Check:** `Saved encrypted vault: secrets_vault.age`.
3. **Add the file's name to `mmwatchdog_secret_files`** in
   `inventories/hub_and_standalone.yml`.
   **Check:** each name in the list is a name in the vault.
4. **Write the job's check group,** such as
   `files/hub_and_standalone/check_groups/weekly_report.yml`, as in
   [step 4](#steps), with its own `max_time_between_heartbeats` and
   `expected_bearer_token_file: /etc/mmwatchdog/secrets/weekly-report-heartbeat`.
   **Check:** no other check group has the name.
5. **Preview and install:** `mmx dryrun hub_and_standalone`, then
   `mmx run hub_and_standalone`.
   **Check:** the run ends with `Run ...: succeeded`, and
   `sudo mmwatchdog status weekly_report` on the node shows
   `waiting_for_first_heartbeat`.
6. **Make the job send its heartbeat,** as in [step 7](#steps), with the
   new check group's name and token file.
   **Check:** after you run the job once, `status` shows `ok`.

## Report from another machine

A job on another machine cannot reach the heartbeat socket. It sends its
heartbeats over HTTPS to the **heartbeat receiver**: the part of
MMWatchdog on the node that takes heartbeats from the network. HTTPS needs
a **certificate** and its **key**. Reason for HTTPS: nobody on the way can
read the token.

First do steps 1 to 4 of [Steps](#steps) on your controller machine: the
token, the vault, the inventory, and the check group of the node that
receives the heartbeats. This section uses that node's address
`203.0.113.11` and port 8444. Port 8444 is an example: any free port works.

1. **Make the receiver's certificate and key:**

   ```sh
   openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
     -subj "/CN=app01" -addext "subjectAltName=IP:203.0.113.11" \
     -keyout heartbeat-receiver.key -out heartbeat-receiver.crt
   ```

   The command writes two files: the key `heartbeat-receiver.key`, which is
   secret, and the certificate `heartbeat-receiver.crt`, which is not. The
   certificate names the address where the other machine reaches the node,
   and is valid for 10 years (`-days 3650`). For a host name, write
   `DNS:app01.example.com` instead of `IP:203.0.113.11`; for several names,
   join them with commas.

   **Check:**

   ```sh
   openssl x509 -in heartbeat-receiver.crt -noout -ext subjectAltName
   ```

   ```text
   X509v3 Subject Alternative Name:
       IP Address:203.0.113.11
   ```

2. **Put the certificate and the key in the vault.** Run
   `mmx secrets edit`, and under `mmwatchdog_secret_files` add the contents
   of the two files. Indent each line of a file by four spaces under its
   name:

   ```yaml
   mmwatchdog_secret_files:
     slack-webhook-team: "https://hooks.slack.com/services/YOUR/WEBHOOK/ADDRESS"
     nightly-backup-heartbeat: "58ba5ed9832159481c44241f0fe1d3fc923362d7e7ea0f507c2bbbad09785b93"
     heartbeat-receiver.crt: |
       -----BEGIN CERTIFICATE-----
       MIIBhTCCASygAwIBAgIUeyhk7iNJEoOM17v9d9PO8PYxBR8wCgYIKoZI...
       ...
       -----END CERTIFICATE-----
     heartbeat-receiver.key: |
       -----BEGIN PRIVATE KEY-----
       ...
       -----END PRIVATE KEY-----
   ```

   The certificate is not secret, but it goes in the vault too. Reason:
   each install writes the vault's files to the node before it checks the
   configuration, and the check opens the certificate.

   **Check:** `mmx secrets edit` prints `Saved encrypted vault:
   secrets_vault.age`. Then delete the key from the project directory,
   because the vault holds it now:

   ```sh
   rm heartbeat-receiver.key
   ```

   Keep `heartbeat-receiver.crt`: the other machine needs a copy in step 7.

3. **List the two files in the inventory.** In
   `inventories/hub_and_standalone.yml`:

   ```yaml
   mmwatchdog_secret_files: [slack-webhook-team, nightly-backup-heartbeat, heartbeat-receiver.crt, heartbeat-receiver.key]
   ```

   **Check:** each name in the list is a name in the vault.

4. **Turn on the receiver.** Add this block at the end of
   `files/hub_and_standalone/mmwatchdog_node_settings.yml`:

   ```yaml
   # Where jobs on other machines send their heartbeats, over HTTPS.
   heartbeat_receiver:
     listen_on_address: 203.0.113.11:8444
     tls_certificate_file: /etc/mmwatchdog/secrets/heartbeat-receiver.crt
     tls_private_key_file: /etc/mmwatchdog/secrets/heartbeat-receiver.key
   ```

   `listen_on_address` is the node's address that the other machine
   reaches, or `0.0.0.0:8444` for every IPv4 interface. An address without
   a host, such as `:8444`, is refused.

   **Check:** `mmx validate` prints `Valid: mmwatchdog`.

5. **Open the port in the node's firewall for the other machine only.**
   Reason: the token protects the receiver, but a closed port is one thing
   fewer that strangers can try. The command depends on the node's
   firewall.

   **Check:** from a machine that is not the other machine, a connection
   to port 8444 fails.

6. **Install the change:** `mmx dryrun hub_and_standalone`, then
   `mmx run hub_and_standalone`. The install restarts MMWatchdog, because
   a new receiver needs a restart.

   **Check:** the run ends with `Run ...: succeeded`, and on the node
   `sudo ss -ltnp | grep 8444` shows the receiver:

   ```text
   LISTEN 0      4096    203.0.113.11:8444       0.0.0.0:*    users:(("mmwatchdog",pid=17872,fd=14))
   ```

7. **Put the token, the certificate, and the program on the other
   machine.** Put the token in a file of the job's account, with mode
   `0600`, and the certificate beside it. Copy the `mmwatchdog` program,
   the file `mmwatchdog` in the MMX project, to `/usr/local/bin/` there,
   or use `curl` as below.

   **Check:** `ls -l` in that directory shows the token's file with
   `-rw-------`.

8. **Make the job send its heartbeat over HTTPS,** as in
   [step 7](#steps), with `--receiver-url` and
   `--trusted-certificate-authority-file`. In the directory that holds the
   two files:

   ```sh
   mmwatchdog heartbeat nightly_backup --receiver-url https://203.0.113.11:8444 --bearer-token-file nightly-backup-heartbeat --trusted-certificate-authority-file heartbeat-receiver.crt
   ```

   ```text
   Heartbeat accepted: nightly_backup
   ```

   `--trusted-certificate-authority-file` makes the command trust your
   certificate from step 1. In a service or a cron line, write the files'
   full paths.

   **Check:** the command prints `Heartbeat accepted: nightly_backup`, and
   `sudo mmwatchdog status nightly_backup` on the node shows `ok`.

**The other machine has no `mmwatchdog` program.** Any HTTPS client works:
the job sends `POST /v1/heartbeats/CHECK_GROUP` with the token as a bearer
token, and the receiver answers `204` once it has saved the heartbeat. With
`curl`, in the directory that holds the two files:

```sh
curl --fail --silent --show-error --cacert heartbeat-receiver.crt -X POST \
  -H "Authorization: Bearer $(cat nightly-backup-heartbeat)" \
  https://203.0.113.11:8444/v1/heartbeats/nightly_backup
```

`curl` prints nothing and exits with code 0 on success. The token appears
in the machine's process list while `curl` runs; on a machine where other
people have accounts, use the `mmwatchdog` program instead.

## If something is different

- **The run failed at `validate_config`.** The output names the file, the
  line, and the fault, such as:

  ```text
  mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/nightly_backup.yml:8:5: check_groups.nightly_backup.check_every: does not apply to a heartbeat check
  ```

  The faults of this page's settings, and their fixes:

  | Error | Fix |
  |---|---|
  | `check_groups.nightly_backup.check_every: does not apply to a heartbeat check` | Remove the timing from the heartbeat check group |
  | `check_groups.weekly_report.heartbeat.expected_bearer_token_file: must use a different token for each heartbeat check group` | Make a new token for the second check group |
  | `check_groups.nightly_backup.heartbeat.expected_bearer_token_file: ...: expected a single token of 16..4096 bytes` | Put a token of 16 to 4096 bytes, on one line, in the vault |
  | `heartbeat_receiver: open /etc/mmwatchdog/secrets/heartbeat-receiver.crt: no such file or directory` | Only with a receiver: put the certificate and the key in the vault and the inventory, as in steps 2 and 3 of [Report from another machine](#report-from-another-machine) |

  Fix the files and run `mmx run hub_and_standalone` again. Nothing changed
  on the node.
- **The job's heartbeat fails.** When the command cannot connect, it tries
  three times in about 3 seconds. Then, and after any other error, it exits
  with code 4. A refused heartbeat (a wrong token, or no heartbeat check
  group of that name) exits with code 1 at once, so the job's log tells a
  setup fault from an outage. Nobody keeps the heartbeat to send later: the next success
  sends the next one. With `ExecStartPost`, systemd then marks the job's
  service `failed`, although the backup itself succeeded. The command's
  errors:

  | Error | Fix |
  |---|---|
  | `there is no heartbeat socket /run/mmwatchdog/heartbeat.sock: MMWatchdog is not running on this node, or its configuration has no heartbeat check group` | Start MMWatchdog, or install a heartbeat check group, as in steps 4 to 6. A job on another machine needs `--receiver-url` |
  | `the heartbeat was not sent, because the heartbeat socket /run/mmwatchdog/heartbeat.sock is not safe for the token: its directory /run/mmwatchdog can be written by accounts other than root. ...` | Someone changed the socket or its directory. The message names what is wrong; for this example, run `sudo chmod 755 /run/mmwatchdog`. The command sends nothing until root owns the socket and its directory, neither is a symbolic link, and only root can write to the directory |
  | `heartbeat --trusted-certificate-authority-file applies only with --receiver-url; a heartbeat without --receiver-url goes through the heartbeat socket, which needs no certificate` | For a job on the node, remove `--trusted-certificate-authority-file`. For a job on another machine, add `--receiver-url` |
  | `MMWatchdog refused the heartbeat (HTTP status code 401): the token does not match, or this node has no heartbeat check group named NAME` | The token, or the check group's name, does not match the node's configuration. MMWatchdog gives the same answer for both, so a stranger who guesses learns nothing. Over HTTPS the message begins `the receiver refused the heartbeat` |
  | `the account that runs this job cannot read /etc/mmwatchdog/secrets/nightly-backup-heartbeat: give the job its own copy of the token that its account can read, ...` | Give the account its own copy, as in the next item |
  | `expected a single token of 16..4096 bytes` | The token's file is empty or wrong: copy the token again |
  | `tls: failed to verify certificate: x509: certificate signed by unknown authority` | Over HTTPS: add `--trusted-certificate-authority-file` with the certificate from step 1 of [Report from another machine](#report-from-another-machine) |
  | `x509: certificate is valid for 203.0.113.11, not 127.0.0.1` | Over HTTPS: use the address that the certificate names, here `https://203.0.113.11:8444` |
  | `dial tcp 203.0.113.11:8445: connect: connection refused` | Over HTTPS: MMWatchdog does not run, or the receiver listens on another address or port: compare `listen_on_address` |
  | `heartbeat --receiver-url must start with https://` | Write `https://` |

- **The job runs as an account other than root.** That account cannot read
  `/etc/mmwatchdog/secrets/`, but it can use the heartbeat socket. Give the
  account its own copy of the token, owned by the account with mode
  `0600`. On the node, for the account `deploy`:

  ```sh
  sudo install -o deploy -g deploy -m 0600 /etc/mmwatchdog/secrets/nightly-backup-heartbeat /home/deploy/nightly-backup-heartbeat
  ```

  The job then uses `--bearer-token-file /home/deploy/nightly-backup-heartbeat`.
  A token's file that other accounts can read still works, but the command
  warns:

  ```text
  warning: /home/deploy/nightly-backup-heartbeat has permissions mode 0644: every account and the file's group can read the token; run chmod 600 /home/deploy/nightly-backup-heartbeat so that only the account of this job can read it
  ```

  Run the `chmod`. When you change the token in the vault, update the copy
  too.
- **The job runs more often than once a day.** Choose
  `max_time_between_heartbeats` from its timer: a job every 15 minutes
  that takes up to 5 minutes needs about `25m`.
- **You change `max_time_between_heartbeats`.** After the install, the
  check group waits for a first heartbeat again, as in step 6, and the
  deadline counts from the install. A deploy that does not change the
  check group, and a restart of MMWatchdog, keep the last heartbeat and
  the deadline.
- **You want to know whether the job ran.** `sudo mmwatchdog status
  nightly_backup` shows the last heartbeat and the deadline.
  `sudo mmwatchdog check-now nightly_backup` refuses with
  `heartbeat checks receive heartbeats instead of running on demand. Use mmwatchdog status to inspect the latest receipt and deadline`.
- **A maintenance window covers the check group.** A deadline missed during
  the window alerts when the window ends, without waiting.
  [Pause alerts during planned work](pause-alerts-during-planned-work.md)
  explains the windows.
- **You change the token.** Change it in the vault with `mmx secrets edit`
  and run `mmx run hub_and_standalone`. A job that reads the file in
  `/etc/mmwatchdog/secrets/` sends the new token at its next run; update
  every copy on other accounts and machines.
- **You change a node that MMX did not install.** Put the files in
  `/etc/mmwatchdog/secrets/`, owned by root with mode `0600`, change the
  configuration in `/etc/mmwatchdog/`, then run `sudo mmwatchdog validate`
  and `sudo mmwatchdog reload`. A reload that adds the first heartbeat
  check group opens the heartbeat socket. A change of `heartbeat_receiver`
  needs `sudo systemctl restart mmwatchdog` instead:
  `sudo mmwatchdog reload` refuses it.

## Related pages

- [Add a check](add-a-check.md)
- [Send alerts where your team sees them](send-alerts-somewhere.md)
- [See what is happening](see-what-is-happening.md)
- [Install MMWatchdog on one node](install-on-one-node.md): the vault and
  its secret files.
