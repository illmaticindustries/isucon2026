#!/bin/bash
# Set up a local streaming read replica on an application host (europe1/america1)
# from the primary database (asia2). Run ON THE REPLICA HOST as the isucon user
# with sudo available. See PR for the required primary-side prep.
#
# Usage: scripts/replica-setup.sh <primary_host>
#   primary_host: asia2 address as seen from this host (e.g. 192.168.4.10)
#
# Primary-side prep (run once on asia2, as postgres):
#   # wal_level=replica (default), max_wal_senders>=1 (default 10)
#   # pg_hba.conf: host replication isureal <replica_cidr> scram-sha-256
#   # reload:  SELECT pg_reload_conf();
#
# After the replica is streaming, make replication synchronous for
# read-your-writes. ONLY after this replica appears in pg_stat_replication:
#   ALTER SYSTEM SET synchronous_standby_names = '1 (any)';
#   ALTER SYSTEM SET synchronous_commit = 'remote_apply';
#   SELECT pg_reload_conf();
set -euo pipefail

primary=${1:?usage: replica-setup.sh <primary_host>}
pgver=16
pgdata=/var/lib/postgresql/${pgver}/main
confdir=/etc/postgresql/${pgver}/main
slot=isureal_replica

. /home/isucon/env.sh
export PGPASSWORD="$ISUREAL_DB_PASSWORD"

# Freeze connectivity so the app keeps working against the primary until the
# replica is streaming; the app only uses it when ISUREAL_DB_REPLICA_HOST is set.
sudo -n systemctl stop "postgresql@${pgver}-main" 2>/dev/null || true

sudo -n -u postgres pg_basebackup \
  -h "$primary" -p "${ISUREAL_DB_PORT:-5432}" -U "$ISUREAL_DB_USER" \
  -D "$pgdata" -Fp -Xs -R -P -S "$slot"

# A replica must not accept writes; the app routes only read-only handlers here.
sudo -n -u postgres tee "$confdir/conf.d/replica.conf" >/dev/null <<'EOF'
hot_standby = on
EOF

sudo -n systemctl start "postgresql@${pgver}-main"

for i in {1..30}; do
  sudo -n -u postgres pg_isready -q -h 127.0.0.1 && break
  sleep 1
done
sudo -n -u postgres psql -X -At -c "SELECT pg_is_in_recovery()"
echo "Replica configured. Set ISUREAL_DB_REPLICA_HOST=$(hostname -I | awk '{print $1}') in /home/isucon/env.sh and restart isureal-go.service."
