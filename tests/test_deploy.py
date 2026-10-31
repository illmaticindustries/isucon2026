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

if __name__ == '__main__':
    unittest.main()
