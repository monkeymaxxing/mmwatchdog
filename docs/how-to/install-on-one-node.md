# Install MMWatchdog on one node

**When to use this page:** you want MMWatchdog to watch a Linux server, a
**node**, that does not run MMWatchdog yet. For several nodes that report to
one fleet hub, install the first node with this page, then follow
[Set up a fleet](set-up-a-fleet.md).

**Who acts:** you, or your coding agent. Steps 1 to 9 run on your
**controller machine**: the machine where you run `mmx`, such as your
laptop. Steps 10 and 11 also run commands on the node. Show the person the
output of step 8 and wait for their yes before step 9, because step 9
changes the node. The person runs `mmx secrets write-passphrase-file` once,
in their own terminal, in step 6. The agent then runs `mmx dryrun` and
`mmx run` itself, with `MMX_VAULT_PASSWORD_FILE` set.

**What this page does:** [MMX](https://github.com/monkeymaxxing/mmx), the
Monkeymaxxing tool that sets up servers, copies the `mmwatchdog` program,
its configuration, and its modules to the node, and starts MMWatchdog as the
systemd service `mmwatchdog.service`. Then you prove that an alert reaches
you.

## Before you start

- **MMX is installed on the controller machine,** which runs macOS or
  Linux. `mmx version` prints a version. MMX's
  [install section](https://github.com/monkeymaxxing/mmx#install) says how.
- **The node runs Linux with systemd,** on x86-64 (amd64) or ARM64.
  MMWatchdog is tested on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky
  Linux 9, AlmaLinux 10, Fedora 45, Arch Linux, and openSUSE Leap 16.1.
  [Limits on each distribution](../reference/limits-on-each-distribution.md) says
  what MMWatchdog cannot check on some of them.
- **You log in to the node with `ssh`,** as root or as an account that can
  run any command with `sudo`. MMX uses your SSH settings, keys, and agent,
  and connects only to a node whose host key your SSH already trusts.
- **You know where alerts go.** This page uses a Slack channel with an
  [incoming webhook](https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks).
  For email, a webhook of your own, Telegram, Discord, Mattermost,
  Pushover, or MMHotline, see
  [If something is different](#if-something-is-different). The node must
  be able to reach the destination, for Slack over HTTPS.

## Steps

1. **Download the release for your node.** Choose the archive for the
   node's processor, not the controller machine's: `linux-amd64` for x86-64
   nodes, `linux-arm64` for ARM64 nodes. `uname -m` on the node prints
   `x86_64` or `aarch64`.

   ```sh
   version=0.4.0
   platform=linux-amd64      # linux-arm64 for ARM64 nodes
   release=https://github.com/monkeymaxxing/mmwatchdog/releases/download/v$version

   curl -fLO "$release/mmwatchdog-$version-$platform.tar.gz"
   curl -fLO "$release/SHA256SUMS"
   ```

   **Check:** both files are in the current directory.

   **If GitHub is down,** build the same two
   files from the mirror at `gitlab.com/monkeymaxxing/mmwatchdog`, which has
   the code and the release tags but no release archives. You need Git,
   Python 3, and Go 1.21 or newer; Go downloads the exact Go version that
   MMWatchdog needs:

   ```sh
   git clone https://gitlab.com/monkeymaxxing/mmwatchdog.git mmwatchdog-source
   git -C mmwatchdog-source checkout "v$version"
   python3 mmwatchdog-source/scripts/release.py "$version" --output-dir .
   ```

   The script builds both archives and `SHA256SUMS` into the current
   directory. Then continue with step 2.

2. **Check the download:**

   ```sh
   shasum -a 256 --check --ignore-missing SHA256SUMS      # on macOS
   sha256sum --check --ignore-missing SHA256SUMS          # on Linux
   ```

   ```text
   mmwatchdog-0.4.0-linux-amd64.tar.gz: OK
   ```

   **Check:** your archive's line ends with `OK`. If not, download it
   again; do not unpack an archive that fails the check.
   `--ignore-missing` skips the archive of the other processor, which you
   did not download.

3. **Unpack the archive, and go into it:**

   ```sh
   tar -xzf "mmwatchdog-$version-$platform.tar.gz"
   cd "mmwatchdog-$version-$platform"
   ```

   The directory is an MMX **project**. It holds the `mmwatchdog` program
   for the node, the starter configuration in `files/hub_and_standalone/`,
   the playbook `hub_and_standalone` that installs MMWatchdog, and the
   inventory `inventories/hub_and_standalone.yml` that names the node. Keep
   this directory, with the vault `secrets_vault.age` that step 6 creates
   and the directory `.mmx/`, where MMX saves each run: every later change to
   the node is made from it. A version-control system such as Git suits
   it.

   **Check:** `ls` lists `mmx_project_settings.yml` and `mmwatchdog`.

4. **Name the node in the inventory.** In
   `inventories/hub_and_standalone.yml`, replace the starter values
   `node.example.com` and `deploy` with your node's address and account:

   ```yaml
   nodes:
     node:
       address: 203.0.113.11        # the node's address, or a host name from your SSH settings
       ssh_user: deploy             # root, or an account that can use sudo without a password
   ```

   **Check:** you can log in, and the project is valid:

   ```sh
   ssh deploy@203.0.113.11 true && echo login works
   mmx validate
   ```

   ```text
   login works
   Valid: mmwatchdog
   ```

   If `ssh` asks whether to trust the node's host key, compare the
   fingerprint with the one your hosting provider shows and answer `yes`.
   Reason: MMX never accepts a new host key itself. A coding agent, which
   cannot answer the prompt, shows the person the fingerprint and, after
   their yes, logs in once with
   `ssh -o StrictHostKeyChecking=accept-new deploy@203.0.113.11 true`. If `ssh` asks for a
   password, that works too: the first `mmx dryrun` asks for it, and MMX
   then adds a key of its own to the node, as MMX's
   [get started](https://github.com/monkeymaxxing/mmx/blob/main/docs/get-started.md)
   describes.

5. **Read the node settings.** `files/hub_and_standalone/mmwatchdog_node_settings.yml`
   states the six settings that every node needs. The starter values suit
   most nodes:

   ```yaml
   keep_undelivered_alerts_for: 24h                 # keep an alert that could not be delivered, and try again, for 24 hours
   undelivered_alerts_content_size_limit: 8MiB      # and keep at most 8 MiB of such alerts
   alert_severity_when_this_node_goes_silent: sos   # used by a fleet hub when this node stops reporting
   alert_severity_when_this_node_has_rebooted: warn # the severity of the notice after a reboot of this node
   keep_measurement_history_for: 7d                 # keep every reading of the checks for 7 days
   measurement_history_size_limit: 256MiB           # and never more than 256 MiB of readings
   ```

   **Check:** none; change a value only if you have a reason, such as
   `info` for the reboot notice of one of many web servers.

6. **Put the Slack address in MMX's vault.** The address is a secret:
   anyone who has it can post to your channel. MMX keeps secrets in the
   encrypted file `secrets_vault.age` in the project, and writes them to the
   node during the install. Store the address under the file name
   `slack-webhook-team`:

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

   MMX asks for the value. Type your webhook's address at the hidden
   prompt. A coding agent asks the person to run this command and type the
   address themselves, so the address never passes through the chat.
   The vault then holds:

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

   Each install writes each listed file to `/etc/mmwatchdog/secrets/` on
   the node, readable only by root, before it checks the configuration.

   **Check:** `mmx secrets list-keys` prints `mmwatchdog_secret_files`.
   `mmx secrets set` prints nothing when it succeeds. If it reports an
   error, it kept the vault as it was: run it again. To change the address
   later, run it again with `--replace`.

7. **Say where alerts go.** Replace `destinations: {}` in
   `files/hub_and_standalone/alert_destinations.yml` with:

   ```yaml
   destinations:
     team_slack_channel:
       slack:
         webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
       alert_severities: [info, warn, sos]   # this destination takes alerts of every severity
       remind_every: 4h                      # remind of each open problem every 4 hours
   ```

   **Check:** `mmx validate` prints `Valid: mmwatchdog`.

8. **Preview what the install changes:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   The output lists each step of the playbook on the node, such as:

   ```text
     install / node / install_monitoring/changes/install_binary: would create "/usr/local/bin/mmwatchdog"
       absent -> 17318048 bytes, permissions_mode 0755, uid 0, gid 0
     install / node / install_monitoring/changes/install_unit: would create "/etc/systemd/system/mmwatchdog.service"
   ```

   **Check:** the dryrun connected to the node and lists the changes. On a
   first install its exit code is 3: before anything is installed, some
   steps cannot be checked in advance, and their lines say `not_checked` or `skipped,
   unknown`. Exit code 1, or a line that says `failed`, is a fault; see
   [If something is different](#if-something-is-different).

9. **Install MMWatchdog:**

   ```sh
   mmx run hub_and_standalone
   ```

   ```text
   Run 2b792738: succeeded
   1 finished
   ```

   The run checks the whole configuration on the node before it installs
   anything. The starter check group `machine`, in
   `files/hub_and_standalone/check_groups/node.yml`, records CPU use and
   alerts at `warn` when memory use exceeds 90 percent or the root
   filesystem is more than 85 percent full, and at `sos` when the root
   filesystem is more than 95 percent full.

   **Check:** the run ends with `succeeded`.

10. **Confirm that MMWatchdog runs.** On the node:

    ```sh
    ssh deploy@203.0.113.11 sudo mmwatchdog version
    ssh deploy@203.0.113.11 sudo mmwatchdog status
    ```

    ```text
    mmwatchdog 0.4.0 (ad75e5909a0af9b03215d2b2ac4c5bdfa9a56777, linux/amd64, modified=false)
    ```

    ```text
    machine/cpu_use_is_recorded  ok           cpu_used_percent is 2.56 over 0.201s
    machine/memory_is_below_90_percent ok           memory_used_percent is 17.41 (fails above 90)
    machine/root_filesystem_is_below_85_percent ok           space_used_percent is 9.12 (fails above 85)
    machine/root_filesystem_is_below_95_percent ok           space_used_percent is 9.28 (fails above 95)
    Alert deliveries: 0 waiting, 0 dropped
    ```

    **Check:** `version` shows the release you installed and the node's
    processor, and `status` lists every check, with no line that says that
    a destination fails. A destination that fails has a line such as `Destination
    team_slack_channel: 1 alert is not delivered`, with the reason.

11. **Prove that an alert reaches you.** A check that runs well proves
    nothing about the alert path, so make one check fail and recover. Add
    this test check group to the project:

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
    failing since 04:04 UTC
    ```

    ```text
    Resolved warning: app01: alert_path_test/shell is ok again
    command exited 0
    ```

    Then delete `files/hub_and_standalone/check_groups/alert_path_test.yml`
    and run `mmx run hub_and_standalone` again, which removes the test check
    group from the node.

    Each run that changes the configuration also sends two `Info` messages,
    `maintenance on` and `maintenance off`: MMWatchdog restarts inside a
    short maintenance window, so the restart sends no check alerts.

    **Check:** both messages arrived. If not, see
    [If something is different](#if-something-is-different).

### The two files you changed, whole

```yaml
# inventories/hub_and_standalone.yml
# The directory of this project whose files each node gets as its
# MMWatchdog configuration in /etc/mmwatchdog. A node or a group of
# another role sets its own directory under files/ in its vars.
vars:
  mmwatchdog_config_source_dir: files/hub_and_standalone
  # The secret files that each node gets in /etc/mmwatchdog/secrets, with
  # mode 0600. List the file names here, and put each file's content in the
  # MMX vault under the secret mmwatchdog_secret_files with
  # mmx secrets set mmwatchdog_secret_files NAME, which asks for the value:
  #   mmwatchdog_secret_files:
  #     slack-webhook-team: "https://hooks.slack.com/services/..."
  # A node of this inventory gets the whole mmwatchdog_secret_files secret,
  # and writes the files listed here.
  mmwatchdog_secret_files: [slack-webhook-team]

nodes:
  node:
    address: 203.0.113.11
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

MMWatchdog now watches the node, and the starter check group alerts you
when memory or the disk runs short. Next,
[add a check](add-a-check.md) for each service or application on the node.

## If something is different

- **The node is ARM64.** Download `linux-arm64` in step 1. One project
  serves nodes of one processor kind: for x86-64 and ARM64 nodes, unpack
  both archives and use two projects.
- **Alerts go elsewhere than Slack.** Email (`email`), a JSON webhook of
  your own (`webhook`), and MMHotline (`mmhotline`) are built in.
  Telegram, Discord, Mattermost, and Pushover are modules that ship with
  MMWatchdog, named the same way. Each module's page, printed by
  `mmwatchdog describe-module NAME` and listed in the
  [module pages](../reference/modules/README.md), gives its inputs and how to get
  its address or token. Put each password or token in the vault and the
  inventory as in step 6, and name its file in the destination's input
  that ends in `_file`.
- **No alerts yet.** Keep `destinations: {}` and skip steps 6, 7, and 11.
  MMWatchdog then runs the checks and restarts services, and `status` says
  `This node sends alerts to nobody.`
- **Step 4's login check fails.** Fix the SSH login first: MMX logs in the
  same way. A host name, a port, or a key file for the node can go in your
  SSH settings, such as `~/.ssh/config`, and the inventory's `address` can
  name that host.
- **The run fails at `validate_config`.** The dryrun does not check your
  new files: it checks only the copy of the configuration that the last run
  left on the node. Only `mmx run` checks the new configuration. The output names
  the file, the line, and the fault, such as a mistake in a check group, a
  secret file that a destination names but the node does not have, or a
  module that neither the release nor the project holds. Fix it and run
  again. The node keeps its configuration, its modules, and its running
  MMWatchdog; only the secret files of step 6 are already written, and
  MMWatchdog already uses a changed Slack address.
- **The run fails at `write_secret_files`** with a line such as
  `.secrets.mmwatchdog_secret_files[.secret_file]: value is unavailable at
  /secrets/mmwatchdog_secret_files/missing-file`. The inventory lists a
  file, here `missing-file`, that the vault's `mmwatchdog_secret_files`
  does not hold. Each name in the inventory's list must be a key under
  `mmwatchdog_secret_files` in the vault.
- **The status line of the destination says the alert is not delivered.**
  The line gives the reason, such as `the module slack reported: the file
  that webhook_url_file names must hold one web address that starts with
  https:// or http://, and nothing else`. Fix the address with
  `mmx secrets set mmwatchdog_secret_files NAME --replace` and run `mmx run hub_and_standalone`; a changed secret
  file restarts MMWatchdog.
- **You build the archives yourself.** [Release MMWatchdog](../changing-mmwatchdog/releasing.md)
  says how; the archives are the same as a downloaded release's.
- **The node must not be installed by MMX.** The
  [list of installed files](../reference/installed-files.md) says where each
  file of an installation goes, the account `mmwatchdog`, and the systemd
  unit, and
  [Install with a tool of your own](../reference/installed-files.md#install-with-a-tool-of-your-own)
  says how to check the new files before you install them.

## What the install puts on the node

- The program `/usr/local/bin/mmwatchdog`, and the systemd unit
  `/etc/systemd/system/mmwatchdog.service`, enabled and started.
- The configuration in `/etc/mmwatchdog/`: `mmwatchdog_node_settings.yml`,
  `alert_destinations.yml`, `check_groups/`, and `secrets/`.
- The modules that are not built in, in `/usr/local/lib/mmwatchdog/`.
- The system account `mmwatchdog`, which runs the checks that need no root,
  with its home `/var/lib/mmwatchdog-checks`. MMWatchdog itself runs as
  root.
- MMWatchdog's state, in `/var/lib/mmwatchdog/`.

## Related pages

- [Add a check](add-a-check.md)
- [See what is happening](see-what-is-happening.md)
- [Update MMWatchdog](update-mmwatchdog.md)
