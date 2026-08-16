#!/usr/bin/env python3
"""閲覧数の反映方式を実測して比べる (Phase 4 / ADR 0006 の「ベンチマーク題材」)。

**本命は 2 番目の比較になる。**

    1. A (同期 UPDATE) vs D (バッファ)
       スレッド詳細取得のスループットとレイテンシ分布

    2. **閲覧数の更新 あり / なし で、コメント投稿の直列化失敗率**
       「無関係に見える機能追加が、別の機能の並行制御を壊す」ことを数字で示す

    3. view_count 索引 あり / なし で、同期 UPDATE の HOT update 率

2 が ADR 0006 で D を選んだ理由そのもので、Phase 2 と Phase 4 を繋ぐ結節点になる。
コメント投稿は SERIALIZABLE (ssi) に固定して測る —— 直列化失敗が
現れるのはこのモードだけで、既定の unique では一意制約違反として出るため。

使い方:
    make viewcount-probe                        既定 (投稿 16 並列 / 閲覧 16 並列)
    make viewcount-probe PROBE_WORKERS=32

環境変数:
    BASE_URL        API のベース URL (既定: http://localhost:8080)
    SQL_EXEC        psql の呼び出し。**末尾に -c を付けないこと**
                    (concurrency-probe.py と同じ規約)
    PROBE_WORKERS   同時に投稿するクライアント数 (既定: 16)
    PROBE_VIEWERS   同時に閲覧するクライアント数 (既定: 16)
    PROBE_VIEWS     1 クライアントあたりの閲覧回数 (既定: 20)

**CI には載せない。** 数字が環境の性能に左右されるため、
通す / 落とすの基準にできない (concurrency-probe と同じ)。
"""

from __future__ import annotations

import http.client
import json
import os
import shlex
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE_URL = os.environ.get("BASE_URL", "http://localhost:8080").rstrip("/")

# csrfGuard は Origin の無い POST を 403 で弾く (ADR 0013 決定 1)。
ORIGIN = os.environ.get("PROBE_ORIGIN", "http://localhost:3000")

SQL_EXEC = os.environ.get("SQL_EXEC", "")
WORKERS = int(os.environ.get("PROBE_WORKERS", "16"))
VIEWERS = int(os.environ.get("PROBE_VIEWERS", "16"))
VIEWS_PER_CLIENT = int(os.environ.get("PROBE_VIEWS", "20"))

# 試行回数はログにしか出ない。**読む先は fluent-bit**
# (Phase 9 後半でログを転送するようにしたため。ADR 0010 決定 1)。
LOG_EXEC = os.environ.get(
    "LOG_EXEC", "docker compose logs fluent-bit --no-log-prefix --tail=6000")

COMPOSE_UP = ["docker", "compose", "up", "-d", "--force-recreate", "go-api"]


def call(method: str, path: str, body: str | None = None):
    """API を叩き、(ステータス, JSON, 所要ミリ秒) を返す。"""
    req = urllib.request.Request(BASE_URL + path, method=method)
    if method not in ("GET", "HEAD", "OPTIONS"):
        req.add_header("Origin", ORIGIN)
    data = None
    if body is not None:
        req.add_header("Content-Type", "application/json")
        data = body.encode()

    started = time.monotonic()
    try:
        with urllib.request.urlopen(req, data, timeout=30) as res:
            payload = json.loads(res.read() or b"null")
            return res.status, payload, (time.monotonic() - started) * 1000
    except urllib.error.HTTPError as e:
        raw = e.read()
        elapsed = (time.monotonic() - started) * 1000
        try:
            return e.code, json.loads(raw or b"null"), elapsed
        except json.JSONDecodeError:
            return e.code, None, elapsed
    except (urllib.error.URLError, OSError, http.client.HTTPException) as e:
        # 作り直した直後のコンテナは、ポートは開いているのに
        # まだ応答しない瞬間がある。
        return 0, {"error": str(e)}, (time.monotonic() - started) * 1000


