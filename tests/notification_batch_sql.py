"""Emit SELECT-only PostgreSQL fixtures for the actual notification query.
Usage: python3 tests/notification_batch_sql.py | psql -X -At -v ON_ERROR_STOP=1 <test-db> | python3 tests/notification_batch_sql.py --check
Only CTE values are used: no production data or schema is read or changed.
A moved user remains a candidate here; the handler's session-region guard closes
that stream, covered separately by TestNotificationBatchLifecycle.
"""
from pathlib import Path
import re,json,sys
s=(Path(__file__).resolve().parents[1]/'webapp/go/notification_api.go').read_text()
q=re.search(r'GetContext\(ctx, &row, `(.+?)`',s,re.S)[1]
q=q.replace('$1',"'u1'").replace('$2',"TIMESTAMPTZ '2026-10-31 00:00:00+00'")
cases=[('active','active','asia','NULL','00:00:01',True),('ended','active','asia',"TIMESTAMPTZ '2026-10-31 00:00:02+00'",'00:00:01',False),('before-connect','active','asia','NULL','00:00:00',False),('inactive','deleted','asia','NULL','00:00:01',False),('moved','active','europe','NULL','00:00:01',True)]
scripts=[]
for label,status,region,ended,created,expected in cases:
 cte=f"""WITH users(id,region,status) AS (VALUES ('u1'::text,'{region}'::text,'{status}'::text)),
 image_cache_generation(generation) AS (VALUES(1::bigint)),
 posting_windows(id,region,ended_at) AS (VALUES ('w1'::text,'asia'::text,{ended}::timestamptz)),
 notifications(id,user_id,posting_window_id,created_at) AS (SELECT NULL::text,NULL::text,NULL::text,NULL::timestamptz WHERE false),
 notification_batches(posting_window_id,created_at) AS (VALUES('w1'::text,TIMESTAMPTZ '2026-10-31 {created}+00')),
 result AS ({q})
 SELECT json_build_object('case','{label}','matches',COALESCE((SELECT jsonb_array_length(batch_candidates)>0 FROM result),false),'rows',(SELECT count(*) FROM result));"""
 scripts.append(cte)
if '--check' not in sys.argv:
 print('\n'.join(scripts))
 raise SystemExit(0)
output=sys.stdin.read()
results=[json.loads(line) for line in output.splitlines()]
for row,case in zip(results,cases):
 assert row['matches']==case[-1],row
 assert row['rows']==(0 if case[1]=='deleted' else 1),row
 print(row)
assert len(results)==len(cases)
