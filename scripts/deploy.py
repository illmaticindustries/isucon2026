#!/usr/bin/env python3
"""Deploy an origin-pushed revision through Make; never build the working tree."""
import argparse
import datetime
import hashlib
import os
from pathlib import Path
import re
import shlex
import subprocess as sp
import tarfile
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
ALP_URL = 'https://github.com/tkuchiki/alp/releases/download/v1.0.22/alp_linux_amd64.tar.gz'
ALP_SHA256 = 'e5830a70ca064aa89b6ed4715a71591e83e0fe9c85284bbec4208b7d949cbc42'
# Path parameters of webapp/go/server.go routes, most specific first.
ALP_MATCH = ','.join([
    '^/api/admin/users/[^/]+/certification$', '^/api/posts/[^/]+/comments$', '^/api/posts/[^/]+/realmojis$',
    '^/api/friend-requests/[^/]+/[^/]+$', '^/api/(posts|users|blocks|follows|friendships|comments|realmojis|images|memories|feeds)/[^/]+$',
    '^/assets/.+$',
])
HOSTS = {'asia1': '54.248.250.51', 'europe1': '54.248.84.101', 'america1': '52.196.60.188', 'asia2': '52.197.163.28'}

def run(args, **kwargs):
    return sp.run(args, check=True, **kwargs)

def output(args):
    return sp.check_output(args, text=True).strip()