def sql_value(statement: str) -> str:
    """SQL を実行して 1 つの値を文字列で返す。SQL_EXEC 未設定なら空文字。"""
    if not SQL_EXEC:
        return ""
    out = subprocess.run(shlex.split(SQL_EXEC) + ["-t", "-A", "-c", statement],
                         check=True, capture_output=True, text=True)
    return out.stdout.strip()


def sql_exec(statement: str) -> None:
    if not SQL_EXEC:
        return
    subprocess.run(shlex.split(SQL_EXEC) + ["-c", statement], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def retry_stats(thread_id: int) -> tuple[float, int, int]:
    """このスレッドへの投稿の (平均試行, 最大試行, 上限到達数) を返す。

    **API の応答からは分からない。** リトライは永続化層に閉じている
    (ADR 0019 決定 4)。JSON と logfmt の両方を読む。

    【上限到達を別に数える理由】
    `comment_created` は**成功した投稿にしか出ない**。
    リトライを使い切って 409 になった投稿はここに現れないため、
    平均試行だけを見ると **最も競合した投稿が集計から丸ごと落ちる。**

    実際、同期 UPDATE の条件では 409 が 4 件出たのに平均試行は
    他の条件より低く出た。「競合が減った」と読めてしまう形になる。
    上限到達 (`serialization_retry_exhausted`) を並べて初めて、
    何が起きたかが読める。
    """
    if not LOG_EXEC:
        return 0.0, 0, 0

    out = subprocess.run(shlex.split(LOG_EXEC), check=False,
                         capture_output=True, text=True)
    values: list[int] = []
    exhausted = 0
    for line in out.stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            continue
        if rec.get("thread_id") != thread_id:
            continue
        if rec.get("msg") == "comment_created" and isinstance(rec.get("attempts"), int):
            values.append(rec["attempts"])
        elif rec.get("msg") == "serialization_retry_exhausted":
            exhausted += 1
    if not values:
        return 0.0, 0, exhausted
    return sum(values) / len(values), max(values), exhausted


def restart(view_mode: str, dedupe_seconds: str = "0") -> None:
    """go-api を指定の設定で作り直し、応答するまで待つ。

    **コメント投稿は ssi に固定する。** 直列化失敗 (40001) が現れるのは
    このモードだけで、既定の unique では一意制約違反 (23505) として出る。
    測りたいのは「閲覧数の更新が rw-conflict をどれだけ増やすか」なので、
    SSI でなければ現象そのものが起きない。

    **重複抑制は既定で切る (窓 0)。** 同じ IP から叩くため、
    抑制が効くと閲覧が 1 回しか数えられず、**同期版でも UPDATE が
    ほとんど飛ばない** —— 比較そのものが成立しなくなる。
    """
    env = dict(os.environ,
               COMMENT_POST_MODE="ssi",
               VIEW_COUNT_MODE=view_mode,
               VIEW_COUNT_DEDUPE_SECONDS=dedupe_seconds)
    subprocess.run(COMPOSE_UP, check=True, env=env,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    for _ in range(60):
        status, _, _ = call("GET", "/healthz")
        if status == 200:
            return
        time.sleep(1)
    print("API が起動しませんでした", file=sys.stderr)
    sys.exit(1)


def create_thread(label: str) -> int:
    status, payload, _ = call("POST", "/threads",
                              json.dumps({"title": f"閲覧数プローブ {label}"}))
    if status != 201:
        print(f"スレッドを作れませんでした ({label}, status={status})", file=sys.stderr)
        sys.exit(1)
    return int(payload["id"])


def percentiles(samples: list[float]) -> tuple[float, float, float]:
    """(p50, p95, max) を返す。"""
    if not samples:
        return 0.0, 0.0, 0.0
    ordered = sorted(samples)
    p50 = statistics.median(ordered)
    idx = min(len(ordered) - 1, int(len(ordered) * 0.95))
    return p50, ordered[idx], ordered[-1]


def run_case(label: str, view_mode: str, with_views: bool) -> dict:
    """1 条件を測る。

    投稿と閲覧を**同時に**走らせる。順番に流すと、
    「閲覧が投稿の直列化に与える影響」という測りたいものが消える。
    """
    restart(view_mode)
    thread_id = create_thread(label)
    # 閲覧の対象は投稿先と**同じスレッド**にする。
    # 別スレッドにすると threads の別の行になり、rw-conflict が起きない ——
    # それでは「無関係な操作が競合を生む」ことを再現できない。
    view_path = f"/threads/{thread_id}"

    latencies: list[float] = []

    def post(i: int):
        return call("POST", f"/threads/{thread_id}/comments",
                    json.dumps({"body": f"{label} からの同時投稿 {i}"}))

    def view(_: int):
        out = []
        for _ in range(VIEWS_PER_CLIENT):
            _, _, ms = call("GET", view_path)
            out.append(ms)
        return out

    started = time.monotonic()
    with ThreadPoolExecutor(max_workers=WORKERS + VIEWERS) as pool:
        view_futures = [pool.submit(view, i) for i in range(VIEWERS)] if with_views else []
        post_futures = [pool.submit(post, i) for i in range(WORKERS)]
        results = [f.result() for f in post_futures]
        for f in view_futures:
            latencies.extend(f.result())
    elapsed_ms = (time.monotonic() - started) * 1000

    created = sum(1 for status, _, _ in results if status == 201)
    conflicts = sum(1 for status, _, _ in results if status == 409)
    failures = len(results) - created - conflicts

    # **ログが S3 ではなく fluent-bit の stdout に出るまで少し待つ。**
    # Flush 1 秒 + 転送のぶん。ここを詰めると試行回数が 0 になる。
    time.sleep(2)
    avg_attempts, max_attempts, exhausted = retry_stats(thread_id)

    p50, p95, worst = percentiles(latencies)
    return {
        "label": label,
        "created": created,
        "conflicts": conflicts,
        "failures": failures,
        "avg_attempts": avg_attempts,
        "max_attempts": max_attempts,
        "exhausted": exhausted,
        "elapsed_ms": elapsed_ms,
        "view_p50": p50,
        "view_p95": p95,
        "view_max": worst,
        "views": len(latencies),
    }


def hot_update_ratio(with_index: bool) -> dict:
    """同期 UPDATE の HOT update 率を測る (ADR 0006 の問題 3)。

    **HOT (Heap Only Tuple) update は、更新対象の列がどの索引にも
    含まれていないときだけ効く。** view_count に索引を張った時点で、
    1 回の更新がヒープと索引の両方を書くようになる。

    索引を落として比べることで、その代償を数字にする。
    """
    if not SQL_EXEC:
        return {}

    if with_index:
        sql_exec("CREATE INDEX IF NOT EXISTS threads_alive_popular_idx "
                 "ON threads (view_count DESC, id DESC) WHERE deleted_at IS NULL;")
    else:
        sql_exec("DROP INDEX IF EXISTS threads_alive_popular_idx;")

    # **統計をリセットしてから測る。** 累積値なので、
    # リセットしないと前の条件の数字が混ざる。
    sql_exec("SELECT pg_stat_reset_single_table_counters('threads'::regclass);")

    restart("sync")
    thread_id = create_thread("HOT update " + ("索引あり" if with_index else "索引なし"))

    def view(_: int):
        for _ in range(VIEWS_PER_CLIENT):
            call("GET", f"/threads/{thread_id}")

    with ThreadPoolExecutor(max_workers=VIEWERS) as pool:
        list(pool.map(view, range(VIEWERS)))

    # autovacuum と統計収集の反映を待つ。
    time.sleep(2)
    raw = sql_value(
        "SELECT n_tup_upd, n_tup_hot_upd, n_dead_tup FROM pg_stat_user_tables "
        "WHERE relname = 'threads';")
    if not raw:
        return {}
    upd, hot, dead = (int(v) for v in raw.split("|"))
    return {
        "with_index": with_index,
        "updates": upd,
        "hot_updates": hot,
        "hot_ratio": (hot / upd * 100) if upd else 0.0,
        "dead_tuples": dead,
    }


def main() -> int:
    print(f"投稿 {WORKERS} 並列 / 閲覧 {VIEWERS} 並列 x {VIEWS_PER_CLIENT} 回 "
          f"(コメント投稿は ssi 固定)\n")

    cases = [
        # **閲覧なしが基準線。** これが無いと「閲覧ありの数字が悪い」と
        # 言えない (ADR 0019 で naive を測ったのと同じ形)。
        ("閲覧なし", "buffered", False),
        ("閲覧あり (D: バッファ)", "buffered", True),
        ("閲覧あり (A: 同期 UPDATE)", "sync", True),
    ]
    results = [run_case(*c) for c in cases]

    print(f"{'条件':<26}{'成功':>6}{'競合':>6}{'失敗':>6}"
          f"{'上限到達':>10}{'平均試行':>10}{'最大試行':>10}{'所要ms':>9}")
    print("-" * 86)
    for r in results:
        print(f"{r['label']:<26}{r['created']:>6}{r['conflicts']:>6}{r['failures']:>6}"
              f"{r['exhausted']:>10}{r['avg_attempts']:>10.2f}"
              f"{r['max_attempts']:>10}{r['elapsed_ms']:>9.0f}")

    # **平均試行だけを見ない。** 上の docstring (retry_stats) のとおり、
    # 成功した投稿しか comment_created を出さないので、
    # 競合が激しいほど「平均試行」は下振れする。
    print("\n  ※ 平均試行は**成功した投稿のみ**の値です。")
    print("     リトライを使い切った投稿 (上限到達) は含まれないため、")
    print("     競合が激しい条件ほど平均試行は下振れします。")

    print(f"\n{'条件':<26}{'閲覧数':>8}{'p50 ms':>10}{'p95 ms':>10}{'最大 ms':>10}")
    print("-" * 66)
    for r in results:
        if r["views"] == 0:
            continue
        print(f"{r['label']:<26}{r['views']:>8}{r['view_p50']:>10.1f}"
              f"{r['view_p95']:>10.1f}{r['view_max']:>10.1f}")

    # ---------------------------------------------------------------
    # HOT update (ADR 0006 の問題 3)
    # ---------------------------------------------------------------
    if SQL_EXEC:
        print(f"\n{'HOT update (同期 UPDATE)':<26}{'更新':>8}{'HOT':>8}"
              f"{'HOT率%':>10}{'デッド':>8}")
        print("-" * 62)
        hot_rows = [hot_update_ratio(True), hot_update_ratio(False)]
        for row in hot_rows:
            if not row:
                continue
            name = "索引あり" if row["with_index"] else "索引なし"
            print(f"{name:<26}{row['updates']:>8}{row['hot_updates']:>8}"
                  f"{row['hot_ratio']:>10.1f}{row['dead_tuples']:>8}")

        # **索引を必ず戻す。** 落としたままだと人気順が全表走査になる。
        sql_exec("CREATE INDEX IF NOT EXISTS threads_alive_popular_idx "
                 "ON threads (view_count DESC, id DESC) WHERE deleted_at IS NULL;")
        print("\n(threads_alive_popular_idx は測定後に復元済み)")
    else:
        print("\nSQL_EXEC が未設定のため、HOT update の比較は行いませんでした")

    # 設定を既定へ戻す。**戻さないと sync のまま開発が続く。**
    restart("buffered", dedupe_seconds="")
    print("(go-api は buffered に戻してあります)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
