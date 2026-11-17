# Write your own check or destination

**When to use this page:** no module of MMWatchdog checks what you need, such
as a number that only your application knows, or no module delivers alerts
to a service that your team uses, such as an alarm system of your own.
Use this page also when you want alerts in your own words: MMWatchdog has
no message templates, and a destination module gets the whole alert and
writes what it sends.

**Who acts:** you, or your coding agent. Each step runs on your controller
machine, the machine where you run `mmx`, except where the step runs a
command on the node over `ssh`. A coding agent asks the person to run
`mmx secrets set mmwatchdog_secret_files alarm-system-token` in step 12 and
to type the token themselves, so the token
never passes through the chat. The agent runs `mmx dryrun` and `mmx run`
with `MMX_VAULT_PASSWORD_FILE` set, as
[Install MMWatchdog on one node](install-on-one-node.md) shows.

**What this page does:** you write `queued_jobs`, a check module that reads
how many jobs wait in your application's queue. You try the module on the
node, install it with MMX, and watch a check that uses it fail and recover
in Slack. Then you write `alarm_system`, a destination module that posts
each alert to your team's alarm system with a token, install it, and prove
that a test alert arrives.

**Terms:**

- A **module** is the code that does the work of one check or of one alert
  destination, such as `http` or `slack`. A check or a destination names
  exactly one module, and writes the module's **inputs** below the name.
- A **module of the project's own** is a directory in the project's
  `mmwatchdog_external_modules/`, named after the module. Each deploy
  installs the directory on the node.
- A **Starlark script** is the usual form of such a module: `module.star`,
  written in [Starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md),
  a small language that reads like Python.
- A **primitive** is a function that MMWatchdog gives the script to read the
  node, such as `read_file` or `http_request`. A script reaches the node
  only through primitives.
- The **module description** is the file `module_description.yml`: what the
  module does, its inputs, its measurements, and examples.

