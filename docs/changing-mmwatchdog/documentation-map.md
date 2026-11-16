# MMWatchdog documentation map

This map says where each kind of fact about MMWatchdog is written, and what else to
update when that fact changes. Read the documentation map before you change
MMWatchdog's behavior, a name that users read, or any document. The
documentation map is written for everyone who changes MMWatchdog, people and
coding agents alike.

## The rules

- **One fact, one owner.** Each kind of fact has exactly one owner document,
  listed below. Another document may summarize the fact in one sentence and
  link to the owner. A document that does not own the fact never holds a
  second full description, because two descriptions drift apart.
- **Link to the owner section.** When a document relies on a section of
  another document, it links to that section. Before you change a section
  that other documents link to, find those links with
  `git grep -n "reference/configuration-files.md#secret-files"` (use the
  file and the anchor of the section you change), and update the linking
  documents in the same change.
- **The current build only.** The guides and the module pages describe what
  MMWatchdog does now. Unbuilt design lives in the [release plan](design/README.md)
  and its design documents. The
  [compatibility rules](../../AGENTS.md#compatibility-between-releases) say what to
  delete.
- **Generated files are never edited by hand.** Each generated file's first
  line names the source files. Edit the named source files, then generate
  again.
- **Tests enforce the rules that a program can check.** The
  [section on tests](#what-the-tests-check) lists the rules enforced by
  tests.

## Where each kind of fact lives

### Entry points

- [`README.md`](../../README.md) is for users who have never heard of
  MMWatchdog: what MMWatchdog does, why to use a monitoring tool instead of
  a cron script, where MMWatchdog differs from Monit and when another tool
  fits better, one example with real output, how to install and start, the
  setup prompt and checklist for a coding agent, what to do when a check
  fails, and links to the guides.
  The table under "The Monkeymaxxing tools" is the same in the README of
  every Monkeymaxxing tool. Change the table in all of them together.
- [`AGENTS.md`](../../AGENTS.md) is for coding agents: MMWatchdog's glossary, the
  compatibility rules between releases, the reference projects and what may be taken from
  them, and links to this map and the other guides.
- [`CONTRIBUTING.md`](../../CONTRIBUTING.md) is for people outside the project:
  how a bug report, an idea, or a pull request becomes a change to
  MMWatchdog.

### Owner documents

| Document | Owns | Reader |
|---|---|---|
| [How MMWatchdog works](../how-it-works.md) | The parts of MMWatchdog, where each one runs, how they work together, delivery, heartbeats and the fleet as ideas, and the security model. | A user who wants to understand MMWatchdog, and anyone who reads MMWatchdog's code |
| [Commands reference](../reference/commands.md) | Every command and its flags, `--json` fields, exit codes, and check states. | A user |
| [Configuration files reference](../reference/configuration-files.md) | Node settings, check groups and checks, the alert destinations file, secret files, measurement history, and limits. | A user |
| [Alert format reference](../reference/alert-format.md) | The title and body of each kind of alert, the webhook request, and the fields of an event. | A user |
| [Installed files reference](../reference/installed-files.md) | Every file, account, and unit that an install puts on a node. | A user |
| Module pages, [index](../reference/modules/README.md) | Everything about one module: what it checks or delivers, its inputs and measurements, examples, how it works, its errors, where it is tested, and the reasons for the module's behavior. | A user, and an agent that writes configuration |
| [Writing modules reference](../reference/writing-modules.md) | Modules of the project's own, as a Starlark script or an executable. | A user who writes a module |
| The pages of [`how-to/`](../how-to/) | One job each: install, set up and change a fleet, update, remove, add a check, send alerts, and the rest. | A user who deploys MMWatchdog |
| [When something fails](../when-something-fails.md) | Diagnosis, from what the reader sees to the cause and the fix. | A user |
| [Release plan](design/README.md) | Product principles, the Monkeymaxxing requirements, what release 1 contains, the status of each feature, the remaining work, and the open decisions. | The maintainers, and anyone who changes MMWatchdog |
| The design documents in [`design/`](design/README.md#design-map) | The design that spans modules: check groups and checks, recovery and maintenance, alerts, the fleet, and operation. | Anyone who changes MMWatchdog |
| [Acceptance contracts](acceptance.md) | What MMWatchdog must get right, and the tests that prove it. | A reviewer |
| [Code map](code-map.md) | Where each part of MMWatchdog lives in the code: the seven packages and their import direction, the files to start from for each topic, which goroutines run, and a reading path for a first reading. | Anyone who changes MMWatchdog's code |
| [Implementation rules](implementation-rules.md) | The rules for MMWatchdog's own code, among them where a new core module's code goes, and MMWatchdog's Go dependencies with their pruned copies in `third_party/`. | Anyone who changes MMWatchdog's code |
| [Testing guide](testing.md) | The test commands, which tests run at which level, and where the suites run. The coverage index covers real-world Monit work. | Anyone who runs tests |
| [Limits on each distribution](../reference/limits-on-each-distribution.md) | For each tested distribution: what MMWatchdog cannot do or did not test there, the cause as measured with its date and source, what would change it, and what a user can do. Module pages link to it; the page [Watch for security updates](../how-to/watch-security-updates.md) links to it. | A user who runs that distribution |
| [Validation record](validation.md) | The latest test results and the beta validation with real-world Monit work. The measured size of the `mmwatchdog` program, and what is not verified. | Everyone |
| [`official_extra_modules/README.md`](../../official_extra_modules/README.md) | What the directory of the official extra modules holds, the version file, and how a deploy installs the official extra modules directory. | A user, and anyone who adds an official extra module |
| [`examples/`](../../examples/) | Example configuration files, example modules of a project's own, an example deployment project, and a whole worked example, [`web-service-fleet`](../../examples/web-service-fleet/README.md): the configuration of two machines of a web service, with a README that says why each check is there. | A user |

## Module pages

Every module has one page. A user reads a module page on the web or in
the repository. The command `mmwatchdog describe-module NAME` prints the
module page in a terminal, on a node or on the machine where the configuration
is written.

The core module pages are built into the `mmwatchdog` program because a
person needing a page at 3 AM is logged in on the node where a check fails.
MMWatchdog has no other program that could carry the core module pages.

### Where a page comes from

- **A core module** has two sources side by side, in
  `internal/checksandalerts/`:
  - its code file, `check_<module>.go` for a check module or
    `destination_<module>.go` for a destination module, holds the code and
    the module's description: the summary, each input with its type and
    notes, the measurements with their units, and one example. The module's
    line in the registry (`CoreCheckModules` in `checkmodules.go` or
    `CoreDestinationModules` in `destinationmodules.go`) names the topic
    that groups the module in the index.
  - the page source beside the module's code file, `check_<module>.md` or
    `destination_<module>.md`, holds the page's text.

  `go generate ./docs/reference/modules` combines the page source and the module's
  description into `docs/reference/modules/<module>.md` and writes the index
  `docs/reference/modules/README.md`.
- **An official extra module** keeps its page as
  `official_extra_modules/<module>/README.md`, beside its script and its
  `module_description.yml`. An official extra module's page is not
  generated.
  A deploy installs the page on every node with the module's directory.
  The command `mmwatchdog describe-module NAME` prints the installed
  `README.md` from the module's directory.
- **A module of the project's own** may keep a page as
  `mmwatchdog_external_modules/<module>/README.md`. MMWatchdog does not
  require a page for a module of the project's own.

### The sections of a module page

Every module page follows the section order in the table below. A check
module has only the sections in the "Check module" column. A destination
module has only the sections in the "Destination module" column.
Required sections are marked. Other sections appear only when the sections
have content.

| Section | Holds | Check module | Destination module |
|---|---|---|---|
| `# <module>`, the name in backticks | What the module checks or where it delivers alerts, through which program or protocol, and when to choose it over a related module. | Required | Required |
| `## Set up` | What the node needs before the module works, such as a program to install, an account, or a file of credentials, as whole YAML or commands. | When the node needs something | When the node needs something |
| `## Inputs` | Every input, its type, whether it is required, and what happens without it. | Required; generated on a core page | Required; generated on a core page |
| `## Measurements` | Every measurement, its unit, and whether a check can set a limit on it. | When the module measures; generated on a core page | Not used |
| `## When the check fails, and when it cannot check` | What makes a run fail, what makes MMWatchdog unable to check, and which incident a failed or unable-to-check run opens. The findings when a run can report several. | Required | Not used |
| `## What it sends` | What the destination sends for an alert: the title, the body, and how the service shows them. | Not used | Required |
| `## When a delivery fails` | What counts as a failed delivery, and how MMWatchdog tries again. | Not used | Required |
| `## Examples` | Whole examples. Each example says what the configuration does. Prose after each example says what happens, in order, and on which machine. A core page may give each example its own `###` heading. | Required | Required |
| `## Test the destination` | How to send a test alert, such as with `mmwatchdog send-notice`, and what to look for. | Not used | When the destination can be tested by hand |
| `## How it works` | The steps of a check run or an alert delivery. The program run, the account used, the data read, and the time limits for the run or delivery. | When needed | When needed |
| `## Errors and what they mean` | The validation errors and the messages a user meets, and what to do about each. | When needed | When needed |
| `## Tested on` | The distributions and tool versions that the module's tests run on, and where the tool cannot be installed. | Required | Required |
| `## Why it works this way` | The reasons for the module's behavior, each in one or two sentences. | Required | Required |

An official extra module's page writes its `## Inputs` as a table with the
columns "Input", "Meaning", and "When omitted", and its
`## Errors and what they mean` as a table. An official extra module's page
has two or three examples.

### How to write a page

- Follow the lab's writing rules and use the terms of MMWatchdog's glossary in
  [`AGENTS.md`](../../AGENTS.md#public-terminology).
- Write each example as a whole configuration file: a check group file that
  starts with the comment `# /etc/mmwatchdog/check_groups/<name>.yml` and the key
  `check_groups`, or an alert destinations file that starts with the comment
  `# /etc/mmwatchdog/alert_destinations.yml` and the key `destinations`. A test
  loads every such block through MMWatchdog's validation.
- Write links relative to the file you edit. The generator rewrites the
  links of a core page source for `docs/reference/modules/`.
- When the page relies on a section that several modules share, link to that
  section, and add the page to that section's list of module pages.

### How to change a page

1. Edit the page source `internal/checksandalerts/check_<module>.md` or
   `internal/checksandalerts/destination_<module>.md`, the module's description in the
   code file beside it, or both. The first line of the generated page names
   the page source and the module's code file.
2. From the repository root, run `go generate ./docs/reference/modules`. The generator
   names on standard error each core module that has no page source yet.
3. Run `go test ./docs/reference/modules/... ./internal/checksandalerts/ ./internal/config/ ./cmd/mmwatchdog/`:
   the page tests, the tests of the descriptions, the tests of the documented
   configuration, and the tests of `mmwatchdog describe-module`. Follow the
   [testing guide](testing.md) for additional tests.

## Sections that several modules share

For behavior that several modules share, the user guidance and the design
have separate owner sections:

- The pages for a user (a page of [`how-to/`](../how-to/), the [Configuration files reference](../reference/configuration-files.md), or [How MMWatchdog works](../how-it-works.md)) own what a user needs to know about the
  shared behavior.
- a design document owns the design and reasons for the shared behavior.

Before you change a shared section, find the module pages that link to it with
`git grep -n "FILE.md#ANCHOR"`, for example
`git grep -n "configuration-files.md#secret-files"`, and update them in the same
change. When you change a module, check each shared section that its page links to.

## When you change something, also update

| When you change | Also update |
|---|---|
| A core module's inputs, allowed values, measurements, or example | Update the core module's description in the module's code file, then run `go generate ./docs/reference/modules`. The tests fail until the description matches the code. |
| A core module's behavior | Update the core module's page source. If the behavior change affects shared sections linked from the module page, update the affected shared sections. If a contract changes, update the [acceptance contracts](acceptance.md). |
| Behavior that several modules share | The shared section, and every page on its list. |
| A new core module | Add the new core module's code file with the module description, registry line with the module's topic, and page source. Run `go generate ./docs/reference/modules`. Update the status table of the [release plan](design/README.md#status) and the [acceptance contracts](acceptance.md). After the new core module's tests run, update the [validation record](validation.md). |
| An official extra module | The official extra module's `README.md` and `module_description.yml`, and the status table of the [release plan](design/README.md#status). |
| A command or an option of `mmwatchdog` | The command's section in the [Commands reference](../reference/commands.md), and the [command reference](design/operation.md#command-reference). |
| A setting of `mmwatchdog_node_settings.yml`, a check group, or `alert_destinations.yml` | The section of the [Configuration files reference](../reference/configuration-files.md) that owns the changed node setting, check group, or alert destination setting. The design document that owns the design of the changed setting or check group. |
| A term of the glossary | The glossary in [`AGENTS.md`](../../AGENTS.md#public-terminology), then every document and page that uses the old term. |
| A heading that other documents link to | The links. The link test names each broken link. |
| A test result, or the size of the `mmwatchdog` program | The [validation record](validation.md). |
| A test command or the test harness | The [testing guide](testing.md). |
| A rule for MMWatchdog's own code | The [implementation rules](implementation-rules.md). |
| A file, package, or worker that moves or is added | The [code map](code-map.md). |
| A Go dependency, its copy in `third_party/`, or the Go toolchain | Update the dependency copy's `PRUNED_COPY_RECORD.md` and [Go dependencies](implementation-rules.md#go-dependencies) in the implementation rules. |
| How MMWatchdog's documentation is organized | This map, and the lines in `AGENTS.md` and `README.md` that point to it. |

## What the tests check

The Go tests in `docs/reference/modules/generator/` (`go test ./docs/reference/modules/...`)
fail when:

- a core module has any of the following page problems:
  - The core module has no page source.
  - The page source does not start with ``# `<module>` ``.
  - The page source has a section not listed for a check module or
    destination module, as applicable, in the module page section table.
  - The page source has a section that the generator writes.
  - The page source lacks a required section.
  - The page source has sections out of the order in the module page section table.
- an official extra module's `README.md` has any of the following problems:
  - The `README.md` does not start with ``# `<module>` ``.
  - The `README.md` has a section not listed for a check module or
    destination module, as applicable, in the module page section table.
  - The `README.md` lacks a required section.
  - The `README.md` has sections out of the order in the module page section table.
  - The `## Inputs` table does not name exactly the inputs in the official
    extra module's `module_description.yml`.
- a generated file in `docs/reference/modules/` differs from what `go generate
  ./docs/reference/modules` writes now;
- a relative link in a Markdown file of the repository leads to a file or a
  heading that does not exist;
- a package other than `cmd/mmwatchdog` imports `docs/reference/modules`, the package that
  holds the page text;
- a YAML or JSON name in the code, or an input or measurement of a module,
  breaks the [env, config, and dir table](design/README.md#env-config-and-dir-have-one-spelling).

The test `TestDocumentedConfigurationIsValid` in `internal/config/` loads every
configuration block of the guides, the README, the page sources, and the
pages of the official extra modules through MMWatchdog's validation. The
[module description tests](implementation-rules.md#one-place-for-each-module)
in `internal/checksandalerts/` check each core module's description against its code, and
that its registry line names one of the topics in the index.
