#!/usr/bin/env bash
#
# comments テーブルの HASH パーティションで partition pruning が
# 実際に効いていることを検証する。
#
# ローカルでも実行できる:
#   make up
#   PGHOST=localhost bash .github/scripts/verify-partition-pruning.sh
#
set -euo pipefail

PGHOST="${PGHOST:-localhost}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-app}"
PGDATABASE="${PGDATABASE:-bbs}"
export PGPASSWORD="${PGPASSWORD:-password}"

# 実際に作成したパーティション数 (db/migrations と揃える)
readonly EXPECTED_PARTITIONS=8

psql_query() {
  psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" \
    -v ON_ERROR_STOP=1 -X -c "$1"
}

# 実行計画から「走査対象になったパーティションの実数」を数える。
#
# 単純に grep -c 'comments_p' で行数を数えてはいけない。
# 実行計画には次の 2 行が現れ、どちらも 'comments_p' を含むため、
# 1 パーティションしか読んでいなくても 2 と数えてしまう。
#
#    Bitmap Heap Scan on comments_p0 comments
#      ->  Bitmap Index Scan on comments_p0_pkey
#
# パーティション名そのものだけを抜き出して重複を除く。
# comments_p0_pkey からも comments_p0 が取れるため、
# ヒープ走査とインデックス走査が同じパーティションを指していれば 1 と数えられる。
count_scanned_partitions() {
  # grep は一致が無いと終了コード 1 を返す。set -o pipefail の下では
  # それがそのまま伝播し、set -e でスクリプトが即死してしまう。
  # そうなると下の ::error:: 診断が出力されないまま CI が落ち、
  # 「原因不明の非ゼロ終了」になる。0 件も正常な計測結果として扱う。
  { grep -oE 'comments_p[0-9]+' || true; } | sort -u | wc -l | tr -d ' '
}

echo "=============================================================="
echo "1. thread_id を等値指定した場合 (pruning が効くべき)"
echo "=============================================================="
plan_pruned="$(psql_query "EXPLAIN (COSTS OFF) SELECT * FROM comments WHERE thread_id = 1;")"
echo "$plan_pruned"

scanned="$(printf '%s' "$plan_pruned" | count_scanned_partitions)"
echo ""
echo "走査対象パーティション数: ${scanned} / ${EXPECTED_PARTITIONS}"

if [ "$scanned" -ne 1 ]; then
  echo "::error::partition pruning が効いていません (走査数=${scanned}, 期待=1)"
  exit 1
fi
echo "OK: 1 パーティションのみを走査している"

echo ""
echo "=============================================================="
echo "2. 全件走査の場合 (全パーティションに触れるべき)"
echo "=============================================================="
# 対照実験。これが 1 のままなら、上の検査は「常に 1 を返すだけの
# 意味のないアサーション」であり、pruning を検証できていないことになる。
plan_full="$(psql_query "EXPLAIN (COSTS OFF) SELECT count(*) FROM comments;")"
echo "$plan_full"

scanned_full="$(printf '%s' "$plan_full" | count_scanned_partitions)"
echo ""
echo "走査対象パーティション数: ${scanned_full} / ${EXPECTED_PARTITIONS}"

if [ "$scanned_full" -ne "$EXPECTED_PARTITIONS" ]; then
  echo "::error::全件走査で全パーティションに触れていません" \
       "(走査数=${scanned_full}, 期待=${EXPECTED_PARTITIONS})。" \
       "検査の前提が崩れているため、1 の結果も信用できません"
  exit 1
fi
echo "OK: ${EXPECTED_PARTITIONS} パーティションすべてを走査している"

echo ""
echo "=============================================================="
echo "3. パーティション構成の確認"
echo "=============================================================="
psql_query "
  SELECT
      c.relname AS partition,
      pg_get_expr(c.relpartbound, c.oid) AS bound
  FROM pg_class parent
  JOIN pg_inherits i ON i.inhparent = parent.oid
  JOIN pg_class c ON c.oid = i.inhrelid
  WHERE parent.relname = 'comments'
  ORDER BY c.relname;
"

actual="$(psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -X -tAc "
  SELECT count(*)
  FROM pg_inherits i
  JOIN pg_class parent ON parent.oid = i.inhparent
  WHERE parent.relname = 'comments';
")"

if [ "$actual" -ne "$EXPECTED_PARTITIONS" ]; then
  echo "::error::パーティション数が想定と違います (実際=${actual}, 期待=${EXPECTED_PARTITIONS})"
  exit 1
fi

echo ""
echo "すべての検証に成功しました。"
