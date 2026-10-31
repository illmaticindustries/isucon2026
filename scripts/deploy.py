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
from operation_lock import mutation

ROOT = Path(__file__).resolve().parents[1]
ALP_URL = 'https://github.com/tkuchiki/alp/releases/download/v1.0.22/alp_linux_amd64.tar.gz'
ALP_SHA256 = 'e5830a70ca064aa89b6ed4715a71591e83e0fe9c85284bbec4208b7d949cbc42'
# Path parameters of webapp/go/server.go routes, most specific first.
ALP_MATCH = ','.join([
    '^/api/admin/users/[^/]+/certification$', '^/api/posts/[^/]+/comments$', '^/api/posts/[^/]+/realmojis$',
    '^/api/friend-requests/[^/]+/[^/]+$',
    *['^/api/' + r + '/[^/]+$' for r in ['posts', 'users', 'blocks', 'follows', 'friendships', 'comments', 'realmojis', 'images', 'memories', 'feeds']],
    '^/assets/.+$',
])
HOSTS = {'asia1': '54.248.250.51', 'europe1': '54.248.84.101', 'america1': '52.196.60.188', 'asia2': '52.197.163.28'}

def run(args, **kwargs):
    return sp.run(args, check=True, **kwargs)

def output(args):
    return sp.check_output(args, text=True).strip()

def ensure_app_schema(work, remote):
    """Apply only additive schema files bundled in the pushed application REF.

    Called under the same shared mutation reservation, after build/tests and
    before activating any application host. Older REFs need no new tables.
    """
    command = '. /home/isucon/env.sh; export PGPASSWORD="$ISUREAL_DB_PASSWORD"; exec psql -X --no-password -v ON_ERROR_STOP=1 -h "$ISUREAL_DB_HOST" -p "$ISUREAL_DB_PORT" -U "$ISUREAL_DB_USER" -d "$ISUREAL_DB_NAME"'
    for name in ('005-image-cache.sql', '006-notification-batches.sql'):
        path = work / 'webapp/sql' / name
        if path.exists():
            print('asia2: required application schema ' + name, flush=True)
            remote('asia2', 'bash -eu -c ' + shlex.quote(command), input=path.read_bytes())


