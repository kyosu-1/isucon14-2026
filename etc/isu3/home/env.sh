# DB は isucon14-2（deploy.sh が private IP に置換する）
ISUCON_DB_HOST="__ISU2_IP__"
ISUCON_DB_PORT="3306"
ISUCON_DB_USER="isucon"
ISUCON_DB_PASSWORD="isucon"
ISUCON_DB_NAME="isuride"

# マッチング間隔（秒）
# マッチング間隔（秒）。椅子が足りない状況では、解放された椅子が次のマッチングまで待つ時間がそのまま無駄になる
ISUCON_MATCHING_INTERVAL=0.1
