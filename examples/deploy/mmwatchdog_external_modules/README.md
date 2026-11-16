# MMWatchdog modules of this project

The deployment project's `mmwatchdog_external_modules/` directory holds the
MMWatchdog modules that you write. Keep each module in a directory with the
module's name.

```text
mmwatchdog_external_modules/
  queued_jobs/
    module.star                 the script
    module_description.yml      the module's inputs, measurements, and examples
```

A module is a Starlark script, `module.star`. If a module needs something
Starlark cannot provide, use an executable in `module_executable` instead.
Every module has a `module_description.yml`. The
[writing modules reference](../../../docs/reference/writing-modules.md) explains how to write
Starlark modules, executable modules, and module descriptions.

The official extra modules, which the MMWatchdog project writes, are not in
`mmwatchdog_external_modules/`. The official extra modules are in
`official_extra_modules/`, beside `mmwatchdog_external_modules/` in the
deployment project, and come with the MMWatchdog release.

Each deploy installs every module from `mmwatchdog_external_modules/` on the nodes.
The installed modules go in `/usr/local/lib/mmwatchdog/external_modules`.
The node's `/usr/local/lib/mmwatchdog/external_modules` directory then
contains exactly the modules from the deployment project's
`mmwatchdog_external_modules/`. If you remove a module from
`mmwatchdog_external_modules/`, the next deploy removes the module from the
nodes' `/usr/local/lib/mmwatchdog/external_modules` directories. A check or
an alert destination names a module by the name of its directory.

A module in `mmwatchdog_external_modules/` cannot have the name of a core
module or of an official extra module. A name means one module on
every node, so a conflicting name stops deployment, which reports the
module's name.

Nobody edits an official extra module. For a module that works differently,
write a module of your own in `mmwatchdog_external_modules/` under another name.

Keep `mmwatchdog_external_modules/` even when your deployment project has no
module of your own. The shipped `mmwatchdog_external_modules/README.md` is
enough. The workflows copy the contents of `mmwatchdog_external_modules/`
exactly to staging.
MMX refuses a missing source directory before connecting to any node,
so a missing directory cannot erase installed modules.
