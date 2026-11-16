"""Queue ownership, failure reporting, and the target distributions, without
starting VMs."""
from contextlib import contextmanager, redirect_stdout
import importlib.util
import io
from pathlib import Path
import tempfile
import threading
import unittest

spec = importlib.util.spec_from_file_location('vm_all', Path(__file__).with_name('all.py'))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


@contextmanager
def stand_in_guests(slot, pending):
    """all.py's guests for a test: the remote.json content of a pair that
    is not there."""
    yield {'name': f'pair-{slot}', 'node': {}, 'peer_node': {}}


class RunnerTests(unittest.TestCase):
    def test_queue_parallel_and_failure(self):
        barrier = threading.Barrier(2)
        calls = []
        guard = threading.Lock()

        def execute(arguments, root, log, timeout=None):
            with guard:
                calls.append((arguments[0], root))
            if arguments[0] in {'long', 'medium'}:
                barrier.wait(timeout=5)  # Both slots must work concurrently.
            print('suite evidence', file=log)
            return int(arguments[0] == 'medium')

        suites = tuple((name, (name,)) for name in ['long', 'medium', 'short'])
        with tempfile.TemporaryDirectory() as temporary, redirect_stdout(io.StringIO()) as output:
            root = Path(temporary)
            self.assertEqual(runner.run_suites(root, 2, guests=stand_in_guests, suites=suites, execute=execute), 1)
            self.assertCountEqual([name for name, _ in calls], ['long', 'medium', 'short', 'save-guest-journals'])
            self.assertEqual({path for _, path in calls}, {root / 'slots/1', root / 'slots/2'})
            self.assertEqual({name for name, _ in calls[:2]}, {'long', 'medium'})
            self.assertIn('suite evidence', output.getvalue())
            self.assertIn('medium.log', output.getvalue())
            self.assertIn('3 suites, 1 failures', output.getvalue())

    def test_sequential_order(self):
        calls = []
        def execute(arguments, root, log):
            calls.append(arguments[0])
            return 0
        suites = tuple((name, (name,)) for name in ['long', 'short'])
        with tempfile.TemporaryDirectory() as temporary, redirect_stdout(io.StringIO()):
            self.assertEqual(runner.run_suites(Path(temporary), 1, guests=stand_in_guests, suites=suites, execute=execute), 0)
        self.assertEqual(calls, ['long', 'short'])

    def test_a_suite_that_cannot_start_fails_the_run_and_the_queue_goes_on(self):
        calls = []
        def execute(arguments, root, log):
            calls.append(arguments[0])
            if arguments[0] == 'broken':
                raise OSError('cannot launch')
            return 0
        suites = (('broken', ('broken',)), ('healthy', ('healthy',)))
        with tempfile.TemporaryDirectory() as temporary, redirect_stdout(io.StringIO()) as output:
            self.assertEqual(runner.run_suites(Path(temporary), 1, guests=stand_in_guests, suites=suites, execute=execute), 1)
            self.assertIn('cannot launch', output.getvalue())
            self.assertIn('2 suites, 1 failures', output.getvalue())
        self.assertEqual(calls, ['broken', 'healthy'])


