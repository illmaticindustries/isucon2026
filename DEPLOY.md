# 競技用デプロイ

公式規則とAGENTS.mdを順守する。競技終了後は運営の許可が出るまで接続・操作しない。

## 手元から

Python 3、Git、Go（go.mod指定版）、OpenSSH、tarが必要。SSH鍵と確認済みknown_hostsを用意する。接続先はscripts/deploy.pyに列挙したポータル登録済み4台。スタックを作り直した場合は、ポータルで確認してこの一覧を更新・プッシュする。

1. 変更をコミットして非公開originへプッシュする。
2. チームのベンチマーク・デプロイと重なっていないことを確認する。
3. `make preflight` で3台の接続・権限・サービスを確認する。
4. `make deploy REF=<プッシュ済みコミットSHA>` で反映する。
5. `make status` で確認し、ポータルからベンチマークを実行する。

`HOSTS=asia1` で1台だけを指定できる。既定はasia1、europe1、america1の順次反映。SSH設定は `SSH_OPTIONS` で上書きできる。CI経由のログインは必須ではない。

デプロイはoriginをfetchして指定コミットが共有ブランチに含まれることを確認し、一時ディレクトリにgit archiveで展開してテスト・Linux amd64ビルドする。未コミット変更を成果物に含めない。反映対象はGoバイナリ、sql/init.sh、sql/reset.sql。DBマイグレーション、nginx、systemd、環境変数、静的ファイルは自動変更しない。これらの変更が必要な場合は別の明示的なMakeターゲットを追加する。

各ホストで排他ロックを取得し、既存ファイルをバックアップしてサービス停止・配置・起動する。稼働確認はサービス状態と未認証GET /api/meの401。DBを含む全機能検証の代わりにはならない。POST /api/initializeは実行しない。

反映失敗時はそのホストを自動復旧し、後続ホストへ進まない。先に成功したホストは新バージョンのままなので、表示された結果を確認して `make rollback HOSTS="asia1 ..."` で必要なホストだけ戻す。直前バックアップは /home/isucon/.isureal-deploy/previous、反映SHAは /home/isucon/webapp/.deploy-revision。初回の旧バージョンSHAはunknown。ロールバックはDBスキーマを戻さない。プロセス強制終了・接続断などでは手動復旧が必要になる場合がある。

## DBインデックス

`make deploy-indexes REF=<SHA> HOSTS=asia2` で共有PostgreSQLにインデックスを適用し、有効性を検証する。アプリ反映とは独立。`make verify-indexes REF=<SHA> HOSTS=asia2` は検証のみ。sql配下の旧Make入口もルートへ委譲する。

## nginx・alp

nginxの設定はリポジトリの `etc/nginx/` が正。`make nginx-deploy REF=<SHA>` で3台に反映する。各ホストで既存設定をバックアップし、`nginx -t` に失敗したら元に戻してreloadしない。成功時はreloadのみで再起動しない。バックアップは /home/isucon/.isureal-deploy/nginx-backup.*。アクセスログはalp用のLTSV形式で、リモートアドレス・UA・リファラは出力しない。

`make alp-install` はalp v1.0.22をチェックサム照合のうえ /usr/local/bin/alp に配置する。`make alp` で集計する（クエリ文字列は除外され、パスパラメータは正規化される）。

## CI

PR/mainでGo検証。main上の手動Deployワークフローも同じ `make preflight` と `make deploy` を使用し、自動pushデプロイはしない。GitHubの非公開リポジトリに `DEPLOY_SSH_KEY` と `DEPLOY_KNOWN_HOSTS` のSecretsが必要（値をGitやログに含めない）。手元とCIをまたぐ同時実行も避ける。各ホストのロックはあるが、全3台を一括トランザクションにはしない。
