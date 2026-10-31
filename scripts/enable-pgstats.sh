#!/bin/bash
set -euo pipefail
# Run only on registered asia2; preserve any existing preload entries.
current=$(sudo -n -u postgres psql -X -At -c 'SHOW shared_preload_libraries')
if [[ ",$current," != *pg_stat_statements* ]]; then
 value=${current:+$current,}pg_stat_statements
 sudo -n -u postgres psql -X -v ON_ERROR_STOP=1 -c "ALTER SYSTEM SET shared_preload_libraries = '$value'"
 sudo -n systemctl restart postgresql@16-main
fi
. /home/isucon/env.sh
sudo -n -u postgres psql -X -v ON_ERROR_STOP=1 -d "$ISUREAL_DB_NAME" -c 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements' -c 'SELECT count(*) FROM pg_stat_statements'
