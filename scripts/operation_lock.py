#!/usr/bin/env python3
"""Coordinate all PCs and CI through one persistent reservation on asia2."""
import contextlib
import datetime
import getpass
import json
import os
from pathlib import Path
import shlex
import socket
import subprocess
import sys
import uuid

COORDINATOR = 'isucon@52.197.163.28'
SCRIPT = Path(__file__).with_name('operation_state.py')


def require_portal_idle():
    if os.environ.get('PORTAL_IDLE') != 'yes':
        raise RuntimeError('Check the team portal has no PENDING/WAITING/RUNNING job, '
                           'then set PORTAL_IDLE=yes. Unknown status must stop the operation.')


def request(action, token='', metadata=None):
    options = shlex.split(os.environ.get('SSH_OPTIONS',
                         '-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes'))
    command = shlex.join(['python3', '-', action, token, json.dumps(metadata)])
    result = subprocess.run(['ssh', *options, COORDINATOR, command],
                            input=SCRIPT.read_text(), text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or 'Coordinator unreachable; operation refused')
    return json.loads(result.stdout)


def reserve(kind, description):
    require_portal_idle()
    token = str(uuid.uuid4())
    metadata = {'kind': kind, 'owner': getpass.getuser() + '@' + socket.gethostname(),
                'started_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                'description': description}
    # The caller knows the token even if SSH fails after the remote reservation was saved.
    print('Reservation token: ' + token, flush=True)
    request('acquire', token, metadata)
    return token


@contextlib.contextmanager
def mutation(description):
    token = reserve('deploy', description)
    try:
        yield
    except BaseException:
        # A disconnect may leave a remote deploy running. Do not unlock automatically.
        print('Operation failed or interrupted; reservation is retained. '
              'Check all hosts and the portal, then release with OPERATION_TOKEN=' + token,
              file=sys.stderr)
        raise
    else:
        request('release', token)


def main():
    action = sys.argv[1]
    end = datetime.datetime(2026, 10, 31, 9, tzinfo=datetime.timezone.utc)
    if datetime.datetime.now(datetime.timezone.utc) >= end:
        raise RuntimeError('Competition ended; no connections until organizer permission')
    if action == 'reserve':
        token = reserve('benchmark', os.environ.get('BENCH_NOTE', 'portal benchmark'))
        print('Reservation acquired. Enqueue once, record the job ID, and keep this reservation '
              'until the job is FINISHED/FAIL and no job is pending.')
        print('Release: make bench-release PORTAL_IDLE=yes OPERATION_TOKEN=' + token)
    elif action == 'release':
        require_portal_idle()
        request('release', os.environ.get('OPERATION_TOKEN', ''))
        print('Reservation released.')
    elif action == 'status':
        print(json.dumps(request('status'), ensure_ascii=False, indent=2))
    else:
        raise RuntimeError('Unknown action')


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        sys.exit(str(error))
