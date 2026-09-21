#!/usr/bin/env bash
# 任意のSQLの実行計画を見る（EXPLAIN と EXPLAIN ANALYZE）。
# ssh越しに文字列で渡すとバッククォートが食われるので、ファイルにして転送する。
#
# usage: ./tools/analyze/explain.sh "SELECT ..."  [db_host]
#        echo "SELECT ..." | ./tools/analyze/explain.sh - [db_host]
set -euo pipefail

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
