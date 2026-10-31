import sys
from pathlib import Path
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
from pool_sample import summarize


def sample(elapsed, count, duration, started='same-process', in_use=2):
    return dict(elapsed_s=elapsed, pid=42, started=started,
                db=dict(WaitCount=count, WaitDuration=duration, InUse=in_use, MaxOpenConnections=30))


class PoolSummaryTest(unittest.TestCase):
    def test_uses_interval_deltas_not_lifetime(self):
        result = summarize([sample(0, 100, 40_000_000_000), sample(10, 120, 70_000_000_000, in_use=30)])
        self.assertTrue(result['available'])
        self.assertEqual(result['wait_count_delta'], 20)
        self.assertEqual(result['wait_seconds_delta'], 30)
        self.assertEqual(result['mean_wait_ms'], 1500)
        self.assertEqual(result['mean_waiting_connections'], 3)
        self.assertEqual(result['max_in_use'], 30)

    def test_restart_and_counter_reset_are_not_zero_wait(self):
        for samples in ([sample(0, 1, 100), sample(10, 2, 200, started='new')],
                        [sample(0, 2, 200), sample(10, 1, 100)], [{'error': '404'}]):
            self.assertFalse(summarize(samples)['available'])

    def test_missing_sample_is_reported(self):
        result = summarize([sample(0, 0, 0), {'error': 'timeout'}, sample(10, 0, 0)])
        self.assertEqual(result['failed_samples'], 1)
        self.assertEqual(result['mean_waiting_connections'], 0)
        self.assertIsNone(result['mean_wait_ms'])
