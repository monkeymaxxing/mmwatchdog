"""Checks for the configuration files of the guests and for test isolation;
no VMs are needed."""
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import testenv
import vm


WORKER = """
import sys
from pathlib import Path
import testenv
with testenv.locked(Path(sys.argv[1])):
    print('acquired', flush=True)
    sys.stdin.readline()
"""


# A test command that holds the lock and has started a process of its own.
# It is a file, because the lock records the command line that started it.
HOLDER_WITH_A_PROCESS_OF_ITS_OWN = """
import subprocess
import sys
from pathlib import Path
import testenv
with testenv.locked(Path(sys.argv[1])):
    started = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
    print(started.pid, flush=True)
    sys.stdin.readline()
"""


class ConfigurationFileTests(unittest.TestCase):
    def test_settings_and_check_groups_are_separate_without_mutating_fixture(self):
        configuration = {'keep_undelivered_alerts_for': '24h', 'undelivered_alerts_content_size_limit': '8MiB', 'socket': '/run/example.sock',
                         'alert_severity_when_this_node_goes_silent': 'warn',
                         'alert_severity_when_this_node_has_rebooted': 'info',
                         'keep_measurement_history_for': '7d',
                         'measurement_history_size_limit': '256MiB',
                         'join_fleet_hub': {'join_code_file': '/etc/mmwatchdog/secrets/join'},
                         'check_groups': {'example': {'check_every': '1s', 'time_limit_per_run': '500ms',
                                                      'alert_after_failing_for': '1m', 'recovered_after_healthy_for': '30s',
                                                      'shell': 'true', 'alert_severity': 'warn'}}}
        original = json.loads(json.dumps(configuration))
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            vm.write_configuration_files(directory, configuration)
            settings = json.loads((directory / 'mmwatchdog_node_settings.yml').read_text())
            groups = json.loads((directory / 'check_groups/node.yml').read_text())
            self.assertNotIn('check_groups', settings)
            self.assertEqual(dict(settings, **groups), original)
            self.assertFalse((directory / 'alert_destinations.yml').exists())
        self.assertEqual(configuration, original)


class GuestsTests(unittest.TestCase):
    def test_a_suite_without_guests_refuses_to_run_and_names_the_scripts_that_create_them(self):
        with tempfile.TemporaryDirectory() as temporary:
            env = dict(os.environ, MMWATCHDOG_TEST_ROOT=temporary)
            env.pop(testenv.LOCK_FD, None)
            result = subprocess.run([sys.executable, Path(vm.__file__), 'test', '--group', 'node'], env=env,
                                    capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 1)
        self.assertIn('remote.json does not exist', result.stderr)
        self.assertIn('tests/integration/incus.py, tests/integration/hetzner.py, or tests/integration/cloud.py', result.stderr)


class EnvironmentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def worker(self, root, extra_env=None):
        env = dict(os.environ)
        env.pop(testenv.LOCK_FD, None)
        env.update(extra_env or {})
        process = subprocess.Popen([sys.executable, '-u', '-c', WORKER, str(root)],
                                   cwd=Path(__file__).parent, env=env, text=True,
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE)
        self.addCleanup(self.stop, process)
        return process

    def stop(self, process):
        if process.poll() is None:
            process.kill()
        process.wait(timeout=5)
        for pipe in (process.stdin, process.stdout, process.stderr):
            pipe.close()

    def line(self, pipe):
        self.assertTrue(select.select([pipe], [], [], 5)[0], 'child did not respond')
        return pipe.readline().strip()

    def release(self, process):
        process.stdin.write('\n')
        process.stdin.flush()
        self.assertEqual(process.wait(timeout=5), 0)

    def test_same_environment_waits_other_environment_runs(self):
        owner = self.worker(self.root / 'mmx')
        self.assertEqual(self.line(owner.stdout), 'acquired')
        waiter = self.worker(self.root / 'mmx')
        message = self.line(waiter.stderr)
        self.assertIn('Waiting for test environment', message)
        self.assertIn(f'PID {owner.pid}', message)
        self.assertFalse(select.select([waiter.stdout], [], [], 0)[0])
        independent = self.worker(self.root / 'mmwatchdog')
        self.assertEqual(self.line(independent.stdout), 'acquired')
        self.release(independent)
        self.release(owner)
        self.assertEqual(self.line(waiter.stdout), 'acquired')
        self.release(waiter)

    def test_owner_death_releases_lock_without_removing_file(self):
        owner = self.worker(self.root)
        self.assertEqual(self.line(owner.stdout), 'acquired')
        waiter = self.worker(self.root, {testenv.LOCK_FD: '999999'})
        self.assertIn('Waiting', self.line(waiter.stderr))
        owner.kill()
        owner.wait(timeout=5)
        self.assertEqual(self.line(waiter.stdout), 'acquired')
        self.release(waiter)
        self.assertTrue((self.root / '.test.lock').exists())

    def test_symlink_alias_shares_lock(self):
        directory = self.root / 'real'
        directory.mkdir()
        alias = self.root / 'alias'
        alias.symlink_to(directory, target_is_directory=True)
        owner = self.worker(directory)
        self.assertEqual(self.line(owner.stdout), 'acquired')
        waiter = self.worker(alias)
        self.assertIn('Waiting', self.line(waiter.stderr))
        self.release(owner)
        self.assertEqual(self.line(waiter.stdout), 'acquired')
        self.release(waiter)

    def test_build_shares_suite_lock_and_uses_selected_root(self):
        fake_bin = self.root / 'tools'
        fake_bin.mkdir()
        go = fake_bin / 'go'
        go.write_text('#!/bin/sh\nset -eu\nwhile [ "$1" != "-o" ]; do shift; done\n'
                      'shift\nprintf fixture > "$1"\n')
        go.chmod(0o755)
        env = dict(vm.ENV, MMWATCHDOG_TEST_ROOT=str(self.root),
                   PATH=str(fake_bin) + os.pathsep + os.environ['PATH'])
        with patch.object(vm, 'ROOT', self.root), patch.object(vm, 'ENV', env):
            with testenv.locked(self.root):
                built = vm.cmd(['sh', vm.REPO / 'scripts/build.sh'],
                               timeout=10, inherit_lock=True)
                self.assertEqual(built.returncode, 0)
                for name in ['mmwatchdog', 'mmwatchdog-linux-arm64', 'mmwatchdog-linux-amd64']:
                    self.assertEqual((self.root / 'bin' / name).read_text(), 'fixture')
                # An ordinary command must not pass the lease to SSH daemons.
                clean = vm.cmd([sys.executable, '-c',
                                f'import os; print({testenv.LOCK_FD!r} in os.environ)'])
                self.assertEqual(clean.stdout.strip(), 'False')
                waiter = self.worker(self.root)
                self.assertIn('Waiting', self.line(waiter.stderr))
            self.assertEqual(self.line(waiter.stdout), 'acquired')
            self.release(waiter)

    def test_stop_run_ends_the_lock_holder_and_what_it_started_and_nothing_else(self):
        script = self.root / 'holder.py'
        script.write_text(HOLDER_WITH_A_PROCESS_OF_ITS_OWN)
        env = dict(os.environ, PYTHONPATH=str(Path(__file__).parent))
        env.pop(testenv.LOCK_FD, None)
        holder = subprocess.Popen([sys.executable, '-u', script, self.root / 'mine'], env=env, text=True,
                                  stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop, holder)
        started_by_holder = int(self.line(holder.stdout))
        other = self.worker(self.root / 'other')
        self.assertEqual(self.line(other.stdout), 'acquired')

        self.assertIn('holder.py', testenv.stop_run(self.root / 'mine', seconds_until_kill=5))

        self.assertIsNotNone(holder.wait(timeout=5))
        self.assertEqual(testenv.still_running([holder.pid, started_by_holder]), [])
        self.assertIsNone(testenv.lock_holder(self.root / 'mine'))
        # The other environment's command still runs and still holds its lock.
        self.assertIsNone(other.poll())
        self.assertEqual(testenv.lock_holder(self.root / 'other')[0], other.pid)
        self.release(other)

    def test_stop_run_without_a_holder_ends_nothing(self):
        self.assertIsNone(testenv.stop_run(self.root / 'never-used'))
        owner = self.worker(self.root)
        self.assertEqual(self.line(owner.stdout), 'acquired')
        self.release(owner)
        self.assertIsNone(testenv.stop_run(self.root))

    def test_stop_run_refuses_a_process_that_is_not_the_named_command(self):
        # The lock is held, but the process ID in it now belongs to another
        # command, as after the holder ended while a command it started kept the lock.
        bystander = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
        self.addCleanup(lambda: (bystander.kill(), bystander.wait(timeout=5)))
        with testenv.locked(self.root):
            (self.root / '.test.lock').write_text(f'PID {bystander.pid}: vm.py test\n')
            with self.assertRaisesRegex(RuntimeError, 'still holds the lock'):
                testenv.stop_run(self.root)
        self.assertIsNone(bystander.poll())

    def test_wrapper_propagates_exit_status(self):
        result = subprocess.run([sys.executable, Path(testenv.__file__),
                                 '--root', self.root, '--', sys.executable,
                                 '-c', 'raise SystemExit(7)'], timeout=5)
        self.assertEqual(result.returncode, 7)
        owner = self.worker(self.root)
        self.assertEqual(self.line(owner.stdout), 'acquired')
        self.release(owner)


if __name__ == '__main__':
    unittest.main()
