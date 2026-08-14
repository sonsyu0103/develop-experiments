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

    SMOKE_REQUIRE_FULL
              1 なら、**1 つでもスキップした節があれば失敗させる**。
              未設定のときは CI 環境変数の有無で決まる (CI では既定で有効)。
              0 を明示すると無効にできる。

**スキップは黙って通してはいけない。**

このスクリプトは以前、認証まわりが丸ごとスキップされた状態で
「74 件すべて成功しました」と表示し、終了コード 0 で通っていた。
冪等キーの 11 件は CI で一度も実行されていなかったのに、CI は緑だった
(docs/adr/0005-authentication.md 決定 4)。

原因はスキップを失敗として数えないことにある。設定を 1 つ落としただけで
検証範囲が静かに縮む。**SQL_EXEC を消す / psql がランナーから消える**
だけで、60 件近くが同じように消える。
そのため CI では未実行の節そのものを失敗として扱う。
"""

# macOS 標準の Python 3.9 でも動くよう、PEP 604 の "X | None" 記法を
# 実行時評価させない。CI (Ubuntu) は新しい Python だが、
# ローカルで動かないスクリプトは結局使われなくなる。
from __future__ import annotations

import hashlib
import json
import os
import secrets
import shlex
import subprocess
import sys
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE_URL = os.environ.get("BASE_URL", "http://localhost:8080").rstrip("/")

# **状態変更メソッドには Origin が要る** (docs/adr/0013-http-defense.md 決定 1)。
#
# csrfGuard は Origin も Referer も無い POST / PUT / PATCH / DELETE を 403 で弾く。
# 「無ければ通す」にすると送らないだけで迂回できるため、ブラウザ以外の
# クライアント (このスクリプトを含む) も付ける必要がある。
#
# 既定は config の CORS_ALLOWED_ORIGINS の既定値と同じ。
# API 側の設定を変えたらここも変える —— 食い違うと全部 403 になる。
ORIGIN = os.environ.get("SMOKE_ORIGIN", "http://localhost:3000")

# 副作用を持ちうるメソッド。csrfGuard の isStateChanging と揃える。
STATE_CHANGING = ("POST", "PUT", "PATCH", "DELETE")
SQL_EXEC = os.environ.get("SQL_EXEC", "")


def _require_full() -> bool:
    """スキップを失敗として扱うかどうか。

    **既定は「CI なら扱う」。** 明示的な env を必須にすると、
    その 1 行を消しただけで検出が止まる —— それは今回直した事故と同じ形になる。
    ここは fail-closed 側に倒す。手元で一部だけ回したいときに 0 を渡す。
    """
    raw = os.environ.get("SMOKE_REQUIRE_FULL")
    if raw is not None:
        return raw.strip() not in ("", "0", "false", "no")
    return bool(os.environ.get("CI"))


REQUIRE_FULL = _require_full()

# シードデータが作る状態 (db/seed/seed.sql と揃える)。
# スレッド 1-4 に 4 件ずつ、スレッド 5 は 0 件。
# うち最も古いコメント 1 件が論理削除されるため、スレッド 1 だけ 3 件になる。
EXPECTED_COUNTS = {1: 3, 2: 4, 3: 4, 4: 4, 5: 0}

failures: list[str] = []
checks = 0
# 実行しなかった節。**件数ではなく節そのもの**を記録する ——
# 「何件通ったか」は縮んでも気づけないが、「どの節を飛ばしたか」は残る。
skipped: list[str] = []


def _decode(raw: bytes):
    """本文を JSON として読む。読めなければ None を返す。

    **例外にしない。** リダイレクトの本文は HTML なので、
    json.loads がそのまま落ちるとスモーク全体がトレースバックで止まり、
    check() の集計にも乗らない (何件中何件失敗したかが分からなくなる)。
    """
    if not raw:
        return None
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return None


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """リダイレクトを追跡しないためのハンドラ。

    **既定の urllib は 302 を自動で追いかける。** API のスモークでは
    「何番を返したか」を見たいので、追われると検査にならない。

    実害もある。認証を設定した環境では /auth/google が
    accounts.google.com へリダイレクトするため、
    **スモークが外部へ実際に接続していた**。CI から意図しない
    外向き通信が出るし、Google 側の応答に結果が左右される。
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_opener = urllib.request.build_opener(_NoRedirect)


def call(method: str, path: str, body: str | None = None,
         headers: dict[str, str] | None = None, origin: str | None = None):
    """API を叩き、(ステータス, JSON, ヘッダ) を返す。

    origin を省くと、状態変更メソッドには ORIGIN を自動で付ける。
    空文字を渡すと**付けない** —— csrfGuard そのものを検査するときに使う。
    """
    req = urllib.request.Request(BASE_URL + path, method=method)
    if origin is None:
        origin = ORIGIN if method in STATE_CHANGING else ""
    if origin:
        req.add_header("Origin", origin)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    data = None
    if body is not None:
        req.add_header("Content-Type", "application/json")
        data = body.encode()
    try:
        with _opener.open(req, data, timeout=10) as res:
            raw = res.read()
            # **dict() にしない。** Set-Cookie のように同名で複数回
            # 現れるヘッダが 1 本に潰れ、「3 つ発行したのに 1 つしか見えない」
            # という形で検査が嘘をつく (実際に踏んだ)。
            # Message のまま返せば get_all() で全部取れる。
            return res.status, _decode(raw), res.headers
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, _decode(raw), e.headers
        except json.JSONDecodeError:
            return e.code, raw.decode(errors="replace")[:200], e.headers


def sql(statement: str, quiet: bool = False) -> None:
    """SQL を 1 文実行する。SQL_EXEC 未設定なら何もしない。

    quiet=True は「失敗を期待する」呼び出し用。標準エラーも捨てる。
    **期待どおりの拒否で ERROR 行がログに出ると、
    本物の異常と見分けが付かなくなる** (赤い行を無視する癖がつく)。
    """
    # **ガードが本体に無いまま docstring だけが「何もしない」と書いていた。**
    # 空だと shlex.split("") が [] になり、SQL 文字列を
    # プログラムとして exec しようとして FileNotFoundError で落ちる。
    # 呼び出しがすべて if SQL_EXEC: の下にある間は表に出ないが、
    # 外で 1 度呼ばれた瞬間に、check() の集計に乗らない形で全体が止まる。
    if not SQL_EXEC:
        return

    subprocess.run(shlex.split(SQL_EXEC) + [statement], check=True,
                   stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL if quiet else None)


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


def skip(title: str, reason: str) -> None:
    """節を実行しなかったことを記録する。

    **print だけで済ませない。** 済ませていた結果が
    「CI が緑のまま冪等キーを 1 件も検証していない」状態だった。
    REQUIRE_FULL が立っていれば、これは最後に失敗として集計される。
    """
    skipped.append(f"{title} ({reason})")
    print(f"  \033[33mSKIP\033[0m {reason}")


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
    # カーソルは不透明トークン。中身 (id=4) を素で返していないことも見る。
    token = page1["nextCursor"]
    check("nextCursor が文字列トークン", isinstance(token, str), f"got={token!r}")
    check("nextCursor が id を素で公開していない", token != "4" and token != 4,
          f"got={token!r}")

    status, page2, _ = call("GET", f"/threads?size=2&cursor={token}")
    ids2 = [t["id"] for t in page2["threads"]]
    check("2 ページ目が [3, 2]", ids2 == [3, 2], f"ids={ids2}")
    check("ページ間で重複しない", not set(ids1) & set(ids2))

    # クライアントが ?cursor=${nextCursor ?? ''} と組み立てても
    # 初回ロードが 400 にならないこと。
    status, empty_cursor, _ = call("GET", "/threads?size=2&cursor=")
    check("空の cursor は先頭ページと同じ",
          status == 200 and [t["id"] for t in empty_cursor["threads"]] == ids1,
          f"status={status}")

