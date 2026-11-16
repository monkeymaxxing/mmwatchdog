# MMWatchdog documentation

Start with [Get started](get-started.md), the path through your first hour.

## Do a job

| Page | Read it when you want to |
|---|---|
| [Install MMWatchdog on one node](how-to/install-on-one-node.md) | Install MMWatchdog on a server with MMX, and prove that an alert reaches you |
| [Add a check](how-to/add-a-check.md) | Watch one more service, page, port, disk, or database on a node |
| [See what is happening](how-to/see-what-is-happening.md) | Read the state of each check, what happened and when, and a fresh run |
| [Decide when a check alerts](how-to/decide-when-a-check-alerts.md) | Choose how soon a failure alerts you, how serious the alert is, and when the problem counts as over |
| [Restart a service automatically](how-to/restart-a-service-automatically.md) | Restart a service, or run a command of your own, when a check fails |
| [Send alerts where your team sees them](how-to/send-alerts-somewhere.md) | Add Slack, email, a webhook, or another destination, and choose which alerts each one gets |
| [Pause alerts during planned work](how-to/pause-alerts-during-planned-work.md) | Do planned work without alerts, and without forgetting to turn them back on |
| [Report from a scheduled job](how-to/report-from-a-scheduled-job.md) | Get an alert when a backup or another scheduled job stops succeeding |
| [Stop alerts that follow another failure](how-to/stop-alerts-that-follow-another-failure.md) | Get one alert, not many, when one failure, such as a database, breaks other checks |
| [Look back at measurements](how-to/look-back-at-measurements.md) | See how memory, CPU, disk, or another number changed over time |
| [Watch for security updates](how-to/watch-security-updates.md) | Get an alert when security updates wait, a known exploited flaw affects the node, or it needs a reboot |
| [Set up a fleet](how-to/set-up-a-fleet.md) | Make one server the hub, join the others, and see them all in one place |
| [Watch a fleet](how-to/watch-a-fleet.md) | See the whole fleet, one node's checks, and which node went silent |
| [Change a fleet](how-to/change-a-fleet.md) | Add or forget a node, use node groups, rotate the join code, or move the hub |
| [Update MMWatchdog](how-to/update-mmwatchdog.md) | Move a node to a new release and keep its checks, destinations, vault, and history |
| [Remove MMWatchdog](how-to/remove-mmwatchdog.md) | Remove MMWatchdog from a node completely, and prove nothing is left |
| [Write your own check or destination](how-to/write-your-own-check-or-destination.md) | Check something no module covers, or send alerts to a service no module serves |

## Look something up

| Page | Read it when you want to |
|---|---|
| [Commands](reference/commands.md) | Look up a command, its flags, its `--json` fields, its exit codes, and what each check state means |
| [Configuration files](reference/configuration-files.md) | Look up a setting, its default, a built-in limit and its reason, or a validation error |
| [Alert format](reference/alert-format.md) | See the title and body of each kind of alert, the webhook's JSON, and the fields of an event |
| [Writing modules](reference/writing-modules.md) | Look up the module description, the primitives, findings, state, and the protocol of an executable module |
| [Installed files](reference/installed-files.md) | See every file, account, and unit that an install puts on a node, and what each is for |
| [Module pages](reference/modules/README.md) | Look up a module: what it checks or where it delivers, its inputs, examples, and errors |
| [Limits on each distribution](reference/limits-on-each-distribution.md) | See what MMWatchdog cannot do on each tested distribution, and why |

## Understand and fix

| Page | Read it when you want to |
|---|---|
| [How MMWatchdog works](how-it-works.md) | Understand why MMWatchdog behaves as it does: checks, alerts, delivery, the fleet, and the security model |
| [When something fails](when-something-fails.md) | Start from what you see, such as an alert that did not arrive, and find the cause and the fix |

## Change MMWatchdog itself

[docs/changing-mmwatchdog/](changing-mmwatchdog/) holds the release plan, how to
[release MMWatchdog](changing-mmwatchdog/releasing.md), the design, the testing guide, the validation record, the code map, the implementation rules,
the acceptance contracts, and the documentation map.
