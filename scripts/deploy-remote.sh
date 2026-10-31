#!/bin/bash
set -euo pipefail
base=/home/isucon/webapp
state=/home/isucon/.isureal-deploy
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
*) echo 'Expected deploy or rollback' >&2; exit 2 ;;
esac
