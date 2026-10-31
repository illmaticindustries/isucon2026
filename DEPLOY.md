# 競技用デプロイ

公式規則とAGENTS.mdを順守する。競技終了後は運営の許可が出るまで接続・操作しない。

## 手元から

Python 3、Git、Go（go.mod指定版）、OpenSSH、tarが必要。SSH鍵と確認済みknown_hostsを用意する。接続先はscripts/deploy.pyに列挙したポータル登録済み4台。スタックを作り直した場合は、ポータルで確認してこの一覧を更新・プッシュする。

1. 変更をコミットして非公開originへプッシュする。
2. チームのベンチマーク・デプロイと重なっていないことを確認する。
3. `make preflight` で3台の接続・権限・サービスを確認する。
4. ポータルにPENDING・WAITING・RUNNINGがないことを確認し、`make deploy REF=<プッシュ済みコミットSHA> PORTAL_IDLE=yes` で反映する。
5. `make status` で確認し、以下のベンチ予約手順でベンチマークを実行する。

`HOSTS=asia1` で1台だけを指定できる。既定はasia1、europe1、america1の順次反映。SSH設定は `SSH_OPTIONS` で上書きできる。CI経由のログインは必須ではない。

デプロイはoriginをfetchして指定コミットが共有ブランチに含まれることを確認し、一時ディレクトリにgit archiveで展開してテスト・Linux amd64ビルドする。未コミット変更を成果物に含めない。反映対象はGoバイナリ、sql/init.sh、sql/reset.sql。指定REFに005-image-cache.sql・006-notification-batches.sqlが含まれる場合は、ビルドとテストの成功後、アプリを更新する前に同じ共有予約内でasia2へ適用する。既存データと世代を保持する追加テーブルの冪等な作成だけが対象で、001-schema.sqlなど再構築SQLは自動実行しない。HOSTS=asia1などの指定でも追加スキーマの対象は共有DBのasia2。SQL失敗時はアプリ更新を始めず予約を保持する。その他のDB変更、nginx、systemd、環境変数、静的ファイルの変更は対応する明示的なMakeターゲットで行う。

各ホストで排他ロックを取得し、既存ファイルをバックアップしてサービス停止・配置・起動する。稼働確認はサービス状態と未認証GET /api/meの401。DBを含む全機能検証の代わりにはならない。POST /api/initializeは実行しない。

反映失敗時はそのホストを自動復旧し、後続ホストへ進まない。先に成功したホストは新バージョンのままなので、表示された結果を確認して `make rollback HOSTS="asia1 ..." PORTAL_IDLE=yes` で必要なホストだけ戻す。直前バックアップは /home/isucon/.isureal-deploy/previous、反映SHAは /home/isucon/webapp/.deploy-revision。初回の旧バージョンSHAはunknown。ロールバックはDBスキーマを戻さない。プロセス強制終了・接続断などでは手動復旧が必要になる場合がある。

## DBインデックス

`make deploy-indexes REF=<SHA> HOSTS=asia2 PORTAL_IDLE=yes` で共有PostgreSQLにインデックスを適用し、有効性を検証する。アプリ反映とは独立。`make verify-indexes REF=<SHA> HOSTS=asia2` は検証のみ。sql配下の旧Make入口もルートへ委譲する。

## nginx・alp

nginxの設定はリポジトリの `etc/nginx/` が正。`make nginx-deploy REF=<SHA> PORTAL_IDLE=yes` で3台に反映する（共有予約の対象）。各ホストで既存設定をバックアップし、`nginx -t` に失敗したら元に戻してreloadしない。成功時はreloadのみで再起動しない。バックアップは /home/isucon/.isureal-deploy/nginx-backup.*。アクセスログはalp用のLTSV形式で、リモートアドレス・UA・リファラは出力しない。

`make alp-install PORTAL_IDLE=yes` はalp v1.0.22をチェックサム照合のうえ /usr/local/bin/alp に配置する。`make alp` で集計する（クエリ文字列は除外され、パスパラメータは正規化される）。

## PostgreSQL設定

