"""Exercise activation and automatic rollback with local fake services."""
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]

class DeployTest(unittest.TestCase):
    def exercise(self, fail=False):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            base, state, stage, bins = [root/n for n in ['webapp', 'state', 'stage', 'bin']]
            for p in [base/'go', base/'sql', stage/'source/go', stage/'source/sql', bins]:
                p.mkdir(parents=True, exist_ok=True)
            for name in ['go/isureal', 'sql/init.sh', 'sql/reset.sql']:
                (base/name).write_text('old')
                (stage/'source'/name).write_text('new')
            (stage/'source/revision').write_text('new-sha\n')
            (base/'.deploy-revision').write_text('old-sha\n')
            with tarfile.open(stage/'payload.tar', 'w') as tar:
                for name in ['go/isureal', 'sql/init.sh', 'sql/reset.sql', 'revision']:
                    tar.add(stage/'source'/name, arcname=name)
            scripts = {
                'sudo': '#!/bin/bash\nshift\nif [ "$1" = -l ]; then exit 0; fi\nexec "$@"\n',
                'systemctl': '#!/bin/bash\nif [ "$1" = start ] && [ -f "$FAIL_ONCE" ]; then rm "$FAIL_ONCE"; exit 1; fi\nexit 0\n',
                'curl': '#!/bin/bash\nprintf 401\n',
                # macOS does not ship flock or GNU mv -T.
                'flock': '#!/bin/bash\nexit 0\n',
                'mv': '#!/bin/bash\nif [ "$1" = -Tf ]; then shift; /bin/rm -f "$2"; fi\nexec /bin/mv "$@"\n',
            }
            for n, text in scripts.items():
                (bins/n).write_text(text)
                (bins/n).chmod(0o755)
            marker = root/'fail-once'
            if fail:
                marker.touch()
            env = dict(os.environ, PATH=str(bins)+':'+os.environ['PATH'], FAIL_ONCE=str(marker))
            script = (ROOT/'scripts/deploy-remote.sh').read_text().replace('base=/home/isucon/webapp', 'base='+str(base)).replace('state=/home/isucon/.isureal-deploy', 'state='+str(state))
            result = subprocess.run(['bash', '-s', '--', 'deploy', str(stage)], input=script, text=True, env=env, capture_output=True)
            self.assertEqual(result.returncode != 0, fail, result.stderr + result.stdout)
            for name in ['go/isureal', 'sql/init.sh', 'sql/reset.sql']:
                self.assertEqual((base/name).read_text(), 'old' if fail else 'new')
            self.assertEqual((base/'.deploy-revision').read_text(), 'old-sha\n' if fail else 'new-sha\n')
            if not fail:
                subprocess.run(['bash', '-s', '--', 'rollback'], input=script, text=True, env=env, check=True, capture_output=True)
                self.assertEqual((base/'go/isureal').read_text(), 'old')
                self.assertEqual((base/'.deploy-revision').read_text(), 'old-sha\n')

    def test_success_and_manual_rollback(self):
        self.exercise()

    def test_failed_restart_restores_previous_files(self):
        self.exercise(fail=True)

    def exercise_nginx(self, fail=False, host="asia1", backend_ready=True):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            nginx, state, stage, bins = [root/n for n in ['nginx', 'state', 'stage', 'bin']]
            for p in [nginx/'sites-available', stage/'source/nginx/sites-available', bins]:
                p.mkdir(parents=True, exist_ok=True)
            for name in ['nginx.conf', 'sites-available/isureal.conf']:
                (nginx/name).write_text('old')
                (stage/'source/nginx'/name).write_text('new')
            for region in ['europe', 'america']:
                (stage/'source/nginx/sites-available'/('isureal-'+region+'.conf')).write_text(region)
            with tarfile.open(stage/'nginx.tar', 'w') as tar:
                tar.add(stage/'source/nginx', arcname='nginx')
            scripts = {
                'sudo': '#!/bin/bash\nshift\nexec "$@"\n',
                'curl': '#!/bin/bash\nprintf '+('401' if backend_ready else '503')+'\n',
                'nginx': '#!/bin/bash\nif [ -f "$NGINX_FAIL" ]; then exit 1; fi\nexit 0\n',
                'systemctl': '#!/bin/bash\necho "$@" >> "$RELOADS"\n',
                'flock': '#!/bin/bash\nexit 0\n',
            }
            for n, text in scripts.items():
                (bins/n).write_text(text)
                (bins/n).chmod(0o755)
            marker, reloads = root/'nginx-fail', root/'reloads'
            if fail:
                marker.touch()
            env = dict(os.environ, PATH=str(bins)+':'+os.environ['PATH'], NGINX_FAIL=str(marker), RELOADS=str(reloads))
            script = (ROOT/'scripts/deploy-remote.sh').read_text().replace('nginx=/etc/nginx', 'nginx='+str(nginx)).replace('state=/home/isucon/.isureal-deploy', 'state='+str(state))
            result = subprocess.run(['bash', '-s', '--', 'nginx', str(stage), host], input=script, text=True, env=env, capture_output=True)
            failed = fail or not backend_ready
            self.assertEqual(result.returncode != 0, failed, result.stderr + result.stdout)
            for name in ['nginx.conf', 'sites-available/isureal.conf']:
                expected = 'new' if name == 'nginx.conf' or host == 'asia1' else host[:-1]
                self.assertEqual((nginx/name).read_text(), 'old' if failed else expected)
            self.assertEqual(reloads.exists() and reloads.read_text(), False if failed else 'reload nginx\n')

    def test_nginx_config_reloads(self):
        self.exercise_nginx()

    def test_regional_config_selection(self):
        for host in ['europe1', 'america1']:
            self.exercise_nginx(host=host)

    def test_unready_regional_backend_does_not_switch_ingress(self):
        self.exercise_nginx(host='europe1', backend_ready=False)

    def test_invalid_nginx_config_is_restored(self):
        self.exercise_nginx(fail=True)
    def exercise_postgres(self, fail=False, previous=None):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            pgconf, state, stage, bins = [root/n for n in ['conf.d', 'state', 'stage', 'bin']]
            for p in [pgconf, stage/'source/postgres', bins]:
                p.mkdir(parents=True, exist_ok=True)
            if previous is not None:
                (pgconf/'tuning.conf').write_text(previous)
            (stage/'source/postgres/tuning.conf').write_text('new')
            with tarfile.open(stage/'postgres.tar', 'w') as tar:
                tar.add(stage/'source/postgres', arcname='postgres')
            scripts = {
                # Drop -u/-o/-g options so the fakes run as the current user.
                'sudo': '#!/bin/bash\nshift\nif [ "$1" = -u ]; then shift 2; fi\nexec "$@"\n',
                'install': '#!/bin/bash\nargs=()\nwhile [ $# -gt 0 ]; do case "$1" in -o|-g) shift 2 ;; *) args+=("$1"); shift ;; esac; done\nexec /usr/bin/install "${args[@]}"\n',
                # Fail the first restart while the new file is installed.
                'systemctl': '#!/bin/bash\nif [ "$1" = restart ] && [ -f "$PG_FAIL" ] && grep -q new "$PGCONF/tuning.conf"; then exit 1; fi\nexit 0\n',
                'pg_isready': '#!/bin/bash\nexit 0\n',
                'psql': '#!/bin/bash\necho "shared_buffers = 131072 8kB"\n',
                'flock': '#!/bin/bash\nexit 0\n',
            }
            for n, text in scripts.items():
                (bins/n).write_text(text)
                (bins/n).chmod(0o755)
            marker = root/'pg-fail'
            if fail:
                marker.touch()
            env = dict(os.environ, PATH=str(bins)+':'+os.environ['PATH'], PG_FAIL=str(marker), PGCONF=str(pgconf))
            script = (ROOT/'scripts/deploy-remote.sh').read_text().replace('pgconf=/etc/postgresql/16/main/conf.d', 'pgconf='+str(pgconf)).replace('state=/home/isucon/.isureal-deploy', 'state='+str(state))
            result = subprocess.run(['bash', '-s', '--', 'postgres', str(stage)], input=script, text=True, env=env, capture_output=True)
            self.assertEqual(result.returncode != 0, fail, result.stderr + result.stdout)
            conf = pgconf/'tuning.conf'
            if not fail:
                self.assertEqual(conf.read_text(), 'new')
            elif previous is None:
                self.assertFalse(conf.exists())
            else:
                self.assertEqual(conf.read_text(), previous)

    def test_postgres_tuning_applies(self):
        self.exercise_postgres(previous='old')

    def test_postgres_start_failure_restores_previous_tuning(self):
        self.exercise_postgres(fail=True, previous='old')

    def test_postgres_start_failure_removes_first_tuning(self):
        self.exercise_postgres(fail=True)

if __name__ == '__main__':
    unittest.main()
