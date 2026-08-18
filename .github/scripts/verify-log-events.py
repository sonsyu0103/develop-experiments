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

**フィールドの型が名前ごとに 1 つであることも見る。** Athena は
1 つの列に 2 つの型を持てないため、同じ名前を別の型で出すと
**どちらかが必ず壊れる。** しかもエラーは出ない ——
SerDe が読み飛ばすか NULL になるだけになる (infra/athena/table.sql の罠 3)。

【実際にこれで見つかったもの】

Phase 9 後半の時点で、22 か所が日本語の自由文だった:

    slog.Info("サーバを起動しました", slog.String("addr", ...))

ADR は Phase 9 前半で既に 4-2 を定めていたが、**守られていることを
検査する仕組みが無かった**ため、決めた後に書かれたログが逸れていた。

型の検査は、その後のレビューで 2 組の衝突を見つけて足した:

    status     http_request は int、画像と通報の「状態」は文字列
    target_id  通報の対象は bigint、モデレーション操作の対象は文字列

**どちらも「DDL を書こうとして初めて気づいた」形**で、
アプリだけを見ている限り誰も困らない。だから検査に落とす。

【DDL との突き合わせ】

**Athena はスキーマオンリード。** 宣言しなかった列はエラーにならず、
静かに NULL になる (infra/athena/table.sql の罠 2)。
アプリが新しいフィールドを出し始めても、DDL に足すまで**本番では見えない。**

実際に起きた: Phase 8 で contact_* を出し始めたとき、DDL に
**contact 系の列を 1 つも足していなかった。** 控えの失敗を数えようとした
段階で気づき、そこで手作業で 6 列足したが、**その洗い出しも 3 列
取りこぼしていた** (oldest_age_ms / smtp_host / starttls。レビュー指摘)。

