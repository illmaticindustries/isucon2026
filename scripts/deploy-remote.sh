#!/bin/bash
set -euo pipefail
base=/home/isucon/webapp
state=/home/isucon/.isureal-deploy
nginx=/etc/nginx
pgconf=/etc/postgresql/16/main/conf.d
mkdir -p "$state"
exec 9>"$state/lock"
flock -n 9 || { echo 'Another deploy is active'; exit 1; }
health() {
    local i code
    for i in {1..15}; do
        code=$(curl --max-time 3 -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/api/me) || code=000
        if systemctl is-active --quiet isureal-go && [ "$code" = 401 ]; then return 0; fi
        sleep 1
    done
    return 1
}
restore() {
    test -f "$state/previous/go/isureal"
    sudo -n systemctl stop isureal-go
    cp -p "$state/previous/go/isureal" "$base/go/isureal"
    cp -p "$state/previous/sql/init.sh" "$base/sql/init.sh"
    cp -p "$state/previous/sql/reset.sql" "$base/sql/reset.sql"
    cp "$state/previous/revision" "$base/.deploy-revision"
    sudo -n systemctl start isureal-go
    health
}
case "${1:-}" in
rollback)
    restore
    echo 'Restored previous application files; database schema was not changed.'
    ;;
deploy)
    stage=$2
    tar -xf "$stage/payload.tar" -C "$stage"
    test -s "$stage/go/isureal"
    test -s "$stage/sql/init.sh"
    test -s "$stage/sql/reset.sql"
    sudo -n -l >/dev/null
    backup=$(mktemp -d "$state/backup.XXXXXXXX")
    mkdir "$backup/go" "$backup/sql"
    cp -p "$base/go/isureal" "$backup/go/isureal"
    cp -p "$base/sql/init.sh" "$backup/sql/init.sh"
    cp -p "$base/sql/reset.sql" "$backup/sql/reset.sql"
    if test -f "$base/.deploy-revision"; then cp "$base/.deploy-revision" "$backup/revision"; else echo unknown > "$backup/revision"; fi
    ln -s "$backup" "$state/previous.new"
    mv -Tf "$state/previous.new" "$state/previous"
    # Roll back this host on installation, restart or readiness failure.
    trap 'rc=$?; trap - EXIT; if [ "$rc" -ne 0 ]; then echo "Deploy failed; restoring previous files"; restore || echo "ROLLBACK FAILED: operator action required" >&2; fi; exit "$rc"' EXIT
    sudo -n systemctl stop isureal-go
    install -m 755 "$stage/go/isureal" "$base/go/isureal"
    install -m 755 "$stage/sql/init.sh" "$base/sql/init.sh"
    install -m 644 "$stage/sql/reset.sql" "$base/sql/reset.sql"
    cp "$stage/revision" "$base/.deploy-revision"
    sudo -n systemctl start isureal-go
    health
    trap - EXIT
    ;;
nginx)
    stage=$2
    host=${3:-asia1}
    tar -xf "$stage/nginx.tar" -C "$stage"
    test -s "$stage/nginx/nginx.conf"
    site=isureal.conf
    case "$host" in
        asia1) ;;
        europe1) test ! -f "$stage/nginx/sites-available/isureal-europe.conf" || site=isureal-europe.conf ;;
        america1) test ! -f "$stage/nginx/sites-available/isureal-america.conf" || site=isureal-america.conf ;;
        *) echo "Unexpected app host" >&2; exit 1 ;;
    esac
    test -s "$stage/nginx/sites-available/$site"
    if [ "$site" != isureal.conf ]; then
        # europe1/america1 serve their own region from the local app; verify it
        # is ready before switching ingress off the previous configuration.
        code=$(curl --max-time 10 -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8080/api/me")
        [ "$code" = 401 ] || { echo "Local backend is not ready: HTTP $code" >&2; exit 1; }
    fi
    backup=$(mktemp -d "$state/nginx-backup.XXXXXXXX")
    cp -p "$nginx/nginx.conf" "$backup/nginx.conf"
    cp -p "$nginx/sites-available/isureal.conf" "$backup/isureal.conf"
    # Keep the running config if the new one fails validation.
    sudo -n install -m 644 "$stage/nginx/nginx.conf" "$nginx/nginx.conf"
    sudo -n install -m 644 "$stage/nginx/sites-available/$site" "$nginx/sites-available/isureal.conf"
    if ! sudo -n nginx -t; then
        sudo -n install -m 644 "$backup/nginx.conf" "$nginx/nginx.conf"
        sudo -n install -m 644 "$backup/isureal.conf" "$nginx/sites-available/isureal.conf"
        echo "nginx -t failed; restored $backup" >&2
        exit 1
    fi
    sudo -n systemctl reload nginx
    echo "nginx reloaded; previous config in $backup"
    ;;
postgres)
    stage=$2
    tar -xf "$stage/postgres.tar" -C "$stage"
    test -s "$stage/postgres/tuning.conf"
    pg_ready() {
        local i
        for i in {1..30}; do
            if systemctl is-active --quiet postgresql@16-main && pg_isready -q -h 127.0.0.1; then return 0; fi
            sleep 1
        done
        return 1
    }
    psql_at() { sudo -n -u postgres psql -X -At -c "$1"; }
    restore() {
        if test -f "$backup/tuning.conf"; then
            sudo -n install -o postgres -g postgres -m 644 "$backup/tuning.conf" "$pgconf/tuning.conf"
        else
            sudo -n rm -f "$pgconf/tuning.conf"
        fi
    }
    backup=$(mktemp -d "$state/postgres-backup.XXXXXXXX")
    if test -f "$pgconf/tuning.conf"; then cp -p "$pgconf/tuning.conf" "$backup/tuning.conf"; fi
    sudo -n install -o postgres -g postgres -m 644 "$stage/postgres/tuning.conf" "$pgconf/tuning.conf"
    # pg_file_settings parses the files on disk, so invalid values are caught before they are applied.
    if [ "$(psql_at "SELECT count(*) FROM pg_file_settings WHERE error IS NOT NULL")" != 0 ]; then
        psql_at "SELECT sourcefile || ':' || sourceline || ' ' || name || ': ' || error FROM pg_file_settings WHERE error IS NOT NULL" >&2
        restore
        echo "Invalid settings; restored $backup without reloading" >&2
        exit 1
    fi
    sudo -n systemctl reload postgresql@16-main
    # Restart only for postmaster settings (e.g. shared_buffers) so the buffer cache stays warm otherwise.
    if [ "$(psql_at "SELECT count(*) FROM pg_settings WHERE pending_restart")" != 0 ]; then
        psql_at "SELECT 'restart required: ' || name FROM pg_settings WHERE pending_restart"
        # PostgreSQL refuses to start on an invalid setting; put the previous file back and start again.
        if ! sudo -n systemctl restart postgresql@16-main || ! pg_ready; then
            restore
            sudo -n systemctl restart postgresql@16-main
            pg_ready || echo 'ROLLBACK FAILED: PostgreSQL is not ready; operator action required' >&2
            echo "PostgreSQL did not start with the new settings; restored $backup" >&2
            exit 1
        fi
        action=restarted
    else
        action=reloaded
    fi
    psql_at "SELECT name || ' = ' || setting || coalesce(' ' || unit, '') FROM pg_settings WHERE sourcefile LIKE '%/tuning.conf' ORDER BY name"
    echo "PostgreSQL $action; previous config in $backup"
    ;;
*) echo 'Expected deploy, rollback, nginx or postgres' >&2; exit 2 ;;
esac
