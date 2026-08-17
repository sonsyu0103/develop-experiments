#!/usr/bin/env python3
"""パーティションの損益分岐点を実測する (Phase 4)。

**README の Phase 4 で「未測定」として残っていた 1 件。**
理由は「20 万行では *まだ効かない側* にいることしか分からない。
数千万行のデータセットが要る」だった。ここでその規模を作る。

`comments` は HASH(thread_id) の 8 分割になっている
(db/migrations/000001_init_schema.up.sql)。マイグレーションのコメントは
**「現在のデータ量ではまったく不要」と自分で書いている** ——
入れた理由は「どの規模から効き始めるかを Phase 4 で測るため」だった。

【測り方】
本番の `comments` は触らない。`bench_part` スキーマに
**同じ列・同じ索引の 2 つ (または 3 つ) の表**を作り、
同一データを入れて同じクエリを流す。

    c_flat   分割なし
    c_h8     HASH(thread_id) 8 分割 (本番と同じ)
    c_h32    HASH(thread_id) 32 分割 (ADR 0009 未決 #12「8 で足りるか」)

【何が損で何が益か】
    益: パーティション除外が効くクエリ (thread_id で絞る) は、
        走査する索引が 1/N の大きさになる
    損: プランナがパーティションを列挙するぶん **Planning Time が増える**。
        除外が効かないクエリでは全パーティションを走査するので、
        索引が N 個に割れているぶん不利になりうる

**したがって Planning Time と Execution Time を分けて出す。**
合計だけ見ると、小規模で分割が負ける理由 (プランニング) が見えない。

【UNLOGGED を使っている】
WAL を書かないぶん投入が数倍速く、ディスクも半分で済む。
読み取りの計測は WAL と無関係なので影響しない。
**INSERT の絶対値だけは楽観的に出る** —— 比べているのは
flat と h8 の比なので、比較としては成立する。

使い方:
    make partition-probe                        既定の段 (20 万 → 200 万 → 2,000 万)
    make partition-probe PROBE_SCALES=200000    段を指定する
    make partition-probe PROBE_PARTITIONS=0,8,32

**CI には載せない。** 数字が環境の性能に左右されるため
(concurrency-probe / viewcount-probe / query-probe と同じ)。
"""

from __future__ import annotations

import os
import random
import re
import shlex
import statistics
import subprocess
import sys
import time

SQL_EXEC = os.environ.get(
    "SQL_EXEC", "docker compose exec -T postgres psql -U app -d bbs -X -q")
# 空き容量の確認だけは psql では取れないので、別に持つ。
DF_EXEC = os.environ.get(
    "DF_EXEC", "docker compose exec -T postgres df -P /var/lib/postgresql/data")
RUNS = int(os.environ.get("PROBE_RUNS", "5"))

# 段。**小さいほうから測る。** 大きい段でディスクが尽きても、
# それまでの結果は残る。
SCALES = [int(s) for s in os.environ.get(
    "PROBE_SCALES", "200000,2000000,20000000").split(",")]

# 0 は「分割なし」を意味する。
PARTITIONS = [int(p) for p in os.environ.get("PROBE_PARTITIONS", "0,8").split(",")]

# 1 スレッドあたりのコメント数。本番のベンチデータセット
# (2,000 スレッド / 20 万コメント) と同じ 100 件に揃える。
COMMENTS_PER_THREAD = int(os.environ.get("PROBE_COMMENTS_PER_THREAD", "100"))

# 1 行あたりのディスク使用量の見積もり (ヒープ + 索引 3 本)。
# 実測から出した概算で、投入前の空き容量チェックにだけ使う。
BYTES_PER_ROW = 220

EXEC_TIME = re.compile(r"Execution Time: ([\d.]+) ms")
PLAN_TIME = re.compile(r"Planning Time: ([\d.]+) ms")
# **最初の 1 つだけを採る。** 計画木の Buffers は親が子の合計を持つので、
# 全行を足すと入れ子の深さぶん重複して数える ——
# 区画ごとにノードが並ぶパーティション側だけが不当に大きく出る
# (8 分割で 3 倍近くに膨らんだ)。根 = 最初に現れる行が全体の合計になる。
BUFFERS = re.compile(r"Buffers: shared hit=(\d+)(?: read=(\d+))?")
# プランニング中に読んだバッファ。**分割のコストがここに出る。**
PLAN_BUFFERS = re.compile(r"Planning:\s*\n\s*Buffers: shared hit=(\d+)(?: read=(\d+))?")
# 実際に触ったパーティションを数える。**「除外が効いた」を数字で示す**ため。
# 計画木に出ないパーティションは走査されていない。
SCANNED_PART = re.compile(r"on (c_h\d+_p\d+|c_flat)\b")


