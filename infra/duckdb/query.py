#!/usr/bin/env python3
"""S3 に着地したログを SQL で読む (docs/adr/0010-log-pipeline.md)。

    logs-query "SELECT msg, count(*) FROM go_api_logs GROUP BY 1"
    logs-query /queries/http-status.sql

ビュー名は go_api_logs。パーティション列 (service / dt / hour) も
そのまま列として見える。**共通フィールドの検査には使わない** ——
パーティション列がレコード側を上書きするため (logs_source を参照)。
"""

from __future__ import annotations

import sys

import duckdb

from logs_source import VIEW_NAME, open_logs


def read_sql(argv: list[str]) -> str:
    """引数を SQL として解釈します。

    **.sql で終わればファイルとして読みます。** -c のような切り替えを
    置かないのは、Makefile 側で「SQL 文かファイルか」を分岐させると
    make logs-query の引数が 2 種類に割れるためです。
    SQL 文が .sql で終わることは無いので、この判定で曖昧になりません。
    """
    if len(argv) != 1 or not argv[0].strip():
        print(__doc__, file=sys.stderr)
        raise SystemExit(2)

    arg = argv[0]
    if arg.endswith(".sql"):
        with open(arg, encoding="utf-8") as f:
            return f.read()
    return arg


def main() -> int:
    sql = read_sql(sys.argv[1:])
    try:
        con = open_logs()
    except duckdb.Error as e:
        # **ビューを作る時点で落ちる。** read_json は 1 件も
        # ファイルが無いとエラーになるので、「クエリが悪い」と
        # 見分けが付くようにここで説明を足す。
        print(f"S3 にログがまだ 1 件もありません: {e}", file=sys.stderr)
        return 1

    # **1 行も無いときに「クエリが 0 行を返した」と区別する。**
    # ログ基盤の検証では、この 2 つを取り違えると
    # 「クエリが間違っている」と「そもそも届いていない」を
    # 同じ結果として見てしまう。
    total = con.execute(f"SELECT count(*) FROM {VIEW_NAME}").fetchone()[0]
    if total == 0:
        print(
            "ログが 1 件も見つかりません。"
            "fluent-bit のアップロード契機 (LOG_UPLOAD_TIMEOUT) を待っていない可能性があります。",
            file=sys.stderr,
        )
        return 1

    con.sql(sql).show(max_rows=100)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
