#!/usr/bin/env python3
"""コメント投稿の並行制御を、4 つのモードで実測して比べる。

同一バイナリのまま COMMENT_POST_MODE だけを差し替え、
**同じスレッドへ同時に投稿**したときに何が起きるかを測る。
設計は docs/adr/0019-comment-concurrency.md。

測るもの:
    成功        201 が返った数
    競合        409 (リトライを使い切った競合)
    失敗        それ以外 (naive では一意制約違反がここに出る)
    試行        1 投稿あたりの平均試行回数 (ログの attempts から)
    重複        DB 側で同じ (thread_id, seq) が 2 行以上あるか
    所要        全投稿が終わるまでの実時間

**「テストが通った」ではなく「DB が拒否しなかった」を根拠にする。**
レス番号の一意性は UNIQUE (thread_id, seq) が保証しているので、
壊れていれば必ずどこかに現れる。

使い方:
    make concurrency-probe                      既定 (32 並列 x 4 モード)
    make concurrency-probe PROBE_WORKERS=64     並列数を変える

環境変数:
    BASE_URL        API のベース URL (既定: http://localhost:8080)
    SQL_EXEC        psql の呼び出し。**末尾に -c を付けないこと** ——
                    このスクリプトが -t -A -c を足して値だけを取り出す
                    (smoke-test.py の SQL_EXEC とは規約が違う)。
                    例: "docker compose exec -T postgres psql -U app -d bbs -X -q"
    PROBE_WORKERS   同時に投稿するクライアント数 (既定: 32)
    PROBE_MODES     測るモードをカンマ区切りで指定 (既定: 全 4 つ)
"""

from __future__ import annotations

import http.client
import json
import os
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE_URL = os.environ.get("BASE_URL", "http://localhost:8080").rstrip("/")
SQL_EXEC = os.environ.get("SQL_EXEC", "")
WORKERS = int(os.environ.get("PROBE_WORKERS", "32"))
MODES = [m.strip() for m in
         os.environ.get("PROBE_MODES", "naive,ssi,pessimistic,unique").split(",")
         if m.strip()]
# リトライ回数はログにしか出ない。開発モードの text 形式を読む。
LOG_EXEC = os.environ.get(
    "LOG_EXEC", "docker compose logs go-api --no-log-prefix --tail=2000")

# モードを切り替えるにはコンテナを作り直す必要がある。
# compose.yaml が COMMENT_POST_MODE をホストから補間して渡している
# (既定値は compose 側に書かず、config の parseCommentPostMode に任せてある)。
COMPOSE_UP = ["docker", "compose", "up", "-d", "--force-recreate", "go-api"]


def call(method: str, path: str, body: str | None = None):
    """API を叩き、(ステータス, JSON) を返す。"""
    req = urllib.request.Request(BASE_URL + path, method=method)
    data = None
    if body is not None:
        req.add_header("Content-Type", "application/json")
        data = body.encode()
    try:
        with urllib.request.urlopen(req, data, timeout=30) as res:
            return res.status, json.loads(res.read() or b"null")
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw or b"null")
        except json.JSONDecodeError:
            return e.code, None
    except (urllib.error.URLError, OSError, http.client.HTTPException) as e:
        # 作り直した直後のコンテナは、ポートは開いているのに
        # まだ応答しない瞬間がある。そこで RemoteDisconnected が飛ぶ。
        # **これを捕まえないと、起動待ちのループごと落ちる。**
        return 0, {"error": str(e)}


def sql_value(statement: str) -> str:
    """SQL を実行して 1 つの値を文字列で返す。SQL_EXEC 未設定なら空文字。"""
    if not SQL_EXEC:
        return ""
    out = subprocess.run(shlex.split(SQL_EXEC) + ["-t", "-A", "-c", statement],
                         check=True, capture_output=True, text=True)
    return out.stdout.strip()


def attempts_for(thread_id: int) -> tuple[float, int]:
    """このスレッドへの投稿にかかった試行回数の (平均, 最大) を返す。

    **API の応答からは分からない。** リトライは永続化層に閉じているので、
    観測できるのはログだけになる (docs/adr/0019-comment-concurrency.md 決定 4)。
    開発モードの text 形式を前提に key=value を拾う。
    """
    if not LOG_EXEC:
        return 0.0, 0

    out = subprocess.run(shlex.split(LOG_EXEC), check=False,
                         capture_output=True, text=True)
    values = []
    for line in out.stdout.splitlines():
        if "msg=comment_created" not in line or f"thread_id={thread_id} " not in line:
            continue
        for field in line.split():
            if field.startswith("attempts="):
                values.append(int(field.removeprefix("attempts=")))
    if not values:
        return 0.0, 0
    return sum(values) / len(values), max(values)


