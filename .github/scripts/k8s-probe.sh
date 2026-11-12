#!/usr/bin/env bash
# kind 上の掲示板と、Argo CD による「Git どおりに揃える」動きを実測する (ADR 0026)。
#
#   make k8s-verify
#
# 確かめること:
#   1. エッジ経由で画面・API・DB が本番と同じ形で動いている
#   2. compose と同じ初期化 (マイグレーション全件・pg_trgm が日本語を索引できる) になっている
#   3. Argo CD が Git の指すコミットに揃えている (Synced / Healthy / リビジョン一致)
#   4. **クラスタを手で崩しても Git の状態に戻る** (selfHeal の効きを実際に見る)
#
# 3・4 は Application が居るときだけ。make k8s-apply-local (Argo CD なし) で入れた
# 環境では「Argo CD 不在」と表示して飛ばす —— 飛ばしたことは最後に必ず出す。
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl --context kind-bbs}"
BASE="${BASE:-http://localhost:8088}"
NS=bbs
MIGRATIONS_DIR="${MIGRATIONS_DIR:-apps/go-api/db/migrations}"
HEAL_TIMEOUT="${HEAL_TIMEOUT:-180}"

fail=0
skipped=()
pass() { printf '  \033[32mOK\033[0m    %s\n' "$1"; }
bad()  { printf '  \033[31mNG\033[0m    %s\n' "$1"; fail=$((fail + 1)); }
k()    { $KUBECTL "$@"; }
status() { curl -s --max-time 10 -o /dev/null -w '%{http_code}' "$@" || true; }
psql_q() { k -n "$NS" exec -i statefulset/postgres -- psql -U app -d bbs -tAc "$1" | tr -d '[:space:]'; }

# **起動途中を検査結果と取り違えない。** エッジは自分で /edge-healthz を返すので、
# go-api や next-app が上がる前でも 200 になる。3 つとも揃うまで待つ。
wait_edge() {
	local i
	for i in $(seq 1 120); do
		if [ "$(status "$BASE/edge-healthz")" = "200" ] &&
			[ "$(status "$BASE/api/readyz")" = "200" ] &&
			[ "$(status "$BASE/")" = "200" ]; then
			return 0
		fi
		sleep 2
	done
	echo "エッジ / go-api / next-app のいずれかが応答しません ($BASE)" >&2
	return 1
}

echo "  対象が起動するのを待っています..."
wait_edge

echo
echo "=== 1. エッジ経由で本番と同じ形に動くか ==="

[ "$(status "$BASE/")" = "200" ] && pass "/ が next-app に届く" || bad "/ が next-app に届かない"
[ "$(status "$BASE/api/healthz")" = "200" ] && pass "/api/healthz が go-api に届く (プレフィックスが剥がされる)" \
	|| bad "/api/healthz が届かない"
[ "$(status "$BASE/api/readyz")" = "200" ] && pass "/api/readyz が 200 (go-api から DB に繋がる)" \
	|| bad "/api/readyz が 200 でない"

post() {
	curl -s --max-time 10 -o /dev/null -w '%{http_code}' -X POST "$BASE/api/threads" \
		-H 'Content-Type: application/json' -d '{"title":"k8s-probe"}' "$@" || true
}
# **この 201 は X-Forwarded-Proto の効きを証明しない。** http の構成では
# XFP が無くても scheme は http のままで、しかも Origin は CORS の許可リストで
# 先に通る。XFP と TRUSTED_PROXIES の因果は compose 側の make https-verify が
# 測っている (ADR 0023)。ここで見ているのは「DB まで書ける」ことと CSRF 検査の有無。
code="$(post -H "Origin: $BASE")"
[ "$code" = "201" ] && pass "正しい Origin の書き込みが DB まで通る (${code})" \
	|| bad "正しい Origin の書き込みが通らない (got $code, want 201)"
code="$(post -H 'Origin: https://evil.test')"
[ "$code" = "403" ] && pass "他所の Origin の書き込みは 403 (CSRF 検査が効いている)" \
	|| bad "他所の Origin の書き込みが弾かれない (got $code)"

# /images/* は MinIO の bbs-images へ書き換えて届く。無いキーを引いて、
# next-app の 404 ではなく **MinIO がバケット名つきで** 答えることを見る。
body="$(curl -s --max-time 10 "$BASE/images/k8s-probe-missing.png" || true)"
case "$body" in
*"<BucketName>bbs-images</BucketName>"*) pass "/images/* が MinIO の bbs-images に届く (minio-init がバケットを作った)" ;;
*) bad "/images/* が MinIO の bbs-images に届かない" ;;
esac

