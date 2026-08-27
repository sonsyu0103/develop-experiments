"""S3 に着地したログを DuckDB から読むための共通部品。

docs/adr/0010-log-pipeline.md の「ローカルでの再現」。

**Athena との差は 2 つある。** 混同すると、手元で通ったクエリが本番で
落ちる (あるいはその逆) ので、ここに明記しておく。

  1. スキーマ
     Athena はスキーマオンリード —— DDL で宣言した列は、データに
     1 件も出ていなくても NULL として読める。DuckDB は既定では
     読んだファイルから推論するので、**まだ発生していないイベントの
     列を参照した瞬間にクエリが落ちる。**

     これは実害が大きい。「Phase 7 を入れる前に view_count_flushed の
     クエリを手元で確かめる」ができないと、ローカルで検証する意味が薄れる。

     そのため、**Athena の DDL (infra/athena/table.sql) をそのまま
     スキーマとして read_json に渡す** (create_view)。推論を使わないので、
     宣言した列は必ず在り、型も Athena と揃う。DDL が単一の情報源であり、
     手元と本番で同じクエリが書ける状態を保つ。

     **推論をやめたのは性能上の理由もある。** 下の「接続の話」を参照。

  2. パーティション
     Athena は partition projection でキーの範囲を宣言する (決定 2)。
     DuckDB には無く、hive_partitioning でパスから列を起こすだけになる。
     **スキャン量の削減は再現しない。** ここで測れるのは
     「キーの規則性が壊れていないか」までになる。

=============================================================================
接続の話 (ここを踏み抜いた)
=============================================================================
**DuckDB の httpfs は、読むファイル 1 つにつき TCP 接続を 1 本張る。**
接続は使い回されないので、TIME_WAIT が積み上がっていく。

コンテナのエフェメラルポートは 32768-60999 の 28,232 個しかない。
つまり **1 プロセスが触れるファイルは、累計およそ 28,000 個が上限**になる。
超えると `Cannot assign requested address` —— DuckDB からは
`IO Error: Could not establish connection` として見える。

以前はここで 3 回踏んでいた:

  - スキーマ推論が全ファイルを開く。ビューを作るだけで 1 万接続使う
  - 分析用と検査用の 2 本を必ず作っていたので、その倍を使う
  - **残りでクエリ本体を実行するので、ログが溜まるほど先に枯れる**

対策は 2 つとも要る (片方では足りない):

  - **推論をやめる** (このモジュール)。ビュー作成の接続が 1 万 → 十数本になる
  - **TIME_WAIT を使い回す** (compose の logs-query の sysctls)。
    クエリ本体はファイル数ぶん接続を張るので、こちらが無いといずれ再発する
"""

from __future__ import annotations

import os
import re
import sys

import duckdb

# ログの置き場。fluent-bit の s3_key_format と対になる。
#
# **service=* を含めるのは意図的。** キーの 1 段目が service なので、
# ここを固定すると「go-api 以外のサービスが増えた日」に
# パーティション列が消えて、クエリが静かに壊れる。
LOGS_GLOB = "s3://{bucket}/logs/service=*/dt=*/hour=*/*.json.gz"

# 分析用。パーティション列 (service / dt / hour) が列として見え、
# 列と型は Athena の DDL がそのまま決める。
VIEW_NAME = "go_api_logs"

# Athena のテーブル定義。**列の単一の情報源。**
ATHENA_DDL_PATH = os.environ.get("ATHENA_DDL_PATH", "/athena/table.sql")

# Hive / Athena の型 → DuckDB の型。
# DDL に出てくるものだけを持つ (増えたらここに足す)。
#
# **知らない型は黙って捨てず、落とす** (athena_columns)。
# この表がそのままビューのスキーマになるので、載っていない型を
# 読み飛ばすと「Athena では見えるのに手元では見えない」列が生まれる ——
# このモジュールが防ぐために書かれたもの、そのものになる。
_TYPE_MAP = {
    "string": "VARCHAR",
    "bigint": "BIGINT",
    "int": "INTEGER",
    "double": "DOUBLE",
    "boolean": "BOOLEAN",
}

# DDL の列宣言。**`名前` 型, だけの行**に限る。
#
# **行頭と行末に錨を打つ。** これが無いと、コメント中の
# バッククォート付きの語 (「`contact_id` は」など) まで列として拾う ——
# 実際、table.sql の日本語コメントから contact_id が偽の型つきで
# 拾えてしまっていた。.github/scripts/verify-log-events.py が
# 同じ理由で同じ錨を打っており、**両者がずれると検査の意味が消える。**
_DDL_COLUMN = re.compile(r"^\s*`([a-z_0-9]+)`\s+(\w+)\s*,?\s*$", re.M)