def restart_with_mode(mode: str) -> None:
    """go-api を指定モードで作り直し、応答するまで待つ。"""
    env = dict(os.environ, COMMENT_POST_MODE=mode)
    subprocess.run(COMPOSE_UP, check=True, env=env,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    # go run は起動時にコンパイルするため、健全性が返るまで数秒かかる。
    for _ in range(60):
        status, _ = call("GET", "/healthz")
        if status == 200:
            return
        time.sleep(1)
    raise SystemExit(f"go-api が mode={mode} で起動しませんでした")


def probe(mode: str) -> dict:
    """1 モードぶんの実測を行う。"""
    restart_with_mode(mode)

    status, thread = call("POST", "/threads", json.dumps({"title": f"並行投稿の実測 ({mode})"}))
    if status != 201:
        raise SystemExit(f"スレッドを作れませんでした (mode={mode}, status={status})")
    thread_id = thread["id"]

    def post(i: int):
        return call("POST", f"/threads/{thread_id}/comments",
                    json.dumps({"body": f"{mode} からの同時投稿 {i}"}))

    started = time.monotonic()
    with ThreadPoolExecutor(max_workers=WORKERS) as pool:
        results = list(pool.map(post, range(WORKERS)))
    elapsed = time.monotonic() - started

    created = [payload for status, payload in results if status == 201]
    conflicts = sum(1 for status, _ in results if status == 409)
    failures = len(results) - len(created) - conflicts

    # **正しさの根拠は API の応答ではなく DB に置く。**
    # 同じ (thread_id, seq) が 2 行あれば、それは一意索引が
    # 張られていないか、張り忘れた経路があるということになる。
    duplicates = sql_value(
        "SELECT count(*) FROM ("
        f"  SELECT seq FROM comments WHERE thread_id = {thread_id}"
        "   GROUP BY seq HAVING count(*) > 1) d;")
    stored = sql_value(f"SELECT count(*) FROM comments WHERE thread_id = {thread_id};")
    max_seq = sql_value(
        f"SELECT COALESCE(max(seq), 0) FROM comments WHERE thread_id = {thread_id};")

    avg_attempts, max_attempts = attempts_for(thread_id)

    return {
        "mode": mode,
        "thread_id": thread_id,
        "ok": len(created),
        "conflict": conflicts,
        "failed": failures,
        "avg_attempts": avg_attempts,
        "max_attempts": max_attempts,
        "elapsed_ms": elapsed * 1000,
        "duplicates": duplicates or "?",
        "stored": stored or "?",
        "max_seq": max_seq or "?",
        # seq が欠けていても集計を止めない。
        # KeyError で落ちると、全モードぶんの結果表が出ないまま終わる。
        "seqs": sorted(c["seq"] for c in created if "seq" in c),
    }


def main() -> int:
    print(f"同時投稿 {WORKERS} 並列 / モード {', '.join(MODES)}")
    if not SQL_EXEC:
        print("  \033[33m注意\033[0m SQL_EXEC が未設定のため、DB 側の重複検査は行いません")

    rows = [probe(mode) for mode in MODES]

    print()
    print(f"{'モード':<14}{'成功':>6}{'競合':>6}{'失敗':>6}{'重複':>6}"
          f"{'平均試行':>9}{'最大試行':>9}{'所要ms':>9}")
    print("-" * 68)
    for r in rows:
        print(f"{r['mode']:<12}{r['ok']:>6}{r['conflict']:>6}{r['failed']:>6}"
              f"{str(r['duplicates']):>6}{r['avg_attempts']:>9.2f}"
              f"{r['max_attempts']:>9}{r['elapsed_ms']:>9.0f}")

    print()
    problems = []
    for r in rows:
        # 重複は「あってはならない」。一意索引がある限り DB が拒否するので、
        # ここが 0 でないなら索引が張られていない経路がある。
        if r["duplicates"] not in ("0", "?"):
            problems.append(f"{r['mode']}: レス番号が {r['duplicates']} 通り重複している")

        # 成功したぶんのレス番号は必ず 1..成功数 の連番になる。
        # 抜けや飛びがあれば、採番が「読み直していない」ことを意味する。
        expected = list(range(1, r["ok"] + 1))
        if r["seqs"] != expected:
            problems.append(
                f"{r['mode']}: レス番号が連番でない (got={r['seqs'][:8]}... want=1..{r['ok']})")

    if problems:
        print("\033[31m問題が見つかりました\033[0m")
        for p in problems:
            print(f"  - {p}")
        return 1

    print("\033[32mレス番号の一意性と連番は、全モードで保たれています\033[0m")
    print("(naive の「壊れ方」は重複ではなく失敗として現れます —— 上の 失敗 列を参照)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
