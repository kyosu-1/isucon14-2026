#!/usr/bin/env bash
# 稼働中の Go アプリから CPU プロファイルを取る（アプリは 127.0.0.1:6060 で pprof を待ち受けている）。
# ベンチと同時に走らせないと意味がないので、make bench の最中に別で実行する。
#
# usage: ./tools/analyze/pprof.sh [app_host] [seconds] [out.pprof]
set -euo pipefail
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }

HOST="${1:-isucon14-3}"
SEC="${2:-30}"
OUT="${3:-/tmp/cpu.pprof}"
ssh "$HOST" "curl -fsS -o /tmp/cpu.pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=$SEC'"
scp -q "$HOST:/tmp/cpu.pprof" "$OUT"
go tool pprof -top -nodecount=35 "$OUT" 2>/dev/null
