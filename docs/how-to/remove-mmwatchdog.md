# Remove MMWatchdog from a node

**When to use this page:** MMWatchdog should no longer watch a node, and
its program, files, and account should go from the node. To stop
MMWatchdog for a while instead, follow
[Stop MMWatchdog for a while](pause-alerts-during-planned-work.md#stop-mmwatchdog-for-a-while).

**Who acts:** you, or your coding agent. Steps 2 and 4 to 7 run on the
node, logged in with an account that can use `sudo`. Steps 1, 3, 8, and 9
run on your **controller machine**, the machine where you run `mmx`. Step 3 saves the
node's history into the MMX project, and step 5 deletes it from the node.
A coding agent always does step 3, unless the person said that the
history may go: a saved copy costs only disk space on the controller
machine, and a deleted history cannot come back.

**What this page does:** the MMX project that installed MMWatchdog has no
playbook that removes it, so you remove it with commands on the node. You
stop the service, save the node's history, remove every file that the
install made, and remove the account `mmwatchdog`. Then you prove that
nothing of MMWatchdog is left, and take the node out of the project so
that no later run installs MMWatchdog again.

## Choose the route

| The node | Route |
|---|---|
| A **standalone node**: it sends its own alerts, with no fleet hub | [Steps](#steps) |
| A **joined node**: it reports to a fleet hub | [Remove a node](change-a-fleet.md#remove-a-node) on the page Change a fleet. It sends you to steps 1 to 8 below, then takes the node out of the inventory and the hub |
| The **fleet hub** | First move the hub to another node, as [Move the hub to another server](change-a-fleet.md#move-the-hub-to-another-server) says, then [Steps](#steps) |

## Before you start

- **Find out which kind of node it is.** On the node:

  ```sh
  sudo grep -E '^(join_fleet_hub|fleet_hub_role):' /etc/mmwatchdog/mmwatchdog_node_settings.yml
  ```

  The command prints nothing on a standalone node, `join_fleet_hub:` on a
  joined node, and `fleet_hub_role:` on the hub.
- **Check whether the node uses other paths than the defaults.** The steps
  remove the default paths. On the node:

  ```sh
  sudo grep -E '^(state_dir|module_state_dir|socket):' /etc/mmwatchdog/mmwatchdog_node_settings.yml
  ```

  The command prints nothing when the node uses the defaults. Otherwise,
  remove the paths that it prints in place of the defaults in step 5.
- **Tell MMHotline, if the node sends alerts to it.** A standalone node
  with an `mmhotline` destination sends MMHotline a check-in every 30
  seconds, and MMHotline alerts when the check-ins stop. Before step 1,
  take the node's alert source out of `alert_sources` in MMHotline's
  settings file and run `sudo mmhotline settings apply` on the MMHotline
  server, as
  [Connect a tool that sends alerts](https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/connect-a-tool-that-sends-alerts.md)
  says. From the apply on, MMHotline no longer expects check-ins from the
  node. Done the other way round, the silence between the two steps raises
  a missed check-in warning, then an SOS. `sudo mmhotline alert-sources
  list` then says the source is disabled. An incident that was already
  open stays open: close it as MMHotline's
  [Handle an incident](https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/handle-an-incident.md)
  says.

## Steps

1. **Log in to the node:**

   ```sh
   ssh deploy@203.0.113.11
   ```

   **Check:** the prompt shows the node's host name. Steps 2 and 4 to 7
   run in this login.

2. **Stop MMWatchdog, and keep it from starting at boot:**

   ```sh
   sudo systemctl disable --now mmwatchdog
   ```

   ```text
   Removed '/etc/systemd/system/multi-user.target.wants/mmwatchdog.service'.
   ```

   From now on, MMWatchdog runs no check and sends no alert. It sends no
   message when it stops.

   **Check:** `systemctl is-active mmwatchdog` prints `inactive`.

3. **Save the node's history.** `/var/lib/mmwatchdog/` holds the only copy
   of the node's events and measurement history, and step 5 deletes it. On
   the controller machine, in another terminal, go into the MMX project
   that installed MMWatchdog, and copy the directory into one archive
   there:

   ```sh
   ssh deploy@203.0.113.11 sudo tar -C / -czf - var/lib/mmwatchdog > mmwatchdog-state-app01.tar.gz
   tar -tzf mmwatchdog-state-app01.tar.gz
   ```

   ```text
   var/lib/mmwatchdog/
   var/lib/mmwatchdog/measurement_history.db
   var/lib/mmwatchdog/daemon.lock
   var/lib/mmwatchdog/state.db
   ```

   Reason for stopping first: a stopped MMWatchdog has written everything
   into the two database files, so the copy is complete. To use the
   history again, put the directory back on a node before you install
   MMWatchdog there:
   `ssh deploy@203.0.113.11 sudo tar -C / -xzf - < mmwatchdog-state-app01.tar.gz`.
   The install then keeps it, and `sudo mmwatchdog events` lists the
   earlier events.

   **Check:** the list shows `state.db` and `measurement_history.db`.
   Skip this step only when the person said that the history may go.

4. **Remove the program and the systemd unit:**

   ```sh
   sudo rm -f -- /etc/systemd/system/mmwatchdog.service /usr/local/bin/mmwatchdog
   sudo systemctl daemon-reload
   ```

   Reason for `daemon-reload`: systemd forgets the unit only when it
   reads its unit files again.

   **Check:** `systemctl status mmwatchdog` prints
   `Unit mmwatchdog.service could not be found.`

5. **Remove MMWatchdog's directories:**

   ```sh
   sudo rm -rf -- /usr/local/lib/mmwatchdog /etc/mmwatchdog
   sudo rm -rf -- /var/lib/mmwatchdog /var/lib/mmwatchdog-module-state /var/lib/mmwatchdog-checks
   ```

   | Directory | What goes with it |
   |---|---|
   | `/usr/local/lib/mmwatchdog/` | The modules that are not built in, and the copies that each install checks before it installs them |
   | `/etc/mmwatchdog/` | The configuration, and the secret files in `secrets/`, such as the Slack address |
   | `/var/lib/mmwatchdog/` | The events, the measurement history, and open problems |
   | `/var/lib/mmwatchdog-module-state/` | What checks of modules that are not built in keep from one run to the next. It exists only on a node with such a check |
   | `/var/lib/mmwatchdog-checks/` | The home of the account `mmwatchdog` |

   The secret files are also in the vault of your MMX project, so you
   need no copy of them. A secret file that you put on the node by hand is
   gone after this step: copy it first if you need it.
   [Files that an install puts on a node](../reference/installed-files.md)
   lists every file.

   **Check:** none here; step 7 checks every path.

6. **Remove the account and the group `mmwatchdog`:**

   ```sh
   sudo userdel mmwatchdog
   getent group mmwatchdog && sudo groupdel mmwatchdog
   ```

   On many distributions, such as Debian, `userdel` also removes the group,
   and `getent` then prints nothing, so `groupdel` does not run.

   **Check:** `getent passwd mmwatchdog` and `getent group mmwatchdog`
   print nothing.

7. **Prove that nothing of MMWatchdog is left:**

   ```sh
   systemctl list-unit-files 'mmwatchdog*'
   ls -d /usr/local/bin/mmwatchdog /usr/local/lib/mmwatchdog /etc/mmwatchdog /var/lib/mmwatchdog /var/lib/mmwatchdog-module-state /var/lib/mmwatchdog-checks /run/mmwatchdog
   getent passwd mmwatchdog; getent group mmwatchdog
   pgrep -a mmwatchdog
   ```

   ```text
   UNIT FILE STATE PRESET

   0 unit files listed.
   ls: cannot access '/usr/local/bin/mmwatchdog': No such file or directory
   ls: cannot access '/usr/local/lib/mmwatchdog': No such file or directory
   ls: cannot access '/etc/mmwatchdog': No such file or directory
   ls: cannot access '/var/lib/mmwatchdog': No such file or directory
   ls: cannot access '/var/lib/mmwatchdog-module-state': No such file or directory
   ls: cannot access '/var/lib/mmwatchdog-checks': No such file or directory
   ls: cannot access '/run/mmwatchdog': No such file or directory
   ```

   **Check:** no unit file, `No such file or directory` for each path,
   and nothing from `getent` or `pgrep`. systemd removed `/run/mmwatchdog/`
   itself in step 2. If a path is still there, remove it as in steps 4 to
   6. Log out of the node.

8. **Remove MMX from the node too, if MMX should no longer manage it.**
   MMX keeps its own program, service, and records on each node that it
   manages, and a record of the project's runs in `/var/lib/mmx/`. If
   MMX still manages the node for other work, skip this step. Otherwise,
   in the MMX project that installed MMWatchdog, on the controller
   machine:

   ```sh
   mmx remove-mmx-from-node node
   ```

   ```text
   node: stopped and disabled mmx-https-api.service
   node: removed /etc/systemd/system/mmx-https-api.service
   node: removed /opt/mmx
   node: removed /var/lib/mmx
   node: removed /run/mmx
   controller machine: removed .../.mmx/nodes/757251cdfcf6b7b5252ec4049deab203d849e183fbbdc9aa7f80596525d4983d.json
   MMX did not change the inventory or the playbooks. Take node out of the inventory, its groups, and the playbooks; until then, a run that selects node sets MMX up on it again.
   ```

   `node` is the node's name in `inventories/hub_and_standalone.yml`. A
   joined node is in `inventories/hub_members.yml` instead, so name that
   inventory, such as
   `mmx remove-mmx-from-node web02 --inventory hub_members`. MMX finds the
   node only while it is still in the inventory, so do this step before
   step 9. MMX's [Remove MMX from a server](https://github.com/monkeymaxxing/mmx/blob/main/docs/how-to/keep-mmx-off-a-server.md#remove-mmx-from-a-server)
   says more.

   **Check:** the output lists each removed path, as above. For a joined
   node, go back now to step 2 of
   [Remove a node](change-a-fleet.md#remove-a-node), which takes the node
   out of the inventory and the hub in place of step 9.

9. **Take the node out of the MMX project.** Reason: the next
   `mmx run hub_and_standalone` would install MMWatchdog on every node of
   the inventory again.

   - **The project serves other nodes too.** Delete the node's entry under
     `nodes:` in `inventories/hub_and_standalone.yml`.
   - **The project served only this node.** Keep the project as it is, as
     a record of the configuration, or delete the project directory. Do
     not delete only the node's entry: an inventory needs at least one
     node, and `mmx validate` then fails with
     `inventory requires at least one node`.

   **Check:** for a project that serves other nodes,
   `mmx nodes hub_and_standalone` no longer lists the node:

   ```text
   Playbook: hub_and_standalone   Inventory: hub_and_standalone (inventories/hub_and_standalone.yml, 1 node)

   Nodes
     web02   203.0.113.12   deploy
   ...
   ```

   Here the node `node` is gone, and the node `web02` stays.

## What stays

- **On the controller machine:** your MMX project, with its
  configuration, the vault with your secrets, and MMX's record of each run
  in `.mmx/`. They are yours to keep or delete.
- **On the node:** the lines that MMWatchdog wrote to the system journal,
  which `journalctl -u mmwatchdog` shows until the journal deletes old
  lines by its own rules. MMX's own files, unless you did step 8. A
  secret file that another program uses, outside `/etc/mmwatchdog/`: step
  5 removes only MMWatchdog's own directories.
- **In the MMX project,** the archive of step 3,
  `mmwatchdog-state-app01.tar.gz`, if you made it: the node's events and
  measurement history.

## If something is different

- **A `sudo` command asks for a password.** The account can use `sudo`
  with a password: type it. Log in as root instead to run the commands
  without `sudo`.
- **You want to keep MMWatchdog installed but not running.** Do step 2
  only. `sudo systemctl enable --now mmwatchdog` starts it again.
- **A later `mmx run` installed MMWatchdog again.** The node was still in
  the inventory. Do step 9, then steps 1 to 7 again.

## Related pages

- [Files that an install puts on a node](../reference/installed-files.md)
- [Install MMWatchdog on one node](install-on-one-node.md)
- [Pause alerts during planned work](pause-alerts-during-planned-work.md)
- [Update MMWatchdog](update-mmwatchdog.md)
