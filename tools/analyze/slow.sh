#!/usr/bin/env bash
# MySQLスロークエリを pt-query-digest で集計する。
# 上位が「合計実行時間の重いクエリ」= まず潰すべきクエリ。
#
# 見るポイント:
#   - Rows examine が Rows sent よりずっと大きい -> インデックスが効いていない
#   - Calls が異様に多い                          -> N+1 / ポーリング
#
# usage: ./tools/analyze/slow.sh [db_host]
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

HOST="${1:-isucon14-1}"
SLOW="${SLOW_LOG:-/var/log/mysql/slow.log}"
LIMIT="${LIMIT:-15}"

ssh "$HOST" "sudo test -s '$SLOW' || { echo 'slow log is empty — make measure-on を実行してからベンチしてください'; exit 0; }
  sudo pt-query-digest --limit '$LIMIT' '$SLOW' 2>/dev/null"