def main():
    p = argparse.ArgumentParser()
    p.add_argument('action', choices=['regional-deploy', 'build', 'deploy', 'preflight', 'rollback', 'status', 'logs', 'deploy-indexes', 'verify-indexes', 'nginx-log', 'alp', 'enable-pgstats', 'alp-install', 'nginx-deploy', 'nginx-rotate', 'pgstats-reset', 'pg-tune', 'deploy-cache-schema', 'deploy-notification-schema'])
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
    if a.action in ('regional-deploy', 'deploy', 'rollback', 'preflight', 'nginx-deploy', 'nginx-rotate') and 'asia2' in names:
        p.error('application actions cannot target the DB server asia2')
    if a.action in ('deploy-indexes', 'verify-indexes', 'enable-pgstats', 'pgstats-reset', 'pg-tune', 'deploy-cache-schema', 'deploy-notification-schema') and names != ['asia2']:
        p.error('DB operations require HOSTS=asia2')
    if a.action == 'regional-deploy' and names != ['asia1', 'europe1', 'america1']:
        p.error('regional-deploy requires HOSTS="asia1 europe1 america1" in that order')
    ssh = ['ssh', *shlex.split(os.environ.get('SSH_OPTIONS', '-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes'))]
    def remote(name, command, **kwargs):
        return run(ssh + ['isucon@' + HOSTS[name], command], **kwargs)
    def deploy_nginx(work, rev):
        payload = work/'nginx.tar'
        with tarfile.open(payload, 'w') as tar:
            tar.add(work/'etc/nginx', arcname='nginx')
        for n in names:
            stage = output(ssh + ['isucon@'+HOSTS[n], 'mktemp -d /home/isucon/.deploy-stage.XXXXXXXX'])
            if not re.fullmatch(r'/home/isucon/\.deploy-stage\.[A-Za-z0-9]+', stage):
                raise RuntimeError('unexpected stage path')
            try:
                remote(n, 'cat > ' + stage + '/nginx.tar', input=payload.read_bytes())
                remote(n, 'bash -s -- nginx ' + stage + ' ' + n, input=(work/'scripts/deploy-remote.sh').read_bytes())
            finally:
                remote(n, 'rm -rf -- ' + shlex.quote(stage))
            print(n + ': nginx config ' + rev, flush=True)

    def perform():
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
        # Called by bench-reserve while it holds the benchmark reservation.
        if a.action == 'nginx-rotate':
            rotated = 'access.log.' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
            for n in names:
                print('=== ' + n, flush=True)
                remote(n, 'cd /var/log/nginx && if sudo -n test -e access.log; then sudo -n mv access.log ' + rotated + '; fi && sudo -n nginx -s reopen && echo "previous log: /var/log/nginx/' + rotated + '"')
            return
        # Called by bench-reserve; skipped until enable-pgstats has been applied.
        if a.action == 'pgstats-reset':
            remote('asia2', 'export LC_ALL=C.UTF-8; . /home/isucon/env.sh && sudo -n -u postgres psql -X -At -d "$ISUREAL_DB_NAME"'
                            ' -c "SELECT pg_stat_reset()" -c "SELECT pg_stat_reset_shared(\'bgwriter\')" >/dev/null && echo "database and bgwriter stats reset" || echo "database stats reset failed"; { sudo -n -u postgres psql -X -At -d "$ISUREAL_DB_NAME" -c "SELECT pg_stat_statements_reset()" >/dev/null'
                            ' && echo "pg_stat_statements reset" || echo "pg_stat_statements unavailable; skipped"; }')
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
                paths = ['etc/nginx'] if a.action == 'nginx-deploy' else ['webapp/go', 'webapp/sql', 'scripts/enable-pgstats.sh']
                if a.action == 'pg-tune':
                    paths = ['etc/postgresql/16/main/conf.d/tuning.conf']
                if a.action == 'regional-deploy':
                    paths.append('etc/nginx')
                run(['git', '-C', str(ROOT), 'archive', rev, *paths, 'scripts/deploy-remote.sh'], stdout=f)
            run(['tar', '-xf', str(archive), '-C', str(work)])
            if a.action == 'enable-pgstats':
                remote('asia2', 'bash -s', input=(work/'scripts/enable-pgstats.sh').read_bytes())
                return
            if a.action.endswith('indexes') or a.action in ('deploy-cache-schema', 'deploy-notification-schema'):
                sql = '004-performance-indexes.sql' if a.action == 'deploy-indexes' else 'verify-performance-indexes.sql'
                files = [sql, 'verify-performance-indexes.sql'] if a.action == 'deploy-indexes' else [sql]
                if a.action == 'deploy-cache-schema':
                    files = ['005-image-cache.sql']
                elif a.action == 'deploy-notification-schema':
                    files = ['006-notification-batches.sql']
                command = '. /home/isucon/env.sh; export PGPASSWORD="$ISUREAL_DB_PASSWORD"; exec psql -X --no-password -v ON_ERROR_STOP=1 -h "$ISUREAL_DB_HOST" -p "$ISUREAL_DB_PORT" -U "$ISUREAL_DB_USER" -d "$ISUREAL_DB_NAME"'
                for f in files:
                    remote('asia2', 'bash -eu -c ' + shlex.quote(command), input=(work/'webapp/sql'/f).read_bytes())
                return
            if a.action == 'nginx-deploy':
                deploy_nginx(work, rev)
                return
            if a.action == 'pg-tune':
                payload = work/'postgres.tar'
                with tarfile.open(payload, 'w') as tar:
                    tar.add(work/'etc/postgresql/16/main/conf.d/tuning.conf', arcname='postgres/tuning.conf')
                stage = output(ssh + ['isucon@'+HOSTS['asia2'], 'mktemp -d /home/isucon/.deploy-stage.XXXXXXXX'])
                if not re.fullmatch(r'/home/isucon/\.deploy-stage\.[A-Za-z0-9]+', stage):
                    raise RuntimeError('unexpected stage path')
                try:
                    remote('asia2', 'cat > ' + stage + '/postgres.tar', input=payload.read_bytes())
                    remote('asia2', 'bash -s -- postgres ' + stage, input=(work/'scripts/deploy-remote.sh').read_bytes())
                finally:
                    remote('asia2', 'rm -rf -- ' + shlex.quote(stage))
                print('asia2: postgres config ' + rev, flush=True)
                return
            env = dict(os.environ, GOOS='linux', GOARCH='amd64', CGO_ENABLED='0')
            run(['go', 'test', './...'], cwd=work/'webapp/go')
            run(['go', 'build', '-trimpath', '-o', 'isureal', './cmd/isureal'], cwd=work/'webapp/go', env=env)
            if a.action == 'build':
                print('Pushed revision builds and tests successfully.')
                return
            ensure_app_schema(work, remote)
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
            if a.action == 'regional-deploy':
                deploy_nginx(work, rev)

    if a.action in ('regional-deploy', 'deploy', 'rollback', 'deploy-indexes', 'enable-pgstats', 'nginx-deploy', 'alp-install', 'pg-tune', 'deploy-cache-schema', 'deploy-notification-schema'):
        with mutation(f'{a.action} ref={a.ref} hosts={a.hosts}'):
            perform()
    else:
        perform()

if __name__ == '__main__':
    main()