# コメント側は経路が別 (別 usecase・別クエリ・パーティション越し) なので、
# スレッド一覧が通っていてもここが通っている保証にはならない。
status, all_comments, _ = call("GET", "/threads/2/comments")
if status == 200:
    total = len(all_comments["comments"])
    check("スレッド 2 のコメントが 3 件以上ある (ページ送りの前提)", total >= 3, f"got={total}")

    status, cpage1, _ = call("GET", "/threads/2/comments?size=2")
    cids1 = [c["id"] for c in cpage1["comments"]]
    check("コメント 1 ページ目が 2 件", len(cids1) == 2, f"ids={cids1}")
    check("コメントも新しい順 (ID 降順)", cids1 == sorted(cids1, reverse=True), f"ids={cids1}")

    ctoken = cpage1["nextCursor"]
    check("コメントの nextCursor が不透明トークン",
          isinstance(ctoken, str) and ctoken != str(cids1[-1]), f"got={ctoken!r}")

    status, cpage2, _ = call("GET", f"/threads/2/comments?size=2&cursor={ctoken}")
    cids2 = [c["id"] for c in cpage2["comments"]]
    # カーソルが「最後に返した行」ではなく先頭を指していると、ここで重複が出る。
    check("コメント 2 ページ目は 1 ページ目より小さい ID だけを返す",
          cids2 and max(cids2) < min(cids1), f"1: {cids1}, 2: {cids2}")
    check("コメントのページ間で重複しない", not set(cids1) & set(cids2))

section("単体取得とコメント")
check_status("GET /threads/1", "GET", "/threads/1", 200)
check_status("GET /threads/1/comments", "GET", "/threads/1/comments", 200)
check_status("GET /threads/9999 は 404", "GET", "/threads/9999", 404, want_code="NOT_FOUND")
check_status("GET /threads/9999/comments は 404 (0 件ではない)",
             "GET", "/threads/9999/comments", 404, want_code="NOT_FOUND")

status, payload, _ = call("GET", "/threads/5/comments")
check("コメント 0 件のスレッドは 200 かつ空配列",
      status == 200 and payload["comments"] == [], f"status={status}")

# レス番号 (docs/adr/0019-comment-concurrency.md)。
# シードはスレッド 1 の 1 番を論理削除しているので、2..4 が残る。
status, payload, _ = call("GET", "/threads/1/comments")
if status == 200:
    # **c["seq"] を先に評価しない。** 詰め替えが落ちて seq が消えると
    # KeyError でスモーク全体がトレースバックで止まり、
    # 「何件中何件失敗したか」の集計にも FAIL としても残らない。
    # この検査が守ろうとしているまさにその壊れ方で、検査が消える。
    check("レス番号が API に出る", all("seq" in c for c in payload["comments"]))
    seqs = sorted(c.get("seq") for c in payload["comments"] if "seq" in c)
    # **削除しても番号を詰めない。** 詰めると過去の >>5 が別の投稿を指す。
    check("削除されたコメントの番号は欠番のまま残る", seqs == [2, 3, 4], f"got={seqs}")

section("書き込み")
check_status("POST /threads", "POST", "/threads", 201, '{"title":"smoke test"}')
status, payload, _ = call("POST", "/threads/1/comments", '{"body":"smoke test"}')
check("POST /threads/1/comments が 201", status == 201, f"status={status}")
if status == 201:
    check("authorName が既定値になる", payload["authorName"] == "名無しさん",
          f"got={payload['authorName']}")

section("コメント投稿の並行制御")

# **ユニットテストでは絶対に検出できない領域。**
# フェイクのリポジトリは採番を競合させないため、
# 「同時に投稿したらレス番号がどうなるか」はここでしか分からない。
#
# 並列数は控えめにしてある。ここで測りたいのは性能ではなく正しさで、
# モード別のスループット比較は make concurrency-probe の担当。
CONCURRENT_POSTS = 12

status, thread, _ = call("POST", "/threads", '{"title":"同時投稿の検証"}')
if status != 201:
    check("並行投稿用のスレッドを作れる", False, f"status={status}")
else:
    tid = thread["id"]

    def _post(i: int):
        return call("POST", f"/threads/{tid}/comments",
                    json.dumps({"body": f"同時投稿 {i}"}))

    with ThreadPoolExecutor(max_workers=CONCURRENT_POSTS) as pool:
        results = list(pool.map(_post, range(CONCURRENT_POSTS)))

    created = [p for s, p, _ in results if s == 201]
    codes = sorted(s for s, _, _ in results)
    # seq が欠けていても集計を止めない (上の一覧の検査と同じ理由)。
    seqs = sorted(c["seq"] for c in created if "seq" in c)

    check(f"{CONCURRENT_POSTS} 件の同時投稿がすべて 201",
          len(created) == CONCURRENT_POSTS, f"status={codes}")
    check("投稿の応答すべてに seq がある",
          len(seqs) == len(created), f"{len(seqs)} / {len(created)} 件にしか入っていない")

    # **ここが Phase 2 の本体。** 重複したら一意制約が拒否するので、
    # 実際には「重複した seq が保存される」ことは起きない。
    # 起きるのは投稿の失敗なので、上の 201 の数と合わせて意味を持つ。
    check("レス番号が重複しない", len(set(seqs)) == len(seqs), f"seqs={seqs}")

    # 成功したぶんは必ず 1..N の連番になる。
    # 採番が「読み直していない」なら、ここに飛びが出る。
    check("レス番号が 1 から始まる連番になる",
          seqs == list(range(1, len(created) + 1)), f"seqs={seqs}")

    if SQL_EXEC:
        # 正しさの根拠を API の応答ではなく DB に置く。
        #
        # **件数ではなく合言葉を返させる。** psql の既定は整形済みの表
        # (罫線と "(1 row)" つき) なので、"0" を探す形だと
        # 罫線の一部や別の桁に当たって、壊れていても通りうる。
        dup = subprocess.run(
            shlex.split(SQL_EXEC) + [
                "SELECT CASE WHEN count(*) = 0 THEN 'NO_DUPLICATE_SEQ' "
                "ELSE 'DUPLICATE_SEQ_FOUND' END FROM ("
                f"SELECT seq FROM comments WHERE thread_id = {tid} "
                "GROUP BY seq HAVING count(*) > 1) d;"],
            check=True, capture_output=True, text=True).stdout
        check("DB 側にも重複した (thread_id, seq) が無い",
              "NO_DUPLICATE_SEQ" in dup, f"got={dup.strip()!r}")

