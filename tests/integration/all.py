#!/usr/bin/env python3
"""The catalog of VM suites, MMWatchdog's target distributions, and the job slots
that run the suites, for incus.py and hetzner.py (Python standard library).
Each job slot runs suites on the two guests that its caller creates for it;
all.py creates no guests of its own."""
from concurrent.futures import ThreadPoolExecutor
from collections import deque
import json
import os
from pathlib import Path
from queue import Empty, Queue
import subprocess
import sys
import threading
import time
import mmx_build
import testenv

REPO = Path(__file__).resolve().parents[2]


def monitoring_setup_suite(name, scenarios_file):
    """The catalog entry of a suite that runs the scenarios of a whole
    monitoring setup (monitoring_setup_scenarios.py) from its scenario file."""
    return (name, ('monitoring-setup-scenarios', '--scenarios-file', str(scenarios_file)))


# Longest expected first; each entry sets up its own prerequisites. The
# example is the longest, so it is always the first suite on its guests,
# which it needs: its setup installs servers that other suites set up their
# own way.
SUITES = (
    monitoring_setup_suite('web_service_fleet_example', REPO / 'tests/integration/web_service_fleet_example/scenarios.json'),
    ('fleet', ('test', '--group', 'fleet')),
    ('deployment', ('deployment',)),
    ('diagnostics', ('test', '--group', 'diagnostics')),
    ('native', ('test', '--group', 'native')),
    ('resources', ('test', '--group', 'resources')),
    ('node', ('test', '--group', 'node')),
    ('services', ('test', '--group', 'services')),
    ('acceptance', ('test', '--group', 'acceptance')),
    ('soak', ('soak', '--prepare')),
)
# MMWatchdog's target distributions, the names that incus.py and hetzner.py take
# with --distribution: supported Debian releases and Ubuntu LTS releases,
# while they receive security fixes without a subscription, and, with MMX's
# names, Rocky Linux 9 and AlmaLinux 10 (the Red Hat family, with SELinux
# enforcing), Arch Linux, and the newest openSUSE Leap. Only incus.py has
# images of the last four.
TARGET_DISTRIBUTIONS = ('debian12', 'debian13', 'ubuntu2404', 'ubuntu2604', 'rocky9', 'almalinux10', 'fedora45', 'archlinux', 'opensuseleap161')
# Suites that run only on some distributions, and why. Every other suite
# runs on each distribution of a run.
SUITE_DISTRIBUTIONS = {
    # The example's application server has a ZFS pool, and Ubuntu is the one
    # target distribution whose kernel packages hold ZFS's kernel module; a
    # test of MMWatchdog builds no kernel module. Its setup also installs Ubuntu's
    # packages by their names. One distribution is enough: the example's
    # checks run the same MMWatchdog code on every distribution, and the node and
    # services suites test what differs between distributions.
    'web_service_fleet_example': ('ubuntu2404',),
}
# Suites that leave their guests unfit for the suites after them, and why.
# The job slot deletes its guests after such a suite and takes new ones for
# its next suite.
SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_GUESTS = {
    'web_service_fleet_example': "the example's setup leaves MariaDB, Redis, memcached, dnsmasq, and Postfix running "
                                 "with its own settings, which other suites set up their own way, and a RAID array, "
                                 "a ZFS pool, and swap space",
}
HARNESS = Path(__file__).with_name('vm.py')
# Lines of several runners in one process, such as incus.py's, stay whole.
OUTPUT_LOCK = threading.Lock()


# Saving the journals of a failed suite's guests ends within this time, also
# when a guest does not answer.
JOURNAL_TIME_LIMIT_SECONDS = 300
# The journals start this long before the suite, for a guest whose clock is
# a little behind this machine's.
JOURNAL_SECONDS_BEFORE_THE_SUITE = 10


def runs_on_distribution(suite, distribution):
    """Whether a suite (an entry of SUITES) runs on a distribution."""
    return distribution in SUITE_DISTRIBUTIONS.get(suite[0], (distribution,))


def suites_by_distribution(suites, distributions):
    """The suites that run on each distribution, for the distributions that
    have any. Print each selected suite that some of the distributions
    leave out; end the program with a message when none of the suites runs
    on any of them."""
    for suite in suites:
        left_out = [d for d in distributions if not runs_on_distribution(suite, d)]
        if left_out:
            print(f'{suite[0]} runs only on {", ".join(SUITE_DISTRIBUTIONS[suite[0]])} (SUITE_DISTRIBUTIONS in '
                  f'tests/integration/all.py); this run leaves it out on {", ".join(left_out)}', flush=True)
    selected = {}
    for distribution in distributions:
        if chosen := tuple(s for s in suites if runs_on_distribution(s, distribution)):
            selected[distribution] = chosen
    if not selected:
        raise SystemExit(f'Nothing to run: none of the selected suites runs on {", ".join(distributions)}. '
                         'Each line above names the distributions where its suite runs.')
    return selected


def run_command(arguments, root, log, timeout=None):
    """Run vm.py with the arguments in the test environment root; after
    timeout seconds the command is ended and subprocess.TimeoutExpired
    raised."""
    env = dict(os.environ, MMWATCHDOG_TEST_ROOT=str(root))
    env.pop(testenv.LOCK_FD, None)
    return subprocess.run([sys.executable, '-u', str(HARNESS), *arguments],
                          env=env, stdout=log, stderr=subprocess.STDOUT, timeout=timeout).returncode


