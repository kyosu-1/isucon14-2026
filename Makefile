.DEFAULT_GOAL := help
SHELL := /bin/bash

# ---- 対象ホスト -------------------------------------------------------------
# isuenv が振ったssh設定(~/.ssh/isuenv_config)のHost名をそのまま使う。
# private IP は作り直すたびに変わるので `make hosts` で hosts.generated.mk に書き出す。
#
#   ENTRY     : ベンチマーカーがアクセスする入口（nginx が動くノード）
#   APP_HOSTS : 競技サーバー3台（レギュレーション上、使えるのはこの3台だけ）
#   BENCH     : ベンチマーカー専用ノード（競技サーバーではない）
ENTRY     ?= isucon14-1
APP_HOSTS ?= isucon14-1 isucon14-2 isucon14-3
DB_HOST   ?= isucon14-2

-include hosts.generated.mk
BENCH ?= isucon14-4

# tools/ 配下のスクリプトは環境変数で対象ホストを受け取る。
# Make変数は自動ではレシピの環境に入らないので明示的にexportする。
export ENTRY APP_HOSTS DB_HOST BENCH
export ISU1_IP ISU2_IP ISU3_IP BENCH_IP

# =============================================================================
help: ## このヘルプ
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---- 環境 -------------------------------------------------------------------
.PHONY: hosts
hosts: ## isuenvの現況からホスト情報(private IP)を再生成する
	@./tools/setup/gen-hosts.sh

.PHONY: status
status: ## 稼働中の環境とTTLを表示
	@AWS_PROFILE=$${AWS_PROFILE:-personal} isuenv list

.PHONY: info
info: ## 現在の対象ホストを表示
	@echo "ENTRY     = $(ENTRY)"
	@echo "APP_HOSTS = $(APP_HOSTS)"
	@echo "DB_HOST   = $(DB_HOST)"
	@echo "BENCH     = $(BENCH) ($(BENCH_IP))"
	@echo "ISU1..3   = $(ISU1_IP) $(ISU2_IP) $(ISU3_IP)"

.PHONY: down
down: ## 環境を破棄する（要確認）
	@AWS_PROFILE=$${AWS_PROFILE:-personal} isuenv down isucon14

# ---- 初期セットアップ（環境を作り直したら1回だけ） --------------------------
.PHONY: setup
setup: ## 環境を作り直したあとの初期化を全部やる（冪等）
	@./tools/setup/gen-hosts.sh
	@./tools/setup/bench-node.sh $(BENCH)
	@./tools/setup/install-tools.sh $(APP_HOSTS)
	@$(MAKE) deploy
	@$(MAKE) measure-on
	@echo "==> setup 完了。make bench でスコアを確認する。"

.PHONY: measure-on
measure-on: ## MySQLスロークエリ(long_query_time=0)を有効化
	@./tools/setup/measure.sh on $(DB_HOST)

.PHONY: measure-off
measure-off: ## 計測ログを止める（最終スコア狙いのベンチ前に実行）
	@./tools/setup/measure.sh off $(DB_HOST)

# ---- デプロイ ---------------------------------------------------------------
.PHONY: build
build: ## Goアプリをlinux/amd64向けにクロスビルド
	@./tools/deploy/build.sh

.PHONY: deploy
deploy: ## ローカルの変更を全台に配布して再起動（ローカルが正）
	@./tools/deploy/build.sh
	@./tools/deploy/deploy.sh $(APP_HOSTS)

# ---- 計測ループ -------------------------------------------------------------
.PHONY: bench
bench: ## ログ初期化 → ベンチ → 解析 → 記録 まで一気にやる（メインの入口）
	@./tools/bench/run.sh "$(NOTE)"

.PHONY: analyze
analyze: ## 直近のログをその場で解析して表示（ベンチなし）
	@./tools/analyze/alp.sh $(ENTRY); ./tools/analyze/slow.sh $(DB_HOST) | head -150

.PHONY: alp
alp: ## アクセスログをalpで集計
	@./tools/analyze/alp.sh $(ENTRY)

.PHONY: slow
slow: ## スロークエリをpt-query-digestで集計
	@./tools/analyze/slow.sh $(DB_HOST)

.PHONY: explain
explain: ## SQLの実行計画 (make explain SQL="SELECT ...")
	@./tools/analyze/explain.sh "$(SQL)" $(DB_HOST)

# ---- 検証 -------------------------------------------------------------------
.PHONY: restart-test
restart-test: ## 再起動試験（3台とも再起動してもベンチが通ること）
	@./tools/bench/restart-test.sh $(APP_HOSTS)

.PHONY: logs
logs: ## アプリのエラーログを追う
	@ssh $(ENTRY) 'sudo journalctl -f -u isuride-go -n 100'
