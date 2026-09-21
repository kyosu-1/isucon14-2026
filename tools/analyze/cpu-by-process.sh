#!/usr/bin/env bash
# pidstat の出力をプロセス別に平均して降順に並べる。
#
# 「アプリを速くしたのにスコアが動かない」ときは、
# そもそもアプリがマシン全体のCPUの何割を使っているかを見ないと判断できない。
# c5.large は 2 vCPU なので上限は 200%。
#
# 平均は計測秒数（ベンチ前後のアイドル時間を含む約100秒）で割る。負荷中のピークはこれより高い。
# マシン全体の飽和は vmstat-<host>.txt の id 列で見ること。
# top -bn1 は %CPU の差分が取れず全プロセス0.0%になるので使わない。
#
# usage: ./tools/analyze/cpu-by-process.sh <pidstat-raw.txt>
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

awk '
  # LC_ALL=C の pidstat -u: Time UID PID %usr %system %guest %wait %CPU CPU Command
  # 秒ごとにヘッダ行が出る。平均はヘッダ数（=計測秒数）で割る（その秒に出てこないプロセスは0%扱い）。
  # 末尾の Average: 行は二重計上になるので捨てる。
  $1 == "Average:" { next }
  $NF == "Command" { secs++; next }
  NF >= 10 && $(NF-2) ~ /^[0-9.]+$/ { cpu[$NF] += $(NF-2) }
  END {
    for (c in cpu) printf "%-24s %9.1f\n", c, cpu[c]/(secs ? secs : 1)
  }
' "${1:?usage: cpu-by-process.sh <pidstat-raw.txt>}" | sort -k2 -rn | head -15 | \
  { printf "%-24s %9s\n" "COMMAND" "AVG_CPU%"; cat; }
