"""Verify that benchmarking and mutations cannot own the environment together."""
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import deploy
import operation_lock
from operation_state import change


class StateTest(unittest.TestCase):
    def test_benchmark_blocks_deploy_and_other_benchmarks(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            change(root, 'acquire', 'bench', {'kind': 'benchmark'})
            for kind in ('deploy', 'benchmark'):
                with self.assertRaisesRegex(RuntimeError, 'busy'):
                    change(root, 'acquire', kind, {'kind': kind})
            self.assertEqual(change(root, 'status')['token'], 'bench')
            with self.assertRaisesRegex(RuntimeError, 'does not match'):
                change(root, 'release', 'other-person')
            change(root, 'release', 'bench')
            change(root, 'acquire', 'deploy', {'kind': 'deploy'})
            with self.assertRaisesRegex(RuntimeError, 'busy'):
                change(root, 'acquire', 'bench2', {'kind': 'benchmark'})

    def test_simultaneous_requests_have_one_winner(self):
        with tempfile.TemporaryDirectory() as d:
            def acquire(i):
                try:
                    change(Path(d), 'acquire', str(i), {'kind': 'deploy' if i % 2 else 'benchmark'})
                    return True
                except RuntimeError:
                    return False
            with ThreadPoolExecutor(max_workers=12) as executor:
                self.assertEqual(sum(executor.map(acquire, range(24))), 1)

    def test_corrupt_state_stops_operations(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root/'state.json').write_text('{bad json')
            with self.assertRaises(json.JSONDecodeError):
                change(root, 'acquire', 'new', {'kind': 'deploy'})


class ApplicationSchemaTest(unittest.TestCase):
    def test_old_reference_does_not_apply_unrelated_schema(self):
        with tempfile.TemporaryDirectory() as d:
            work = Path(d)
            sql = work / 'webapp/sql'
            sql.mkdir(parents=True)
            (sql / '001-schema.sql').write_text('DROP TABLE users;')
            with patch.object(deploy, 'run') as remote:
                deploy.ensure_app_schema(work, remote)
                remote.assert_not_called()

    def test_only_bundled_additive_files_apply_in_order(self):
        with tempfile.TemporaryDirectory() as d:
            work = Path(d)
            sql = work / 'webapp/sql'
            sql.mkdir(parents=True)
            (sql / '005-image-cache.sql').write_text('first')
            (sql / '006-notification-batches.sql').write_text('second')
            calls = []
            (sql / '007-user-search.sql').write_text('third')
            (sql / '009-image-storage-external.sql').write_text('fourth')
            deploy.ensure_app_schema(work, lambda host, command, **kwargs: calls.append((host, command, kwargs['input'])))
            self.assertEqual([(c[0], c[2]) for c in calls], [('asia2', b'first'), ('asia2', b'second'), ('asia2', b'third'), ('asia2', b'fourth')])
            self.assertTrue(all('ON_ERROR_STOP=1' in c[1] for c in calls))

    def test_schema_failure_stops_before_next_step(self):
        with tempfile.TemporaryDirectory() as d:
            work = Path(d)
            sql = work / 'webapp/sql'
            sql.mkdir(parents=True)
            (sql / '005-image-cache.sql').write_text('first')
            (sql / '006-notification-batches.sql').write_text('second')
            with patch.object(deploy, 'run', side_effect=subprocess.CalledProcessError(1, 'psql')) as remote:
                with self.assertRaises(subprocess.CalledProcessError):
                    deploy.ensure_app_schema(work, remote)
                self.assertEqual(remote.call_count, 1)


class WorkflowTest(unittest.TestCase):
    def test_portal_confirmation_required_before_network(self):
        with patch.dict(os.environ, {'PORTAL_IDLE': 'no'}), patch.object(operation_lock, 'request') as request:
            with self.assertRaisesRegex(RuntimeError, 'PORTAL_IDLE=yes'):
                with operation_lock.mutation('deploy'):
                    self.fail('mutation must not start')
            request.assert_not_called()

    def test_coordinator_failure_stops_mutation(self):
        with patch.dict(os.environ, {'PORTAL_IDLE': 'yes'}), patch.object(operation_lock, 'request', side_effect=RuntimeError('unreachable')):
            with self.assertRaisesRegex(RuntimeError, 'unreachable'):
                with operation_lock.mutation('deploy'):
                    self.fail('mutation must not start')

    def test_shared_lock_covers_all_hosts(self):
        events = []
        def request(action, *args):
            events.append(action)
        with patch.dict(os.environ, {'PORTAL_IDLE': 'yes'}), patch.object(operation_lock, 'request', side_effect=request), patch.object(deploy, 'run', side_effect=lambda *args, **kwargs: events.append('host')), patch.object(sys, 'argv', ['deploy.py', 'rollback']):
            deploy.main()
        self.assertEqual(events, ['acquire', 'host', 'host', 'host', 'release'])

    def test_failed_remote_operation_keeps_reservation(self):
        events = []
        with patch.dict(os.environ, {'PORTAL_IDLE': 'yes'}), patch.object(operation_lock, 'request', side_effect=lambda action, *args: events.append(action)), patch.object(deploy, 'run', side_effect=subprocess.CalledProcessError(255, 'ssh')), patch.object(sys, 'argv', ['deploy.py', 'rollback']):
            with self.assertRaises(subprocess.CalledProcessError):
                deploy.main()
        self.assertEqual(events, ['acquire'])

    def test_busy_reservation_stops_all_mutation_entry_points(self):
        for action in ('deploy', 'rollback', 'deploy-indexes', 'enable-pgstats', 'pg-tune', 'deploy-cache-schema', 'deploy-notification-schema'):
            with self.subTest(action=action), patch.dict(os.environ, {'PORTAL_IDLE': 'yes'}), patch.object(operation_lock, 'request', side_effect=RuntimeError('busy')), patch.object(deploy, 'run') as run, patch.object(sys, 'argv', ['deploy.py', action, '--hosts', 'asia2' if action in ('deploy-indexes', 'enable-pgstats', 'pg-tune', 'deploy-cache-schema', 'deploy-notification-schema') else 'asia1']):
                with self.assertRaisesRegex(RuntimeError, 'busy'):
                    deploy.main()
                run.assert_not_called()

    def test_bench_reserve_rotates_nginx_logs_after_acquire(self):
        events = []
        with patch.dict(os.environ, {'PORTAL_IDLE': 'yes'}), patch.object(operation_lock, 'request', side_effect=lambda action, *args: events.append(action)), patch.object(operation_lock, 'reset_measurements', side_effect=lambda: events.append('rotate')), patch.object(sys, 'argv', ['operation_lock.py', 'reserve']):
            operation_lock.main()
        self.assertEqual(events, ['acquire', 'rotate'])

    def test_failed_rotation_keeps_bench_reservation(self):
        events = []
        with patch.dict(os.environ, {'PORTAL_IDLE': 'yes'}), patch.object(operation_lock, 'request', side_effect=lambda action, *args: events.append(action)), patch.object(operation_lock, 'reset_measurements', side_effect=subprocess.CalledProcessError(255, 'ssh')), patch.object(sys, 'argv', ['operation_lock.py', 'reserve']):
            with self.assertRaises(subprocess.CalledProcessError):
                operation_lock.main()
        self.assertEqual(events, ['acquire'])

    def test_nginx_rotate_runs_on_app_hosts_without_new_reservation(self):
        commands = []
        with patch.object(operation_lock, 'request') as request, patch.object(deploy, 'run', side_effect=lambda args, **kwargs: commands.append(args[-2:])), patch.object(sys, 'argv', ['deploy.py', 'nginx-rotate', '--hosts', 'asia1 europe1 america1']):
            deploy.main()
        request.assert_not_called()
        self.assertEqual([c[0] for c in commands], ['isucon@' + deploy.HOSTS[n] for n in ('asia1', 'europe1', 'america1')])
        self.assertTrue(all('nginx -s reopen' in c[1] and 'sudo -n mv access.log access.log.' in c[1] for c in commands))


if __name__ == '__main__':
    unittest.main()