# 検査用。**パーティション列を起こさず、スキーマを推論する。**
#
# 【なぜ 2 つ要るか】
# (1) S3 のキーの 1 段目が service=go-api で、レコードの中にも service がある
#     (ADR 0010 の 4-2)。**同じ名前なので、hive_partitioning はレコード側を
#     上書きする** —— 実測すると、JSON ですらない行 (gin のデバッグ出力) にも
#     service='go-api' が入る。つまり分析用のビューでは、**アプリが service を
#     出さなくなっても検査が通ってしまう。**
# (2) 分析用は DDL を渡すので、**DDL に無い列は見えない** (Athena の
#     「罠 2」と同じ挙動)。「着地しているのに宣言し忘れている列」を
#     見つけるには、推論したビューが要る。verify.py の要になっている検査。
#
# **こちらは推論するので接続を食う** (モジュール冒頭の「接続の話」)。
# make logs-verify は S3 を空にしてから測るのでファイルが少なく、実害は無い。
# 作るのは検査のときだけにしてある —— logs-query では作らない。
RAW_VIEW_NAME = "go_api_logs_raw"


def athena_columns(path: str = ATHENA_DDL_PATH) -> dict[str, str]:
    """Athena の DDL から列名と DuckDB の型を読み取ります。

    **DDL を写経しないためにパースしています。** 列の一覧を Python 側に
    書き写すと、DDL に足した列をこちらに足し忘れた瞬間に
    「本番では見えるのに手元では見えない」がまた生まれます。

    PARTITIONED BY の中は読みません —— そちらは hive_partitioning が
    パスから起こすため、こちらで宣言する対象ではありません。
    """
    try:
        with open(path, encoding="utf-8") as f:
            ddl = f.read()
    except OSError:
        # DDL をマウントしていない使い方 (単発のクエリ) でも動かす。
        return {}

    # CREATE ... ( ... ) の最初の括弧の中だけを見る。
    body = ddl.split("CREATE EXTERNAL TABLE", 1)[-1]
    body = body.split("PARTITIONED BY", 1)[0]

    columns: dict[str, str] = {}
    for name, raw_type in _DDL_COLUMN.findall(body):
        duck_type = _TYPE_MAP.get(raw_type.lower())
        if duck_type is None:
            # **黙って捨てない。** ここで捨てると、その列は分析用の
            # ビューから消え、Athena にだけ在る状態になる (上の _TYPE_MAP)。
            raise ValueError(
                f"DDL の列 `{name}` の型 {raw_type} を DuckDB の型に写せません。"
                f" {__name__} の _TYPE_MAP に追加してください。"
            )
        columns[name] = duck_type
    return columns


def connect() -> duckdb.DuckDBPyConnection:
    """MinIO を読めるようにした接続を返します。"""
    con = duckdb.connect()
    con.execute("LOAD httpfs")

    endpoint = os.environ.get("LOG_S3_ENDPOINT", "minio:9000")
    # DuckDB の ENDPOINT はスキームを含まない。
    # http:// を付けたまま渡すと「名前解決できない」形で落ちる。
    use_ssl = endpoint.startswith("https://")
    endpoint = endpoint.removeprefix("http://").removeprefix("https://")

    con.execute(
        """
        CREATE OR REPLACE SECRET minio (
            TYPE       s3,
            KEY_ID     $key_id,
            SECRET     $secret,
            REGION     $region,
            ENDPOINT   $endpoint,
            -- MinIO は path-style が要る (bucket.minio は名前解決できない)。
            -- go-api の S3_USE_PATH_STYLE と同じ理由。
            URL_STYLE  'path',
            USE_SSL    $use_ssl
        )
        """,
        {
            "key_id": os.environ.get("AWS_ACCESS_KEY_ID", "minioadmin"),
            "secret": os.environ.get("AWS_SECRET_ACCESS_KEY", "minioadmin"),
            "region": os.environ.get("AWS_REGION", "ap-northeast-1"),
            "endpoint": endpoint,
            "use_ssl": use_ssl,
        },
    )
    return con


def _glob() -> str:
    return LOGS_GLOB.format(bucket=os.environ.get("LOG_BUCKET", "bbs-logs"))


