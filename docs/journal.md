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

### 01:55〜02:37 構成とイベント順序（167236 → 495562）

| 変更 | スコア | 根拠 |
| --- | ---: | --- |
| coordinate: 距離をメモリで積み上げ・200msごとに書き出し | 196333 | upsert が DB の 60% |
| アプリを isucon14-3 へ（1号機は nginx 専用） | 244124 | 1号機で nginx と app が CPU を取り合い |
| listen backlog 8192 + reuseport + sysctl | 251778 | ListenOverflows 85,551（GET /client のタイムアウト） |
| マッチングをメモリ先行 + retrieved_at をロック内 | 254211 | nearby「既にライド中」 |
| 送信済みフラグのDB書き込みを非同期に | 264471 | 同上（同期UPDATE中に「送信済み・未受信」） |
| 決済をトランザクションの外へ | 287776 | DB接続を握ったまま決済待ち → 他APIの p99 ~1s |
| マッチング間隔 0.1s | 274047 | **revert**（候補が減って pickup 悪化） |
| マッチング: 全組を迎車時間順に貪欲 + aging | 320624 | 椅子不足で古いライド順が非効率 |
| 通知を SSE に | 431087 | ポーリング 50万回/分、遷移ごとの反応待ち |
| COMPLETED 時の統計を先に更新 | 430410 | 「椅子の総乗車回数が一致しません」 |
| 招待コードをメモリで予約 | 同上 | FOR UPDATE のデッドロック |
| 割り当ての DB コミット後に通知 / pool 128 | 421622 | 遅れた UPDATE が完了日時を上書き |
| username の UNIQUE を外す | 418749 | ベンチ側の名前が偶然衝突 |
| COMPLETED → 決済の順 | 160395 FAIL | **revert**（評価応答前の完了は WARN 268） |
| 決済: Idempotency-Key で即再送 | 495562 | 成功率3割、100ms待ち+照合で 1件 0.7s |

### 02:50 ベンチ機を c5.2xlarge に（競技サーバーは変更なし）

measurements/20260922-024630 でベンチ機(c5.xlarge)の idle が負荷終盤に 5% 未満の秒が 7/64 あり、
アプリ側より先に頭打ちになりかけていた。ベンチ機は競技サーバーではないのでレギュレーション対象外。

```sh
export AWS_PROFILE=personal AWS_REGION=ap-northeast-1
I=i-08a58155409fcad18   # isucon14-4
aws ec2 stop-instances --instance-ids $I && aws ec2 wait instance-stopped --instance-ids $I
aws ec2 modify-instance-attribute --instance-id $I --instance-type '{"Value":"c5.2xlarge"}'
aws ec2 start-instances --instance-ids $I && aws ec2 wait instance-running --instance-ids $I
# パブリックIPが変わるので ssh 設定を作り直す（isuenv ssh が再生成する）
AWS_PROFILE=personal perl -e 'alarm 40; exec @ARGV' isuenv ssh isucon14-4 < /dev/null
```

API からの stop は OS の shutdown ではないので、isuenv の shutdown-behavior=terminate にはかからない。
TTL（/var/lib/isuenv-expires-at）はディスクにあるので維持される。private IP も変わらない。
次に作り直すときは `isuenv up isucon14 --nodes 3 --bench-instance-type c5.2xlarge` にする。

### 02:45 nearby の猶予 1s は -20%（revert）

nearby に空き椅子を出すのを COMPLETED 通知の1秒後にしたら WARN は 0 になったが、同一コード2回で
457804 / 466938（前は 595402）。招待経由の新規登録が 2900→2190。nearby の見え方が需要に効く。
WARN 3件/回は上限200件に対して十分小さいので、50ms に戻す。

### 02:37〜03:35 DB書き込みの非同期化・マッチング間隔・HTTP/2（495562 → 1117581）

