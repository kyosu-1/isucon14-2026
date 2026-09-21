#!/usr/bin/env bash
# 任意のSQLの実行計画を見る（EXPLAIN と EXPLAIN ANALYZE）。
# ssh越しに文字列で渡すとバッククォートが食われるので、ファイルにして転送する。
#
# usage: ./tools/analyze/explain.sh "SELECT ..."  [db_host]
#        echo "SELECT ..." | ./tools/analyze/explain.sh - [db_host]
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

SQL="${1:?usage: explain.sh <SQL|-> [db_host]}"
HOST="${2:-isucon14-1}"
[ "$SQL" = "-" ] && SQL="$(cat)"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
{
  printf 'EXPLAIN %s;\n' "${SQL%;}"
  printf 'EXPLAIN ANALYZE %s;\n' "${SQL%;}"
} > "$tmp"

scp -q "$tmp" "$HOST:/tmp/explain.sql"
ssh "$HOST" "sudo mysql isuride -t < /tmp/explain.sql"
