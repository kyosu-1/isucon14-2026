# CLAUDE.md — ISUCON14 (ISURIDE) 攻略の運用ルール

このリポジトリは **ISUCON14 の問題（ISURIDE）で、レギュレーションを守った上で最高スコアを取る**ためのもの。
AIエージェント（Claude Code）が主体で「計測 → 改善」のループを回す前提で書いてある。
実装言語は **Go**（`webapp/go/`）。

---

## 0. まずこれを読む

| 知りたいこと | 見る場所 |
| --- | --- |
| 何をやったか（再現手順） | `docs/journal.md` |
| スコアの推移 | `scores/log.md` |
| 計測の生データ | `measurements/<timestamp>/` |
| 当日マニュアル（レギュレーション・スコア計算） | `docs/reference/manual.md` |
| アプリケーション仕様 | `docs/reference/app_manual.md` / `webapp/openapi.yaml` |
| 計測改善ループの判断基準 | `.claude/skills/tuning-isucon14/SKILL.md` |

---

## 1. レギュレーション（違反したら全部無意味）

`docs/reference/manual.md` が原典。要点:

### 構成

- 競技サーバーは **c5.large（2 vCPU / 4GB）× 3台**。**この3台だけ**で処理する。
  - isuenv では `isucon14-1..3` が競技サーバー、`isucon14-4`（c5.2xlarge）は**ベンチマーカー専用**。
    ベンチ機にアプリの処理を載せるのは禁止（外部リソースの利用にあたる）。
  - 現在の役割: **isu1 = nginx のみ / isu2 = MySQL / isu3 = Go アプリ + マッチャー**
    （`etc/isuN/services` が正。アプリは状態をメモリに持つので1プロセスだけ）
- 3台の役割分担（DBを別ノードに出す、アプリを複数台に置く等）は自由。
- ベンチマーカーは `isucon14-1:443` にアクセスする（`make bench` の `ENTRY`）。

### スコア

```
椅子がマッチした位置から乗車位置までの移動距離の合計 × 0.1
+ 椅子の乗車位置から目的地までの移動距離の合計
+ ライド完了数 × 5
```

**ライドが多く・長く完了するほど点が入る**。ユーザーの満足度（マッチまでの時間・椅子の到着の速さ・
乗車後の移動時間）が高いと新規登録が増え、負荷（=稼げるライド）が増える。
つまり「速く返す」だけでなく「**良いマッチング**」がスコアを決める。

### FAIL（スコア無効）になる

- 初期化処理（`POST /api/initialize` 30秒、`POST /api/owner/owners` 等）や整合性チェックの失敗
- 負荷走行中のクリティカルエラー（想定外の通知、状態遷移の順序違反、長時間マッチされないライド、
  評価のタイムアウト、支払い漏れ、決済サーバーへの誤った支払い）
- **WARN が200件**に達する
- 負荷終了時点で走っていた決済が5秒以内に終わらない

### 猶予時間（ここを超える遅延は許されない）

- `POST /api/chair/coordinate` → `GET /api/app/nearby-chairs` への反映: **3秒以内**
- `POST /api/chair/coordinate` → `GET /api/owner/chairs` の `total_distance`: **3秒の猶予**
- ライド要求 → マッチ後にユーザー・椅子へ通知されるまで: **30秒**
- 状態変化 → 通知（JSON/SSE どちらでも）: **3秒以内**が期待値。
  通知は**すべての状態遷移を順序通り、少なくとも1回**返すこと。

### 変更してはいけない

- ポート・URIなどベンチマーカーから見えるインターフェース（`webapp/openapi.yaml` が仕様）
- `webapp/public/` の静的ファイルの内容
- `POST /api/initialize` のレスポンスの `language`（`go` のまま）
- SSH(22) / HTTPS(443) のセキュリティグループ

### やっていい

- DBスキーマの変更、インデックス、キャッシュ、ミドルウェアの入れ替え・設定変更
- `GET /api/internal/matching` と `isuride-matcher.service` は**自由に仕様変更してよい**（マニュアルに明記）
- 通知エンドポイントを SSE にする（マニュアルに明記）
- 決済マイクロサービスへの `Idempotency-Key` ヘッダの付与

### 守らないといけない性質

- **追試**: 3台を再起動 → 負荷走行。**任意のタイミングの再起動に耐えること**。
  → 手で起動したプロセス、`enable` していないサービス、起動順序に依存した構成は全部アウト。
  → 終盤に `make restart-test` を必ず通す。
- 初期化処理はベンチマーカーが期待する状態を**漏れなく**作ること。
  キャッシュをアプリ内に持つなら `POST /api/initialize` で作り直す。

---

## 2. 大原則

1. **計測なき改善は禁止。** 変更前に必ず根拠（alp / スロークエリ / pidstat / ベンチの不満率）を
   `measurements/` から示す。