def psql(statement: str, quiet: bool = False) -> str:
    """psql に文を流し、標準出力を返す。"""
    out = subprocess.run(shlex.split(SQL_EXEC) + ["-v", "ON_ERROR_STOP=1", "-c", statement],
                         capture_output=True, text=True)
    if out.returncode != 0:
        if not quiet:
            print(out.stderr.strip(), file=sys.stderr)
        raise RuntimeError(f"psql が失敗しました: {out.stderr.strip()[:400]}")
    return out.stdout


def value(statement: str) -> str:
    out = subprocess.run(shlex.split(SQL_EXEC) + ["-t", "-A", "-c", statement],
                         check=True, capture_output=True, text=True)
    return out.stdout.strip()


def progress(msg: str) -> None:
    """途中経過を出す。**投入は数分かかる。**"""
    print(f"  [{time.strftime('%H:%M:%S')}] {msg}", flush=True)


# ---------------------------------------------------------------------------
# 表の定義
# ---------------------------------------------------------------------------
# 本番の comments と同じ列・同じ索引にする。
# **索引まで揃えないと意味が無い** —— 測っているのは索引の大きさの差なので、
# 索引の構成が違えば何を比べたのか分からなくなる。

COLUMNS = """
    id          BIGINT      NOT NULL DEFAULT nextval('bench_part.c_id_seq'),
    thread_id   BIGINT      NOT NULL,
    author_name TEXT        NOT NULL DEFAULT '名無しさん',
    body        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ,
    seq         INTEGER     NOT NULL
"""


def table_name(parts: int) -> str:
    return "c_flat" if parts == 0 else f"c_h{parts}"


def create_tables() -> None:
    """スキーマと表を作り直す。"""
    psql("CREATE SCHEMA IF NOT EXISTS bench_part;")
    for parts in PARTITIONS:
        psql(f"DROP TABLE IF EXISTS bench_part.{table_name(parts)} CASCADE;")
    psql("DROP SEQUENCE IF EXISTS bench_part.c_id_seq;")
    psql("CREATE SEQUENCE bench_part.c_id_seq AS BIGINT;")

    for parts in PARTITIONS:
        name = table_name(parts)
        if parts == 0:
            psql(f"""
                CREATE UNLOGGED TABLE bench_part.{name} (
                    {COLUMNS},
                    CONSTRAINT {name}_pkey PRIMARY KEY (thread_id, id)
                );
            """)
            continue

        psql(f"""
            CREATE UNLOGGED TABLE bench_part.{name} (
                {COLUMNS},
                CONSTRAINT {name}_pkey PRIMARY KEY (thread_id, id)
            ) PARTITION BY HASH (thread_id);
        """)
        for r in range(parts):
            psql(f"CREATE UNLOGGED TABLE bench_part.{name}_p{r} "
                 f"PARTITION OF bench_part.{name} "
                 f"FOR VALUES WITH (MODULUS {parts}, REMAINDER {r});")


def create_indexes(parts: int) -> None:
    """投入後に索引を張る。**先に張ると投入が数倍遅くなる。**"""
    name = table_name(parts)
    # 生存コメントのみの部分索引 (本番の comments_alive_thread_id_desc_idx)。
    psql(f"CREATE INDEX {name}_alive_idx ON bench_part.{name} (thread_id, id DESC) "
         f"WHERE deleted_at IS NULL;")
    # レス番号の一意制約 (本番の 000004)。**INSERT のコストに効く**ので外さない。
    psql(f"CREATE UNIQUE INDEX {name}_seq_idx ON bench_part.{name} (thread_id, seq);")