class GuestJournalTests(unittest.TestCase):
    def test_a_failed_suite_saves_the_journals_of_its_guests_before_they_are_deleted(self):
        from contextlib import contextmanager
        events = []

        @contextmanager
        def guests(slot, pending):
            yield {'name': f'pair-{slot}', 'node': {}, 'peer_node': {}}
            events.append('guests deleted')

        def execute(arguments, root, log, timeout=None):
            events.append(arguments[0])
            if arguments[0] == 'save-guest-journals':
                self.assertEqual(arguments[3:], ('--suite', 'fleet'))
                self.assertLessEqual(int(arguments[2]), runner.time.time() - runner.JOURNAL_SECONDS_BEFORE_THE_SUITE)
                self.assertEqual(timeout, runner.JOURNAL_TIME_LIMIT_SECONDS)
                for guest in ('mmwatchdog-dev', 'mmwatchdog-peer'):
                    (root / f'fleet.journal-of-{guest}.log').write_text('mmwatchdog[553]: an alert goes directly\n')
                print('Saved the journals', file=log)
                return 0
            print('AssertionError: the reboot alert went directly', file=log)
            return int(arguments[0] == 'fleet')
        suites = (('fleet', ('fleet',)), ('node', ('node',)))
        with tempfile.TemporaryDirectory() as temporary, redirect_stdout(io.StringIO()) as output:
            root = Path(temporary)
            # A journal that an earlier failure left is not this run's.
            (root / 'slots/1').mkdir(parents=True)
            (root / 'slots/1/node.journal-of-mmwatchdog-peer.log').write_text('an earlier run\n')
            self.assertEqual(runner.run_suites(root, 1, suites=suites, execute=execute, guests=guests), 1)
            self.assertEqual(events, ['fleet', 'save-guest-journals', 'node', 'guests deleted'])
            self.assertEqual(sorted(path.name for path in (root / 'slots/1').glob('*.journal-of-*')),
                             ['fleet.journal-of-mmwatchdog-dev.log', 'fleet.journal-of-mmwatchdog-peer.log'])
            self.assertIn('Saved the journals', (root / 'slots/1/fleet.log').read_text())
        printed = output.getvalue()
        self.assertIn('fleet: slot 1 FAIL', printed)
        self.assertIn('AssertionError: the reboot alert went directly\nJournal of a guest: ', printed)
        self.assertIn('/slots/1/fleet.journal-of-mmwatchdog-peer.log\n', printed)
        self.assertNotIn('Saved the journals', printed)

    def test_journals_that_cannot_be_saved_leave_the_suites_failure_as_it_is(self):
        def execute(arguments, root, log, timeout=None):
            if arguments[0] == 'save-guest-journals':
                raise runner.subprocess.TimeoutExpired(arguments, timeout)
            print('AssertionError: the suite failed', file=log)
            return int(arguments[0] == 'fleet')
        with tempfile.TemporaryDirectory() as temporary, redirect_stdout(io.StringIO()) as output:
            root = Path(temporary)
            self.assertEqual(runner.run_suites(root, 1, guests=stand_in_guests, suites=(('fleet', ('fleet',)),), execute=execute), 1)
            self.assertIn('could not be saved: TimeoutExpired', (root / 'slots/1/fleet.log').read_text())
        self.assertIn('AssertionError: the suite failed\nNot every guest\'s journal was saved', output.getvalue())


class GuestProviderTests(unittest.TestCase):
    def test_new_guests_get_a_remote_file_and_no_down(self):
        from contextlib import contextmanager
        calls, deleted = [], []

        @contextmanager
        def guests(slot, pending):
            yield {'name': f'pair-{slot}', 'node': {}, 'peer_node': {}}
            deleted.append(slot)

        def execute(arguments, root, log):
            remote = runner.json.loads((root / 'remote.json').read_text())
            calls.append((arguments[0], remote['name']))
            return 0
        suites = tuple((name, (name,)) for name in ['long', 'short'])
        with tempfile.TemporaryDirectory() as temporary, redirect_stdout(io.StringIO()) as output:
            root = Path(temporary)
            self.assertEqual(runner.run_suites(root, 1, suites=suites, execute=execute, guests=guests, label='debian13: '), 0)
            self.assertFalse((root / 'slots/1/remote.json').exists())
        self.assertEqual(calls, [('long', 'pair-1'), ('short', 'pair-1')])
        self.assertEqual(deleted, [1])
        self.assertIn('debian13: long: slot 1 PASS', output.getvalue())

    def test_a_slot_takes_new_guests_after_a_suite_that_leaves_them_unfit(self):
        from contextlib import contextmanager
        from unittest.mock import patch
        pairs, calls = [], []

        @contextmanager
        def guests(slot, pending):
            pairs.append(f'pair-{len(pairs) + 1}')
            yield {'name': pairs[-1], 'node': {}, 'peer_node': {}}

        def execute(arguments, root, log):
            calls.append((arguments[0], runner.json.loads((root / 'remote.json').read_text())['name']))
            return 0
        suites = tuple((name, (name,)) for name in ['example', 'fleet', 'node'])
        with patch.dict(runner.SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_GUESTS, {'example': 'it installs servers'}, clear=True), \
                tempfile.TemporaryDirectory() as temporary, redirect_stdout(io.StringIO()):
            self.assertEqual(runner.run_suites(Path(temporary), 1, suites=suites, execute=execute, guests=guests), 0)
        self.assertEqual(calls, [('example', 'pair-1'), ('fleet', 'pair-2'), ('node', 'pair-2')])
        # No new guests once the queue is empty.
        self.assertEqual(pairs, ['pair-1', 'pair-2'])

    def test_suites_that_no_slot_got_guests_for_fail_the_run(self):
        from contextlib import contextmanager

        @contextmanager
        def guests(slot, pending):
            yield None

        def execute(arguments, root, log):
            raise AssertionError('no suite runs without guests')
        suites = (('long', ('long',)),)
        with tempfile.TemporaryDirectory() as temporary, redirect_stdout(io.StringIO()) as output:
            self.assertEqual(runner.run_suites(Path(temporary), 1, suites=suites, execute=execute, guests=guests), 1)
        self.assertIn('1 suites, 0 failures, 1 not run', output.getvalue())


