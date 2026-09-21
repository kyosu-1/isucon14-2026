#!/usr/bin/env bash
# ローカルの内容をサーバーへ配布し、必要なものだけ再起動する。
#
# 「サーバー上で直接編集しない」を守るための唯一の反映経路。
# アプリも /etc の設定も、ローカルのgitが正。
#
# 配るもの（ホストごとの設定は etc/isu<N>/ に置く）:
#   build/isuride            -> /home/isucon/webapp/go/isuride
#   webapp/sql/              -> /home/isucon/webapp/sql/      (POST /api/initialize が init.sh を叩く)
#   etc/isuN/nginx/          -> /etc/nginx/
#   etc/isuN/mysql/          -> /etc/mysql/                   (変更があったときだけ mysql を再起動)
#   etc/isuN/systemd/        -> /etc/systemd/system/
#   etc/isuN/home/env.sh     -> /home/isucon/env.sh
#
# usage: ./tools/deploy/deploy.sh [host...]
set -euo pipefail
# isuenv の ssh 設定は known_hosts を持たないので、毎回出る "Permanently added" 警告を黙らせる
ssh() { command ssh -o LogLevel=ERROR "$@"; }
scp() { command scp -o LogLevel=ERROR "$@"; }
export RSYNC_RSH="ssh -o LogLevel=ERROR"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

HOSTS=("${@:-isucon14-1}")
REMOTE=/home/isucon/webapp

[ -x build/isuride ] || { echo "build/isuride がありません。make build を先に実行してください" >&2; exit 1; }

# 未コミットの変更があるまま配布すると、あとで「どのコードのスコアか」が分からなくなる。
if ! git diff --quiet HEAD -- webapp etc 2>/dev/null; then
  echo "warning: webapp/ か etc/ に未コミットの変更があります。ベンチ前に commit してください。" >&2
fi

deploy_one() {
  local HOST="$1"
  local n="${HOST##*-}"
  local dir="etc/isu$n"
  local log="/tmp/deploy-$HOST.log"
  : > "$log"

  # SSHユーザー(ubuntu)は /home/isucon 配下に書けないので rsync 側を sudo -u isucon で動かす
  local RS_ISUCON='sudo -u isucon rsync'
  rsync -az --rsync-path="$RS_ISUCON" build/isuride "$HOST:$REMOTE/go/isuride"
  rsync -az --rsync-path="$RS_ISUCON" webapp/sql/ "$HOST:$REMOTE/sql/"

  local changed_nginx="" changed_mysql="" changed_systemd="" changed_env=""
  if [ -d "$dir/nginx" ]; then
    changed_nginx="$(rsync -rlpcz -i --rsync-path='sudo rsync' "$dir/nginx/" "$HOST:/etc/nginx/" | grep -v '^\.' || true)"
  fi
  if [ -d "$dir/mysql" ]; then
    changed_mysql="$(rsync -rlpcz -i --rsync-path='sudo rsync' "$dir/mysql/" "$HOST:/etc/mysql/" | grep -v '^\.' || true)"
  fi
  if [ -d "$dir/systemd" ]; then
    changed_systemd="$(rsync -rlpcz -i --rsync-path='sudo rsync' "$dir/systemd/" "$HOST:/etc/systemd/system/" | grep -v '^\.' || true)"
  fi
  if [ -f "$dir/home/env.sh" ]; then
    changed_env="$(rsync -lpcz -i --rsync-path="$RS_ISUCON" "$dir/home/env.sh" "$HOST:/home/isucon/env.sh" | grep -v '^\.' || true)"
  fi

  [ -n "$changed_nginx$changed_mysql$changed_systemd$changed_env" ] && \
    echo "    [$HOST] 設定変更: ${changed_nginx:+nginx }${changed_mysql:+mysql }${changed_systemd:+systemd }${changed_env:+env.sh}"

  ssh "$HOST" "set -euo pipefail
    sudo chown root:root -R /etc/nginx /etc/mysql 2>/dev/null || true
    ${changed_systemd:+sudo systemctl daemon-reload}
    ${changed_mysql:+sudo systemctl restart mysql}
    if [ -n '${changed_nginx}' ]; then sudo nginx -t -q && sudo systemctl reload nginx; fi

    # 役割を外したノードでは isuride-go を disable しておけば、ここで触らない
    if systemctl is-enabled --quiet isuride-go 2>/dev/null; then
      sudo systemctl restart isuride-go
    fi
    if systemctl is-enabled --quiet isuride-matcher 2>/dev/null; then
      sudo systemctl restart isuride-matcher
    fi
    sleep 1
    printf '    [%s] ' '$HOST'
    for s in nginx mysql isuride-go isuride-matcher; do
      systemctl is-enabled --quiet \$s 2>/dev/null && printf '%s=%s ' \$s \$(systemctl is-active \$s)
    done
    if systemctl is-active --quiet nginx; then
      printf 'api=%s' \$(curl -sk -o /dev/null -w '%{http_code}' --resolve isuride.xiv.isucon.net:443:127.0.0.1 'https://isuride.xiv.isucon.net/api/app/nearby-chairs?latitude=0&longitude=0')
    fi
    echo
  "
}

for HOST in "${HOSTS[@]}"; do
  echo "==> $HOST : deploy"
  deploy_one "$HOST" &
done
wait

echo "==> deployed to: ${HOSTS[*]} ($(git rev-parse --short HEAD 2>/dev/null || echo -))"