def load(rows: int) -> None:
    """flat に生成し、他の変種へコピーする。

    **生成は 1 回だけにする。** 変種ごとに generate_series を回すと、
    乱数や now() のずれで中身が微妙に変わり、
    「同じデータに対する比較」でなくなる。
    """
    flat = table_name(0) if 0 in PARTITIONS else None
    source = flat or table_name(PARTITIONS[0])

    started = time.time()
    progress(f"{rows:,} 行を bench_part.{source} に生成中...")
    psql(f"""
        INSERT INTO bench_part.{source} (thread_id, seq, author_name, body, created_at, deleted_at)
        SELECT
            ((n - 1) / {COMMENTS_PER_THREAD}) + 1,
            ((n - 1) % {COMMENTS_PER_THREAD}) + 1,
            'ベンチ',
            'コメント本文 ' || n,
            -- **時刻を散らす。** 除外が効かないクエリ (created_at で絞る) を
            -- 測るとき、全行が同じ時刻だと範囲指定が全件か 0 件になる。
            now() - ((n % 2592000) || ' seconds')::interval,
            -- 本番のベンチデータセットと同じく 10% を論理削除する。
            CASE WHEN n % 10 = 0 THEN now() ELSE NULL END
        FROM generate_series(1, {rows}) AS n;
    """)
    progress(f"生成完了 ({time.time() - started:.0f} 秒)")

    for parts in PARTITIONS:
        name = table_name(parts)
        if name == source:
            continue
        started = time.time()
        progress(f"bench_part.{name} へコピー中...")
        psql(f"INSERT INTO bench_part.{name} "
             f"(id, thread_id, author_name, body, created_at, deleted_at, seq) "
             f"SELECT id, thread_id, author_name, body, created_at, deleted_at, seq "
             f"FROM bench_part.{source};")
        progress(f"コピー完了 ({time.time() - started:.0f} 秒)")

    for parts in PARTITIONS:
        started = time.time()
        progress(f"bench_part.{table_name(parts)} に索引を作成中...")
        create_indexes(parts)
        progress(f"索引完了 ({time.time() - started:.0f} 秒)")

    for parts in PARTITIONS:
        # **VACUUM ANALYZE を必ず流す。** バルク投入直後は visibility map が
        # 未整備で Index Only Scan が Heap Fetches を伴い、
        # 索引の効きが一時的に消える (db/bench/dataset.sql と同じ理由)。
        progress(f"bench_part.{table_name(parts)} を VACUUM ANALYZE 中...")
        psql(f"VACUUM (ANALYZE) bench_part.{table_name(parts)};")


# ---------------------------------------------------------------------------
# 計測
# ---------------------------------------------------------------------------

def explain(sql: str, parallel: bool = False) -> dict:
    """EXPLAIN (ANALYZE, BUFFERS) を流し、計画時間・実行時間・触れた区画数を返す。"""
    prelude = "" if parallel else "SET max_parallel_workers_per_gather = 0;"
    text = psql(f"{prelude} EXPLAIN (ANALYZE, BUFFERS) {sql}")

    exec_m = EXEC_TIME.search(text)
    plan_m = PLAN_TIME.search(text)
    buf_m = BUFFERS.search(text)
    plan_buf_m = PLAN_BUFFERS.search(text)
    # 同じ区画が複数ノードに出ることがあるので、集合にして数える。
    scanned = len(set(SCANNED_PART.findall(text)))

    def total(m) -> int:
        return 0 if m is None else int(m.group(1)) + int(m.group(2) or 0)

    return {
        "exec": float(exec_m.group(1)) if exec_m else 0.0,
        "plan": float(plan_m.group(1)) if plan_m else 0.0,
        "buffers": total(buf_m),
        "plan_buffers": total(plan_buf_m),
        "scanned": scanned,
    }


def measure(statements: list[str], parallel: bool = False) -> dict:
    """与えられた文を順に流し、中央値を返す。

    **文のリストを受け取る形にしてある。** 変種ごとに違うスレッド ID を
    引いてしまうと、「分割の差」ではなく「引いたスレッドの差」を
    測ることになる。呼び出し側が全変種に同じ引数を渡せる形にする。
    """
    samples = [explain(s, parallel) for s in statements]
    return {
        "exec": statistics.median(s["exec"] for s in samples),
        "plan": statistics.median(s["plan"] for s in samples),
        "buffers": statistics.median(s["buffers"] for s in samples),
        "plan_buffers": statistics.median(s["plan_buffers"] for s in samples),
        "scanned": max(s["scanned"] for s in samples),
    }


