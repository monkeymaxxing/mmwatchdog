# Official extra modules

A **module** is the code that does the work of one check or of one alert
destination of MMWatchdog. An **official extra module** is a module that the MMWatchdog
project writes, maintains, and tests, as a Starlark script. Starlark is a
small language that reads like Python. An official extra module is not built
into the `mmwatchdog` program: MMWatchdog runs the script when a check or an alert
destination names the module.

Official extra modules are for uncommon work and for alerts sent to a
service operated by a single provider, such as Slack or Telegram.

The [index of the module pages](../docs/reference/modules/README.md#official-extra-modules)
lists every official extra module, with what each one checks or where it
delivers alerts.

Each module has a directory in `official_extra_modules/`, named for the tool
or the service:

```text
official_extra_modules/
  shipped_with_mmwatchdog_version       one line: the version of the MMWatchdog release
  slack/
    module.star                 the script
    module_description.yml      the module's inputs, measurements, and examples
    README.md                   the module's page: how to set it up, and what its messages mean
  zfs_pool/
    module.star
    module_description.yml
    README.md
```

An MMWatchdog release holds `official_extra_modules/` beside the `mmwatchdog`
program, so a deployment project made from the release also holds
`official_extra_modules/`. Every deploy with the release's MMX workflows
installs the whole directory on every node, in
`/usr/local/lib/mmwatchdog/official_extra_modules`. The
[Write your own check or destination](../docs/how-to/write-your-own-check-or-destination.md) has the steps.

**To use a module, name it.** A check or an alert destination names the
module like any module of MMWatchdog, and writes the module's inputs below the name:

```yaml
# /etc/mmwatchdog/alert_destinations.yml (on a standalone node or on the MMWatchdog fleet hub)
destinations:
  team_chat:
    remind_every: never
    slack:                                                  # the official extra module for Slack
      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team  # an input of the module
    alert_severities: [info, warn, sos]
```

There is no other step. MMWatchdog loads only the modules that the configuration of
a node names, so a module that nothing names is never read and never run.
On a node, `mmwatchdog describe-module` lists every module, and
`mmwatchdog describe-module slack` prints the page of one: the `README.md` in its
directory.

A module that delivers alerts needs nothing on a node but MMWatchdog. Most
modules that check a tool run the tool's own program on the node to obtain
the check result. So the node needs the tool's program, such as `zpool` for
`zfs_pool`. The `apache_status` module sends an HTTP request directly and
needs no separate program. The page of each module says what the module
needs.

Each module is named for the tool or service. When a module checks
something of a tool, the module's name also identifies what the module checks,
such as `zfs_pool`.

**The modules and the `mmwatchdog` program belong to one release.** The file
`shipped_with_mmwatchdog_version` holds the version of the MMWatchdog release
that supplies the official extra modules.
`mmwatchdog` compares the version in `shipped_with_mmwatchdog_version` with
the program's version and refuses to run when the two versions differ.
The error message names the modules' release version and the program's
version, such as `the official extra modules in
/usr/local/lib/mmwatchdog/official_extra_modules were shipped with MMWatchdog 1.0.0, and
this mmwatchdog program is MMWatchdog 1.1.0. One MMWatchdog release holds both: deploy that release
again`. In the MMWatchdog repository, `shipped_with_mmwatchdog_version` says
`dev`, the version of a build that is no release.

**Do not edit a module in `official_extra_modules/`,** and do not copy an
official extra module into a project. The next MMWatchdog release brings
the module again.
For a module that works differently, write a module of your own under
another name, in the directory
`mmwatchdog_external_modules/` of your deployment project. The
[writing modules reference](../docs/reference/writing-modules.md) says how.