| 変更 | スコア | 根拠 |
| --- | ---: | --- |
| POST /api/app/rides をメモリで | 509473 | 全ライドの状態を1件ずつ引く N+1 |
| 履歴(GET /api/app/rides)をメモリから | 595402 | ライドごとのクーポン・椅子・オーナー |
| nearby の猶予 1s | 457804 / 466938 | **revert**（同一コード2回で確認） |
| ベンチ機を c5.2xlarge に | 589374 | ベンチ機 idle<5% の秒あり（結果は変わらず） |
| 静的ファイルに expires 1d | 624935 | 総リクエストの8割が静的ファイルの再検証 |
| 送信済みを DB に書かない | 622088 | 追試は再起動→initialize。永続化しても使われない |
| 状態遷移を FIFO writer で | 750992 | 同期トランザクションで chair status avg 94ms → 終了間際の EOF 136件 |
| 乗車時間の重み | 708815 | **revert** |
| マッチング 0.5→0.2→0.1s | 854057 / 925871 | 1回の割り当てバースト（終盤 EOF）も小さくなる |
| マッチング 0.05s | 930004 | 差なし・WARN 増 → 0.1s に戻す |
| SSE の購読を接続ごとに | 924767 | 60秒で張り直す瞬間に古い接続が起こされていた |
| 長いライド優先 | 864656 | **revert**（離脱増） |
| owner/sales をメモリ・COMPLETED を待たない | 987383 | 評価 avg 0.5s のうち DB 書き込み待ち |
| 決済トークン/URL をメモリ | 978690 | |
| nearby の猶予 300ms | 326635 | **revert**（需要が消えた、下記） |
| HTTP/2 | 985413 | 全リクエストが HTTP/2 に。nginx busy 80→71% |
| マッチング候補を椅子ごと上位に絞る | 1101372 | pprof で matching が CPU 34%（全組ソート） |
| 空き椅子の集合 | 1117581 | pprof で nearby 15%（全椅子走査） |

分かったこと:

- **nearby に空き椅子が見えることが需要の入口。** マッチング 0.1s の今、猶予 300ms にすると解放された椅子が
  nearby に出る前に割り当てられ、配車依頼そのものが減って -67%（序盤の matching 不満 0%、椅子数も半分）。
  nearby の「既にライド中」WARN（1回 0〜48件）は需要を保つための許容コストとして残す。
- ライドのフェーズ別平均（ride_statuses の時刻から）: 依頼→受理 5.2s（椅子待ち）、迎車 0.26s、乗車待ち 0.11s、
  運搬 1.07s、到着→完了 0.32s。椅子が足りないのでライドの回転を速くするほど伸びる。
- 負荷終了の瞬間に処理中のリクエストはベンチに切断され、EOF の WARN として数えられる。
  一斉に投げられるリクエスト（マッチング直後の ENROUTE）を小さく・速くする。
- ベンチのクライアントは HTTP/2 に対応している（nginx で http2 を有効にしたら全リクエストが HTTP/2.0）。

### 03:37 最終構成と再起動試験

```sh
# nginx の access_log を off にしてコミット → make deploy
make measure-off          # slow log OFF（SET PERSIST なので再起動後も OFF）
make bench                # 1137655
make restart-test         # 3台を同時に reboot → 全サービス自動起動 → ベンチ 1159771 (pass, WARN 15)
```

再起動後の役割: isu1 nginx / isu2 mysql / isu3 isuride-go + isuride-matcher。
アプリは起動時に DB から状態を読み込み（DB がまだなら panic → systemd が 5 秒後に再起動）、ベンチの initialize で作り直す。

### 06:30 椅子と依頼の分布の分析（最後のベンチ = 再起動試験3回目 1,134,748 のデータ）

読み取りだけ。`ssh isucon14-2 'sudo mysql isuride'` で集計した（created_at > '2026-09-01' でベンチ中のデータに絞る）。

- 依頼は2つの町に分かれている: (0,0) 中心と (300,300) 中心のそれぞれ 100×100。町の間は距離 ~600。
- 町ごとの需給はほぼ同じ比率: 椅子 842 / 657（1.28）、依頼 12,718 / 9,872（1.29）。平均待ちも 4.72s / 4.75s。
  → 町の間で椅子を寄せる余地はない。
- 町をまたいだ割り当ては 0 / 18,079 件。迎車距離は平均 6.3（運ぶ距離は平均 54）。
- 速度は 2/3/5/7 がほぼ1/4ずつ。1脚あたりの完了ライドは 6.1 / 7.2 / 9.8 / 11.3、運んだ距離は 326 / 385 / 525 / 614。
  速度7は速度2の約1.9倍（速度比 3.5倍には届かない = ライドごとの固定の手間が効いている）。
- **負荷終了までに一度も割り当てられなかった依頼が 2,621 件（12%）**。常に需要が供給を上回っている。
- ベンチ中に登録された椅子 2,155 のうち 656 は位置が無いが、すべて負荷終了直後（18:52:25.49〜）に登録された
  もの（最後の売上で増えた椅子）。負荷中の 1,499 脚はすべて稼働していた。
- 待ち時間補正（2.0/秒）は迎車コスト（平均 6.3 / 速度 ≈ 1.5）に比べて大きく、割り当ては実質「古い依頼から、
  その近くの椅子」に近い。

### 06:30 椅子の1サイクルの内訳

同じデータで、前のライドの COMPLETED → 同じ椅子の次の ENROUTE を測ると平均 388ms（17,103件）。
1サイクル ≈ 空き 388 + 迎車 264 + 乗車待ち 108 + 運搬 1,072 + 評価・決済 315 ≈ 2.15s（運搬は50%）。

