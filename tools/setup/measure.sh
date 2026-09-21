#!/usr/bin/env bash
# MySQL スロークエリ記録のON/OFFを切り替える。
#
# SET PERSIST で mysqld-auto.cnf に永続化するので、MySQLを再起動しても設定が保たれる。
# （SET GLOBAL だけだと、my.cnf を変えて mysql を再起動した瞬間に計測が黙って切れる）
#
# long_query_time はセッション開始時にグローバル値がコピーされる。
# アプリのコネクションプールに残っている既存接続には効かないので、ONにしたらアプリを再起動する。
#
# 最終スコアを取る前には必ず off にすること（long_query_time=0 は全クエリをディスクに書く）。
#
# usage: ./tools/setup/measure.sh {on|off} [db_host]
set -euo pipefail

MODE="${1:-}"
HOST="${2:-isucon14-1}"
SLOW_FILE=/var/log/mysql/slow.log

case "$MODE" in
  on)  SQL="SET PERSIST slow_query_log_file = '$SLOW_FILE';
            SET PERSIST long_query_time = 0;
            SET PERSIST slow_query_log = 1;" ;;
  off) SQL="SET PERSIST slow_query_log = 0;" ;;
  *)   echo "usage: measure.sh {on|off} [db_host]" >&2; exit 1 ;;
esac

echo "==> $HOST : slow query log $MODE"
ssh "$HOST" "set -euo pipefail
  sudo mysql -e \"$SQL\"
  sudo mysql -e 'SELECT @@slow_query_log AS slow_query_log, @@long_query_time AS long_query_time, @@slow_query_log_file AS file'
"

# アプリの既存接続に long_query_time を反映させる
for h in ${APP_HOSTS:-$HOST}; do
  ssh "$h" 'systemctl is-enabled --quiet isuride-go 2>/dev/null && sudo systemctl restart isuride-go || true'
done
