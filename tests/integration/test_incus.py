"""The shared guest slots of incus.py; no guests are needed."""
import importlib.util
from pathlib import Path
import threading
import time
import unittest

spec = importlib.util.spec_from_file_location('incus', Path(__file__).with_name('incus.py'))
incus = importlib.util.module_from_spec(spec)
spec.loader.exec_module(incus)


class SharedGuestSlotsTests(unittest.TestCase):
    def test_a_run_alone_takes_every_slot_and_two_runs_share_them(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            for number in range(1, 5):
                (Path(directory) / f'slot-{number}').touch()
            first, second = incus.SharedGuestSlots(directory), incus.SharedGuestSlots(directory)
            taken = [first.acquire(1, 'first', lambda: True) for _ in range(4)]
            self.assertTrue(all(taken))
            self.assertIsNone(second.acquire(1, 'second', lambda: True))
            self.assertEqual((Path(directory) / 'slot-1').read_text(), 'first\n')
            # While the second run waits, the first gives a slot back and may
            # not take it again: its share is two of the four. The second run
            # counts as waiting once it holds its file in runs/.
            got = []

            def second_run():
                got.append(second.acquire(1, 'second', lambda: bool(got)))
            thread = threading.Thread(target=second_run)
            thread.start()
            deadline = time.monotonic() + 60
            while second.own_marker is None:
                self.assertLess(time.monotonic(), deadline, 'the second run never began to wait for a slot')
                time.sleep(0.01)
            first.release(taken.pop())
            self.assertIsNone(first.acquire(1, 'first', lambda: True))
            thread.join(timeout=30)
            self.assertTrue(got and got[0])
            # Once the second run holds nothing and wants nothing, the first
            # takes every slot again.
            second.release(got[0])
            self.assertTrue(first.acquire(1, 'first', lambda: True))
            for held in taken:
                first.release(held)
