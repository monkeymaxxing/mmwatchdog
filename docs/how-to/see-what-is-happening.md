# See what is happening

**When to use this page:** an alert arrived, a check looks wrong, or you
want to know what MMWatchdog sees on a node right now and what it did
earlier.

**Who acts:** you, or your coding agent, on the node, logged in with an
account that can use `sudo`. The commands only read, except
`check-now --count-as-scheduled-run`, which step 4 explains.

**What this page does:** you read the state of each check, the history of
what happened, and a fresh run of the checks.

## Choose the command

| You want to know | Command |
|---|---|
| The state of every check now, and why | `sudo mmwatchdog status` |
| The state of the checks of one check group | `sudo mmwatchdog status myapp` |
| What happened and when: failures, restarts, recoveries | `sudo mmwatchdog events myapp` |
| What a check finds right now, without waiting for its next run | `sudo mmwatchdog check-now myapp` |
| The same, as data for a script or a coding agent | add `--json` to any of the commands |

## Steps

1. **Read the state of every check:**

   ```sh
   sudo mmwatchdog status
   ```

   Each line is one check: its name as `CHECK_GROUP/CHECK`, its state, and
   the result of its latest run:

   ```text
   machine/cpu_use_is_recorded  ok           cpu_used_percent is 0.17 over 30.323s
   machine/memory_is_below_90_percent ok           memory_used_percent is 17.73 (fails above 90)
   machine/root_filesystem_is_below_85_percent ok           space_used_percent is 9.13 (fails above 85)
   machine/root_filesystem_is_below_95_percent ok           space_used_percent is 9.13 (fails above 95)
   myapp/health_page_answers    failed       expected HTTP 200, received 404
   myapp/service_is_running     ok           systemd unit myapp.service is active
   Alert deliveries: 0 waiting, 0 dropped
   This node sends alerts to nobody.
   ```

   The last lines say whether alerts reach their destinations. Here the
   node has no destination yet, so it sends alerts to nobody.

   **Check:** the command exits with 0 when every check is healthy, 1 when
   a check has failed, and 3 when no check has failed but MMWatchdog cannot
   tell, such as a check that is `unknown` or a destination that does not
   accept alerts. A failing destination shows in the output in both cases. A check
   in `confirming_failure` counts as healthy here, because it has not
   failed for `alert_after_failing_for` yet. Read the states in step 2.

2. **Read the checks of one check group, with the details:**

   ```sh
   sudo mmwatchdog status myapp
   ```

   ```text
   myapp/health_page_answers    failed       expected HTTP 200, received 404
     Config: /etc/mmwatchdog/check_groups/myapp.yml:13:7
     Rule: check every 30s, time limit per run 5s, alert after failing for 1m, recovered after healthy for 30s
     Alert severity: sos
     Alert severity of the open failure incident: sos
     Observed: 2026-11-15T03:52:04.073604074Z
     Failure evidence:
     Confirmed incident: 5ebcbd3addb90ab46b602cbceac3c139
     Config at failure: /etc/mmwatchdog/check_groups/myapp.yml:13:7
     Waiting times: alert after failing for 1m, recovered after healthy for 30s
     Result: failed — "expected HTTP 200, received 404"
     Observed: 2026-11-15T03:52:04.073604074Z
   myapp/service_is_running     ok           systemd unit myapp.service is active
   ...
   ```

   `Config` is the file and the line of the check. `Failure evidence` is
   the run that opened the failure: it stays the same while the check keeps
   failing, so you can see what the check found first.

   What to do in each state:

   | State | What it means | What to do |
   |---|---|---|
   | `ok` | The check is healthy. | Nothing. |
   | `confirming_failure` | The check fails, but not yet for `alert_after_failing_for`. No alert has gone out. A check that fails on and off also shows `confirming_failure` while its latest run is healthy. | Wait, or fix the cause now. |
   | `failed` | The check has failed for `alert_after_failing_for`. The failure is open. Its alert went to each destination that takes its severity, unless a maintenance window or a failing cause holds it back. | Fix the cause. Step 3 shows the history. |
   | `confirming_recovery` | The failure is still open, but the latest run was healthy. The failure closes after `recovered_after_healthy_for` of healthy runs. | Wait. |
   | `unknown` | MMWatchdog could not tell whether the check is healthy, such as a `shell` check whose script ran longer than `time_limit_per_run`. (An `http` check that gets no answer in time has failed: the line says `no answer within 5s (time_limit_per_run)`.) The line says why. | Fix what the line names. `waiting for a new run` after a restart or a configuration change only means the check has not run since: wait `check_every`. |

   [How MMWatchdog works](../how-it-works.md) explains each state, and
   the two states of heartbeat and fleet checks, which wait for their first
   message.

   **Check:** you know the state of each check and the reason in its line.

