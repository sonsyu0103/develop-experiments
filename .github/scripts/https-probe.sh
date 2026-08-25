#!/usr/bin/env bash
#
# エッジ (infra/caddy) の配下でしか通らない分岐を実測する。
#
# **目視では気づけない壊れ方を捕まえる。**
# このリポジトリの HTTP 防御 (ADR 0013) とセッション Cookie (ADR 0005) は
# 「本番ならこう振る舞う」という推論のうえに書かれていた。compose が
# すべて http だったため、以下はどれも 1 度も実行されていない:
#
#   internal/httpapi/middleware.go  X-Forwarded-Proto によるスキーム判定
#   internal/httpapi/auth.go        Cookie の Secure 属性
#   internal/httpapi/server.go      TRUSTED_PROXIES (ClientIP の決定)
#
# とくに X-Forwarded-Proto は**落ちても静かに壊れる**。
# GET も画面も 200 のまま、書き込みだけが全部 403 になるため、
# GET のヘルスチェックしか見ていない監視は緑のままになる。
#
# 前提: make up-https で起動していること (make https-verify が面倒を見る)。

set -euo pipefail

SITE="${SITE_ADDRESS:-localhost}"
BASE="https://${SITE}"

fail=0
pass() { printf '  \033[32mOK\033[0m    %s\n' "$1"; }
bad()  { printf '  \033[31mNG\033[0m    %s\n' "$1"; fail=$((fail + 1)); }

# 自己署名なので -k が要る。証明書チェーンの検証はここの目的ではない
# (見たいのは「TLS 終端の向こう側で何が起きるか」)。
req()    { curl -sk --max-time 10 "$@"; }
status() { req -o /dev/null -w '%{http_code}' "$@"; }

# **エッジだけでなく upstream の復帰も待つ。**
# /edge-healthz はエッジ自身が返すので、go-api や next-app が起動途中でも
# 200 になる。そこで先に進むと、起動待ちの 502 を「検査結果」と取り違える。
#
# 待つ対象は 3 つとも要る。go-api は go run のコンパイル、
# next-app は初回の起動に時間がかかり、**それぞれ別のタイミングで上がる。**
wait_edge() {
	local i
	for i in $(seq 1 120); do
		if [ "$(status "$BASE/edge-healthz")" = "200" ] &&
			[ "$(status "$BASE/api/healthz")" = "200" ] &&
			[ "$(status "$BASE/")" = "200" ]; then
			return 0
		fi
		sleep 1
	done
	echo "エッジ / go-api / next-app のいずれかが応答しません ($BASE)" >&2
	return 1
}

# **どの経路で抜けても既定へ戻す。** XFP を壊したまま抜けると、
# 次に触った人の環境が「書き込みだけ 403」になり、原因が分からなくなる。
restore_edge() {
	docker compose --profile edge up -d edge >/dev/null 2>&1 || true
	wait_edge >/dev/null 2>&1 || true
}
trap restore_edge EXIT

# **検査の前提は検査自身が保証する。**
# make down の直後などは go-api / next-app のコンパイルに時間がかかり、
# 起動途中を測ると「エッジが振り分けていない」と誤判定する。
echo "  対象が起動するのを待っています..."
wait_edge

echo
echo "=== 1. エッジが本番と同じ形で振り分けているか ==="

[ "$(status "$BASE/edge-healthz")" = "200" ] \
	&& pass "エッジ自身が応答する" \
	|| bad  "エッジ自身が応答しない"

[ "$(status "$BASE/")" = "200" ] \
	&& pass "/ が next-app に届く" \
	|| bad  "/ が next-app に届かない"

# /api/* のプレフィックスは剥がされる (本番の CloudFront の Origin Path 相当)。
# 剥がし忘れると go-api 側が 404 を返すので、ここで気づける。
[ "$(status "$BASE/api/healthz")" = "200" ] \
	&& pass "/api/* が go-api に届く (プレフィックスが剥がれている)" \
	|| bad  "/api/* が go-api に届かない"

