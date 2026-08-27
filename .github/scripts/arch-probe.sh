#!/usr/bin/env bash
#
# 境界プローブ —— アーキテクチャ検査が「落とすべきものを落とし、
# 通すべきものを通す」ことを、意図的な違反コードを置いて実測する。
#
# 設定を読んだだけでは正しさを判断できない。ADR 0004 では、緩和ルールの
# deny に相手モジュールを入れ忘れた欠陥が、プローブを置いて初めて見つかった。
# その手作業を CI に残したものがこのスクリプト。
#
# 検査対象は 2 つの arch ファイル:
#   .go-arch-lint.yml        本体コード用 (テストファイルは対象外)
#   .go-arch-lint.tests.yml  テストの緩和を含む版 (全ファイルが対象)
# 本体コードには両方が適用されるため、実質「テストにだけ緩和が効く」形になる。
#
# 経緯は docs/adr/0017-arch-lint-and-depguard.md
#
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
API="$ROOT/apps/go-api"
PKG="develop-experiments/apps/go-api"

cd "$API" || exit 1

if ! command -v go-arch-lint >/dev/null 2>&1; then
  echo "go-arch-lint が見つかりません。'make tools' を実行してください。" >&2
  exit 127
fi

MODEL="$PKG/internal/thread/domain/model"
CMODEL="$PKG/internal/comment/domain/model"
PG="$PKG/internal/infrastructure/postgres"
TUC="$PKG/internal/thread/usecase"
PGX="github.com/jackc/pgx/v5/pgtype"
GIN="github.com/gin-gonic/gin"
# O / P の「宣言していないライブラリ」。
# 以前は uuid を使っていたが、投稿者の公開 ID を運ぶために
# thread-usecase が uuid を宣言した時点でプローブが無効化された
# (期待 fail のはずが pass になり、arch-probe が検出した)。
# 宣言済みのベンダを使うと同じことが起きるため、
# **usecase 層が絶対に触らないもの**を選ぶ。oauth2 は認証アダプタ専用。
OAUTH2="golang.org/x/oauth2"

# id | 置くファイル | package 名 | import する先 | 期待 | 説明
#
# 期待 fail = どちらかの arch ファイルが落とすべき
# 期待 pass = 両方が通すべき (緩和が効いていることの確認)
PROBES=(
"A|internal/comment/usecase/zz_probe.go|usecase|$MODEL|fail|本体 comment/usecase -> thread/domain/model (モジュール境界)"
"B|internal/thread/domain/model/zz_probe.go|model|$PGX|fail|本体 thread/domain/model -> pgx (DB ドライバの流出)"
"C|internal/thread/usecase/zz_probe.go|usecase|$PG|fail|本体 thread/usecase -> infrastructure (レイヤ)"
"D|internal/thread/usecase/zz_probe_test.go|usecase|$PG|pass|テスト thread/usecase -> infrastructure (統合テストの緩和)"
"E|internal/comment/usecase/zz_probe_test.go|usecase|$MODEL|fail|テスト comment/usecase -> thread/domain/model (境界はテストでも閉じる)"
"F|internal/httpapi/zz_probe.go|httpapi|$MODEL|fail|本体 httpapi -> thread/domain/model (レイヤ)"
"G|internal/httpapi/zz_probe_test.go|httpapi|$PG|fail|テスト httpapi -> infrastructure (テストでも永続化層は不可)"
"H|internal/comment/usecase/zz_probe.go|usecase|$TUC|fail|本体 comment/usecase -> thread/usecase (モジュール境界)"
"I|internal/httpapi/zz_probe_test.go|httpapi|$MODEL|pass|テスト httpapi -> thread/domain/model (フェイク用の緩和)"
"J|internal/thread/domain/zzprobe/zz_probe.go|zzprobe|$TUC|fail|本体 domain -> usecase (レイヤ方向)"
"K|internal/thread/usecase/zz_probe.go|usecase|$GIN|fail|本体 thread/usecase -> gin (フレームワークの流出)"
"L|internal/thread/usecase/zz_probe_test.go|usecase|$GIN|fail|テスト thread/usecase -> gin (テストでも不可)"
"M|internal/user/usecase/zz_probe.go|usecase|$CMODEL|fail|設定を更新せずに足した新モジュール -> 他モジュール"
"N|internal/user/usecase/zz_probe.go|usecase|$PG|fail|設定を更新せずに足した新モジュール -> infrastructure"
"O|internal/thread/usecase/zz_probe.go|usecase|$OAUTH2|fail|宣言していないライブラリを本体で使う"
"P|internal/thread/usecase/zz_probe_test.go|usecase|$OAUTH2|fail|宣言していないライブラリをテストで使う"
)

CREATED=""
cleanup() {
  for f in $CREATED; do
    rm -f "$API/$f"
    rmdir -p "$(dirname "$API/$f")" 2>/dev/null
  done
  CREATED=""
}
trap cleanup EXIT INT TERM

# 出力の grep ではなく終了コードで判定する。出力の文言で判定すると、
# 想定していない種類の通知 (未宣言パッケージなど) を取りこぼす。
check_main()  { go-arch-lint check >/dev/null 2>&1; }
check_tests() { go-arch-lint check --arch-file .go-arch-lint.tests.yml >/dev/null 2>&1; }

# 前提: プローブを置く前は両方通っていること。
# ここが汚れていると、以降の結果はすべて意味を失う。
if ! check_main || ! check_tests; then
  echo "プローブを置く前の時点で arch 検査が落ちています。先にそちらを直してください。" >&2
  go-arch-lint check --output-color=false >&2
  go-arch-lint check --arch-file .go-arch-lint.tests.yml --output-color=false >&2
  exit 1
fi

# 見出しと行で桁を揃える。日本語は printf の幅では 1 文字に数えられるが
# 表示は 2 桁ぶんになるので、**同じ書式指定を使わないと列がずれる。**
printf "%-4s %-10s %-8s %-10s %-10s  %s\n" ID 期待 結果 本体用 テスト用 内容
printf -- "%s\n" "----------------------------------------------------------------------------------"

FAILED=0
for p in "${PROBES[@]}"; do
  IFS='|' read -r id path pkg imp expect desc <<<"$p"

  mkdir -p "$(dirname "$path")"
  printf 'package %s\n\nimport _ "%s"\n' "$pkg" "$imp" > "$path"
  # **追記する。** 代入だと、1 回のプローブで 2 つ以上のファイルを
  # 置いた瞬間に、後始末が最後の 1 つしか消さなくなる。
  # 消し残したファイルは次のプローブの前提を壊す (arch 検査は
  # ディレクトリ全体を見るため)。
  CREATED="$CREATED $path"

  check_main  && m="通る" || m="落ちる"
  check_tests && t="通る" || t="落ちる"

  cleanup

  # どちらか一方でも落とせば「落ちる」
  if [ "$m" = "落ちる" ] || [ "$t" = "落ちる" ]; then actual="落ちる"; else actual="通る"; fi
  [ "$expect" = "fail" ] && want="落ちる" || want="通る"

  if [ "$actual" = "$want" ]; then mark="OK"; else mark="NG"; FAILED=1; fi

  printf "%-4s %-10s %-8s %-10s %-10s  %s\n" "$id" "$want" "$mark" "$m" "$t" "$desc"
done

echo
if [ "$FAILED" -ne 0 ]; then
  echo "境界プローブが期待どおりに動いていません。arch ファイルを確認してください。" >&2
  exit 1
fi
echo "境界プローブ ${#PROBES[@]} 件すべてが期待どおりでした。"
