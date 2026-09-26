#!/usr/bin/env bash
#
# apply したあとの AWS 環境を実測する。
#
# **ADR 0024 の「まだ確かめていないこと」に対応する。**
# 手元の make https-verify と同じ考え方で、
# 「そう書いてある」ことではなく「実際にそうなる」ことを見る。
#
# とくに 4 が本命になる。CloudFront ←→ ALB が HTTP なので
# ALB が X-Forwarded-Proto を http で上書きし、
# CORS_ALLOWED_ORIGINS の明示だけが書き込みを守っている
# (ADR 0023 の 3 で手元に再現した形)。**そこが効いているかを測る。**

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=infra/scripts/_common.sh
. "$SCRIPT_DIR/_common.sh"

require_applied

fail=0
pass() { printf '  \033[32mOK\033[0m    %s\n' "$1"; }
bad()  { printf '  \033[31mNG\033[0m    %s\n' "$1"; fail=$((fail + 1)); }
skip() { printf '  \033[33mSKIP\033[0m  %s\n' "$1"; }

BASE="$(tf output -raw public_url)"
ALB="$(tf output -raw alb_dns_name)"

req()    { curl -s --max-time 20 "$@"; }
status() { req -o /dev/null -w '%{http_code}' "$@"; }

echo
echo "対象: $BASE"

echo
echo "=== 1. 振り分け (CloudFront -> ALB / S3) ==="

[ "$(status "$BASE/")" = "200" ] \
	&& pass "/ が next-app に届く" \
	|| bad  "/ が next-app に届かない"

# **CloudFront Function のパス剥がしが効いているか。**
# 剥がれていないと go-api には /api/healthz が届き、404 になる。
[ "$(status "$BASE/api/healthz")" = "200" ] \
	&& pass "/api/* が go-api に届く (Function がプレフィックスを剥がしている)" \
	|| bad  "/api/* が go-api に届かない (パス剥がしか、ポート分けを疑う)"

api_body="$(req "$BASE/api/healthz")"
case "$api_body" in
	*'"status":"ok"'*) pass "go-api の応答が正しい形" ;;
	*)                 bad  "go-api の応答が想定と違う: $api_body" ;;
esac

[ "$(status "$BASE/api/threads")" = "200" ] \
	&& pass "一覧が引ける (RDS まで到達している)" \
	|| bad  "一覧が引けない (RDS への到達か、マイグレーション未実行を疑う)"

echo
echo "=== 2. CloudFront を迂回できないか ==="
# **ALB の SG が CloudFront のプレフィックスリストだけを許しているか。**
# 迂回できると、CloudFront で付けているセキュリティヘッダが素通しになる。
# 塞がっていれば接続がタイムアウトする (拒否ではなく無応答)。
# **`|| echo` を足さない。**
# curl は失敗しても -w の値 (000) を出力するので、`|| echo timeout` を
# 付けると "000timeout" のように連結し、case のどれにも当たらない。
# 終了コードは別に受ける。
alb_code="$(curl -s --max-time 8 -o /dev/null -w '%{http_code}' "http://$ALB/" 2>/dev/null)" || true
case "$alb_code" in
	000|"") pass "ALB を直接叩いても届かない (SG で塞がっている)" ;;
	*)      bad  "ALB に直接届いてしまう (got $alb_code) —— CloudFront を迂回できる" ;;
esac

echo
echo "=== 3. セキュリティヘッダ (ADR 0013 決定 2) ==="
for path in "/" "/api/threads"; do
	headers="$(req -D - -o /dev/null "$BASE$path")"
	for h in "x-content-type-options: nosniff" \
	         "referrer-policy: strict-origin-when-cross-origin" \
	         "x-frame-options: DENY"; do
		if printf '%s' "$headers" | grep -qi "^$h"; then
			pass "$path に ${h%%:*} が付く"
		else
			bad  "$path に ${h%%:*} が付かない"
		fi
	done
done

# **本番では HSTS を実効値にする** (手元は max-age=0)。
hsts="$(req -D - -o /dev/null "$BASE/" | grep -i '^strict-transport-security:' | tr -d '\r' || true)"
case "$hsts" in
	*"max-age=0"*) bad  "HSTS が max-age=0 のまま (手元の設定が本番に出ている)" ;;
	"")            bad  "HSTS ヘッダが無い" ;;
	*)             pass "HSTS が有効 (${hsts#*: })" ;;
esac

echo
echo "=== 4. 書き込みが通るか (X-Forwarded-Proto 上書きへの耐性) ==="
# **ここが本命。**
# ALB は XFP を自分の受信スキーム (http) で上書きするので、
# go-api の selfOrigin() は http:// を組み立てる。
# CORS_ALLOWED_ORIGINS に CloudFront のドメインが入っていなければ、
# **すべての書き込みが 403 になる。**
body='{"title":"aws-verify","body":"probe"}'
post() {
	req -o /dev/null -w '%{http_code}' -X POST "$BASE/api/threads" \
		-H "Content-Type: application/json" -d "$body" "$@"
}

write_code="$(post -H "Origin: $BASE")"
[ "$write_code" = "201" ] \
	&& pass "書き込みが通る (CORS の明示が XFP 上書きを吸収している)" \
	|| bad  "書き込みが $write_code —— CORS_ALLOWED_ORIGINS が公開ドメインと一致していない"

