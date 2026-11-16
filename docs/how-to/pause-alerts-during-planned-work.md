# Pause alerts during planned work

**When to use this page:** you plan work that makes checks fail on a node,
such as moving an application's database, restarting several services, or
rebooting the node, and you want no alerts and no automatic restarts while
you work.

**Who acts:** you, or your coding agent, on the node, logged in with an
account that can use `sudo`. A deploy with `mmx run` needs no action from
you: it pauses alerts by itself, as
[Deploys through MMX](#deploys-through-mmx) says.

**What this page does:** you open a **maintenance window** for one or more
check groups, for a time that you state. During the window, MMWatchdog
keeps running the checks of those check groups and shows their results, but
sends no alert for them, restarts no systemd unit, and runs no recovery
command. The window ends when you turn it off, or by itself when its time
is over, so a window that you forget cannot keep the node quiet for good.

**Terms:** a **check group** is a named set of checks, such as `myapp`, the
checks of one application. `sudo mmwatchdog status` names each check as
`CHECK_GROUP/CHECK`. The **whole-node window** covers every check group of
the node.

## Choose the window

| Your planned work | Window | Section |
|---|---|---|
| Work on one application, such as a database move | The application's check group | [Pause one check group](#pause-one-check-group) |
| Work on several applications at once, such as a deploy of several workers | Several check groups, with one command | [Pause several check groups](#pause-several-check-groups) |
| Work on the whole node, such as a kernel update and a reboot | Every check group | [Pause every check group](#pause-every-check-group) |
| A change to MMWatchdog's configuration with `mmx run` | None: the run opens its own | [Deploys through MMX](#deploys-through-mmx) |
| Routine work that your team does not need to hear about | Any of the above, without the two messages | [If something is different](#if-something-is-different) |
| MMWatchdog itself must not run for a while | None: stop the service | [Stop MMWatchdog for a while](#stop-mmwatchdog-for-a-while) |

## Before you start

- **MMWatchdog runs on the node.** `sudo mmwatchdog status` lists the
  checks. If not, follow
  [Install MMWatchdog on one node](install-on-one-node.md) first.
- **You know about how long the work takes.** Every window needs `--for`,
  the time after which it ends by itself. Choose a little more than you
  expect: you can end the window early, and you can make it longer, as
  [If something is different](#if-something-is-different) says.
- **Your team sees each window.** Each alert destination that takes `info`
  alerts gets one message when a window opens and one when it ends, with
  the reason and the account that opened it. A destination without `info`
  in its `alert_severities` gets neither message.

## Pause one check group

1. **Find the name of the check group.** On the node:

   ```sh
   sudo mmwatchdog status
   ```

   ```text
   machine/cpu_use_is_recorded  ok           cpu_used_percent is 4.08 over 0.737s
   ...
   myapp/health_page_answers    ok           HTTP 200 and configured content match
   myapp/service_is_running     ok           systemd unit myapp.service is active
   Alert deliveries: 0 waiting, 0 dropped
   ```

   **Check:** the name before the `/`, here `myapp`, is the check group of
   the application that you work on.

2. **Open the window,** with the longest time that the work may take and
   the reason:

   ```sh
   sudo mmwatchdog maintenance on myapp --for 1h --reason "moving the myapp database"
   ```

   ```text
   myapp is in maintenance until 2026-11-15 05:40 UTC (1h).
     MMWatchdog keeps running the checks of myapp and shows the results in mmwatchdog status.
     During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for myapp.
     To end the maintenance early: sudo mmwatchdog maintenance off myapp
   ```

   Reason for `--reason`: your team sees it, and knows that the work is
   planned. The reason is one line of at most 200 bytes.

   **Check:** your Slack channel, or each other destination that takes
   `info`, shows the message:

   ```text
   Info: app01: maintenance on for myapp
   maintenance until 2026-11-15 05:40 UTC, turned on by deploy: moving the myapp database
   ```

   `deploy` is the account that ran `sudo`.

3. **Do the work.** The checks keep running. While you work:

   ```sh
   sudo mmwatchdog status myapp
   ```

   ```text
   myapp/health_page_answers    failed       expected HTTP 200, received 404
   ...
   myapp/service_is_running     ok           systemd unit myapp.service is active
   ...
   Maintenance on: myapp until 2026-11-15 05:40 UTC, turned on by deploy: moving the myapp database
   ```

   **Check:** the `Maintenance on` line names the check group. The checks
   still run and show what they find: a failing check shows
   `confirming_failure`, or `failed` once it has failed for
   `alert_after_failing_for`, which `sudo mmwatchdog events myapp` then
   records. MMWatchdog sends no alert and restarts nothing. To prove the
   window holds, break the check on purpose for longer than its
   `alert_after_failing_for` and its `restart_after_failing_for`: the
   destinations get nothing but the `maintenance on` message, and `events`
   shows no `systemd_unit_restart_requested`. The
   reminders of a problem that was open before the window wait too, and
   come again one `remind_every` after the window, if the problem lasts.

4. **End the window** when the work is done:

   ```sh
   sudo mmwatchdog maintenance off myapp
   ```

   ```text
   myapp is out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.
   ```

   **Check:** the destinations get `Info: app01: maintenance off for myapp`,
   and `sudo mmwatchdog status myapp` has no `Maintenance on` line. Each
   check waits for its next run before it alerts or restarts anything, so a
   check may show `unknown` with `waiting for a new run` for up to its
   `check_every`. Then the runs decide: a check that still fails alerts,
   and a failure that ended during the window sends nothing, neither the
   failure nor its recovery. MMWatchdog never sends later what the window
   held back.

If you forget step 4, the window ends by itself after `--for`, and the
destinations get the message with the word `expired`:

```text
Info: app01: maintenance off for myapp
maintenance expired: short test
```

## Pause several check groups

1. **Open one window for all of them,** with their names:

   ```sh
   sudo mmwatchdog maintenance on myapp machine --for 10m --reason "deploy v1.4"
   ```

   ```text
   myapp, machine are in maintenance until 2026-11-15 04:52 UTC (10m).
     MMWatchdog keeps running the checks of the check groups in maintenance and shows the results in mmwatchdog status.
     During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for the check groups in maintenance.
     To end the maintenance early: sudo mmwatchdog maintenance off myapp machine
   ```

   **Check:** the destinations get one message for the command, which names
   every check group: `Info: app01: maintenance on for myapp, machine`.

2. **Do the work.**

3. **End the window:**

   ```sh
   sudo mmwatchdog maintenance off myapp machine
   ```

   ```text
   myapp, machine are out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.
   ```

   **Check:** the destinations get one message,
   `Info: app01: maintenance off for myapp, machine`.

## Pause every check group

Use this for work on the node itself, such as a kernel update and a reboot.

1. **Open the whole-node window:**

   ```sh
   sudo mmwatchdog maintenance on --every-check-group --for 15m --reason "kernel update and reboot"
   ```

   ```text
   Every check group on this node is in maintenance until 2026-11-15 04:57 UTC (15m).
     MMWatchdog keeps running the checks of every check group and shows the results in mmwatchdog status.
     During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for the check groups in maintenance.
     To end the maintenance early: sudo mmwatchdog maintenance off --every-check-group
   ```

   The window also covers a check group that a deploy adds during the
   window. A reboot during the window sends no reboot notice, also when the
   window ends while the node is down. Without the window, each reboot
   sends the notice `app01 rebooted`; a window of a check group by name
   does not hold that notice back.

   **Check:** the destinations get
   `Info: app01: maintenance on for the whole node`.

2. **Do the work,** such as `sudo apt upgrade` and `sudo reboot`. The window
   survives the reboot and a restart of MMWatchdog.

3. **End the window:**

   ```sh
   sudo mmwatchdog maintenance off --every-check-group
   ```

   ```text
   The maintenance for every check group on this node is off. Alerts, restarts and recovery commands resume after fresh checks. A check group that has its own maintenance window stays in maintenance until that window ends.
   ```

   **Check:** the destinations get
   `Info: app01: maintenance off for the whole node`. As the output says, a
   check group that you paused with its own name stays paused until its own
   window ends: end it with `sudo mmwatchdog maintenance off myapp`.

## Deploys through MMX

`mmx run hub_and_standalone` pauses alerts by itself. A run that changes
the node's MMWatchdog configuration, modules, secret files, or program
opens a whole-node window for 15 minutes, restarts MMWatchdog, and ends the
window. Reason: the restart makes every check wait for a new run, and the
window keeps the restart from sending alerts. The destinations get the two
`info` messages, and `sudo mmwatchdog events` shows the window:

```text
2026-11-15T04:40:00Z maintenance_off      every_check_group maintenance turned off by root: mmwatchdog deploy
2026-11-15T04:39:59Z maintenance_on       every_check_group maintenance until 2026-11-15 04:54 UTC, turned on by root: mmwatchdog deploy
```

- A run that changes nothing opens no window and sends nothing.
- A run that fails after the window opened leaves the window to end by
  itself, within 15 minutes.
- A run during a whole-node window that you opened yourself keeps your
  window, with its end time and reason, and opens none of its own, so the
  run's restart is already inside your window. A window of a check group
  by name stays too.
- A project that deploys often, such as from a scheduled job, can keep
  the two messages out of the destinations: in each playbook, add
  `--without-maintenance-on-and-off-alerts` to the `mmwatchdog maintenance
  on` line of the node task `start_maintenance`. `sudo mmwatchdog events`
  still shows each window. An update brings new playbooks, so make the
  change again after each update.

## Stop MMWatchdog for a while

Prefer a maintenance window: the checks keep running, so you see the
results, and the window ends by itself. Stop MMWatchdog only when it must
not run at all, such as while you test the node's disk in a way that a
check would disturb.

1. **Stop the service:**

   ```sh
   sudo systemctl stop mmwatchdog
   ```

   MMWatchdog sends nothing when it stops, and runs no check, sends no
   alert, and restarts nothing until it starts again. Nothing reminds you
   that it is stopped.

   An `mmx run` of the playbook starts MMWatchdog again, and enables it at
   boot, also after `systemctl disable`. Do not run the playbook for this
   node while MMWatchdog must stay stopped.

   **Check:** `systemctl is-active mmwatchdog` prints `inactive`.

2. **Start it again** when the work is done:

   ```sh
   sudo systemctl start mmwatchdog
   ```

   **Check:** `sudo mmwatchdog status` lists the checks. A check that has
   not run since the start shows `unknown` with `waiting for a new run`. A
   start within the same boot sends no message. A stopped MMWatchdog also starts
   again when the node boots, because the service stays enabled.

On a node that sends check-ins to MMHotline, or that has joined a fleet
hub, a stopped MMWatchdog stops reporting, and MMHotline or the hub alerts.
On a joined node, open a whole-node window before you stop the service, as
[If something is different](#if-something-is-different) says. To keep
MMWatchdog stopped across reboots too, use `sudo systemctl disable --now
mmwatchdog`, and `sudo systemctl enable --now mmwatchdog` to undo it.
[Remove MMWatchdog from a node](remove-mmwatchdog.md) covers removing
MMWatchdog.

## If something is different

- **The work takes longer than `--for`.** Run `maintenance on` again for
  the same check group. The new window replaces the old one, and the
  destinations get a new message with the new end:

  ```sh
  sudo mmwatchdog maintenance on myapp --for 2h --reason "moving the myapp database, second try"
  ```

  `--for` has no upper limit, and `d` means days, so `--for 30d` is valid.
- **Routine work that the team does not need to hear about,** such as a
  nightly data import. Add `--without-maintenance-on-and-off-alerts`:

  ```sh
  sudo mmwatchdog maintenance on myapp --for 10m --reason "nightly data import" --without-maintenance-on-and-off-alerts
  ```

  ```text
  myapp is in maintenance until 2026-11-15 04:52 UTC (10m).
  ...
    Maintenance start and end are not announced.
  ```

  The window works as any other, and its start and end are still in `sudo
  mmwatchdog events`; only the two messages are not sent, also when the
  window expires. `sudo mmwatchdog status` says `maintenance start and end
  are not announced` on the `Maintenance on` line.
- **The command refuses the window.** The command opens no window at all
  and exits with code 4:

  | Error | Fix |
  |---|---|
  | `mmwatchdog: unknown check group name "myap"` | Use a name that `sudo mmwatchdog status` lists |
  | `mmwatchdog: maintenance on requires --for with a positive duration, such as 1h or 30d` | Add `--for`, such as `--for 1h` |
  | `mmwatchdog: maintenance takes check group names or --every-check-group, not both` | Write the names, or `--every-check-group` |
  | `mmwatchdog: maintenance reason must be single-line text of at most 200 bytes` | Shorten `--reason` to one line of at most 200 bytes |

- **You stop a service by hand.** MMWatchdog never starts a systemd unit
  that is stopped, so the unit stays stopped, and its
  `systemd_unit_state` check alerts after `alert_after_failing_for`. Open a
  window for its check group first.
- **A check group has a heartbeat check.** When the window ends,
  MMWatchdog sends the alert of a heartbeat that was missed during the
  window, without waiting for a new run.
- **The node has joined a fleet hub.** Run the command on the node, as
  above. `maintenance on` and `off` wait until the hub has the change, at
  most 25 seconds, so a reboot right after `maintenance on` does not look
  like a node that stopped reporting. If the hub does not accept the
  change, the window is still on or off on the node, and the command says
  so and exits with code 1, so a script can stop before it reboots the
  node. A window covers only the node where you run the command: for work
  on several nodes, run the command on each.
- **The node is the fleet hub.** The whole-node window on the hub also
  covers `node_reports`, the check of the joined nodes, so the hub sends no
  alert that a joined node stopped reporting. To pause only those alerts,
  for example while you move several joined nodes, run
  `sudo mmwatchdog maintenance on node_reports --for 1h` on the hub.
  [Watch a fleet](watch-a-fleet.md) explains the fleet.
- **A deploy removes a check group** that has a window. The window ends
  with the check group, with its end message.

## Related pages

- [Send alerts where your team sees them](send-alerts-somewhere.md)
- [See what is happening](see-what-is-happening.md)
- [Add a check](add-a-check.md)
- The [command reference](../reference/commands.md), with every command of
  MMWatchdog.
