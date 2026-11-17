# Change a fleet

**When to use this page:** your nodes report to a fleet hub, and you want
to add a node, remove a node for good, put nodes in node groups, give the
fleet a new join code, or move the hub to another server.

**Who acts:** you, or your coding agent. Most steps run on your
**controller machine**, the machine where you run `mmx`, in the MMX project
that set up the fleet. Some steps run a command on the hub or on a node
through `ssh`, as the step shows. A coding agent shows the person the
output of each `mmx dryrun` and waits for their yes before the `mmx run`
that follows it, and asks the person to put each secret into the vault
themselves. The agent runs `mmx dryrun` and `mmx run` with
`MMX_VAULT_PASSWORD_FILE` set, as
[Install MMWatchdog on one node](install-on-one-node.md) shows.

**What this page does:** each section makes one change to the fleet with
MMX or with a `mmwatchdog fleet` command on the hub, and checks the result
with `sudo mmwatchdog fleet` on the hub.

**Terms:** the **fleet hub** is the node that the other nodes report to.
A **joined node** is a node that reports to the hub. The **join code** is
the secret that lets a node join the hub; the vault of the MMX project
holds it as `mmwatchdog_join_code`. A **node group** is a name, such as
`web_servers`, that a node gives itself, so that the hub can show the
nodes of that name together.

## Choose the change

