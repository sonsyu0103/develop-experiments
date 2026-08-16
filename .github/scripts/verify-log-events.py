#!/usr/bin/env python3
"""ログの msg がイベント名になっているかを、ソースの静的検査で確かめる。

docs/adr/0010-log-pipeline.md の 4-2:

> `msg` を自由文にすると、集計のたびに `LIKE` を書くことになる。
> **固定の識別子とし、可変の情報は別フィールドに出す。**

【なぜ静的検査なのか】

実行時の検査 (make logs-verify) は、**実際に出たログしか見られない。**
エラー経路や、設定が揃っている環境でしか通らない分岐のログは、
手元で 1 回動かしただけでは出ない。出ないものは検査できない。

一方この検査は、**呼ばれていないログも含めて**全部見る。
S3 も MinIO も要らないので CI で回せる ——
ログ基盤の検証で CI に載せられるのはここまでになる
(パイプライン全体の検証は実環境が要る。README の「ログ基盤」節)。

【実際にこれで見つかったもの】

Phase 9 後半の時点で、22 か所が日本語の自由文だった:

    slog.Info("サーバを起動しました", slog.String("addr", ...))

ADR は Phase 9 前半で既に 4-2 を定めていたが、**守られていることを
検査する仕組みが無かった**ため、決めた後に書かれたログが逸れていた。
"""

from __future__ import annotations

import pathlib
import re
import sys

# 検査対象。apps/go-api 以下の Go ファイル (テストを除く)。
ROOT = pathlib.Path(__file__).resolve().parents[2] / "apps" / "go-api"

# イベント名として許す形。英小文字・数字・アンダースコアのみ。
EVENT_NAME = re.compile(r"^[a-z][a-z0-9_]*$")

# slog の呼び出しから msg を取り出す。
#
# **メソッド名で絞っている。** 「最初の文字列リテラル」を取る方式だと、
# slog.String("key", "値") の第 1 引数まで拾ってしまう。
CALLS = [
    # slog.Info("msg", ...) / logger.Warn("msg", ...)
    re.compile(r"\.(?:Info|Warn|Error|Debug)\(\s*\"([^\"]*)\""),
    # slog.InfoContext(ctx, "msg", ...)
    re.compile(r"\.(?:Info|Warn|Error|Debug)Context\(\s*[^,]+,\s*\"([^\"]*)\""),
    # slog.Log(ctx, level, "msg", ...) / slog.LogAttrs(ctx, level, "msg", ...)
    re.compile(r"\.Log(?:Attrs)?\(\s*[^,]+,\s*[^,]+,\s*\"([^\"]*)\""),
]


def violations() -> list[tuple[str, int, str]]:
    found: list[tuple[str, int, str]] = []
    for path in sorted(ROOT.rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        text = path.read_text(encoding="utf-8")
        for pattern in CALLS:
            for m in pattern.finditer(text):
                msg = m.group(1)
                if EVENT_NAME.match(msg):
                    continue
                line = text.count("\n", 0, m.start()) + 1
                found.append((str(path.relative_to(ROOT.parents[1])), line, msg))
    return sorted(found)


def main() -> int:
    found = violations()
    if not found:
        print("OK  ログの msg はすべてイベント名になっています")
        return 0

    print(f"NG  自由文の msg が {len(found)} 件あります (ADR 0010 の 4-2)")
    print()
    for path, line, msg in found:
        print(f"  {path}:{line}")
        print(f"    msg = {msg!r}")
    print()
    print("イベント名 (英小文字 + アンダースコア) に直し、")
    print("可変の情報は別フィールド、説明はコメントに出してください。")
    print("例: slog.Info(\"server_started\", slog.String(\"addr\", addr))")
    return 1


if __name__ == "__main__":
    sys.exit(main())
