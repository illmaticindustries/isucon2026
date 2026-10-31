HOST ?= isu2026-1
APP := isureal

# 公式レギュレーション・当日マニュアルとルートの AGENTS.md を順守する。
# 変更はリポジトリへプッシュし、プッシュ済みコミットを Makefile から反映する。
# 現在のルートターゲットはビルド・調査用。DBインデックス反映は webapp/sql/Makefile。

# ローカルでクロスビルドする（このターゲットだけではデプロイしない）
.PHONY: go-build-local
go-build-local:
	cd ./webapp/go && GOOS=linux GOARCH=amd64 go build -o $(APP) ./cmd/$(APP)

# サーバから設定ファイルを取得する（読み取りのみ・sudo なし・秘密情報を含む env.sh は取得しない）
.PHONY: get-conf
get-conf:
	mkdir -p ./etc/mysql/mysql.conf.d ./etc/nginx/sites-enabled
	ssh $(HOST) "cat /etc/mysql/mysql.conf.d/mysqld.cnf" > ./etc/mysql/mysql.conf.d/mysqld.cnf
	sleep 1
	ssh $(HOST) "cat /etc/nginx/nginx.conf" > ./etc/nginx/nginx.conf
	sleep 1
	ssh $(HOST) "cat /etc/nginx/sites-enabled/$(APP).conf" > ./etc/nginx/sites-enabled/$(APP).conf

.PHONY: deployed-version
deployed-version:
	ssh $(HOST) "cd /home/isucon && git rev-parse HEAD"

.PHONY: status
status:
	ssh $(HOST) "systemctl status $(APP)-go.service nginx.service mysql.service --no-pager"

.PHONY: journalctl
journalctl:
	ssh $(HOST) "journalctl -u $(APP)-go.service -n 200 --no-pager"

.PHONY: nginx-log
nginx-log:
	ssh $(HOST) "tail -n 200 /var/log/nginx/access.log"

.PHONY: pt-query-digest
pt-query-digest:
	ssh $(HOST) "pt-query-digest --limit 10 /var/log/mysql/mysql-slow.log"

ALPSORT=sum
# TODO: isureal のエンドポイントに合わせて正規化パターンを調整する
ALPM=/assets/.+,/images/.+
OUTFORMAT=count,method,uri,min,max,sum,avg,p99

.PHONY: alp
alp:
	ssh $(HOST) "alp ltsv --file=/var/log/nginx/access.log --nosave-pos --sort $(ALPSORT) --reverse -o $(OUTFORMAT) -m $(ALPM) -q"