### 06:30〜06:45 分布の分析を踏まえたマッチング（最高 1,196,520）

比較の基準は、分析前の最終構成（計測 OFF）8回の平均 1,133,000。ブレが ±2〜4% あるので各設定を2回ずつ回した。

| 設定 | スコア（2回） | 平均 | 判断 |
| --- | --- | ---: | --- |
| 速度で椅子を選ぶ項 重み 1.0（補正 2.0） | 1,177,878 / 1,153,498 | 1.166M | 採用 |
| 同 重み 2.0 | 1,111,020 / 1,119,596 | 1.115M | 戻した（pickup 悪化・離脱増） |
| 待ち時間補正 4.0 | 1,192,594 / 1,158,577 | 1.176M | 採用 |
| 待ち時間補正 8.0 | 1,196,520 / 1,190,386 | 1.193M | 採用 |
| 待ち時間補正 16.0 | 1,182,649 / 1,166,979 | 1.175M | 戻した（pickup 5%・離脱 241〜262） |
| 再起動試験（補正 8.0 + 重み 1.0） | 1,164,393 | — | 合格（4回目） |

- 速度で椅子を選ぶ項: `乗車距離 × (1/v − 1/vRef)`（vRef はその回の空き椅子の速さの調和平均）。
  長いライドに速い椅子を当てて運搬時間の合計を減らす。椅子全体で平均 0 なので「どの依頼から配るか」は変えない。
  以前の「乗車時間の重み」（乗車距離 / v をそのまま足す: -6%）は長いライドを後回しにしていた。
- 待ち時間補正は 2.0 → 8.0 で +2.4%。山は 8.0 付近（16.0 で迎車の不満と離脱が増えて下がる）。
- 最新構成の3回（1,196,520 / 1,190,386 / 1,164,393）の平均は 1.184M で、分析前から +4.5%。

### 06:55〜07:15 ハンガリアン法（最高 1,218,271）

貪欲法（コストの小さい組から確定）をやめ、その回の割り当て全体でコストの合計を最小にする（Kuhn-Munkres, O(n²m)）。
候補ライドは各椅子の上位「空き椅子の数」件の和集合（最適解の割り当ては必ずこの中に入る: 上位の外を割り当てられた
椅子があれば、上位の中に他の椅子が使っていないライドがあり、そちらに替えるとコストが下がるため）。
行² × 列 が上限を超える回は貪欲法。

| 設定 | スコア | 判断 |
| --- | --- | --- |
| ハンガリアン法（初版） | 0 / 0 / 0 **FAIL** | 依頼 < 空き椅子の回で添え字を取り違えて panic、マッチングされずクリティカルエラー |
| 修正後（上限 2,000万） | 1,218,271 / 1,192,302 / 1,213,310 | 採用（平均 1.208M、貪欲法 1.184M から +2.0%） |
| 上限 500万 | 1,191,611 / 1,206,308 / 1,158,392 | 戻した（平均 1.185M） |
| 所要時間の内訳を計測 | 1,208,796 | ピーク時 1回: 計算 最大85〜175ms、DB の UPDATE 最大158〜380ms |
| 割り当ての DB 書き込みも FIFO へ（すぐ通知） | 1,185,235 / 1,172,962 / 1,185,463 | 戻した（1回 最大 47〜175ms に縮んだが平均 1.181M） |
| 再起動試験（5回目） | 1,209,389 | 合格 |

反省:

- 初版のテストは行列の計算（hungarian）だけで、向きを入れ替える経路（依頼 < 空き椅子）を通っていなかった。
  → minCostAssign に切り出し、両方の向きを総当たりと比べるテストを追加（hungarian_test.go）。
- 1回目の FAIL を確認せずに3回続けて回した。結果を見てから次を回す。
- マッチングを速くしても（DB 待ちをなくしても）スコアは上がらなかった。速さより割り当ての質が効いている。

### 07:33 決済の並列送信（FAIL → revert）

同じ Idempotency-Key で3本同時に再試行し、最初の 204 で返す案。決済（平均 3.3回 × 35〜70ms）を ~1.5回ぶんに縮める狙い。

- 結果: 548 **FAIL**。「決済サーバーに誤った支払いがリクエストされました (CODE=35): 既に支払い済みです」。
  決済の計測は 204 が2回 + 422 が2回。
- **Idempotency-Key は「前の要求が終わった後の再送」にしか効かない。送信中の要求が重なると両方処理される。**
  決済は1本ずつ順に再試行する（今の実装）しかない。
- revert 後の確認: 1,187,909（pass）。