[ "$(post -H "Origin: https://evil.test")" = "403" ] \
	&& pass "別オリジンからの書き込みは 403" \
	|| bad  "別オリジンからの書き込みが弾かれない"

[ "$(post)" = "403" ] \
	&& pass "Origin なしの書き込みは 403" \
	|| bad  "Origin なしの書き込みが弾かれない"

echo
echo "=== 5. Cookie の Secure 属性 (ADR 0005) ==="
cookie="$(req -D - -o /dev/null "$BASE/api/auth/google" | grep -i '^set-cookie: oauth_state' | tr -d '\r' || true)"
if [ -z "$cookie" ]; then
	skip "Cookie を検査できない (google_client_id 未設定)"
else
	case "$cookie" in
		*Secure*) pass "Cookie に Secure が付く" ;;
		*)        bad  "Cookie に Secure が付いていない" ;;
	esac
fi

echo
echo "=== 6. 画像経路 (S3 + OAC) ==="
# **アップロードして確かめることはできない。**
# POST /images は sessionCookie を要求する (api/openapi.yaml) ので、
# セッションを持たないこのスクリプトからは必ず 401 になる。
# 初版はそこで SKIP に落ち、**S3_PUBLIC_BASE_URL を一度も通らないまま
# 「到達している」と報告していた** —— ADR 0024 の 14 で踏んだ
# 二重プレフィックスを、強化したはずの検査が構造的に見逃す形だった。
#
# 代わりに**設定値そのものを見る。** オブジェクトキーが
# "images/<uuid>" で始まる (image/domain/model/image.go) ので、
# S3_PUBLIC_BASE_URL が /images で終わっていたら必ず二重になる。
# これはアップロードしなくても判定できる。
region="$(tf output -raw region)"
s3_base="$(aws ecs describe-task-definition \
	--region "$region" \
	--task-definition "$(tf output -raw ecs_cluster_name)-go-api" \
	--query "taskDefinition.containerDefinitions[0].environment[?name=='S3_PUBLIC_BASE_URL'].value | [0]" \
	--output text 2>/dev/null || echo "")"

case "$s3_base" in
	"")
		bad "S3_PUBLIC_BASE_URL がタスク定義から読めない" ;;
	*/images | */images/)
		bad "S3_PUBLIC_BASE_URL が /images で終わっている ($s3_base) —— キーが images/ で始まるので二重になる" ;;
	"$BASE")
		pass "S3_PUBLIC_BASE_URL が公開 URL と一致している ($s3_base)" ;;
	*)
		bad "S3_PUBLIC_BASE_URL が公開 URL と違う (got $s3_base, want $BASE)" ;;
esac

# 経路そのものの到達性も見る。存在しないキーには 404 を期待する。
#
# **404 が返るのは s3.tf の ListBucket 文があるからで、GetObject では
# 足りない。** S3 は ListBucket を持たない主体には、キーが無いときも
# 403 AccessDenied を返す (不存在を教えないため)。
# 初版のコメントは「arn/* に GetObject を与えているため 404」と
# 書いていたが理屈が逆で、**この検査は 2026-09-26 まで一度も
# 通ったことがなかった** (ADR 0024 の 16)。
#
# **s3.tf の ListBucket 文はこの検査を支えている (load-bearing)。**
# 消すと 403 に戻ってここが落ちる —— そのとき期待値を 403 に
# 緩めてはいけない。**403 は OAC が壊れているときにも返る**ので、
# 緩めると診断力がゼロになる。404 が返ること自体が
# 「認可が成立している」証明になっている。
probe_key="images/00000000-0000-0000-0000-000000000000.webp"
img_code="$(status "$BASE/$probe_key")"
case "$img_code" in
	404) pass "S3 オリジンに到達し、存在しないキーは 404 (OAC が効いている)" ;;
	403) bad  "存在しないキーが 403 —— OAC かバケットポリシーが効いていない" ;;
	*)   bad  "S3 オリジンの応答が想定外 (status=$img_code)" ;;
esac

echo
echo "=== 7. 秘密がタスク定義に平文で入っていないか (ADR 0024 決定 7) ==="

# **塞いだ穴に検査を付ける。**
#
# GOOGLE_CLIENT_SECRET が environment に平文で入っていた
# (ecs:DescribeTaskDefinition があれば読める)。直したが、
# **同じ間違いはいつでも再発できる** —— environment に 1 行足すほうが
# secrets + IAM を足すより楽なので、楽なほうに倒れる。
#
# 手で「SECRET を含む項目 0 件」を数えるのは、MAIL_SMTP_PASSWORD の
# ような名前を取り逃す。**名前の規則で機械的に見る。**
#
# ここで見るのは名前だけで、値は読まない (読めば検査のログに秘密が乗る)。
secretish='(SECRET|PASSWORD|TOKEN|CREDENTIAL|PRIVATE_KEY|ACCESS_KEY)'
cluster="$(tf output -raw ecs_cluster_name)"

for family in "$cluster-go-api" "$cluster-next-app" "$cluster-migrate"; do
	names="$(aws ecs describe-task-definition \
		--region "$region" \
		--task-definition "$family" \
		--query 'taskDefinition.containerDefinitions[].environment[].name' \
		--output text 2>/dev/null || echo "__READ_FAILED__")"

	case "$names" in
		__READ_FAILED__)
			bad "$family のタスク定義を読めない" ;;
		*)
			hits="$(printf '%s\n' $names | grep -E "$secretish" || true)"
			if [ -n "$hits" ]; then
				bad "$family の environment に秘密らしい名前がある: $(printf '%s ' $hits)"
			else
				pass "$family の environment に秘密らしい名前は無い"
			fi ;;
	esac
done

echo
echo "=== 後片付け ==="
echo "  検査で作ったスレッドは残ります。消すには:"
echo "    make tf-secret で接続情報を見て、DELETE FROM threads WHERE title='aws-verify';"

echo
if [ "$fail" -ne 0 ]; then
	echo "失敗: $fail 件"
	exit 1
fi
echo "すべて通った。"
