#!/usr/bin/env bash
#
# CD (.github/workflows/deploy.yml) が使う値を、GitHub の Variables に登録する。
#
# **値をハードコードしない。** terraform output から取る。
# 立て直すと CloudFront のドメインが変わるので、
# 手で貼り直す運用にすると必ずどこかで古い値が残る。
#
# **Secrets ではなく Variables に入れる。**
# ここに入るのはロール ARN やクラスタ名で、知られても他人には使えない
# —— OIDC の信頼ポリシーがリポジトリを限定しているため
# (infra/terraform/iam.tf)。AWS の資格情報そのものは 1 つも置かない。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=infra/scripts/_common.sh
. "$SCRIPT_DIR/_common.sh"

require_applied

REPO="${REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}"
echo "== $REPO に登録します =="

# ECR の output は完全な URL なので、リポジトリ名だけを取り出す
# (deploy.yml が "$REGISTRY/$ECR_REPO_API" の形で組み立てるため)。
ecr_name() { tf_output_key ecr_repository_urls "$1" | sed 's|.*/||'; }

# public_url は https:// 付きなので、ホスト名だけにする。
cf_domain() { tf output -raw public_url | sed 's|^https://||'; }

set_var() {
	printf '  %-24s %s\n' "$1" "$2"
	gh variable set "$1" --repo "$REPO" --body "$2" >/dev/null
}

set_var AWS_DEPLOY_ROLE_ARN "$(tf output -raw github_actions_role_arn)"
set_var ECS_CLUSTER         "$(tf output -raw ecs_cluster_name)"
set_var ECS_SERVICE_API     "$(tf_output_key ecs_service_names api)"
set_var ECS_SERVICE_WEB     "$(tf_output_key ecs_service_names web)"
set_var ECS_TASK_MIGRATE    "$(tf output -raw migrate_task_definition)"
set_var ECR_REPO_API        "$(ecr_name api)"
set_var ECR_REPO_WEB        "$(ecr_name web)"
set_var ECR_REPO_MIGRATE    "$(ecr_name migrate)"
set_var CLOUDFRONT_DOMAIN   "$(cf_domain)"

echo
echo "登録しました。デプロイを流すには:"
echo "  gh workflow run deploy.yml --repo $REPO"
