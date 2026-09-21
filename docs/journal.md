# 作業ログ (ISUCON14 / ISURIDE, 2026-09-22〜)

再現性のため、**実行したコマンドと結果を時系列で記録する**。失敗した試行も消さない。

- 1エントリ = 1アクション。`### HH:MM 何をしたか` の見出しで始める。
- コマンドは**コピペで再実行できる形**で書く。
- ベンチのスコアは `scores/log.md` が正。ここには「何をしたか・なぜか」を書く。

---

## 2026-09-22

### 00:38 環境作成

```sh
aws login --profile personal
cd ~/ghq/github.com/kyosu-1/isuenv && go install .     # isuenv bench が入っている feat/bench-command ブランチ
AWS_PROFILE=personal isuenv up isucon14 --nodes 3 --bench-instance-type c5.xlarge --ttl 8h
```

| ノード | 役割 | タイプ | private |
| --- | --- | --- | --- |
| isucon14-1 | 競技（初期状態では nginx + Go + MySQL 全部入り。ベンチの入口） | c5.large | 10.100.0.235 |
| isucon14-2 | 競技 | c5.large | 10.100.0.249 |
| isucon14-3 | 競技 | c5.large | 10.100.0.94 |
| isucon14-4 | ベンチマーカー専用 | c5.xlarge | 10.100.0.90 |

AMI: `ami-0fcf9e8e8675a9ee4 (isucon14-20260818100152)`、MySQL 8.0.46、Ubuntu 24.04。
EC2の vCPU クォータは 64（`L-1216C47A`）で、4台構成（2+2+2+4=10 vCPU）は問題なし。

ベンチ機のアプリ用サービスは止めた（CPUを取られないため）:

```sh
./tools/setup/bench-node.sh isucon14-4
```

### 00:40 素のベースライン（計測なし）

`isuenv bench isucon14` が出したコマンドをそのまま実行。

```sh
ssh isucon14-4 'cd /home/isucon && sudo -u isucon ./bench run --addr 10.100.0.235:443 --target https://isuride.xiv.isucon.net --payment-url http://10.100.0.90:12346 --payment-bind-port 12346'
```

- `pass=true スコア=1097 種別エラー数=map[26:1]`
- WARN 1件: `total_distanceの反映が遅いデータがあります`（CODE=26、owner/chairs の3秒猶予超え）
- 不満率: matching 100% / pickup 81.8% / ride 100%
- 新規登録は評判経由10人、招待0人。離脱2人。

ベンチの出力形式: stdout に INFO/WARN、stderr に DEBUG。最後に `msg=結果 pass=... スコア=... 種別エラー数=map[...]`。
→ `tools/bench/parse.py` で `score.json` に要約する。

### 00:45 リポジトリ化

サーバーの初期状態を取り込んで `initial` コミットにした。

```sh
git init -b main
rsync -az --exclude=isuride isucon14-1:/home/isucon/webapp/go/ webapp/go/
rsync -az isucon14-1:/home/isucon/webapp/{sql,public} webapp/   # 実際は個別に
rsync -az isucon14-1:/home/isucon/webapp/openapi.yaml webapp/
for n in 1 2 3; do
  rsync -az --rsync-path='sudo rsync' --exclude=tls/ isucon14-$n:/etc/nginx/ etc/isu$n/nginx/
  rsync -az --rsync-path='sudo rsync' --exclude=debian.cnf isucon14-$n:/etc/mysql/ etc/isu$n/mysql/
  rsync -az --rsync-path='sudo rsync' "isucon14-$n:/etc/systemd/system/isuride-*.service" etc/isu$n/systemd/
  rsync -az --rsync-path='sudo rsync' isucon14-$n:/home/isucon/env.sh etc/isu$n/home/
done
```

3台の設定は完全に同一だった。マニュアル（当日・アプリケーション）は gist から `docs/reference/` に保存。

### 00:47 計測基盤

`make deploy`（ローカルでクロスビルド→全台rsync→差分のあるサービスだけ再起動）と
`make bench`（ログ初期化→ベンチ→alp/pt-query-digest/pidstat/vmstat→`scores/log.md`）を用意。
ベンチ結果は `tools/bench/parse.py` で `score.json`（スコア・不満率・新規登録・売上・WARN内訳）にする。

ハマった点:

- `種別エラー数` はエラー種別が複数あると `"map[1:3 3:2]"` とクォートされ、正規表現が外れてスコア0と誤記録した → parse.py 修正・再集計。
- `pidstat` の平均で `Average:` 行を二重計上し、分母も誤っていて 200% を超える値が出た → 修正。マシン全体の飽和は vmstat の busy で見る。
- deploy.sh で `set -e` + `grep` の不一致(=1) により3号機だけ途中終了していた。

### 00:52〜01:12 DBまわり（1032 → 28987）

| 変更 | スコア | 根拠 |
| --- | ---: | --- |
| インデックス追加 | 3424 | ride_statuses / chair_locations / rides の全件走査 |
| owner/chairs の距離を差分集計（chair_distances） | 3691 | ウィンドウ関数が DB時間の59.6% |
| binlog off + flush_log_at_trx_commit=2 | 4362 | COMMIT が50.8% |
| マッチング: 待ち全件を最短時間の椅子へ | 11801 | ランダム1件/0.5秒だった |
| MySQL を isucon14-2 に分離 | 16743 | 1号機 180%/200% |
| コネクションプール64 | 18686 | Too many connections |
| interpolateParams | 26548 | PREPARE/CLOSE 90万回 |
| coupons(code) インデックス | 28987 | 招待コード登録のデッドロック（全件Xロック） |

### 01:17〜01:52 メモリ化（28987 → 167236）

| 変更 | スコア | 根拠 |
| --- | ---: | --- |
| rides.status 列（最新状態） | 27411 | 最新状態の取得が14万回・24%（ブレの範囲） |
| retry_after_ms 30→100 | 31337 | 通知ポーリング6万回/分 |
| 通知をメモリから返す（state.go） | 43608 | getChairStats の N+1 など |
| nearby-chairs をメモリから | 77891 | nearby 由来クエリが DB の33% |
| 認証キャッシュ | 103129 | トークン検索20万回/分 |
| マッチング判定をメモリで | 103439 | matching max 3.7s |
| マッチングの UPDATE を1文に | 97693 | 1回2.5秒（5秒に2回しか回っていなかった） |
| coordinate を upsert 1文に | 121498 | 椅子は座標更新の成功まで動かない |
| nginx keepalive 等 | 167236 | nginx 90%、タイムアウトの WARN 30件 |

ハマった点:

- `rides` に列を足したら初期データの `INSERT INTO rides VALUES (...)`（列名なし）が列数不一致で initialize 失敗 → 列はデータ投入後に `ALTER TABLE` で足す。
- 同じく `owner/sales` の JOIN で `status` が曖昧になって FAIL。
- nearby-chairs は「DBで COMPLETED」ではなく「椅子が COMPLETED の通知を受け取った」後でないと、「既にライド中」の WARN になる。
- slow log (long_query_time=0) 自体が約1割の重し（同一コードで 72399 → 82922）。比較は ON 同士で行う。
- マッチング統計（5秒ごとのログ）で「椅子は余っているのにマッチングが遅い」ことが分かった。計測を足すと判断が変わる。
