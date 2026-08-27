#!/usr/bin/env python3
"""ログが Athena に載る形になっているかを、ソースの静的検査で確かめる。

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

【いま見ているもの】

    msg の形            イベント名か (4-2)
    フィールド名の形     イベント名と同じ規則か
    型の一貫性          同じ名前が 2 つの型で出ていないか (罠 3)
    禁止された型        Duration / Any を使っていないか
    禁止された項目      4-5 が挙げた項目を出していないか
    パーティション列     dt / hour と名前がぶつかっていないか (罠 1)
    DDL との型の一致     slog の型が DDL の型と揃っているか (罠 3)
    DDL への宣言        DDL に無いフィールドを出していないか (罠 2)

**下 4 つは 2026-08-24 のレビューで足した。** それまでは:

  - 4-5 は**実測の検査 (infra/duckdb/verify.py) にしか無く**、
    そちらは実環境が要るため CI で 1 度も回っていなかった。
    一度 S3 に出したものは事実上消せないので、
    「あとで気づいて直す」が効かない規則がいちばん検査から遠かった
  - DDL の読み取りが PARTITIONED BY より先まで及んでおり、
    dt / hour が**レコードの列として宣言済み**に見えていた (罠 1 の再来)
  - slog の型と DDL の型は**誰も突き合わせていなかった。**
    Athena は型の合わない値を静かに NULL にする

【この検査自身の検出力】

.github/scripts/checker-probe.py が、ここを壊して落ちることを実測する。
**検査は壊れても出力が変わらない** —— 何も検出できない状態でも
「OK」と出るので、そこを見る仕組みを別に置いてある。
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
#
# **名前の文字クラスで絞らない。** 以前は "([a-z_]+)" で、数字を含む名前
# (http2_streams など) は**正規表現に一致せず、どの検査にも当たらないまま
# 素通り**していた —— 型の衝突も、禁止型も、DDL の宣言漏れも見なくなる。
# 下の DDL_COLUMN が [a-z_0-9]+ を許しているのとも食い違っていた。
# ここでは中身を問わず拾い、名前の形そのものは FIELD_NAME で検査する。
FIELD_TYPES = re.compile(r"slog\.(String|Int64|Int|Bool|Float64|Duration|Any)\(\s*\"([^\"]*)\"")

# フィールド名として許す形。**msg (EVENT_NAME) と同じ規則にする** ——
# 片方だけ数字を許すと、DDL に書ける名前とアプリが出せる名前がずれる。
FIELD_NAME = re.compile(r"^[a-z][a-z0-9_]*$")

# Athena に持っていけない slog の型 (上の説明を参照)。
BANNED_TYPES = {"Duration", "Any"}

# Athena のテーブル定義。ここに宣言が無い列は本番で見えない。
ATHENA_DDL = pathlib.Path(__file__).resolve().parents[2] / "infra" / "athena" / "table.sql"

# DDL の列宣言。`名前` 型, の形だけを拾う。
#
# **型名まで見る。** これが無いと、コメント中に出てくる `contact_id` のような
# バッククォート付きの語まで列として数えてしまう。
#
# **型を捨てずに捕まえる。** 以前は (?:string|bigint|...) と非捕獲群で
# 書いていたため、(a) 型が比較に使えず、(b) 一覧に無い型 (timestamp など) を
# 足した列が**宣言されていないことになる**という 2 つの問題があった。
# infra/duckdb/logs_source.py の _DDL_COLUMN は (\w+) で取って未知の型を
# 例外にしており、同じ形に揃える —— あちらのコメントが
# 「両者がずれると検査の意味が消える」と書いているとおりになる。
DDL_COLUMN = re.compile(r"^\s*`([a-z_0-9]+)`\s+(\w+)\s*,?\s*$", re.M)

# Athena の型 → その列に出してよい slog の型。**1 対 1 に固定する。**
#
# bigint に slog.Int を許すような緩め方はしない。Athena の int は 32 bit で、
# 2^31 を超える値は静かに壊れる。実測では現行の 61 列すべてが
# この 1 対 1 に収まっていた (String/28, Int64/17, Int/12, Bool/3, Float64/1)。
DDL_TYPE_TO_SLOG = {
    "string": "String",
    "bigint": "Int64",
    "int": "Int",
    "double": "Float64",
    "boolean": "Bool",
}

# **DDL に宣言できない / しないフィールド。**
#
#   service  S3 のキーが logs/service=go-api/... なので Hive 形式では
#            パーティション列に見えるが、レコードの中にも同名がある。
#            Athena は両者の同名を許さないため、LOCATION に埋め込んで
#            列としては宣言していない (infra/athena/table.sql の罠 1)
DDL_EXEMPT = {"service"}

# **ログに出してはいけない項目** (ADR 0010 の 4-5)。
#
# 名前への部分一致で当てる。session_id でも sessionId でも拾いたいので、
# 小文字化した名前に対する部分一致にする。
#
# 【なぜここにも要るか】
# 同じ一覧を infra/duckdb/verify.py が持っているが、あちらは
# **S3 に着地したログしか見られない**うえ MinIO と duckdb が要り、
# CI では回らない (make logs-verify は実環境が要る)。
# つまり **4-5 は CI で 1 度も検査されていなかった。**
#
# 4-5 は他の規則と性質が違う。**一度 S3 に出したものは事実上消せない**ので、
# 「あとで気づいて直す」が効かない。呼ばれていない分岐まで見えるこちらで
# 止めるのが本筋になる。下の FORBIDDEN_SOURCE で両者の一覧が
# ずれていないことも検査する。
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

# 一覧の写し元。**ずれたら落とす。** 片方にだけ項目を足すと、
# 「静的検査は通るのに実測では落ちる」(あるいはその逆) が起きる。
FORBIDDEN_SOURCE = (
    pathlib.Path(__file__).resolve().parents[2]
    / "infra" / "duckdb" / "verify.py"
)

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


def _ddl_sections() -> tuple[str, str]:
    """DDL を (列の宣言部, PARTITIONED BY 以降) に割ります。"""
    text = ATHENA_DDL.read_text(encoding="utf-8")
    # **CREATE 文より前は見ない。** 冒頭の解説にコメントとして
    # 列の例が並んでおり、そこまで拾うと宣言済みとして通ってしまいます。
    body = text[text.index("CREATE EXTERNAL TABLE") :]
    head, sep, tail = body.partition("PARTITIONED BY")
    return head, (sep + tail)


def athena_columns() -> dict[str, str]:
    """Athena の DDL が宣言している列名 → 型を返します。

    **PARTITIONED BY 以降は列として数えません。**

    以前はここが本文の末尾まで読んでおり、`dt` と `hour`
    (パーティション列) が**レコードの列として宣言済み**に見えていました。
    Athena はパーティション列とデータ列の同名を許さない
    (infra/athena/table.sql の罠 1) ので、アプリが dt / hour を出し始めると
    **DDL がそもそも作れない**のに、この検査は通ってしまう ——
    service で実際に踏んだ罠を、検査の側が見逃す形になっていました。

    infra/duckdb/logs_source.py の athena_columns() は最初から
    PARTITIONED BY で切っており、そちらに揃えます。
    """
    columns: dict[str, str] = {}
    for name, raw_type in DDL_COLUMN.findall(_ddl_sections()[0]):
        columns[name] = raw_type.lower()
    return columns


def partition_columns() -> set[str]:
    """PARTITIONED BY が宣言しているパーティション列を返します。

    **レコード側で使ってはいけない名前**の一覧になります (罠 1)。
    """
    return {name for name, _ in DDL_COLUMN.findall(_ddl_sections()[1])}


def forbidden_parts_in_source() -> list[str] | None:
    """infra/duckdb/verify.py の FORBIDDEN_FIELD_PARTS を読み取ります。

    **ast で読む。** 正規表現で拾うと、書式を変えた瞬間に
    「読めなかった = ずれていない」と誤読する側へ倒れます。
    読めなかった場合は None を返し、呼び出し側が失敗として扱います。
    """
    import ast

    try:
        tree = ast.parse(FORBIDDEN_SOURCE.read_text(encoding="utf-8"))
    except (OSError, SyntaxError):
        return None
    for node in tree.body:
        if not isinstance(node, ast.Assign):
            continue
        for target in node.targets:
            if isinstance(target, ast.Name) and target.id == "FORBIDDEN_FIELD_PARTS":
                try:
                    value = ast.literal_eval(node.value)
                except ValueError:
                    return None
                return list(value) if isinstance(value, list) else None
    return None


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

    # **名前がイベント名と同じ形になっているか。**
    # 以前は正規表現の文字クラスで弾いていたが、それだと弾いた名前が
    # 「見なかったこと」になり、以降の検査を全部素通りしていた。
    malformed = sorted(name for name in fields if not FIELD_NAME.match(name))
    if malformed:
        failed = True
        print(f"NG  フィールド名の形が正しくないものが {len(malformed)} 件あります")
        print("    英小文字で始まり、英小文字・数字・アンダースコアだけを使ってください")
        print()
        for name in malformed:
            places = sorted({p for ps in fields[name].values() for p in ps})
            print(f"  {name!r}: {places[0]}")
        print()
    else:
        print("OK  フィールド名はすべてイベント名と同じ形です")

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

    # **禁止された項目を出していないか** (ADR 0010 の 4-5)。
    #
    # ここが CI で 4-5 を守る唯一の場所になる。実測の検査
    # (infra/duckdb/verify.py) は実環境が要り、CI では回らない。
    source_parts = forbidden_parts_in_source()
    if source_parts is None:
        failed = True
        print("NG  infra/duckdb/verify.py の FORBIDDEN_FIELD_PARTS を読み取れません")
        print("    一覧がずれていないことを確認できないため、失敗として扱います")
        print()
    elif sorted(source_parts) != sorted(FORBIDDEN_FIELD_PARTS):
        failed = True
        print("NG  禁止項目の一覧が infra/duckdb/verify.py とずれています")
        print(f"    こちら: {sorted(FORBIDDEN_FIELD_PARTS)}")
        print(f"    あちら: {sorted(source_parts)}")
        print()
        print("片方だけに足すと、静的検査と実測の結果が食い違います。両方に足してください。")
        print()
    else:
        print("OK  禁止項目の一覧が infra/duckdb/verify.py と一致しています")

    leaked = {
        name: [part for part in FORBIDDEN_FIELD_PARTS if part in name.lower()]
        for name in fields
    }
    leaked = {name: parts for name, parts in leaked.items() if parts}
    if leaked:
        failed = True
        print(f"NG  ログに出してはいけない項目が {len(leaked)} 件あります (ADR 0010 の 4-5)")
        print("    S3 に長期保管するため、一度出したものは事実上消せません")
        print()
        for name, parts in sorted(leaked.items()):
            places = sorted({p for ps in fields[name].values() for p in ps})
            print(f"  {name} ({', '.join(parts)} に一致): {places[0]}")
        print()
        print("値そのものを出さず、識別子や真偽値に置き換えてください")
        print("(例: session_id -> なし、email -> has_email)。")
        print()
    else:
        print("OK  ログに出してはいけない項目は使っていません")

    declared = athena_columns()
    partitions = partition_columns()

    # **パーティション列と同じ名前をレコードに出していないか** (罠 1)。
    #
    # Athena はパーティション列とデータ列の同名を許さない。
    # 出し始めた時点で DDL が作れなくなるが、アプリ側では何も起きない。
    collide = sorted(set(fields) & partitions)
    if collide:
        failed = True
        print(f"NG  パーティション列と同じ名前のフィールドが {len(collide)} 件あります")
        print(f"    パーティション列: {sorted(partitions)}")
        print("    Athena は両者の同名を許しません (infra/athena/table.sql の罠 1)")
        print()
        for name in collide:
            places = sorted({p for ps in fields[name].values() for p in ps})
            print(f"  {name}: {places[0]}")
        print()
        print("フィールド名を変えてください (service が LOCATION に逃げたのと同じ問題)。")
        print()
    else:
        print("OK  パーティション列と名前がぶつかるフィールドはありません")

    # **DDL の型と slog の型が一致しているか。**
    #
    # 名前ごとの型が 1 つでも、その型が DDL と食い違えば意味が無い。
    # Athena は ignore.malformed.json で**静かに NULL にする**ので、
    # 「クエリを書いたら全部 NULL だった」で初めて気づくことになる
    # (infra/athena/table.sql の罠 3)。
    mismatched: list[tuple[str, str, str, str]] = []
    unknown_ddl_types: list[tuple[str, str]] = []
    for name, kinds in sorted(fields.items()):
        ddl_type = declared.get(name)
        if ddl_type is None:
            continue
        want = DDL_TYPE_TO_SLOG.get(ddl_type)
        if want is None:
            unknown_ddl_types.append((name, ddl_type))
            continue
        for kind in sorted(kinds):
            if kind != want:
                mismatched.append((name, ddl_type, want, kind))
    if unknown_ddl_types:
        failed = True
        print(f"NG  DDL_TYPE_TO_SLOG に無い型の列が {len(unknown_ddl_types)} 件あります")
        print("    型を比べられないため、宣言と実際の食い違いを検出できません")
        print()
        for name, ddl_type in unknown_ddl_types:
            print(f"  {name}: {ddl_type}")
        print()
        print("このスクリプトの DDL_TYPE_TO_SLOG と")
        print("infra/duckdb/logs_source.py の _TYPE_MAP の両方に足してください。")
        print()
    if mismatched:
        failed = True
        print(f"NG  DDL の型と食い違うフィールドが {len(mismatched)} 件あります")
        print("    Athena は型の合わない値を静かに NULL にします (罠 3)")
        print()
        for name, ddl_type, want, got in mismatched:
            places = sorted(fields[name][got])
            print(f"  {name}: DDL は {ddl_type} (slog.{want} が要る) / 実際は slog.{got}")
            print(f"    {places[0]}")
        print()
        print("slog の型を直すか、infra/athena/table.sql の型を直してください。")
        print()
    elif not unknown_ddl_types:
        print("OK  フィールドの型が Athena の DDL と一致しています")

    # DDL に宣言されていないフィールドが無いか。
    missing = sorted(set(fields) - set(declared) - DDL_EXEMPT - partitions)
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
