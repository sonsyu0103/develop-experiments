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
# **最初の 1 つだけを採る。** 計画木の Buffers は親が子の合計を持つので、
# 全行を足すと入れ子の深さぶん重複して数える。根 = 最初に現れる行が全体の合計になる。
#
# **この数え方は途中で直している。** 以前は全行を足していたため、
# 節が深いクエリほど大きく出ていた (パーティションを跨ぐクエリで 3 倍近く膨らんだ)。
# db/query/threads.sql に残した実測値は、直したあとの数字に更新済み。
BUFFERS = re.compile(r"Buffers: shared hit=(\d+)(?: read=(\d+))?")

# **走査を見る対象の表を指定できるようにする。** 計画木の先頭を取ると、
# 200 行の users への Seq Scan (それ自体は正しい選択) が出てしまい、
# 「一覧クエリが全表走査している」と読めてしまう。
#
# comments はパーティション名 (comments_p3 など) で出るので、前方一致で拾う。
SCAN_PREFIX = (r"(Seq Scan|Index Scan(?: Backward)?|Index Only Scan|Bitmap Heap Scan)"
               r"(?: using (\S+))? on ")


def scan_kind_re(table: str) -> re.Pattern:
    return re.compile(SCAN_PREFIX + table + r"\w*\b")


# 実際に触ったパーティションの数。**「8 つ全部走る」を数字で示す**ために使う。
SCANNED_PART = re.compile(r"on (comments_p\d+)\b")


def explain(sql: str, force_index: bool = False, table: str = "threads") -> tuple:
    """EXPLAIN (ANALYZE, BUFFERS) を流し、(実行 ms, バッファ, 走査の種類, 区画数) を返す。

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

    b = BUFFERS.search(text)
    buffers = 0 if b is None else int(b.group(1)) + int(b.group(2) or 0)

    # 索引を使ったかどうかが要点。索引名まで出す ——
    # 「索引を使った」だけでは、**意図した索引かどうか**が分からない。
    matches = scan_kind_re(table).findall(text)
    if matches:
        scan, index = matches[0]
        kind = f"{scan} ({index})" if index else scan
    else:
        kind = "?"

    parts = len(set(SCANNED_PART.findall(text)))
    return ms, buffers, kind, parts


def measure(label: str, sql: str, note: str = "", force_index: bool = False,
            table: str = "threads") -> dict:
    samples = [explain(sql, force_index, table) for _ in range(RUNS)]
    times = [s[0] for s in samples]
    return {
        "label": label,
        "ms": statistics.median(times),
        "min": min(times),
        "max": max(times),
        "buffers": statistics.median([s[1] for s in samples]),
        "kind": samples[-1][2],
        "parts": max(s[3] for s in samples),
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


# ---------------------------------------------------------------------------
# 5. 投稿者で絞る (マイページ「自分の投稿」)
# ---------------------------------------------------------------------------
# **画面が「実際に測ってから決めます」と書いたまま残っていた項目。**
# ADR 0016 も「Phase 4 の測定対象に加える」で止まっていた。
#
# 索引は 000002 で既に入っている (threads_author_id_desc_idx /
# comments_author_id_desc_idx)。**測るのは「足すかどうか」ではなく
# 「足した索引で足りているか」**になる。
#
# comments は HASH(thread_id) の 8 分割で、author_id にキーが含まれない。
# 除外が効かないので 8 区画すべてに索引スキャンが走る。
# ADR 0016 はこれを「スレッド内のコメント一覧とはコストが桁で違う」と書いた。
# その 2 つを並べて測る。


def my_threads(author_id: int, cursor_id: int | None = None) -> str:
    cursor = "" if cursor_id is None else f"  AND id < {cursor_id}\n"
    return f"""
SELECT id, title, created_at, view_count
FROM threads
WHERE author_id = {author_id}
  AND deleted_at IS NULL
{cursor}ORDER BY id DESC LIMIT 20;
"""


def my_comments(author_id: int, cursor_id: int | None = None) -> str:
    """自分のコメント一覧。**8 区画すべてを走る側。**

    一覧にはスレッドのタイトルが要る (どのスレッドへの投稿か分からないと
    画面として成立しない) ので、threads との JOIN を含めた形で測る。
    """
    cursor = "" if cursor_id is None else f"      AND c.id < {cursor_id}\n"
    return f"""
