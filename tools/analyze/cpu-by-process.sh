#!/usr/bin/env bash
# pidstat の出力をプロセス別に平均して降順に並べる。
#
# 「アプリを速くしたのにスコアが動かない」ときは、
# そもそもアプリがマシン全体のCPUの何割を使っているかを見ないと判断できない。
# c5.large は 2 vCPU なので上限は 200%。
#
# 平均は「サンプル総数」ではなく「計測秒数」で割る（プロセスが寝ていた秒は0%として扱う）。
# top -bn1 は %CPU の差分が取れず全プロセス0.0%になるので使わない。
#
# usage: ./tools/analyze/cpu-by-process.sh <pidstat-raw.txt>
set -euo pipefail

awk '
  # LC_ALL=C の pidstat -u: Time UID PID %usr %system %guest %wait %CPU CPU Command
  NF >= 10 && $NF != "Command" && $(NF-2) ~ /^[0-9.]+$/ {
    cmd = $NF
    cpu[cmd] += $(NF-2)
    if (!($1 in seen)) { seen[$1] = 1; secs++ }
  }
  END {
    for (c in cpu) printf "%-24s %9.1f\n", c, cpu[c]/(secs ? secs : 1)
  }
' "${1:?usage: cpu-by-process.sh <pidstat-raw.txt>}" | sort -k2 -rn | head -15 | \
  { printf "%-24s %9s\n" "COMMAND" "AVG_CPU%"; cat; }
