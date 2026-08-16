"""S3 に着地したログを DuckDB から読むための共通部品。

docs/adr/0010-log-pipeline.md の「ローカルでの再現」。

**Athena との差は 2 つある。** 混同すると、手元で通ったクエリが本番で
落ちる (あるいはその逆) ので、ここに明記しておく。

  1. スキーマ
     Athena はスキーマオンリード —— DDL で宣言した列は、データに
     1 件も出ていなくても NULL として読める。DuckDB は読んだファイルから
     推論するので、**まだ発生していないイベントの列を参照した瞬間に
     クエリが落ちる。**

     これは実害が大きい。「Phase 7 を入れる前に view_count_flushed の
     クエリを手元で確かめる」ができないと、ローカルで検証する意味が薄れる。

     そのため、**Athena の DDL (infra/athena/table.sql) を読んで、
     欠けている列を NULL 列として補う。** DDL が単一の情報源であり、
     手元と本番で同じクエリが書ける状態を保つ。

  2. パーティション
     Athena は partition projection でキーの範囲を宣言する (決定 2)。
     DuckDB には無く、hive_partitioning でパスから列を起こすだけになる。
     **スキャン量の削減は再現しない。** ここで測れるのは
     「キーの規則性が壊れていないか」までになる。
"""

from __future__ import annotations

import os
import re

import duckdb

# ログの置き場。fluent-bit の s3_key_format と対になる。
#
# **service=* を含めるのは意図的。** キーの 1 段目が service なので、
# ここを固定すると「go-api 以外のサービスが増えた日」に
# パーティション列が消えて、クエリが静かに壊れる。
LOGS_GLOB = "s3://{bucket}/logs/service=*/dt=*/hour=*/*.json.gz"

# 分析用。パーティション列 (service / dt / hour) が列として見え、
# Athena の DDL にある列は、データに無くても NULL 列として見える。
VIEW_NAME = "go_api_logs"

# 上の素材。S3 から読んだそのまま (パーティション列あり、補完なし)。
HIVE_VIEW_NAME = "go_api_logs_hive"

# Athena のテーブル定義。**列の単一の情報源。**
ATHENA_DDL_PATH = os.environ.get("ATHENA_DDL_PATH", "/athena/table.sql")

# Hive / Athena の型 → DuckDB の型。
# DDL に出てくるものだけを持つ (増えたらここに足す)。
_TYPE_MAP = {
    "string": "VARCHAR",
    "bigint": "BIGINT",
    "int": "INTEGER",
    "double": "DOUBLE",
    "boolean": "BOOLEAN",
}

# 検査用。**パーティション列を起こさない。**
#
# 【なぜ 2 つ要るか】
# S3 のキーの 1 段目が service=go-api で、レコードの中にも service がある
# (ADR 0010 の 4-2)。**同じ名前なので、hive_partitioning はレコード側を
# 上書きする** —— 実測すると、JSON ですらない行 (gin のデバッグ出力) にも
# service='go-api' が入る。
#
# つまり分析用のビューでは、**アプリが service を出さなくなっても
# 検査が通ってしまう。** 共通フィールドの検査はこちらで行う。
RAW_VIEW_NAME = "go_api_logs_raw"


def athena_columns(path: str = ATHENA_DDL_PATH) -> dict[str, str]:
    """Athena の DDL から列名と DuckDB の型を読み取ります。

    **DDL を写経しないためにパースしています。** 列の一覧を Python 側に
    書き写すと、DDL に足した列をこちらに足し忘れた瞬間に
    「本番では見えるのに手元では見えない」がまた生まれます。

    PARTITIONED BY の中は読みません —— そちらは hive_partitioning が
    パスから起こすため、NULL で補う対象ではありません。
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
    for name, raw_type in re.findall(r"`(\w+)`\s+(\w+)", body):
        duck_type = _TYPE_MAP.get(raw_type.lower())
        if duck_type:
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


def create_view(con: duckdb.DuckDBPyConnection) -> None:
    """ログを 2 つのビューとして見えるようにします。

    go_api_logs     : 分析用 (パーティション列あり)
    go_api_logs_raw : 検査用 (パーティション列なし。上書きを避ける)
    """
    glob = LOGS_GLOB.format(bucket=os.environ.get("LOG_BUCKET", "bbs-logs"))
    con.execute(
        f"""
        CREATE OR REPLACE VIEW {RAW_VIEW_NAME} AS
        SELECT * FROM read_json(
            '{glob}',
            hive_partitioning = false,
            union_by_name = true,
            format = 'newline_delimited',
            sample_size = -1
        )
        """
    )
    con.execute(
        f"""
        CREATE OR REPLACE VIEW {HIVE_VIEW_NAME} AS
        SELECT * FROM read_json(
            '{glob}',
            -- パスの dt= / hour= / service= を列として起こす。
            -- **Athena の partition projection に相当するものではない** ——
            -- こちらは全ファイルを開いてから列を足すだけで、
            -- スキャン量は減らない (モジュール冒頭の差 2)。
            hive_partitioning = true,
            -- ログは行ごとにフィールドが違う (msg ごとに固有の項目が付く)。
            -- 揃っている前提で読むと、後から出てきた項目が落ちる。
            union_by_name = true,
            format = 'newline_delimited',
            -- **全件から推論する。** 既定のサンプリングだと、
            -- 頻度の低いイベント (view_count_flushed など) の列が
            -- 「たまたま見えない」ことがある。手元のログ量なら安い。
            sample_size = -1
        )
        """
    )

    # **Athena の DDL に宣言済みで、まだデータに出ていない列を NULL で補う。**
    #
    # Athena はスキーマオンリードなので、宣言した列はデータが 1 件も
    # 無くても NULL として読める。DuckDB は推論なので、そのままだと
    # 「まだ発生していないイベントの列」を参照した瞬間にクエリが落ちる。
    #
    # これを埋めないと、**手元で確かめられるのは「既に起きたこと」だけ**に
    # なる。Phase 7 を入れる前に view_count_flushed のクエリを検証する、
    # といった使い方ができなくなる。
    present = {
        row[0] for row in con.execute(f"DESCRIBE {HIVE_VIEW_NAME}").fetchall()
    }
    padding = [
        f"CAST(NULL AS {duck_type}) AS {name}"
        for name, duck_type in athena_columns().items()
        if name not in present
    ]
    select_list = ", ".join(["*", *padding])
    con.execute(
        f"CREATE OR REPLACE VIEW {VIEW_NAME} AS SELECT {select_list} FROM {HIVE_VIEW_NAME}"
    )


def open_logs() -> duckdb.DuckDBPyConnection:
    """接続とビューをまとめて用意します。"""
    con = connect()
    create_view(con)
    return con
