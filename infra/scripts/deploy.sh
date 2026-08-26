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

roll_services

echo
echo "デプロイしました: $(tf output -raw public_url)"
