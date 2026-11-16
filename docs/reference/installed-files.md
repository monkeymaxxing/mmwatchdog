# Files that an install puts on a node

This page lists every file, directory, account, and socket that an install
of MMWatchdog with the release's MMX project puts on a node, with its owner,
mode, and purpose, and what a later deploy does with it. The paths are the
defaults; a node's `mmwatchdog_node_settings.yml` can move the state
directory (`state_dir`), the module state directory (`module_state_dir`),
and the socket (`socket`).

The values come from a Debian 13 node that runs MMWatchdog 0.1.1, installed
with the playbook `hub_and_standalone`. `ls -l`, `stat`, and `getent` on a
node show the same.

## The program and the service

| Path | Owner, mode | What it is for | A deploy |
|---|---|---|---|
| `/usr/local/bin/mmwatchdog` | `root:root`, `0755` | The `mmwatchdog` program: the service, and every command, such as `mmwatchdog status` | Replaces it when the project's `mmwatchdog` differs |
| `/etc/systemd/system/mmwatchdog.service` | `root:root`, `0644` | The systemd unit that runs `mmwatchdog daemon` as root | Replaces it when the project's `mmwatchdog.service` differs |
| `/etc/systemd/system/multi-user.target.wants/mmwatchdog.service` | A link that systemd makes | Starts MMWatchdog at boot | Makes it again if it is missing |

The unit, as the release installs it:

```ini
[Unit]
Description=MMWatchdog monitoring
After=network-online.target
Wants=network-online.target
# No start limit: systemd's default refuses a sixth start within 10 seconds,
# which fails a deploy that restarts MMWatchdog several times in a row.
# Restart=always with RestartSec=3 already keeps a crashing daemon to one
# start every 3 seconds.
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=/usr/local/bin/mmwatchdog daemon --config-dir /etc/mmwatchdog
ExecReload=/usr/local/bin/mmwatchdog reload
Restart=always
RestartSec=3
RuntimeDirectory=mmwatchdog
RuntimeDirectoryMode=0755
StateDirectory=mmwatchdog
StateDirectoryMode=0700
UMask=0077
KillMode=control-group

[Install]
WantedBy=multi-user.target
```

`RuntimeDirectory` and `StateDirectory` make systemd create
`/run/mmwatchdog/` and `/var/lib/mmwatchdog/` when the service starts.

## The configuration

| Path | Owner, mode | What it is for | A deploy |
|---|---|---|---|
| `/etc/mmwatchdog/` | `root:root`, `0755` | The configuration directory that the service reads (`--config-dir`) | Keeps it |
| `/etc/mmwatchdog/mmwatchdog_node_settings.yml` | `root:root`, `0644` | The node's settings | Replaces it with the project's file |
| `/etc/mmwatchdog/alert_destinations.yml` | `root:root`, `0644` | Where alerts go. A joined node has none: it gets its destinations from the hub | Replaces it with the project's file; `join_hub` removes it |
| `/etc/mmwatchdog/check_groups/` | `root:root`, `0755`; each `.yml` file `0644` | The check groups, one or more `.yml` files | Makes the directory match the project's: removes each `.yml` file that the project no longer has |
| `/etc/mmwatchdog/secrets/` | `root:root`, `0700`; each file `0600` | The secret files, such as `slack-webhook-team` with a Slack address, which the configuration names in inputs that end in `_file` | Writes each file that the inventory's `mmwatchdog_secret_files` lists, from the vault. Never removes a file, also not one that the inventory no longer lists |
| `/etc/mmwatchdog/secrets/fleet-hub-join-code` | `root:root`, `0600` | On a joined node only: the code that lets the node join its hub | `join_hub` writes it from the vault |

A file that a deploy does not write, directly in `/etc/mmwatchdog/`, stays
there.

## The modules and what a deploy checks

