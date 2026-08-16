#!/usr/bin/env python3
"""一覧・検索・ページ送りのクエリを実測する (Phase 4)。

**約束していた測り直しを片付けるためのもの。** 3 か所で「Phase 4 で測る」と
書いたまま残っていた:

    db/query/threads.sql   コメント数の集計。**投稿者の LEFT JOIN を
                           足す前の数字しか無い** (それ以降測っていない)
    ADR 0012               検索クエリのレスポンス分布。
                           最低文字数を設けるかの判断材料
    ADR 0012 / PR #33      深いページでカーソルの OR を COALESCE に
                           置き換えるかどうか (レビュー指摘 5)

`EXPLAIN (ANALYZE, BUFFERS)` を各 3 回流して**中央値**を取る。
1 回だけだと、キャッシュに乗る前の 1 回目が混ざって数字が跳ねる。

使い方:
    make bench-dataset      2,000 スレッド / 20 万コメントを投入する
    make query-probe        測る

**CI には載せない。** 数字が環境の性能に左右されるため
(concurrency-probe / viewcount-probe と同じ)。
"""

from __future__ import annotations

import os
import re
import shlex
import statistics
import subprocess
import sys

SQL_EXEC = os.environ.get(
    "SQL_EXEC", "docker compose exec -T postgres psql -U app -d bbs -X -q")
RUNS = int(os.environ.get("PROBE_RUNS", "3"))

# **並列実行を止める。** ワーカー数が実行ごとに変わると、
# 同じクエリでも数字がばらつき、実装の差なのか並列度の差なのか
# 区別できなくなる (ADR 0012 の実測と同じ条件に揃える)。
PRELUDE = "SET max_parallel_workers_per_gather = 0;"

EXEC_TIME = re.compile(r"Execution Time: ([\d.]+) ms")
BUFFERS = re.compile(r"Buffers: shared hit=(\d+)(?: read=(\d+))?")
# **threads に対する走査だけを拾う。** 計画木の先頭を取ると、
# 200 行の users への Seq Scan (それ自体は正しい選択) が出てしまい、
# 「一覧クエリが全表走査している」と読めてしまう。
SCAN_KIND = re.compile(
    r"(Seq Scan|Index Scan(?: Backward)?|Index Only Scan|Bitmap Heap Scan)"
    r"(?: using (\S+))? on threads")


def explain(sql: str, force_index: bool = False) -> tuple[float, int, str]:
    """EXPLAIN (ANALYZE, BUFFERS) を流し、(実行 ms, 読んだバッファ, 走査の種類) を返す。

    force_index=True は `enable_seqscan = off` を足す。
    **planner の選択が最善かどうか**を見るために使う ——
    「索引を使わなかった」と「索引を使っても遅かった」は別の話で、
    前者なら統計かコスト見積もりの問題になる。
    """
    prelude = PRELUDE + (" SET enable_seqscan = off;" if force_index else "")
    statement = f"{prelude} EXPLAIN (ANALYZE, BUFFERS) {sql}"
    out = subprocess.run(shlex.split(SQL_EXEC) + ["-c", statement],
                         check=True, capture_output=True, text=True)
    text = out.stdout

    m = EXEC_TIME.search(text)
    ms = float(m.group(1)) if m else 0.0

    buffers = 0
    for hit, read in BUFFERS.findall(text):
        buffers += int(hit) + int(read or 0)

    # 索引を使ったかどうかが要点。索引名まで出す ——
    # 「索引を使った」だけでは、**意図した索引かどうか**が分からない。
    matches = SCAN_KIND.findall(text)
    if matches:
        scan, index = matches[0]
        kind = f"{scan} ({index})" if index else scan
    else:
        kind = "?"
    return ms, buffers, kind


def measure(label: str, sql: str, note: str = "", force_index: bool = False) -> dict:
    samples = [explain(sql, force_index) for _ in range(RUNS)]
    times = [s[0] for s in samples]
    return {
        "label": label,
        "ms": statistics.median(times),
        "min": min(times),
        "max": max(times),
        "buffers": statistics.median([s[1] for s in samples]),
        "kind": samples[-1][2],
        "note": note,
    }


