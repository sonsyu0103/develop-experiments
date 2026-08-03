#!/usr/bin/env python3
"""API が実 DB と正しく喋れることを、HTTP 越しに検証する。

ユニットテストはフェイクのリポジトリで動くため、
「SQL が実際に意図どおり動くか」「DB とアプリの前提がずれていないか」は
検証できない。ここを埋めるのがこのスクリプト。

前提:
    - PostgreSQL が起動し、マイグレーションとシードが適用済み
    - API が BASE_URL で待ち受けている

使い方:
    make smoke                        # ローカル (compose 経由)
    BASE_URL=... SQL_EXEC=... python3 .github/scripts/smoke-test.py

環境変数:
    BASE_URL  API のベース URL (既定: http://localhost:8080)
    SQL_EXEC  SQL を 1 文実行するコマンド。末尾に SQL が引数として渡される。
              指定しない場合、DB 直接操作を伴う検証はスキップする。
              例: "docker compose exec -T postgres psql -U app -d bbs -X -q -c"
"""

# macOS 標準の Python 3.9 でも動くよう、PEP 604 の "X | None" 記法を
# 実行時評価させない。CI (Ubuntu) は新しい Python だが、
# ローカルで動かないスクリプトは結局使われなくなる。
from __future__ import annotations

import json
import os
import shlex
import subprocess
import sys
import urllib.error
import urllib.request

BASE_URL = os.environ.get("BASE_URL", "http://localhost:8080").rstrip("/")
SQL_EXEC = os.environ.get("SQL_EXEC", "")

# シードデータが作る状態 (db/seed/seed.sql と揃える)。
# スレッド 1-4 に 4 件ずつ、スレッド 5 は 0 件。
# うち最も古いコメント 1 件が論理削除されるため、スレッド 1 だけ 3 件になる。
EXPECTED_COUNTS = {1: 3, 2: 4, 3: 4, 4: 4, 5: 0}

failures: list[str] = []
checks = 0


def call(method: str, path: str, body: str | None = None):
    """API を叩き、(ステータス, JSON, ヘッダ) を返す。"""
    req = urllib.request.Request(BASE_URL + path, method=method)
    data = None
    if body is not None:
        req.add_header("Content-Type", "application/json")
        data = body.encode()
    try:
        with urllib.request.urlopen(req, data, timeout=10) as res:
            raw = res.read()
            return res.status, (json.loads(raw) if raw else None), dict(res.headers)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw), dict(e.headers)
        except json.JSONDecodeError:
            return e.code, raw.decode(errors="replace")[:200], dict(e.headers)


def sql(statement: str) -> None:
    """SQL を 1 文実行する。SQL_EXEC 未設定なら何もしない。"""
    subprocess.run(shlex.split(SQL_EXEC) + [statement], check=True,
                   stdout=subprocess.DEVNULL)


def check(label: str, ok: bool, detail: str = "") -> None:
    global checks
    checks += 1
    if ok:
        print(f"  \033[32mOK\033[0m   {label}")
    else:
        print(f"  \033[31mFAIL\033[0m {label}" + (f"  ({detail})" if detail else ""))
        failures.append(label)


def check_status(label: str, method: str, path: str, want: int,
                 body: str | None = None, want_code: str | None = None) -> None:
    status, payload, _ = call(method, path, body)
    got_code = ""
    if isinstance(payload, dict) and isinstance(payload.get("error"), dict):
        got_code = payload["error"].get("code", "")

    ok = status == want and (want_code is None or got_code == want_code)
    detail = f"status={status} want={want}"
    if want_code:
        detail += f" / code={got_code or '(なし)'} want={want_code}"
    check(label, ok, detail)


def section(title: str) -> None:
    print(f"\n\033[1m{title}\033[0m")


# ---------------------------------------------------------------------------

section("死活監視")
check_status("GET /healthz", "GET", "/healthz", 200)
check_status("GET /readyz  (DB 疎通を含む)", "GET", "/readyz", 200)

section("一覧とコメント数の集計")
status, payload, _ = call("GET", "/threads")
check("GET /threads が 200", status == 200, f"status={status}")
if status == 200:
    got = {t["id"]: t["commentCount"] for t in payload["threads"]}
    check(f"コメント数が論理削除を除外している {EXPECTED_COUNTS}",
          got == EXPECTED_COUNTS, f"got={got}")
    ids = [t["id"] for t in payload["threads"]]
    check("新しい順 (ID 降順) で返る", ids == sorted(ids, reverse=True), f"ids={ids}")
    check("最終ページでは nextCursor が null", payload["nextCursor"] is None,
          f"nextCursor={payload['nextCursor']}")
    check("createdAt が含まれる", all("createdAt" in t for t in payload["threads"]))

