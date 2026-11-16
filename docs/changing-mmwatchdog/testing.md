# Testing MMWatchdog

The [test environment rules](#test-environment-rules) define isolation and
ownership.

The MMWatchdog testing guide specifies the test commands and describes what
MMWatchdog's tests cover.

The [acceptance contracts](acceptance.md) state the outcomes MMWatchdog must
achieve. Two tables map each contract to the Go tests and Linux workflow
suites that check the contract:
[standalone](acceptance.md#standalone-acceptance-checks) and
[fleet](acceptance.md#fleet-checks).

## Commands

Use the checks relevant to the change. Routine validation uses short fixture
timings and waits for observed results. Passing routine validation does not
require a fixed soak duration. State-transition unit tests advance timestamps
directly.

```sh
go test ./...
go test -race ./...
go vet ./...
python3 -m py_compile tests/integration/*.py scripts/release.py
python3 -m unittest discover -s tests/integration -p 'test_*.py' -v
./scripts/build.sh
```

`./scripts/build.sh` builds `mmwatchdog` for this machine, and
`mmwatchdog-linux-amd64` and `mmwatchdog-linux-arm64`, without cgo and
without symbol tables, into `$MMWATCHDOG_TEST_ROOT/bin`
(`~/.cache/mmwatchdog-testing/bin` by default).

### Test in levels

Test in levels, so that work goes fast and still ends proven. The goal is
to test, not to wait: run the smallest set that can show whether the change
works, and run a larger set only when it can find something that the smaller
set cannot. MMX also runs a larger test set only when smaller tests cannot
establish correctness.

- **While iterating**, run unit tests for the code that the change touched,
  on the controller machine. For Go packages, run `go test`. Nothing else. Each package of
  `internal/` keeps its Go tests beside its code
  ([code map](code-map.md)): the tests of a
  check module are in `./internal/checksandalerts`, and the tests of the
  store in `./internal/store`. The tests of `./internal/daemon` start the
  node monitoring process and test how the packages work together. The two
  largest packages, `checksandalerts` and `daemon`, take about 15 seconds
  each on a Mac, because most of their tests run in parallel
  ([fast Go tests](#fast-go-tests)). While you iterate on one part, run the
  tests of that part by name, and run the touched packages before a commit:

  ```sh
  go test ./internal/checksandalerts -run 'TestLogLines|TestFileCheck'
  go test ./internal/checksandalerts ./internal/daemon
  ```

- **After an ordinary change**, level 1: run only the VM suites that the
  change touches, on Debian 13 and Ubuntu 26.04: the two reference
  distributions. The table in
  [Evidence and coverage](#evidence-and-coverage) names the scenario file,
  and with it the suite, of each contract:

  ```sh
  python3 tests/integration/incus.py --distribution debian13 --distribution ubuntu2604 \
    --suite node --suite services
  ```

- **Once at the end of each build step or stage**, before its report and
  its push, run the step's new and changed suites on
  [every distribution](#every-target-distribution): the command with
  `--suite` and without `--distribution`. Whoever leads the step runs the
  new and changed suites on every distribution once. A person or agent who
  builds one part of the step stays at level 1. Nobody repeats the run of
  new and changed suites on every distribution after each fix.

  ```sh
  python3 tests/integration/incus.py --suite node --suite services
  ```

- **When a change touches code of one family of distributions**, such as
  apt's code in `apt_security_updates`, also run the touched suites on one
  distribution of that family. Debian 13 and Ubuntu 26.04 cover the apt
  family; a family outside apt adds one of its own: `rocky9` (Rocky Linux 9)
  for the Red Hat family, `opensuseleap161` for SUSE, and `archlinux`. For
  example, a change to `dnf_security_updates` or to the Red Hat part of
  `reboot_after_updates` runs the node suite on Debian 13, Ubuntu 26.04, and
  Rocky Linux 9. The Red Hat family has two versions of dnf: a change to the
  code that reads dnf 5 also runs the node suite on `fedora45` (Fedora 45).
- **When the step changed code that every suite uses**, also run every
  suite on Debian 13 and Ubuntu 26.04, once: the command with the two
  `--distribution` options and without `--suite`.
- **Every suite on every distribution**, the command without options, runs
  only before a release and after a change to the test harness or to the
  guests' base images: the prepared operating-system images from which the
  harness creates guests.
- **On ARM64**, three suites run on Debian 13 and Ubuntu 26.04, on
  Hetzner's ARM64 servers (`cax11`), only before a release and after a
  change to the harness's ARM64 path, such as the ARM64 images or server
  type in `hetzner.py`. MMWatchdog is Go without C. MMWatchdog's Go code runs
  on AMD64 and ARM64. What the architecture can change is the build, which
  `native` covers by running every Go test on the ARM64 guest; the bundle
  that MMX installs, which `deployment` covers; and what the kernel and
  the hardware tell the node's checks, which `node` covers. Suites besides
  `native`, `deployment`, and `node` would repeat the MMWatchdog code paths
  already tested on AMD64. Running `services` on ARM64 would also exercise
  third-party service servers rather than additional architecture-specific
  MMWatchdog behavior. Everyday runs, at every level above, stay on Incus VMs
  on the AMD64 test servers, where an ARM64 guest would run emulated, many
  times slower. See
  [Hetzner Cloud servers](#hetzner-cloud-servers):

  ```sh
  python3 tests/integration/hetzner.py --server-architecture arm64 \
    --distribution debian13 --distribution ubuntu2604 \
    --suite native --suite deployment --suite node
  ```

- **When a run fails in one suite**, find the cause in its logs, fix it,
  and rerun only the failed suite on the distribution where the suite failed.
  Run a larger set again
  only when the fix changed something that the larger set covers. A
  failure that a rerun does not show is still a fault: see
  [tests that never depend on the machine's speed](#tests-that-never-depend-on-the-machines-speed).
- **When a change adds or changes a Go test that waits for something,** or
  code that runs in goroutines or on timers, run those tests under stress
  before the commit: `tests/run_go_tests_under_stress.sh --tests-matching
  'TestA|TestB'`, about a minute.
- **Once at the end of a larger rework, and before a release,** run every Go
  test under stress: `tests/run_go_tests_under_stress.sh`, about 18 minutes
  on a Mac. `tests/run_go_tests_under_stress.sh` runs every Go test with the
  race detector, so the full stress run takes the place of
  `go test -race ./...`.
- **A commit that changes only documentation** runs no VM suite and creates
  no cloud guest.
- **When a suite or a run is slow**, find where the time goes before running
  more, and remove waits and repeats that are not the subject of a test.

### Fuzz tests for a release

Fuzzing is a [release checklist](design/README.md#release-checklist) step.
Ordinary changes run the saved seed cases with their package's Go tests;
they do not run a fuzzing campaign.
Before a release, run each command below on a Linux test server, from the
repository root. Each command fuzzes one external input for 10 minutes.
The tests use local files and in-memory HTTP requests, with no network.

| Fuzz test | Input and properties |
| --- | --- |
| `FuzzCheckGroupConfigurationFile` | Raw check group YAML through configuration loading; repeatable acceptance and validated values. |
| `FuzzNodeSettingsFile` | Raw node settings YAML through configuration loading; repeatable acceptance and validated values. |
| `FuzzModuleDescription` | A non-core module's description file; repeatable parsing and valid inputs, measurements, and examples. |
| `FuzzExecutableModuleOutput` | An executable's check and destination JSON results; strict decoding and round trips, plus check-result validation. |
| `FuzzStarlarkModuleResult` | The returned values of run_check and deliver_alert. Conversion must produce JSON stating module-result protocol version 1, and invalid result values must be rejected. |
| `FuzzJoinRequestOnTheHub` | A join request through the hub's HTTP handler; the 64 KiB body limit and valid joined-node settings. |
| `FuzzNodeStatusReportOnTheHub` | A node status report through the hub's HTTP handler; the 4 MiB body limit, strict decoding, and report validation. |
| `FuzzMMHotlineReceipt` | Receipt signature headers and response bodies; the 64 KiB body limit and binding to the body, route, HTTP status code, and request. |

```sh
go test -run '^$' -fuzz '^FuzzCheckGroupConfigurationFile$' -fuzztime 10m ./internal/config
go test -run '^$' -fuzz '^FuzzNodeSettingsFile$' -fuzztime 10m ./internal/config
go test -run '^$' -fuzz '^FuzzModuleDescription$' -fuzztime 10m ./internal/checksandalerts
go test -run '^$' -fuzz '^FuzzExecutableModuleOutput$' -fuzztime 10m ./internal/checksandalerts
go test -run '^$' -fuzz '^FuzzStarlarkModuleResult$' -fuzztime 10m ./internal/checksandalerts
go test -run '^$' -fuzz '^FuzzJoinRequestOnTheHub$' -fuzztime 10m ./internal/daemon
go test -run '^$' -fuzz '^FuzzNodeStatusReportOnTheHub$' -fuzztime 10m ./internal/daemon
go test -run '^$' -fuzz '^FuzzMMHotlineReceipt$' -fuzztime 10m ./internal/checksandalerts
```

Fix each product fault that fuzzing finds, and keep Go's saved input under
`testdata/fuzz/<test-name>/`. The package's ordinary tests then run the saved
input that exposed the product fault on every test run. If the test's
property was wrong, correct the property.
A fuzzing failure must be understood before the release proceeds.

### Real services before a release

The Go tests in `tests/official_extra_modules` test the alert modules
against stand-ins for the services, on loopback. Before a release, one more
Go test sends real alerts through the official extra modules
[`slack`](../../official_extra_modules/slack/README.md),
[`telegram`](../../official_extra_modules/telegram/README.md),
[`discord`](../../official_extra_modules/discord/README.md), and
[`pushover`](../../official_extra_modules/pushover/README.md) to the real
services, with real keys. Running the real-service tests is a
[release checklist](design/README.md#release-checklist) step. Run the
real-service tests by hand on the machine where the keys are, such as a Mac.
Keeping the run on the machine with the keys keeps secrets off shared test
servers.

Each real-service test reads keys from environment variables and skips when
the required variables are not set. Without the service variables,
`go test ./...` sends no real alerts:

| Test | Environment variables |
| --- | --- |
| `TestRealServiceSlack` | `MMWATCHDOG_REAL_SERVICE_TEST_SLACK_WEBHOOK_URL`: the address of an incoming webhook for a test channel |
| `TestRealServiceTelegram` | `MMWATCHDOG_REAL_SERVICE_TEST_TELEGRAM_BOT_TOKEN` and `MMWATCHDOG_REAL_SERVICE_TEST_TELEGRAM_CHAT_ID`: a bot and a test chat that the bot is a member of |
| `TestRealServiceDiscord` | `MMWATCHDOG_REAL_SERVICE_TEST_DISCORD_WEBHOOK_URL`: the address of a webhook for a test channel |
| `TestRealServicePushover` | `MMWATCHDOG_REAL_SERVICE_TEST_PUSHOVER_APPLICATION_TOKEN` and `MMWATCHDOG_REAL_SERVICE_TEST_PUSHOVER_USER_OR_GROUP_KEY`: a test application and the user or delivery group that gets its messages |

Set the variables, then run the tests from the repository root:

```sh
go test ./tests/official_extra_modules -run '^TestRealService' -count=1 -v
```

Each test does the following against its service:

- Each real-service test verifies the destination with the right keys and
  with a wrong key, as MMWatchdog does at a reload. Verification with valid
  keys and verification with an invalid key send no messages.
- Each real-service test attempts one delivery with a wrong key and checks
  that the module's message gives the advice for the wrong key.
- Each real-service test delivers a failure alert with a check's output,
  a recovery alert for the failed check, and a long alert. The module cuts
  the long alert to the service's limit. The failure, recovery, and long
  alerts must all be delivered.
- Telegram and Discord answer a delivered message with the message as they
  stored it. The test checks that the stored text is the text that the
  module sent. On Discord, a mention in the input `mention` must notify,
  and `@everyone` in a check's output must not.
- Pushover gets two emergency messages of one check, as two nodes of a
  fleet send the messages, and then the closing alert, which stops repeats
  by tag. The test reads Pushover's receipts. The two emergency messages
  must repeat before the closing alert stops repeats, and neither message
  may repeat after the closing alert. A closing alert of a tag that no
  message has must be delivered too.

The deliveries of `telegram`, `discord`, and `pushover` go through a proxy
on loopback inside the test. The proxy passes each request unchanged to the
real service and keeps the answer, because only the answer shows the stored
message or Pushover's receipt. The verifications and the requests with a
wrong key go straight to the services.

Two parts need a person:

- **Slack answers only `ok`.** Look at the Slack test channel once. The test
  prints what to look for: the first line in plain text with the mention,
  the check's output in a code block with every character shown as it is,
  and the long message cut at its end with `…`.
- **Pushover's emergency messages sound** on every device of the user or
  group, also on a muted phone set up as the
  [`pushover` guide](../../official_extra_modules/pushover/README.md#make-an-alert-wake-the-person)
  says, for about a minute, until the test stops them. Tell the people in
  the group before the run. Nobody acknowledges Pushover's emergency
  messages during the run. An acknowledgement stops repeats, so the test
  could no longer prove that cancellation by tag stops repeats.

The test prints each service's answers without the keys. Write what changed
since the last run into the [validation record](validation.md).

**MMHotline.** One more real-service test, `TestRealServiceMMHotline` in
`internal/checksandalerts`, sends to a real MMHotline through the core
module [`mmhotline`](../reference/modules/mmhotline.md). Use a separate MMHotline instance
for `TestRealServiceMMHotline`. Add an alert source made for the test.
`TestRealServiceMMHotline` sends check-ins from the test's alert source.
MMHotline warns when check-ins stop, by default 2 minutes after the last
check-in.

As soon as `TestRealServiceMMHotline` finishes, remove the test's alert
source from MMHotline's settings file. Then run
`sudo mmhotline settings apply`.

| Environment variable | What the environment variable holds |
| --- | --- |
| `MMWATCHDOG_REAL_SERVICE_TEST_MMHOTLINE_URL` | MMHotline's address, such as `http://127.0.0.1:8484` |
| `MMWATCHDOG_REAL_SERVICE_TEST_MMHOTLINE_ALERT_SOURCE_ID` | The test alert source's ID |
| `MMWATCHDOG_REAL_SERVICE_TEST_MMHOTLINE_SIGNING_SECRET` | The test alert source's signing secret, as 64 hexadecimal characters |

```sh
go test ./internal/checksandalerts -run '^TestRealServiceMMHotline$' -count=1 -v
```

`TestRealServiceMMHotline` performs the following actions in order:

1. Send an `info` alert, built as MMWatchdog queues a notice, and check the
   signed receipt.
2. Send the `info` alert again. Check that MMHotline answers
   `already_received` with the alert ID returned for the first delivery.
3. Send a check-in and check the check-in's receipt.
4. Send an `sos` alert, built as a standalone node queues a failure.
   Check that MMHotline opened an incident for the `sos` alert.
5. Send a check-in every 30 seconds for two minutes, as a standalone node
   does.
6. Send the `resolved-sos`. Check that MMHotline joined the resolution to
   the incident opened for the `sos` alert.
7. Send an alert without a title and check the signed `400`.
8. Send a request with a wrong signing secret and check the `401` message.

`TestRealServiceMMHotline` prints the alert ID and incident ID returned by
MMHotline, so the alert and incident can be found on MMHotline's pages.

The test's SOS wakes the person on call in the separate test MMHotline
instance. Put one person on call in the test MMHotline instance, with a
push app. Tell the on-call person before the run. The phone rings at once and again until the person
acknowledges the SOS, which assigns responsibility for the incident to the
person. The `resolved-sos` stops repeats, and the person gets a quiet
message that the SOS is resolved.

### Fast Go tests

The Go tests run in parallel and wait for observed results, not for fixed
times. The VM suite `native` runs the Go tests in every Linux guest.
Saving a second in the local Go test run also saves a second on every
distribution. Follow the rules below to keep the Go tests fast:

- **A test calls `t.Parallel()`** unless it changes something that the whole
  test process shares: a package-level variable, the environment of the
  process, or the standard logger. Such a test stays sequential, and every
  sequential test adds its whole time to the run of its package. About 50
  tests are sequential, mostly in `./internal/checksandalerts` and
  `./internal/daemon`. Sequential tests include the following categories:
  - Tests that substitute files for kernel files, using
    `testhelpers.KernelFiles`, or for node files, using `nodeFiles`.
  - Tests that run programs as the test's own account, using
    `asThisAccount`.
  - Tests that replace the boot ID, using `setBoot`.
  - Tests of the environment that MMWatchdog passes on.
- **A stand-in program is a link to one launcher script.** Use
  `testhelpers.WriteStandInProgram` when a test needs a stand-in for a node's
  program, such as `systemctl`. Use the helper for a test module's
  `module_executable` too (`testhelpers.InstallModule` calls the helper).
  macOS checks every new executable file at its first start, one file at a
  time: up to half a second for each file, and seconds on a busy machine.
  Eighty new files cost about 16 seconds whatever runs in parallel, and a
  stand-in delayed by macOS's first-start check can run past a check's time
  limit.
  A link to a file that has started before starts at once. Never write to a
  stand-in's path or change its mode, because that changes the launcher of
  every stand-in; `testhelpers.WriteNotExecutableFile` replaces a stand-in
  with a file that no account may run. The launchers come from
  `testhelpers.RunTests`, which the `TestMain` of each package whose tests
  start a module or a stand-in calls (`main_test.go`).
- **A test hands its stand-ins to MMWatchdog through a context, not through
  `PATH`.** `testhelpers.PutProgramsFirst(t, directory)` puts the directory
  first on the `PATH` of the programs that MMWatchdog starts under
  `testhelpers.TestContext(t)`. Pass the context returned by
  `testhelpers.TestContext(t)` to the daemon, check, or command that starts
  the stand-in programs.
  `t.Setenv` makes a test sequential; use it only in a test of the
  environment that MMWatchdog passes on.
- **Waits that no test studies are short in tests.** The `TestMain` of
  `./internal/daemon` shortens how often the notifications worker reads its
  queue. A test of a run that does not end uses
  `waitingForARunToEnd` to set how long MMWatchdog waits after the run's time
  limit expires. A test of the hub's alert grouping sets
  `hub.alertGroups.window` before the hub's notifications start.
- **Margins of real time are wide.** A run's time limit is far longer than a
  stand-in takes on a busy machine, unless the time limit is what the test
  studies. A port or a Unix socket that must refuse connections comes from
  `testhelpers.AddressWhereNothingListens` or `leaveUnixSocketFile`, never
  from a
  listener that the test closed: a process that another test starts at that
  moment holds a copy of the listener until it starts its program.

### Tests that never depend on the machine's speed

A test passes on a loaded machine as it does on an idle one: on a busy
laptop, in a guest during a run of every suite, and beside other tests. A
test that passes on an idle machine and fails once in a while on a loaded
one is broken, also when a rerun passes. Such a failure looks like a fault
of MMWatchdog, costs an investigation each time, and teaches people to ignore a
failed run.

- **Wait for the event, not for a time.** A test that expects something to
  happen polls for the event with a deadline far above the normal time.
  `testhelpers.WaitFor` and the other waits of the Go tests wait up to
  `testhelpers.LongestWaitForAnEvent`, a minute; they and the waits of the VM suites return as soon as the event
  happens, so a long deadline costs nothing. A fixed sleep followed by an
  assertion that something happened is a fault.
- **The test controls every time span that its outcome depends on.** When
  the outcome depends on a window, an interval, or a delay of MMWatchdog itself,
  such as the hub's alert grouping window, the test holds the span open
  until its own work is done and then ends it, or it sets the span far
  above what that work takes. `testhelpers.NewTimeLimitThatTheTestEnds` is
  such a time limit for a run: the time limit expires only when the test
  ends the time limit. For example, the test can expire the time limit once a
  stand-in program has written its process number
  (`testhelpers.StartedProgram`). A short real span that the test's own
  work must fit into is a fault, and so is an exact count of periodic events
  within a real time.
- **An upper limit on elapsed time is wide.** The elapsed-time limit proves
  only that something ended instead of hanging or waiting for the
  operation's default timeout or interval. Set the limit at least ten times
  the normal duration and at least 3 seconds. Keep the limit shorter than
  the hanging operation or unintended default wait would take.
- **A test of a daemon wait unaffected by wall-clock adjustments uses a
  test-controlled clock.**
  `testhelpers.Clock` stands still until the
  test lets time pass with `Pass`, and `Jump` steps its wall clock forward
  or backward while its monotonic clock does not move, as a time service
  steps a node's clock. The test starts the daemon with
  `newDaemon(path, clock.Now)`, or a hub or a joined node with
  `startHubOnClock` or `joinedNodeOnClock`, and the jump tests in
  `clockjump_test.go` show the pattern. Go's public API cannot build a time
  whose wall reading jumps while its monotonic reading runs on, so the clock
  writes the time's two readings with `unsafe`; `TestClock` fails when Go
  changes the layout of a time. Only tests use `testhelpers.Clock`, including
  the clock's `unsafe` operations.
- **A wait that proves that something did not happen** may be a fixed
  sleep. A slow machine cannot make the absence assertion fail, but can
  delay the unexpected event until after the assertion. Keep the fixed
  sleep short. A lower limit on elapsed time is fine too, because real time
  never runs faster.
- **Prove the tests under stress.**
  `tests/run_go_tests_under_stress.sh` builds
  the Go tests with the race detector and runs them with Go's own `stress`
  tool (`golang.org/x/tools/cmd/stress`): 40 runs, 4 at once, which loads
  the machine. `tests/run_go_tests_under_stress.sh` lists each failed test
  and keeps the whole output of each failed run. Run the tests that a change
  adds or changes by name, and every test before a release:

  ```sh
  tests/run_go_tests_under_stress.sh --tests-matching 'TestRemindersGoThroughTheHub|TestSQLiteDatabaseCheck'
  tests/run_go_tests_under_stress.sh
  ```

- **A test that failed once is fixed at once.** Reproduce the failure with
  the stress command above, find the cause, and fix the cause by the rules
  for tests that never depend on the machine's speed. Prove the fix with
  the stress command. Never add a
  retry of a failed assertion, a skip on a slow machine, or a longer sleep.
  The rule also applies to a scenario of a VM suite. The harness keeps the
  guest journals of a failed suite. Use the saved guest journals to find
  the cause.

### Where the suites run

The VM suites run only on disposable guests that the harness creates for
each run, never on the machine where you start the suites. A job slot is a
worker that runs suites sequentially in a separate test environment, with
two guests: `mmwatchdog-dev` and `mmwatchdog-peer`. Each guest pair uses
Incus VMs or Hetzner Cloud servers. The suites run unchanged on Incus VMs
and Hetzner Cloud servers:

- **Incus VMs on a Linux machine with KVM**
  ([`incus.py`](#every-target-distribution)): the fastest way, for every
  target distribution at once. Test servers run suites in Incus VMs, and
  every ordinary run uses Incus VMs. The guests have the architecture of the
  test server, AMD64.
- **Hetzner Cloud servers** ([`hetzner.py`](#hetzner-cloud-servers)), AMD64
  or ARM64: the way to test on ARM64, and the way for a contributor without
  a Linux machine with KVM.

For each job slot, `incus.py` and `hetzner.py` write the file `remote.json`
into the slot's test environment. `remote.json` names the slot's two guests
with addresses, SSH logins, and SSH host keys, and records the guests'
distribution and processor architecture. `vm.py`, which runs every suite,
reaches the guests through `remote.json` and checks the architecture on each
guest. The suites build `mmwatchdog` and the native Go test programs for the
guests' processor architecture. MMX installs a runner built for the guests'
processor architecture. A suite started in a test environment
without `remote.json` refuses to run and names the scripts that create
guests.

A guest's clock must not jump during a suite unless a scenario changes the
clock deliberately. A guest boots with the time of the emulated hardware
clock, which counts whole seconds and can be about two seconds behind.
At the first synchronization, the guest's time service would jump the
guest's clock to the correct time. On Ubuntu and Debian, synchronization
occurs about half a minute after boot, when a suite can already be running.

MMWatchdog times waits on the monotonic clock, so a wall-clock jump changes
none of MMWatchdog's waits
([the system clock](design/operation.md#the-system-clock)). But MMWatchdog's
recorded and displayed times would jump. A suite would then observe a clock
jump outside the suite's control.

To prevent unintended clock jumps, `vm.py` sets an unsynchronized guest's
clock to the time of the machine running `vm.py`. The adjustment happens
before a suite uses the guest (`vm.py up`) and after the guest reboots
during a suite. The guest's time service then finds the clock correct to a
few milliseconds and adjusts the clock gradually, without a jump.

The clock-jump scenarios in `acceptance` and `fleet` change the guest's
clock with `vm.set_clock`. The scenarios also restore the guest's clock
with `vm.set_clock`, even when the guest's clock has already synchronized.

The catalog of suites is `SUITES` in [`all.py`](../../tests/integration/all.py),
ordered by expected execution duration, longest first. The set covers
[the web service fleet example](#the-web-service-fleet-example), fleet,
deployment, diagnostics, native Go tests, resources (including lifecycle),
the node's own checks, the checks of common services, acceptance, and short
soak. Each suite prepares its own fixture; soak installs the daemon first. `--suite` on
`incus.py` and `hetzner.py` runs only the named suite and can be repeated.
In a test environment of the [cloud helper](#hetzner-cloud-servers),
`vm.py test` runs all acceptance modules and native Go tests together. For
a focused independent group, use `vm.py test --group` with `acceptance`,
`diagnostics`, `resources`, `node`, `services`, `fleet`, or `native`.
Use `vm.py soak --prepare` to install the soak fixture when no earlier suite
has installed the fixture.

Each distribution's suites go to its job slots. Each job slot has its own
test environment, `$MMWATCHDOG_TEST_ROOT/<distribution>/slots/<n>`, with its builds
and `.test.lock`. A slot takes the next suite until none is left: suites in
one slot run one after the other, and separate slots run at the same time.
After a suite that leaves its guests unfit for the suites after it, the
slot deletes its guests and takes new ones for its next suite.
`SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_GUESTS` in `all.py` names the suites
that require replacement guests, each with a reason: today the web service
fleet example. The shared suite runner in `all.py`, called by `incus.py`
and `hetzner.py`, prints each suite's result and duration and a final wall
time. `incus.py` and `hetzner.py` exit nonzero when a suite failed or when
no slot could run a suite.
Full output is in `<slot>/<suite>.log`, overwritten on the next run;
failures print the path and last 30 lines. In a suite's log, each
scenario's line starts with the seconds that the scenario took, and each
step of the suite's setup has a line that starts with `SETUP` and its
seconds: the steps before the first scenario, and a step between scenarios
such as the installation of the servers that the services suite checks.

When a suite fails, the runner saves the systemd journal of each of the
slot's two guests, from the suite's start on, beside the suite's log:
`<slot>/<suite>.journal-of-mmwatchdog-dev.log` and
`<slot>/<suite>.journal-of-mmwatchdog-peer.log`. The shared suite runner
prints the saved journals' paths after the last lines of the suite's log.
The shared suite runner saves the two guests' journals at once, before the
next suite and before guest deletion. A guest's journal goes with the
guest, and a failure that happens once has no other record. Each saved
journal covers every boot from the suite's start until journal collection.
The fleet suite reboots `mmwatchdog-peer`, and the node suite reboots
`mmwatchdog-dev`. Every guest keeps the journal on disk (`vm.py up`). A
guest that does not answer within the time limit gets no journal file, and
the end of the suite's log says why. The suite's own result stays as it
was. The next run of the suite deletes the journals of the previous one.
`vm.py save-guest-journals --since-unix-time SECONDS --suite NAME` lets you
save the systemd journals of the test environment's two guests by hand.

To end a run before it finishes, use the harness:

```sh
python3 tests/integration/vm.py stop
```

`stop` ends the test command that holds the test environment (`MMWATCHDOG_TEST_ROOT`,
or the default root) and the test commands of its job slots, with every
process they started. `vm.py stop` then deletes the remaining Incus guests
and Hetzner servers created by runs in the selected test environment. Each
environment's lock file names the process that holds it, and `stop` ends
only that process and what it started. Runs in other test environments, and
other projects' runs on the machine running `vm.py stop`, go on. Never end a
run with `pkill` or `killall`: a name pattern also matches the runs of others
on a shared test machine.

The VM suite checks that `mmwatchdog-dev` is ready, builds MMWatchdog, builds
the sibling MMX working tree, resets MMWatchdog's fixtures, deploys through
MMX, and tests standalone behavior. `deployment` exercises the actual
release bundle and
its shipped MMX workflow. `soak` runs against the installed daemon after
deployment/testing. By default, `vm.py soak` finishes after two
failure/recovery cycles, notification retries, two daemon restarts with
queued notifications, and a fully drained queue. Steps advance as soon as
their expected result is observed, with bounded failure deadlines.

For infrequent investigation of resource growth, request the sustained
phase, in a test environment of the [cloud helper](#hetzner-cloud-servers):

```sh
python3 tests/integration/vm.py soak --sustained
```

After coverage, `vm.py soak --sustained` runs until the daemon has reached
steady state and done enough work to expose growth, rather than for a fixed
time. On the default guest, the sustained phase takes a few minutes.

Twenty-four checks flip together every 500 ms while in maintenance.
MMWatchdog records the checks' transitions at full rate but does not deliver
alerts for the transitions. Suppressing delivery keeps the notification
queue below the queue's limit, because each destination receives at most
about five events per second. An HTTP failure/recovery cycle with rejected
and retried deliveries runs every 50 rounds.

The fixture states `event_history_content_size_limit: 2MiB`. The sustained
phase ends once the event history has reached the size limit and then half
of the event history has turned over. Pruning therefore runs on every
commit. The sustained phase compares median memory and descriptors, and
peak storage, between the first and last quarters of steady state. Memory,
descriptors, and storage should stay flat.

The run starts from empty state and fails if any observation is missed for
lack of execution capacity. Every observation is a committed write, and the
flipping checks' interval leaves room for the checks' commits to queue
together.

The sustained phase is separate from routine development and acceptance.
The sustained phase runs in one daemon process, after the two restart
cases, so restarts cannot hide growth. The default soak mode and the
sustained soak mode leave product defaults unchanged.

Two other commands of `vm.py` prepare `mmwatchdog-dev`: `up` waits until it is
ready and checks its architecture, and `reset` resets MMWatchdog's fixtures on it.
`vm.py up` and `vm.py reset` check `mmwatchdog-dev`'s disposable-test marker.

`MMWATCHDOG_TEST_ROOT` selects the whole environment; its default is
`~/.cache/mmwatchdog-testing`. Builds, suites, and cleanup hold
`$MMWATCHDOG_TEST_ROOT/.test.lock`; nested builds inherit it, while SSH helpers do
not. Wrap a manual command that changes a test environment with `testenv.py`
to hold the environment's lock. The wrapper holds the lock of the test root
that `--root` names (default: `MMWATCHDOG_TEST_ROOT`, or the default root)
while the command after `--` runs:

```sh
python3 tests/integration/testenv.py --root ~/.cache/mmwatchdog-testing -- \
  rm -rf ~/.cache/mmwatchdog-testing/mmx-bin
```

The Python lock tests cover serialization, independent roots, aliases,
nested builds, owner termination, and command exit status without starting
VMs.

## Static analysis for a release

From the repository root on the development machine, run:

```sh
scripts/static-analysis.sh
```

The script installs pinned tools in a temporary directory and runs `go vet`,
`staticcheck`, `errcheck -ignoretests -exclude errcheck_excluded_functions.txt`,
`exhaustive -d`, `nilaway -test=false -include-pkgs=mmwatchdog`, and
`scripts/check-dependency-vulnerabilities.py`, which runs `govulncheck`.
Every tool checks `./...`; any failure makes the script exit nonzero.
For exhaustive, `-d` counts a default branch as exhaustive. The vulnerability
check uses a temporary repository copy with upstream dependencies, without
the local replacements. The check fails only for a reached advisory that
no copy's record lists as fixed, as the
[dependency rules](implementation-rules.md#when-a-copy-or-the-toolchain-changes) explain.
`tests/integration/test_dependency_vulnerabilities.py` tests the script with
a stand-in `govulncheck`.

Each reviewed exception has a one-line reason beside it. Whole-function
errcheck exceptions live in `errcheck_excluded_functions.txt`; individual
calls use commented `_ =` assignments. NilAway exceptions use
`//nolint:nilaway` on or immediately before the reported statement.
`staticcheck.conf` excludes ST1005 because MMWatchdog's errors shown to
users are full sentences. Deliberately partial
switches use `//exhaustive:ignore` with their reason immediately above the
directive. Errcheck and NilAway leave out test files.

## Continuous integration

On GitHub, [`.github/workflows/checks.yml`](../../.github/workflows/checks.yml)
runs on every push and pull request, as MMX's workflow does. MMWatchdog's
workflow runs `go test ./...` on Linux and macOS. On Linux, MMWatchdog's
workflow also runs `go vet ./...` and a `gofmt` check of MMWatchdog's own
directories. The Linux jobs also run the harness's Python unit tests and
build `mmwatchdog` for linux/amd64 and linux/arm64.
[`.github/workflows/dependency-vulnerabilities.yml`](../../.github/workflows/dependency-vulnerabilities.yml)
runs `scripts/check-dependency-vulnerabilities.py` every Monday, so an
advisory published after a change still fails a run.

MMWatchdog's workflow pins actions to commit hashes. Each job checks that
Go runs the `toolchain` that `go.mod` names. The VM suites do not run on
GitHub's runners. The VM suites need a Linux machine with KVM or Hetzner
Cloud servers
([where the suites run](#where-the-suites-run)).

The workflow checks out only the MMWatchdog repository.
`TestRelativeLinksResolve` passes on GitHub's runners only when no document
links into another repository by a relative path. The public-readiness
review before publication replaces cross-repository relative links.

## The fleet scenario and the test fixtures

The fleet scenario is the Linux workflow of the
[fleet checks](acceptance.md#fleet-checks). The fleet scenario runs on
two guests: `mmwatchdog-dev` as the hub and `mmwatchdog-peer` as the joined
node. The shipped joined-node MMX workflow deploys `mmwatchdog-peer`, which
reaches the hub over the guests' network.

`python3 tests/integration/vm.py fleet` runs only the fleet scenario against
an installed daemon. The full acceptance run, `vm.py test`, also runs the
fleet scenario, except in a cloud environment with only one VM
([cloud guests](#hetzner-cloud-servers)). The two
guests reach each other by address, and `mmwatchdog-peer` reaches the hub by the
name `mmwatchdog-dev.mmwatchdog-test.internal`, which `vm.py up` writes into the
`/etc/hosts` of `mmwatchdog-dev` and `mmwatchdog-peer`.

The MMHotline part runs a fake MMHotline
([`mmhotline_fixture.py`](../../tests/integration/mmhotline_fixture.py)) on the
hub guest. `mmwatchdog-dev` and `mmwatchdog-peer` reach the configured
`mmhotline_url`. The scenario checks signed alerts, receipts, check-ins,
status, and direct delivery while the MMWatchdog fleet hub is stopped. The
fake follows MMHotline's alert intake contract. Go tests cover the receipt
and signature failure cases too.

Two parts of the scenario need more nodes or other requests than MMWatchdog on the
peer sends. [`fleet_hub_client.py`](../../tests/integration/fleet_hub_client.py)
runs on `mmwatchdog-peer` and posts requests to the hub as a joined node does, over
TLS pinned to the certificate that the join code names. The scenario uses
`fleet_hub_client.py` for two parts:

- While the peer's daemon is stopped, the client uses the peer's credential
  to send reports and alerts that the hub must refuse.
- The client runs five synthetic joined nodes for fleet alert grouping.
  The synthetic nodes join and send alerts but never report.

Maintenance of the hub's `node_reports` holds the synthetic nodes' silence
alerts until the peer reports again and the scenario forgets the synthetic
nodes.

The scenario makes the hub fail where a test must control what the hub
does, with SQLite triggers that it adds to the hub's `fleet.db` and
`state.db` and drops again, also when the scenario fails. To simulate a hub
that cannot save reports or the peer's alerts, the triggers reject every
report write and every write of the peer's alerts.

For fleet alert grouping, a trigger holds the messages of the scenario's
check until all matching alerts are in the hub's alert intake and the hub
has restarted. The restarted hub forgets when each alert grouping key last
sent a message, so the restart ends the grouping minute. The restarted hub
plans one message containing all the waiting alerts.

The fixtures write `mmwatchdog_node_settings.yml`, `check_groups/*.yml`,
and, except on joined nodes, `alert_destinations.yml`. The fixtures use
`check-now` for observations without incident or recovery changes and
`check-now --without-daemon
--config-dir DIR` for standalone checks.

Status assertions use the check type for direct checks (`pipeline/shell`,
for example), `failure_incident` and `cannot_check_incident`, and the
separate service-restart and recovery-command attempt maps. Exit codes are
0 healthy, 1 failed, 3 unknown and 4 command error. A failure takes
precedence over unknown. Shell fixture exit codes remain the codes
returned by the fixture scripts.

Module fixtures install under `/usr/local/lib/mmwatchdog/external_modules`, one
directory for each module with its `module_description.yml` and its
`module.star` or `module_executable`.
The Bash example `file_age` exercises `MMWATCHDOG_MODULE_HELPER` and
`module-json`. Heartbeat senders use `--receiver-url` and
`--bearer-token-file`. Maintenance uses `on` and `off`, including
`--every-check-group`. Fleet checks are in `node_reports`.

MMX's `validate` command checks deploy project files locally without connecting
to nodes. From a prepared release directory (with `mmwatchdog` and `mmwatchdog.service` present):

```sh
mmx validate hub_and_standalone --inventory hub_and_standalone
mmx validate join_hub --inventory hub_members
```

The two `mmx validate` commands check MMX structure and source files.
`deployment.py` and `fleet.py` exercise MMWatchdog's runtime validation,
installation, unchanged-process guarantee, socket readiness, and maintenance
notification ordering on Linux.

The deployment suite also checks configuration-set selection through
`mmwatchdog_config_source_dir`, unchanged staging copies, and rejection of
a missing module source directory before installed modules or the daemon
change.

## The web service fleet example

The suite `web_service_fleet_example` tests the complete two-machine
monitoring setup in
[`examples/web-service-fleet`](../../examples/web-service-fleet/README.md).
The example's application server runs on `mmwatchdog-dev`, and the utility
host runs on `mmwatchdog-peer`.

The `web_service_fleet_example` suite builds the services that the example
watches. Then the suite deploys MMWatchdog through MMX on each guest with
the example's files for the corresponding machine. The suite runs 31
scenarios that cause real faults. The example's README says what each
check is there for and what each scenario shows.

```sh
python3 tests/integration/incus.py --distribution ubuntu2404 --suite web_service_fleet_example
```

- **`web_service_fleet_example` runs on Ubuntu 24.04 only.** The example's
  application server has a ZFS pool, and Ubuntu is the one target
  distribution whose kernel packages hold ZFS's kernel module; a test of
  MMWatchdog builds no kernel module. The setup also installs Ubuntu's
  packages by their names. One distribution is
  enough: the example's checks run the same code of MMWatchdog on every
  distribution, and the `node` and `services` suites test what differs
  between distributions.
- **`web_service_fleet_example` takes about 7 minutes.** As the longest
  suite, `web_service_fleet_example` comes first in `SUITES` and runs before
  any further suite on the example's guests. Of 400 seconds in one run,
  the scenarios took 331, and the packages, the setup, and the deploys
  about 70.
- **After `web_service_fleet_example` finishes, the job slot takes new
  guests.** The example's setup leaves MariaDB, Redis, memcached, dnsmasq,
  and Postfix running with the example's settings. Later suites configure
  MariaDB, Redis, memcached, dnsmasq, and Postfix differently. The setup also
  leaves a RAID array, a ZFS pool, and swap space.
- **The test's own files are in
  [`tests/integration/web_service_fleet_example/`](../../tests/integration/web_service_fleet_example/):**
  the setup scripts, the commands of the scenarios' steps, and the scenario
  file `scenarios.json`.

The driver
[`monitoring_setup_scenarios.py`](../../tests/integration/monitoring_setup_scenarios.py)
runs the scenarios of a whole monitoring setup from a scenario file. The
scenario file names each guest's MMWatchdog configuration, host names, and
setup scripts, and defines the quiet check and the scenarios. The driver's
docstring describes the scenario file's format.

The scenarios' steps cause failures and wait for alerts at a webhook
receiver on each guest.
An alert that no step waits for and that the scenario file does not allow
fails the scenario.

The suite's log has a line with `PASS` or `FAIL` and the seconds of each
scenario, and the driver
writes the results of each scenario as JSON to
`<job slot>/web_service_fleet_example/results/`. A setup absent from
`SUITES` in `tests/integration/all.py`, such as a setup kept outside the
MMWatchdog repository, runs on new guests with:

```sh
python3 tests/integration/incus.py --distribution debian13 --monitoring-setup-scenarios-file PATH/scenarios.json
```

The run of an uncatalogued setup writes results to
`<job slot>/<name>/results/`, where `<name>` is the name
of the scenario file's directory.

## Test environment rules

MMWatchdog's harness runs beside MMX's harness and shares nothing with MMX's
harness:

1. **MMWatchdog's own environment.** MMWatchdog's harness uses a separate
   test root and guest names, and never MMX's `mmx-dev`, `mmx-peer`, test
   root, or mutable build directory.
   MMWatchdog's harness reuses MMX's simple SSH approach, explicit port
   allocation, and one operating-system lock for the whole environment.
   MMWatchdog's harness therefore needs no shared service and no global
   test scheduler.
2. **Lock first.** MMWatchdog's harness takes the environment lock before
   building, starting or resetting guests, running tests, or cleaning up.
   If the environment is busy, the competing harness command shows the
   lock owner and waits. Runs within a test root are serialized, but runs
   in different roots can run concurrently. The lock is released when the
   harness process holding the lock exits. A nested build command reuses
   the parent command's lock and never leaves the lock in a long-lived SSH
   helper process.
   [`testenv.py`](../../tests/integration/testenv.py) holds the lock, and
   [`test_environment.py`](../../tests/integration/test_environment.py) tests it.
3. **MMX as it is.** MMWatchdog and MMX are released together, on the same
   day, and MMWatchdog is tested with the MMX code released with it. Each
   suite run builds the sibling MMX checkout as it is, uncommitted changes included, into MMWatchdog's own
   test root (`mmx-bin/`), and records MMX's commit, whether its tree was
   dirty, and the hashes of the binaries ([current MMX input](#current-mmx-input)).
4. **MMWatchdog's own deployment state.** The suites in which MMX deploys
   MMWatchdog run against MMWatchdog's guests, with an inventory and
   controller state that MMWatchdog owns.
5. **Behavior proved directly.** The suites prove monitoring, recovery, and
   notifications directly, as well as the deployment through MMX: MMX
   deploys MMWatchdog and is not a running dependency of it.

Guests have no controller-directory mounts, and no container runtime until
the scenarios of the checks of common services install Docker for the
`docker_container` check.

- A competing command reports the lock owner's PID and command, then waits;
  independent roots run concurrently. Read-only inventory and unit tests that
  do not use the VM environment take no lock.
- Keep the webhook test receiver inside the guest unless the node needs
  the webhook test receiver.
- Harness exit alone does not prove guest work stopped: after an interruption,
  reset the fixtures before the next suite. Cleanup stays inside the selected
  root.

## Current MMX input

[`mmx_build.py`](../../tests/integration/mmx_build.py) builds MMX from the `mmx`
checkout beside the MMWatchdog repository, including the MMX checkout's
working-tree changes. Each suite that deploys through MMX rebuilds the
controller and Linux ARM64/AMD64 runners into `$MMWATCHDOG_TEST_ROOT/mmx-bin`. Each suite builds
MMWatchdog with `scripts/build.sh`; Go's build cache keeps repeated builds
fast. Independent test slots may build concurrently into their own roots.
MMWatchdog never uses MMX's mutable `bin/` directory or test environment.

`incus.py` and `hetzner.py` build MMWatchdog and MMX once before their suites,
while the first guests start, and each job slot waits for that build. The
build of each suite then finds every package in Go's build cache. Otherwise
every job slot would compile the same changed packages at the same time,
which costs the first suite of each slot about 15 seconds.

`mmx-bin/manifest.json` and suite summaries record the MMX commit, working-tree
dirty status, and binary SHA-256 hashes. Keep the sibling MMX source
unchanged while suites run. The recorded commit, dirty status, and hashes
must describe the source and binaries used by the suites. Building the
current sibling MMX checkout into MMWatchdog's test root supersedes the
fixed-input rule in MMX's shared testing guide, because MMWatchdog and MMX
are released together, on the same day.

## Evidence and coverage

The [standalone validation record](validation.md) summarizes the verified run.

`vm.py test` without `--group` writes `summary-<distribution>-<arch>.json` with
passed scenarios, the MMWatchdog binary hash, and MMX provenance. Independent
acceptance groups write `summary-<group>-<distribution>-<arch>.json`; the
native group records its Go test output in
`linux-tests-<distribution>-<arch>.txt`. Deployment results and
controller state stay under `$MMWATCHDOG_TEST_ROOT`. A successful summary,
not a running command, is acceptance evidence.

The Go tests cover:

- Strict config validation, failing times, unknown states, and maintenance.
- Storage atomicity and bounds.
- Reload retirement.
- Process output and time-limit rules.
- The framing of a module's request and result.
- Starlark scripts and primitives, module descriptions, and helper value
  preservation.
- Daemon ownership.
- Heartbeat authentication and durability.
- Scheduling fairness.

The VM suite covers the product CLI with Linux/systemd, MMX deployment,
modules as a Starlark script and as Bash and Go executables,
notification retries, recovery attempts, reloads, daemon restarts, maintenance,
heartbeats, and node-resource/TCP checks. After deployment the
harness stops the persistent MMX endpoint and checks that no MMX runner remains
before exercising standalone behavior. MMX reaches each guest at its address
in `remote.json`, as it reaches any node.

The diagnostics scenarios also exercise check-group file source errors,
retained webhook/CLI evidence after recovery and daemon SIGKILL, actual
systemd start limits and slow startup, and real ENOSPC on an isolated
8 MiB tmpfs. Cleanup unmounts the isolated 8 MiB tmpfs.

`vm.py reset` stops the fault-test units, removes a leftover mount after
interruption, and clears MMX node job records so repeated suites cannot
fill the guest disk.
Controller run history remains available across MMX deployment runs and
suite reruns.
Neither the guest root disk nor the controller disk is filled.

Resource scenarios cover:

- Measuring a real CPU-consuming child process holding 48 MiB.
- Verifying node and unit measurements that are only recorded, without limits.
- Exercising incidents from a limit and the incidents' removal.
- Stopping and restarting the service and daemon.

The resource scenarios also record inodes, swap, and per-core load.
The resource scenarios remount a small isolated tmpfs read-only to fail and
recover the filesystem checks that state an `alert_severity`.
A filesystem check that only records readings on the read-only tmpfs opens
no incident. After an interruption, `vm.py reset` unmounts the isolated tmpfs.
A Linux test verifies that diagnostic sampling and unchanged reloads preserve
scheduled CPU baselines.
The example module `file_age` is tested against absent, fresh, stale, and
future-dated completion markers.
The module `file_age` reports the marker file's age, and the check's limit
decides what is stale.

The scenarios of the node's own checks (`node_scenarios.py`) use what the
guest really has: its own disk device and network interface, a dummy network
interface that they turn off and on, a tmpfs that they unmount and mount, and
three 32 MiB filesystems on loop devices, of which one is filled and one is
remounted read-only. A unit with two programs has one of them killed, and two
small programs hold memory and keep a processor busy for the commands in an
alert. A RAID 1 array on two more loop devices has one device failed and
added again. The check of security updates runs against the guest's real
package manager after an `apt-get update`, so the guest needs its
distribution's package mirror; the node-check scenarios install `mdadm`.
For the filesystem of another machine that is gone, the scenarios install
`nfs-kernel-server`, export a directory of the guest to the guest itself
over NFS, mount it, and stop the NFS server. Reset
unmounts the filesystems, removes the interface, stops the units and the
array, and frees the loop devices after an interruption. `vm.py reset`
unmounts the NFS filesystem without waiting for a reply from the NFS server.

The scenarios of the checks of common services (`service_scenarios.py`)
install real servers on the guest from the distribution's own packages:
Postfix, Dovecot, memcached, Redis, dnsmasq, PostgreSQL, pgBackRest,
MariaDB, and Docker. So the guest needs its distribution's package mirror,
and a guest keeps the packages between runs. A second PostgreSQL server
follows the first as its replica for one scenario, and pgBackRest archives
the first server's write-ahead log and backs it up into a repository on
the guest's own disk for another. The Docker scenario downloads no
image: it makes one from the guest's own `busybox` program. The scenarios
of InnoDB's history list length make the list grow:
one session holds a snapshot open while another commits 300 changes, so
InnoDB keeps the old row version of each change. The growth is exact, and
so the history-list scenarios depend on no machine's speed.

Debian has no package of MySQL and none of MongoDB. So two scenarios
download the images `mysql:8.4` and `mongo:8.0`, which their makers
publish, from Docker Hub to the guest, and run the servers as containers.
MongoDB 8.0 refuses to start on Linux 6.19 and newer, so on Ubuntu 26.04,
whose kernel is Linux 7.0, the scenario takes `mongo:8.2`. The containers are
a MySQL source with its replica, and five MongoDB servers, of which three
form a replica set. The checks themselves run on the guest, as on any
node, with client programs of the guest: the `mariadb` program of Debian,
and then the `mysql` program and the `mongosh` shell, which the scenarios
copy out of the two images. For the time with MySQL's own client program,
the scenario sets Debian's `mariadb` program aside with `dpkg-divert` and
puts it back. Only the MySQL and MongoDB container scenarios require guest
network access to a remote host besides the distribution's package mirror.

The scenario of the official extra module
[`mattermost`](../../official_extra_modules/mattermost/README.md)
(`mattermost_scenarios.py`) delivers MMWatchdog's alerts to a real Mattermost
server in the guest, on Debian and Ubuntu. The scenario installs Mattermost's
open-source Team Edition from Mattermost's release archive.
Each run creates a new database in the guest's PostgreSQL, so no outside
account is needed. The machine that runs the suite downloads the 460 MB
archive once from `releases.mattermost.com`.
The suite checks the archive against Mattermost's published SHA-256 and
keeps the archive in `~/.cache/mmwatchdog-test-downloads` for later runs
on the machine running the suite. Each run sends the Mattermost archive
through the guest's SSH connection straight into `tar`, without the
plugins and the web application, which the test does not use.
The server speaks HTTPS with a certificate that
the test makes and that the guest trusts through its system trust store; it
runs no plugin and asks no server of Mattermost's on the internet. mmctl,
Mattermost's command-line program, sets the server up in its local mode,
without a click and without a login: an administrator, a team, a channel with
two more people in it, and an incoming webhook. The scenario reads the
channel's posts, and how often Mattermost counted a mention of each person,
through Mattermost's REST API. The server answers 6 seconds after the
scenario starts: about 3 seconds to unpack the archive, and 2 for its first
start, which makes the tables of the new database. Mattermost uses about
120 MiB of the guest's memory.

The email scenarios (`email_scenarios.py`), in the acceptance and fleet
suites, use Mailpit v1.31.6, an independent open-source test mail server.
The shared archive downloader in `vm.py` downloads Mailpit's Linux archive
from GitHub once per architecture. The downloader verifies GitHub's
published SHA-256 digest and keeps the Mailpit archive in
`~/.cache/mmwatchdog-test-downloads`, beside Mattermost's archive.
Each run streams only the Mailpit binary into each guest through
SSH and starts three transient systemd units. All SMTP listeners and HTTP
APIs bind to `127.0.0.1`: SMTP port 18025 requires STARTTLS and a login,
18465 requires TLS from the first byte and a login, and 18026 accepts
unencrypted mail without a login. The three Mailpit servers' HTTP API ports
are 18081, 18082, and 18083, respectively. The guest trusts a test
certificate for `localhost` through its system trust store. The scenarios
submit alerts through
MMWatchdog's SMTP code and read the recipient, subject, body, login, and
message ID through Mailpit's API. Raw messages verify the MIME encoding.
Each fleet node uses its own Mailpit servers, so the receiving guest proves
which node sent an alert. Each run stops the servers, which deletes their
temporary message databases, and removes the binary, certificate, private
key, auth file, and certificate trust. Mailpit's version checks and reverse
DNS lookups are disabled. No outside email account is needed. Internet
delivery and spam filtering are outside the email scenarios.

The common-service scenarios (`service_scenarios.py`) run the official
extra modules `apache_status`, against Apache, and `zfs_pool`, against a
pool that mirrors two files. On Ubuntu the scenario installs the package
`zfsutils-linux` and loads the ZFS kernel
module that comes with Ubuntu's kernel. The ZFS scenario scrubs the pool,
and holds a second scrub in progress with OpenZFS's setting
`zfs_scan_suspend_progress`, so that the scrub's scan line stays the same
until the scenario pauses and cancels it. A test of MMWatchdog builds no kernel
module, so on Debian 12 and Debian 13 `zfs_pool` runs on a node without ZFS.

The common-service scenarios also test certificates. The test makes a
certificate for Postfix and Dovecot. `/etc/hosts` maps the mail
certificate's name to the guest's own address.

The database servers get a certificate signed by the test's certificate
authority, for a name of the database servers' own.
`/etc/hosts` also maps a second name to the guest's address, but the second
name is absent from the database certificate.
The guest does not trust the test's certificate authority.
One check trusts the test's certificate authority through
`trusted_certificate_authority_file`, and another check fails without the
configured trust.
The checks of the SSH server use
the guest's own OpenSSH server and never stop it, because the harness works
through it.

Storage unit tests force SQLite's page limit, kill a writer after a
committed transaction, and verify atomic rollback and recovery. The storage
unit tests also churn evidence through the history/outbox byte budgets
while measuring database/WAL growth.

### Real-world Monit work

Beta validation checked MMWatchdog against 203 scenarios of real monitoring work,
taken from 198 public Monit sources and one company's production Monit
setup. Each scenario was first matched against the suites and examples in
the coverage table below.

Work that the listed suites and examples did not prove, and work with an
unusual twist, ran on guests: in whole projects converted from the public and company Monit
setups, and in two packs of separate scenarios. One of the projects is now
the example
[`web-service-fleet`](../../examples/web-service-fleet/README.md), which the
suite [`web_service_fleet_example`](#the-web-service-fleet-example) runs.

The [validation record](validation.md#beta-validation-with-real-world-monit-work)
describes the guest runs of converted projects and separate scenario packs,
with the results of the guest runs.

Uncommon work that a `shell` check or a module of the project's own does
counts as covered. Each row's scenario
count includes covered work and work that MMWatchdog leaves out by design.
The counts include 22 excluded scenarios in all, such as a web page that
starts and stops services.

The contracts are rows of the
[standalone acceptance contracts](acceptance.md#standalone-acceptance-contracts),
and those marked "(fleet)" are rows of the
[fleet acceptance contracts](acceptance.md#fleet-acceptance-contracts). The
scenario files are in `tests/integration/`, and the examples in `examples/`.

| Work | Scenarios | Contracts exercised by the scenarios | Scenario files that prove the work | Examples |
|---|---|---|---|---|
| Services and databases that answer by their protocol | 23 | Check common services by their protocol; Check authenticated endpoints; Check a Unix socket and another machine; Check database servers, containers, and SQLite databases | `service_scenarios.py`, `acceptance.py`, `node_scenarios.py` | `check_groups/myapp.yml`, `web-service-fleet` |
| Running MMWatchdog, and watching MMWatchdog itself | 22 | Configure and deploy; Observe and diagnose; Reload during checks; Keep measurement history; Restart MMWatchdog; Alert on silent nodes (fleet); Watch the hub (fleet) | `acceptance.py`, `deployment.py`, `diagnostics.py`, `fleet.py` | `deploy`, `mmwatchdog_node_settings.yml`, `mmwatchdog.service` |
| Recovery actions and their limits | 21 | Restart a systemd unit; Run a recovery command; Confirm and recover | `diagnostics.py`, `acceptance.py`, `lifecycle.py`, `one_cause_scenarios.py` | `check_groups/myapp.yml`, `web-service-fleet` |
| Services that run | 16 | Restart a systemd unit; Catch every failed systemd unit; Catch a crash loop; Watch one program of a unit; Watch node resources; Check database servers, containers, and SQLite databases | `lifecycle.py`, `node_scenarios.py`, `resources.py`, `acceptance.py`, `service_scenarios.py` | `check_groups/myapp.yml`, `web-service-fleet` |
| The node's resources and network interfaces | 16 | Watch node resources; Alert on reboots; Watch the clock and out-of-memory kills | `node_scenarios.py`, `resources.py`, `fleet.py` | `check_groups/myapp.yml`, `web-service-fleet` |
| Fewer and quieter alerts | 16 | Confirm and recover; Send one alert for one cause; Remind about open incidents; Choose who gets an alert; Plan maintenance | `acceptance.py`, `one_cause_scenarios.py`, `diagnostics.py`, `fleet.py`, `lifecycle.py` | `alert_destinations.yml`, `web-service-fleet` |
| Disks, filesystems, and storage hardware | 14 | Watch node resources; Watch every mounted filesystem; Watch the node's software and hardware; Check tools through official extra modules | `node_scenarios.py`, `resources.py`, `service_scenarios.py` | `check_groups/myapp.yml`, `web-service-fleet` |
| Files, logs, and scheduled jobs | 14 | Check a file; Watch a log; Watch the clock and out-of-memory kills; Receive heartbeats | `node_scenarios.py`, `lifecycle.py`, `acceptance.py` | `modules/file_age` |
| Other machines, endpoints, and certificates | 11 | Check authenticated endpoints; Check a Unix socket and another machine; Check common services by their protocol; Alert on silent nodes (fleet) | `acceptance.py`, `fleet.py`, `service_scenarios.py` | `web-service-fleet` |
| Alert content and destinations | 10 | Write an alert's title and body; Deliver notifications; Send alerts by email; Deliver alerts through a module; Write alerts to the system log; Send a notice | `acceptance.py`, `email_scenarios.py`, `official_extra_destination_module_scenarios.py`, `destination_module_scenarios.py`, `mattermost_scenarios.py` | `alert_destinations.yml`, `modules/alarm_system`, `web-service-fleet` |
| Security and changes on the node | 10 | Check a file; Watch the node's software and hardware | `node_scenarios.py` | |
| Scripts, status pages, and modules of the project's own | 10 | Run a module that is not core; Observe and diagnose; Check tools through official extra modules | `acceptance.py`, `service_scenarios.py`, `resources.py` | `modules/queued_jobs`, `modules/queued_jobs_in_go`, `modules/failed_jobs`, `web-service-fleet` |
| The fleet view | 8 | Join the fleet (fleet); Report status (fleet); Show fleet health (fleet); Keep the measurement history of nodes (fleet); Forget a node (fleet) | `fleet.py`, `resources.py` | |
| Who gets an alert, and how urgent | 6 | Route by alert severity; Choose who gets an alert; Deliver fleet alerts (fleet) | `fleet.py`, `acceptance.py` | `alert_destinations.yml`, `web-service-fleet` |
| Planned work and maintenance | 6 | Plan maintenance; Restart a systemd unit; Configure and deploy | `acceptance.py`, `deployment.py`, `lifecycle.py` | `deploy` |

### Every target distribution

MMWatchdog is built around systemd and is tested on the target
distributions, which are where the suites run: every Debian release and every Ubuntu LTS release
while it receives security fixes without a subscription, Rocky Linux 9 and
AlmaLinux 10, and Fedora 45 of the Red Hat family, Arch Linux, and the
newest openSUSE Leap. Each module page lists under "Tested on" the
distributions where the module is tested. The
[release plan](design/README.md) says what to expect on other systemd
distributions, and why servers without systemd are not supported.

| Distribution | Harness name | Package manager |
| --- | --- | --- |
| Debian 12 | `debian12` | `apt` |
| Debian 13 | `debian13` | `apt` |
| Ubuntu 24.04 LTS | `ubuntu2404` | `apt` |
| Ubuntu 26.04 LTS | `ubuntu2604` | `apt` |
| Rocky Linux 9 | `rocky9` | `dnf` |
| AlmaLinux 10 | `almalinux10` | `dnf` |
| Fedora 45 | `fedora45` | `dnf` (dnf 5) |
| Arch Linux, the current rolling release | `archlinux` | `pacman` |
| openSUSE Leap 16.1 | `opensuseleap161` | `zypper` |

The names are `TARGET_DISTRIBUTIONS` in [`all.py`](../../tests/integration/all.py),
and `incus.py` and `hetzner.py` take them with `--distribution`. The names
are MMX's, which deploys MMWatchdog. Hetzner Cloud has images of the Red Hat
family, but `hetzner.py` takes only the Debian and Ubuntu names for now, as
MMX's runs of the Red Hat family on Hetzner and ARM64 come later; it has no
image of Arch Linux or openSUSE Leap. ARM64 runs cover those four.

Some checks know only some distribution families. `apt_security_updates`
knows Debian and Ubuntu, `dnf_security_updates` knows the Red Hat family,
Fedora included, and `zypper_security_patches` knows SUSE. `reboot_after_updates` and
`distribution_release_security_support` know Debian, Ubuntu, the Red Hat
family, and SUSE. `known_exploited_vulnerabilities_in_installed_packages`
knows Debian and Ubuntu, and `ubuntu_pro_security_fixes` knows Ubuntu. The
node suite tests each of them against the real package manager where it
knows the distribution, and checks that it is unknown with its reason where
it does not. On Rocky Linux and AlmaLinux, the suite first checks that
`reboot_after_updates` is unknown without `dnf-plugins-core` and installs the
package, turns on the refresh timer with the command of the module page, and
lets the check find a security update that the suite makes wait by
installing the version before a package's security update. Fedora's dnf is
dnf 5, which the suite tests in its own way: it takes `dnf5-plugins` away to
see `reboot_after_updates` unknown and puts it back, ages the package data of
every repository to 20 days (one of Fedora's repositories keeps its package
data 14 days), and sees that the failed refresh with the servers blocked
touches no package data, where dnf 4 touches its stamp. The guests of the Red
Hat family run SELinux enforcing, as a default installation does. On openSUSE Leap, the suite also writes the two units of
`zypper_security_patches`'s "Set up" from the module page and starts the
service. Arch Linux has no package-manager check yet. The harness's differences between the package managers are tables:
`INSTALL_COMMANDS`, `PACKAGE_NAMES`, and `TRUST_STORES` in `vm.py`,
`PREPARE` in `incus.py`, and `LAYOUTS` in `service_scenarios.py`.

[`incus.py`](../../tests/integration/incus.py) runs the suites on every
distribution at once, in Incus VMs on a Linux machine with KVM:

```sh
python3 tests/integration/incus.py
python3 tests/integration/incus.py --distribution debian13 --distribution ubuntu2604 --suite node
```

Without options `incus.py` runs every suite on every distribution. Each
line of output starts with its distribution; a failed suite prints the last
lines of
its log, which is `$MMWATCHDOG_TEST_ROOT/<distribution>/slots/<n>/<suite>.log`, and
the paths of
[its guests' journals](#where-the-suites-run) beside it.

- **Every guest is a VM, never a container.** The suites read what the
  guest's own kernel says: its I/O wait, its memory pressure, its disk
  device, its out-of-memory kills, and its clock. The suites also load the
  kernel's modules for ZFS, RAID, and NFS. A container shares the kernel of
  the machine below it.
- **Every guest is a copy of a prepared base of its distribution.** The
  first run makes the base from the distribution's image of
  `images.linuxcontainers.org` (the cloud variant), which it copies once into
  Incus's local store. After the image is copied, runs do not depend on the
  image server.
  `--prepare-bases` makes the bases again from the newest images. A base gets
  an SSH server and Python, no automatic upgrades, a login for root by key
  only, and the file `/etc/mmwatchdog-test-vm`, the disposable-test marker. Secure
  Boot is off, as on a cloud server.
- **Each job slot gets two new copies and deletes them when it is done:**
  `mmwatchdog-dev` with 2 CPUs and 4 GiB, and `mmwatchdog-peer` with 1 CPU and 1 GiB, each
  with a disk of up to 40 GiB. The host allocates disk space only for data
  written by the guest. `mmwatchdog-dev` is large because the common-service
  scenarios install database servers and download two images. The slot writes a `remote.json`
  for `mmwatchdog-dev` and `mmwatchdog-peer`, with a login key of the run.
  The suites reach `mmwatchdog-dev` and `mmwatchdog-peer` over SSH, as the
  suites reach the [Hetzner Cloud servers](#hetzner-cloud-servers).
  Copying a base takes a second and starting a guest about 15 seconds.

  `--pairs-per-distribution` sets the slots of each distribution, with a
  default of 3.
  `--max-guest-pairs-at-once` sets the slots that have guests at the same
  time, with a default of half of the machine's CPU count.
- **An `incus.py` run owns only the guests created by the run.** `incus.py`
  records the run in
  `$MMWATCHDOG_TEST_ROOT/incus-runs/<run>/ownership.json` and labels the run's
  guests with the Incus key `user.mmwatchdog-test-run`. `incus.py` deletes
  the run's guests on success, failure, and interruption. `vm.py stop` with
  the `MMWATCHDOG_TEST_ROOT` of the `incus.py` run ends the run and deletes
  the run's guests. After the machine that ran the `incus.py` run died, run
  `incus.py --cleanup-from-manifest <ownership.json>`.
- **Several projects can share one test machine.** With
  `--shared-guest-slots DIRECTORY`, each guest pair holds two lock files
  named `slot-*` in that directory while it runs (it holds 5 GiB of the
  machine's memory), and waits while another run holds them. Every run on the
  machine, across all projects, passes one common directory to
  `--shared-guest-slots`, so that runs together never start more guests
  than the machine has room for. While several runs
  want slots, each takes at most its fair share, the slots divided by those
  runs; a run alone takes them all.
- **The suites measure in seconds, so their guests need processors that
  are free.** A check that runs every 300 ms, and a delivery with a one-second
  time limit, fail on a test machine that other work keeps busy: a
  delivery that MMWatchdog gave up on arrives a second time. The shared
  slots above keep the guests of all runs within what the machine can serve.
- **A guest's writes do not wait for the disk of the test machine.** MMWatchdog
  commits every observation to its database, and a package manager asks
  for the same after every file. Each such write waited 7 milliseconds on
  a test machine that did nothing else, and seconds while another guest
  installed packages: checks then missed their turns, and a suite measured
  the machine's disk. So the guest's disk has Incus's setting
  `io.cache=unsafe`. A guest whose machine loses power can have a damaged
  disk, which costs nothing: the guest is a copy for one run. Inside the
  guest nothing changes.
- **Every suite runs on every distribution, except one, and most scenarios
  do.** The [web service fleet example](#the-web-service-fleet-example) runs
  on Ubuntu 24.04 only. `SUITE_DISTRIBUTIONS` in `all.py` names the
  distributions of such a suite, with the reason beside it. A run that
  selects other distributions too omits `web_service_fleet_example` on the
  unsupported distributions and prints a line naming the omission.
  A run whose suites run on none of the selected distributions ends with
  a message.

  The common-service scenarios (`service_scenarios.py`) run on every target
  distribution: text exchange, SSH, mail protocols, dnsmasq, memcached,
  Redis, SQLite, PostgreSQL, pgBackRest, MariaDB, Docker, MySQL, MongoDB,
  Apache, and Mattermost.

  The scenario of a real ZFS pool runs on Ubuntu 24.04 and Ubuntu 26.04,
  whose kernel packages hold ZFS's kernel module. Debian 12 and Debian 13
  run `zfs_pool` on a node without ZFS.
- **The scenario of security updates and required reboots**
  (`node_scenarios.py`) uses the guest's apt package lists and reboot hook.
  A guest without a kernel hook that writes `/run/reboot-required`, as the
  guests of Debian are, gets `unattended-upgrades` with automatic updates
  turned off, after the scenario has checked that the required-reboot
  check is unknown.
  The scenario then runs the guest's reboot-required hook directly, rather
  than install a kernel.

The extra lifecycle scenario exercises:

- A reloading unit that passes the unit's check, and a
  deactivating unit. MMWatchdog consumes no service restart attempt and
  does not interrupt the in-progress job.
- A unit stopped with `systemctl stop` that MMWatchdog leaves stopped.
- A unit whose program was killed that MMWatchdog restarts.
- A crash-looping unit failing the `systemd_unit_crash_loop` check.

`vm.py lifecycle` runs just the extra lifecycle scenario against an
installed daemon.

Module pages that rely on the "Every target distribution" section:
[`http`](../reference/modules/http.md), [`tcp`](../reference/modules/tcp.md), [`udp`](../reference/modules/udp.md),
[`unix_socket`](../reference/modules/unix_socket.md), [`ping`](../reference/modules/ping.md),
[`tls`](../reference/modules/tls.md), [`ssh`](../reference/modules/ssh.md), [`smtp`](../reference/modules/smtp.md),
[`imap`](../reference/modules/imap.md), [`pop3`](../reference/modules/pop3.md), [`dns`](../reference/modules/dns.md),
[`memcached`](../reference/modules/memcached.md), [`redis`](../reference/modules/redis.md),
[`postgresql`](../reference/modules/postgresql.md), [`pgbackrest`](../reference/modules/pgbackrest.md),
[`mysql`](../reference/modules/mysql.md),
[`sqlite_database`](../reference/modules/sqlite_database.md),
[`docker_container`](../reference/modules/docker_container.md),
[`systemd_unit_state`](../reference/modules/systemd_unit_state.md),
[`systemd_unit_commands`](../reference/modules/systemd_unit_commands.md),
[`systemd_unit_crash_loop`](../reference/modules/systemd_unit_crash_loop.md),
[`every_systemd_unit`](../reference/modules/every_systemd_unit.md),
[`systemd_unit_resource`](../reference/modules/systemd_unit_resource.md),
[`node_resource`](../reference/modules/node_resource.md),
[`node_filesystem`](../reference/modules/node_filesystem.md),
[`every_disk_and_network_filesystem`](../reference/modules/every_disk_and_network_filesystem.md),
[`node_disk_device`](../reference/modules/node_disk_device.md),
[`node_network_interface`](../reference/modules/node_network_interface.md),
[`node_temperature`](../reference/modules/node_temperature.md),
[`mdadm_array`](../reference/modules/mdadm_array.md),
[`apt_security_updates`](../reference/modules/apt_security_updates.md),
[`dnf_security_updates`](../reference/modules/dnf_security_updates.md),
[`zypper_security_patches`](../reference/modules/zypper_security_patches.md),
[`ubuntu_pro_security_fixes`](../reference/modules/ubuntu_pro_security_fixes.md),
[`reboot_after_updates`](../reference/modules/reboot_after_updates.md),
[`distribution_release_security_support`](../reference/modules/distribution_release_security_support.md),
[`known_exploited_vulnerabilities_in_installed_packages`](../reference/modules/known_exploited_vulnerabilities_in_installed_packages.md),
[`node_clock`](../reference/modules/node_clock.md),
[`out_of_memory_kills`](../reference/modules/out_of_memory_kills.md),
[`file`](../reference/modules/file.md), [`log_lines`](../reference/modules/log_lines.md),
[`shell`](../reference/modules/shell.md), [`webhook`](../reference/modules/webhook.md),
[`email`](../reference/modules/email.md), [`system_log`](../reference/modules/system_log.md),
[`mmhotline`](../reference/modules/mmhotline.md),
[`apache_status`](../../official_extra_modules/apache_status/README.md),
[`discord`](../../official_extra_modules/discord/README.md),
[`mattermost`](../../official_extra_modules/mattermost/README.md),
[`mongodb`](../../official_extra_modules/mongodb/README.md),
[`pushover`](../../official_extra_modules/pushover/README.md),
[`slack`](../../official_extra_modules/slack/README.md),
[`telegram`](../../official_extra_modules/telegram/README.md),
[`zfs_pool`](../../official_extra_modules/zfs_pool/README.md).

## Hetzner Cloud servers

[`hetzner.py`](../../tests/integration/hetzner.py) runs the suites on new
Hetzner Cloud servers, as `incus.py` runs them in Incus VMs, for every
target distribution. The servers are AMD64 (x86) unless
`--server-architecture arm64` asks for Hetzner's ARM64 servers, each from
Hetzner's ARM64 image of its distribution. MMWatchdog's suites run on ARM64
on Hetzner Cloud servers because the test servers that run `incus.py` are
AMD64. A contributor without a Linux machine with KVM also runs the suites
on Hetzner Cloud servers. Use AMD64 servers for ordinary runs and ARM64
servers to test ARM64.

Each job slot gets two servers of 2 CPUs and 4 GiB: `cx23`, or `cpx22` when
Hetzner has sold out of `cx23`, or on ARM64 `cax11`. The servers go to
`hel1`, or to `fsn1` or `nbg1` when Helsinki has no room. The slot also gets
a firewall that lets in SSH and MMX's HTTPS port 7443 from the controller
machine running `hetzner.py`. The firewall also lets the two servers reach
each other. The harness deletes the servers and firewall when the job slot
finishes.

MMX deploys over HTTPS where it can and takes SSH only after an HTTPS
connection fails. A firewall that dropped port 7443 would make MMX wait
for a time-out at every fleet stage of a deployment.
A fleet stage selects nodes and gives each selected node tasks to run.
Each deploy of the deployment suite would take 13 to 30 seconds instead
of a few.

cloud-init gives each server what an Incus base has. The harness logs in as
root with a key that it makes for the run and deletes with the servers,
so that it also runs on a test server without `~/.ssh/id_rsa`; every create request
names the registered key of `MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME`, so that
Hetzner makes no root password.

A run holds at most 6 servers at once. Choose a value for
`--max-servers-at-once` that fits your Hetzner project's server limit.
The harness records the run in
`$MMWATCHDOG_TEST_ROOT/cloud-runs/<run>/ownership.json` and labels its servers and
firewalls with the run; `vm.py stop` and `hetzner.py
--cleanup-from-manifest` delete what a run left.

Before a cloud run, export `MMWATCHDOG_TEST_HETZNER_API_TOKEN` and
`MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME`:

```sh
export MMWATCHDOG_TEST_HETZNER_API_TOKEN='replace-with-your-test-project-api-token'
export MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME='Contributor test key'
```

`MMWATCHDOG_TEST_HETZNER_API_TOKEN` is a Hetzner Cloud API token with read
and write access to the project for MMWatchdog's test servers. The harness
reads `MMWATCHDOG_TEST_HETZNER_API_TOKEN`, not `HCLOUD_TOKEN` of Hetzner's
`hcloud` command. A token for another project in your shell therefore never
creates test servers in the wrong project.

`MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME` is the exact name of an SSH key
already registered in the Hetzner project for MMWatchdog's test servers.
Replace the example values of `MMWATCHDOG_TEST_HETZNER_API_TOKEN` and
`MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME`.

`MMWATCHDOG_TEST_HETZNER_API_TOKEN` and
`MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME` have no defaults. An unset setting
fails before any server is created. Cleanup from a manifest needs only the
token.

```sh
MMWATCHDOG_TEST_ROOT=~/.cache/mmwatchdog-testing-hetzner python3 tests/integration/hetzner.py --distribution debian13 --suite node
MMWATCHDOG_TEST_ROOT=~/.cache/mmwatchdog-testing-hetzner-arm64 python3 tests/integration/hetzner.py --server-architecture arm64 \
  --distribution debian13 --distribution ubuntu2604 --suite native --suite deployment
```

To create an environment and run suites individually, use `cloud.py` with
a separate MMWatchdog test root. The
[cloud helper](../../tests/integration/cloud.py) owns one labeled VM and its
firewall, which lets in only SSH and MMX's HTTPS port 7443 from the
controller machine running `cloud.py`. Pass `--distribution` to
`cloud.py up` to select the VM's operating system from the four target
distributions, such as `ubuntu2604`. Debian 13 is the default.

The VM uses the Hetzner server type selected by `--server-type`: `cx23`
(AMD64) by default, or `cax11` for ARM64. The cloud helper creates the VM
in the first of `hel1`, `fsn1`, and `nbg1` where Hetzner has the selected
server type available. The helper records the selected server type's
processor architecture in `remote.json`.

With `--with-peer`, the cloud helper creates a second VM for the fleet
workflow's joined node. The second VM is of the first VM's kind. The
firewall then also lets the two VMs reach each other on every TCP port.

The helper verifies the key of
`MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME` against `~/.ssh/id_rsa.pub` and
passes its ID through Hetzner's provider-level `ssh_keys` field. SSH login
uses the controller machine's existing `~/.ssh/id_rsa` private key.
The guests' separate SSH host key is pinned before the first connection.
No replacement login key or password bootstrap is used.

```sh
export MMWATCHDOG_TEST_ROOT=~/.cache/mmwatchdog-testing-amd64
python3 tests/integration/cloud.py up --with-peer
python3 tests/integration/vm.py test
python3 tests/integration/vm.py deployment
python3 tests/integration/vm.py soak --prepare
python3 tests/integration/cloud.py down
```

`remote.json` records the owned IDs and SSH connections under
`MMWATCHDOG_TEST_ROOT`, set to `~/.cache/mmwatchdog-testing-amd64` in the
example above. The VM harness reads `remote.json` to run the acceptance,
deployment, and soak suites natively on the VM's architecture.
The harness uses one shared SSH connection to each guest, which exits
after 60 idle seconds.

`vm.py test` runs the fleet workflow when the environment has the second VM,
and skips it otherwise. The cloud servers are created with
`manage_etc_hosts: false`, so that a reboot keeps the name of the main guest
that `vm.py up` writes into each guest's `/etc/hosts`.

Credentials, binaries, MMX builds and result files remain in the test
environment. `cloud.py down` checks the recorded labels
and IDs before deleting only the VMs and firewall recorded in
`remote.json`. The command verifies deletion and records
`cloud-cleanup.json`. Deleting a guest
also removes MMX's controller records for it, since MMX pins node identity by
address and a new guest can reuse the address. On interruption, use the
`MMWATCHDOG_TEST_ROOT` directory used for `cloud.py up`
to inspect or clean up; never create another VM to replace a working one merely
to refresh the harness.

## Large-fleet load run

[`load.py`](../../tests/integration/load.py) measures a hub under a synthetic fleet
from the [load generator](../../tests/load/main.go). Each synthetic node joins
at a random moment in the first report interval. A synthetic node retries
joining each interval until joining succeeds. A real node joins as soon as
the node starts. The generator's 10,000 nodes join as nodes that a deploy
starts over 30 seconds would.

After joining, each synthetic node reports every interval on a new TLS
connection with a small node's usual state, as a real node does. Once the
hub's reply states how far the hub has received a synthetic node's readings,
each of the synthetic node's reports also carries readings from ten runs,
two measurements each. The hub stores the readings in the measurement
history of nodes.

After all synthetic nodes join, the phases run in order: steady reporting,
a group going silent, steady reporting again, a group of the same size
failing `app/http` with an alert each, and steady reporting again.

The hub has a local webhook receiver and runs MMWatchdog under an optional
systemd `MemoryMax`. Every 10 seconds the run samples the hub's CPU, memory, and
database sizes, and every 30 seconds times `mmwatchdog fleet`, `mmwatchdog fleet --json`, and
`mmwatchdog status --json`. The large-fleet load run passes when all four
conditions hold:

- The hub's daemon has no unexpected restarts: systemd's `NRestarts` is 0.
- No synthetic node fails two report attempts in a row. A successful report
  resets the reporting node's consecutive-failure count. A failed report
  followed by a successful report in the next interval passes. The count
  continues across phase changes.

  Nodes that go silent on purpose send no reports during the silence phase.
  Skipped reports during deliberate silence neither add failures nor reset
  the count. Nodes outside the
  deliberately silent subset must keep reporting during the silence phase.

  The largest consecutive-failure count includes attempts still in flight
  when the phases end. The test adds no latency target.
- The hub accepts every alert that a synthetic node posts, including the
  alerts still in flight when the phases end. Any failed post fails the run,
  which names the phase, each type of error, and its count.
- Every group change arrives as at most five grouped messages covering the
  whole group.

`load-result.json` holds the phase latencies, samples, messages, and hub
restart count. `load-result.json` also records the largest count of failed
reports in a row from any node, and the name of the node with the largest
count.

A smoke run uses `mmwatchdog-dev` as hub and `mmwatchdog-peer` as generator, the two VMs
of a test environment of the cloud helper (`cloud.py up --with-peer`):

```sh
python3 tests/integration/load.py run --local --nodes 500 --group 100 --every 10s \
  --steady 30s --silence-for 1m --fail-for 40s --settle 40s
```

The full run uses two cloud VMs in their own test root, created like the
[cloud guest](#hetzner-cloud-servers), with the hub's port open only to the
generator. The full large-fleet load run takes about 20 minutes:

```sh
export MMWATCHDOG_TEST_ROOT=~/.cache/mmwatchdog-testing-load
python3 tests/integration/load.py up --hub-server-type ccx23
python3 tests/integration/load.py run --memory-max ''
python3 tests/integration/load.py down
```

## Release and sustained-test evidence

`release-validation-<distribution>-<arch>.json` records deployment checks and artifact
hashes. The deployment suite verifies archive files against their manifest and
exercises install, no-change rerun, config/unit/binary updates, rejected
configuration, fragment conflicts and missing target credentials,
stopped-service startup, and incident preservation. Release builds require a clean checkout and validate the
version before writing artifacts.

`soak-result.json` and `soak-samples.jsonl` record the soak workload.
The soak test's fixture runs 32 checks: 24 flipping shell checks every
500 ms with `alert_after_failing_for: 0s` and
`recovered_after_healthy_for: 0s`, and the rest every second with both
waiting times at `1s`.

The first restart preserves an active incident and the incident's queued
notification. The second restart preserves a queued failure/recovery pair
after the check recovers. Each outage must reject an initial notification
and a retry. The soak run verifies stable incident identity, ordered
delivery of every rejected event, observation progress, and a healthy
final state with an empty queue.

The soak test checks storage errors/gaps, unexpected restarts, memory
growth and descriptor counts. Samples
record CPU, RSS, systemd memory accounting, descriptors, threads and database/WAL
size.

`soak-result.json` distinguishes the routine coverage phase from a
sustained run. The sustained run's `steady_state` in `soak-result.json`
records the rounds, events, and first- and last-quarter resource use.

The guest workload belongs to `mmwatchdog-soak.service`, which reset stops before
changing fixtures after an interrupted run. Duration is recorded.

The routine coverage phase establishes function. The sustained phase
observes resource use at steady state.

Large fleets, TUI, node power loss and hardware I/O faults remain separate
validation work.
