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

for i in $(seq 1 "$TIMEOUT"); do
	state="$(app '{.status.sync.revision} {.status.sync.status} {.status.health.status}')"
	if [ "$state" = "$sha Synced Healthy" ]; then
		echo "揃った: ${sha:0:7} Synced / Healthy (${i} 秒)"
		exit 0
	fi
	sleep 1
done

echo "NG: ${TIMEOUT} 秒待っても揃わない (いま: ${state:-なし})" >&2
$KUBECTL -n argocd get application bbs -o "jsonpath={range .status.conditions[*]}{.type}: {.message}{'\n'}{end}" >&2 || true
exit 1
