"""Exercise activation and automatic rollback with local fake services."""
import hashlib
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]

class DeployTest(unittest.TestCase):
    def exercise(self, fail=False, compressed=False):
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
            with tarfile.open(stage/'payload.tar', 'w:gz' if compressed else 'w') as tar:
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

    def test_photo_rollback_restores_exact_snapshot_and_preserves_global_nginx(self):
        old, candidate = 'a' * 40, 'b' * 40
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            base, state, nginx, bins = [root/n for n in ('webapp', 'state', 'nginx', 'bin')]
            for path in (base/'go', base/'sql', nginx/'sites-available', bins):
                path.mkdir(parents=True)
            for relative in ('go/isureal', 'sql/init.sh', 'sql/reset.sql'):
                (base/relative).write_text('old ' + relative)
            (base/'.deploy-revision').write_text(old)
            site = nginx/'sites-available/isureal.conf'
            site.write_text('old active site')
            (nginx/'nginx.conf').write_text('teammate global configuration')
            digest = hashlib.sha256(site.read_bytes()).hexdigest()
            mocks = {
                'flock': 'exit 0',
                'systemctl': 'exit 0',
                'sudo': 'shift; exec "$@"',
                'nginx': 'exit 0',
                'curl': 'printf 401',
                'mv': 'if [ "$1" = -T ]; then shift; fi; exec /bin/mv "$@"',
                'sha256sum': 'read -r hash path; test "$hash" = "$(shasum -a 256 "$path" | cut -d" " -f1)"'
            }
            for name, command in mocks.items():
                (bins/name).write_text('#!/bin/bash\n' + command + '\n')
                (bins/name).chmod(0o755)
            script = (ROOT/'scripts/deploy-remote.sh').read_text().replace(
                'base=/home/isucon/webapp', 'base='+str(base)).replace(
                'state=/home/isucon/.isureal-deploy', 'state='+str(state)).replace('nginx=/etc/nginx', 'nginx='+str(nginx))
            def invoke(*args, check=True):
                return subprocess.run(['bash','-s','--',*args],text=True,input=script,check=check,
                                      env=dict(os.environ,PATH=str(bins)+':'+os.environ['PATH']),capture_output=True)
            self.assertNotEqual(invoke('photo-prepare',candidate,old,'0'*64,check=False).returncode,0)
            invoke('photo-prepare',candidate,old,digest)
            invoke('photo-prepare',candidate,old,digest)
            site.write_text('candidate site')
            (base/'go/isureal').write_text('candidate binary')
            (base/'.deploy-revision').write_text('c'*40)
            self.assertNotEqual(invoke('photo-rollback',candidate,old,check=False).returncode,0)
            (base/'.deploy-revision').write_text(candidate)
            invoke('photo-rollback',candidate,old)
            self.assertEqual((base/'go/isureal').read_text(), 'old go/isureal')
            self.assertEqual((base/'.deploy-revision').read_text(),old)
            self.assertEqual(site.read_text(),'old active site')
            self.assertEqual((nginx/'nginx.conf').read_text(),'teammate global configuration')

    def test_success_and_manual_rollback(self):
        self.exercise()

    def test_failed_restart_restores_previous_files(self):
        self.exercise(fail=True)

    def test_compressed_activation_and_rollback(self):
        self.exercise(compressed=True)
        self.exercise(fail=True, compressed=True)

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
    def exercise_postgres(self, previous=None, pending=False, invalid=False, restart_fails=False):
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
            # The fakes only report invalid/pending settings while the new file is installed.
            new_installed = 'grep -q new "$PGCONF/tuning.conf" 2>/dev/null'
            scripts = {
                # Drop -u/-o/-g options so the fakes run as the current user.
                'sudo': '#!/bin/bash\nshift\nif [ "$1" = -u ]; then shift 2; fi\nexec "$@"\n',
                'install': '#!/bin/bash\nargs=()\nwhile [ $# -gt 0 ]; do case "$1" in -o|-g) shift 2 ;; *) args+=("$1"); shift ;; esac; done\nexec /usr/bin/install "${args[@]}"\n',
                'systemctl': '#!/bin/bash\ncase "$1" in reload|restart) echo "$1" >> "$ACTIONS" ;; esac\nif [ "$1" = restart ] && [ -n "$RESTART_FAILS" ] && ' + new_installed + '; then exit 1; fi\nexit 0\n',
                'pg_isready': '#!/bin/bash\nexit 0\n',
                'psql': '#!/bin/bash\nq="${@: -1}"\ncase "$q" in\n'
                        '*"count(*) FROM pg_file_settings"*) if [ -n "$INVALID" ] && ' + new_installed + '; then echo 1; else echo 0; fi ;;\n'
                        '*"count(*) FROM pg_settings WHERE pending_restart"*) if [ -n "$PENDING" ] && ' + new_installed + '; then echo 1; else echo 0; fi ;;\n'
                        '*) echo "bgwriter_delay = 50 ms" ;;\nesac\n',
                'flock': '#!/bin/bash\nexit 0\n',
            }
            for n, text in scripts.items():
                (bins/n).write_text(text)
                (bins/n).chmod(0o755)
            actions = root/'actions'
            env = dict(os.environ, PATH=str(bins)+':'+os.environ['PATH'], PGCONF=str(pgconf), ACTIONS=str(actions),
                       PENDING='1' if pending else '', INVALID='1' if invalid else '', RESTART_FAILS='1' if restart_fails else '')
            script = (ROOT/'scripts/deploy-remote.sh').read_text().replace('pgconf=/etc/postgresql/16/main/conf.d', 'pgconf='+str(pgconf)).replace('state=/home/isucon/.isureal-deploy', 'state='+str(state))
            result = subprocess.run(['bash', '-s', '--', 'postgres', str(stage)], input=script, text=True, env=env, capture_output=True)
            fail = invalid or restart_fails
            self.assertEqual(result.returncode != 0, fail, result.stderr + result.stdout)
            conf = pgconf/'tuning.conf'
            if not fail:
                self.assertEqual(conf.read_text(), 'new')
            elif previous is None:
                self.assertFalse(conf.exists())
            else:
                self.assertEqual(conf.read_text(), previous)
            return actions.read_text().split() if actions.exists() else []

    def test_postgres_sighup_settings_reload_without_restart(self):
        self.assertEqual(self.exercise_postgres(previous='old'), ['reload'])

    def test_postgres_postmaster_settings_restart(self):
        self.assertEqual(self.exercise_postgres(previous='old', pending=True), ['reload', 'restart'])

    def test_postgres_invalid_settings_restore_without_reload(self):
        self.assertEqual(self.exercise_postgres(previous='old', invalid=True), [])

    def test_postgres_start_failure_restores_previous_tuning(self):
        self.assertEqual(self.exercise_postgres(previous='old', pending=True, restart_fails=True), ['reload', 'restart', 'restart'])

    def test_postgres_start_failure_removes_first_tuning(self):
        self.exercise_postgres(pending=True, restart_fails=True)

if __name__ == '__main__':
    unittest.main()
