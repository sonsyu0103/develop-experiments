#!/usr/bin/env python3
"""ログ基盤が「本番と同じ形」で動いているかを検査する。

docs/adr/0010-log-pipeline.md。**目視では気づけない壊れ方を捕まえるのが目的。**

ログ基盤は、壊れていても止まらない:

  - パーサが展開に失敗しても、fluent-bit はレコードを捨てず素通しする。
    S3 にはオブジェクトが増え続けるので、**見た目には動いている**
  - 気づくのは、Athena でクエリを書こうとした数日後になる。
    そのときには、展開されていないログが既に貯まっている

実際にこの形で壊れていた (「実装して分かったこと 1」)。
検査を置いておかないと、同じ壊れ方をもう一度する。
"""

from __future__ import annotations

import os
import sys
import time

import duckdb

from logs_source import (
    RAW_VIEW_NAME,
    VIEW_NAME,
    athena_columns,
    connect,
    create_raw_view,
    create_view,
)

# アプリのログが S3 に着地するまで待つ上限 (秒)。
WAIT_TIMEOUT_SECONDS = int(os.environ.get("LOG_WAIT_TIMEOUT_SECONDS", "120"))


def has_view(con: duckdb.DuckDBPyConnection, name: str) -> bool:
    """そのビューが既にあるかを返します。

    **推論するビューを二度作らないため。** 検査用のビューは
    全ファイルを開くので、作り直しはそのまま接続の消費になります
    (logs_source の「接続の話」)。
    """
    return bool(
        con.execute(
            "SELECT 1 FROM duckdb_views() WHERE view_name = ?", [name]
        ).fetchall()
    )


def upload_interval_seconds() -> int:
    """fluent-bit のアップロード契機を秒で返します。

    compose の x-log-upload-timeout を出す側と共有しているので、
    "10s" のような fluent-bit の書式で渡ってきます。
    """
    raw = os.environ.get("LOG_UPLOAD_TIMEOUT", "30s").strip().lower()
    if raw.endswith("m"):
        return int(float(raw[:-1]) * 60)
    return int(float(raw.removesuffix("s")))


def wait_for_http_requests(con: duckdb.DuckDBPyConnection) -> bool:
    """リクエストのログが「出揃う」まで待ちます。

    **「1 件見つかった」では止めません。** fluent-bit は
    アプリの起動ログ (gin のルート登録) を先にアップロードするので、
    最初のオブジェクトで待つのをやめると、リクエストのログが
    1 件しか無い状態で検査を始めることになります。
    「共通フィールドが全行にある」を 1 行で確かめても、
    検査として何も守っていません (実測でその状態になった)。

    件数が 1 周期ぶん増えなくなったところを「出揃った」とみなします。
    ビューは glob をクエリのたびに評価するので、
    待っている間に増えたオブジェクトも次の実行で見えます。
    """
    # **アップロード契機より長く待つ。** 短いと、単に
    # 「次のアップロードがまだ」の状態を「増えなくなった」と誤読します。
    interval = upload_interval_seconds() + 2
    deadline = time.monotonic() + WAIT_TIMEOUT_SECONDS
    previous = -1

    while True:
        try:
            # **毎周ビューを作り直す。** 検査用のビューはスキーマを
            # 推論するので、後から現れたフィールド (msg など) は
            # 作り直さないと列にならない。
            # バケットが空のときは read_json 自体が落ちるが、
            # それも「まだ来ていない」の一形態として扱う。
            #
            # ここで見るのは RAW_VIEW_NAME だけなので、分析用は作らない
            # (logs_source の「接続の話」—— 推論はファイル 1 つにつき
            # 接続を 1 本使うので、要らないビューは作らない)。
            create_raw_view(con)
            found = con.execute(
                f"SELECT count(*) FROM {RAW_VIEW_NAME} WHERE msg = 'http_request'"
            ).fetchone()[0]
        except duckdb.Error:
            # まだ 1 つもオブジェクトが無い、あるいは msg 列がまだ
            # 推論されていない。どちらもこの時点では正常。
            found = 0

        if found > 0 and found == previous:
            print(f"      リクエストのログが出揃いました ({found} 行)")
            return True
        if time.monotonic() >= deadline:
            return found > 0

        previous = found
        print(f"      ログの着地を待っています... (いま {found} 行)")
        time.sleep(interval)