echo
echo "=== 2. compose と同じ初期化になっているか ==="

want="$(find "$MIGRATIONS_DIR" -name '*.up.sql' | wc -l | tr -d ' ')"
got="$(psql_q 'SELECT version FROM schema_migrations')"
dirty="$(psql_q 'SELECT dirty FROM schema_migrations')"
[ "$got" = "$want" ] && [ "$dirty" = "f" ] \
	&& pass "マイグレーションが最新まで当たっている (version $got / dirty=$dirty)" \
	|| bad  "マイグレーションが最新でない (version $got, want $want / dirty=$dirty)"

n="$(psql_q "SELECT coalesce(array_length(show_trgm('検証'), 1), 0)")"
[ "${n:-0}" -ge 1 ] 2>/dev/null \
	&& pass "pg_trgm が日本語からトライグラムを作れる ($n 個。LC_CTYPE=C.UTF-8 が効いている)" \
	|| bad  "pg_trgm が日本語を索引できない (LC_CTYPE が C のまま。ADR 0012)"

if ! k -n argocd get application bbs >/dev/null 2>&1; then
	skipped+=("3・4 (Argo CD の Application が居ない。make k8s-apply-local で入れた環境)")
else
	echo
	echo "=== 3. Argo CD が Git の指すコミットに揃えているか ==="

	app_json() { k -n argocd get application bbs -o "jsonpath=$1"; }
	rev="$(app_json '{.spec.source.targetRevision}')"
	sync="$(app_json '{.status.sync.status}')"
	health="$(app_json '{.status.health.status}')"
	deployed="$(app_json '{.status.sync.revision}')"
	remote="$(git ls-remote "$(app_json '{.spec.source.repoURL}')" "refs/heads/$rev" | cut -f1)"

	[ "$sync" = "Synced" ] && pass "Synced (Git とクラスタに差が無い)" || bad "Synced でない (got $sync)"
	[ "$health" = "Healthy" ] && pass "Healthy" || bad "Healthy でない (got $health)"
	# **見ているのが本当に今の Git か。** 古いコミットのまま Synced でも上の 2 つは通る。
	[ -n "$remote" ] && [ "$deployed" = "$remote" ] \
		&& pass "同期したコミットが $rev の先頭と一致する (${deployed:0:7})" \
		|| bad  "同期したコミット (${deployed:0:7}) が $rev の先頭 (${remote:0:7}) と違う"

	echo
	echo "=== 4. 手で崩しても Git の状態に戻るか (selfHeal) ==="

	# 戻ってくるまで待つ。戻らなければ NG (タイムアウトは HEAL_TIMEOUT 秒)。
	wait_for() {
		local what="$1" cond="$2" i
		for i in $(seq 1 "$HEAL_TIMEOUT"); do
			if eval "$cond" >/dev/null 2>&1; then
				pass "$what (${i} 秒で戻った)"
				return 0
			fi
			sleep 1
		done
		bad "$what (${HEAL_TIMEOUT} 秒待っても戻らない)"
	}

	# (a) 設定を書き換える: レプリカ数を Git (1) から 0 にする
	k -n "$NS" scale deployment/go-api --replicas=0 >/dev/null
	wait_for "go-api を 0 台に縮めても 1 台に戻る" \
		"[ \"\$(k -n $NS get deployment go-api -o jsonpath='{.spec.replicas}')\" = 1 ]"

	# (b) リソースを消す: Service を丸ごと消す
	k -n "$NS" delete service mailpit --wait=true >/dev/null
	wait_for "消した Service (mailpit) が作り直される" \
		"k -n $NS get service mailpit"

	# 戻った結果、利用者から見ても元どおりか
	wait_edge >/dev/null 2>&1 && pass "崩したあとも画面と API が応答する" || bad "崩したあと応答が戻らない"
fi

echo
if [ "${#skipped[@]}" -gt 0 ]; then
	for s in "${skipped[@]}"; do printf '  \033[33mSKIP\033[0m  %s\n' "$s"; done
fi
if [ "$fail" -gt 0 ]; then
	echo "NG が $fail 件あります"
	exit 1
fi
echo "すべて通りました"
