# Contributing to MMWatchdog

Bug reports, ideas, and pull requests are welcome. `CONTRIBUTING.md` explains
how bug reports, ideas, and pull requests become changes to MMWatchdog.

## Reporting a bug or asking for a feature

Open an issue on GitHub. For a bug, include:

- the output of `mmwatchdog version` on the node;
- the node's Linux distribution and version, and whether the node is a
  standalone node, a joined node, or the fleet hub;
- the smallest configuration that shows the problem: the check group files,
  `mmwatchdog_node_settings.yml`, and `alert_destinations.yml`, with every
  credential removed;
- what you expected, and what MMWatchdog did, with the output of
  `mmwatchdog status --json` and of `mmwatchdog events --since TIME --json`
  for the time of the problem.

For a feature, start with the problem: what you are trying to do.
Describe how you perform the task today, for example with Monit. The
[release plan](docs/changing-mmwatchdog/design/README.md) shows what MMWatchdog intends to do
and what remains. Maintainers use the
[product principles](docs/changing-mmwatchdog/design/README.md#product-principles) to decide
how to weigh a proposal. A check for one tool can often be a module of
your own ([writing modules reference](docs/reference/writing-modules.md)); the design says
[where a module belongs](docs/changing-mmwatchdog/design/checks.md#where-a-module-belongs).

## A pull request is a proposal

MMWatchdog's maintainers write every change that lands in MMWatchdog.
MMWatchdog's maintainers do not merge code from outside the project.
Because MMWatchdog runs as root on your servers and reports server failures,
the origin of every line of MMWatchdog's code is known.
No code arrives from a source whose license, authorship, or tools the project
cannot check.

So a pull request is read as a proposal: a precise description of a change,
with code that shows it. When a maintainer accepts a proposal, the
maintainer writes the change, with its tests and documentation, and credits
you in the commit message:

```text
Proposed by @your-github-name in #123
```

The pull request is then closed with a link to the maintainer's commit
implementing the accepted proposal. A proposal that is not accepted is
closed with the reason.

A proposal is easiest to accept when it:

- changes one thing, and says why;
- includes a test that fails without the change, where one is possible;
- uses the terms of the [glossary](AGENTS.md#public-terminology) and follows
  the [rules for names that users read](docs/changing-mmwatchdog/design/README.md#ai-authors-human-readers).

## Where the rules for changes live

| Document | What the document gives you |
|---|---|
| [`AGENTS.md`](AGENTS.md) | MMWatchdog's glossary and the compatibility rules between releases, for people and coding agents alike |
| [Documentation map](docs/changing-mmwatchdog/documentation-map.md) | Which document owns each fact, and what else to update when something changes |
| [Acceptance contracts](docs/changing-mmwatchdog/acceptance.md) | What MMWatchdog must get right, and the tests that prove MMWatchdog meets the requirements |
| [Implementation rules](docs/changing-mmwatchdog/implementation-rules.md) | The rules for MMWatchdog's own code |
| [Testing guide](docs/changing-mmwatchdog/testing.md) | How to run the tests, and which tests a change needs |

## License

MMWatchdog is licensed under the [Apache License 2.0](LICENSE). Under section
5 of the Apache License 2.0, issue and pull request submissions are offered
under the Apache License 2.0.