def main():
    p = argparse.ArgumentParser()
    p.add_argument('action', choices=['build', 'deploy', 'preflight', 'rollback', 'status', 'logs', 'deploy-indexes', 'verify-indexes', 'nginx-log', 'alp', 'alp-install', 'nginx-deploy'])
    p.add_argument('--ref', default='HEAD')
    p.add_argument('--hosts', default='asia1 europe1 america1')
    a = p.parse_args()
    names = a.hosts.split()
    if not names or any(n not in HOSTS for n in names):
        p.error('hosts must be registered names: ' + ' '.join(HOSTS))
    if a.action != 'build':
        end = datetime.datetime(2026, 10, 31, 18, tzinfo=datetime.timezone(datetime.timedelta(hours=9)))
        if datetime.datetime.now(datetime.timezone.utc) >= end:
            p.error('competition ended; wait for organizer permission and update this guard explicitly')
    if a.action in ('deploy', 'rollback', 'preflight', 'nginx-deploy') and 'asia2' in names:
        p.error('application actions cannot target the DB server asia2')
    ssh = ['ssh', *shlex.split(os.environ.get('SSH_OPTIONS', '-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes'))]
    def remote(name, command, **kwargs):
        return run(ssh + ['isucon@' + HOSTS[name], command], **kwargs)
    if a.action in ('status', 'logs', 'preflight', 'rollback', 'nginx-log', 'alp'):
        for n in names:
            print('=== ' + n, flush=True)
            if a.action == 'rollback':
                remote(n, 'bash -s -- rollback', input=(ROOT/'scripts/deploy-remote.sh').read_bytes())
            elif a.action == 'preflight':
                remote(n, "test $(uname -m) = x86_64 && command -v flock && command -v curl && sudo -n -l >/dev/null && test -w /home/isucon/webapp/go && test -w /home/isucon/webapp/sql && systemctl is-active isureal-go")
            elif a.action == 'nginx-log':
                remote(n, 'sudo -n tail -n 200 /var/log/nginx/access.log')
            elif a.action == 'alp':
                remote(n, 'sudo -n alp ltsv --file=/var/log/nginx/access.log --sort sum --reverse -o count,method,uri,min,max,sum,avg,p99 -m ' + shlex.quote(ALP_MATCH))
            elif a.action == 'logs':
                remote(n, 'journalctl -u ' + ('postgresql' if n == 'asia2' else 'isureal-go') + ' -n 100 --no-pager')
            else:
                remote(n, '(cat /home/isucon/webapp/.deploy-revision 2>/dev/null || true); systemctl --no-pager status ' + ('postgresql' if n == 'asia2' else 'isureal-go nginx'))
        return
    if a.action == 'alp-install':
        with urllib.request.urlopen(ALP_URL, timeout=30) as r:
            tgz = r.read()
        if hashlib.sha256(tgz).hexdigest() != ALP_SHA256:
            raise RuntimeError('alp checksum mismatch')
        for n in names:
            print('=== ' + n, flush=True)
            remote(n, 'd=$(mktemp -d); trap "rm -rf -- $d" EXIT; tar -xzf - -C "$d" alp && sudo -n install -m 755 "$d/alp" /usr/local/bin/alp && alp --version', input=tgz)
        return
    run(['git', 'fetch', 'origin'], cwd=ROOT)
    rev = output(['git', '-C', str(ROOT), 'rev-parse', '--verify', a.ref + '^{commit}'])
    refs = output(['git', '-C', str(ROOT), 'for-each-ref', '--format=%(refname)', '--contains=' + rev, 'refs/remotes/origin/'])
    if not refs:
        p.error('REF is not reachable from a fetched origin branch; push it first')
    print('Revision: ' + rev, flush=True)
    with tempfile.TemporaryDirectory(prefix='isureal-deploy-') as temp:
        work = Path(temp)
        archive = work/'source.tar'
        with archive.open('wb') as f:
            paths = ['etc/nginx'] if a.action == 'nginx-deploy' else ['webapp/go', 'webapp/sql']
            run(['git', '-C', str(ROOT), 'archive', rev, *paths, 'scripts/deploy-remote.sh'], stdout=f)
        run(['tar', '-xf', str(archive), '-C', str(work)])
        if a.action.endswith('indexes'):
            if names != ['asia2']:
                p.error('index operations require HOSTS=asia2')
            sql = '004-performance-indexes.sql' if a.action == 'deploy-indexes' else 'verify-performance-indexes.sql'
            files = [sql, 'verify-performance-indexes.sql'] if a.action == 'deploy-indexes' else [sql]
            command = '. /home/isucon/env.sh; export PGPASSWORD="$ISUREAL_DB_PASSWORD"; exec psql -X --no-password -v ON_ERROR_STOP=1 -h "$ISUREAL_DB_HOST" -p "$ISUREAL_DB_PORT" -U "$ISUREAL_DB_USER" -d "$ISUREAL_DB_NAME"'
            for f in files:
                remote('asia2', 'bash -eu -c ' + shlex.quote(command), input=(work/'webapp/sql'/f).read_bytes())
            return
        if a.action == 'nginx-deploy':
            payload = work/'nginx.tar'
            with tarfile.open(payload, 'w') as tar:
                tar.add(work/'etc/nginx', arcname='nginx')
            for n in names:
                stage = output(ssh + ['isucon@'+HOSTS[n], 'mktemp -d /home/isucon/.deploy-stage.XXXXXXXX'])
                if not re.fullmatch(r'/home/isucon/\.deploy-stage\.[A-Za-z0-9]+', stage):
                    raise RuntimeError('unexpected stage path')
                try:
                    remote(n, 'cat > ' + stage + '/nginx.tar', input=payload.read_bytes())
                    remote(n, 'bash -s -- nginx ' + stage, input=(work/'scripts/deploy-remote.sh').read_bytes())
                finally:
                    remote(n, 'rm -rf -- ' + shlex.quote(stage))
                print(n + ': nginx config ' + rev, flush=True)
            return
        env = dict(os.environ, GOOS='linux', GOARCH='amd64', CGO_ENABLED='0')
        run(['go', 'test', './...'], cwd=work/'webapp/go')
        run(['go', 'build', '-trimpath', '-o', 'isureal', './cmd/isureal'], cwd=work/'webapp/go', env=env)
        if a.action == 'build':
            print('Pushed revision builds and tests successfully.')
            return
        payload = work/'payload.tar'
        (work/'revision').write_text(rev + '\n')
        with tarfile.open(payload, 'w') as tar:
            for src, dest in [('webapp/go/isureal','go/isureal'), ('webapp/sql/init.sh','sql/init.sh'), ('webapp/sql/reset.sql','sql/reset.sql'), ('revision','revision')]:
                tar.add(work/src, arcname=dest)
        # Stage under a unique directory; the remote lock serializes activation.
        for n in names:
            stage = output(ssh + ['isucon@'+HOSTS[n], 'mktemp -d /home/isucon/.deploy-stage.XXXXXXXX'])
            if not re.fullmatch(r'/home/isucon/\.deploy-stage\.[A-Za-z0-9]+', stage):
                raise RuntimeError('unexpected stage path')
            try:
                remote(n, 'cat > ' + stage + '/payload.tar', input=payload.read_bytes())
                remote(n, 'bash -s -- deploy ' + stage, input=(work/'scripts/deploy-remote.sh').read_bytes())
            finally:
                remote(n, 'rm -rf -- ' + shlex.quote(stage))
            print(n + ': deployed ' + rev, flush=True)

if __name__ == '__main__':
    main()
