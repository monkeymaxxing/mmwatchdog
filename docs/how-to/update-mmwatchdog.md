# Update MMWatchdog

**When to use this page:** a new release of MMWatchdog is out, and you want
a node that runs an earlier release to run the new one, with its checks,
alert destinations, secrets, and history kept. For a fleet, a hub and the
nodes that report to it, follow
[Update a fleet](#update-a-fleet).

**Who acts:** you, or your coding agent. Every step runs on your
**controller machine**: the machine where you run `mmx`, such as your
laptop. Steps 1, 9, and 10 also run commands on the node over `ssh`. Show the person the
output of step 7 and wait for their yes before step 8, because step 8
changes the node.

**What this page does:** you unpack the new release as a new MMX project,
copy your own files from your current project into it, and install the
new release on the node with MMX. MMX checks your configuration with the
new program before it changes anything. Then you prove that the node runs
the new release, kept its history, and still alerts you.

**Terms:** the **project** is the directory that you unpacked from a
release and installed MMWatchdog from, with `mmx_project_settings.yml` in
it. The **vault** is the encrypted file `secrets_vault.age` in the
project, where MMX keeps secrets such as your Slack address.

## Before you start

- **MMWatchdog runs on the node, installed with MMX** from your current
  project. If not, follow [Install MMWatchdog on one node](install-on-one-node.md).
- **You know your vault passphrase,** if the project has a vault. The new
  project uses the same vault, and MMX asks for the passphrase in step 5.
- **You read the new release's notes,** on the release's GitHub page. While
  MMWatchdog's version starts with `0.`, a minor release, such as 0.2.0
  after 0.1.1, may need a change to your configuration. Its notes list
  each such change, and what to do, under "Changes you must act on". A
  patch release, such as 0.1.1 after 0.1.0, only fixes faults. The
  example on this page updates a node from 0.1.0 to 0.1.1.

## Steps

1. **Note the release that the node runs now:**

   ```sh
   ssh deploy@203.0.113.11 sudo mmwatchdog version
   ```

   ```text
   mmwatchdog 0.1.0 (ad75e5909a0af9b03215d2b2ac4c5bdfa9a56777, linux/amd64, modified=false)
   ```

   **Check:** the output names the earlier release and the node's
   processor, here `linux/amd64`. Reason: step 9 compares the version, and
   step 2 downloads the archive for this processor.

2. **Download the new release** into the directory that holds your current
   project, beside the project, not into it. Choose `linux-amd64` or
   `linux-arm64` as the processor in step 1 says:

   ```sh
   version=0.1.1
   platform=linux-amd64      # linux-arm64 for ARM64 nodes
   release=https://github.com/monkeymaxxing/mmwatchdog/releases/download/v$version

   curl -fLO "$release/mmwatchdog-$version-$platform.tar.gz"
   curl -fLO "$release/SHA256SUMS"
   ```

   If an earlier `SHA256SUMS` is in the directory, the second `curl`
   replaces it, which is right: the file must come from the new release.

   **Check:** both files are in the current directory.

3. **Check the download:**

   ```sh
   shasum -a 256 --check --ignore-missing SHA256SUMS      # on macOS
   sha256sum --check --ignore-missing SHA256SUMS          # on Linux
   ```

   ```text
   mmwatchdog-0.1.1-linux-amd64.tar.gz: OK
   ```

   **Check:** the new archive's line ends with `OK`. If not, download it
   again; do not unpack an archive that fails the check.

4. **Unpack the archive, and go into it:**

   ```sh
   tar -xzf "mmwatchdog-$version-$platform.tar.gz"
   cd "mmwatchdog-$version-$platform"
   ```

   This directory is your new project. It holds the new release's parts:
   the program `mmwatchdog`, the systemd unit `mmwatchdog.service`, the
   playbooks in `playbooks/`, and the official extra modules in
   `official_extra_modules/`, the modules such as `slack` that ship with
   MMWatchdog but are not built into the program. It also holds starter
   files, which step 5 replaces with yours, and the release's source code,
   documentation, and tests, which the install does not use.

   **Check:** `ls` lists `mmx_project_settings.yml` and `mmwatchdog`.

5. **Copy your own files from the current project.** The commands below
   delete the starter directories in the current directory, so first make
   sure that the current directory is the new project:

   ```sh
   pwd
   ```

   ```text
   /home/dev/monitoring/mmwatchdog-0.1.1-linux-amd64
   ```

   **Check:** the path ends with the new release's directory, here
   `mmwatchdog-0.1.1-linux-amd64`. If it names your current project, stop:
   the `rm -rf` below would delete your own files. Go into the new project
   as step 4 says.

   Set `old` to the full path of your current project, here the directory
   of the earlier release beside the new one, and check that it holds your
   project and its vault:

   ```sh
   old=/home/dev/monitoring/mmwatchdog-0.1.0-linux-amd64
   ls "$old/mmx_project_settings.yml" "$old/secrets_vault.age"
   ```

   ```text
   /home/dev/monitoring/mmwatchdog-0.1.0-linux-amd64/mmx_project_settings.yml
   /home/dev/monitoring/mmwatchdog-0.1.0-linux-amd64/secrets_vault.age
   ```

   **Check:** `ls` lists both files. A project without secrets has no
   `secrets_vault.age`: then `ls` reports only that file missing, and you
   leave it out of the second `cp` below.

   Then replace the starter files with yours:

   ```sh
   rm -rf inventories files mmwatchdog_external_modules
   cp -R "$old/inventories" "$old/files" "$old/mmwatchdog_external_modules" .
   cp "$old/mmx_project_settings.yml" "$old/secrets_vault.age" .
   cp -R "$old/.mmx" .
   ```

   These are your files, and nothing else in the project is:

   | What you copy | What it holds |
   |---|---|
   | `inventories/` | The node's address and account, and the list of secret files |
   | `files/` | Your check groups, alert destinations, and node settings |
   | `mmwatchdog_external_modules/` | The modules of your own, if you wrote any |
   | `mmx_project_settings.yml` | The project's name and default inventory |
   | `secrets_vault.age` | The vault, with your secrets |
   | `.mmx/` | MMX's record of each earlier run |

   **If your vault has another name or place:** a `vault_file` line in
   `mmx_project_settings.yml` or in an inventory names the vault instead of
   `secrets_vault.age`, as MMX's
   [Which vault file](https://github.com/monkeymaxxing/mmx/blob/main/docs/reference/secrets.md#which-vault-file)
   says. A relative path starts at the file that holds the line, so the
   copied files look for the vault at the same place in the new project:
   copy the vault there, unless `inventories/` already brought it. A path
   that starts with `/` needs no copy; the new project uses the same file.
   `.mmx/` is always in the project directory; MMX has no setting that
   moves it.

   Do not copy the earlier `mmwatchdog`, `mmwatchdog.service`,
   `playbooks/`, or `official_extra_modules/`. Reason: each official extra
   module must come from the same release as the program, and the new
   playbooks install the new release correctly.

   **Check:** the project is valid, and the vault holds your secrets:

   ```sh
   mmx validate
   mmx secrets list-keys
   ```

   ```text
   Valid: mmwatchdog
   mmwatchdog_secret_files
   ```

   `mmx secrets list-keys` prints nothing for a project without secrets.
   If `mmx validate` names a value that is not declared, see
   [If something is different](#if-something-is-different).

6. **Make the changes that the release notes list** under "Changes you
   must act on", such as a setting that the new release names differently.
   Edit your files in `files/` and `inventories/` as the notes say. The
   section says "none" when there are none, as for a patch release such as
   0.1.1: then you change nothing in this step.

   **Check:** `mmx validate` prints `Valid: mmwatchdog`.

7. **Preview what the update changes:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   The output lists each step of the playbook on the node. The program is
   among the changes:

   ```text
     install / node / install_monitoring/changes/install_binary: would update "/usr/local/bin/mmwatchdog"
       17346720 bytes, permissions_mode 0755, uid 0, gid 0 -> 17346720 bytes, permissions_mode 0755, uid 0, gid 0
     install / node / install_monitoring/changes/install_unit: succeeded, unchanged
     install / node / install_monitoring/changes/install_modules: succeeded, unchanged
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked input /usr/local/bin/mmwatchdog would change
   ```

   The size of the program can stay the same, as here: `would update`
   means that its content differs. The line `install_modules` may say
   `unchanged` even when the new release changes a module: the dryrun
   compares only the copy that the last run left on the node, and the run
   in step 8 installs the changed modules.

   **Check:** the dryrun connected to the node, and its exit code is 2:
   something would change. Your check groups and destinations show no
   change, such as `0 entries updated; 6 unchanged` for `/etc/mmwatchdog`,
   unless step 6 changed them. Exit code 1, or a line that says `failed`,
   is a fault; see [If something is different](#if-something-is-different).

8. **Install the new release:**

   ```sh
   mmx run hub_and_standalone
   ```

   ```text
   Run 3c3e948e: succeeded
   1 finished
   Saved: /home/dev/monitoring/mmwatchdog-0.1.1-linux-amd64/.mmx/runs/20261115T092543.392792321-3c3e948ee71c49ad
   ```

   The run first checks your whole configuration with the new program on
   the node. If the check fails, the run stops, and the node keeps its
   earlier release, configuration, and modules, and goes on running. Then
   the run installs the new program, systemd unit, and modules, opens a
   maintenance window of 15 minutes for every check group, restarts
   MMWatchdog, and ends the window. Reason: the restart makes each check
   wait for a new run, and the window keeps the restart from sending
   alerts. Your destinations get two `Info` messages, `maintenance on` and
   `maintenance off`.

   The update keeps what the node has stored: the events, the measurement
   history, open problems, maintenance windows, and restart attempts in
   `/var/lib/mmwatchdog/`, and the secret files in
   `/etc/mmwatchdog/secrets/`.

   **Check:** the run ends with `succeeded`.

9. **Confirm that the node runs the new release,** with its history. On
   the node:

   ```sh
   ssh deploy@203.0.113.11 sudo mmwatchdog version
   ssh deploy@203.0.113.11 sudo mmwatchdog status
   ssh deploy@203.0.113.11 sudo mmwatchdog events
   ```

   ```text
   mmwatchdog 0.1.1 (ad75e5909a0af9b03215d2b2ac4c5bdfa9a56777, linux/amd64, modified=false)
   ```

   ```text
   machine/cpu_use_is_recorded  ok           cpu_used_percent is 0.32 over 29.982s
   machine/memory_is_below_90_percent ok           memory_used_percent is 20.26 (fails above 90)
   machine/root_filesystem_is_below_85_percent ok           space_used_percent is 9.21 (fails above 85)
   machine/root_filesystem_is_below_95_percent ok           space_used_percent is 9.21 (fails above 95)
   myapp/health_page_answers    ok           HTTP 200 and configured content match
   myapp/service_is_running     ok           systemd unit myapp.service is active
   Alert deliveries: 0 waiting, 0 dropped
   ```

   ```text
   2026-11-15T09:25:46Z maintenance_off      every_check_group maintenance turned off by root: mmwatchdog deploy
   2026-11-15T09:25:45Z maintenance_on       every_check_group maintenance until 2026-11-15 09:40 UTC, turned on by root: mmwatchdog deploy
   2026-11-15T09:25:02Z maintenance_off      every_check_group maintenance turned off by root: mmwatchdog deploy
   2026-11-15T09:25:00Z maintenance_on       every_check_group maintenance until 2026-11-15 09:40 UTC, turned on by root: mmwatchdog deploy
   2026-11-15T09:24:44Z recovered            alert_path_test/shell command exited 0
   2026-11-15T09:24:34Z failed               alert_path_test/shell command exited 1
   ...
   ```

   Here the node runs the check group `myapp` of
   [Add a check](add-a-check.md) besides the starter check group
   `machine`, and the events below the update's two lines come from the
   earlier release.

   **Check:** `version` shows the new release; `status` lists every check
   that it listed before the update, with no line that says that a
   destination fails; and `events` still lists the events from before the
   update, below the update's `maintenance_on`. A check that shows
   `unknown` with `waiting for a new run` has not run since the restart:
   wait for its `check_every` and run `status` again.

10. **Prove that an alert still reaches you.** The update installed a new
    program and the new release's modules that deliver alerts, such as
    `slack`, so prove the alert path again. Add the test check group from
    [Install MMWatchdog on one node](install-on-one-node.md), step 11:

    ```yaml
    # files/hub_and_standalone/check_groups/alert_path_test.yml
    # The check fails while the file /var/tmp/mmwatchdog-alert-test exists.
    check_groups:
      alert_path_test:
        check_every: 5s
        time_limit_per_run: 2s
        alert_after_failing_for: 0s         # alert at the first failed run
        recovered_after_healthy_for: 0s     # the failure is over at the first healthy run
        shell: test ! -e /var/tmp/mmwatchdog-alert-test
        alert_severity: warn
    ```

    Install it, then make it fail, and after the alert arrives, recover:

    ```sh
    mmx run hub_and_standalone
    ssh deploy@203.0.113.11 touch /var/tmp/mmwatchdog-alert-test
    # wait for the failure message in Slack
    ssh deploy@203.0.113.11 rm /var/tmp/mmwatchdog-alert-test
    # wait for the recovery message
    ```

    Slack shows two messages:

    ```text
    Warning: app01: alert_path_test/shell failed
    command exited 1
    failing since 09:26 UTC
    exit code: 1
    ```

    ```text
    Resolved warning: app01: alert_path_test/shell is ok again
    command exited 0
    ```

    Then delete `files/hub_and_standalone/check_groups/alert_path_test.yml`
    and run `mmx run hub_and_standalone` again, which removes the test check
    group from the node.

    **Check:** both messages arrived. If not, see the status line of the
    destination, as in
    [Install MMWatchdog on one node](install-on-one-node.md#if-something-is-different).

11. **Put the earlier project away.** The new project is now the one that
    you change the node from. Keep the earlier project until you are sure
    of the new release, then delete it or move it to an archive. Never run
    `mmx` from the earlier project: its `mmx run` installs the earlier
    release on the node again, and its dryrun shows that it would replace
    the program:

    ```text
      install / node / install_monitoring/changes/install_binary: would update "/usr/local/bin/mmwatchdog"
        17346720 bytes, permissions_mode 0755, uid 0, gid 0 -> 17346720 bytes, permissions_mode 0755, uid 0, gid 0
    ```

    **Check:** `mmx dryrun hub_and_standalone` in the new project exits with
    code 0: the node matches the new project.

## Update a fleet

A fleet is a hub and the joined nodes that report to it, installed from
one project as [Set up a fleet](set-up-a-fleet.md) says. You make the new
project once, then update the hub first and the joined nodes after it.
Reason for the order: a hub accepts the node status reports of nodes that
run its own release or a release one minor version older, such as 0.1.x
nodes on a 0.2.0 hub. A node that runs a newer release than its hub may
send reports that the hub refuses. The examples use the hub `hub01` at
203.0.113.10 and the joined nodes `web01` and `web02`, and update them
from 0.1.0 to 0.1.1. A coding agent shows the person the output of
steps 3 and 5, and waits for their yes before steps 4 and 6, because those
steps change the nodes.

1. **Note the release that each node runs now,** from the hub:

   ```sh
   ssh deploy@203.0.113.10 sudo mmwatchdog fleet --json | grep -E '"(name|state|last_report_received_at|version)"'
   ```

   ```text
         "name": "hub01",
         "state": "reporting",
         "last_report_received_at": "2026-11-15T09:59:18.608357303Z",
             "version": "0.1.0"
         "name": "web01",
         "state": "reporting",
         "last_report_received_at": "2026-11-15T09:59:01.225496936Z",
             "version": "0.1.0"
         "name": "web02",
         "state": "reporting",
         "last_report_received_at": "2026-11-15T09:59:15.65660063Z",
             "version": "0.1.0"
   ```

   **Check:** every node of the fleet is listed, `reporting`, with the
   earlier release. A node that is not `reporting` has a problem of its
   own: fix it first, as [Watch a fleet](watch-a-fleet.md) says, so that
   step 7 shows what the update did.

2. **Make the new project,** with steps 2 to 6 of
   [Steps](#steps) above: download the new release beside your current
   project, check the download, unpack it, copy your own files into it,
   and make the changes that the release notes list. Your files are those
   of the whole fleet: both inventories, `files/hub_and_standalone/`,
   `files/join_hub/`, and the vault with `mmwatchdog_join_code`.

   **Check:** in the new project, both playbooks are valid, and the vault
   holds both secrets:

   ```sh
   mmx validate
   mmx validate join_hub --inventory hub_members
   mmx secrets list-keys
   ```

   ```text
   Valid: mmwatchdog
   Valid: mmwatchdog
   mmwatchdog_join_code
   mmwatchdog_secret_files
   ```

3. **Preview the update of the hub:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   ```text
     install / hub01 / install_monitoring/changes/install_binary: would update "/usr/local/bin/mmwatchdog"
       17346720 bytes, permissions_mode 0755, uid 0, gid 0 -> 17346720 bytes, permissions_mode 0755, uid 0, gid 0
   ```

   **Check:** the exit code is 2, the program is among the changes, and
   `/etc/mmwatchdog` shows no change, such as
   `0 entries updated; 5 unchanged`, unless step 2 changed your files.
   Exit code 1, or a line that says `failed`, is a fault; see
   [If something is different](#if-something-is-different).

4. **Update the hub:**

   ```sh
   mmx run hub_and_standalone
   ```

   ```text
   Run 903a5533: succeeded
   1 finished
   Saved: /home/dev/monitoring/mmwatchdog-0.1.1-linux-amd64/.mmx/runs/20261115T095935.392735186-903a5533d81e81c3
   ```

   The run restarts MMWatchdog on the hub inside a maintenance window, as
   step 8 above says for one node. While the hub restarts, the joined
   nodes keep running their checks, and report again once the hub
   answers.

   **Check:** the hub runs the new release, and still sees every node:

   ```sh
   ssh deploy@203.0.113.10 sudo mmwatchdog version
   ssh deploy@203.0.113.10 sudo mmwatchdog fleet
   ```

   ```text
   mmwatchdog 0.1.1 (ad75e5909a0af9b03215d2b2ac4c5bdfa9a56777, linux/amd64, modified=false)
   Fleet at 203.0.113.10:8445: 3 nodes, 3 healthy
   Hub-outage alerter nodes: web01, web02
   ```

   Right after the restart, the hub may list itself with checks that say
   `unknown: waiting for a new run`: run `fleet` again after 30 seconds.
   Do not go on to the joined nodes until the hub runs the new release.

5. **Preview the update of the joined nodes:**

   ```sh
   mmx dryrun join_hub --inventory hub_members
   ```

   ```text
     install / web01 / install_monitoring_on_a_joined_node/changes/install_binary: would update "/usr/local/bin/mmwatchdog"
       17346720 bytes, permissions_mode 0755, uid 0, gid 0 -> 17346720 bytes, permissions_mode 0755, uid 0, gid 0
   ```

   **Check:** the exit code is 2, and the program is among the changes of
   each node, here `web01` and `web02`.

6. **Update the joined nodes:**

   ```sh
   mmx run join_hub --inventory hub_members
   ```

   ```text
   Run 85139d24: succeeded
   2 finished
   Saved: /home/dev/monitoring/mmwatchdog-0.1.1-linux-amd64/.mmx/runs/20261115T100026.798841457-85139d246967f10f
   ```

   **Check:** the run ends with `succeeded`, and counts every joined node
   as `finished`.

7. **Confirm that every node runs the new release and reports.** A
   minute after step 6, from the hub:

   ```sh
   ssh deploy@203.0.113.10 sudo mmwatchdog fleet
   ssh deploy@203.0.113.10 sudo mmwatchdog fleet --json | grep -E '"(name|state|last_report_received_at|version)"'
   ```

   ```text
   Fleet at 203.0.113.10:8445: 3 nodes, 3 healthy
   Hub-outage alerter nodes: web01, web02
   ```

   ```text
         "name": "hub01",
         "state": "reporting",
         "last_report_received_at": "2026-11-15T10:01:07.681738669Z",
             "version": "0.1.1"
         "name": "web01",
         "state": "reporting",
         "last_report_received_at": "2026-11-15T10:01:16.934275314Z",
             "version": "0.1.1"
         "name": "web02",
         "state": "reporting",
         "last_report_received_at": "2026-11-15T10:01:00.721501859Z",
             "version": "0.1.1"
   ```

   **Check:** every node is `reporting`, with the new release in
   `version`, and with a `last_report_received_at` after step 6. The
   `version` comes from each node's latest report, so it names the
   release that the node runs. A node with the earlier release did not
   restart: the last line of `sudo mmwatchdog status` on that node gives
   the latest error.

8. **Prove that an alert of a joined node still reaches you through the
   hub.** The update installed a new program and new destination modules
   on every node. Do step 15 of [Set up a fleet](set-up-a-fleet.md#steps)
   in the new project: add the test check group to
   `files/join_hub/check_groups/`, run
   `mmx run join_hub --inventory hub_members`, make the check fail on
   `web01` and recover, then delete the test check group and run the
   playbook again.

   Slack shows two messages, sent by the hub:

   ```text
   Warning: web01: alert_path_test/shell failed
   command exited 1
   failing since 10:02 UTC
   exit code: 1
   ```

   ```text
   Resolved warning: web01: alert_path_test/shell is ok again
   command exited 0
   ```

   Each run restarts MMWatchdog inside maintenance windows, so the
   restarts of steps 4, 6, and 8 send no check alerts. When no node has
   been in a window for 10 minutes, about 10 minutes after the last run,
   the hub sends one message for all the windows:

   ```text
   Info: maintenance off for 3 nodes
   maintenance turned off on 3 nodes: web01, web02, hub01 (by root: mmwatchdog deploy)
   maintenance ended on the last node in the alert at 10:03 UTC. Alerts on each node resumed when the node's maintenance ended. ...
   ```

   **Check:** the failure and the recovery message arrived.

9. **Put the earlier project away,** as step 11 above says. Never run
   `mmx` from the earlier project: its runs would install the earlier
   release on the hub and the nodes again.

   **Check:** in the new project, `mmx dryrun hub_and_standalone` and
   `mmx dryrun join_hub --inventory hub_members` each exit with code 0:
   every node matches the new project.

## If something is different

- **`mmx validate` or the dryrun stops with `reference
  .vars.mmwatchdog_secret_files is not declared`,** such as:

  ```text
  mmx: playbooks/hub_and_standalone.yml:29:11 (fleet_stages[1].when): reference .vars.mmwatchdog_secret_files is not declared for node node: declare it under vars in mmx_project_settings.yml or in inventories/hub_and_standalone.yml (the inventory, a group, or the node), or in command_line_vars of the playbook. set_vars cannot set it here, because a fleet stage's when is read before the fleet stage's node tasks run
  ```

  The playbooks need the list of secret files, and your inventory does not
  declare it. Add it under `vars` in `inventories/hub_and_standalone.yml`,
  empty if the vault holds no secret files:

  ```yaml
  vars:
    mmwatchdog_config_source_dir: files/hub_and_standalone
    mmwatchdog_secret_files: []
  ```

  If you put secret files on the node in another way, such as by hand, you
  can move them into the vault, as step 6 of
  [Install MMWatchdog on one node](install-on-one-node.md) says. The dryrun
  and the run exit with code 4 for this fault, before they connect to the
  node, so nothing changed on it.
- **The dryrun stops with `read vault: stat
  .../secrets_vault.age: no such file or directory`.** The vault was not
  copied in step 5. Copy `secrets_vault.age` from the earlier project.
- **Your project is in Git, or must keep its directory.** Update the
  project in place instead of steps 4 and 5: unpack the new archive
  elsewhere, go into your project, and copy the new release's four parts
  over the earlier ones:

  ```sh
  new=../mmwatchdog-0.1.1-linux-amd64      # the unpacked new release

  rm -rf playbooks official_extra_modules
  cp -R "$new/playbooks" "$new/official_extra_modules" .
  cp "$new/mmwatchdog" "$new/mmwatchdog.service" .
  ```

  Then continue with step 6. Step 11 then has no earlier project to put
  away.
- **You changed the playbooks yourself,** such as to add a task. Step 5
  takes the new release's playbooks. Make your change again in the new
  `playbooks/`; `diff -r "$old/playbooks" playbooks` shows both your change
  and the release's own changes.
- **The run fails at `validate_config`.** The new program does not accept
  part of your configuration. The output names the file, the line, and the
  fault. Fix the file as the release notes say, and run step 8 again.
  Nothing changed on the node: it still runs the earlier release.
- **You must go back to the earlier release,** for example because of a
  fault in the new one. Run `mmx run hub_and_standalone` in the earlier
  project. The node then runs the earlier release again with the history
  that it has; going back from 0.1.1 to 0.1.0 kept every event. The
  release notes say when a release stores its history in a way that an
  earlier release cannot read.
- **The node is ARM64, or you have nodes of both kinds.** Each project
  serves nodes of one processor kind. Update each project with the archive
  of its kind.
- **The node is part of a fleet.** Update the hub first, then the nodes
  that report to it, as [Update a fleet](#update-a-fleet) says.
- **The hub does not answer while a joined node is updated,** such as
  while the hub restarts. The node's `maintenance on` then says that `the
  hub does not know about the maintenance window`, and the deploy goes on.
  The node's own window still holds its check alerts, and the restart is
  short, so the node reports again long before the hub counts it as silent.

## Related pages

- [Install MMWatchdog on one node](install-on-one-node.md)
- [See what is happening](see-what-is-happening.md)
- [Remove MMWatchdog from a node](remove-mmwatchdog.md)
- [Files that an install puts on a node](../reference/installed-files.md)