def sizes(parts: int) -> tuple[str, str]:
    """表と索引の大きさを返す。

    **親だけを見てはいけない。** pg_total_relation_size は
    パーティション親に対して 0 を返す (実体は子が持っている)。
    子を足さないと「分割したら 0 バイトになった」という表が出る。
    """
    name = table_name(parts)
    tree = (f"(SELECT oid FROM pg_class WHERE oid = 'bench_part.{name}'::regclass "
            f"UNION ALL SELECT inhrelid FROM pg_inherits "
            f"WHERE inhparent = 'bench_part.{name}'::regclass)")
    total = value(f"SELECT pg_size_pretty(sum(pg_total_relation_size(oid))) FROM {tree} AS t(oid);")
    idx = value(f"SELECT pg_size_pretty(sum(pg_indexes_size(oid))) FROM {tree} AS t(oid);")
    return total, idx


def free_bytes() -> int:
    """PostgreSQL のデータ領域の空き容量。投入前の判断に使う。

    **測る前に落ちるほうがましなので、先に見る。** 2,000 万行を 2 変種ぶん
    入れると 10 GB 近くになり、途中でディスクが尽きると
    それまでの段の結果ごと巻き添えになる。
    """
    out = subprocess.run(shlex.split(DF_EXEC), capture_output=True, text=True)
    if out.returncode != 0:
        return -1
    for line in out.stdout.splitlines()[1:]:
        cols = line.split()
        if len(cols) >= 4 and cols[3].isdigit():
            return int(cols[3]) * 1024
    return -1


# ---------------------------------------------------------------------------
# クエリ 4 種
# ---------------------------------------------------------------------------

def q_thread_page(name: str, thread_id: int) -> str:
    """1 スレッドのコメント一覧。**除外が最も効く形** (本番の主経路)。"""
    return f"""
SELECT id, seq, body FROM bench_part.{name}
WHERE thread_id = {thread_id} AND deleted_at IS NULL
ORDER BY id DESC LIMIT 50;
"""


def q_thread_count(name: str, thread_id: int) -> str:
    """1 スレッドのコメント数。一覧の相関サブクエリと同じ形。"""
    return f"""
SELECT count(*) FROM bench_part.{name}
WHERE thread_id = {thread_id} AND deleted_at IS NULL;
"""


def q_list_counts(name: str, ids: list[int]) -> str:
    """20 スレッドぶんの集計。**除外はほとんど効かない** ——
    20 個の thread_id はハッシュで散るため、8 区画すべてに当たる。
    スレッド一覧 1 ページぶんの集計に相当する。
    """
    arr = ",".join(str(i) for i in ids)
    return f"""
SELECT thread_id, count(*) FROM bench_part.{name}
WHERE thread_id = ANY(ARRAY[{arr}]::bigint[]) AND deleted_at IS NULL
GROUP BY thread_id;
"""


def q_global(name: str) -> str:
    """時刻で絞る全体集計。**除外がまったく効かない形。**
    パーティション分割が最も不利になる側で、
    「効き始める規模」を見るには両側が要る。
    """
    return f"""
SELECT count(*) FROM bench_part.{name}
WHERE created_at > now() - interval '1 hour' AND deleted_at IS NULL;
"""


def q_insert(name: str, thread_id: int, seq: int) -> str:
    """1 行 INSERT。索引 3 本の更新を含む。"""
    return f"""
INSERT INTO bench_part.{name} (thread_id, seq, body)
VALUES ({thread_id}, {seq}, 'プローブ');
"""


# ---------------------------------------------------------------------------
# 出力
# ---------------------------------------------------------------------------

def report(title: str, note: str, rows: list[dict]) -> None:
    print(f"\n{title}")
    if note:
        print(f"  {note}")
    print(f"{'変種':<10}{'計画ms':>9}{'実行ms':>9}{'合計ms':>9}"
          f"{'buffers':>10}{'計画buf':>9}{'触れた区画':>11}")
    print("-" * 69)
    for r in rows:
        total = r["plan"] + r["exec"]
        # **0 は「-」で出す。** 書き込みの計画木には走査ノードが出ないので、
        # 0 をそのまま並べると「1 区画も触らずに INSERT できた」と読めてしまう。
        scanned = str(r["scanned"]) if r["scanned"] else "-"
        print(f"{r['label']:<10}{r['plan']:>9.3f}{r['exec']:>9.3f}{total:>9.3f}"
              f"{r['buffers']:>10.0f}{r['plan_buffers']:>9.0f}{scanned:>11}")