3. **Read what happened and when:**

   ```sh
   sudo mmwatchdog events myapp
   ```

   The newest event comes first. A failure and its recovery each keep the
   evidence of the failure:

   ```text
   2026-11-15T03:53:08Z recovered            myapp/health_page_answers HTTP 200 and configured content match
     Incident: 5ebcbd3addb90ab46b602cbceac3c139
     ...
   2026-11-15T03:52:04Z failed               myapp/health_page_answers expected HTTP 200, received 404
     Incident: 5ebcbd3addb90ab46b602cbceac3c139
     ...
   ```

   Without a check group, `events` lists the events of the whole node, such
   as the maintenance window of each deploy. `--last 2h` shows only the
   events of the past two hours:

   ```sh
   sudo mmwatchdog events --last 2h
   ```

   ```text
   2026-11-15T04:00:43Z maintenance_off      every_check_group maintenance turned off by root: mmwatchdog deploy
   2026-11-15T04:00:42Z maintenance_on       every_check_group maintenance until 2026-11-15 04:15 UTC, turned on by root: mmwatchdog deploy
   2026-11-15T03:59:10Z recovered            myapp/health_page_answers HTTP 200 and configured content match
   2026-11-15T03:58:10Z failed               myapp/health_page_answers expected HTTP 200, received 404
   ```

   `--since TIME` and `--until TIME` select a span of time instead.

   A problem that began while alerts were paused by a maintenance window
   shows two lines for one incident. The first ends with `(no alert sent:
   alerts were paused by a maintenance window)`. The second comes at the
   first run after the window and sends the alert.

   **Check:** the events show when the failure began and, if MMWatchdog
   acted, what it did, such as `systemd_unit_restart_requested`.

4. **Run the checks now:**

   ```sh
   sudo mmwatchdog check-now myapp
   ```

   ```text
   myapp/health_page_answers failed   expected HTTP 200, received 404
     Observed: 2026-11-15T03:51:31.502932446Z
   myapp/service_is_running ok       systemd unit myapp.service is active
     Observed: 2026-11-15T03:51:31.509719375Z
   ```

   `check-now` runs each check of the group once and shows what it found,
   with the output of a script or a module where it has one. It sends no
   alert, opens or closes no failure, restarts nothing, and runs no
   recovery command. So it is safe to run at any time.

   **After you fixed the cause,** count the runs as scheduled runs, so an
   open failure can close without waiting for the next scheduled run:

   ```sh
   sudo mmwatchdog check-now myapp --count-as-scheduled-run
   ```

   ```text
   myapp/health_page_answers ok       HTTP 200 and configured content match
     Observed: 2026-11-15T03:52:38.400184316Z
   myapp/service_is_running ok       systemd unit myapp.service is active
     Observed: 2026-11-15T03:52:38.405070239Z
   The runs counted as scheduled runs for check group myapp; run mmwatchdog status myapp to see the resulting check status.
   ```

   These runs count like scheduled runs: a failure can close, and a failing
   check can restart its service. The failure still closes only after
   `recovered_after_healthy_for` of healthy runs, so `status` shows
   `confirming_recovery` until then:

   ```text
   myapp/health_page_answers    confirming_recovery the latest run is healthy; the alert closes once the check has been healthy for 30s (recovered_after_healthy_for): HTTP 200 and configured content match
   ```

   **Check:** `check-now` exits with 0 when every check of the group is
   healthy, 1 when one failed, and 3 when one is unknown.

## If something is different

- **A coding agent or a script reads the results.** Add `--json` to
  `status`, `events`, or `check-now`. `status --json` gives, for each check,
  `state`, `last_observation`, `failing_since`, `failure_incident`, and
  `needs_fresh_observation`, among others. The
  [command reference](../reference/commands.md) lists every field and exit code.
- **The node is not reachable, or you want every node at once.** On a fleet
  hub, `sudo mmwatchdog fleet` shows every node and its failing checks;
  see [Watch a fleet](watch-a-fleet.md).
- **A check is `unknown` and the line does not explain enough.**
  `check-now` shows the standard output and standard error of a `shell`
  check or of a module, and its exit code.
  [When something fails](../when-something-fails.md) lists the common causes.
- **You want to try a configuration before the node uses it.** On the
  node, `sudo mmwatchdog check-now myapp --without-daemon --config-dir DIR`
  runs the checks of a configuration in `DIR` without the running
  MMWatchdog. Each run starts with nothing kept from earlier runs.
- **`status` says `This node sends alerts to nobody`.** The node has no
  alert destination. [Send alerts somewhere](send-alerts-somewhere.md)
  adds one.
- **A check stays quiet although it fails.** Its line says that another
  check, which it names, fails, and MMWatchdog keeps the second check quiet
  while the first fails.
  [Stop alerts that follow another failure](stop-alerts-that-follow-another-failure.md)
  explains the setting.

## Related pages

- [Add a check](add-a-check.md)
- [Pause alerts during planned work](pause-alerts-during-planned-work.md)