`/usr/local/lib/mmwatchdog/` (`root:root`, `0755`) holds the modules that
are not built into the program, and the copies that each deploy checks
before it installs anything. MMWatchdog itself reads only the two module
directories.

| Path | Owner, mode | What it is for | A deploy |
|---|---|---|---|
| `official_extra_modules/` | `root:root`, `0755`; files `0644` | The official extra modules of the release, such as `slack` and `mongodb`, one directory each | Replaces the whole directory with a checked copy, when anything in it changed |
| `external_modules/` | `root:root`, `0755`; files `0644` | The modules of your own, from the project's `mmwatchdog_external_modules/` | Same as `official_extra_modules/` |
| `mmwatchdog-candidate` | `root:root`, `0755` | The new program, which checks the new configuration before it is installed | Replaces it at each deploy that brings another program |
| `candidate_config/` | `root:root`, `0700`, and its `check_groups/` `0700`; files `0644` | A copy of the project's configuration, which the deploy checks | Makes it an exact copy of the project's configuration directory |
| `candidate_official_extra_modules/`, `candidate_external_modules/` | `root:root`, `0755` | Copies of the two module directories, which the deploy checks | Makes each an exact copy of the project's directory |
| `installed_config_files` | `root:root`, `0644` | The list of the installed configuration files | Writes it; a change restarts MMWatchdog |
| `installed_module_files_checksum`, `installed_secret_files_checksum` | `root:root`, `0644` | One checksum of all module files, and one of all secret files | Writes them; a change restarts MMWatchdog |
| `deploy_opened_maintenance` | `root:root` | Exists only while a deploy has a maintenance window open, so that the deploy ends only the window that it opened | Makes it and removes it in the same deploy |
| `official_extra_modules.mmwatchdog-new`, `.mmwatchdog-old`, and the same for `external_modules` | `root:root` | Exist only while a deploy swaps a module directory. A deploy that was interrupted can leave them, and in the middle of the swap it can leave the node without `official_extra_modules/` or `external_modules/`: run the playbook again, which puts the directory back | Removes them at the next deploy that changes the modules |

A deploy that fails its check of the configuration stops before it changes
the configuration, the modules, the program, or the running service. But
it has already written the secret files, and on a joined node the join
code, because the check reads them. On a standalone node and on the hub,
MMWatchdog reads a destination's secret file at each delivery, so a changed
secret, such as a new Slack address, is in use at once, although the deploy
failed. Joined nodes keep the hub's earlier copy until the hub restarts or
reloads.

### Install with a tool of your own

A tool of your own must check the new files before it installs any of
them, as the release's playbooks do. Write the secret files first, because
the check reads them. Then copy the whole configuration directory and the
two module directories to separate directories on the node, and check them
with the new program:

```sh
sudo mmwatchdog validate --config-dir "$NEW_CONFIG_DIR" --official-extra-module-dir "$NEW_OFFICIAL_EXTRA_MODULE_DIR" --external-module-dir "$NEW_EXTERNAL_MODULE_DIR"
```

Check the whole set, not one file at a time: only the whole set shows
whether the node settings, the check groups, the destinations, and the
modules fit together. Install the files only after the check passes. Then
restart MMWatchdog, as the playbooks do: a restart starts a new program,
and picks up the secret files that MMWatchdog reads only when it starts.

## The state

