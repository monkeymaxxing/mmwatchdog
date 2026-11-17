# Set up a fleet

**When to use this page:** you have several Linux servers, and you want
one of them, the **fleet hub**, to collect the state of all of them and to
send the alerts of all of them, so that you see every server in one place.
If MMWatchdog already runs on one of the servers, that server can become
the hub: see [If something is different](#if-something-is-different).

**Who acts:** you, or your coding agent. Each step runs on your
**controller machine**: the machine where you run `mmx`, such as your
laptop. Some checks run a command on a node through `ssh`. A coding agent
shows the person the output of each `mmx dryrun` and waits for their yes
before the `mmx run` that follows it, and asks the person to put each
secret into the vault themselves, so that the secret never passes through
the chat. The person runs `mmx secrets write-passphrase-file` once, in step
5, and the agent then runs `mmx dryrun` and `mmx run` itself.

**What this page does:** [MMX](https://github.com/monkeymaxxing/mmx), the
Monkeymaxxing tool that sets up servers, installs MMWatchdog on one node as
the fleet hub, then installs MMWatchdog on the other nodes and joins them
to the hub. Then you prove that an alert of a joined node reaches Slack
through the hub, and that the hub shows every node.

**Terms:** a **node** is a server that MMWatchdog watches. Each node runs
its own checks and restarts its own services, also without the hub. The
**fleet hub** is one node that also has the **fleet hub role**: it receives
a **node status report** from each other node every 30 seconds, sends the
alerts of every node, and alerts you when a node stops reporting. A
**joined node** is a node that reports to the hub. The **join code** is the
secret that lets a node join the hub. The **fleet settings** are what each
joined node copies from the hub: the alert destinations, the content of
each secret file that a destination names, and the fleet timings.

## Before you start

- **MMX is installed on the controller machine,** which runs macOS or
  Linux. `mmx version` prints a version. MMX's
  [install section](https://github.com/monkeymaxxing/mmx#install) says how.
- **Each node runs Linux with systemd,** on x86-64 (amd64) or ARM64.
  MMWatchdog is tested on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky
  Linux 9, AlmaLinux 10, Fedora 45, Arch Linux, and openSUSE Leap 16.1.
- **You log in to each node with `ssh`,** as root or as an account that can
  run any command with `sudo`. MMX uses your SSH settings, keys, and agent,
  and connects only to a node whose host key your SSH already trusts.
- **You chose the hub.** Choose a server that stays for a long time, such
  as a utility server, not one that you replace often. The hub keeps the
  fleet's identity and the readings of every node, so it needs a reliable
  disk with room for them: this page lets the readings of the nodes take
  up to 20 GiB.
- **Every node reaches the hub on one TCP port,** here 8445, over HTTPS.
  The hub listens on that port on every network interface, so let only
  your nodes through the hub's firewall.
- **You know where alerts go.** This page uses a Slack channel with an
  [incoming webhook](https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks).
  [Send alerts where your team sees them](send-alerts-somewhere.md) covers
  email, a webhook of your own, and the other destinations. The hub must
  reach the destination, and so must every joined node: a joined node sends
  an alert itself when it cannot reach the hub.

This page uses three nodes: the hub `hub01` at 203.0.113.10, and `web01`
and `web02` at 203.0.113.11 and 203.0.113.12. The hub knows each node by
the node's host name, which `hostname` prints on the node.

## Steps

1. **Download the release for your nodes.** Choose the archive for the
   nodes' processor, not the controller machine's: `linux-amd64` for x86-64
   nodes, `linux-arm64` for ARM64 nodes. `uname -m` on a node prints
   `x86_64` or `aarch64`.

   ```sh
   version=0.1.2
   platform=linux-amd64      # linux-arm64 for ARM64 nodes
   release=https://github.com/monkeymaxxing/mmwatchdog/releases/download/v$version

   curl -fLO "$release/mmwatchdog-$version-$platform.tar.gz"
   curl -fLO "$release/SHA256SUMS"
   ```

   **Check:** both files are in the current directory.

2. **Check the download:**

   ```sh
   shasum -a 256 --check --ignore-missing SHA256SUMS      # on macOS
   sha256sum --check --ignore-missing SHA256SUMS          # on Linux
   ```

   ```text
   mmwatchdog-0.1.2-linux-amd64.tar.gz: OK
   ```

   **Check:** your archive's line ends with `OK`. If not, download it
   again; do not unpack an archive that fails the check.

3. **Unpack the archive, and go into it:**

   ```sh
   tar -xzf "mmwatchdog-$version-$platform.tar.gz"
   cd "mmwatchdog-$version-$platform"
   ```

   The directory is an MMX **project** with two playbooks.
   `hub_and_standalone` installs the hub, with the configuration in
   `files/hub_and_standalone/`, on the node of
   `inventories/hub_and_standalone.yml`. `join_hub` installs every other
   node, with the configuration in `files/join_hub/`, on the nodes of
   `inventories/hub_members.yml`. Keep this directory, with the vault
   `secrets_vault.age` that step 5 creates and the directory `.mmx/`, where
   MMX saves each run: every later change to the fleet is made from it. A
   version-control system such as Git suits it.

   **Check:** `ls playbooks` lists `hub_and_standalone.yml` and
   `join_hub.yml`.

4. **Name the hub in the inventory.** In
   `inventories/hub_and_standalone.yml`, replace the starter node with the
   hub. Give each node its host name, here `hub01`: the fleet lists each
   node by its host name, so the inventory and the fleet then use the same
   names:

   ```yaml
   nodes:
     hub01:
       address: 203.0.113.10        # the hub's address, or a host name from your SSH settings
       ssh_user: deploy             # root, or an account that can use sudo without a password
   ```

   **Check:** you can log in, and the project is valid:

   ```sh
   ssh deploy@203.0.113.10 true && echo login works
   mmx validate
   ```

   ```text
   login works
   Valid: mmwatchdog
   ```

   If `ssh` asks whether to trust the node's host key, compare the
   fingerprint with the one your hosting provider shows and answer `yes`.
   Reason: MMX never accepts a new host key itself.

5. **Put the Slack address in MMX's vault.** The address is a secret:
   anyone who has it can post to your channel. MMX keeps secrets in the
   encrypted file `secrets_vault.age` in the project. Store the address
   under the file name `slack-webhook-team`:

   **Choose the vault passphrase.** Run this yourself, in your own terminal, not through a coding agent:

   ```sh
   mmx secrets write-passphrase-file
   ```

   MMX asks `New vault passphrase:` and `Confirm new vault passphrase:`. The passphrase is the one key to the encrypted secrets file. Make it new, and save it in your password manager. MMX creates the vault, saves the passphrase in a file that only you can read, and prints:

   ```text
   Created the vault secrets_vault.age with this passphrase. The vault holds no secrets yet.
   Your coding agent can now run mmx with MMX_VAULT_PASSWORD_FILE=~/.config/mmx/vault-passphrase-mmx_example.
   When you are done, delete the file: rm ~/.config/mmx/vault-passphrase-mmx_example
   ```

   A coding agent runs each later `mmx dryrun` and `mmx run` with that `MMX_VAULT_PASSWORD_FILE=...` line in front, and never reads the file. If you do the steps by hand, run `export MMX_VAULT_PASSWORD_FILE="$HOME/.config/mmx/vault-passphrase-mmx_example"` once, or delete the file and type the passphrase at each prompt. [MMX's guide to secrets](https://github.com/monkeymaxxing/mmx/blob/main/docs/how-to/keep-secrets.md) says more.

   Then store the address:

   ```sh
   mmx secrets set mmwatchdog_secret_files slack-webhook-team
   ```

   Type your webhook's address at the hidden prompt. The vault then holds:

   ```yaml
   mmwatchdog_secret_files:
     slack-webhook-team: "https://hooks.slack.com/services/YOUR/WEBHOOK/ADDRESS"
   ```

   Then list the file in `inventories/hub_and_standalone.yml`, under
   `vars`, in place of `mmwatchdog_secret_files: []`:

   ```yaml
   vars:
     mmwatchdog_config_source_dir: files/hub_and_standalone
     mmwatchdog_secret_files: [slack-webhook-team]
   ```

   The install writes the file to `/etc/mmwatchdog/secrets/` on the hub,
   readable only by root. The joined nodes need no such file: they get the
   address from the hub with the fleet settings.

   **Check:** `mmx secrets list-keys` prints `mmwatchdog_secret_files`.
   `mmx secrets set` prints nothing when it succeeds.

6. **Say where alerts go.** Replace `destinations: {}` in
   `files/hub_and_standalone/alert_destinations.yml` with:

   ```yaml
   destinations:
     team_slack_channel:
       slack:
         webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
       alert_severities: [info, warn, sos]   # this destination takes alerts of every severity
       remind_every: 4h                      # remind of each open problem every 4 hours
   ```

   This file on the hub holds the destinations of the whole fleet.

   **Check:** `mmx validate` prints `Valid: mmwatchdog`.

7. **Give the hub the fleet hub role.** At the end of
   `files/hub_and_standalone/mmwatchdog_node_settings.yml`, replace the
   commented-out example, from the line `# On the MMWatchdog fleet hub, add
   the fleet_hub_role block` to the end of the file, with this block, with the hub's
   address in `address_for_nodes`. All eight settings are required:

   ```yaml
   fleet_hub_role:
     listen_on_address: 0.0.0.0:8445                    # the hub listens on port 8445 of every interface
     address_for_nodes: 203.0.113.10:8445               # the address and port that the nodes connect to
     report_every: 30s                                  # each node reports every 30 seconds; 3 missed reports make it silent
     alert_after_hub_unreachable_for: 3m                # a node alerts that it cannot reach the hub after 3 minutes
     send_alert_without_hub_after_waiting_for: 30s      # a node sends an alert itself when the hub has not taken it for 30 seconds
     undelivered_alerts_of_nodes_content_size_limit: 32MiB   # the hub keeps at most 32 MiB of the nodes' alerts that wait for delivery
     keep_measurement_history_of_nodes_for: 7d          # the hub keeps the readings that the nodes send for 7 days
     measurement_history_of_nodes_size_limit: 20GiB     # and lets them take at most 20 GiB of its disk
   ```

   `address_for_nodes` can be a host name, such as `hub01.example.com:8445`.
   It goes into the join code, and every node connects to it.

   **Check:** `mmx validate` prints `Valid: mmwatchdog`.

8. **Preview the install of the hub:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   The output lists each step of the playbook on the hub, such as:

   ```text
     install / hub01 / install_monitoring/changes/install_binary: would create "/usr/local/bin/mmwatchdog"
     install / hub01 / install_monitoring/changes/install_unit: would create "/etc/systemd/system/mmwatchdog.service"
   ```

   **Check:** the dryrun connected to the hub and lists the changes. On a
   first install its exit code is 3: before anything is installed, some
   steps cannot be checked in advance, and their lines say `not_checked`.
   Exit code 1, or a line that says `failed`, is a fault.

9. **Install the hub:**

   ```sh
   mmx run hub_and_standalone
   ```

   ```text
   Run 38f83cc6: succeeded
   1 finished
   Saved: /home/dev/monitoring/mmwatchdog-0.1.2-linux-amd64/.mmx/runs/20261115T092520.420798015-38f83cc6936b45e9
   ```

   On its first start, the hub creates its TLS key, a certificate, and the
   join secret, in `/var/lib/mmwatchdog/fleet.db`.

   **Check:** the run ends with `succeeded`, and the last line of the hub's
   status names the fleet address:

   ```sh
   ssh deploy@203.0.113.10 sudo mmwatchdog status
   ```

   ```text
   machine/cpu_use_is_recorded  ok           cpu_used_percent is 0.51 over 29.484s
   ...
   Alert deliveries: 0 waiting, 0 dropped
   Fleet hub: nodes report to 203.0.113.10:8445; see mmwatchdog fleet
   ```

10. **Put the join code in the vault.** Print the join code on the hub:

    ```sh
    ssh deploy@203.0.113.10 sudo mmwatchdog fleet join-code
    ```

    ```text
    mmwatchdog-join 203.0.113.10:8445 ebba574e17670386674eeb04912fdd7555fb626c4f124addab10c6a5888168ee 6e86939f0a274f03001be8c90ba26997315840fe4b969cb161a531702db4a3b7
    ```

    The code holds the hub's address, the fingerprint of the hub's
    certificate, and the join secret. It is a secret: a node that joins
    with it gets the fleet settings, with your Slack address. Put the code
    in the vault straight from the hub, so that it never shows on a screen
    or passes through a chat:

    ```sh
    ssh deploy@203.0.113.10 sudo mmwatchdog fleet join-code | mmx secrets set mmwatchdog_join_code
    ```

    `mmx secrets set` prints nothing when it has saved the code. The
    pipe gives it the code, so it reads the vault passphrase from the file
    that `MMX_VAULT_PASSWORD_FILE` names, as step 5 shows.

    **Check:** `mmx secrets list-keys` prints `mmwatchdog_join_code` and
    `mmwatchdog_secret_files`.

11. **Name the other nodes in their inventory.** Replace the starter nodes
    `app-1` and `app-2` in `inventories/hub_members.yml` with your nodes:

    ```yaml
    nodes:
      web01:
        address: 203.0.113.11
        ssh_user: deploy

      web02:
        address: 203.0.113.12
        ssh_user: deploy
    ```

    Keep `mmwatchdog_secret_files: []` in this inventory: the joined nodes
    get the destinations' secrets from the hub. The checks of the joined
    nodes are in `files/join_hub/check_groups/`; the starter check group
    `machine` alerts when memory or the root filesystem runs short.

    **Check:** you can log in to each node, and the project is valid:

    ```sh
    ssh deploy@203.0.113.11 true && ssh deploy@203.0.113.12 true && echo login works
    mmx validate join_hub --inventory hub_members
    ```

    ```text
    login works
    Valid: mmwatchdog
    ```

12. **Preview the install of the nodes:**

    ```sh
    mmx dryrun join_hub --inventory hub_members
    ```

    ```text
      install / web01 / install_monitoring_on_a_joined_node/changes/join_code: would create "/etc/mmwatchdog/secrets/fleet-hub-join-code"
      install / web01 / install_monitoring_on_a_joined_node/changes/install_binary: would create "/usr/local/bin/mmwatchdog"
    ```

    **Check:** the dryrun connected to every node and lists the changes,
    and exits with code 3, as for the hub in step 8. The playbook writes the
    join code from the vault to `/etc/mmwatchdog/secrets/fleet-hub-join-code`
    on each node, readable only by root.

13. **Install MMWatchdog on the nodes, and join them to the hub:**

    ```sh
    mmx run join_hub --inventory hub_members
    ```

    ```text
    Run 17da248d: succeeded
    2 finished
    Saved: /home/dev/monitoring/mmwatchdog-0.1.2-linux-amd64/.mmx/runs/20261115T092615.418622364-17da248dfc89be69
    ```

    Each node joins with its first report, a few seconds after MMWatchdog
    starts.

    **Check:** the run ends with `succeeded`. On a joined node, the last
    line of the status names the hub and the node's last report:

    ```sh
    ssh deploy@203.0.113.11 sudo mmwatchdog status
    ```

    ```text
    ...
    node_reports/to_hub          ok           reports reach the hub
    Alert deliveries: 0 waiting, 0 dropped
    Fleet hub: 203.0.113.10:8445 as web01; last report 2026-11-15T09:26:57Z
    ```

14. **See every node on the hub:**

    ```sh
    ssh deploy@203.0.113.10 sudo mmwatchdog fleet
    ```

    ```text
    Fleet at 203.0.113.10:8445: 3 nodes, 3 healthy
    Hub-outage alerter nodes: web01, web02
    ```

    The count includes the hub. A healthy node gets no line of its own; a
    node that needs attention is listed with its problem.
    **Hub-outage alerter nodes** are the two joined nodes that alert you
    when they cannot reach the hub.

    **Check:** the count is the number of your nodes, hub included, and
    all are healthy. Right after an install, a node may be listed with
    `unknown: waiting for a new run` until each check has run once: run the
    command again after 30 seconds. A node that is missing has not joined:
    see [If something is different](#if-something-is-different).
    [Watch a fleet](watch-a-fleet.md) says how to read the fleet.

15. **Prove that an alert of a joined node reaches you through the hub.**
    Add this test check group for the joined nodes:

    ```yaml
    # files/join_hub/check_groups/alert_path_test.yml
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

    Install it, then make it fail on `web01`, and after the alert arrives,
    recover:

    ```sh
    mmx run join_hub --inventory hub_members
    ssh deploy@203.0.113.11 touch /var/tmp/mmwatchdog-alert-test
    # the failure message arrives in Slack within about 10 seconds
    ssh deploy@203.0.113.11 rm /var/tmp/mmwatchdog-alert-test
    # the recovery message arrives within about 10 seconds
    ```

    Slack shows two messages, sent by the hub:

    ```text
    Warning: web01: alert_path_test/shell failed
    command exited 1
    failing since 09:28 UTC
    exit code: 1
    ```

    ```text
    Resolved warning: web01: alert_path_test/shell is ok again
    command exited 0
    ```

    Then delete `files/join_hub/check_groups/alert_path_test.yml` and run
    `mmx run join_hub --inventory hub_members` again, which removes the test
    check group from the nodes.

    A run that changes the configuration restarts MMWatchdog on each node
    inside a whole-node maintenance window, so the restart sends no check
    alerts. Slack also shows the windows of the two runs. The hub sends one
    message when the first window opens, before the failure message:

    ```text
    Info: maintenance on for 2 nodes
    maintenance started on 2 nodes: web01, web02 (by root: mmwatchdog deploy)
    ```

    and one message when no node has been in a window for 10 minutes,
    about 10 minutes after the second run. The windows of both runs fall
    into that one pair of messages:

    ```text
    Info: maintenance off for 2 nodes
    maintenance turned off on 2 nodes: web02, web01 (by root: mmwatchdog deploy)
    maintenance ended on the last node in the alert at 09:29 UTC. Alerts on each node resumed when the node's maintenance ended. ...
    ```

    In a fleet with one joined node, the hub sends that node's own messages
    instead, such as `Info: web01: maintenance on for the whole node`.

    **Check:** the failure and the recovery message arrived. If not, see
    [If something is different](#if-something-is-different).

### The files you changed, whole

```yaml
# inventories/hub_and_standalone.yml
vars:
  mmwatchdog_config_source_dir: files/hub_and_standalone
  mmwatchdog_secret_files: [slack-webhook-team]

nodes:
  hub01:
    address: 203.0.113.10
    ssh_user: deploy
```

```yaml
# files/hub_and_standalone/alert_destinations.yml
destinations:
  team_slack_channel:
    slack:
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
    alert_severities: [info, warn, sos]   # this destination takes alerts of every severity
    remind_every: 4h                      # remind of each open problem every 4 hours
```

```yaml
# files/hub_and_standalone/mmwatchdog_node_settings.yml
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
alert_severity_when_this_node_goes_silent: sos
alert_severity_when_this_node_has_rebooted: warn
keep_measurement_history_for: 7d
measurement_history_size_limit: 256MiB
fleet_hub_role:
  listen_on_address: 0.0.0.0:8445
  address_for_nodes: 203.0.113.10:8445
  report_every: 30s
  alert_after_hub_unreachable_for: 3m
  send_alert_without_hub_after_waiting_for: 30s
  undelivered_alerts_of_nodes_content_size_limit: 32MiB
  keep_measurement_history_of_nodes_for: 7d
  measurement_history_of_nodes_size_limit: 20GiB
```

```yaml
# inventories/hub_members.yml
vars:
  mmwatchdog_config_source_dir: files/join_hub
  mmwatchdog_secret_files: []

nodes:
  web01:
    address: 203.0.113.11
    ssh_user: deploy

  web02:
    address: 203.0.113.12
    ssh_user: deploy
```

The vault holds `mmwatchdog_secret_files` with the Slack address, and
`mmwatchdog_join_code`.

MMWatchdog now watches every node, and the hub shows them all. Next,
[add a check](add-a-check.md) for each service on the nodes: put the
check groups of the joined nodes in `files/join_hub/check_groups/`, and
install them with `mmx run join_hub --inventory hub_members`.

## Where alerts go in a fleet

- **The hub sends every alert.** The destinations are only in the hub's
  `alert_destinations.yml`. A joined node sends each alert to the hub at
  once, and the hub delivers it. When the same check fails on several
  nodes within a minute, the hub sends the first alert at once and the
  others together in one message, such as
  `myapp/http failed on 7 nodes: app-2, app-3, …, and 2 more`.
  [Alerts in a fleet](set-up-a-fleet.md#where-alerts-go-in-a-fleet) gives every rule.
- **A joined node has no destinations of its own.** MMWatchdog on a joined
  node refuses to start while `/etc/mmwatchdog/alert_destinations.yml`
  exists, and `join_hub` removes that file from each node.
- **Each joined node keeps a copy of the fleet settings,** with the
  destinations and their secrets, in
  `/var/lib/mmwatchdog/fleet-settings-from-the-hub.json`, readable only by
  root. A changed destination reaches every node with its next report.
- **A node sends an alert itself when the hub does not take it.** When a
  delivery to the hub has failed, the node sends each alert that the hub
  has not taken 30 seconds after the alert happened
  (`send_alert_without_hub_after_waiting_for`), from its copy of the
  fleet settings. The node's log says so:

  ```text
  the failed alert of myapp/health_page_answers goes directly to the alert destinations, because the latest attempt to deliver an alert to the hub failed: Post "https://203.0.113.10:8445/v1/alerts": dial tcp 203.0.113.10:8445: connect: connection refused
  ```

  For that reason each node must reach each destination, and `join_hub`
  installs every destination module on every node.
- **Two nodes alert you when the hub is down.** Each hub-outage alerter
  node alerts after it has not reached the hub for 3 minutes
  (`alert_after_hub_unreachable_for`), with
  `SOS: web01 cannot reach the MMWatchdog fleet hub`.
  [Watch a fleet](watch-a-fleet.md#when-the-hub-stops) says more.

## If something is different

- **MMWatchdog already runs on one node,** from
  [Install MMWatchdog on one node](install-on-one-node.md). That node can
  be the hub, if it stays for a long time: use the same project, and skip
  steps 1 to 6. Do steps 7 to 15: `mmx run hub_and_standalone` in step 9
  adds the role, and the node keeps its checks and destinations. To make
  another server the hub instead, see
  [Move the hub](change-a-fleet.md#move-the-hub-to-another-server).
- **The nodes reach the hub at another address,** such as through port
  forwarding or a proxy. `listen_on_address` is the interface and port on
  the hub; `address_for_nodes` is what the nodes connect to. Both go into
  effect when `mmx run hub_and_standalone` restarts MMWatchdog. A changed
  `address_for_nodes` needs a new join code on every node, as
  [Move the hub](change-a-fleet.md#move-the-hub-to-another-server) says.
- **The nodes have different jobs,** such as web servers and Git servers,
  each with its own checks. Copy `files/join_hub/` to another directory,
  such as `files/git_servers/`, and change the check groups in it. Then
  choose the directory for those nodes with an MMX inventory group in
  `inventories/hub_members.yml`:

  ```yaml
  # inventories/hub_members.yml
  vars:
    mmwatchdog_config_source_dir: files/join_hub   # the configuration of each node without its own
    mmwatchdog_secret_files: []
  nodes:
    web01:
      address: 203.0.113.11
      ssh_user: deploy
    git01:
      address: 203.0.113.20
      ssh_user: deploy
  groups:
    git_servers:
      nodes: [git01]
      vars:
        mmwatchdog_config_source_dir: files/git_servers   # the whole configuration of these nodes
  ```

  A node's value replaces its group's, and a group's replaces the
  inventory's. An MMX inventory group only chooses files; a **node group**,
  which the hub shows, is another thing:
  [Put nodes in node groups](change-a-fleet.md#put-nodes-in-node-groups).
- **A node matters more than the others.** In
  `files/join_hub/mmwatchdog_node_settings.yml`,
  `alert_severity_when_this_node_goes_silent: warn` makes one silent node a
  warning, and `alert_severity_when_this_node_has_rebooted: info` makes a
  reboot news. For a node that matters, such as the database server, give
  it a configuration directory of its own, as above, with `sos`.
- **Some nodes are ARM64 and some x86-64.** One project serves nodes of one
  processor kind. Unpack both archives as two projects, give both vaults
  the same `mmwatchdog_join_code`, and install the hub before any joined
  node.
- **A node is missing from `mmwatchdog fleet`.** The last line of
  `sudo mmwatchdog status` on the node gives the latest error:

  | Error on the node | Fix |
  |---|---|
  | `dial tcp 203.0.113.10:8445: connect: connection refused` | MMWatchdog does not run on the hub, or a firewall blocks the port. Check `systemctl is-active mmwatchdog` on the hub, and the firewall |
  | `joining the hub: hub returned HTTP 403: invalid join code` | The vault holds an old join code. Put the code that `sudo mmwatchdog fleet join-code` prints on the hub in the vault, and run `mmx run join_hub --inventory hub_members` |

  Two nodes with the same host name cannot both join: the hub refuses the
  second, and `sudo mmwatchdog fleet` shows the name conflict. Give each
  node its own host name.
  [Watch a fleet](watch-a-fleet.md#if-something-is-different) says when
  the `Name conflict:` line goes away.
- **The run fails with `duplicate service configuration path
  /etc/mmwatchdog/alert_destinations.yml`,** such as:

  ```text
    install / web01 / install_monitoring_on_a_joined_node: failed, unchanged — duplicate service configuration path /etc/mmwatchdog/alert_destinations.yml: two node tasks that may run on the node write to the same path: node task install_monitoring_on_a_joined_node/install_config (playbooks/join_hub.yml:243), content from files/join_hub and node task install_monitoring_on_a_joined_node/remove_local_alert_destinations (playbooks/join_hub.yml:252)
  ```

  `files/join_hub/` holds an `alert_destinations.yml`, which
  `install_config` would install, while `remove_local_alert_destinations`
  removes that file from every joined node. Delete
  `files/join_hub/alert_destinations.yml`: the destinations belong in
  `files/hub_and_standalone/`. The run stopped before it installed
  anything, so MMWatchdog on the nodes runs as before. Run
  `mmx run join_hub --inventory hub_members` again.
- **The fleet is large.** In a load test, a hub with 10,000 nodes used
  about one CPU core and 300 MiB of memory, with a 26 MiB `fleet.db`,
  without readings. The readings add one write to the hub's disk for each
  report, and take up to `measurement_history_of_nodes_size_limit`. The hub
  saves every report before it answers, so give it a reliable disk.
- **You update MMWatchdog later.** Update the hub first, then the joined
  nodes: [Update a fleet](update-mmwatchdog.md#update-a-fleet).
- **The nodes must not be installed by MMX.** The
  [how MMWatchdog works](../how-it-works.md#the-fleet) gives every setting of `fleet_hub_role`
  and `join_fleet_hub`, which you write in
  `/etc/mmwatchdog/mmwatchdog_node_settings.yml` on each node.

## Related pages

- [Watch a fleet](watch-a-fleet.md)
- [Change a fleet](change-a-fleet.md)
- [Install MMWatchdog on one node](install-on-one-node.md)
- [Send alerts where your team sees them](send-alerts-somewhere.md)
- [Look back at measurements](look-back-at-measurements.md), with the
  readings of every node on the hub.