2. **1改善 = 1コミット = 1ベンチ。** 複数の変更を混ぜない。
3. **スコアが下がったら revert。** ただしスコアのブレ（同一コードでの差）を先に把握しておくこと。
   ISUCON14のベンチは負荷がユーザー満足度で増減するので、private-isu よりブレが大きい。
4. **サーバー上で直接編集しない。** コードも `/etc` もローカルの git が正。反映は `make deploy` だけ。
   ssh越しの**読み取り**（ログ、EXPLAIN、top）は自由。
5. **やったことは全部 `docs/journal.md` に残す。** 失敗した試行も消さない。
6. **計測の記録はすべてコミットする。** `make bench` の出力 `measurements/<ts>/` と
   `scores/log.md` は、ベンチのたびに `git commit` する（コードの変更とは別コミットでよい）。
7. **終盤は守りに入る。** 再起動試験・計測OFF・最終確認を優先する。

---

## 3. 改善ループ

```
make bench          # ログ初期化 → ベンチ → alp/slow/pidstat/vmstat収集 → scores/log.md に記録
  ↓
measurements/<ts>/ を読む（score.json の不満率・WARN、alp.txt、slow.txt、cpu-*.txt）
  ↓
支配的なボトルネックを1つだけ特定（根拠を添えて）
  ↓
最小の手を選んでローカルで実装 → git commit（根拠を本文に書く）
  ↓
make deploy && make bench     # 上がれば継続、下がれば git revert
  ↓
git add measurements scores && git commit -m "bench: ..."
```

### ボトルネックの読み方

| 症状 | 見るもの | 疑うこと |
| --- | --- | --- |
| 不満率 matching が高い | `score.json` | マッチングの間隔・アルゴリズム・椅子の空き判定 |
| 不満率 pickup が高い | `score.json` | 遠い椅子・遅い椅子を割り当てている |
| 不満率 ride が高い | `score.json` | 遅い椅子（モデルの speed）を割り当てている／座標更新の遅延 |
| 特定URIの `SUM` が突出 | `alp.txt` | そのエンドポイントの実装 |
| 同じクエリの Calls が異常 | `slow.txt` | N+1・ポーリング（通知の `retry_after_ms`） |
| `Rows examine` >> `Rows sent` | `slow.txt` | インデックス欠如 |
| mysqld が CPU を支配 | `cpu-*.txt` | DB。インデックス／クエリ／DB分離 |
| isuride が CPU を支配 | `cpu-*.txt` | アプリ。ログ出力・JSON・アルゴリズム |
| WARN が出る | `score.json` の warnings | 反映遅延（3秒猶予）・状態の不整合 |

---

## 4. 自律で進めてよい範囲

**承認なしで進めてよい:**

- 計測、解析、ログの読み取り
- ローカルでのコード・設定の変更、コミット
- `make deploy` / `make bench` / `make restart-test`（＝改善ループを回し続けること）
- スコアが下がった変更の revert
- 3台の役割分担の変更（DB分離など。レギュレーション内）

**必ず先に確認する:**

- `make down`（環境の破棄）、`isuenv nuke`、環境の作り直し
- **ベンチ機（isucon14-4）のインスタンスタイプの変更**。スコアがベンチ機の性能に左右されるため、
  途中で変えると前後の点数が比べられなくなる（02:50 に c5.xlarge → c5.2xlarge に変えた。以後は変えない）
- GitHub など外部への push・公開
- レギュレーションの解釈が割れる変更

---

## 5. やってはいけないこと

- **ベンチマーカーのソースを読んで挙動を先回りする。** ベンチマーカーはブラックボックスとして扱う。
  判断材料は外から観測できるもの（スコア、不満率、WARN、alp、自分のサーバーのログ）だけ。
  仮説があるなら、読むのではなく**入れて測る**。
- ベンチマーカー自体に手を入れる／ベンチ機に処理を載せる
- 静的ファイルの内容を書き換える
- 計測ログを取らないままの「ついで修正」
- 再起動で消える状態に依存する（アプリ内キャッシュは initialize と起動時に DB から復元できること）
- `long_query_time=0` のまま最終ベンチを回す（`make measure-off` を忘れない）

---

## 6. 環境

```sh
# AWSセッション（切れていたら人間がブラウザでサインイン）
aws login --profile personal

# 作成（競技3台 c5.large + ベンチ1台 c5.2xlarge）
AWS_PROFILE=personal isuenv up isucon14 --nodes 3 --bench-instance-type c5.2xlarge --ttl 8h
make setup      # hosts生成 → ベンチ機のサービス停止 → 計測ツール導入 → deploy → 計測ON
make bench

# 終わったら
make down
```

グローバルIPが変わってSSHが通らなくなったら `AWS_PROFILE=personal isuenv ssh isucon14` で貼り直す。