| Path | Owner, mode | What it is for | A deploy |
|---|---|---|---|
| `/var/lib/mmwatchdog/` | `root:root`, `0700` | MMWatchdog's state directory (`state_dir`). MMWatchdog refuses to start when it is not a directory owned by root with mode `0700`, or is a symbolic link | Never changes it |
| `/var/lib/mmwatchdog/state.db` | `root:root`, `0600` | Events, open problems, maintenance windows, restart attempts, and alerts that wait for delivery | Never changes it |
| `/var/lib/mmwatchdog/measurement_history.db` | `root:root`, `0600` | The node's measurement history: every reading, for `keep_measurement_history_for` and up to `measurement_history_size_limit` | Never changes it |
| `/var/lib/mmwatchdog/*.db-wal`, `*.db-shm` | `root:root`, `0600` | Working files of the two databases while MMWatchdog runs; a clean stop folds them into the `.db` files | Never changes them |
| `/var/lib/mmwatchdog/daemon.lock` | `root:root`, `0600` | Keeps a second MMWatchdog from using the same state | Never changes it |
| `/var/lib/mmwatchdog/fleet.db` | `root:root` | On the hub only: the hub's key, certificate, join secret, and joined nodes | Never changes it |
| `/var/lib/mmwatchdog/measurement_history_of_nodes.db` | `root:root` | On the hub only: the readings that joined nodes send | Never changes it |
| `/var/lib/mmwatchdog-module-state/` | `root:root`, `0711` | One directory for each check whose module is not built in, `CHECK_GROUP/CHECK`, such as `web_server/apache_status`, for what the module keeps from one run to the next. `CHECK_GROUP/` is `root:root`, `0711`; `CHECK/` is `0700`, owned by the account that runs the check, such as `mmwatchdog:mmwatchdog`. MMWatchdog makes each directory at the first run of such a check | Never changes it |

Because a deploy never changes the state, an update keeps the history, and
a node that you install again keeps it too if the directory is still
there. [Remove MMWatchdog from a node](../how-to/remove-mmwatchdog.md)
says how to save it.

## The account and the group

| Name | What it is | What it is for | A deploy |
|---|---|---|---|
| `mmwatchdog` (user) | A system account, with the home `/var/lib/mmwatchdog-checks` and the shell `/usr/sbin/nologin` | Runs each `shell` check, each check of a module that is not built in, and each recovery command, unless the check names another account in `run_as_user_account`. MMWatchdog itself runs as root | Makes it if it is missing |
| `mmwatchdog` (group) | A system group, the account's primary group | — | Makes it if it is missing |
| `/var/lib/mmwatchdog-checks/` | `mmwatchdog:mmwatchdog`, `0700` | The account's home, separate from MMWatchdog's state | Makes it and sets its owner and mode |

The numbers of the user and group differ from node to node: on the Debian
node, `getent passwd mmwatchdog` printed
`mmwatchdog:x:988:989::/var/lib/mmwatchdog-checks:/usr/sbin/nologin`.

## The socket and the network

| What | Owner, mode | What it is for |
|---|---|---|
| `/run/mmwatchdog/` | `root:root`, `0755` | The runtime directory, which systemd makes when the service starts and removes when it stops |
| `/run/mmwatchdog/mmwatchdog.sock` | `root:root`, `0600` | The Unix socket of the running service (`socket`). Commands such as `mmwatchdog status` talk to the service through it, which is why they need `sudo` |

MMWatchdog listens on no network port by default. It listens only where a
setting says so: `heartbeat_receiver.listen_on_address`, for jobs that
report in, and `fleet_hub_role.listen_on_address`, on the hub, for the
reports of joined nodes.

## What MMX itself puts on the node

MMX, which runs the install, keeps files of its own on each node that it
manages: the service `mmx-https-api.service`, and the directories
`/opt/mmx/`, `/var/lib/mmx/`, and `/run/mmx/`. They belong to MMX, not to
MMWatchdog. `mmx remove-mmx-from-node` removes them, as MMX's
[Remove MMX from a server](https://github.com/monkeymaxxing/mmx/blob/main/docs/how-to/keep-mmx-off-a-server.md#remove-mmx-from-a-server)
says.

## Related pages

- [Install MMWatchdog on one node](../how-to/install-on-one-node.md)
- [Update MMWatchdog](../how-to/update-mmwatchdog.md)
- [Remove MMWatchdog from a node](../how-to/remove-mmwatchdog.md)
- [Module directories](configuration-files.md#module-directories), the settings that
  move the module directories
