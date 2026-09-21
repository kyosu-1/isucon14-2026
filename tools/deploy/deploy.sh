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
#   etc/isuN/services        そのノードで enable するサービスの一覧（それ以外の管理対象は disable）
#
# etc/ 配下の __ISU1_IP__ / __ISU2_IP__ / __ISU3_IP__ は hosts.generated.mk の private IP に置換して配る
# （環境を作り直すと IP が変わるため、IP を直書きしない）。
#
# DB_HOST を先に配ってから残りを並列に配る（アプリがDBの再起動中に起動して落ちるのを避ける）。
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
MANAGED="nginx mysql isuride-go isuride-matcher isuride-payment_mock"

[ -x build/isuride ] || { echo "build/isuride がありません。make build を先に実行してください" >&2; exit 1; }

mk() { awk -F':= *' -v k="$1" '$1 ~ "^"k" *$" {print $2}' hosts.generated.mk 2>/dev/null; }
ISU1_IP="${ISU1_IP:-$(mk ISU1_IP)}"; ISU2_IP="${ISU2_IP:-$(mk ISU2_IP)}"; ISU3_IP="${ISU3_IP:-$(mk ISU3_IP)}"
[ -n "$ISU1_IP" ] && [ -n "$ISU2_IP" ] && [ -n "$ISU3_IP" ] || { echo "hosts.generated.mk がありません。make hosts を先に実行してください" >&2; exit 1; }

# 未コミットの変更があるまま配布すると、あとで「どのコードのスコアか」が分からなくなる。
if ! git diff --quiet HEAD -- webapp etc 2>/dev/null; then
  echo "warning: webapp/ か etc/ に未コミットの変更があります。ベンチ前に commit してください。" >&2
fi

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

deploy_one() {
  local HOST="$1"
  local n="${HOST##*-}"
  local src="etc/isu$n"
  local dir="$STAGE/isu$n"

  # プレースホルダを置換した配布用コピーを作る（シンボリックリンクはそのまま）
  mkdir -p "$dir"
  rsync -a "$src/" "$dir/"
  grep -rlE '__ISU[123]_IP__' "$dir" 2>/dev/null | while read -r f; do
    sed -i.bak -e "s/__ISU1_IP__/$ISU1_IP/g" -e "s/__ISU2_IP__/$ISU2_IP/g" -e "s/__ISU3_IP__/$ISU3_IP/g" "$f" && rm -f "$f.bak"
  done

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
  local want="$MANAGED"
  [ -f "$dir/services" ] && want="$(grep -v '^#' "$dir/services" | xargs)"

  {
    [ -n "$changed_nginx$changed_mysql$changed_systemd$changed_env" ] && \
      echo "    [$HOST] 設定変更: ${changed_nginx:+nginx }${changed_mysql:+mysql }${changed_systemd:+systemd }${changed_env:+env.sh}"

    ssh "$HOST" "set -euo pipefail
      sudo chown root:root -R /etc/nginx /etc/mysql 2>/dev/null || true
      ${changed_systemd:+sudo systemctl daemon-reload}

      # このノードの役割: services に書かれたものだけ enable、他の管理対象は disable
      for s in $MANAGED; do
        case ' $want ' in
          *\" \$s \"*) systemctl is-enabled --quiet \$s 2>/dev/null || sudo systemctl enable --now \$s >/dev/null 2>&1 ;;
          *)           systemctl is-enabled --quiet \$s 2>/dev/null && sudo systemctl disable --now \$s >/dev/null 2>&1 || true ;;
        esac
      done

      if [ -n '${changed_mysql}' ] && systemctl is-enabled --quiet mysql; then sudo systemctl restart mysql; fi
      if [ -n '${changed_nginx}' ] && systemctl is-enabled --quiet nginx; then sudo nginx -t -q && sudo systemctl reload nginx; fi
      for s in isuride-go isuride-matcher; do
        systemctl is-enabled --quiet \$s 2>/dev/null && sudo systemctl restart \$s
      done
      sleep 1
      printf '    [%s] ' '$HOST'
      for s in $MANAGED; do
        systemctl is-enabled --quiet \$s 2>/dev/null && printf '%s=%s ' \$s \$(systemctl is-active \$s)
      done
      if systemctl is-active --quiet nginx; then
        printf 'api=%s' \$(curl -sk -o /dev/null -w '%{http_code}' --resolve isuride.xiv.isucon.net:443:127.0.0.1 'https://isuride.xiv.isucon.net/api/app/nearby-chairs?latitude=0&longitude=0')
      fi
      echo
    "
  } > "$STAGE/out-$HOST.log" 2>&1
}

ORDERED=()
for h in "${HOSTS[@]}"; do [ "$h" = "${DB_HOST:-}" ] && ORDERED+=("$h"); done
for h in "${HOSTS[@]}"; do [ "$h" != "${DB_HOST:-}" ] && ORDERED+=("$h"); done

FAILED=0
if [ "${ORDERED[0]}" = "${DB_HOST:-}" ]; then
  echo "==> ${ORDERED[0]} : deploy (DB)"
  deploy_one "${ORDERED[0]}" || FAILED=1
  cat "$STAGE/out-${ORDERED[0]}.log"
  ORDERED=("${ORDERED[@]:1}")
fi
pids=()
for HOST in "${ORDERED[@]}"; do
  echo "==> $HOST : deploy"
  deploy_one "$HOST" & pids+=($!)
done
for p in "${pids[@]}"; do wait "$p" || FAILED=1; done
for HOST in "${ORDERED[@]}"; do cat "$STAGE/out-$HOST.log"; done

[ "$FAILED" = 0 ] || { echo "!! deploy に失敗したノードがあります" >&2; exit 1; }
echo "==> deployed to: ${HOSTS[*]} ($(git rev-parse --short HEAD 2>/dev/null || echo -))"