def build_mmwatchdog_and_mmx_once(root):
    """Build MMWatchdog and MMX once into the test environment root, in the
    background while a run's first guests start, for a runner that gives
    each job slot new guests (incus.py, hetzner.py). Return an event that
    is set when the build has ended; a job slot waits for it before its
    first suite. Each suite builds both again into its job slot and finds
    every package in Go's build cache. Otherwise the job slots would all
    compile the same changed packages at the same time, which costs the
    first suite of each slot about 15 seconds. The caller holds the lock of
    root, which the build of MMWatchdog inherits. A failed build is printed, and
    each suite then builds on its own and reports the error."""
    built = threading.Event()

    def build():
        try:
            lock = int(os.environ[testenv.LOCK_FD])
            subprocess.run(['sh', str(REPO / 'scripts/build.sh')], env=dict(os.environ, MMWATCHDOG_TEST_ROOT=str(root)), pass_fds=(lock,),
                           check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True, timeout=600)
            mmx_build.build()
        except Exception as error:
            details = error.stderr if isinstance(error, subprocess.CalledProcessError) else ''
            with OUTPUT_LOCK:
                print(f'The build of MMWatchdog and MMX before the suites failed: {error} {details}', flush=True)
        finally:
            built.set()
    threading.Thread(target=build, daemon=True).start()
    return built


def save_guest_journals(execute, directory, suite, since_unix_time, log):
    """Save the systemd journals of the slot's two guests for a failed suite,
    beside its log (vm.py save-guest-journals), while the guests are still
    there. A failure to save is written to log and never raised: it must
    not hide the suite's own failure. Return the saved journals' paths."""
    try:
        execute(('save-guest-journals', '--since-unix-time', str(int(since_unix_time)), '--suite', suite),
                directory, log, timeout=JOURNAL_TIME_LIMIT_SECONDS)
    except Exception as error:
        print('The journals of the guests could not be saved: ' + repr(error), file=log, flush=True)
    return sorted(directory.glob(suite + '.journal-of-*.log'))


def run_suites(root, jobs, *, guests, suites=SUITES, execute=run_command, label=''):
    """Run the suites in job slots below root; return 1 when one failed or
    no slot could run it.

    guests(slot, pending) is a context manager that gives the slot the
    content of the remote.json of two new guests, or None when the suites
    ran out while the slot waited for them, and deletes the guests when the
    slot is done. label starts each printed line."""
    pending = Queue()
    for suite in suites:
        pending.put(suite)
    started = time.monotonic()

    def last_lines(path, lines):
        with path.open() as log:
            return ''.join(deque(log, maxlen=lines))

    def worker(slot):
        directory = root / 'slots' / str(slot)
        directory.mkdir(parents=True, exist_ok=True)
        results = []
        remote_file = directory / 'remote.json'
        # A slot takes new guests after a suite that leaves them unfit for the
        # suites after it (SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_GUESTS), while
        # suites are left.
        while not pending.empty():
            with guests(slot, pending) as remote:
                if remote is None:
                    return results
                remote_file.write_text(json.dumps(remote, indent=2) + '\n')
                remote_file.chmod(0o600)
                # MMX pins each node by its address, which a new guest can reuse.
                testenv.forget_guests(directory)
                try:
                    while True:
                        try:
                            name, arguments = pending.get_nowait()
                        except Empty:
                            return results
                        path = directory / (name + '.log')
                        # Journals of an earlier failure of this suite would
                        # pass for this run's.
                        for journal in directory.glob(name + '.journal-of-*.log'):
                            journal.unlink()
                        start = time.monotonic()
                        started_at_unix_time = time.time()
                        with path.open('w') as log:
                            try:
                                code = execute(arguments, directory, log)
                            except Exception as error:
                                print(repr(error), file=log)
                                code = 1
                        elapsed = time.monotonic() - start
                        results.append(code == 0)
                        if code:
                            # The end of the suite's log, before the lines
                            # about the journals follow it.
                            failure = last_lines(path, 30)
                            with path.open('a') as log:
                                journals = save_guest_journals(execute, directory, name,
                                                               started_at_unix_time - JOURNAL_SECONDS_BEFORE_THE_SUITE, log)
                        with OUTPUT_LOCK:
                            print(f'{label}{name}: slot {slot} {"PASS" if code == 0 else "FAIL"} {elapsed:.1f}s', flush=True)
                            if code:
                                print(f'Log: {path}', flush=True)
                                print(failure, end='', flush=True)
                                for journal in journals:
                                    print(f'Journal of a guest: {journal}', flush=True)
                                if len(journals) < 2:
                                    print(f'Not every guest\'s journal was saved; the end of {path} says why', flush=True)
                        if name in SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_GUESTS:
                            break
                finally:
                    remote_file.unlink(missing_ok=True)
                    testenv.forget_guests(directory)
        return results

    # Serialize runner invocations without changing the per-slot harness locks.
    with testenv.locked(root):
        with ThreadPoolExecutor(max_workers=jobs) as workers:
            results = [result for batch in workers.map(worker, range(1, min(jobs, len(suites)) + 1)) for result in batch]
    failed = results.count(False)
    # Suites are left over only when no slot got guests to run them.
    not_run = len(suites) - len(results)
    print(f'{label}Total: {len(suites)} suites, {failed} failures, {not_run} not run, '
          f'{time.monotonic() - started:.1f}s wall time', flush=True)
    return int(failed != 0 or not_run != 0)


if __name__ == '__main__':
    sys.exit('all.py creates no guests of its own. Run its catalog of VM suites with '
             'tests/integration/incus.py or tests/integration/hetzner.py, which create the guests.')
