#!/usr/bin/env bash
# 計測ループの本体。1回叩けば「ログ初期化 → ベンチ → 解析 → 記録」まで終わる。
#
# 出力は measurements/<timestamp>/ に全部入る（gitにコミットする）:
#   score.json               ベンチ結果の要約（score / pass / 不満率 / 売上 / WARN内訳）
#   bench.log                ベンチの INFO/WARN/ERROR 出力（生）
#   bench-debug.log          ベンチの DEBUG 出力（時間経過ごとの内部評価）
#   alp.txt                  エンドポイント別集計（SUM降順）
#   slow.txt                 クエリ別集計（pt-query-digest）
#   cpu-<host>.txt           プロセス別CPU（pidstat の平均）
#   vmstat-<host>.txt        マシン全体のCPU/IO（ベンチ機含む）
#   app-errors-<host>.txt    アプリのエラーログ（種類別件数）
#   meta.txt                 コミット・ホスト・条件
#
# usage: ./tools/bench/run.sh ["メモ"]
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

NOTE="${1:-}"
ENTRY="${ENTRY:-isucon14-1}"
DB_HOST="${DB_HOST:-isucon14-1}"
read -r -a APP_HOSTS_ARR <<< "${APP_HOSTS:-isucon14-1 isucon14-2 isucon14-3}"
BENCH="${BENCH:?BENCH が未設定です。make 経由で実行してください}"

ip_of() { ssh -o ConnectTimeout=5 "$1" 'hostname -I | awk "{print \$1}"'; }
n="${ENTRY##*-}"; v="ISU${n}_IP"
TARGET_IP="${!v:-$(ip_of "$ENTRY")}"
BENCH_IP="${BENCH_IP:-$(ip_of "$BENCH")}"

TS="$(date +%Y%m%d-%H%M%S)"
OUT="measurements/$TS"
mkdir -p "$OUT"

COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo '-')"
SUBJECT="$(git log -1 --format=%s 2>/dev/null || echo '-')"
{
  echo "timestamp : $TS"
  echo "commit    : $COMMIT  $SUBJECT"
  echo "entry     : $ENTRY ($TARGET_IP)"
  echo "app_hosts : ${APP_HOSTS_ARR[*]}"
  echo "db_host   : $DB_HOST"
  echo "bench     : $BENCH ($BENCH_IP)"
  echo "note      : $NOTE"
  echo "dirty     : $(git status --porcelain -- webapp etc | wc -l | tr -d ' ') files uncommitted (webapp/ etc/)"
} > "$OUT/meta.txt"

echo "==> [1/5] ログ初期化・リソース記録開始"
START_EPOCH="$(date +%s)"
ssh "$ENTRY" "sudo truncate -s 0 /var/log/nginx/access.log /var/log/nginx/error.log"
ssh "$DB_HOST" "sudo truncate -s 0 /var/log/mysql/slow.log 2>/dev/null || true"
for h in "${APP_HOSTS_ARR[@]}"; do
  ssh "$h" "nohup sudo sh -c 'vmstat -t 1 110 > /tmp/vmstat.txt 2>&1' >/dev/null 2>&1 &
            nohup sudo sh -c 'LC_ALL=C pidstat -u 1 100 > /tmp/pidstat.txt 2>&1' >/dev/null 2>&1 &
            sleep 0.1" &
done
# 負荷をかける側も測る。アプリ側に余裕があるのにスコアが伸びないときは、ここを先に疑う。
ssh "$BENCH" "nohup sudo sh -c 'vmstat -t 1 110 > /tmp/vmstat.txt 2>&1' >/dev/null 2>&1 & sleep 0.1" &
wait

echo "==> [2/5] 疎通確認"
code="$(ssh "$ENTRY" "curl -sk -o /dev/null -w '%{http_code}' --resolve isuride.xiv.isucon.net:443:127.0.0.1 'https://isuride.xiv.isucon.net/api/app/nearby-chairs?latitude=0&longitude=0'")"
echo "    GET /api/app/nearby-chairs (未認証) -> $code (401 なら正常)"
if [ "$code" != "401" ]; then
  echo "!! アプリに届いていません。ベンチを中止します。" >&2
  exit 1
