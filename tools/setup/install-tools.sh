#!/usr/bin/env bash
# 計測ツールをサーバーへ導入する。冪等なので何度流してもよい。
#
#   alp              : nginx アクセスログ(LTSV)をエンドポイント単位で集計する
#   pt-query-digest  : MySQLスロークエリをクエリ単位で集計する（percona-toolkit）
#   pidstat/vmstat   : プロセス別CPU・マシン全体のCPU/IO（sysstat）
#
# usage: ./tools/setup/install-tools.sh [host...]
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

HOSTS=("${@:-isucon14-1}")
ALP_VERSION=v1.0.21

for HOST in "${HOSTS[@]}"; do
  echo "==> $HOST"
  ssh "$HOST" "set -euo pipefail
    export DEBIAN_FRONTEND=noninteractive

    if ! command -v alp >/dev/null 2>&1; then
      cd /tmp
      curl -fsSL -o alp.tar.gz \
        https://github.com/tkuchiki/alp/releases/download/${ALP_VERSION}/alp_linux_amd64.tar.gz
      tar xf alp.tar.gz alp
      sudo install -m 0755 alp /usr/local/bin/alp
      rm -f alp alp.tar.gz
    fi

    if ! command -v pt-query-digest >/dev/null 2>&1; then
      sudo apt-get update -qq
      sudo apt-get install -y -qq percona-toolkit >/dev/null
    fi
    command -v pidstat >/dev/null 2>&1 || sudo apt-get install -y -qq sysstat >/dev/null

    # 自動アップデートがベンチ中にCPUを食う（実測 isucon14-3 で 78%）ので止める
    sudo systemctl disable --now unattended-upgrades apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true

    sudo mkdir -p /var/log/mysql
    sudo chown mysql:adm /var/log/mysql

    echo -n '  alp: '; alp --version 2>&1 | head -1
    echo -n '  pt-query-digest: '; pt-query-digest --version 2>&1 | head -1
  "
done
