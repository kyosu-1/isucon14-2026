#!/usr/bin/env bash
# ベンチ結果を scores/log.md に1行追記し、前回比を出す。
#
# usage: ./tools/bench/record.sh <measurement_dir> [note]
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

DIR="${1:?usage: record.sh <measurement_dir> [note]}"
NOTE="${2:-}"
LOG=scores/log.md

if [ ! -f "$LOG" ]; then
  cat > "$LOG" <<'EOF'
# スコア推移

`make bench` が自動で1行ずつ追記する。判断（採用/revert）はメモ列に追記する。

- **不満率** = ベンチが出す「X%のライドは〜に不満がありました」の最終値（matching / pickup / ride）。
  ユーザー満足度が上がると新規登録が増え、負荷とスコアが伸びる。
- `pass=false` のスコアは無効。WARN が200件でFAIL打ち切り。

| 時刻 | スコア | 前回比 | pass | WARN | 不満率 m/p/r | 売上 | 変更 | commit | 計測 | メモ |
| --- | ---: | ---: | --- | ---: | --- | ---: | --- | --- | --- | --- |
EOF
fi

read -r SCORE PASS WARN DIS SALES < <(python3 - "$DIR/score.json" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
d = s.get("dissatisfied") or {}
dis = "/".join(f"{d.get(k, '-'):.0f}" if isinstance(d.get(k), float) else "-" for k in ("matching", "pickup", "ride"))
print(s["score"], s["pass"], s["warn_count"], dis, s["total_sales"])
PY
)

COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo '-')"
SUBJECT="$(git log -1 --format=%s 2>/dev/null || echo '-')"
NOW="$(date '+%m-%d %H:%M')"

# 直前の pass したスコア（表の最終行）
PREV="$(grep -E '^\| [0-9-]+ [0-9:]+ \|' "$LOG" | tail -1 | awk -F'|' '{gsub(/ /,"",$3); print $3}' || true)"
DELTA="-"
if [[ "$PREV" =~ ^[0-9]+$ ]] && [ "$PREV" -gt 0 ]; then
  DELTA="$(awk -v a="$SCORE" -v b="$PREV" 'BEGIN{printf "%+.1f%%", (a-b)*100.0/b}')"
fi

PASSMARK="$PASS"
[ "$PASS" = "True" ] && PASSMARK="ok"
[ "$PASS" = "False" ] && PASSMARK="**FAIL**"

printf '| %s | %s | %s | %s | %s | %s | %s | %s | `%s` | %s | %s |\n' \
  "$NOW" "$SCORE" "$DELTA" "$PASSMARK" "$WARN" "$DIS" "$SALES" "${SUBJECT//|/\\|}" "$COMMIT" "${DIR#measurements/}" "$NOTE" >> "$LOG"

echo "recorded: score=$SCORE (prev=${PREV:-none}, $DELTA) pass=$PASS -> $LOG"