fi

echo "==> [3/5] ベンチ実行 -> $TARGET_IP:443 (payment: $BENCH_IP:12346)"
set +e
ssh "$BENCH" "cd /home/isucon && sudo -u isucon ./bench run \
  --addr $TARGET_IP:443 --target https://isuride.xiv.isucon.net \
  --payment-url http://$BENCH_IP:12346 --payment-bind-port 12346" \
  > "$OUT/bench.log" 2> "$OUT/bench-debug.log"
BENCH_RC=$?
set -e
python3 tools/bench/parse.py "$OUT/bench.log" > "$OUT/score.json"
grep -E 'level=(WARN|ERROR)|結果|不満' "$OUT/bench.log" | tail -8 | cut -c1-250

echo "==> [4/5] 解析"
for h in "${APP_HOSTS_ARR[@]}"; do
  (
    ssh "$h" "cat /tmp/pidstat.txt" > "$OUT/raw-pidstat-$h.txt" 2>/dev/null || true
    ./tools/analyze/cpu-by-process.sh "$OUT/raw-pidstat-$h.txt" > "$OUT/cpu-$h.txt" 2>/dev/null || true
    rm -f "$OUT/raw-pidstat-$h.txt"
    ssh "$h" "cat /tmp/vmstat.txt" > "$OUT/vmstat-$h.txt" 2>/dev/null || true
    # アプリのエラー（writeError が slog.Error で出す）を種類別に数える
    ssh "$h" "sudo journalctl -u isuride-go --since @$START_EPOCH --no-pager -o cat 2>/dev/null \
      | grep -E 'level=ERROR|panic|error' | sed -E 's/[0-9A-Z]{26}/<ID>/g; s/time=[^ ]+ //' \
      | cut -c1-200 | sort | uniq -c | sort -rn | head -30" > "$OUT/app-errors-$h.txt" 2>/dev/null || true
    [ -s "$OUT/app-errors-$h.txt" ] || rm -f "$OUT/app-errors-$h.txt"
  ) &
done
ssh "$BENCH" "cat /tmp/vmstat.txt" > "$OUT/vmstat-$BENCH.txt" 2>/dev/null &
./tools/analyze/alp.sh "$ENTRY" > "$OUT/alp.txt" 2>&1 &
./tools/analyze/slow.sh "$DB_HOST" > "$OUT/slow.txt" 2>&1 &
wait

echo "==> [5/5] 記録"
./tools/bench/record.sh "$OUT" "$NOTE"

echo
echo "==> measurements: $OUT"
python3 - "$OUT/score.json" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
d = s.get("dissatisfied") or {}
r = s.get("registered") or {}
print(f"  score={s['score']} pass={s['pass']} warn={s['warn_count']} error={s['error_count']} codes={s['error_codes']}")
print(f"  不満率 matching={d.get('matching')}% pickup={d.get('pickup')}% ride={d.get('ride')}%")
print(f"  新規登録 評判={r.get('reputation')} 招待={r.get('invitation')} 離脱={s['left']}  売上合計={s['total_sales']} 椅子={s['total_chairs']}")
for w in s["warnings"][:5]:
    print(f"  WARN x{w['count']}: {w['msg'][:160]}")
for e in s["errors"][:5]:
    print(f"  ERROR x{e['count']}: {e['msg'][:160]}")
PY
for h in "${APP_HOSTS_ARR[@]}"; do
  printf '  %-11s ' "$h"; sed -n '2,5p' "$OUT/cpu-$h.txt" 2>/dev/null | awk '{printf "%s=%s%% ", $1, $2}'; echo
done
echo "--- alp (上位) ---"; head -14 "$OUT/alp.txt" || true
[ "$BENCH_RC" = 0 ] || echo "!! bench exit code = $BENCH_RC" >&2
