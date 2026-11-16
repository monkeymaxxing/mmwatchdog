# Watch a fleet

**When to use this page:** your nodes report to a fleet hub, and you want
to see the state of the whole fleet or of one node, find out why a node
needs attention, or know what happens when a node or the hub goes silent.

**Who acts:** you, or your coding agent, on the fleet hub, logged in with
an account that can use `sudo`. Every command on this page only reads,
except where a step says to start or stop something.

**What this page does:** you read the fleet with `sudo mmwatchdog fleet`
on the hub: every node at once, one node in detail, or the nodes of one
node group. Then you see what the hub sends when a node stops reporting,
and what the nodes send when the hub stops.

**Terms:** the **fleet hub** is the node that receives a **node status
report** from each **joined node** every `report_every`, 30 seconds in the
starter settings. A node is **silent** when the hub has received no report
from it for three report intervals, 90 seconds. A **node group** is a name,
such as `web_servers`, that a node gives itself to be shown with the other
nodes of the same name. **Hub-outage alerter nodes** are the two joined
nodes that alert you when they cannot reach the hub.

## Choose what to see

| You want to see | Section |
|---|---|
| Whether the whole fleet is healthy, and which nodes need attention | [See the whole fleet](#see-the-whole-fleet) |
| One node's checks, events, and connection to the hub | [See one node](#see-one-node) |
| Only the nodes of one node group, such as the web servers | [See one node group](#see-one-node-group) |
| How memory, CPU, or another number of a node changed over time | [Look back at measurements](look-back-at-measurements.md#read-the-readings-of-every-node-on-the-hub) |
| Why the hub alerted that a node stopped reporting | [When a node goes silent](#when-a-node-goes-silent) |
| What happens when the hub itself stops | [When the hub stops](#when-the-hub-stops) |

## Before you start

- **The nodes report to a hub.** [Set up a fleet](set-up-a-fleet.md) says
  how.
- **You run the commands on the hub.** On another node,
  `sudo mmwatchdog fleet` exits with code 4 and says
  `fleet inspection runs on the hub; this node has no fleet_hub_role setting`.
  The examples use the hub `hub01` at 203.0.113.10, and the joined nodes
  `web01` and `web02`. Log in with `ssh deploy@203.0.113.10`, or put
  `ssh deploy@203.0.113.10` before each command.

## See the whole fleet

1. **Show the fleet.** On the hub:

   ```sh
   sudo mmwatchdog fleet
   ```

   When every node is healthy, the output is short:

   ```text
   Fleet at 203.0.113.10:8445: 3 nodes, 3 healthy
   Hub-outage alerter nodes: web01, web02
   ```

   The first line gives the hub's address, the number of nodes with the
   hub, and the number of healthy ones. A node is healthy when it reports,
   the exit code of its status is 0, and its alert destinations accept its
   alerts.

   **Check:** the number of nodes is the number of your nodes. A node that
   never joined is not counted: see
   [Set up a fleet](set-up-a-fleet.md#if-something-is-different).

2. **Read each node that needs attention.** Each such node has a line, and
   below it the checks that are not `ok`:

   ```text
   Fleet at 203.0.113.10:8445: 3 nodes, 2 healthy
   web01                    reporting  exit 1  cpu 0%  memory 18%  disk 9%  last report 2026-11-15T08:26:20Z (1s ago)
     myapp/health_page_answers failed since 2026-11-15T08:25:51Z: expected HTTP 200, received 404
   Hub-outage alerter nodes: web01, web02
   ```

   The line gives the node's name, its state, its exit code, its CPU,
   memory, and root disk use, and the time of its last report. The state
   is one of three:

   | State | Meaning |
   |---|---|
   | `reporting` | A report arrived within the last three report intervals |
   | `waiting_for_first_report` | The node joined, or the hub started, less than three report intervals ago, and no report came since |
   | `silent` | No report within three report intervals. The lines below show the node's state at its last report |

   Other lines may follow: the node's node groups, a destination that does
   not accept the node's alerts, alerts that wait for the hub, a name
   conflict, the nodes in maintenance, and `Refused joins:`, the joins with
   a wrong join code.

   **Check:** read the exit code of `sudo mmwatchdog fleet` together with
   the count of healthy nodes. The exit code is that of the worst node: 1
   when a check has failed or a node is silent, 3 when a check's state is
   unknown, a destination does not accept a node's alerts, or the hub has
   a storage problem, and 0 otherwise. So exit code 0 does not prove that
   every node is healthy: a node that is listed only for a name conflict,
   for a clock that is off by more than five seconds, or because it waits
   for its first report, counts as not healthy and leaves the exit code at
   0. A script or a coding agent tests the exit code and compares the two
   numbers on the first line, or reads `sudo mmwatchdog fleet --json`.

## See one node

1. **Show the node,** with its name from `sudo mmwatchdog fleet`, which is
   the node's host name:

   ```sh
   sudo mmwatchdog fleet web01
   ```

   ```text
   web01: reporting, exit 0
     Contact: last report 2026-11-15T08:28:20Z (8s ago)
     Joined: 2026-11-15T08:23:20Z from 203.0.113.11
     Node groups: none
     Watches the hub
     Build: mmwatchdog 0.1.0 (ad75e5909a0af9b03215d2b2ac4c5bdfa9a56777, linux/amd64); configuration 8abb938b96cd
     Node resources: cpu 0%  memory 18%  disk 9%
     Alert deliveries: 0 waiting, 0 dropped
   machine/cpu_use_is_recorded  ok           cpu_used_percent is 0.38 over 29.998s
   ...
   myapp/health_page_answers    ok           HTTP 200 and configured content match
   myapp/service_is_running     ok           systemd unit myapp.service is active
   node_reports/to_hub          ok           reports reach the hub
   Recent events:
   2026-11-15T08:27:51Z recovered            myapp/health_page_answers HTTP 200 and configured content match
   2026-11-15T08:25:51Z failed               myapp/health_page_answers expected HTTP 200, received 404
   2026-11-15T08:23:20Z node_joined           joined from 203.0.113.11
   ```

   The checks are those that `sudo mmwatchdog status` shows on the node,
   as of its last report. `Watches the hub` says that the node is a
   hub-outage alerter node; such a node also has the check
   `node_reports/to_hub`. `Recent events` lists the latest 100 events.

   **Check:** the first line says `reporting`. `exit` is the exit code that
   `sudo mmwatchdog status` has on the node. A name that the hub does not
   know exits with code 4: `unknown node "web03"`.

2. **Show the node's events of a time span,** such as the last hour, when
   the latest 100 are not enough:

   ```sh
   sudo mmwatchdog fleet web02 --last 1h
   ```

   ```text
   ...
   2026-11-15T08:36:21Z maintenance_off      every_check_group maintenance turned off by root: mmwatchdog deploy
   2026-11-15T08:36:18Z maintenance_on       every_check_group maintenance until 2026-11-15 08:51 UTC, turned on by root: mmwatchdog deploy
   2026-11-15T08:32:50Z recovered            node_reports/to_hub reports reach the hub again
   2026-11-15T08:31:20Z failed               node_reports/to_hub no report from this node has reached the MMWatchdog fleet hub since 08:28 UTC; mmwatchdog status on this node shows the latest error
   ```

   `--since 2026-11-15T08:00:00Z`, with `--until`, gives a span between
   two times. The hub keeps the events of a node also while the node is
   silent.

   **Check:** the events cover the time that you asked for.

## See one node group

A node is in the node groups that it names in its own settings, as
[Put nodes in node groups](change-a-fleet.md#put-nodes-in-node-groups)
says.

1. **Show the nodes of the node group:**

   ```sh
   sudo mmwatchdog fleet --node-group web_servers
   ```

   ```text
   Node group web_servers of the fleet at 203.0.113.10:8445: 2 nodes, 1 healthy
   web02                    silent     exit 1  last report 2026-11-15T08:36:47Z (2m25s ago)
     Node groups: web_servers
   ```

   The view lists the nodes of the node group that need attention, as
   `sudo mmwatchdog fleet` does, and leaves out the lines about the whole
   fleet, such as the hub-outage alerter nodes.

   **Check:** the exit code is that of the worst node of the node group. A
   name that no node states exits with code 4 and lists the node groups
   that exist:
   `no node is in a node group named "berlin"; the node groups of this fleet are web_servers`.

## When a node goes silent

A node goes silent when MMWatchdog on it stops, when the node shuts down,
or when its reports cannot reach the hub. Each case looks the same to the
hub: the reports stop. After three report intervals without a report, 90
seconds with `report_every: 30s`, the hub alerts. These steps show what
you get, using `web02`, where MMWatchdog was stopped with
`sudo systemctl stop mmwatchdog`.

1. **Read the alert.** The hub sends it to its destinations:

   ```text
   Warning: web02 stopped reporting to the MMWatchdog fleet hub
   web02 has sent no report to the hub since 08:36 UTC
   failing since 08:38 UTC
   ```

   The severity comes from `alert_severity_when_this_node_goes_silent` in
   the node's own settings: `warn` in the starter
   `files/join_hub/mmwatchdog_node_settings.yml`, so one silent node among
   many wakes no one. The hub keeps that severity while the node is
   silent.

2. **Find the node on the hub:**

   ```sh
   sudo mmwatchdog fleet
   ```

   ```text
   Fleet at 203.0.113.10:8445: 3 nodes, 1 healthy
   hub01                    reporting  exit 1  cpu 0%  memory 18%  disk 9%  last report 2026-11-15T08:39:05Z (7s ago)
     node_reports/web02 failed since 2026-11-15T08:38:17Z: web02 has sent no report to the hub since 08:36 UTC
   web02                    silent     exit 1  last report 2026-11-15T08:36:47Z (2m25s ago)
     Node groups: web_servers
   Hub-outage alerter nodes: web01
   ```

   The hub watches each joined node with a check of its own check group
   `node_reports`, here `node_reports/web02`, so the hub itself shows a
   failed check. While `web02` is silent, the hub chooses its hub-outage
   alerter nodes from the nodes that report.

   **Check:** the node's line says `silent`, with the time of its last
   report.

3. **Find out why, on the node.** Log in to the node. If you cannot, the
   node or its network is down. If you can, `systemctl is-active
   mmwatchdog` says whether MMWatchdog runs, and the last line of
   `sudo mmwatchdog status` gives the latest error of its reports, such as
   `connect: connection refused`. Fix the cause, such as with
   `sudo systemctl start mmwatchdog`.

4. **Confirm that the node reports again.** With its next report, the
   hub sends:

   ```text
   Resolved warning: web02 reports to the MMWatchdog fleet hub again
   web02 is reporting again
   ```

   **Check:** `sudo mmwatchdog fleet` no longer lists the node.

A silence that you plan, such as a reboot or a move of the node, needs no
alert: open a whole-node maintenance window on the node first, as
[Pause alerts during planned work](pause-alerts-during-planned-work.md#if-something-is-different)
says. A node that you take out of the fleet for good is removed with
[Remove a node](change-a-fleet.md#remove-a-node).

## When the hub stops

The nodes keep running their checks and restarting their services without
the hub. Two things change while the hub is down:

- **Each node sends its alerts itself.** An alert that the hub has not
  taken goes from the node straight to the destinations, 30 seconds after
  the alert happened (`send_alert_without_hub_after_waiting_for`). The
  nodes use their copy of the hub's destinations.
- **The hub-outage alerter nodes alert you.** The hub chooses two joined
  nodes, or the only one in a fleet of two nodes. `sudo mmwatchdog fleet`
  names them on the line `Hub-outage alerter nodes:`. When such a node has
  not reached the hub for 3 minutes (`alert_after_hub_unreachable_for`),
  it sends, at the severity of the hub's own
  `alert_severity_when_this_node_goes_silent`, `sos` in the starter
  settings:

  ```text
  SOS: web01 cannot reach the MMWatchdog fleet hub
  no report from this node has reached the MMWatchdog fleet hub since 08:28 UTC; mmwatchdog status on this node shows the latest error
  failing since 08:31 UTC
  ```

  Each hub-outage alerter node sends such an alert, so you get two. The
  network error is not in the alert, which may show on a phone's lock
  screen. The last line of `sudo mmwatchdog status` on the node gives it:

  ```text
  Fleet hub: 203.0.113.10:8445 as web01; last report 2026-11-15T08:28:20Z; error at 2026-11-15T08:32:20Z: Post "https://203.0.113.10:8445/v1/report": dial tcp 203.0.113.10:8445: connect: connection refused
  ```

When the hub runs again, the nodes report to it, the hub gets the events
that it missed, and each hub-outage alerter node sends
`Resolved SOS: web01 reaches the MMWatchdog fleet hub again`. A recovery
of a check after the hub is back goes through the hub again.

To check that the hub runs, log in to the hub: `systemctl is-active
mmwatchdog` prints `active`, and `sudo mmwatchdog status` ends with
`Fleet hub: nodes report to 203.0.113.10:8445; see mmwatchdog fleet`.

## If something is different

- **A node is listed with `Name conflict:`,** such as
  `Name conflict: a node at 203.0.113.12 claimed this name at 2026-11-15T08:48:05Z`,
  and counted as not healthy, while its checks are `ok`. Another machine
  with the same host name tried to join the fleet, and the hub refused it,
  because the node with that name still reports. The line, and the count
  as not healthy, stay for one hour after the latest refused join; no
  command clears them sooner. Each new attempt of the other machine starts
  the hour again, so find the machine at the address in the line, and give
  it a host name of its own or stop MMWatchdog on it. A line that stays
  after that hour is a new attempt. `sudo mmwatchdog fleet web01` shows
  the latest conflict also after the hour. To replace a node by a new
  server with the same host name, see
  [Change a fleet](change-a-fleet.md#if-something-is-different).
- **A silent node must wake someone after a while.** Add
  `raise_to_sos_after_this_node_is_silent_for: 10m` to the node's settings,
  such as `files/join_hub/mmwatchdog_node_settings.yml`, and run
  `mmx run join_hub --inventory hub_members`. The hub then raises the
  node's silence alert to `sos` after 10 minutes without a report. A
  raised silence alert stays `sos` until the node reports again.
- **Many silent nodes at once must wake someone.** On the hub, add
  `raise_to_sos_when_silent_nodes_reach_count: 3` to `fleet_hub_role`.
  When 3 nodes are silent at the same time, the hub raises the alert of
  each silent node to `sos`, such as `3 of 30 nodes are not reporting to
  the hub at the same time, so this alert is now SOS
  (raise_to_sos_when_silent_nodes_reach_count: 3)`. A node whose silence
  alert a maintenance window holds does not count.
  `raise_to_sos_when_silent_nodes_reach_percent: 5` counts a share of the
  nodes instead. In a small fleet one silent node can reach a share: with
  10 nodes, one node is 10 percent, more than 5. So use a count there.
- **Two nodes do one job, such as two job servers.** On both, add
  `raise_to_sos_when_every_node_of_these_node_groups_is_silent: [jobs_servers]`
  beside `this_node_is_in_node_groups: [jobs_servers]`, as
  [Put nodes in node groups](change-a-fleet.md#put-nodes-in-node-groups)
  shows. One silent job
  server alerts at its own severity; when both are silent, the hub raises
  both alerts to `sos`. A node group that holds the hub is never silent.
- **The alerts about the hub must go to one destination,** such as a
  phone app that does not depend on the hub's server. On the hub, set
  `send_silence_alerts_of_this_node_only_to_destinations: [on_call_push_app]`
  in `files/hub_and_standalone/mmwatchdog_node_settings.yml`, with a
  destination of that name in the hub's `alert_destinations.yml`, and run
  `mmx run hub_and_standalone`.
- **Some nodes should be the hub-outage alerter nodes,** such as servers
  that stay for a long time, rather than nodes that come and go. Give them
  `prefer_this_node_as_hub_outage_alerter: true` under `join_fleet_hub`
  in their settings. The hub then chooses them first.
- **Many nodes go silent at once,** such as during planned work on the
  network. On the hub, `sudo mmwatchdog maintenance on node_reports --for
  1h --reason "network work"` holds every silence alert for an hour. A
  node that is still silent when the window ends alerts then.
- **The hub and both hub-outage alerter nodes fail together.** Then
  nothing alerts until one of them runs again. An `mmhotline` destination
  on the hub covers that case: the hub sends MMHotline a check-in every 30
  seconds, and MMHotline alerts when the check-ins stop.
  [MMHotline as a destination](send-alerts-somewhere.md#mmhotline-as-a-destination)
  says how.
- **A program on the hub must read the fleet,** such as a dashboard.
  `sudo mmwatchdog fleet --json` gives the same facts as JSON: each node
  in `nodes`, with `name`, `state`, `exit_code`,
  `this_node_is_in_node_groups`, and `last_report_received_at`.
  [Fleet](../how-it-works.md#the-fleet), in the paragraph "Another program on the
  hub reads the fleet view", says how to let a program read it without
  `sudo`.
- **You want the details of the rules,** such as when two alerts of
  different nodes become one message, or what a node's report holds:
  [Fleet](../how-it-works.md#the-fleet) and
  [Alerts in a fleet](set-up-a-fleet.md#where-alerts-go-in-a-fleet).

## Related pages

- [Set up a fleet](set-up-a-fleet.md)
- [Change a fleet](change-a-fleet.md)
- [See what is happening](see-what-is-happening.md), for one node
- [Look back at measurements](look-back-at-measurements.md)
- [Pause alerts during planned work](pause-alerts-during-planned-work.md)
