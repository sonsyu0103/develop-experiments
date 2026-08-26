#!/usr/bin/env bash
#
# イメージをビルドして ECR へ push する。
#
# **ARM64 で焼く。** ECS のタスク定義が Graviton (ARM64) を指定しているため
# (infra/terraform/ecs.tf)。x86 のイメージを push すると、
# タスクは起動を試みて **exec format error で落ち続ける** ——
# ECS のイベントには「タスクが停止した」としか出ないので、
# 原因にたどり着くまで時間がかかる。

set -euo pipefail

TAG="${TAG:-latest}"
PLATFORM="linux/arm64"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=infra/scripts/_common.sh
. "$SCRIPT_DIR/_common.sh"

require_applied

echo "== 出力を読み取っています =="
REGION="$(tf output -raw region)"
API_REPO="$(tf_output_key ecr_repository_urls api)"
WEB_REPO="$(tf_output_key ecr_repository_urls web)"
MIGRATE_REPO="$(tf_output_key ecr_repository_urls migrate)"
REGISTRY="${API_REPO%%/*}"

echo "== ECR にログインしています =="
aws ecr get-login-password --region "$REGION" |
	docker login --username AWS --password-stdin "$REGISTRY"

build_push() {
	local name="$1" repo="$2" context="$3" target="${4:-}"
	echo
	echo "== $name をビルドしています ($PLATFORM) =="
	if [ -n "$target" ]; then
		docker build --platform "$PLATFORM" --target "$target" -t "$repo:$TAG" "$context"
	else
		docker build --platform "$PLATFORM" -t "$repo:$TAG" "$context"
	fi
	docker push "$repo:$TAG"
}

build_push "go-api"   "$API_REPO"     "apps/go-api"     "prod"
build_push "next-app" "$WEB_REPO"     "apps/next-app"   "prod"
build_push "migrate"  "$MIGRATE_REPO" "apps/go-api/db"

echo
echo "push しました (tag=$TAG)"
