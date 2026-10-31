"""Executed via SSH on an app host; queries only the loopback diagnostic HTTP API."""
import json
import sys
import time
import urllib.request


def summarize(samples):
    valid = [s for s in samples if 'db' in s]
    if len(valid) < 2:
        return {'available': False, 'reason': 'fewer than two successful samples'}
    if len({(s['pid'], s['started']) for s in valid}) != 1:
        return {'available': False, 'reason': 'application restarted during measurement'}
    for before, after in zip(valid, valid[1:]):
        if any(after['db'][k] < before['db'][k] for k in ('WaitCount', 'WaitDuration')):
            return {'available': False, 'reason': 'pool counters decreased'}
    first, last = valid[0], valid[-1]
    seconds = last['elapsed_s'] - first['elapsed_s']
    if seconds <= 0:
        return {'available': False, 'reason': 'invalid measurement interval'}
    count = last['db']['WaitCount'] - first['db']['WaitCount']
    duration = (last['db']['WaitDuration'] - first['db']['WaitDuration']) / 1e9
    return {'available': True, 'observed_seconds': seconds, 'successful_samples': len(valid),
            'failed_samples': len(samples) - len(valid), 'wait_count_delta': count,
            'wait_seconds_delta': duration,
            'mean_wait_ms': 1000 * duration / count if count else None,
            # Aggregate concurrent waiting can exceed elapsed wall time.
            'mean_waiting_connections': duration / seconds,
            'max_in_use': max(s['db']['InUse'] for s in valid),
            'max_open_limit': max(s['db']['MaxOpenConnections'] for s in valid)}


def collect(seconds):
    samples = []
    start = time.monotonic()
    for offset in sorted(set([*range(0, seconds + 1, 5), seconds])):
        time.sleep(max(0, start + offset - time.monotonic()))
        try:
            with urllib.request.urlopen('http://127.0.0.1:6060/debug/dbstats', timeout=2) as response:
                sample = json.load(response)
        except Exception as error:
            sample = {'error': str(error)}
        sample['elapsed_s'] = time.monotonic() - start
        samples.append(sample)
    return {'samples': samples, 'summary': summarize(samples)}


if __name__ == '__main__':
    seconds = int(sys.argv[1])
    if not 1 <= seconds <= 300:
        raise SystemExit('duration must be 1..300 seconds')
    print(json.dumps(collect(seconds), indent=2))