class TargetDistributionsTests(unittest.TestCase):
    def test_a_suite_runs_only_on_its_distributions_and_a_selection_that_runs_nowhere_ends_with_a_message(self):
        from unittest.mock import patch
        suites = (('ubuntu_only', ('ubuntu_only',)), ('node', ('test', '--group', 'node')))
        output = io.StringIO()
        with patch.dict(runner.SUITE_DISTRIBUTIONS, {'ubuntu_only': ('ubuntu2404',)}, clear=True), redirect_stdout(output):
            self.assertEqual(runner.suites_by_distribution(suites, ['debian13', 'ubuntu2404']),
                             {'debian13': suites[1:], 'ubuntu2404': suites})
            # A distribution with nothing to run gets no guests.
            self.assertEqual(runner.suites_by_distribution(suites[:1], ['debian13', 'ubuntu2404']), {'ubuntu2404': suites[:1]})
            with self.assertRaises(SystemExit) as nothing:
                runner.suites_by_distribution(suites[:1], ['debian13'])
        self.assertIn('none of the selected suites runs on debian13', str(nothing.exception))
        self.assertIn('ubuntu_only runs only on ubuntu2404 (SUITE_DISTRIBUTIONS in tests/integration/all.py); '
                      'this run leaves it out on debian13', output.getvalue())

    def test_every_target_distribution_has_a_package_manager_that_the_scenarios_know(self):
        import incus
        import service_scenarios
        import vm
        self.assertEqual(set(incus.PACKAGE_MANAGERS), set(runner.TARGET_DISTRIBUTIONS))
        managers = set(incus.PACKAGE_MANAGERS.values())
        self.assertEqual(managers, set(vm.PACKAGE_MANAGERS.values()))
        for manager in managers:
            with self.subTest(package_manager=manager):
                self.assertIn(manager, incus.PREPARE)
                self.assertIn(manager, vm.INSTALL_COMMANDS)
                self.assertIn(manager, vm.TRUST_STORES)
                self.assertIn(manager, service_scenarios.LAYOUTS)

    def test_every_runner_has_an_image_of_each_target_distribution_for_each_architecture(self):
        import cloud
        import hetzner
        import incus
        targets = set(runner.TARGET_DISTRIBUTIONS)
        self.assertEqual(len(runner.TARGET_DISTRIBUTIONS), len(targets))
        self.assertEqual(set(incus.IMAGES), targets)
        self.assertEqual(set(cloud.IMAGES_FOR_SERVER_ARCHITECTURE), set(hetzner.SERVER_TYPES))
        for architecture, images in cloud.IMAGES_FOR_SERVER_ARCHITECTURE.items():
            with self.subTest(architecture=architecture):
                # Hetzner has no image of Arch Linux or openSUSE Leap, and its images of
                # the Red Hat family wait for MMX's runs of that family on Hetzner.
                self.assertEqual(set(images), targets - {'archlinux', 'opensuseleap161', 'rocky9', 'almalinux10', 'fedora45'})
