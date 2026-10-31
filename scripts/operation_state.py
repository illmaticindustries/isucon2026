#!/usr/bin/env python3
"""Persistent team operation reservation, serialized on the coordinator host."""
import fcntl
import json
from pathlib import Path
import sys


def change(root, action, token='', metadata=None):
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (root / 'mutex').open('a') as mutex:
        fcntl.flock(mutex, fcntl.LOCK_EX)
        state_file = root / 'state.json'
        # Corrupt/unreadable state is an error, never interpreted as idle.
        state = json.loads(state_file.read_text()) if state_file.exists() else None
        if action == 'status':
            return state
        if action == 'acquire':
            if state is not None:
                raise RuntimeError('Environment busy: ' + json.dumps(state, ensure_ascii=False))
            if not token or not metadata:
                raise RuntimeError('Reservation requires a token and metadata')
            state = dict(metadata, token=token)
            temp = root / 'state.new'
            temp.write_text(json.dumps(state, ensure_ascii=False) + '\n')
            temp.replace(state_file)
            return state
        if action == 'release':
            if state is None or not token or state.get('token') != token:
                raise RuntimeError('Reservation token does not match; nothing was released')
            state_file.unlink()
            return state
        raise RuntimeError('Unknown operation: ' + action)


if __name__ == '__main__':
    try:
        result = change(Path('/home/isucon/.isureal-operations'), sys.argv[1],
                        sys.argv[2], json.loads(sys.argv[3]))
        print(json.dumps(result, ensure_ascii=False))
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
