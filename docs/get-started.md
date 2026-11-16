# Get started

This page is the path through your first hour with MMWatchdog: from a
server that nothing watches to a server whose application MMWatchdog
watches, restarts when it hangs, and reports to your Slack channel. Each
step is a page of its own; follow them in this order.

**Who acts:** you, or your coding agent. A coding agent shows you the
output of each `mmx dryrun` and waits for your yes before each `mmx run`.

**What you need:**

- A Linux server with systemd that you log in to over SSH, as root or as
  an account that can use `sudo` without a password. MMWatchdog calls it a
  **node**.
- Your **controller machine**, such as your laptop, with
  [MMX](https://github.com/monkeymaxxing/mmx#install) installed. MMX
  installs MMWatchdog on the node; MMWatchdog runs nothing on the
  controller machine.
- A Slack channel with an
  [incoming webhook](https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks),
  or another place for alerts, such as email.

## The path

1. **Install MMWatchdog on the node, and prove that an alert reaches you.**
   Follow [Install MMWatchdog on one node](how-to/install-on-one-node.md),
   all eleven steps.

   **You have then:** MMWatchdog running on the node with the starter
   checks of memory and disk use, Slack as the destination of its alerts,
   and a test alert and its recovery message in your channel. Keep the
   project directory that you unpacked: every later change starts there.

2. **Watch your application.** Follow [Add a check](how-to/add-a-check.md)
   with your application's systemd unit and, if it has one, its health
   page.

   **You have then:** a check group that alerts when your application's
   service stops or its health page stops answering, and `sudo mmwatchdog
   status` on the node lists its checks as `ok`.

3. **Let MMWatchdog restart the application when it hangs.** Add
   `restart_systemd_unit` to the check group, as
   [Restart a service automatically](how-to/restart-a-service-automatically.md) describes:

   ```yaml
       restart_systemd_unit:               # restart myapp when the health page fails
         systemd_unit: myapp.service
         triggered_by_checks: [health_page_answers]
         restart_after_failing_for: 1m
         min_time_between_attempts: 1m
         max_attempts: 3
         reset_attempts_after_healthy_for: 10m
   ```

   The block goes at the end of `myapp.yml`, indented under `myapp:`; the
   whole file is under [The check group of steps 2 and 3, whole](#the-check-group-of-steps-2-and-3-whole).
   Then run `mmx dryrun hub_and_standalone` and `mmx run hub_and_standalone`
   again.

   **Check:** make the application hang, and watch MMWatchdog restart it.
   On the node, stop the service's main process without ending it:

   ```sh
   sudo kill -STOP "$(systemctl show --property MainPID --value myapp.service)"
   ```

   The health page stops answering. After `restart_after_failing_for`, one
   minute, plus up to one `check_every`, `sudo mmwatchdog events myapp`
   shows `systemd_unit_restart_requested`, and soon after `recovered`; the
   whole check group is printed with its events under
   [The check group of steps 2 and 3, whole](#the-check-group-of-steps-2-and-3-whole).
   Slack gets the `SOS` message and the `Resolved SOS` message.

   **You have then:** a hung application that MMWatchdog restarts, up to
   three times, before a person has to step in.

4. **Learn to read what MMWatchdog sees.** Read
   [See what is happening](how-to/see-what-is-happening.md): `status`,
   `events`, and `check-now` on the node.

   **You have then:** the three commands that answer "what is wrong, since
   when, and what did MMWatchdog do?"

## The check group of steps 2 and 3, whole

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
    restart_systemd_unit:               # restart myapp when the health page fails
      systemd_unit: myapp.service
      triggered_by_checks: [health_page_answers]
      restart_after_failing_for: 1m
      min_time_between_attempts: 1m
      max_attempts: 3
      reset_attempts_after_healthy_for: 10m
```

`restart_systemd_unit` sits on the check group, beside `checks`, not inside
a check. When `health_page_answers` has failed for one minute, MMWatchdog
restarts `myapp.service`, and `sudo mmwatchdog events myapp` shows it:

```text
2026-11-15T04:09:11Z systemd_unit_restart_result myapp/health_page_answers systemd unit restart finished. The next check runs will show whether the check has recovered
2026-11-15T04:09:10Z systemd_unit_restart_requested myapp/health_page_answers restart systemd unit myapp.service, attempt 1
2026-11-15T04:09:10Z failed               myapp/health_page_answers expected HTTP 200, received 404
```

## After the first hour

- Several servers, with one place to see them all: set up a fleet hub, as
  [Deploy a fleet](how-to/set-up-a-fleet.md) describes.
- A backup or another scheduled job that should report each success:
  [Report from a scheduled job](how-to/report-from-a-scheduled-job.md).
- Planned work that should send no alerts:
  [Pause alerts during planned work](how-to/pause-alerts-during-planned-work.md).
- Every page: the [documentation index](README.md).
