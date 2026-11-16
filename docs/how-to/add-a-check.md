# Add a check

**When to use this page:** you want MMWatchdog to watch one more thing on a
node, such as a service, a web page, a port, a disk, or a database.

**Who acts:** you, or your coding agent. Each step runs on your controller
machine, the machine where you run `mmx`, except where the step says "on the
node".

**What this page does:** you write the check in the MMX project that
installed MMWatchdog, install it on the node with MMX, and confirm that the
check runs.

**Terms:** a **check** is one thing that MMWatchdog watches. A **check
group** is a named set of checks with shared timings, such as all the checks
of one application. A **module** is the code that does one kind of check,
such as `http` or `systemd_unit_state`. Each check names one module.

## Before you start

- MMWatchdog runs on the node, installed with MMX from the MMX project on
  your controller machine. If not, follow
  [Install MMWatchdog on one node](install-on-one-node.md) first.
- Open a terminal in the MMX project: the directory that holds
  `mmx_project_settings.yml`.
- The node's checks are in the project's
  `files/hub_and_standalone/check_groups/`, one or more `.yml` files. Each
  deploy makes the node's `/etc/mmwatchdog/check_groups/` match this
  directory exactly.

## Steps

1. **Find the module for the job.** On the node, list every module:

   ```sh
   ssh deploy@app01 mmwatchdog describe-module
   ```

   The list gives each module with one line, by topic:

   ```text
   Core modules (built into MMWatchdog), by topic

   Services and protocols
     http          a check   Sends one HTTP or HTTPS request and checks the status code and, when asked, the content of the answer.
     tcp           a check   Opens a TCP connection. The check is healthy when the connection opens. It can also send a text and expect a reply.
   ...
   Systemd units
     systemd_unit_state        a check   Asks systemd for the state of one unit of any kind, such as a service, a timer, or a path unit. ...
   ```

   Then print the page of the module you chose, with its inputs and an
   example:

   ```sh
   ssh deploy@app01 mmwatchdog describe-module http --reference-only
   ```

   **Check:** the module's page lists the inputs you need. If no module
   fits, see [If something is different](#if-something-is-different).

2. **Write the check group.** Create a file in
   `files/hub_and_standalone/check_groups/`, named for the application,
   such as `myapp.yml`. This example watches the service `myapp.service` and
   its health page:

   ```yaml
   # files/hub_and_standalone/check_groups/myapp.yml
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
   ```

   - The four timings are required on every check group.
     [Decide when a check alerts](decide-when-a-check-alerts.md) says how
     to choose them.
   - `alert_severity` is `info`, `warn`, or `sos`. Each alert destination
     takes the severities that it names.
   - Name each check for what is healthy, such as `health_page_answers`,
     because the name appears in every alert.
   - A check group with one check can write the check directly on the
     group, without `checks`. Status then names the check after its
     module, such as `database_port/tcp`:

     ```yaml
     check_groups:
       database_port:
         check_every: 30s
         time_limit_per_run: 5s
         alert_after_failing_for: 1m
         recovered_after_healthy_for: 30s
         tcp: 127.0.0.1:5432
         alert_severity: sos
     ```

   **Check:** the file holds a `check_groups:` mapping, and every check has
   one module and an `alert_severity`.

3. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists your new file as a change to
   `/etc/mmwatchdog`, such as:

   ```text
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked input /etc/mmwatchdog/check_groups/myapp.yml would change
   ```

   The dryrun of this project exits with code 2: something would change.
   Read the lines that say `would_change`. The dryrun does not check the
   check group itself: `validate_config` checks only the copy of the
   configuration that the last run left on the node. Step 4 checks it.
   After a run that failed, that copy is the failed one, so the dryrun
   exits with code 3 until a run succeeds.

4. **Install the check:**

   ```sh
   mmx run hub_and_standalone
   ```

   The run checks the whole configuration on the node before it changes
   anything. A run that changes the configuration restarts MMWatchdog inside
   a 15-minute maintenance window, so the restart sends no check alerts. The
   destinations get one message when the window opens and one when it ends.

   **Check:** the run ends with `Run ...: succeeded`. If it ends with
   `failed`, the node still runs its earlier configuration; see
   [If something is different](#if-something-is-different).

5. **Confirm that the checks run.** On the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status myapp
   ```

   Each check has run once within `check_every` of the restart:

   ```text
   myapp/health_page_answers    ok           HTTP 200 and configured content match
     Config: /etc/mmwatchdog/check_groups/myapp.yml:13:7
     Rule: check every 30s, time limit per run 5s, alert after failing for 1m, recovered after healthy for 30s
     Alert severity: sos
     Observed: 2026-11-15T03:50:34.08793577Z
   myapp/service_is_running     ok           systemd unit myapp.service is active
   ...
   ```

   **Check:** each new check shows `ok`. A check that shows `unknown` with
   `waiting for a new run` has not run yet: wait `check_every` and run the
   command again. Any other `unknown` or `failed` says why in the same
   line; [See what is happening](see-what-is-happening.md) says how to read
   it.

## If something is different

- **The run failed at `validate_config`.** The output names the file, the
  line, and the fault, such as:

  ```text
  mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/myapp.yml:16:11: check_groups.myapp.checks.health_page_answers.http.expected_status_codes: unknown field "expected_status_codes": the inputs of the module http are url, dangerously_skip_server_certificate_check, method, headers, bearer_token_file, ...
  ```

  The path is the copy that the run checks on the node; the line and column
  are those of your file in `files/hub_and_standalone/check_groups/`. Fix
  the file and run step 4 again. Nothing changed on the node: it keeps its
  configuration, its modules, and its running MMWatchdog.
- **The check needs a password or a token.** Put the secret in a file under
  `/etc/mmwatchdog/secrets/`, and name the file in the module's input that
  ends in `_file`, such as `bearer_token_file`. MMWatchdog reads the file
  as root before each run, so the module never needs permission to open
  it. [Install MMWatchdog on one node](install-on-one-node.md#if-something-is-different)
  says how the MMX project writes secret files from its vault.
- **No core module fits.** The modules that ship with MMWatchdog but are not
  built in, such as `mongodb`, `zfs_pool`, and `apache_status`, are named
  the same way as a core module; `mmwatchdog describe-module` lists them
  after the core modules. A `shell` check runs a short Bash script of yours and
  fails when the script exits with a code other than 0, or other than the
  codes in its `success_exit_codes`.
  [Write your own check or destination](write-your-own-check-or-destination.md)
  covers a module of your own.
- **A check of a module that is not core, or a `shell` check, must run as
  another account or in another directory.** Three settings sit beside the
  module's inputs: `run_as_user_account` (default `mmwatchdog`, the
  unprivileged account of the checks), `run_from_directory` (default `/`),
  and `env`, a mapping of more environment variables. The check inherits no
  other environment.
- **A check should only record a number, such as CPU use, and never
  alert.** Write `only_record_readings: true` on the check.
  [Look back at measurements](look-back-at-measurements.md) says how to
  read the numbers.
- **A check should fail above or below a number.** A module that measures,
  such as `node_filesystem`, takes `fail_when_measurement_above` or
  `fail_when_measurement_below` with the measurement's name:

  ```yaml
  root_filesystem_is_below_90_percent:
    node_filesystem: /
    fail_when_measurement_above:
      space_used_percent: 90
    alert_severity: warn
  ```

  The module's page lists its measurements.
- **You change a node that MMX did not install.** Edit the files in
  `/etc/mmwatchdog/check_groups/` on the node, then run
  `sudo mmwatchdog validate` and `sudo mmwatchdog reload`. `reload` uses
  the new configuration only if all of it is valid.

## Related pages

- [Decide when a check alerts](decide-when-a-check-alerts.md)
- [Restart a service automatically](restart-a-service-automatically.md)
- [See what is happening](see-what-is-happening.md)
- The [module pages](../reference/modules/README.md), one for each module.