section("キーセットページネーション")
status, page1, _ = call("GET", "/threads?size=2")
if status == 200:
    ids1 = [t["id"] for t in page1["threads"]]
    check("1 ページ目が [5, 4]", ids1 == [5, 4], f"ids={ids1}")
    check("nextCursor が 4", page1["nextCursor"] == 4, f"got={page1['nextCursor']}")

    status, page2, _ = call("GET", f"/threads?size=2&cursor={page1['nextCursor']}")
    ids2 = [t["id"] for t in page2["threads"]]
    check("2 ページ目が [3, 2]", ids2 == [3, 2], f"ids={ids2}")
    check("ページ間で重複しない", not set(ids1) & set(ids2))

section("単体取得とコメント")
check_status("GET /threads/1", "GET", "/threads/1", 200)
check_status("GET /threads/1/comments", "GET", "/threads/1/comments", 200)
check_status("GET /threads/9999 は 404", "GET", "/threads/9999", 404, want_code="NOT_FOUND")
check_status("GET /threads/9999/comments は 404 (0 件ではない)",
             "GET", "/threads/9999/comments", 404, want_code="NOT_FOUND")

status, payload, _ = call("GET", "/threads/5/comments")
check("コメント 0 件のスレッドは 200 かつ空配列",
      status == 200 and payload["comments"] == [], f"status={status}")

section("書き込み")
check_status("POST /threads", "POST", "/threads", 201, '{"title":"smoke test"}')
status, payload, _ = call("POST", "/threads/1/comments", '{"body":"smoke test"}')
check("POST /threads/1/comments が 201", status == 201, f"status={status}")
if status == 201:
    check("authorName が既定値になる", payload["authorName"] == "名無しさん",
          f"got={payload['authorName']}")

section("仕様書によるリクエスト検証")
for label, method, path, body in [
    ("threadId が 0 (minimum 違反)", "GET", "/threads/0", None),
    ("threadId が非数値", "GET", "/threads/abc", None),
    ("size が上限超過", "GET", "/threads?size=101", None),
    ("size が 0", "GET", "/threads?size=0", None),
    ("cursor が 0", "GET", "/threads?cursor=0", None),
    ("cursor が非数値", "GET", "/threads?cursor=xyz", None),
    ("title が空", "POST", "/threads", '{"title":""}'),
    ("title 未指定", "POST", "/threads", "{}"),
    ("title が型違い", "POST", "/threads", '{"title":123}'),
    ("title が長すぎる", "POST", "/threads", json.dumps({"title": "あ" * 201})),
    ("body が長すぎる", "POST", "/threads/1/comments", json.dumps({"body": "あ" * 2001})),
    ("body が空白のみ (ドメイン層で検出)", "POST", "/threads/1/comments", '{"body":"   "}'),
]:
    check_status(label, method, path, 400, body, want_code="INVALID_ARGUMENT")

check_status("上限ちょうどの size は通る", "GET", "/threads?size=100", 200)

section("ルーティング")
check_status("仕様書に無いパスは 404", "GET", "/threads/1/likes", 404, want_code="NOT_FOUND")
check_status("未定義メソッドは 405", "DELETE", "/threads/1", 405,
             want_code="METHOD_NOT_ALLOWED")

section("CORS")
req = urllib.request.Request(BASE_URL + "/threads")
with urllib.request.urlopen(req, timeout=10) as res:
    vary = res.headers.get("Vary", "")
check("Origin ヘッダが無くても Vary: Origin が付く", "Origin" in vary, f"Vary={vary!r}")

section("エラー応答が内部構造を漏らさない")
_, payload, _ = call("GET", "/threads/9999")
leaked = [w for w in ("Repository", "sqlc", "SELECT", "pgx", "threads.")
          if w in json.dumps(payload, ensure_ascii=False)]
check("404 応答に内部名が含まれない", not leaked, f"leaked={leaked}")

if SQL_EXEC:
    section("論理削除されたスレッドの扱い")
    sql("UPDATE threads SET deleted_at = now() WHERE id = 3;")
    try:
        check_status("参照は 404", "GET", "/threads/3", 404, want_code="NOT_FOUND")
        check_status("コメント一覧も 404 (中身が読めない)",
                     "GET", "/threads/3/comments", 404, want_code="NOT_FOUND")
        check_status("投稿も 404 (FK は満たされてしまうため要注意)",
                     "POST", "/threads/3/comments", 404,
                     '{"body":"削除済みへの投稿"}', want_code="NOT_FOUND")
        status, payload, _ = call("GET", "/threads")
        ids = [t["id"] for t in payload["threads"]]
        check("一覧から消える", 3 not in ids, f"ids={ids}")
    finally:
        sql("UPDATE threads SET deleted_at = NULL WHERE id = 3;")
else:
    section("論理削除されたスレッドの扱い")
    print("  \033[33mSKIP\033[0m SQL_EXEC が未設定のためスキップ")

# ---------------------------------------------------------------------------

print()
if failures:
    print(f"\033[31m{len(failures)} / {checks} 件が失敗しました\033[0m")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)

print(f"\033[32m{checks} 件すべて成功しました\033[0m")
