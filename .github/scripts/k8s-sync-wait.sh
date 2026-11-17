#!/usr/bin/env bash
# Argo CD の Application bbs が「いま見ている枝の先頭」に揃うまで待つ (make k8s-app)。
#
# **Synced / Healthy だけを待たない。** 枝を切り替えた直後は、Argo CD が
# 新しい枝をまだ見ていないので、状態は前の枝のまま Synced / Healthy でいる。
# そこで待つと即座に通り、クラスタが前の枝のままなのに「揃った」と報告する
# (レビュー指摘)。同期したコミットが枝の先頭と一致するところまで見る。
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl --context kind-bbs}"
TIMEOUT="${TIMEOUT:-600}"

app() { $KUBECTL -n argocd get application bbs -o "jsonpath=$1"; }

repo="$(app '{.spec.source.repoURL}')"
rev="$(app '{.spec.source.targetRevision}')"
path="$(app '{.spec.source.path}')"

# 枝 (またはタグ) の先頭のコミットを引く。コミット SHA を直接指していればそのまま。
if [[ "$rev" =~ ^[0-9a-f]{40}$ ]]; then
	sha="$rev"
else
	sha="$(git ls-remote "$repo" "refs/heads/$rev" "refs/tags/$rev^{}" "refs/tags/$rev" | head -1 | cut -f1)"
fi
if [ -z "$sha" ]; then
	echo "NG: $repo に $rev が無い (push したか?)" >&2
	exit 1
fi

# **その枝にマニフェストがあるかを先に確かめる。** 無いと Argo CD は
# ComparisonError のまま、こちらはタイムアウトまで黙って待つことになる。
git fetch -q "$repo" "$sha" 2>/dev/null || git fetch -q "$repo" "$rev"
if ! git cat-file -e "$sha:$path/kustomization.yaml" 2>/dev/null; then
	echo "NG: $rev (${sha:0:7}) に $path が無い。まだこの枝に入っていない" >&2
	exit 1
fi

echo "$rev (${sha:0:7}) に揃うのを待っています (最大 ${TIMEOUT} 秒)"
# 定期の確認 (60 秒間隔) を待たずに、すぐ見に行かせる。
$KUBECTL -n argocd annotate application bbs argocd.argoproj.io/refresh=normal --overwrite >/dev/null

# **同期の操作が終わっていることも待つ。** Argo CD は普通のリソースを当てた時点で
# Synced と表示し、Sync フック (migrate / minio-init) の完了を待たない。
# 0.5 秒刻みで測ると、push から 5 秒で「新しいコミット・Synced・Healthy」に
# なったが、操作は 14 秒まで Running だった (フックを流している最中)。
# 3 つだけを見る初版は、その間に「揃った」と言っていた。
#
# 操作が無いとき (マニフェストが変わらないコミットでは同期が走らない) は
# phase が前回のもののまま残るので、Running でなければ良しとする。
for i in $(seq 1 "$TIMEOUT"); do
	state="$(app '{.status.sync.revision} {.status.sync.status} {.status.health.status}')"
	phase="$(app '{.status.operationState.phase}')"
	op_rev="$(app '{.status.operationState.syncResult.revision}')"
	case "$phase" in
	Failed | Error)
		if [ "$op_rev" = "$sha" ]; then
			echo "NG: ${sha:0:7} の同期が $phase で終わった" >&2
			app '{.status.operationState.message}' >&2
			echo >&2
			exit 1
		fi
		;;
	esac
	if [ "$state" = "$sha Synced Healthy" ] && [ "$phase" != Running ] && [ "$phase" != Terminating ]; then
		echo "揃った: ${sha:0:7} Synced / Healthy、同期の操作は ${phase:-なし} (${i} 秒)"
		exit 0
	fi
	sleep 1
done

echo "NG: ${TIMEOUT} 秒待っても揃わない (いま: ${state:-なし})" >&2
$KUBECTL -n argocd get application bbs -o "jsonpath={range .status.conditions[*]}{.type}: {.message}{'\n'}{end}" >&2 || true
exit 1
