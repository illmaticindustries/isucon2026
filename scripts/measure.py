#!/usr/bin/env python3
"""Collect top, pprof, alp, logs and PostgreSQL stats (queries, waits, health) for one benchmark run.

Start right after Enqueue. Read-only except for the profile file saved under ~/pprof on each app host.
"""
from concurrent.futures import ThreadPoolExecutor
import datetime
import os
from pathlib import Path
import shlex
import subprocess as sp

from deploy import ALP_MATCH, HOSTS, ROOT

APPS = ['asia1', 'europe1', 'america1']
DB = 'asia2'
GO = '/home/isucon/local/go/bin/go'
SECONDS = int(os.environ.get('MEASURE_SECONDS', '60'))


def ssh(name, command):
    options = shlex.split(os.environ.get('SSH_OPTIONS', '-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes'))
    result = sp.run(['ssh', *options, 'isucon@' + HOSTS[name], command], capture_output=True, text=True)
    if result.returncode:
        return result.stdout + '\n[exit ' + str(result.returncode) + '] ' + result.stderr
    return result.stdout


def top(name):
    samples = SECONDS // 10 + 1
    return ssh(name, f'top -b -d 10 -n {samples} -w 200 | grep -E "^(top|%Cpu|MiB Mem)|^ *[0-9]+ " | awk "/^top/{{n=0}} {{n++}} n<=8"')


def pprof(name):
    return ssh(name, f'mkdir -p ~/pprof && f=~/pprof/cpu-$(date -u +%Y%m%dT%H%M%SZ).pb.gz'
                     f' && curl -sf -o "$f" "http://127.0.0.1:6060/debug/pprof/profile?seconds={SECONDS}"'
                     f' && echo "saved: $f" && {GO} tool pprof -top -nodecount=25 "$f" 2>/dev/null')


def alp(name):
    return ssh(name, 'sudo -n alp ltsv --file=/var/log/nginx/access.log --sort sum --reverse'
                     ' -o count,method,uri,min,max,sum,avg,p99 -m ' + shlex.quote(ALP_MATCH))


def logs(name):
    # Status codes from the LTSV access log (rotated by bench-reserve), then app errors during this run.
    since = f'{SECONDS + 120} seconds ago'
    return ssh(name, 'echo "# nginx status"; sudo -n grep -o -P "\\tstatus:\\d+" /var/log/nginx/access.log | sort | uniq -c | sort -rn;'
                     f' echo "# isureal-go errors"; journalctl -u isureal-go --since {shlex.quote(since)} --no-pager'
                     ' | grep -iE "error|panic" | cut -c1-200 | sort | uniq -c | sort -rn | head -20')


def psql(name, *queries):
    args = ' '.join('-c ' + shlex.quote(q) for q in queries)
    return ssh(name, 'export LC_ALL=C.UTF-8; . /home/isucon/env.sh && sudo -n -u postgres psql -X -d "$ISUREAL_DB_NAME" ' + args)


def dbwait(name):
    # Each line is one active backend in one 5s sample; lock waits show up as Lock:*.
    query = ("SELECT coalesce(wait_event_type, 'CPU') || ':' || coalesce(wait_event, 'running') FROM pg_stat_activity"
             " WHERE datname = current_database() AND state = 'active' AND pid <> pg_backend_pid()")
    return ssh(name, f'export LC_ALL=C.UTF-8; . /home/isucon/env.sh && for i in $(seq {SECONDS // 5}); do'
                     ' sudo -n -u postgres psql -X -At -d "$ISUREAL_DB_NAME" -c ' + shlex.quote(query) + '; sleep 5; done'
                     ' | sort | uniq -c | sort -rn')


def dbhealth(name):
    # Counters are reset by bench-reserve, so they cover this run.
    return psql(name,
        "SELECT round(100.0 * blks_hit / nullif(blks_hit + blks_read, 0), 2) AS hit_pct, blks_read, temp_files,"
        " pg_size_pretty(temp_bytes) AS temp, deadlocks, xact_commit, xact_rollback, stats_reset"
        " FROM pg_stat_database WHERE datname = current_database()",
        "SELECT checkpoints_timed, checkpoints_req, round(checkpoint_write_time) AS write_ms, round(checkpoint_sync_time) AS sync_ms,"
        " buffers_checkpoint, buffers_backend, stats_reset FROM pg_stat_bgwriter",
        "SELECT (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend') AS connections,"
        " current_setting('max_connections') AS max_connections",
        "SELECT name, current_setting(name) AS value FROM pg_settings WHERE name IN ('shared_buffers', 'effective_cache_size',"
        " 'work_mem', 'max_wal_size', 'checkpoint_timeout', 'random_page_cost', 'jit') ORDER BY name")


def pgstats(name):
    query = ("SELECT calls, round(total_exec_time::numeric) AS total_ms, round(mean_exec_time::numeric, 2) AS mean_ms, rows,"
             " left(regexp_replace(query, '\\s+', ' ', 'g'), 160) AS query"
             " FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 20")
    return psql(name, query)


def main():
    out = ROOT / 'measure' / datetime.datetime.now().strftime('%Y%m%d-%H%M%S')
    out.mkdir(parents=True)
    print(f'Measuring {SECONDS}s of load into {out}', flush=True)
    with ThreadPoolExecutor(max_workers=8) as pool:
        during = {f'top-{n}': pool.submit(top, n) for n in [*APPS, DB]}
        during.update({f'pprof-{n}': pool.submit(pprof, n) for n in APPS})
        during['dbwait-' + DB] = pool.submit(dbwait, DB)
        for key, future in during.items():
            (out / f'{key}.txt').write_text(future.result())
        after = {f'alp-{n}': pool.submit(alp, n) for n in APPS}
        after.update({f'logs-{n}': pool.submit(logs, n) for n in APPS})
        after['pgstats-' + DB] = pool.submit(pgstats, DB)
        after['dbhealth-' + DB] = pool.submit(dbhealth, DB)
        for key, future in after.items():
            (out / f'{key}.txt').write_text(future.result())
    for path in sorted(out.iterdir()):
        print(path.relative_to(ROOT))


if __name__ == '__main__':
    main()