def sql_value(statement: str) -> str:
    out = subprocess.run(shlex.split(SQL_EXEC) + ["-t", "-A", "-c", statement],
                         check=True, capture_output=True, text=True)
    return out.stdout.strip()


# ---------------------------------------------------------------------------
# 1. コメント数の集計 (db/query/threads.sql の測り直し)
# ---------------------------------------------------------------------------
# **前回の数字は投稿者の LEFT JOIN が入る前のもの。**
# 「約 5.5 倍速い」がいまも成り立つかを確かめる。

LIST_CORRELATED = """
WITH page AS (
    SELECT id, title, created_at, view_count, author_id, icon_image_id
    FROM threads
    WHERE deleted_at IS NULL
    ORDER BY id DESC
    LIMIT 20
)
SELECT p.id, p.title, p.created_at, p.view_count,
    (SELECT count(*) FROM comments c
      WHERE c.thread_id = p.id AND c.deleted_at IS NULL)::bigint AS comment_count,
    u.public_id, u.display_name, u.avatar_url, u.deleted_at
FROM page p
LEFT JOIN users u ON u.id = p.author_id
ORDER BY p.id DESC;
"""

LIST_GROUP_BY = """
SELECT t.id, t.title, t.created_at, t.view_count,
    count(c.id) FILTER (WHERE c.deleted_at IS NULL) AS comment_count,
    u.public_id, u.display_name, u.avatar_url, u.deleted_at
FROM threads t
LEFT JOIN comments c ON c.thread_id = t.id
LEFT JOIN users u ON u.id = t.author_id
WHERE t.deleted_at IS NULL
GROUP BY t.id, u.public_id, u.display_name, u.avatar_url, u.deleted_at
ORDER BY t.id DESC
LIMIT 20;
"""

# ---------------------------------------------------------------------------
# 2. 検索 (ADR 0012 の宿題)
# ---------------------------------------------------------------------------
# 検索語の珍しさで、GIN と id 順走査のどちらが有利かが変わる。

def search_sql(keyword: str) -> str:
    return f"""
WITH page AS (
    SELECT id, title, created_at, view_count, author_id, icon_image_id
    FROM threads
    WHERE deleted_at IS NULL
      AND title ILIKE '%{keyword}%' ESCAPE '\\'
    ORDER BY id DESC
    LIMIT 20
)
SELECT p.id, p.title,
    (SELECT count(*) FROM comments c
      WHERE c.thread_id = p.id AND c.deleted_at IS NULL)::bigint AS comment_count
FROM page p ORDER BY p.id DESC;
"""


# ---------------------------------------------------------------------------
# 3. 深いページ (レビュー指摘 5)
# ---------------------------------------------------------------------------
# カーソルの `IS NULL OR` を COALESCE に置き換えるべきかを測る。
#
# **検索語の OR とは失うものが違う。** あちらは畳めないと GIN を
# 一切使えないが、カーソルの OR は畳めなくても索引は使える ——
# 開始位置が「カーソルの行」から「先頭」に落ちるだけになる。

def deep_page_or(cursor_id: int) -> str:
    return f"""
SELECT id, title FROM threads
WHERE deleted_at IS NULL
  AND (NULLIF({cursor_id}, 0)::bigint IS NULL OR id < NULLIF({cursor_id}, 0)::bigint)
ORDER BY id DESC LIMIT 20;
"""


def deep_page_coalesce(cursor_id: int) -> str:
    return f"""
SELECT id, title FROM threads
WHERE deleted_at IS NULL
  AND id < COALESCE(NULLIF({cursor_id}, 0)::bigint, 9223372036854775807)
ORDER BY id DESC LIMIT 20;
"""


POPULAR_FIRST = """
SELECT id, title, view_count FROM threads
WHERE deleted_at IS NULL
ORDER BY view_count DESC, id DESC LIMIT 20;
"""