[ "$(status "$BASE/images/")" = "200" ] \
	&& pass "/images/* が MinIO に届く (本番の S3 + OAC 相当)" \
	|| bad  "/images/* が MinIO に届かない"

# http は 308 で https へ。**平文で受け付けたままにしない。**
redirect_code="$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' "http://${SITE}/")"
[ "$redirect_code" = "308" ] \
	&& pass "http は https へリダイレクトされる ($redirect_code)" \
	|| bad  "http が https へリダイレクトされない (got $redirect_code)"

echo
echo "=== 2. セキュリティヘッダ (ADR 0013 決定 2) ==="
# **API と画面の両方を見る。** ADR は「フロント側の応答に付ける」と
# 書いていたが、エッジで付ければ両方に乗る。片方だけ通る実装だと
# ここで露見する。
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
	if printf '%s' "$headers" | grep -qi "^server:"; then
		bad  "$path が Server ヘッダで素性を晒している"
	else
		pass "$path は Server ヘッダを出さない"
	fi
done

# **HSTS の既定は max-age=0。**
# HSTS はホスト単位でポートを区別しないので、localhost に有効な max-age を
# 付けると http://localhost:3000 での開発まで巻き添えで止まる
# (ADR 0013 決定 2 が「付けるとブラウザに記憶されて開発が壊れる」と書いている)。
hsts="$(req -D - -o /dev/null "$BASE/" | grep -i '^strict-transport-security:' | tr -d '\r')"
case "$hsts" in
	*"max-age=0"*) pass "HSTS の既定が max-age=0 (手元の開発を壊さない)" ;;
	"")            bad  "HSTS ヘッダが無い" ;;
	*)             pass "HSTS が明示的に有効化されている (${hsts#*: })" ;;
esac

echo
echo "=== 3. Cookie の Secure 属性 (ADR 0005) ==="
# エッジ経由は https なので、開発モードでも Secure を付けられる
# (SECURE_COOKIE=true)。ここが外れていると、開発環境だけが本番と違う
# Cookie を配ることになり、Secure 起因の不具合が手元で再現しない。
cookie="$(req -D - -o /dev/null "$BASE/api/auth/google" | grep -i '^set-cookie: oauth_state' | tr -d '\r' || true)"
if [ -z "$cookie" ]; then
	# 認証情報が無い環境では 503 になり Cookie が出ない。落とさずに知らせる。
	printf '  \033[33mSKIP\033[0m  Cookie を検査できない (GOOGLE_CLIENT_ID 未設定)\n'
else
	case "$cookie" in
		*Secure*) pass "セッション経路の Cookie に Secure が付く" ;;
		*)        bad  "Cookie に Secure が付いていない ($cookie)" ;;
	esac
	case "$cookie" in
		*HttpOnly*) pass "Cookie に HttpOnly が付く" ;;
		*)          bad  "Cookie に HttpOnly が付いていない" ;;
	esac
fi

echo
echo "=== 4. csrfGuard (ADR 0013 決定 1) ==="
body='{"title":"https-probe","body":"probe"}'
post() {
	req -o /dev/null -w '%{http_code}' -X POST "$BASE/api/threads" \
		-H "Content-Type: application/json" -d "$body" "$@"
}

ok_code="$(post -H "Origin: $BASE")"
[ "$ok_code" = "201" ] \
	&& pass "正しい Origin の書き込みは通る ($ok_code)" \
	|| bad  "正しい Origin の書き込みが通らない (got $ok_code, want 201)"

