#!/usr/bin/env bash
# ベンチマーカー専用ノードから競技用サービスを止める。
#
# AMIは競技サーバーと同じものなので、何もしないとベンチ機でも
# nginx / mysql / isuride-go / matcher（0.5秒ごとにcurl）が動いていてCPUを取られる。
# 負荷をかける側が詰まるとスコアがアプリではなくベンチ機の性能で決まってしまう。
#
# usage: ./tools/setup/bench-node.sh [bench_host]
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

HOST="${1:-isucon14-4}"
echo "==> $HOST : 競技用サービスを停止"
ssh "$HOST" 'sudo systemctl disable --now isuride-go isuride-matcher isuride-payment_mock nginx mysql >/dev/null 2>&1 || true
  command -v sar >/dev/null 2>&1 || sudo apt-get install -y -qq sysstat >/dev/null
  for s in isuride-go isuride-matcher nginx mysql; do printf "  %-18s %s\n" $s "$(systemctl is-active $s)"; done'