def popular_deep(view_count: int, thread_id: int) -> str:
    return f"""
SELECT id, title, view_count FROM threads
WHERE deleted_at IS NULL
  AND (view_count, id) < ({view_count}, {thread_id})
ORDER BY view_count DESC, id DESC LIMIT 20;
"""


def report(title: str, rows: list[dict]) -> None:
    print(f"\n{title}")
    print(f"{'クエリ':<34}{'中央値ms':>10}{'最小':>8}{'最大':>8}{'buffers':>10}  走査")
    print("-" * 86)
    for r in rows:
        print(f"{r['label']:<34}{r['ms']:>10.2f}{r['min']:>8.2f}{r['max']:>8.2f}"
              f"{r['buffers']:>10.0f}  {r['kind']}")
        if r["note"]:
            print(f"    {r['note']}")


def main() -> int:
    threads = sql_value("SELECT count(*) FROM threads;")
    comments = sql_value("SELECT count(*) FROM comments;")
    print(f"データセット: {threads} スレッド / {comments} コメント "
          f"(各クエリ {RUNS} 回の中央値)")

    if int(threads) < 1000:
        print("\n**データが少なすぎます。** make bench-dataset を先に流してください。",
              file=sys.stderr)
        return 1

    report("1. コメント数の集計 (投稿者の LEFT JOIN を含む)", [
        measure("相関サブクエリ (採用)", LIST_CORRELATED),
        measure("LEFT JOIN + GROUP BY", LIST_GROUP_BY),
    ])

    report("2. 検索: 語の珍しさで変わるか (ADR 0012)", [
        measure("珍しい語 (ゼロ幅接合子)", search_sql("ゼロ幅接合子"),
                "1980 件中 10 件に一致"),
        # **planner が選ばなかった計画を測る。** 「索引を使わなかった」と
        # 「索引を使っても遅かった」は別の話で、前者なら
        # 統計かコスト見積もりの問題になる (ADR 0012 の 3 番と同じ構造)。
        measure("珍しい語 (GIN を強制)", search_sql("ゼロ幅接合子"),
                "enable_seqscan = off", force_index=True),
        measure("中くらいの語 (PostgreSQL)", search_sql("PostgreSQL"),
                "1980 件中 480 件に一致"),
        measure("ありふれた語 (スレッド)", search_sql("スレッド"),
                "全件 (1980) に一致。GIN が候補を絞れない"),
        measure("2 文字 (雑談)", search_sql("雑談"),
                "1697 件に一致。トライグラムが 3 文字単位で選択性が落ちる"),
    ])

    # 深いページ。カーソルは末尾付近に置く。
    deep_cursor = int(sql_value(
        "SELECT id FROM threads WHERE deleted_at IS NULL ORDER BY id LIMIT 1 OFFSET 20;"))
    report(f"3. 深いページ: カーソルの OR (境界 id={deep_cursor})", [
        measure("IS NULL OR (現行)", deep_page_or(deep_cursor)),
        measure("COALESCE (番兵)", deep_page_coalesce(deep_cursor)),
        measure("先頭ページ (OR / cursor なし)", deep_page_or(0)),
        measure("先頭ページ (COALESCE)", deep_page_coalesce(0)),
    ])

    row = sql_value(
        "SELECT view_count || ' ' || id FROM threads WHERE deleted_at IS NULL "
        "ORDER BY view_count DESC, id DESC LIMIT 1 OFFSET 40;")
    vc, tid = (int(v) for v in row.split())
    report(f"4. 人気順の索引 (境界 view_count={vc}, id={tid})", [
        measure("人気順 先頭ページ", POPULAR_FIRST),
        measure("人気順 深いページ (行値比較)", popular_deep(vc, tid)),
    ])

    print("\n※ Seq Scan が出ていても即座に誤りではありません ——")
    print("   一致件数が多いほど、索引を辿って捨てるより全表走査が安くなります。")
    print("   問題になるのは「珍しい語で Seq Scan」のほうです。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
