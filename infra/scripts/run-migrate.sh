#!/usr/bin/env bash
#
# マイグレーションを AWS 上で 1 回だけ流す。
#
# **サービスにしない。** 常駐させると、失敗のたびに再起動を繰り返し、
# 「マイグレーションが延々と走り続ける」状態になる。
# run-task で 1 回流し、終了コードを見る。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=infra/scripts/_common.sh
. "$SCRIPT_DIR/_common.sh"

require_applied

REGION="$(tf output -raw region)"
CLUSTER="$(tf output -raw ecs_cluster_name)"
TASK_DEF="$(tf output -raw migrate_task_definition)"
# **JSON をそのまま渡す。**
# --network-configuration は shorthand 記法 (key=value,...) と
# JSON の両方を受け付けるが、**混ぜられない。**
# terraform output -json は JSON を返すので、
# awsvpcConfiguration ごと JSON で包む。
NET="$(tf output -json run_task_network_configuration)"

# **サブコマンドを渡せるようにする。**
# 既定は up (タスク定義の CMD)。dirty から復旧するときは:
#   make tf-migrate ARGS="force 9"
# **空配列の展開に注意。**
# macOS の bash は 3.2 で、set -u のもとでは "${arr[@]}" が
# 「未定義変数」として落ちる。${arr[@]+"${arr[@]}"} と書くと、
# 空のときは何も展開されない。
OVERRIDES=()
if [ "$#" -gt 0 ]; then
	echo "== migrate $* を実行します =="
	OVERRIDES=(--overrides "$(python3 -c '
import json, sys
print(json.dumps({"containerOverrides": [{"name": "migrate", "command": sys.argv[1:]}]}))
' "$@")")
fi

echo "== マイグレーションを起動しています =="
TASK_ARN="$(aws ecs run-task \
	--region "$REGION" \
	--cluster "$CLUSTER" \
	--task-definition "$TASK_DEF" \
	--launch-type FARGATE \
	--network-configuration "{\"awsvpcConfiguration\":$NET}" \
	${OVERRIDES[@]+"${OVERRIDES[@]}"} \
	--query 'tasks[0].taskArn' --output text)"

echo "   $TASK_ARN"
echo "== 終了を待っています =="
aws ecs wait tasks-stopped --region "$REGION" --cluster "$CLUSTER" --tasks "$TASK_ARN"

# **終了コードを必ず見る。**
# run-task はタスクの起動に成功した時点で成功を返すので、
# マイグレーションが失敗しても、ここを見ないと気づけない。
EXIT_CODE="$(aws ecs describe-tasks --region "$REGION" --cluster "$CLUSTER" --tasks "$TASK_ARN" \
	--query 'tasks[0].containers[0].exitCode' --output text)"

REASON="$(aws ecs describe-tasks --region "$REGION" --cluster "$CLUSTER" --tasks "$TASK_ARN" \
	--query 'tasks[0].stoppedReason' --output text)"

echo "   exitCode=$EXIT_CODE  reason=$REASON"

if [ "$EXIT_CODE" != "0" ]; then
	echo
	echo "失敗しました。ログを見てください:"
	echo "  aws logs tail /ecs/$(tf output -raw ecs_cluster_name)/migrate --region $REGION"
	exit 1
fi

echo "マイグレーションが完了しました。"