section("仕様書によるリクエスト検証")
for label, method, path, body in [
    ("threadId が 0 (minimum 違反)", "GET", "/threads/0", None),
    ("threadId が非数値", "GET", "/threads/abc", None),
    ("size が上限超過", "GET", "/threads?size=101", None),
    ("size が 0", "GET", "/threads?size=0", None),
    # 前 2 つは仕様書 (pattern / maxLength) が、
    # 最後の 1 つはハンドラ側の復号が弾く。
    ("cursor に使えない文字", "GET", "/threads?cursor=abc.def", None),
    ("cursor が長すぎる", "GET", "/threads?cursor=" + "A" * 257, None),
    ("cursor が復号できない", "GET", "/threads?cursor=notAToken", None),
    ("title が空", "POST", "/threads", '{"title":""}'),
    ("title 未指定", "POST", "/threads", "{}"),
    ("title が型違い", "POST", "/threads", '{"title":123}'),
    ("title が長すぎる", "POST", "/threads", json.dumps({"title": "あ" * 201})),
    ("body が長すぎる", "POST", "/threads/1/comments", json.dumps({"body": "あ" * 2001})),
    ("body が空白のみ (ドメイン層で検出)", "POST", "/threads/1/comments", '{"body":"   "}'),
]:
    check_status(label, method, path, 400, body, want_code="INVALID_ARGUMENT")

check_status("上限ちょうどの size は通る", "GET", "/threads?size=100", 200)

section("認証")
# **資格情報の有無で期待が変わるので、まず状態を判定する。**
# CI は置いていないので 503、手元に .env を置くと 302 になる。
# 決め打ちにすると、資格情報を入れた環境でスモークが落ちる (実際に踏んだ)。
#
# **分岐するのはこの節だけ。** セッションを要する検査は分岐しない ——
# 設定が無くても発行済みセッションは解決されるため
# (docs/adr/0005-authentication.md 決定 4)。
auth_status, auth_payload, auth_headers = call("GET", "/auth/google")

if auth_status == 503:
    # 認証だけが使えない状態。全体が落ちる設計だと、
    # ここで掲示板の検証がすべて巻き添えになる。
    code = (auth_payload or {}).get("error", {}).get("code")
    check("認証が未設定なら /auth/google は 503 UNAVAILABLE",
          code == "UNAVAILABLE", f"status={auth_status} code={code}")
else:
    # 資格情報が入っている。Google の認可エンドポイントへ送ること。
    location = auth_headers.get("Location", "")
    check("認証が設定済みなら /auth/google は 302",
          auth_status == 302, f"status={auth_status}")
    check("リダイレクト先が Google の認可エンドポイント",
          location.startswith("https://accounts.google.com/"), f"Location={location[:80]}")
    # state / nonce / PKCE を持ち回るための Cookie が発行されること。
    cookies = auth_headers.get_all("Set-Cookie") or []
    joined = " | ".join(cookies)
    for name in ("oauth_state", "oauth_nonce", "oauth_verifier"):
        check(f"{name} が発行される", name in joined, f"Set-Cookie={joined[:200]}")

# 仕様書の security 宣言が 401 として強制されること。
# gin-middleware は security 違反を 400 に丸めるため、
# respondSpecError で振り分け直していないとここが 400 になる。
check_status("未ログインで /me は 401", "GET", "/me", 401, want_code="UNAUTHENTICATED")
check_status("未ログインで POST /auth/logout は 401", "POST", "/auth/logout", 401,
             want_code="UNAUTHENTICATED")

# 匿名投稿は認証必須にならないこと (OpenAPI の security は「必須」しか書けないため、
# 投稿系には宣言していない)。ここが 401 になると匿名投稿ができなくなる。
status, _, _ = call("POST", "/threads", '{"title":"匿名でも書ける"}')
check("匿名のスレッド作成は 401 にならない", status == 201, f"status={status}")

section("ルーティング")
check_status("仕様書に無いパスは 404", "GET", "/threads/1/likes", 404, want_code="NOT_FOUND")
check_status("未定義メソッドは 405", "DELETE", "/threads/1", 405,
             want_code="METHOD_NOT_ALLOWED")

section("CSRF (ADR 0013 決定 1)")

# **CORS では CSRF を防げない。** CORS はブラウザにレスポンスを読ませない
# 仕組みで、リクエスト自体は飛ぶ。ここで見るのはサーバ側の拒否。
_s, _p, _ = call("POST", "/threads", json.dumps({"title": "CSRF"}), origin="")
check("Origin も Referer も無い POST は 403", _s == 403, f"status={_s}")
check("403 のコードは PERMISSION_DENIED",
      isinstance(_p, dict) and _p.get("error", {}).get("code") == "PERMISSION_DENIED",
      f"body={_p}")

_s, _, _ = call("POST", "/threads", json.dumps({"title": "CSRF"}),
                origin="https://evil.test")
check("知らない Origin からの POST は 403", _s == 403, f"status={_s}")

# **前方一致で判定していないこと。** 許可値で始まる別ホストを弾く。
_s, _, _ = call("POST", "/threads", json.dumps({"title": "CSRF"}),
                origin=ORIGIN + ".evil.test")
check("許可値で始まる別オリジンは 403", _s == 403, f"status={_s}")

# Origin が無ければ Referer で照合する。
_s, _, _ = call("POST", "/threads", json.dumps({"title": "CSRF referer"}),
                headers={"Referer": ORIGIN + "/threads"}, origin="")
check("許可オリジンの Referer なら通る", _s == 201, f"status={_s}")

_s, _, _ = call("POST", "/threads", json.dumps({"title": "CSRF referer"}),
                headers={"Referer": "https://evil.test/threads"}, origin="")
check("知らない Referer は 403", _s == 403, f"status={_s}")

# **GET は素通しすること** (前提: GET に副作用を持たせない)。
_s, _, _ = call("GET", "/threads", origin="")
check("GET は Origin が無くても通る", _s == 200, f"status={_s}")

# **multipart はプリフライトが起きない = CORS が効かない経路。**
# csrfGuard だけが防御になるので、ここは必ず検査する。
_s, _p, _ = call("POST", "/images", None, origin="")
check("画像アップロードも Origin 無しは 403", _s == 403, f"status={_s}")

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
    skip("論理削除されたスレッドの扱い", "SQL_EXEC が未設定")