WITH page AS (
    SELECT c.id, c.thread_id, c.seq, c.body, c.created_at
    FROM comments c
    WHERE c.author_id = {author_id}
      AND c.deleted_at IS NULL
{cursor}    ORDER BY c.id DESC
    LIMIT 20
)
SELECT p.id, p.thread_id, p.seq, p.body, p.created_at,
       t.title, t.deleted_at IS NOT NULL AS thread_deleted
FROM page p
JOIN threads t ON t.id = p.thread_id
ORDER BY p.id DESC;
"""


def thread_comments(thread_id: int) -> str:
    """比較対象: スレッド内のコメント一覧 (除外が効く側)。"""
    return f"""
SELECT id, seq, body, created_at
FROM comments
WHERE thread_id = {thread_id}
  AND deleted_at IS NULL
ORDER BY id DESC LIMIT 20;
"""


def report(title: str, rows: list[dict], show_parts: bool = False) -> None:
    print(f"\n{title}")
    head = f"{'クエリ':<34}{'中央値ms':>10}{'最小':>8}{'最大':>8}{'buffers':>10}"
    if show_parts:
        head += f"{'区画':>6}"
    print(head + "  走査")
    print("-" * (92 if show_parts else 86))
    for r in rows:
        line = (f"{r['label']:<34}{r['ms']:>10.2f}{r['min']:>8.2f}{r['max']:>8.2f}"
                f"{r['buffers']:>10.0f}")
        if show_parts:
            line += f"{r['parts'] or '-':>6}"
        print(line + f"  {r['kind']}")
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

    # -----------------------------------------------------------------------
    # 5. 投稿者で絞る
    # -----------------------------------------------------------------------
    # **投稿数が多い利用者を選ぶ。** 平均的な利用者を引くと、
    # 走査する行が少なすぎて「8 区画を走る」コストが埋もれる。
    author = int(sql_value(
        "SELECT author_id FROM comments WHERE author_id IS NOT NULL "
        "GROUP BY author_id ORDER BY count(*) DESC LIMIT 1;"))
    n_threads = sql_value(
        f"SELECT count(*) FROM threads WHERE author_id = {author} AND deleted_at IS NULL;")
    n_comments = sql_value(
        f"SELECT count(*) FROM comments WHERE author_id = {author} AND deleted_at IS NULL;")
    # 深いページ用の境界。末尾から 20 件目に置く。
    deep_comment = int(sql_value(
        f"SELECT id FROM comments WHERE author_id = {author} AND deleted_at IS NULL "
        f"ORDER BY id LIMIT 1 OFFSET 20;"))
    busy_thread = int(sql_value(
        "SELECT thread_id FROM comments WHERE deleted_at IS NULL "
        "GROUP BY thread_id ORDER BY count(*) DESC LIMIT 1;"))

    report(f"5. 投稿者で絞る (利用者 {author}: スレッド {n_threads} 件 / "
           f"コメント {n_comments} 件)", [
        measure("自分のスレッド 先頭ページ", my_threads(author),
                "threads は分割していないので 1 表を索引で辿るだけ"),
        measure("自分のコメント 先頭ページ", my_comments(author),
                "**8 区画すべてに索引スキャンが走る**", table="comments"),
        measure("自分のコメント 深いページ", my_comments(author, deep_comment),
                "カーソルつき。区画ごとに開始位置を探す", table="comments"),
        measure("[比較] スレッド内のコメント一覧", thread_comments(busy_thread),
                "除外が効く側。ADR 0016 が「桁で違う」と書いた相手",
                table="comments"),
    ], show_parts=True)

    print("\n※ Seq Scan が出ていても即座に誤りではありません ——")
    print("   一致件数が多いほど、索引を辿って捨てるより全表走査が安くなります。")
    print("   問題になるのは「珍しい語で Seq Scan」のほうです。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