| You want to | Section |
|---|---|
| Watch one more server with the fleet | [Add a node](#add-a-node) |
| Stop watching a server, such as one that you shut down for good | [Remove a node](#remove-a-node) |
| See the web servers, or the nodes of one data center, together | [Put nodes in node groups](#put-nodes-in-node-groups) |
| Make the join code useless to someone who may have seen it | [Rotate the join code](#rotate-the-join-code) |
| Make another server the hub, with the same fleet | [Move the hub to another server](#move-the-hub-to-another-server) |

## Before you start

- **The fleet was set up with MMX,** as [Set up a fleet](set-up-a-fleet.md)
  says. You work in that MMX project: the hub is in
  `inventories/hub_and_standalone.yml`, the joined nodes are in
  `inventories/hub_members.yml`, and the vault holds
  `mmwatchdog_join_code`.
- **You can log in to the hub,** here `hub01` at 203.0.113.10, and run
  `sudo mmwatchdog fleet` there. The joined nodes are `web01` and `web02`,
  at 203.0.113.11 and 203.0.113.12.

## Add a node

1. **Name the node in the inventory of the joined nodes.** In
   `inventories/hub_members.yml`, under `nodes`, add the node:

   ```yaml
   nodes:
     web01:
       address: 203.0.113.11
       ssh_user: deploy

     web02:
       address: 203.0.113.12        # the new node
       ssh_user: deploy
   ```

   The node needs Linux with systemd, and an account that can use `sudo`
   without a password, as for the other nodes.

   **Check:** you can log in, and the project is valid:

   ```sh
   ssh deploy@203.0.113.12 true && echo login works
   mmx validate join_hub --inventory hub_members
   ```

   ```text
   login works
   Valid: mmwatchdog
   ```

2. **Preview the change:**

   ```sh
   mmx dryrun join_hub --inventory hub_members
   ```

   **Check:** the lines of the new node say `would_change`, and the lines
   of the other nodes say `unchanged`. The exit code is 3, because the new
   node has no MMWatchdog yet, so some steps cannot be checked in advance.

3. **Install MMWatchdog on the node, and join it to the hub:**

   ```sh
   mmx run join_hub --inventory hub_members
   ```

   ```text
   Run b7da8267: succeeded
   2 finished
   ```

   The run changes nothing on the nodes that already run this
   configuration.

4. **Check that the node joined.** On the hub:

   ```sh
   sudo mmwatchdog fleet web02
   ```

   ```text
   web02: reporting, exit 0
     Contact: last report 2026-11-15T08:41:00Z (21s ago)
     Joined: 2026-11-15T08:41:00Z from 203.0.113.12
     Node groups: web_servers
   ...
   ```

   **Check:** the node is `reporting`. A node joins with its first report,
   a few seconds after MMWatchdog starts on it. If the hub answers
   `unknown node "web02"` after a minute, the last line of
   `sudo mmwatchdog status` on the node gives the reason, such as
   `joining the hub: hub returned HTTP 403: invalid join code`.

## Remove a node

Remove a node from the fleet when you shut it down for good, or when it
should no longer be watched. Without these steps, the hub alerts that the
node stopped reporting. Do the steps in this order: first the node, then
the inventory, then the hub.

1. **Remove MMWatchdog from the node,** if the node still runs. Follow
   [Remove MMWatchdog from a node](remove-mmwatchdog.md#steps), steps 1
   to 8, then come back here for step 2; step 2 below takes the place of
   step 9 there. A joined node is in `inventories/hub_members.yml`, so
   step 8 there names that inventory:

   ```sh
   mmx remove-mmx-from-node web02 --inventory hub_members
   ```

   To keep MMWatchdog's files on the node, do only step 2 there,
   `sudo systemctl disable --now mmwatchdog`. If the server is already
   shut down for good, skip this step.

   Reason: a node whose MMWatchdog still runs joins the hub again with its
   next report, and `mmx remove-mmx-from-node` finds the node only while
   the node is still in the inventory.

   **Check:** `ssh deploy@203.0.113.12 systemctl is-active mmwatchdog`
   prints `inactive`.

2. **Remove the node from the inventory.** Delete the node's entry from
   `inventories/hub_members.yml`, so that the next
   `mmx run join_hub --inventory hub_members` does not install it again.

   **Check:** `mmx validate join_hub --inventory hub_members` prints
   `Valid: mmwatchdog`, and `mmx nodes join_hub --inventory hub_members` no
   longer lists the node.

3. **Remove the node from the hub.** On the hub:

   ```sh
   sudo mmwatchdog fleet forget web02
   ```

   ```text
   Removed node web02 from the fleet; if MMWatchdog still runs on the removed node, the node joins again with the current join code.
   ```

   The hub deletes the node's entry, its last report, its events, its
   readings in the measurement history on the hub, and its credential. If
   the hub had alerted that the node stopped reporting, it closes that
   alert with
   `web02 was removed from the fleet; MMWatchdog does not know whether the problem is fixed`.

   **Check:** `sudo mmwatchdog fleet` counts one node less, and does not
   list the node:

   ```text
   Fleet at 203.0.113.10:8445: 2 nodes, 2 healthy
   Hub-outage alerter nodes: web01
   ```

   If the node was a hub-outage alerter node, the hub chooses another one
   with the next report of a joined node. The hub cannot remove itself:
   `sudo mmwatchdog fleet forget hub01` says
   `the hub cannot forget itself`.

## Put nodes in node groups

A node names its node groups in its own settings. The hub has no list of
node groups: a node group exists while a node names it. A node can be in
several node groups, such as `web_servers` and `frankfurt`.

1. **Name the node group in the nodes' settings.** For every node that
   uses `files/join_hub/`, put this line in
   `files/join_hub/mmwatchdog_node_settings.yml`:

   ```yaml
   this_node_is_in_node_groups: [web_servers]
   ```

   If the file already has a `this_node_is_in_node_groups` line, change
   that line instead of adding a second one: replace the old name, or add
   the new name to the list, such as `[web_servers, frankfurt]`. The
   setting is always a list, also for one name. A name has at most 64
   letters, digits, hyphens, and underscores. For nodes of another job,
   write the line in their own configuration directory, as
   [Set up a fleet](set-up-a-fleet.md#if-something-is-different) says. The
   hub can be in node groups too, with the same line in
   `files/hub_and_standalone/mmwatchdog_node_settings.yml`.

   **Check:** `mmx validate join_hub --inventory hub_members` prints
   `Valid: mmwatchdog`.

2. **Preview and install the change:**

   ```sh
   mmx dryrun join_hub --inventory hub_members
   mmx run join_hub --inventory hub_members
   ```

   ```text
   Run e662682d: succeeded
   2 finished
   ```

   **Check:** the dryrun exits with code 2, and lists
   `install_config: succeeded, would_change` for each node. The run ends
   with `succeeded`. A fault in the line, such as
   `this_node_is_in_node_groups: expected a list, found the string "web_servers"`,
   fails the run at `validate_config`, and changes nothing on the nodes.

3. **See the node group on the hub.** Each node sends its node groups with
   its next report:

   ```sh
   sudo mmwatchdog fleet --node-group web_servers
   ```

   ```text
   Node group web_servers of the fleet at 203.0.113.10:8445: 2 nodes, 2 healthy
   ```

   **Check:** the count is the number of nodes that you put in the node
   group. `sudo mmwatchdog fleet web01` shows `Node groups: web_servers`.
   [Watch a fleet](watch-a-fleet.md#see-one-node-group) says how to read
   the view.

## Rotate the join code

Rotate the join code when someone who should not have it may have seen
it, such as after it was pasted into a chat. A node that joins with the
code gets the fleet settings, with the secrets of your alert destinations.

1. **Make a new join code.** On the hub:

   ```sh
   sudo mmwatchdog fleet rotate-join-code
   ```

   ```text
   mmwatchdog-join 203.0.113.10:8445 ebba574e17670386674eeb04912fdd7555fb626c4f124addab10c6a5888168ee 2f543d908806e06776c120ae807508dbf104a13a95e9b806a2cc8cd9b418a34f
   ```

   The command prints the new code. Only its last part, the join secret,
   is new. From now on the old code joins no node. The joined nodes keep
   reporting: they joined with the old code, and report with a credential
   of their own.

   **Check:** `sudo mmwatchdog fleet join-code` prints the same new code.

2. **Put the new code in the vault,** straight from the hub, in place of
   the old code:

   ```sh
   ssh deploy@203.0.113.10 sudo mmwatchdog fleet join-code | mmx secrets set --replace mmwatchdog_join_code
   ```

   **Check:** `mmx secrets set` prints nothing, and `mmx secrets diff-keys`
   lists `mmwatchdog_join_code` as changed.

3. **Give every node the new code:**

   ```sh
   mmx dryrun join_hub --inventory hub_members
   mmx run join_hub --inventory hub_members
   ```

   Reason: a node uses the code when it has to join again, such as after
   its host name changes or after it was removed from the hub. With the old
   code, such a node cannot join, and the hub shows only
   `Refused joins: 1 with a wrong join code, latest from 203.0.113.12 at 2026-11-15T08:43:00Z`.

   **Check:** the dryrun lists `join_code: succeeded, would_change` for
   each node, and the run ends with `succeeded`. The run restarts
   MMWatchdog on each node, because the join code changed.
   `sudo mmwatchdog fleet` on the hub then counts every node as healthy.

## Move the hub to another server

Move the hub when its server must go, such as to a larger server. The
fleet keeps its identity: the hub's key, certificate, join secret, nodes,
events, and the readings of the nodes are in `/var/lib/mmwatchdog/` on the
hub, and you copy that directory to the new server. The steps move the hub
from `hub01` at 203.0.113.10 to `hub02` at 203.0.113.13, a server where
MMWatchdog has never run. The nodes cannot reach a hub for a few minutes
during the move; they keep their checks, and send alerts themselves.

1. **See which nodes are the hub-outage alerter nodes.** On the old hub:

   ```sh
   sudo mmwatchdog fleet
   ```

   ```text
   Fleet at 203.0.113.10:8445: 3 nodes, 3 healthy
   Hub-outage alerter nodes: web01, web02
   ```

   **Check:** the line `Hub-outage alerter nodes:` names one or two nodes.

2. **Hold the alerts about the hub on each of those nodes,** for a little
   longer than the move takes:

   ```sh
   ssh deploy@203.0.113.11 sudo mmwatchdog maintenance on node_reports --for 30m --reason "moving the fleet hub"
   ssh deploy@203.0.113.12 sudo mmwatchdog maintenance on node_reports --for 30m --reason "moving the fleet hub"
   ```

   ```text
   node_reports is in maintenance until 2026-11-15 10:14 UTC (30m).
   ...
     The MMWatchdog fleet hub received the maintenance change.
   ```

   Reason: each hub-outage alerter node alerts when it has not reached the
   hub for 3 minutes, and the move may take longer.

   **Check:** each command ends with `The MMWatchdog fleet hub received the
   maintenance change.` Slack gets one start message, for the first node:
   the hub groups windows with the same reason and account, and names
   every node of the group in the one message when the windows end.

3. **Stop MMWatchdog on the old hub for good:**

   ```sh
   ssh deploy@203.0.113.10 sudo systemctl disable --now mmwatchdog
   ```

   Reason: two hubs with the same state must never run at the same time,
   and the state must not change during the copy.

   **Check:** `ssh deploy@203.0.113.10 systemctl is-active mmwatchdog`
   prints `inactive`.

4. **Copy the hub's state to the new server,** straight from server to
   server:

   ```sh
   ssh deploy@203.0.113.10 sudo tar -C /var/lib -czf - mmwatchdog | ssh deploy@203.0.113.13 sudo tar -C /var/lib -xzf -
   ```

   The copy holds the hub's private key and the join secret: do not keep
   it anywhere else.

   **Check:** the new server has the state:

   ```sh
   ssh deploy@203.0.113.13 sudo ls /var/lib/mmwatchdog
   ```

   ```text
   daemon.lock
   fleet.db
   measurement_history.db
   measurement_history_of_nodes.db
   state.db
   ```

5. **Name the new server as the hub.** In
   `inventories/hub_and_standalone.yml`, replace the old hub with the new
   one:

   ```yaml
   nodes:
     hub02:
       address: 203.0.113.13
       ssh_user: deploy
   ```

   In `files/hub_and_standalone/mmwatchdog_node_settings.yml`, set the new
   address in `address_for_nodes`, and keep the other settings:

   ```yaml
   fleet_hub_role:
     listen_on_address: 0.0.0.0:8445
     address_for_nodes: 203.0.113.13:8445               # the new hub's address
     report_every: 30s
     alert_after_hub_unreachable_for: 3m
     send_alert_without_hub_after_waiting_for: 30s
     undelivered_alerts_of_nodes_content_size_limit: 32MiB
     keep_measurement_history_of_nodes_for: 7d
     measurement_history_of_nodes_size_limit: 20GiB
   ```

   **Check:** `mmx validate` prints `Valid: mmwatchdog`.

6. **Preview the install of the new hub:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the dryrun lists the changes on `hub02`, and exits with code
   3, as for a first install.

7. **Install the new hub:**

   ```sh
   mmx run hub_and_standalone
   ```

   ```text
   Run 2496d798: succeeded
   1 finished
   Saved: /home/dev/monitoring/mmwatchdog-0.1.0-linux-amd64/.mmx/runs/20261115T094439.106346175-2496d798b467fb47
   ```

   The new hub sends no `rebooted` notice: MMWatchdog compares the
   machine ID in `/etc/machine-id` with the one saved in the copied state,
   sees that the state comes from another machine, and treats the start
   as a first start on this machine, not as a reboot. If both servers
   were made from one disk image and have the same `/etc/machine-id`, the
   new hub sends `Warning: hub02 rebooted` instead; no action is needed.

   **Check:** the last line of `ssh deploy@203.0.113.13 sudo mmwatchdog
   status` is
   `Fleet hub: nodes report to 203.0.113.13:8445; see mmwatchdog fleet`,
   and no `rebooted` message arrives.

8. **Put the new join code in the vault.** On the new hub:

   ```sh
   ssh deploy@203.0.113.13 sudo mmwatchdog fleet join-code
   ```

   ```text
   mmwatchdog-join 203.0.113.13:8445 ebba574e17670386674eeb04912fdd7555fb626c4f124addab10c6a5888168ee 6e86939f0a274f03001be8c90ba26997315840fe4b969cb161a531702db4a3b7
   ```

   Only the address is new: the certificate's fingerprint and the join
   secret came with the state. Put the code in the vault in place of the
   old one, straight from the new hub:

   ```sh
   ssh deploy@203.0.113.13 sudo mmwatchdog fleet join-code | mmx secrets set --replace mmwatchdog_join_code
   ```

   **Check:** `mmx secrets set` prints nothing.

9. **Give every node the new address:**

   ```sh
   mmx run join_hub --inventory hub_members
   ```

   ```text
   Run 0fb8b5c5: succeeded
   2 finished
   Saved: /home/dev/monitoring/mmwatchdog-0.1.0-linux-amd64/.mmx/runs/20261115T094454.067688493-0fb8b5c5b074a443
   ```

   Each node reports to the new hub with its credential; no node joins
   again.

   **Check:** on the new hub, `sudo mmwatchdog fleet` counts every node,
   and shows the windows of step 2:

   ```text
   Fleet at 203.0.113.13:8445: 3 nodes, 3 healthy
   Hub-outage alerter nodes: web01, web02
   Maintenance: web01 node_reports until 2026-11-15 10:14 UTC, turned on by deploy: moving the fleet hub
   Maintenance: web02 node_reports until 2026-11-15 10:14 UTC, turned on by deploy: moving the fleet hub
   ```

   The new hub lists itself as `hub02`; the old hub's name is gone from
   the fleet.

10. **End the windows of step 2:**

    ```sh
    ssh deploy@203.0.113.11 sudo mmwatchdog maintenance off node_reports
    ssh deploy@203.0.113.12 sudo mmwatchdog maintenance off node_reports
    ```

    ```text
    node_reports is out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.
      The MMWatchdog fleet hub received the maintenance change.
    ```

    **Check:** the last line of `sudo mmwatchdog status` on each node is
    `Fleet hub: 203.0.113.13:8445 as web01; last report ...`, with a recent
    time.

11. **Remove MMWatchdog from the old server,** or shut the server down.
    [Remove MMWatchdog from a node](remove-mmwatchdog.md) says how to remove
    MMWatchdog's files. Do not start MMWatchdog there again with the old
    state.

## If something is different

- **The node to add already reported to the hub before,** such as a
  server that you removed and now add again. It joins as a new node, and
  `sudo mmwatchdog fleet web02` shows the new join time. Its old events
  and readings are gone from the hub, if you removed it with
  `fleet forget`.
- **A node is replaced by a new server with the same host name.** Shut
  the old server down, and add the new one. While the old node is
  `reporting` or `waiting_for_first_report`, the hub refuses the new one
  with `node name "web02" is in use. A new node can use the name only after
  the fleet hub marks the current node as silent`, and `sudo mmwatchdog
  fleet` shows `Name conflict:`. Once the old node is silent, the new one
  joins in its place, and the silence alert of the old node closes with
  `web02 joined the fleet again as a new node; MMWatchdog does not know
  whether the problem is fixed`.
- **You add many nodes, or nodes of another job,** such as Git servers
  with checks of their own. Give them a configuration directory of their
  own, as [Set up a fleet](set-up-a-fleet.md#if-something-is-different)
  says.
- **The silence of a whole node group must wake someone,** such as when
  both servers of a background job stop. Add
  `raise_to_sos_when_every_node_of_these_node_groups_is_silent: [jobs_servers]`
  to the settings of those nodes, next to their
  `this_node_is_in_node_groups`. [Silent nodes](watch-a-fleet.md#if-something-is-different)
  gives the rules.
- **The join code was seen by someone, and a node may have joined with
  it.** Rotate the code, then compare the nodes that
  `sudo mmwatchdog fleet --json` lists with your inventory, and remove
  each node that you do not know. Then change the secrets of your alert
  destinations, because a joined node has them: change each one in its
  service, put it in the vault with `mmx secrets set mmwatchdog_secret_files NAME
  --replace`, and run
  `mmx run hub_and_standalone`.
- **The hub's address is a host name that stays the same,** such as
  `hub.example.com:8445` in `address_for_nodes`. Point the name at the new
  server before step 9. The join code holds the host name, so it does not
  change: skip step 8, and keep step 9 only if you changed other files.
- **The old hub's state is lost,** such as with a dead disk. Install the
  new hub as in [Set up a fleet](set-up-a-fleet.md), steps 4 to 10: it
  has a new identity and a new join code. Then run
  `mmx run join_hub --inventory hub_members`, and each node joins the new
  hub. Until then, the nodes refuse the new hub's certificate, send their
  alerts themselves, and the hub-outage alerter nodes alert. The readings
  and events that the old hub held are gone; each node still has its own.
- **The fleet was not set up with MMX.** The commands on the hub are the
  same. Write the new join code to `/etc/mmwatchdog/secrets/fleet-hub-join-code`
  on each node yourself, and run `sudo mmwatchdog reload` there.
  [Forgetting nodes, rotating the join code, and moving the hub](change-a-fleet.md)
  gives the rules.

## Related pages

- [Set up a fleet](set-up-a-fleet.md)
- [Watch a fleet](watch-a-fleet.md)
- [Pause alerts during planned work](pause-alerts-during-planned-work.md)
- [Remove MMWatchdog from a node](remove-mmwatchdog.md)
- [Update a fleet](update-mmwatchdog.md#update-a-fleet)