# ログに出してはいけない項目 (ADR 0010 の 4-5)。
#
# **S3 に長期保管する以上、一度出したものは事実上消せない。**
# 列名に部分一致で当てる —— session_id でも sessionId でも拾いたいので、
# 小文字化した列名に対する部分一致にする。
FORBIDDEN_FIELD_PARTS = [
    "session",
    "cookie",
    "email",
    "google_sub",
    "authorization",
    "password",
    "secret",
    "token",
]

# 全ログに必ず含める最小集合 (ADR 0010 の 4-2)。
REQUIRED_FIELDS = ["time", "level", "msg", "service", "version"]

# **DDL に宣言できない列。**
#
# service は S3 のキーの 1 段目 (service=go-api) と同じ名前で、
# Athena はパーティション列とデータ列の同名を許さない。
# そのため DDL では LOCATION に埋め込み、列としては宣言していない
# (infra/athena/table.sql の罠 1)。
#
# レコード側には出続けるので、宣言漏れの検査からは除外する。
UNDECLARABLE_FIELDS = {"service"}


class Result:
    """検査の結果を貯めます。"""

    def __init__(self) -> None:
        self.failures: list[str] = []
        self.notes: list[str] = []

    def check(self, ok: bool, label: str, detail: str = "") -> None:
        mark = "OK  " if ok else "NG  "
        print(f"{mark}{label}")
        if detail:
            print(f"      {detail}")
        if not ok:
            self.failures.append(label)

    def note(self, text: str) -> None:
        print(f"      {text}")
        self.notes.append(text)


