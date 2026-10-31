import sys
from pathlib import Path
from threading import Barrier, Event, Thread
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
from deploy import run_hosts


class ParallelDeployTest(unittest.TestCase):
    def test_hosts_overlap_and_all_complete(self):
        barrier = Barrier(3)
        done = []
        def action(host):
            barrier.wait(timeout=3)
            done.append(host)
        run_hosts(['asia1', 'europe1', 'america1'], action)
        self.assertCountEqual(done, ['asia1', 'europe1', 'america1'])

    def test_failure_waits_for_other_hosts_before_raising(self):
        started, finish, failed, returned = Event(), Event(), Event(), Event()
        errors = []
        def action(host):
            if host == 'bad':
                started.wait(3)
                failed.set()
                raise RuntimeError('activation failed')
            started.set()
            if not finish.wait(3):
                raise RuntimeError('test timed out')
        def deploy():
            try:
                run_hosts(['bad', 'slow'], action)
            except RuntimeError as exc:
                errors.append(str(exc))
            finally:
                returned.set()
        thread = Thread(target=deploy)
        thread.start()
        try:
            self.assertTrue(failed.wait(3))
            self.assertFalse(returned.wait(0.05))
        finally:
            finish.set()
            thread.join(3)
        self.assertTrue(returned.is_set())
        self.assertEqual(errors, ['host operations failed: bad'])

    def test_serial_fallback_and_duplicate_rejection(self):
        done = []
        run_hosts(['a', 'b'], done.append, workers=1)
        self.assertEqual(done, ['a', 'b'])
        with self.assertRaises(ValueError):
            run_hosts(['a', 'a'], done.append)
        self.assertEqual(done, ['a', 'b'])
