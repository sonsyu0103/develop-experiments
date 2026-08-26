# 4 つのスクリプトで共有する土台。
#
# **環境が立っていることを、進む前に確かめる。**
# apply していない状態で push を叩くと、terraform output のエラーと
# python の traceback が並んで出て、**本当の原因 (apply していない) が
# 読み取れない。** CD (.github/workflows/deploy.yml) には
# 同じ確認を入れてあったのに、手元のスクリプトには無かった。

# **cwd に依存しない。**
# 相対パスにすると、リポジトリルート以外から実行したときに
# terraform -chdir がディレクトリ不在で落ちるが、
# require_applied が stderr を捨てるので
# **「AWS 環境がまだありません」という誤った診断**に化ける ——
# このファイルが直そうとした失敗と同じ形になる。
SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/../.." && pwd)"
TF_DIR="${TF_DIR:-$REPO_ROOT/infra/terraform}"

# docker build のコンテキストも同じ理由で絶対パスにする。
cd "$REPO_ROOT"

tf() { terraform -chdir="$TF_DIR" "$@"; }

# **終了コードで判定しない。**
# `terraform output` は state が無くても **exit 0 を返す**
# (エラーメッセージは標準エラーに出すのに)。`if ! tf output ...` では
# 素通りするので、`-json` が空オブジェクトを返すかどうかで見る。
require_applied() {
	local out
	out="$(tf output -json 2>/dev/null || true)"
	if [ -z "$out" ] || [ "$out" = "{}" ]; then
		cat >&2 <<'MSG'

AWS 環境がまだありません。

  make tf-apply     環境を作る (15 分ほど。**課金が発生する**)

apply が済んでから、この順で:

  make tf-push      イメージを ECR へ
  make tf-migrate   マイグレーションを流す
  make tf-verify    実測する

MSG
		exit 1
	fi
}

# JSON の output から 1 つのキーを取り出す。
tf_output_key() {
	tf output -json "$1" | python3 -c "import json,sys;print(json.load(sys.stdin)['$2'])"
}

# サービスを新しいイメージで入れ替え、安定するまで待つ。
#
# **push だけでは切り替わらない。** 稼働中のタスクは古いイメージのまま
# 走り続けるので、force-new-deployment で引き直させる必要がある。
# これを忘れると「新しいイメージを push したのに、古いタスクを計測して
# 検査が通る」という、いちばん気づけない形になる。
roll_services() {
	local region cluster api web
	region="$(tf output -raw region)"
	cluster="$(tf output -raw ecs_cluster_name)"
	api="$(tf_output_key ecs_service_names api)"
	web="$(tf_output_key ecs_service_names web)"

	echo "== サービスを入れ替えています =="
	local svc
	for svc in "$api" "$web"; do
		aws ecs update-service --region "$region" --cluster "$cluster" \
			--service "$svc" --force-new-deployment >/dev/null
		echo "   $svc"
	done

	# **安定するまで待つ。** 待たないと、古いタスクが動いている状態で
	# 「デプロイできた」と言うことになる。
	echo "== 安定するのを待っています (数分かかる) =="
	aws ecs wait services-stable --region "$region" --cluster "$cluster" \
		--services "$api" "$web"
}
