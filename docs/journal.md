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