if SQL_EXEC:
    section("投稿者の紐付け (ADR 0005 決定 2 / ADR 0014)")

    # 認証は資格情報が無いので使えない。HTTP でログインできないため、
    # 投稿者つきの行は SQL で作る。**検証したいのは LEFT JOIN と CTE が
    # 実 DB で意図どおり動くか**であり、そこは HTTP 越しに読める。
    # **投入も try の中に入れる。** 外に置くと、投入が失敗した時点で
    # finally に到達せず 900001 の行が残る。しかも check() の FAIL ではなく
    # トレースバックでスモーク全体が異常終了するため、
    # 「何件中何件が失敗したか」の集計にも乗らない。
    try:
        sql("""
            INSERT INTO users (id, public_id, google_sub, email, display_name, avatar_url)
            VALUES (900001, '01920000-0000-7000-8000-000000900001'::uuid,
                    'smoke-sub-1', 'smoke@example.com', 'スモークの人',
                    'https://example.com/a.png')
            ON CONFLICT (google_sub) DO NOTHING;
        """)
        sql("""
            INSERT INTO threads (id, title, author_id)
            VALUES (900001, 'スモーク: 投稿者つきスレッド', 900001)
            ON CONFLICT (id) DO UPDATE SET deleted_at = NULL, author_id = 900001;
        """)
        sql("""
            INSERT INTO comments (thread_id, seq, author_name, body, author_id)
            VALUES (900001, 1, '名無しさん', 'スモーク: 投稿者つきコメント', 900001)
            ON CONFLICT DO NOTHING;
        """)

        status, payload, _ = call("GET", "/threads/900001")
        author = payload.get("author")
        check("詳細で投稿者が解決される", author is not None, f"payload={payload}")
        if author:
            check("表示名が users から引かれる",
                  author["displayName"] == "スモークの人", f"author={author}")
            check("公開 ID が返る (内部 ID ではない)",
                  author.get("publicId", "").startswith("01920000"), f"author={author}")
            check("在籍中は withdrawn=false", author["withdrawn"] is False, f"author={author}")
        # 内部 ID (users.id) を漏らさないこと。
        # author に id が生えたり、authorId がそのまま出ていないかを見る。
        check("投稿者の内部 ID が応答に含まれない",
              "authorId" not in json.dumps(payload) and (author is None or "id" not in author),
              f"payload={payload}")

        # 添字で取り出す前に形を確かめる。
        # エラー応答や 0 件のときに KeyError で飛ぶと、
        # FAIL として記録されないまま全体が止まる。
        status, payload, _ = call("GET", "/threads/900001/comments")
        comments = payload.get("comments") or []
        c = comments[0] if comments else None
        check("コメントでも投稿者が解決される",
              c is not None and c.get("author") is not None
              and c["author"]["displayName"] == "スモークの人",
              f"payload={payload}")

        # 匿名投稿が LEFT JOIN で消えないこと。
        # INNER にすると、ここで一覧から丸ごと落ちる。
        status, payload, _ = call("GET", "/threads")
        anon = [t for t in payload["threads"] if t["author"] is None]
        check("匿名スレッドが一覧から消えない (LEFT であること)",
              len(anon) > 0, f"threads={[t['id'] for t in payload['threads']]}")

        # 退会させると表示が差し替わる。
        sql("UPDATE users SET deleted_at = now() WHERE id = 900001;")
        status, payload, _ = call("GET", "/threads/900001")
        author = payload.get("author")
        check("退会後は表示名が差し替わる",
              author is not None and author["displayName"] == "退会したユーザー",
              f"author={author}")
        check("退会後は公開 ID を返さない",
              author is not None and author.get("publicId") is None, f"author={author}")
        check("退会後もアバターを返さない",
              author is not None and author.get("avatarUrl") is None, f"author={author}")
        check("退会しても投稿自体は残る", status == 200, f"status={status}")
    finally:
        sql("DELETE FROM comments WHERE thread_id = 900001;")
        sql("DELETE FROM threads WHERE id = 900001;")
        sql("DELETE FROM users WHERE id = 900001;")
else:
    section("投稿者の紐付け")
    skip("投稿者の紐付け", "SQL_EXEC が未設定")

if SQL_EXEC:
    section("権限 (ADR 0011 決定 1)")

    # 認証を通さずにロールの読み出し経路を確かめる。
    # セッションは ID がトークンの SHA-256 なので、こちらで計算して挿入する
    # (生のトークンは DB に保存しない設計。docs/adr/0005-authentication.md)。
    # **トークンは実行ごとに作る。** 固定値をコミットすると、
    # 検査の間だけとはいえ「公開された有効なセッション」が存在することになる。
    # 前回の実行が後片付け前に落ちた場合に、古い行へ衝突する事故も避けられる。
    probe_token = "smoke-role-" + secrets.token_hex(16)
    probe_hash = hashlib.sha256(probe_token.encode()).hexdigest()

    try:
        sql("""
            INSERT INTO users (id, public_id, google_sub, email, display_name)
            VALUES (900002, '01920000-0000-7000-8000-000000900002'::uuid,
                    'smoke-sub-2', 'smoke-role@example.com', 'ロール検証')
            ON CONFLICT (google_sub) DO NOTHING;
        """)
        sql(f"""
            INSERT INTO sessions (id, user_id, expires_at)
            VALUES ('{probe_hash}', 900002, now() + interval '5 minutes');
        """)
        cookie = {"Cookie": f"session={probe_token}"}

        # **セッションを要する検査を、認証の設定で分岐させない。**
        # 以前はここが auth_enabled で囲まれており、資格情報を置いていない
        # CI では丸ごと SKIP されていた。セッションの検証は sessions を
        # 引くだけで Google を必要としないので、切り離した
        # (docs/adr/0005-authentication.md 決定 4 / ADR 0003 未決 #16)。
        status, payload, _ = call("GET", "/me", headers=cookie)
        check("ログイン中は /me に role が載る",
              status == 200 and (payload or {}).get("role") == "user",
              f"status={status} payload={payload}")

        # **昇格が読み出し側に反映されること。**
        # ここが繋がっていないと、権限を変えても API から見えない。
        sql("UPDATE users SET role = 'moderator' WHERE id = 900002;")
        _, payload, _ = call("GET", "/me", headers=cookie)
        check("ロールを変えると /me に反映される",
              (payload or {}).get("role") == "moderator", f"payload={payload}")

        # **ログアウトは OIDC の設定を要求しない。**
        # 発行済みセッションを捨てるだけで IdP には触れないため。
        # ここが 503 になる実装だと、資格情報の無い環境に
        # 破棄できないセッションが残る。
        status, _, _ = call("POST", "/auth/logout", headers=cookie)
        check("ログアウトは認証が未設定でも 204", status == 204, f"status={status}")
        status, _, _ = call("GET", "/me", headers=cookie)
        check("ログアウト後のセッションは無効", status == 401, f"status={status}")

        # **他人のロールは投稿一覧に出さない。**
        # 誰がモデレーターかを晒す必要がない (Author に role は無い)。
        sql("""
            INSERT INTO threads (id, title, author_id)
            VALUES (900002, 'スモーク: モデレーターの投稿', 900002)
            ON CONFLICT (id) DO UPDATE SET deleted_at = NULL, author_id = 900002;
        """)
        _, payload, _ = call("GET", "/threads/900002")
        author = (payload or {}).get("author")
        # **先に「投稿者が解決できていること」を確かめる。**
        # 空の辞書へ倒すと、404 などで author が無いときに
        # "role" not in {} が真になり、素通しで OK になる。
        # この検査が唯一守ろうとしている「role が Author に漏れる」を、
        # フィクスチャが壊れているときに限って見逃すことになる。
        check("モデレーターの投稿で author が解決される",
              author is not None, f"payload={payload}")
        check("投稿一覧の author に role を含めない",
              author is not None and "role" not in author, f"author={author}")

        # DB 側の CHECK 制約が効いていること。
        # アプリと DB のどちらか片方だけ値を増やすと、ここで気づける。
        try:
            sql("UPDATE users SET role = 'superadmin' WHERE id = 900002;", quiet=True)
            check("不正なロールを DB が拒否する", False, "CHECK 制約が効いていない")
        except subprocess.CalledProcessError:
            check("不正なロールを DB が拒否する", True)
    finally:
        sql("DELETE FROM threads WHERE id = 900002;")
        sql("DELETE FROM sessions WHERE user_id = 900002;")
        sql("DELETE FROM users WHERE id = 900002;")
else:
    section("権限")
    skip("権限", "SQL_EXEC が未設定")