[ "$(post -H "Origin: https://evil.test")" = "403" ] \
	&& pass "別オリジンからの書き込みは 403" \
	|| bad  "別オリジンからの書き込みが弾かれない"

[ "$(post)" = "403" ] \
	&& pass "Origin なしの書き込みは 403" \
	|| bad  "Origin なしの書き込みが弾かれない"

echo
echo "=== 5. X-Forwarded-Proto と selfOrigin の因果 (検査が効いているかの確認) ==="
# **ここが本題。**
# 「XFP が要る」と書いてあることと、落としたときに実際に壊れることは別。
# 壊れないなら、この検査は何も守っていない ——
# LOG_FORMAT=text make logs-verify で検査の効きを見るのと同じ考え方。
#
# **エッジ経由では測れない。** isAllowedOrigin は許可リストを先に見るので
# (middleware.go)、CORS_ALLOWED_ORIGINS に自分のオリジンが入っていると
# selfOrigin() まで到達せず、XFP を落としても通ってしまう。
# そこで go-api を直接叩き、**許可リストに無いホスト名**を名乗ることで
# selfOrigin() だけを頼りにさせる。
DIRECT="http://localhost:8080"
probe_host="probe.test"
direct_post() {
	curl -s --max-time 10 -o /dev/null -w '%{http_code}' -X POST "$DIRECT/threads" \
		-H "Host: $probe_host" -H "Origin: https://$probe_host" \
		-H "Content-Type: application/json" -d '{"title":"https-probe","body":"probe"}' "$@"
}

[ "$(direct_post -H "X-Forwarded-Proto: https")" = "201" ] \
	&& pass "XFP があれば selfOrigin が https を組み立て、書き込みが通る" \
	|| bad  "XFP を付けても書き込みが通らない (selfOrigin の判定が想定と違う)"

xfp_dropped="$(direct_post)"
[ "$xfp_dropped" = "403" ] \
	&& pass "XFP を落とすと 403 になる (検査が効いている)" \
	|| bad  "XFP を落としても書き込みが通ってしまう (got $xfp_dropped) —— selfOrigin の判定が死んでいる"

# **同時に「静かに壊れる」ことも確かめる。**
# GET が 200 のままなのが、この故障がタチの悪い理由になる。
get_code="$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' "$DIRECT/threads" -H "Host: $probe_host")"
[ "$get_code" = "200" ] \
	&& pass "その状態でも GET は 200 (監視では気づけない)" \
	|| bad  "GET まで落ちている (想定と違う壊れ方をしている)"

echo
echo "=== 6. 多層防御: CORS の明示が XFP 落ちの保険になっているか ==="
# 5 の壊れ方は「CORS_ALLOWED_ORIGINS に自分のオリジンが入っていない」
# 構成でだけ起きる。make up-https は https://$SITE を明示しているので、
# **エッジが XFP を落としても書き込みは守られる**はず。
#
# これが成り立たないと、単一オリジン構成は XFP ヘッダ 1 本に命を預けることになる。
echo "  XFP=http でエッジを作り直しています..."
XFP=http docker compose --profile edge up -d edge >/dev/null 2>&1
wait_edge

guarded="$(post -H "Origin: $BASE")"
[ "$guarded" = "201" ] \
	&& pass "XFP を落としてもエッジ経由の書き込みは通る (CORS の明示が保険になっている)" \
	|| bad  "CORS を明示しているのに書き込みが落ちた (got $guarded) —— 保険が効いていない"

echo "  エッジを既定に戻しています..."
docker compose --profile edge up -d edge >/dev/null 2>&1
wait_edge

recovered="$(post -H "Origin: $BASE")"
[ "$recovered" = "201" ] \
	&& pass "既定に戻しても書き込みが通る ($recovered)" \
	|| bad  "既定に戻したのに書き込みが通らない (got $recovered)"

echo
echo "=== 後片付け ==="
# 検査で作ったスレッドを消す。残すと make smoke などの件数前提とずれる。
deleted="$(docker compose exec -T postgres \
	psql -U app -d bbs -tAc "DELETE FROM threads WHERE title = 'https-probe';" 2>/dev/null || echo "?")"
echo "  検査で作ったスレッドを削除: $deleted"

echo
if [ "$fail" -ne 0 ]; then
	echo "失敗: $fail 件"
	exit 1
fi
echo "すべて通った。"