| What you need | Where |
| --- | --- |
| A check that measures or tests something of your own | [Steps](#steps), steps 1 to 9 |
| Alerts delivered to a service that no module serves | [Steps](#steps), steps 10 to 15 |
| A module in Bash, Go, or another language | [Writing modules](../reference/writing-modules.md), the part about an executable |
| State kept between runs, one incident for each failed thing, or a destination checked without a message | [Writing modules](../reference/writing-modules.md) |
| Every primitive, field, and limit | [Writing modules](../reference/writing-modules.md) |

## Before you start

- MMWatchdog runs on the node, installed with MMX from your project, and
  alerts reach your Slack channel. If not, follow
  [Install MMWatchdog on one node](install-on-one-node.md) first. The
  examples use the node `app01`.
- Open a terminal in the project: the directory that holds
  `mmx_project_settings.yml`.
- No module fits the job. `ssh deploy@app01 mmwatchdog describe-module`
  lists every module with one line each. A test that one shell command
  answers, such as `test -f /var/lib/myapp/ready`, needs no module: write
  a `shell` check, as [Add a check](add-a-check.md) says. A module is worth
  writing when the check takes inputs, measures a number, or serves several
  checks.
- The example application `myapp` writes the number of its waiting jobs
  into the file `/var/lib/myapp/pending`. To follow the example without
  such an application, create the file on the node:

  ```sh
  ssh deploy@app01 'sudo mkdir -p /var/lib/myapp && echo 12 | sudo tee /var/lib/myapp/pending'
  ```

## Steps

1. **Create the check module's directory:**

   ```sh
   mkdir -p mmwatchdog_external_modules/queued_jobs
   ```

   Reason: the directory's name is the module's name, which a check
   writes. A name uses lower-case letters, digits, and underscores, starts
   with a letter, and is not the name of a module that MMWatchdog already
   has, such as `http` or `slack`.

   **Check:** `ls mmwatchdog_external_modules` lists `queued_jobs` and the
   `README.md` that came with the release.

2. **Write the script.** Create
   `mmwatchdog_external_modules/queued_jobs/module.star`:

   ```python
   # mmwatchdog_external_modules/queued_jobs/module.star
   # Reads the number of waiting jobs from a file that the application writes.
   # The module only measures: the check states the limit.

   def run_check(inputs, file_contents):
       queue_file = read_file(inputs["queue_file_path"])
       if queue_file.error:
           return {"status": "unknown", "message": "MMWatchdog cannot read the queue file: " + queue_file.error}
       waiting = queue_file.content.strip()
       if not waiting.isdigit():
           return {"status": "unknown", "message": "the queue file does not hold a whole number"}
       return {
           "status": "ok",
           "message": waiting + " queued jobs",
           "measurements": {"queued_jobs": int(waiting)},
       }
   ```

   What the script does, at each run of a check that uses it:

   - MMWatchdog calls `run_check` with the check's inputs, such as
     `{"queue_file_path": "/var/lib/myapp/pending"}`. `file_contents` is
     empty here; [Writing modules](../reference/writing-modules.md) says
     when it is not.
   - `read_file` is a primitive. Its result has the field `content`, and
     the field `error`, which is empty when the call worked. A failed call
     does not stop the script, so the script decides what the failure
     means.
   - The script returns a status, a message, and the measurement
     `queued_jobs`. `ok` means that the module could measure, and
     `unknown` means that it could not. The script does not compare the
     number with a limit: the check states the limit in step 5, and
     MMWatchdog applies it.

   **Check:** none yet, because MMWatchdog reads a module only with its
   description. Step 4 runs both.

3. **Describe the module.** Create
   `mmwatchdog_external_modules/queued_jobs/module_description.yml`:

   ```yaml
   # mmwatchdog_external_modules/queued_jobs/module_description.yml
   summary: Counts the jobs that wait in a queue file.
   used_as: check
   inputs:
     queue_file_path:
       type: string
       required: true
       description: The absolute path of the file that holds the number of waiting jobs.
   measurements:
     queued_jobs:
       unit: count
       description: The number of jobs that wait.
   examples:
     - description: Fail when more than 500 jobs wait.
       config: |
         queued_jobs:
           queue_file_path: /var/lib/myapp/pending
         fail_when_measurement_above:
           queued_jobs: 500
         alert_severity: warn
   ```

   Reason: MMWatchdog checks every check that names the module against this
   description before anything runs. `used_as: check` says that checks use
   the module. Each input states its `type`, whether it is `required`, and a
   `description`, which MMWatchdog prints when a check leaves out a
   required input. Each measurement states its `unit`.

   **Check:** step 4 reads the description.

4. **Try the module on the node, before a deploy.** The `mmwatchdog`
   program in the project is the Linux program for the node, so try the
   module there. Copy the modules directory to the node, print the
   module's description, and run the script once:

   ```sh
   scp -r mmwatchdog_external_modules deploy@app01:/tmp/modules-to-try
   ssh deploy@app01 mmwatchdog describe-module queued_jobs --external-module-dir /tmp/modules-to-try
   echo '{"protocol":1,"inputs":{"queue_file_path":"/var/lib/myapp/pending"}}' |
     ssh deploy@app01 mmwatchdog run-starlark-module /tmp/modules-to-try/queued_jobs
   ```

   ```text
   queued_jobs: a module of the project's own (Starlark), used as a check
   Counts the jobs that wait in a queue file.
   Files: /tmp/modules-to-try/queued_jobs/

   Inputs
     queue_file_path   string   required   The absolute path of the file that holds the number of waiting jobs.

   Measurements
     queued_jobs   count   The number of jobs that wait.

   Example: Fail when more than 500 jobs wait.
     queued_jobs:
       queue_file_path: /var/lib/myapp/pending
     fail_when_measurement_above:
       queued_jobs: 500
     alert_severity: warn
   ```

   ```text
   {"measurements":{"queued_jobs":12},"message":"12 queued jobs","protocol":1,"status":"ok"}
   ```

   `run-starlark-module` reads one request on its standard input, with the
   inputs that a check would give, and prints the script's result. It runs
   the script as `deploy`, the account of your `ssh` command, where a
   scheduled run uses the account `mmwatchdog`.

   **Check:** `describe-module` prints the description, and the result says
   `"status":"ok"` with the number in the file. An error names the file and
   the line to fix; see [If something is different](#if-something-is-different).
   When the module works, remove the copy:
   `ssh deploy@app01 rm -r /tmp/modules-to-try`.

5. **Write the check that uses the module.** Create
   `files/hub_and_standalone/check_groups/myapp_jobs.yml`:

   ```yaml
   # files/hub_and_standalone/check_groups/myapp_jobs.yml
   check_groups:
     myapp_jobs:
       check_every: 30s
       time_limit_per_run: 10s
       alert_after_failing_for: 1m
       recovered_after_healthy_for: 30s
       checks:
         job_queue_is_short:
           queued_jobs:                               # the module, in mmwatchdog_external_modules/queued_jobs/
             queue_file_path: /var/lib/myapp/pending  # an input of the module
           fail_when_measurement_above:
             queued_jobs: 500                         # MMWatchdog applies this limit to the measurement
           alert_severity: warn
   ```

   A check names a module of the project's own exactly as it names a core
   module: the module's name, with its inputs below. A check with such a
   module always states an `alert_severity`, because the module can answer
   `failed` by itself. [Add a check](add-a-check.md) explains the four
   timings.

   **Check:** step 6 checks the check group against the module's
   description on the node, before it changes anything.

6. **Install the module and the check:**

   ```sh
   mmx run hub_and_standalone
   ```

   ```text
     install / node / install_monitoring/changes/install_modules: succeeded, changed
     install / node / install_monitoring/activate/start_maintenance: succeeded, changed — ran because tracked inputs /etc/mmwatchdog/check_groups/myapp_jobs.yml and 2 more changed
   ...
   Run c739e4ba: succeeded
   ```

   The run copies `mmwatchdog_external_modules/` to the node and checks the
   whole configuration against it. Then it installs the modules in
   `/usr/local/lib/mmwatchdog/external_modules/` and the check group in
   `/etc/mmwatchdog/check_groups/`, and restarts MMWatchdog inside a
   maintenance window. The node then has exactly the modules of the
   project.

   **Check:** the run ends with `succeeded`, and `install_modules` says
   `changed`. If the run fails at `validate_config`, the node keeps its
   earlier configuration and modules; see
   [If something is different](#if-something-is-different).

7. **Confirm that the check runs:**

   ```sh
   ssh deploy@app01 sudo mmwatchdog status myapp_jobs
   ```

   ```text
   myapp_jobs/job_queue_is_short ok           queued_jobs is 12 (fails above 500); the module reported: 12 queued jobs
     module queued_jobs: the project's own, Starlark
     Config: /etc/mmwatchdog/check_groups/myapp_jobs.yml:9:7
     Rule: check every 30s, time limit per run 10s, alert after failing for 1m, recovered after healthy for 30s
     Alert severity: warn
     Observed: 2026-11-15T10:31:03.405753404Z
     queued_jobs: 12 count
   Alert deliveries: 0 waiting, 0 dropped
   ```

   The first line gives MMWatchdog's comparison with the limit, then the
   module's own message. `module queued_jobs` says where the module comes
   from, because the check group does not say so.

   **Check:** the check shows `ok` with the number in the file. A check that
   shows `unknown` with `waiting for a new run` has not run yet: wait 30
   seconds and run the command again.

8. **Make the check fail, and see the alert.** Write a number above the
   limit into the file, and ask for a fresh run:

   ```sh
   ssh deploy@app01 'echo 780 | sudo tee /var/lib/myapp/pending'
   ssh deploy@app01 sudo mmwatchdog check-now myapp_jobs
   ```

   ```text
   myapp_jobs/job_queue_is_short failed   queued_jobs is 780 (fails above 500); the module reported: 780 queued jobs
     Observed: 2026-11-15T10:31:42.246729786Z
     queued_jobs: 780 count
   mmwatchdog: one or more checks need attention
   ```

   `check-now` shows a fresh run and sends no alert. The scheduled runs
   fail too, and after 1 minute of failing (`alert_after_failing_for`),
   Slack shows:

   ```text
   Warning: app01: myapp_jobs/job_queue_is_short failed
   queued_jobs is 780 (fails above 500); the module reported: 780 queued jobs
   failing since 10:32 UTC
   ```

   **Check:** the message arrived, about a minute after the first failed
   run. If not, run `ssh deploy@app01 sudo mmwatchdog status myapp_jobs`:
   its last line names a destination that fails, and
   [See what is happening](see-what-is-happening.md) says how to read it.

9. **Recover:**

   ```sh
   ssh deploy@app01 'echo 12 | sudo tee /var/lib/myapp/pending'
   ```

   After 30 seconds of healthy runs (`recovered_after_healthy_for`), Slack
   shows:

   ```text
   Resolved warning: app01: myapp_jobs/job_queue_is_short is ok again
   queued_jobs is 12 (fails above 500); the module reported: 12 queued jobs
   ```

   **Check:** the message arrived. The check module is done. Steps 10 to
   15 write a destination module.

10. **Write the destination module's script.** The example alarm system
    takes each alert as JSON at `https://alarm.example.com/alerts`, and
    wants a token in the `Authorization` header. Create the directory
    `mmwatchdog_external_modules/alarm_system/` and, in it, `module.star`:

    ```sh
    mkdir -p mmwatchdog_external_modules/alarm_system
    ```

    ```python
    # mmwatchdog_external_modules/alarm_system/module.star
    # Posts each alert as JSON to the team's alarm system, with a token.

    def deliver_alert(alert, inputs, file_contents):
        alert_for_the_alarm_system = {
            "title": alert["title"],
            "alert_severity": alert["alert_severity"],
            "type": alert["type"],
        }
        # A notice has no incident, and a notice sent without a body has no body.
        for name in ["incident_id", "body"]:
            if name in alert:
                alert_for_the_alarm_system[name] = alert[name]

        response = http_request(
            method = "POST",
            url = inputs["alarm_system_url"],
            headers = {
                "Content-Type": "application/json",
                "Authorization": "Bearer " + file_contents["api_token_file"],
            },
            body = json.encode(alert_for_the_alarm_system),
        )
        if response.error:
            return {"status": "failed", "message": "MMWatchdog could not reach the alarm system: " + response.error}
        if response.http_status_code // 100 != 2:
            return {"status": "failed", "message": "the alarm system answered HTTP %d" % response.http_status_code}
        return {"status": "delivered", "message": "the alarm system accepted the alert (HTTP %d)" % response.http_status_code}
    ```

    What the script does, once for each alert that the destination gets:

    - MMWatchdog calls `deliver_alert` with the alert, the destination's
      inputs, and `file_contents`. [Alert format](../reference/alert-format.md)
      lists the alert's fields.
    - `file_contents["api_token_file"]` holds the token. An input whose name
      ends in `_file` names a file, and MMWatchdog reads the file as root
      and gives the module its text. So the token file can stay readable by
      root alone, and the module never opens it.
    - The script returns `delivered` or `failed`, with a message. MMWatchdog
      tries a failed delivery again later, so the module does not retry.
    - `response.error` never holds the address of the request, so the
      message is safe to show. Never put the token into a message.

    **Check:** none yet. Step 11 checks the script with its description.

11. **Describe the destination module.** Create
    `mmwatchdog_external_modules/alarm_system/module_description.yml`:

    ```yaml
    # mmwatchdog_external_modules/alarm_system/module_description.yml
    summary: Posts each alert as JSON to the team's own alarm system, with a token.
    used_as: alert_destination
    inputs:
      alarm_system_url:
        type: string
        required: true
        description: The address of the alarm system that takes the alerts, such as https://alarm.example.com/alerts.
      api_token_file:
        type: string
        required: true
        description: The absolute path of the file that holds the alarm system's token. MMWatchdog reads the file for every alert.
    examples:
      - description: Send every warning and SOS alert to the alarm system.
        config: |
          alarm_system:
            alarm_system_url: https://alarm.example.com/alerts
            api_token_file: /etc/mmwatchdog/secrets/alarm-system-token
          alert_severities: [warn, sos]
          remind_every: never
    ```

    `used_as: alert_destination` says that destinations use the module. A
    destination module has no measurements.

    **Check:** copy the modules to the node and print the description, as
    in step 4:

    ```sh
    scp -r mmwatchdog_external_modules deploy@app01:/tmp/modules-to-try
    ssh deploy@app01 mmwatchdog describe-module alarm_system --external-module-dir /tmp/modules-to-try
    ssh deploy@app01 rm -r /tmp/modules-to-try
    ```

    The first line of the description says
    `alarm_system: a module of the project's own (Starlark), used as an alert destination`.

12. **Put the token in MMX's vault.** The token is a secret, so MMX keeps
    it in the encrypted file `secrets_vault.age` and writes it to the node
    during the install. Store the alarm system's token, beside the Slack
    address, under the file name `alarm-system-token`:

    ```sh
    mmx secrets set mmwatchdog_secret_files alarm-system-token
    ```

    Type the token at the hidden prompt. The vault then holds:

    ```yaml
    mmwatchdog_secret_files:
      slack-webhook-team: "https://hooks.slack.com/services/YOUR/WEBHOOK/ADDRESS"
      alarm-system-token: "the token of your alarm system"
    ```

    Then list the new file in `inventories/hub_and_standalone.yml`, under
    `vars`:

    ```yaml
    vars:
      mmwatchdog_config_source_dir: files/hub_and_standalone
      mmwatchdog_secret_files: [slack-webhook-team, alarm-system-token]
    ```

    Each install writes each listed file to `/etc/mmwatchdog/secrets/` on
    the node, readable only by root.

    **Check:** `mmx secrets list-keys` prints `mmwatchdog_secret_files`,
    and `mmx validate` prints `Valid: mmwatchdog`.

13. **Add the destination.** In
    `files/hub_and_standalone/alert_destinations.yml`, add
    `team_alarm_system` below the Slack destination:

    ```yaml
    # files/hub_and_standalone/alert_destinations.yml
    destinations:
      team_slack_channel:
        slack:
          webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
        alert_severities: [info, warn, sos]
        remind_every: 4h
      team_alarm_system:
        alarm_system:                                                 # the module, in mmwatchdog_external_modules/alarm_system/
          alarm_system_url: https://alarm.example.com/alerts
          api_token_file: /etc/mmwatchdog/secrets/alarm-system-token  # MMWatchdog reads this file and gives the module its content
        alert_severities: [warn, sos]
        remind_every: never
    ```

    `team_alarm_system` gets the warning and SOS alerts of every check,
    and no reminders. [Send alerts where your team sees them](send-alerts-somewhere.md)
    explains `alert_severities` and `remind_every`.

    **Check:** `mmx validate` prints `Valid: mmwatchdog`. Step 14 checks
    the destination against the module's description.

14. **Install the destination:**

    ```sh
    mmx run hub_and_standalone
    ```

    ```text
      write_secret_files / node / secret_files[1]: created "/etc/mmwatchdog/secrets/alarm-system-token"
    ...
      install / node / install_monitoring/changes/install_modules: succeeded, changed
    ...
    Run 26eced38: succeeded
    ```

    **Check:** the run ends with `succeeded`, and it created
    `/etc/mmwatchdog/secrets/alarm-system-token`.

15. **Prove that an alert reaches the alarm system.** Send a test notice
    to the new destination alone:

    ```sh
    ssh deploy@app01 sudo mmwatchdog send-notice --alert-severity info \
      --title "Test of the alarm system. Nothing is wrong." --destination team_alarm_system
    ```

    ```text
    Notice sent to the destination team_alarm_system:
      team_alarm_system   delivered
    ```

    The notice goes through the real delivery path: MMWatchdog reads the
    token file, runs the module, and prints what the module answered.
    `--destination` sends the notice to the named destination, whatever its
    `alert_severities` say. The alarm system receives this JSON:

    ```text
    {"alert_severity":"info","title":"app01: Test of the alarm system. Nothing is wrong.","type":"notice"}
    ```

    **Check:** the command says `delivered`, and the alarm system shows the
    test notice. From now on, the warning and SOS alerts of every check,
    such as the one in step 8, reach the alarm system too.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| The run fails at `validate_config` with `mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/myapp_jobs.yml:11:11: check_groups.myapp_jobs.checks.job_queue_is_short.queued_jobs.queue_file: unknown field "queue_file": the inputs of the module queued_jobs are queue_file_path` | The check states an input that the description does not list. The line and column are those of your file in `files/hub_and_standalone/check_groups/`. Fix the input's name, or add the input to the description, and run `mmx run hub_and_standalone` again. Nothing changed on the node. |
| `mmwatchdog: /tmp/modules-to-try/queued_jobs/module_description.yml:7: inputs.queue_file_path: unknown field "requird"; the fields are type, required, allowed_values, description` | The description has a mistake at the line after the file name. Fix the line. |
| `mmwatchdog: /tmp/modules-to-try/queued_jobs/module.star:6: key "queue_file_path" not in dict` from `run-starlark-module`, with exit code 4 | The script stopped at line 6: here, the request lacks the input that line 6 reads. In a scheduled run, the same error makes the check `unknown`, with the message `the script of the module queued_jobs ended with an error:` and the error. |
| `the module slack in /tmp/modules-to-try has the name of the official extra module slack, which every deploy installs. A name means one module on every node: give your module another name` | Another module has the name. Rename the directory and every check or destination that names it. |
| `/tmp/modules-to-try/queue-length is no module: the name of a module uses lower-case letters, digits, and underscores, starts with a letter, and has at most 64 characters, such as queued_jobs` | A directory in `mmwatchdog_external_modules/` has a name that no module can have, such as one with a hyphen. Rename the directory. |
| `the module queued_jobs has no code: ... must hold the script module.star or the executable module_executable` | The module's directory has no script. Check the file name. |
| The check shows `unknown` with `MMWatchdog cannot read the queue file: open /var/lib/myapp/pending: permission denied` | The module runs as the account `mmwatchdog`, which cannot read the file. Make the file readable by that account, or run the module as the application's account: add `run_as_user_account: myapp` beside `queue_file_path`. |
| The check shows `unknown` with `the module queued_jobs reported the measurement jobs, which its description does not list` | The script reports a measurement under another name than the description. Use the same name in both files. |
| `team_alarm_system   failed: the module alarm_system reported: the alarm system answered HTTP 401` | The module reached the alarm system, and the alarm system refused the token. Change the token with `mmx secrets set mmwatchdog_secret_files alarm-system-token --replace`, run `mmx run hub_and_standalone`, and send the notice again. MMWatchdog keeps a failed alert and tries again. |
| The node is in a fleet | Destinations live on the hub, but a joined node that cannot reach the hub delivers its alerts itself, with its own copy of the module. Put the module in the project once: the hub's and the joined nodes' playbooks install the same modules on every node. |
| You change a node that MMX did not install | Copy the module's directory into `/usr/local/lib/mmwatchdog/external_modules/` on the node, as root, then run `sudo mmwatchdog validate` and `sudo mmwatchdog reload`. `reload` reads the modules again, and uses the new configuration only if all of it is valid. |

## Related pages

- [Writing modules](../reference/writing-modules.md): every primitive,
  the description's fields, findings, state kept between runs, a
  destination checked without a message, and a module as an executable
  with its JSON protocol.
- [Add a check](add-a-check.md)
- [Send alerts where your team sees them](send-alerts-somewhere.md)
- [Alert format](../reference/alert-format.md): the fields of an alert that
  a destination module gets.
- The [module pages](../reference/modules/README.md), one for each module
  that comes with MMWatchdog.