def compare(title: str, note: str, build: callable, parallel: bool = False) -> None:
    """全変種に同じ引数でクエリを流し、並べて出す。

    build は (表名) を受け取って RUNS 個の文を返す。
    """
    report(title, note,
           [{"label": label(p), **measure(build(table_name(p)), parallel)}
            for p in PARTITIONS])


def run_scale(rows: int) -> bool:
    """1 つの段を測る。空き容量が足りなければ False を返す。"""
    threads = rows // COMMENTS_PER_THREAD
    print(f"\n{'=' * 72}")
    print(f"規模: {rows:,} 行 / {threads:,} スレッド (各クエリ {RUNS} 回の中央値)")
    print(f"{'=' * 72}")

    need = rows * BYTES_PER_ROW * len(PARTITIONS)
    free = free_bytes()
    if 0 < free < need:
        print(f"\n**空き容量が足りないので、この段は測れませんでした。**")
        print(f"  必要 約 {need / 1e9:.1f} GB / 空き {free / 1e9:.1f} GB")
        return False

    create_tables()
    load(rows)

    # **スレッド ID は毎回振り直す。** 同じ 1 件を繰り返すと、
    # その区画だけがキャッシュに乗り、除外の効果を過大評価する。
    # 種を固定して、段をまたいでも同じ引き方になるようにする。
    rng = random.Random(20260817)
    picks = [rng.randint(1, threads) for _ in range(RUNS)]
    list_ids = sorted(rng.sample(range(1, threads + 1), min(20, threads)))

    print("\n■ サイズ")
    print(f"{'変種':<10}{'合計':>12}{'索引':>12}")
    print("-" * 34)
    for parts in PARTITIONS:
        total, idx = sizes(parts)
        print(f"{label(parts):<10}{total:>12}{idx:>12}")

    compare("■ 1. 1 スレッドのコメント一覧 (LIMIT 50)",
            "除外が最も効く形。本番の主経路",
            lambda name: [q_thread_page(name, t) for t in picks])

    compare("■ 2. 1 スレッドのコメント数",
            "一覧の相関サブクエリと同じ形",
            lambda name: [q_thread_count(name, t) for t in picks])

    compare("■ 3. 20 スレッドぶんの集計",
            "ハッシュで散るため、除外はほとんど効かない",
            lambda name: [q_list_counts(name, list_ids)] * RUNS)

    compare("■ 4. 時刻で絞る全体集計 (並列なし)",
            "除外がまったく効かない形。分割が最も不利になる側",
            lambda name: [q_global(name)] * RUNS)

    compare("■ 5. 時刻で絞る全体集計 (並列あり)",
            "**分割の別の効き方。** Parallel Append は区画を並列に走査できる",
            lambda name: [q_global(name)] * RUNS, parallel=True)

    # seq は (thread_id, seq) が一意なので、変種ごとに同じ値を使ってよい
    # (表が別なので衝突しない)。スレッド 1 の末尾に足していく。
    seq_base = COMMENTS_PER_THREAD + 1
    compare("■ 6. 1 行 INSERT (索引 3 本の更新を含む)",
            "**UNLOGGED なので絶対値は楽観的。** 比べるのは変種どうしの比",
            lambda name: [q_insert(name, 1, seq_base + i) for i in range(RUNS)])

    return True


def label(parts: int) -> str:
    return "分割なし" if parts == 0 else f"HASH {parts}"


def main() -> int:
    print("パーティションの損益分岐点 (Phase 4)")
    print(f"変種: {', '.join(label(p) for p in PARTITIONS)}")
    print(f"段: {', '.join(f'{s:,}' for s in SCALES)} 行")
    free = free_bytes()
    if free > 0:
        print(f"データ領域の空き: {free / 1e9:.1f} GB")

    reached = []
    for rows in SCALES:
        if not run_scale(rows):
            # **測れなかった段を黙って落とさない。** 「測った」と
            # 「測れなかった」の区別が消えるのが一番まずい (README の Phase 4)。
            print(f"\n到達した段: {', '.join(f'{r:,}' for r in reached) or 'なし'} 行")
            print(f"未到達: {rows:,} 行以上")
            return 3
        reached.append(rows)

    print("\n※ 合計 ms = 計画 + 実行。**小規模で分割が負けるのは計画のほう**で、")
    print("   実行だけを見ていると損益分岐点を見誤ります。")
    print("\n後片付け: make partition-probe-clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
