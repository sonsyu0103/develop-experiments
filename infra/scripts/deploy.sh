#!/usr/bin/env bash
#
# イメージを push して ECS のサービスを入れ替える。
#
# **タスク定義を作り直さない。** イメージタグを固定 (latest) にしてあるので、
# サービスに「新しいデプロイを始めろ」と言えば最新を取りに行く。
# タスク定義を毎回登録すると、リビジョンが際限なく増える。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=infra/scripts/_common.sh
. "$SCRIPT_DIR/_common.sh"

require_applied

bash "$SCRIPT_DIR/push-images.sh"

REGION="$(tf output -raw region)"
CLUSTER="$(tf output -raw ecs_cluster_name)"
API_SVC="$(tf_output_key ecs_service_names api)"
WEB_SVC="$(tf_output_key ecs_service_names web)"

echo
echo "== サービスを入れ替えています =="
for svc in "$API_SVC" "$WEB_SVC"; do
	aws ecs update-service --region "$REGION" --cluster "$CLUSTER" \
		--service "$svc" --force-new-deployment >/dev/null
	echo "   $svc"
done

# **安定するまで待つ。** 待たないと、まだ古いタスクが動いている状態で
# 「デプロイできた」と言うことになる。
echo "== 安定するのを待っています (数分かかる) =="
aws ecs wait services-stable --region "$REGION" --cluster "$CLUSTER" \
	--services "$API_SVC" "$WEB_SVC"

echo
echo "デプロイしました: $(tf output -raw public_url)"
