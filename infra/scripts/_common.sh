# 4 つのスクリプトで共有する土台。
#
# **環境が立っていることを、進む前に確かめる。**
# apply していない状態で push を叩くと、terraform output のエラーと
# python の traceback が並んで出て、**本当の原因 (apply していない) が
# 読み取れない。** CD (.github/workflows/deploy.yml) には
# 同じ確認を入れてあったのに、手元のスクリプトには無かった。

TF_DIR="${TF_DIR:-infra/terraform}"

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