# ---------------------------------------------------------------------------
# 冪等キー (docs/adr/0015-idempotency.md)
# ---------------------------------------------------------------------------
#
# **ユニットテストでは検証できない領域。**
# キーの確保・投稿・応答の記録を 1 トランザクションに同居させる (決定 3) 部分は、
# フェイクのリポジトリでは再現できない。ON CONFLICT DO NOTHING が
# 未コミットの行を待つ挙動も、実 DB でしか出ない。

if SQL_EXEC:
    section("冪等キー (ADR 0015)")

    idem_token = "smoke-idem-" + secrets.token_hex(16)
    idem_hash = hashlib.sha256(idem_token.encode()).hexdigest()
    # 名前空間の検証用にもう 1 人。同じキーを使っても衝突しないこと。
    other_token = "smoke-idem2-" + secrets.token_hex(16)
    other_hash = hashlib.sha256(other_token.encode()).hexdigest()

    try:
        for uid, sub, sess in [
            (900010, "smoke-sub-idem", idem_hash),
            (900011, "smoke-sub-idem2", other_hash),
        ]:
            sql(f"""
                INSERT INTO users (id, public_id, google_sub, email, display_name)
                VALUES ({uid},
                        '01920000-0000-7000-8000-000000{uid}'::uuid,
                        '{sub}', '{sub}@example.com', '冪等検証')
                ON CONFLICT (google_sub) DO NOTHING;
            """)
            sql(f"""
                INSERT INTO sessions (id, user_id, expires_at)
                VALUES ('{sess}', {uid}, now() + interval '5 minutes');
            """)

        me = {"Cookie": f"session={idem_token}"}
        other = {"Cookie": f"session={other_token}"}

        status, thread, _ = call("POST", "/threads", '{"title":"冪等キーの検証"}', headers=me)
        if status != 201:
            check("検証用スレッドを作れる", False, f"status={status}")
            # **失敗 1 件では、残り 10 件が消えたことが分からない。**
            # 前提が崩れた形の未実行なので、スキップとしても記録する。
            skip("冪等キー", f"検証用スレッドを作れなかった (status={status})")
        else:
            tid = thread["id"]
            key = "smoke-key-" + secrets.token_hex(8)
            headers = dict(me, **{"Idempotency-Key": key})
            body = '{"body":"二重送信の検証"}'

            s1, first, _ = call("POST", f"/threads/{tid}/comments", body, headers=headers)
            s2, second, _ = call("POST", f"/threads/{tid}/comments", body, headers=headers)

            check("同じキーで 2 回送っても両方 201", s1 == 201 and s2 == 201, f"status={s1}, {s2}")
            # **ここが主題。** 記録した応答をそのまま返すので、
            # レス番号まで含めて一致する。
            check("再送で同じ応答が返る", first == second, f"1: {first}\n2: {second}")

            status, listed, _ = call("GET", f"/threads/{tid}/comments")
            n = len(listed["comments"]) if status == 200 else -1
            check("投稿は 1 件しか作られない", n == 1, f"件数={n}")

            # 記録が主トランザクションと同時にコミットされていること。
            # completed_at が NULL のまま残ると、次の再送が永久に待つ側に倒れる。
            recorded = subprocess.run(
                shlex.split(SQL_EXEC) + [
                    "SELECT CASE WHEN count(*) = 1 THEN 'RECORDED' ELSE 'MISSING' END "
                    f"FROM idempotency_keys WHERE user_id = 900010 AND key = '{key}' "
                    "AND completed_at IS NOT NULL AND response_status = 201 "
                    "AND response_body IS NOT NULL;"],
                check=True, capture_output=True, text=True).stdout
            check("応答が記録されている (completed_at と response_body が入る)",
                  "RECORDED" in recorded, f"got={recorded.strip()!r}")

            # **結果に影響しない差で 422 にしない。**
            # 指紋を「受け取ったままの値」から作ると、ここが落ちる。
            # ログイン中は authorName が捨てられ、body は TrimSpace される
            # ので、どちらも投稿結果を変えない。
            # 422 は再試行では解けないので、誤検出の害が大きい。
            norm_key = "smoke-norm-" + secrets.token_hex(8)
            norm_headers = dict(me, **{"Idempotency-Key": norm_key})
            n1, norm_first, _ = call("POST", f"/threads/{tid}/comments",
                                     '{"authorName":"ホシノ","body":"正規化の検証"}',
                                     headers=norm_headers)
            # 再送では名前欄が空になり、本文の前後に空白が付いている。
            n2, norm_second, _ = call("POST", f"/threads/{tid}/comments",
                                      '{"body":"  正規化の検証  "}', headers=norm_headers)
            check("結果に影響しない差 (名前欄・前後の空白) では 422 にならない",
                  n1 == 201 and n2 == 201, f"status={n1}, {n2}")
            if n1 == 201 and n2 == 201:
                check("その再送でも同じ応答が返る", norm_first == norm_second,
                      f"1: {norm_first}\n2: {norm_second}")

            # 同じキーで別の内容 -> 422。黙って前回の結果を返さない。
            s3, err3, _ = call("POST", f"/threads/{tid}/comments",
                               '{"body":"別の内容"}', headers=headers)
            code3 = err3["error"]["code"] if isinstance(err3, dict) and "error" in err3 else ""
            check("同じキーで別の内容は 422", s3 == 422 and code3 == "FAILED_PRECONDITION",
                  f"status={s3} code={code3}")

            # **キーはユーザーごとに名前空間が分かれる。**
            # 分かれていないと、他人の投稿結果が返るという最悪の事故になる。
            s4, mine, _ = call("POST", f"/threads/{tid}/comments",
                               '{"body":"別の人の投稿"}',
                               headers=dict(other, **{"Idempotency-Key": key}))
            check("別の利用者が同じキーを使っても衝突しない", s4 == 201, f"status={s4}")
            if s4 == 201:
                check("他人の投稿結果が返らない",
                      mine.get("body") == "別の人の投稿", f"got={mine.get('body')!r}")

            # 匿名ではヘッダを無視する (決定 4)。記録も残らない。
            anon_key = "smoke-anon-" + secrets.token_hex(8)
            s5, _, _ = call("POST", f"/threads/{tid}/comments", '{"body":"匿名の投稿"}',
                            headers={"Idempotency-Key": anon_key})
            check("匿名でもヘッダ付きで投稿できる (無視される)", s5 == 201, f"status={s5}")

            ignored = subprocess.run(
                shlex.split(SQL_EXEC) + [
                    "SELECT CASE WHEN count(*) = 0 THEN 'NOT_RECORDED' ELSE 'RECORDED' END "
                    f"FROM idempotency_keys WHERE key = '{anon_key}';"],
                check=True, capture_output=True, text=True).stdout
            check("匿名の投稿はキーを記録しない", "NOT_RECORDED" in ignored,
                  f"got={ignored.strip()!r}")

            sql(f"DELETE FROM threads WHERE id = {tid};")
    finally:
        sql("DELETE FROM idempotency_keys WHERE user_id IN (900010, 900011);")
        sql("DELETE FROM threads WHERE author_id IN (900010, 900011);")
        sql("DELETE FROM sessions WHERE user_id IN (900010, 900011);")
        sql("DELETE FROM users WHERE id IN (900010, 900011);")
