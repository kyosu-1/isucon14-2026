#!/usr/bin/env bash
# nginxアクセスログ(LTSV)をalpで集計する。SUM（合計時間）降順 = 潰す価値が高い順。
#
# ISURIDE のパスパラメータ（ULID）を持つエンドポイントは -m でまとめないとばらけて読めない。
# クエリ文字列（nearby-chairs の座標など）は alp が既定で落とすのでそのままでよい。
#
# usage: ./tools/analyze/alp.sh [host]
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

HOST="${1:-isucon14-1}"
LOG="${ACCESS_LOG:-/var/log/nginx/access.log}"

MATCH='^/api/app/rides/[0-9A-Z]+/evaluation,^/api/chair/rides/[0-9A-Z]+/status,^/assets/.+,^/images/.+,^/client.*,^/owner.*,^/simulator.*'

ssh "$HOST" "sudo alp ltsv --file '$LOG' \
  --sort sum -r \
  -m '$MATCH' \
  -o count,method,uri,min,avg,max,sum,p99,2xx,3xx,4xx,5xx"