asia2の設定はリポジトリの `etc/postgresql/16/main/conf.d/tuning.conf` が正。`make pg-tune REF=<SHA> PORTAL_IDLE=yes` で共有予約を取得し、既存ファイルをバックアップして配置する。`pg_file_settings` で値の誤りを検出したら元に戻してreloadしない。問題なければreloadし、`shared_buffers` など再起動が必要な設定（`pending_restart`）が変わったときだけ `postgresql@16-main` を再起動する（再起動中の数秒間は3台のアプリからDBに接続できず、バッファキャッシュも空になる）。起動しない場合は元のファイルに戻して再起動する。成功時は `tuning.conf` 由来の設定値と、reload/restartのどちらを行ったかを表示する。バックアップは asia2 の /home/isucon/.isureal-deploy/postgres-backup.*。`fsync`・`synchronous_commit`・`full_page_writes` など永続性に関わる設定は変更しない。

## CI

PR/mainでGo検証。main上の手動Deployワークフローも同じ `make preflight` と `make deploy` を使用し、自動pushデプロイはしない。GitHubの非公開リポジトリに `DEPLOY_SSH_KEY` と `DEPLOY_KNOWN_HOSTS` のSecretsが必要（値をGitやログに含めない）。手元とCIをまたぐ同時実行も避ける。各ホストのロックはあるが、全3台を一括トランザクションにはしない。

## ベンチとデプロイの共有予約

ベンチがPENDING・WAITING・RUNNINGの間は変更禁止。ポータルが確認できない場合も変更しない。`PORTAL_IDLE=yes` は「ポータルで未終了ジョブがないことを直前に確認した」という申告で、自動照会ではない。設定を常時exportしない。

ベンチ担当者は次の順で操作する。

1. ポータルの未終了ジョブがないことを確認する。
2. `make bench-reserve PORTAL_IDLE=yes BENCH_NOTE="今回の比較対象など"` を実行する。予約に失敗したらEnqueueしない。
   予約取得後、asia1・europe1・america1のnginxアクセスログを `access.log.<UTC時刻>` へ退避して `nginx -s reopen` する（削除しない）。あわせてasia2の `pg_stat_statements` をリセットする（未導入ならスキップ）。以後の `make alp` と `make measure` はこのベンチ分だけを集計する。ローテートに失敗した場合は予約を残して停止するので、各ホストを確認してから解除する。
3. 予約成功時のトークンを控え、ポータルで1回Enqueueし、ジョブIDを記録する。
   Enqueue直後に `make measure`（既定60秒、`MEASURE_SECONDS` で変更）を実行する。負荷中のtop（4台）とpprof（アプリ3台）、終了後のalp・nginxステータス内訳・アプリのエラー・`pg_stat_statements` 上位を `measure/<日時>/` に保存する（Git管理外）。読み取りのみで、プロファイルは各アプリサーバの `~/pprof/` に残る。
4. ジョブがFINISHEDまたはFAILになり、他の未終了ジョブもないことを確認する。
5. `make bench-release PORTAL_IDLE=yes OPERATION_TOKEN=<発行されたトークン>` で解除する。Enqueueを取りやめた場合も、未終了ジョブがないことを確認して解除する。

`deploy`、`rollback`、`deploy-indexes`、`enable-pgstats`、`nginx-deploy`、`alp-install` は同じ共有予約を自動取得する。3台全体の作業中は保持し、成功時に解除する。ベンチ予約中・他の変更中は即時エラーとなる。`make enable-pgstats REF=<SHA> PORTAL_IDLE=yes` はDBの再起動を伴うため、この保護の対象。通常の状態・ログ確認とpprof採取は予約を取得しない。

共有状態は登録済みasia2の `/home/isucon/.isureal-operations/state.json` に保存する。`make operation-status` で用途・所有者・開始時刻・トークンを確認できる。アクセスには既存SSH設定を使い、追加ポートや外部サービスは不要。全PC・CIから同一の状態を使う。予約の競合判定はサーバ側flockで直列化し、同時要求でも1件だけを許可する。

通信断・失敗・中断時は遠隔処理が続いている可能性があるため予約を残す。自動タイムアウトや強制解除オプションは設けない。担当者に確認し、ジョブ・遠隔処理とも終了を確認してから、状態に記録されたトークンで解除する。コーディネーターに接続できないときは変更を開始しない。

ポータルの直接Enqueueや古いMakefile、手動SSH操作を技術的に遮断することはできない。チーム全員が予約手順と最新版Makefileを使うこと。ベンチ中の変更禁止はAGENTS.mdにも記載している。