else:
    section("冪等キー (ADR 0015)")
    # **以前はここが CI で必ずスキップされていた。**
    # 資格情報が無いと resolveSession がセッションを解決せず、
    # 常に匿名として扱われていたため (匿名は冪等キーの対象外・決定 4)。
    # セッションの解決を OIDC の設定から切り離したので、
    # 残る条件は SQL_EXEC だけになった
    # (docs/adr/0005-authentication.md 決定 4)。
    skip("冪等キー", "SQL_EXEC が未設定")


# ---------------------------------------------------------------------------
# 画像 (docs/adr/0007-image-storage.md)
# ---------------------------------------------------------------------------
#
# **ユニットテストのフェイクでは確かめられない領域。**
#   - DB とストレージ 2 システムの整合 (pending -> PUT -> committed)
#   - 実際に保存されたバイト列が配信 URL から読めること
#   - 添付した画像がコメント一覧に載ること

if SQL_EXEC:
    section("画像 (ADR 0007)")

    img_token = "smoke-img-" + secrets.token_hex(16)
    img_hash = hashlib.sha256(img_token.encode()).hexdigest()
    other_img_token = "smoke-img2-" + secrets.token_hex(16)
    other_img_hash = hashlib.sha256(other_img_token.encode()).hexdigest()

    # 32x32 の PNG を最小限の依存で作る (Pillow を入れない)。
    # zlib と struct だけで組める。
    def _png(width: int, height: int, rgb: tuple = (60, 120, 200)) -> bytes:
        import struct
        import zlib

        raw = b""
        for _ in range(height):
            # 各行の先頭にフィルタ種別 (0 = None) が要る。
            raw += b"\x00" + bytes(rgb) * width
        def chunk(tag: bytes, data: bytes) -> bytes:
            return (struct.pack(">I", len(data)) + tag + data
                    + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))
        ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
        return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr)
                + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))

    def _multipart(kind: str, body: bytes) -> tuple:
        """multipart/form-data の本文と Content-Type を組み立てる。"""
        boundary = "----smoke" + secrets.token_hex(8)
        parts = []
        if kind is not None:
            parts.append(
                f"--{boundary}\r\n"
                f'Content-Disposition: form-data; name="kind"\r\n\r\n{kind}\r\n'.encode())
        if body is not None:
            parts.append(
                f"--{boundary}\r\n"
                f'Content-Disposition: form-data; name="file"; filename="a.png"\r\n'
                f"Content-Type: image/png\r\n\r\n".encode() + body + b"\r\n")
        parts.append(f"--{boundary}--\r\n".encode())
        return b"".join(parts), f"multipart/form-data; boundary={boundary}"

    def upload(kind, body, cookie_token):
        """画像をアップロードする。urllib を直接使う (call は JSON 専用)。"""
        payload, content_type = _multipart(kind, body)
        req = urllib.request.Request(BASE_URL + "/images", method="POST")
        req.add_header("Content-Type", content_type)
        # multipart/form-data は simple request なのでプリフライトが起きない。
        # **CORS が効かない経路**なので、csrfGuard だけが防御になる (ADR 0013 決定 1)。
        req.add_header("Origin", ORIGIN)
        if cookie_token:
            req.add_header("Cookie", f"session={cookie_token}")
        try:
            with _opener.open(req, payload, timeout=30) as res:
                return res.status, _decode(res.read())
        except urllib.error.HTTPError as e:
            return e.code, _decode(e.read())

    try:
        for uid, sub, sess in [
            (900020, "smoke-sub-img", img_hash),
            (900021, "smoke-sub-img2", other_img_hash),
        ]:
            sql(f"""
                INSERT INTO users (id, public_id, google_sub, email, display_name)
                VALUES ({uid},
                        '01920000-0000-7000-8000-000000{uid}'::uuid,
                        '{sub}', '{sub}@example.com', '画像検証')
                ON CONFLICT (google_sub) DO NOTHING;
            """)
            sql(f"""
                INSERT INTO sessions (id, user_id, expires_at)
                VALUES ('{sess}', {uid}, now() + interval '5 minutes');
            """)

        # ストレージが設定されていない環境では画像だけが 503 になる。
        probe_status, probe_body = upload("comment_attachment", _png(8, 8), img_token)
        storage_enabled = probe_status != 503

        if not storage_enabled:
            code = (probe_body or {}).get("error", {}).get("code")
            check("ストレージが未設定なら POST /images は 503 UNAVAILABLE",
                  code == "UNAVAILABLE", f"status={probe_status} code={code}")
            skip("画像", "ストレージが未設定")
        else:
            check("画像をアップロードできる", probe_status == 201, f"status={probe_status}")

            status, uploaded = upload("comment_attachment", _png(64, 48), img_token)
            check("アップロードが 201 を返す", status == 201, f"status={status} body={uploaded}")

            if status == 201:
                image_id = uploaded["id"]
                # **絶対 URL を返す** (決定 5)。
                check("絶対 URL が返る",
                      isinstance(uploaded.get("url"), str)
                      and uploaded["url"].startswith("http"),
                      f"url={uploaded.get('url')!r}")
                # **オブジェクトキーを外に出さない。**
                check("応答にオブジェクトキーが含まれない",
                      "objectKey" not in json.dumps(uploaded), f"body={uploaded}")

                # **DB の状態が committed であること** (決定 3 の手順 3)。
                # pending のまま残っていたら、確定の UPDATE が効いていない。
                out = subprocess.run(
                    shlex.split(SQL_EXEC) + [
                        f"SELECT status || ':' || content_type FROM images "
                        f"WHERE id = '{image_id}'::uuid;"],
                    check=True, capture_output=True, text=True).stdout
                check("DB では committed になっている", "committed" in out,
                      f"got={out.strip()!r}")
                # **コメント添付は JPEG で保存される** (決定 6)。
                # PNG を送っても JPEG が返るのが再エンコードの証拠になる。
                check("コメント添付は JPEG に再エンコードされる", "image/jpeg" in out,
                      f"got={out.strip()!r}")

                # 配信 URL から実際に読めること。
                try:
                    with urllib.request.urlopen(uploaded["url"], timeout=10) as res:  # noqa: S310
                        fetched = res.read()
                        fetched_type = res.headers.get("Content-Type", "")
                    check("配信 URL から実体を取得できる", len(fetched) > 0,
                          f"{len(fetched)} バイト")
                    check("配信時の Content-Type が image/jpeg",
                          fetched_type == "image/jpeg", f"got={fetched_type!r}")
                    # **入力と別のバイト列** (再エンコードされている)。
                    check("保存されたのは入力そのものではない",
                          not fetched.startswith(b"\x89PNG"), "PNG のまま保存されている")
                except Exception as e:  # noqa: BLE001
                    check("配信 URL から実体を取得できる", False, f"{e}")

                # コメントへ添付する。
                s, thread = call("POST", "/threads", '{"title":"画像つきの検証"}',
                                 headers={"Cookie": f"session={img_token}"})[:2]
                tid = thread["id"] if s == 201 and isinstance(thread, dict) else None
                if tid is None:
                    check("検証用スレッドを作れる", False, f"status={s}")
                    skip("画像", "検証用スレッドを作れなかった")
                else:
                    s, posted, _ = call(
                        "POST", f"/threads/{tid}/comments",
                        json.dumps({"body": "画像つき", "imageId": image_id}),
                        headers={"Cookie": f"session={img_token}"})
                    check("画像つきで投稿できる", s == 201, f"status={s} body={posted}")
                    check("応答に画像が載る",
                          isinstance(posted, dict) and (posted.get("image") or {}).get("id") == image_id,
                          f"image={posted.get('image') if isinstance(posted, dict) else posted}")

                    # **一覧でも画像が解決される** (LEFT JOIN が効いていること)。
                    #
                    # **listed が dict とは限らない。** call() は本文を
                    # JSON として読めなければ None を返す (502 や HTML が
                    # 返った場合)。添字で触るとトレースバックで全体が止まり、
                    # 集計にも乗らない —— _decode の docstring が
                    # 警告しているのと同じ形になる。
                    _, listed, _ = call("GET", f"/threads/{tid}/comments")
                    comments = listed.get("comments") if isinstance(listed, dict) else None
                    first = comments[0] if comments else None
                    check("一覧でも画像が解決される",
                          first is not None and (first.get("image") or {}).get("id") == image_id,
                          f"comment={first}")

                    # **画像なしの投稿が一覧から消えないこと** (LEFT であること)。
                    call("POST", f"/threads/{tid}/comments", '{"body":"画像なし"}')
                    _, listed, _ = call("GET", f"/threads/{tid}/comments")
                    comments = listed.get("comments") if isinstance(listed, dict) else None
                    no_image = [c for c in (comments or []) if c.get("image") is None]
                    check("画像なしのコメントが一覧から消えない (LEFT であること)",
                          len(no_image) > 0, f"comments={comments}")

                    # **他人の画像は添付できない (404)。**
                    # 403 にすると「その ID が存在すること」が漏れる。
                    s, err, _ = call(
                        "POST", f"/threads/{tid}/comments",
                        json.dumps({"body": "他人の画像", "imageId": image_id}),
                        headers={"Cookie": f"session={other_img_token}"})
                    code = err["error"]["code"] if isinstance(err, dict) and "error" in err else ""
                    check("他人の画像を添付すると 404", s == 404 and code == "NOT_FOUND",
                          f"status={s} code={code}")

                    # **匿名は画像を添付できない。**
                    s, _, _ = call("POST", f"/threads/{tid}/comments",
                                   json.dumps({"body": "匿名で画像", "imageId": image_id}))
                    check("匿名で画像を添付すると 401", s == 401, f"status={s}")

                    sql(f"DELETE FROM comments WHERE thread_id = {tid};")
                    sql(f"DELETE FROM threads WHERE id = {tid};")

            # 未ログインは 401。
            s, _ = upload("comment_attachment", _png(8, 8), None)
            check("未ログインのアップロードは 401", s == 401, f"status={s}")

            # SVG は受け付けない (決定 2)。
            s, _ = upload("comment_attachment",
                          b'<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>', img_token)
            check("SVG は 400 で拒否される", s == 400, f"status={s}")

            # 未知の用途は拒否する。
            s, _ = upload("banner", _png(8, 8), img_token)
            check("未知の kind は 400", s == 400, f"status={s}")

            # --- プロフィール画像 (ADR 0007) ---
            s, avatar = upload("avatar", _png(64, 64), img_token)
            check("アバターをアップロードできる", s == 201, f"status={s} body={avatar}")
            if s == 201:
                s, me, _ = call("PUT", "/me/avatar",
                                json.dumps({"imageId": avatar["id"]}),
                                headers={"Cookie": f"session={img_token}"})
                check("プロフィール画像を設定できる", s == 200, f"status={s} body={me}")
                # **アップロードした画像が /me に出ること。**
                # Google の画像より優先する (ADR 0007 のスキーマ)。
                check("設定した画像が /me の avatarUrl に出る",
                      isinstance(me, dict) and (me.get("avatarUrl") or "") == avatar["url"],
                      f"avatarUrl={me.get('avatarUrl') if isinstance(me, dict) else me}")

                # **他人の画像は 404。** 存在を隠すため。
                s, err, _ = call("PUT", "/me/avatar",
                                 json.dumps({"imageId": avatar["id"]}),
                                 headers={"Cookie": f"session={other_img_token}"})
                code = err["error"]["code"] if isinstance(err, dict) and "error" in err else ""
                check("他人の画像はアバターにできない (404)",
                      s == 404 and code == "NOT_FOUND", f"status={s} code={code}")

                # null で解除できること。
                s, me, _ = call("PUT", "/me/avatar", '{"imageId":null}',
                                headers={"Cookie": f"session={img_token}"})
                check("null で解除できる",
                      s == 200 and isinstance(me, dict) and me.get("avatarUrl") != avatar["url"],
                      f"status={s} avatarUrl={me.get('avatarUrl') if isinstance(me, dict) else me}")

            # --- スレッドアイコン (ADR 0007) ---
            s, icon = upload("thread_icon", _png(48, 48), img_token)
            check("スレッドアイコンをアップロードできる", s == 201, f"status={s}")
            if s == 201:
                s, th, _ = call("POST", "/threads",
                                json.dumps({"title": "アイコンつきスレッド",
                                            "iconImageId": icon["id"]}),
                                headers={"Cookie": f"session={img_token}"})
                check("アイコンつきでスレッドを作れる", s == 201, f"status={s} body={th}")
                check("応答にアイコンが載る",
                      isinstance(th, dict) and (th.get("icon") or {}).get("id") == icon["id"],
                      f"icon={th.get('icon') if isinstance(th, dict) else th}")

                if s == 201:
                    icon_tid = th["id"]
                    # **一覧でも解決されること** (LEFT JOIN が効いている)。
                    _, listed, _ = call("GET", f"/threads/{icon_tid}")
                    check("詳細でもアイコンが解決される",
                          isinstance(listed, dict) and (listed.get("icon") or {}).get("id") == icon["id"],
                          f"thread={listed}")
                    sql(f"DELETE FROM threads WHERE id = {icon_tid};")

                # 匿名はアイコンを設定できない。
                s, _, _ = call("POST", "/threads",
                               json.dumps({"title": "匿名でアイコン",
                                           "iconImageId": icon["id"]}))
                check("匿名はアイコンを設定できない (401)", s == 401, f"status={s}")

                # **用途が違う画像は使えない** (ADR 0007 決定 6)。
                # 形式と寸法が用途で変わるので、取り違えると
                # 仕様書の説明と実装が食い違う。404 にするのは存在を隠すため。
                s, err, _ = call("PUT", "/me/avatar",
                                 json.dumps({"imageId": icon["id"]}),
                                 headers={"Cookie": f"session={img_token}"})
                code = err["error"]["code"] if isinstance(err, dict) and "error" in err else ""
                check("アイコン用の画像はアバターにできない (404)",
                      s == 404 and code == "NOT_FOUND", f"status={s} code={code}")

                s, _, _ = call("POST", f"/threads",
                               json.dumps({"title": "コメント用をアイコンに",
                                           "iconImageId": image_id}),
                               headers={"Cookie": f"session={img_token}"})
                check("コメント添付用の画像はアイコンにできない (404)", s == 404, f"status={s}")

            # --- 回収バッチ (ADR 0007 決定 3 / ADR 0016 問題 3) ---
            #
            # **status で扱いが分かれることを実 DB で確かめる。**
            # 定期処理は起動時にばらつかせて回るので、ここでは待たずに
            # SQL で「対象になるか」を検査する。
            s, orphan = upload("comment_attachment", _png(16, 16), img_token)
            if s == 201:
                # created_at を過去にして回収対象にする。
                sql(f"UPDATE images SET created_at = now() - interval '2 hours' "
                    f"WHERE id = '{orphan['id']}'::uuid;")
                # **件数ではなく合言葉を返させる。**
                # psql の既定は整形済みの表 (罫線と `(1 row)` つき) なので、
                # 行番号で切り出すと、オプションが変わった瞬間に
                # IndexError か誤検出になる。しかも != "0" 方向の検査は、
                # パースがずれると**壊れていても通る**側へ倒れる。
                # このファイルの他の DB 検査と同じ書き方に揃える。
                #
                # **対象の 1 件に絞る。** 絞らないと「DB のどこかに
                # 回収対象がある」しか見ないので、判定が壊れても
                # 古い行が 1 件あれば通ってしまう。
                #
                # **述語は db/query/images.sql の ListReclaimableImages と揃える。**
                # ここは手書きの写しなので、本体を直したら一緒に直すこと ——
                # 000007 で attached_at を足したとき、写しが古いまま
                # 通り続けていた (実測)。写しがずれると、
                # 「本体が壊れているのにスモークは緑」になる。
                reclaimable = f"""
                    SELECT CASE WHEN EXISTS (
                        SELECT 1 FROM images
                        WHERE id = '{orphan['id']}'::uuid
                          AND object_reclaimed_at IS NULL
                          AND (attached_at IS NULL OR status = 'deleted')
                          AND created_at < now() - interval '1 hour'
                          AND (status = 'deleted' OR (
                            NOT EXISTS (SELECT 1 FROM comments c WHERE c.image_id = images.id)
                            AND NOT EXISTS (SELECT 1 FROM users u WHERE u.avatar_image_id = images.id)
                            AND NOT EXISTS (SELECT 1 FROM threads t WHERE t.icon_image_id = images.id)))
                    ) THEN 'RECLAIMABLE' ELSE 'NOT_RECLAIMABLE' END;
                """
                out = subprocess.run(shlex.split(SQL_EXEC) + [reclaimable],
                                     check=True, capture_output=True, text=True).stdout
                check("添付されなかった画像が回収対象になる",
                      "NOT_RECLAIMABLE" not in out and "RECLAIMABLE" in out,
                      f"got={out.strip()!r}")

                # 添付すると対象から外れること。
                #
                # **attached_at を書かずに参照だけ張る。** 000007 で足した
                # 「索引で絞る列」が書き漏れた状態そのものになるので、
                # ここで効くのは NOT EXISTS 3 本の側だけになる。
                # 落ちたときは、安全網が外れたという意味。
                sql(f"UPDATE users SET avatar_image_id = '{orphan['id']}'::uuid WHERE id = 900020;")
                out = subprocess.run(shlex.split(SQL_EXEC) + [reclaimable],
                                     check=True, capture_output=True, text=True).stdout
                check("参照されている画像は回収対象にならない (NOT EXISTS の安全網)",
                      "NOT_RECLAIMABLE" in out, f"got={out.strip()!r}")
                sql("UPDATE users SET avatar_image_id = NULL WHERE id = 900020;")

            # **実際の API で添付すると attached_at が入ること** (000007)。
            #
            # 上の検査は SQL で参照を張っており、HTTP の経路を通っていない。
            # attached_at を書くのは添付する側のクエリなので、
            # **API を通さないと「書けているか」を一度も検査しないことになる。**
            s, attach_probe = upload("avatar", _png(48, 48), img_token)
            if s == 201:
                attached_sql = f"""
                    SELECT CASE WHEN attached_at IS NULL
                                THEN 'NOT_ATTACHED' ELSE 'ATTACHED' END
                    FROM images WHERE id = '{attach_probe['id']}'::uuid;
                """
                out = subprocess.run(shlex.split(SQL_EXEC) + [attached_sql],
                                     check=True, capture_output=True, text=True).stdout
                check("アップロード直後は未添付", "NOT_ATTACHED" in out, f"got={out.strip()!r}")

                s, _, _ = call("PUT", "/me/avatar",
                               json.dumps({"imageId": attach_probe["id"]}),
                               headers={"Cookie": f"session={img_token}"})
                out = subprocess.run(shlex.split(SQL_EXEC) + [attached_sql],
                                     check=True, capture_output=True, text=True).stdout
                check("API で設定すると attached_at が入る",
                      s == 200 and "NOT_ATTACHED" not in out and "ATTACHED" in out,
                      f"status={s} got={out.strip()!r}")

                # **解除すると戻ること。** 戻らないと、差し替えた画像が
                # どこからも参照されないまま永久に回収されない。
                s, _, _ = call("PUT", "/me/avatar", json.dumps({"imageId": None}),
                               headers={"Cookie": f"session={img_token}"})
                out = subprocess.run(shlex.split(SQL_EXEC) + [attached_sql],
                                     check=True, capture_output=True, text=True).stdout
                check("解除すると attached_at が NULL に戻る",
                      s == 200 and "NOT_ATTACHED" in out, f"status={s} got={out.strip()!r}")
    finally:
        # **参照を外すのが先。** images を先に消すと外部キー違反になり、
        # sql() は check=True なので finally からトレースバックが飛び、
        # 失敗集計が印字されないまま終わる。
        #
        # threads.icon_image_id が参照になったので、
        # **アイコン付きのスレッドは author_id では拾えない**
        # (匿名で作られていた場合)。images を参照する行を id で消す。
        sql("DELETE FROM comments WHERE image_id IN "
            "(SELECT id FROM images WHERE owner_id IN (900020, 900021));")
        sql("DELETE FROM threads WHERE icon_image_id IN "
            "(SELECT id FROM images WHERE owner_id IN (900020, 900021));")
        sql("DELETE FROM threads WHERE author_id IN (900020, 900021);")
        sql("UPDATE users SET avatar_image_id = NULL WHERE id IN (900020, 900021);")
        sql("DELETE FROM images WHERE owner_id IN (900020, 900021);")
        sql("DELETE FROM sessions WHERE user_id IN (900020, 900021);")
        sql("DELETE FROM users WHERE id IN (900020, 900021);")
else:
    section("画像 (ADR 0007)")
    skip("画像", "SQL_EXEC が未設定")

# ---------------------------------------------------------------------------

print()

# **スキップを先に報告する。** 失敗が 0 件でも、検証範囲が縮んでいれば
# 「何件通ったか」は意味を持たない。
if skipped:
    print(f"\033[33m{len(skipped)} 節を実行していません\033[0m")
    for s in skipped:
        print(f"  - {s}")

if failures:
    print(f"\033[31m{len(failures)} / {checks} 件が失敗しました\033[0m")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)

# **未実行の節を成功として扱わない。**
#
# ここが無かったせいで、認証まわりが丸ごと飛んだ状態の CI が
# 「74 件すべて成功しました」と表示して緑になっていた。
# 件数だけを見ていると、縮んだことに気づく手がかりが 1 つも無い。
if skipped and REQUIRE_FULL:
    print(f"\033[31m{checks} 件は通ったが、{len(skipped)} 節が未実行のため失敗とします\033[0m")
    print("  (意図的に一部だけ回すなら SMOKE_REQUIRE_FULL=0)")
    sys.exit(1)

print(f"\033[32m{checks} 件すべて成功しました\033[0m")