def create_view(con: duckdb.DuckDBPyConnection) -> None:
    """分析用のビュー (go_api_logs) を作ります。**推論しません。**

    列と型は Athena の DDL がそのまま決めるので、

      - **宣言済みの列は、データに 1 件も出ていなくても NULL で読める。**
        Phase を入れる前にクエリを検証できる (モジュール冒頭の差 1)
      - 型が Athena と揃う。推論だと request_id が UUID、status が
        BIGINT になり、手元でだけ通る書き方が生まれる
      - **ビューを作る時点で S3 のファイルを 1 つも開かない。**
        推論をやめた効果がここに出る (冒頭の「接続の話」)

    **DDL に無い列は見えません。** Athena の「罠 2」と同じ挙動で、
    そこは意図どおりです。着地しているのに宣言し忘れている列は
    検査用のビュー (create_raw_view) が見つけます。
    """
    columns = athena_columns()
    if not columns:
        # DDL が読めない使い方でも動かす。**推論に落ちるので、
        # ファイルが増えると接続を使い切って落ちる** (冒頭の「接続の話」)。
        # 黙って遅くなる形なので、警告を出す。
        print(
            f"警告: Athena の DDL を読めませんでした ({ATHENA_DDL_PATH})。"
            "スキーマを推論します —— ログのファイル数が多いと落ちます。",
            file=sys.stderr,
        )
        source = f"""read_json(
            '{_glob()}',
            hive_partitioning = true,
            union_by_name = true,
            format = 'newline_delimited',
            sample_size = -1
        )"""
    else:
        declared = ", ".join(f"'{name}': '{t}'" for name, t in columns.items())
        source = f"""read_json(
            '{_glob()}',
            -- **Athena の DDL がスキーマ。** 推論しない。
            columns = {{{declared}}},
            -- **宣言と型が食い違う値を、行ごと落とさず NULL にする。**
            --
            -- Athena の SerDe の ignore.malformed.json='true' と同じ挙動に揃える
            -- (infra/athena/table.sql)。これが無いと、`status` に文字列が
            -- 1 つ着地しただけで **その列を触るクエリが全部落ちる** ——
            -- 本番は動き続けるのに手元だけ死ぬ、という**逆さまの差**になる。
            -- 実測: 壊れた行だけが NULL になり、同じファイルの他の行は残る。
            --
            -- 引き受けるコスト: 型の食い違いが**静かに** NULL になる。
            -- 気づく経路は別に用意してある ——
            -- 検査用ビュー (推論) と .github/scripts/verify-log-events.py。
            ignore_errors = true,
            -- パスの dt= / hour= / service= を列として起こす。
            -- **Athena の partition projection に相当するものではない** ——
            -- こちらは全ファイルを開いてから列を足すだけで、
            -- スキャン量は減らない (モジュール冒頭の差 2)。
            hive_partitioning = true,
            format = 'newline_delimited'
        )"""

    con.execute(f"CREATE OR REPLACE VIEW {VIEW_NAME} AS SELECT * FROM {source}")


def create_raw_view(con: duckdb.DuckDBPyConnection) -> None:
    """検査用のビュー (go_api_logs_raw) を作ります。**推論します。**

    分析用と分ける理由は RAW_VIEW_NAME のコメントにあります。
    要るのは verify.py だけなので、logs-query では呼びません ——
    **推論はファイル 1 つにつき接続を 1 本使う**ためです
    (モジュール冒頭の「接続の話」)。
    """
    con.execute(
        f"""
        CREATE OR REPLACE VIEW {RAW_VIEW_NAME} AS
        SELECT * FROM read_json(
            '{_glob()}',
            hive_partitioning = false,
            -- ログは行ごとにフィールドが違う (msg ごとに固有の項目が付く)。
            -- 揃っている前提で読むと、後から出てきた項目が落ちる。
            union_by_name = true,
            format = 'newline_delimited',
            -- **全件から推論する。** 既定のサンプリングだと、
            -- 頻度の低いイベントの列が「たまたま見えない」ことがある。
            -- 検査は「宣言し忘れた列を見つける」のが仕事なので、
            -- 見落とすくらいなら時間をかける。
            sample_size = -1
        )
        """
    )


def open_logs() -> duckdb.DuckDBPyConnection:
    """接続と分析用のビューをまとめて用意します。"""
    con = connect()
    create_view(con)
    return con
