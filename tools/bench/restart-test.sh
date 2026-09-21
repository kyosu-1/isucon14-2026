#!/usr/bin/env bash
# 再起動試験。
#
# ISUCON14の追試は「ポータルに表示されている3台のサーバーを再起動」→「負荷走行」。
# メモリ上だけの状態・手で起動したプロセス・systemctl enable し忘れたサービス・
# 起動順序に依存した構成（アプリがDBより先に立ち上がって落ちる等）はここで落ちる。
#
# 競技終了の1時間前には必ず一度通しておくこと。
#
# usage: ./tools/bench/restart-test.sh [host...]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

[ $# -eq 0 ] && set -- isucon14-1 isucon14-2 isucon14-3
HOSTS=("$@")
SSH_OPTS=(-o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR)

# 再起動前の boot id を控える。「sshが繋がったこと」だけで判定すると、
# まだ落ちていない再起動前のsshdに繋がって誤判定する。
BOOT_IDS=()
for i in "${!HOSTS[@]}"; do
  BOOT_IDS[$i]="$(ssh "${SSH_OPTS[@]}" "${HOSTS[$i]}" 'cat /proc/sys/kernel/random/boot_id')"
done

for HOST in "${HOSTS[@]}"; do
  echo "==> $HOST : reboot"
  # reboot は halt/poweroff ではないので instance-initiated-shutdown-behavior=terminate には引っかからない
  ssh "${SSH_OPTS[@]}" "$HOST" 'sudo systemctl reboot' || true
done

echo "==> 起動待ち（boot_id が変わるまで）"
for i in "${!HOSTS[@]}"; do
  HOST="${HOSTS[$i]}"
  for attempt in $(seq 1 90); do
    now="$(ssh "${SSH_OPTS[@]}" "$HOST" 'cat /proc/sys/kernel/random/boot_id' 2>/dev/null || true)"
    if [ -n "$now" ] && [ "$now" != "${BOOT_IDS[$i]}" ]; then
      echo "    $HOST rebooted"
      break
    fi
    sleep 5
    [ "$attempt" = 90 ] && { echo "!! $HOST が再起動しませんでした" >&2; exit 1; }
  done
  ssh "${SSH_OPTS[@]}" "$HOST" 'systemctl is-system-running --wait >/dev/null 2>&1 || true'
done

echo "==> サービス確認"
for HOST in "${HOSTS[@]}"; do
  ssh "${SSH_OPTS[@]}" "$HOST" "printf '  %-11s ' \$(hostname); for s in nginx mysql isuride-go isuride-matcher; do
      systemctl is-enabled --quiet \$s 2>/dev/null && printf '%s=%s ' \$s \$(systemctl is-active \$s); done; echo"
done

echo "==> 再起動後のベンチで最終確認"
if [ -z "${BENCH:-}" ] && [ -f hosts.generated.mk ]; then
  BENCH="$(awk -F':= *' '/^BENCH :=/ {print $2}' hosts.generated.mk)"
  export BENCH
fi
./tools/bench/run.sh "再起動試験後"
