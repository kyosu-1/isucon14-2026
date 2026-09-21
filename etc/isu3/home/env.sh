# DB は isucon14-2（deploy.sh が private IP に置換する）
ISUCON_DB_HOST="__ISU2_IP__"
ISUCON_DB_PORT="3306"
ISUCON_DB_USER="isucon"
ISUCON_DB_PASSWORD="isucon"
ISUCON_DB_NAME="isuride"

# マッチング間隔（秒）
ISUCON_MATCHING_INTERVAL=0.1

# GC の頻度を下げる。負荷後半の CPU プロファイルで GC が ~10%（アシスト 5.7% + バックグラウンド 4.6%）。
# RSS 385MB・空き 2.7GB なので、ヒープを5倍まで許す。上限は GOMEMLIMIT で押さえる
GOGC=400
GOMEMLIMIT=2GiB