def main() -> int:  # noqa: PLR0915 - 検査項目を 1 本の流れで読ませたい
    con = connect()
    r = Result()

    if not wait_for_http_requests(con):
        print("NG  リクエストのログが S3 に届かなかった")
        print("      fluent-bit のアップロード契機 (LOG_UPLOAD_TIMEOUT) と、")
        print("      LOG_FORMAT が json かどうかを確認する")
        # **ここで打ち切らない。** 何が着地しているかを見せてから落とす ——
        # 「何も来ていない」と「JSON でない行だけ来ている」は原因が違う。
        r.failures.append("リクエストのログが S3 に届かなかった")

    try:
        # **検査は両方のビューを使う。** 分析用は DDL の列と型
        # (本番と同じ見え方)、検査用は推論した列 (宣言し忘れの検出)。
        #
        # 分析用は S3 に触らないので安い。検査用は**全ファイルを推論する**ので、
        # 待ち合わせの最後に作れていたら作り直さない
        # (logs_source の「接続の話」—— ファイル 1 つにつき接続 1 本)。
        create_view(con)
        if not has_view(con, RAW_VIEW_NAME):
            create_raw_view(con)
    except (duckdb.Error, ValueError) as e:
        # **原因を決めつけない。** ここは以前「ログが 1 件も S3 に届いて
        # いない」と言い切っていたが、この時点の失敗は届いていないとは
        # 限らない —— DDL が読めない・型を写せない (logs_source の
        # _TYPE_MAP)・**推論中にポートが枯れる**、どれでもここに来る。
        # query.py で直したのと同じ嘘だった。
        print(f"NG  ログのビューを作れなかった: {e}")
        print("      思い当たるもの:")
        print("      - S3 にまだ 1 件も着地していない (fluent-bit と MinIO を確認する)")
        print("      - 接続を張り切っている (Could not establish connection):")
        print("        compose の logs-query の sysctls net.ipv4.tcp_tw_reuse=1")
        print("      - DDL の型を DuckDB に写せない (logs_source の _TYPE_MAP)")
        return 1

    total = con.execute(f"SELECT count(*) FROM {VIEW_NAME}").fetchone()[0]
    if total == 0:
        print("NG  ログが 1 件も S3 に届いていない")
        print("      fluent-bit と MinIO が起動しているか確認する")
        return 1
    print(f"      S3 上のログ: {total} 行")

    columns = [row[0] for row in con.execute(f"DESCRIBE {VIEW_NAME}").fetchall()]
    # **共通フィールドの検査はこちらで行う。** 分析用のビューでは
    # パーティション列 service がレコード側を上書きするため、
    # アプリが service を出さなくなっても検査が通ってしまう
    # (logs_source の RAW_VIEW_NAME を参照)。
    raw_columns = [row[0] for row in con.execute(f"DESCRIBE {RAW_VIEW_NAME}").fetchall()]

    # -----------------------------------------------------------------
    # 1. パーティションのキーが読める (決定 2)
    #
    # **これが崩れると Athena の課金が青天井になる。** partition
    # projection は S3 のキーの規則性だけに依存しているので、
    # s3_key_format を書き換えた瞬間に、静かに全期間スキャンへ落ちる。
    # -----------------------------------------------------------------
    for key in ("service", "dt", "hour"):
        r.check(key in columns, f"パーティション列 {key} が読める")

    if "dt" in columns and "hour" in columns:
        partitions = con.execute(
            f"SELECT DISTINCT service, dt, hour FROM {VIEW_NAME} ORDER BY dt, hour"
        ).fetchall()
        r.note(f"パーティション: {len(partitions)} 個 (例: {partitions[0]})")

    # -----------------------------------------------------------------
    # 2. FireLens の二重ネストが展開されている (「罠」節)
    #
    # **ここが今回いちばん壊れやすい。** アプリ側のログ形式が
    # JSON でなくなると (LOG_FORMAT=text)、パーサは失敗し、
    # log キーの文字列がそのまま S3 に着地する。
    # -----------------------------------------------------------------
    r.check(
        "msg" in raw_columns,
        "パーサが JSON を展開している (msg が列として読める)",
        "msg が無い = log キーの文字列のまま着地している (LOG_FORMAT を確認する)",
    )
    if "msg" not in raw_columns:
        # 以降の検査は msg 前提なので、ここで打ち切る。
        return 1

    # **展開できた行の定義は「log キーが残っていないこと」。**
    # msg の有無で定義すると、msg が欠けた行を「展開されていない行」として
    # 除外することになり、4-2 の検査 (msg が全行にあるか) が
    # 自分自身を前提にしたトートロジーになる。
    parsed_where = "log IS NULL" if "log" in raw_columns else "true"

    structured = con.execute(
        f"SELECT count(*) FROM {RAW_VIEW_NAME} WHERE {parsed_where}"
    ).fetchone()[0]
    r.check(structured > 0, f"構造化ログが着地している ({structured} 行)")

    http = con.execute(
        f"SELECT count(*) FROM {RAW_VIEW_NAME} WHERE msg = 'http_request'"
    ).fetchone()[0]
    r.check(http > 0, f"http_request が着地している ({http} 行)")

    # **JSON でない行は残ってよい。** go run のコンパイルエラーや
    # gin のデバッグ出力は JSON ではないので、log キーのまま素通しする。
    # 捨てないのは、障害時にそれが最も見たい行だからになる。
    if "log" in raw_columns:
        unparsed = con.execute(
            f"SELECT count(*) FROM {RAW_VIEW_NAME} WHERE log IS NOT NULL"
        ).fetchone()[0]
        r.note(f"JSON でない行 (log キーのまま素通し): {unparsed} 行")

    # -----------------------------------------------------------------
    # 3. 共通フィールドが揃っている (4-2)
    #
    # Athena はスキーマオンリードなので、欠けた列は NULL になるだけで
    # エラーにならない。**欠けていること自体に気づけない。**
    # -----------------------------------------------------------------
    for field in REQUIRED_FIELDS:
        if field not in raw_columns:
            r.check(False, f"共通フィールド {field} が列として無い")
            continue
        missing = con.execute(
            f"SELECT count(*) FROM {RAW_VIEW_NAME} WHERE {parsed_where} AND {field} IS NULL"
        ).fetchone()[0]
        r.check(missing == 0, f"共通フィールド {field} が全行にある", f"欠けている行: {missing}")

    # request_id は http_request に必ず要る (4-1)。
    # **相関 ID が無いと、同時刻の別リクエストと区別できない。**
    if "request_id" in raw_columns:
        missing = con.execute(
            f"SELECT count(*) FROM {RAW_VIEW_NAME} "
            f"WHERE msg = 'http_request' AND request_id IS NULL"
        ).fetchone()[0]
        r.check(missing == 0, "http_request に request_id が付いている", f"欠けている行: {missing}")
    else:
        r.check(False, "request_id が列として無い")

    # パーティションの service と、レコード側の service が一致しているか。
    #
    # **ズレると、S3 のキーとレコードの中身が食い違う。** 分析用のビューでは
    # パーティション側が勝つので、この食い違いはそこからは見えない ——
    # 「go-api のログとして集計したものに別サービスの行が混ざる」形で
    # 静かに間違う。fluent-bit の Match と s3_key_format が対応している
    # 限り起きないが、対応が崩れたことを検出できるのはここだけになる。
    services = [
        row[0]
        for row in con.execute(
            f"SELECT DISTINCT service FROM {RAW_VIEW_NAME} WHERE service IS NOT NULL"
        ).fetchall()
    ]
    partitions_services = [
        row[0]
        for row in con.execute(f"SELECT DISTINCT service FROM {VIEW_NAME}").fetchall()
    ]
    r.check(
        sorted(services) == sorted(partitions_services),
        "S3 のキーの service とレコードの service が一致している",
        f"キー: {sorted(partitions_services)} / レコード: {sorted(services)}",
    )

    # -----------------------------------------------------------------
    # 3-2. msg はイベント名であって自由文ではない (4-2)
    #
    # **自由文にすると、集計のたびに LIKE を書くことになる。**
    # 実際、起動時のログは「サーバを起動しました」のような日本語の
    # 文章になっていた —— 数えるにも絞るにも使えない形で、
    # しかも文言を直した瞬間に過去のクエリが当たらなくなる。
    # -----------------------------------------------------------------
    free_text = [
        row[0]
        for row in con.execute(
            f"""
            SELECT DISTINCT msg FROM {RAW_VIEW_NAME}
            WHERE msg IS NOT NULL
              AND NOT regexp_matches(msg, '^[a-z][a-z0-9_]*$')
            """
        ).fetchall()
    ]
    r.check(
        not free_text,
        "msg がイベント名になっている (自由文でない)",
        f"自由文の msg: {free_text}" if free_text else "",
    )

    # -----------------------------------------------------------------
    # 3-3. Athena の DDL に宣言されていない列が出ていないか
    #
    # **Athena はスキーマオンリード。宣言しなかった列は静かに NULL になる。**
    # アプリが新しいフィールドを出し始めても、DDL に足すまで見えない。
    # エラーにならないので、「クエリを書いたら全部 NULL だった」で
    # 初めて気づくことになる。
    # -----------------------------------------------------------------
    declared = set(athena_columns())
    if declared:
        undeclared = sorted(set(raw_columns) - declared - UNDECLARABLE_FIELDS)
        r.check(
            not undeclared,
            "Athena の DDL に宣言されていない列が無い",
            f"DDL (infra/athena/table.sql) に足す: {undeclared}" if undeclared else "",
        )

    # -----------------------------------------------------------------
    # 3-4. DDL に宣言した列が、分析用のビューに全部出ているか
    #
    # **3-3 の逆向き。** 3-3 は「データにあって DDL に無い列」を見るが、
    # こちらは「DDL にあってビューに無い列」を見る。
    #
    # 分析用のビューは DDL をそのままスキーマにしているので、
    # **写せない型が 1 つあると、その列だけ静かに消える。**
    # そうなると Athena では見えるのに手元では見えない —— 
    # logs_source がまさに防ぐために書かれた壊れ方に戻る。
    # (_TYPE_MAP は未知の型で落ちるようにしてあるが、検査でも見ておく)
    # -----------------------------------------------------------------
    if declared:
        missing_in_view = sorted(declared - set(columns))
        r.check(
            not missing_in_view,
            "DDL の列が分析用のビューに全部出ている",
            f"ビューに出ていない: {missing_in_view}" if missing_in_view else "",
        )

    # -----------------------------------------------------------------
    # 4. latency は ms の数値 (4-4)
    #
    # slog.Duration のままだと JSON にはナノ秒の整数で出る。
    # 間違いではないが、Athena で毎回 / 1e6 を書くことになる。
    # -----------------------------------------------------------------
    if "latency_ms" in raw_columns:
        dtype = con.execute(
            f"SELECT typeof(latency_ms) FROM {RAW_VIEW_NAME} "
            f"WHERE latency_ms IS NOT NULL LIMIT 1"
        ).fetchone()
        r.check(
            dtype is not None and dtype[0] not in ("VARCHAR", "NULL"),
            "latency_ms が数値として読める",
            f"typeof = {dtype[0] if dtype else '(行が無い)'}",
        )
    else:
        r.check(False, "latency_ms が列として無い", "latency (ナノ秒) のままになっていないか")

    # -----------------------------------------------------------------
    # 5. 出してはいけないものが出ていない (4-5)
    #
    # **一度 S3 に出したものは事実上消せない。** 検査を後から足しても
    # 過去のオブジェクトは直せないので、早く鳴るほど価値がある。
    # -----------------------------------------------------------------
    leaked = [
        col
        for col in sorted(set(columns) | set(raw_columns))
        if any(part in col.lower() for part in FORBIDDEN_FIELD_PARTS)
    ]
    r.check(
        not leaked,
        "ログに出してはいけない項目が無い (4-5)",
        f"見つかった列: {leaked}" if leaked else "",
    )

    # -----------------------------------------------------------------
    # 6. パーティションの時刻とレコードの時刻のズレ (決定 2 の「罠」)
    #
    # **検査ではなく観測。** ズレるのは正常で、ズレ幅を知っておくために出す。
    # hour=14 のパーティションに 13:58 のログが混ざるのは、
    # キーが「fluent-bit がフラッシュした時刻」で決まるため。
    # -----------------------------------------------------------------
    skew = con.execute(
        f"""
        SELECT
            max(epoch(
                strptime(CAST(dt AS VARCHAR) || ' ' || lpad(CAST(hour AS VARCHAR), 2, '0'),
                         '%Y-%m-%d %H')
                - date_trunc('hour', CAST(time AS TIMESTAMP))
            )) / 3600
        FROM {VIEW_NAME}
        WHERE msg IS NOT NULL AND time IS NOT NULL
        """
    ).fetchone()[0]
    if skew is not None:
        r.note(
            f"パーティション時刻とレコード時刻の最大差: {skew:.0f} 時間 "
            "(0 でなくても異常ではない。キーはフラッシュ時刻で決まる)"
        )

    print()
    if r.failures:
        print(f"NG: {len(r.failures)} 件の検査に失敗しました")
        for f in r.failures:
            print(f"  - {f}")
        return 1
    print("すべての検査を通過しました")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