手で数える限り漏れる。実行時の検査 (make logs-verify) は
**着地した列しか見られない**うえ実環境が要り、CI に載らない。
ソース側なら全部見えるので、ここで差分を取る。
"""

from __future__ import annotations

import pathlib
import re
import sys

# 検査対象。apps/go-api 以下の Go ファイル (テストを除く)。
ROOT = pathlib.Path(__file__).resolve().parents[2] / "apps" / "go-api"

# イベント名として許す形。英小文字・数字・アンダースコアのみ。
EVENT_NAME = re.compile(r"^[a-z][a-z0-9_]*$")

# フィールドの型。slog.<型>("名前", ...) から拾う。
#
# **Duration と Any は使わせない。**
#   Duration : JSON ハンドラではナノ秒の整数になる。ADR 0010 の 4-4 が
#              「latency は ms」と定めており、単位が混ざる
#   Any      : 出る型が呼び出しごとに変わる。Athena では宣言できない
FIELD_TYPES = re.compile(r"slog\.(String|Int64|Int|Bool|Float64|Duration|Any)\(\s*\"([a-z_]+)\"")

# Athena に持っていけない slog の型 (上の説明を参照)。
BANNED_TYPES = {"Duration", "Any"}

# Athena のテーブル定義。ここに宣言が無い列は本番で見えない。
ATHENA_DDL = pathlib.Path(__file__).resolve().parents[2] / "infra" / "athena" / "table.sql"

# DDL の列宣言。`名前` 型, の形だけを拾う。
#
# **型名まで見る。** これが無いと、コメント中に出てくる `contact_id` のような
# バッククォート付きの語まで列として数えてしまう。
DDL_COLUMN = re.compile(
    r"^\s*`([a-z_0-9]+)`\s+(?:string|bigint|int|double|boolean)\s*,?\s*$", re.M
)

# **DDL に宣言できない / しないフィールド。**
#
#   service  S3 のキーが logs/service=go-api/... なので Hive 形式では
#            パーティション列に見えるが、レコードの中にも同名がある。
#            Athena は両者の同名を許さないため、LOCATION に埋め込んで
#            列としては宣言していない (infra/athena/table.sql の罠 1)
DDL_EXEMPT = {"service"}

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


def field_types() -> dict[str, dict[str, list[str]]]:
    """フィールド名 → {slog の型: 出現場所} を集めます。"""
    fields: dict[str, dict[str, list[str]]] = {}
    for path in sorted(ROOT.rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        text = path.read_text(encoding="utf-8")
        for m in FIELD_TYPES.finditer(text):
            kind, name = m.group(1), m.group(2)
            line = text.count("\n", 0, m.start()) + 1
            where = f"{path.relative_to(ROOT.parents[1])}:{line}"
            fields.setdefault(name, {}).setdefault(kind, []).append(where)
    return fields


def athena_columns() -> set[str]:
    """Athena の DDL が宣言している列名を集めます。"""
    text = ATHENA_DDL.read_text(encoding="utf-8")
    # **CREATE 文より前は見ない。** 冒頭の解説にコメントとして
    # 列の例が並んでおり、そこまで拾うと宣言済みとして通ってしまいます。
    body = text[text.index("CREATE EXTERNAL TABLE") :]
    return set(DDL_COLUMN.findall(body))


def main() -> int:
    failed = False

    found = violations()
    if found:
        failed = True
        print(f"NG  自由文の msg が {len(found)} 件あります (ADR 0010 の 4-2)")
        print()
        for path, line, msg in found:
            print(f"  {path}:{line}")
            print(f"    msg = {msg!r}")
        print()
        print("イベント名 (英小文字 + アンダースコア) に直し、")
        print("可変の情報は別フィールド、説明はコメントに出してください。")
        print("例: slog.Info(\"server_started\", slog.String(\"addr\", addr))")
        print()
    else:
        print("OK  ログの msg はすべてイベント名になっています")

    fields = field_types()

    # 1 つの名前が 2 つ以上の型で出ていないか。
    conflicts = {name: kinds for name, kinds in fields.items() if len(kinds) > 1}
    if conflicts:
        failed = True
        print(f"NG  同じ名前で型が違うフィールドが {len(conflicts)} 件あります")
        print("    Athena は 1 つの列に 2 つの型を持てません (infra/athena/table.sql の罠 3)")
        print()
        for name, kinds in sorted(conflicts.items()):
            print(f"  {name}")
            for kind, places in sorted(kinds.items()):
                print(f"    slog.{kind}: {', '.join(places)}")
        print()
        print("用途を名前に含めて分けてください (例: status -> image_status)。")
        print()
    else:
        print("OK  フィールドの型は名前ごとに 1 つです")

    # Athena に持っていけない型を使っていないか。
    banned = {
        name: kinds[kind]
        for name, kinds in fields.items()
        for kind in kinds
        if kind in BANNED_TYPES
    }
    if banned:
        failed = True
        print(f"NG  Athena に載せられない型のフィールドが {len(banned)} 件あります")
        print()
        for name, places in sorted(banned.items()):
            print(f"  {name}: {', '.join(places)}")
        print()
        print("slog.Duration は ms の整数へ (例: interval_ms)、")
        print("slog.Any は文字列へ固定してください (例: fmt.Sprint(v))。")
        print()
    else:
        print("OK  Athena に載せられない型 (Duration / Any) は使っていません")

    # DDL に宣言されていないフィールドが無いか。
    missing = sorted(set(fields) - athena_columns() - DDL_EXEMPT)
    if missing:
        failed = True
        print(f"NG  Athena の DDL に無いフィールドが {len(missing)} 件あります")
        print("    宣言しない列はエラーにならず、静かに NULL になります")
        print("    (infra/athena/table.sql の罠 2)")
        print()
        for name in missing:
            kinds = ", ".join(f"slog.{k}" for k in sorted(fields[name]))
            print(f"  {name} ({kinds})")
            for places in fields[name].values():
                print(f"    {places[0]}")
        print()
        print("infra/athena/table.sql に列を足してください。")
        print("宣言できない事情がある場合は DDL_EXEMPT に理由つきで入れます。")
        print()
    else:
        print("OK  すべてのフィールドが Athena の DDL に宣言されています")

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
