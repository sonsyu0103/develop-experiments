#!/usr/bin/env python3
"""DB の CHECK 制約に、それを守る検査が付いているかを確かめる。

**Go のテストは、自分が存在しないことを検出できない。**

値の突き合わせ (定数 / CHECK 制約 / 仕様書) は、それぞれのドメインの
テストが持っている —— そのほうが定数の隣にあり、落ちたときのメッセージが
ドメインの言葉になるためになる。しかしその形では、
**「そもそも検査を書き忘れた制約」に誰も気づけない。**

実際に漏れた (2026-08-19 に発見):

    image/domain/model/image_test.go は
    「DB の CHECK 制約 (images_content_type_valid) と揃っている必要がある」
    と書きながら、比較していたのはテスト内のリテラル同士だった。
    制約から image/webp を消しても緑のまま。
    テストが 1 本あるように見えていたので、レビューでも見落とされた。

そこでこのスクリプトは**値を見ない**。見るのは対応関係だけになる。

    1. マイグレーションから、値を持つ CHECK 制約をすべて拾う
       (長さ = char_length(...)、列挙 = ... IN ('a', 'b'))
    2. GUARDED に、制約ごとの「守っているテスト」を宣言しておく
    3. 宣言の無い制約があれば落とす
    4. 宣言が嘘になっていないか (テストファイルが存在し、その名前の
       テストがあり、実際にそのマイグレーションを読んでいるか) も見る

4 が要る。**対応表だけなら、テストを消しても表は残る。**

値そのものの検査ではないため、CI では Go のテストと両方を回す。
このスクリプトは DB もネットワークも要らない。

**マイグレーションは前へ直す** (既に走ったものは書き換えない) ので、
制約を消すときは新しい番号で DROP CONSTRAINT を足すことになる。
そのため、拾った制約から**後の up.sql が落としたものを引く** ——
引かないと、消した制約に検査を宣言し続けることを要求され、しかも
「もう無い制約が対応表に残っています」と同時に言われて、
どちらにも直しようが無くなる。

設計の背景は docs/adr/0003-open-questions.md の#8。
"""

from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
MIGRATIONS = ROOT / "apps" / "go-api" / "db" / "migrations"

# **「値を持つ」の判定は広く取る。** 書き方を列挙する形にすると、
# 別の書き方をした制約が「値を持たない」として静かに検出から外れる ——
# このスクリプトは検査の不在を見つけるためだけに在るので、
# 見逃しは存在意義そのものを失わせる。
#
# そこで判定は「文字列リテラルを含む」か「長さの関数を呼んでいる」の
# どちらかにする。char_length / length / octet_length の違い、
# BETWEEN と <= の違い、IN と = の違いのいずれにも依存しない。
LITERAL = re.compile(r"'[^']*'")
LENGTH_CALL = re.compile(r"\w*length\s*\(", re.I)

# 下は**種別の表示に使うだけ**で、検出には使わない。
LENGTH = re.compile(r"\w*length\s*\(", re.I)
ENUM = re.compile(r"\w+\s+IN\s*\(\s*'")
CONDITIONAL = re.compile(r"\w+\s*(?:=|<>)\s*'")

# 制約名 -> (テストファイル, テスト名)。
#
# **テストファイルは「その制約を守っている」ものを書く。** 単に制約名が
# 出てくるだけのファイルではない (コメントで言及しているだけの箇所がある)。
GUARDED: dict[str, tuple[str, str]] = {
    # 長さ
    "threads_title_length": (
        "apps/go-api/internal/thread/domain/model/thread_test.go",
        "TestTitleLengthLimit_AgreesAcrossSources",
    ),
    "comments_body_length": (
        "apps/go-api/internal/comment/domain/model/comment_test.go",
        "TestLengthLimits_AgreeAcrossSources",
    ),
    "comments_author_length": (
        "apps/go-api/internal/comment/domain/model/comment_test.go",
        "TestLengthLimits_AgreeAcrossSources",
    ),
    "users_display_name_length": (
        "apps/go-api/internal/user/domain/model/user_test.go",
        "TestDisplayNameMaxLength_AgreesWithConstraint",
    ),
    "idempotency_keys_key_length": (
        "apps/go-api/internal/idempotency/idempotency_test.go",
        "TestMaxKeyLength_AgreesAcrossSources",
    ),
    "moderation_reason_length": (
        "apps/go-api/internal/moderation/usecase/interactor_test.go",
        "TestMaxReasonLength_AgreesAcrossSources",
    ),
    # **等値ではなく「収まること」を見る検査。** Go 側に定数が無く、
    # target_id は入力ではなく組み立てた文字列になるため。
    "moderation_target_id_length": (
        "apps/go-api/internal/moderation/usecase/interactor_test.go",
        "TestTargetIDLength_FitsConstraint",
    ),
    "reports_note_length": (
        "apps/go-api/internal/moderation/usecase/report_test.go",
        "TestMaxNoteLength_AgreesAcrossSources",
    ),
    "contact_name_length": (
        "apps/go-api/internal/contact/domain/model/message_test.go",
        "TestLengthLimits_AgreeAcrossSources",
    ),
    "contact_email_length": (
        "apps/go-api/internal/contact/domain/model/message_test.go",
        "TestLengthLimits_AgreeAcrossSources",
    ),
    "contact_subject_length": (
        "apps/go-api/internal/contact/domain/model/message_test.go",
        "TestLengthLimits_AgreeAcrossSources",
    ),
    "contact_body_length": (
        "apps/go-api/internal/contact/domain/model/message_test.go",
        "TestLengthLimits_AgreeAcrossSources",
    ),
    # 列挙
    "users_role_valid": (
        "apps/go-api/internal/user/domain/model/role_test.go",
        "TestRole_DefinitionsAgreeAcrossSources",
    ),
    "images_status_valid": (
        "apps/go-api/internal/image/domain/model/image_test.go",
        "TestImageDefinitions_AgreeAcrossSources",
    ),
    "images_kind_valid": (
        "apps/go-api/internal/image/domain/model/image_test.go",
        "TestImageDefinitions_AgreeAcrossSources",
    ),
    "images_content_type_valid": (
        "apps/go-api/internal/image/domain/model/image_test.go",
        "TestImageDefinitions_AgreeAcrossSources",
    ),
    "moderation_action_valid": (
        "apps/go-api/internal/moderation/domain/model/action_test.go",
        "TestActionType_DefinitionsAgreeAcrossSources",
    ),
    "moderation_target_type_valid": (
        "apps/go-api/internal/moderation/domain/model/action_test.go",
        "TestTargetType_DefinitionsAgreeAcrossSources",
    ),
    "reports_target_type_valid": (
        "apps/go-api/internal/moderation/domain/model/report_test.go",
        "TestReportEnums_AgreeAcrossSources",
    ),
    "reports_reason_valid": (
        "apps/go-api/internal/moderation/domain/model/report_test.go",
        "TestReportEnums_AgreeAcrossSources",
    ),
    "reports_status_valid": (
        "apps/go-api/internal/moderation/domain/model/report_test.go",
        "TestReportEnums_AgreeAcrossSources",
    ),
    "contact_status_valid": (
        "apps/go-api/internal/contact/domain/model/message_test.go",
        "TestStatuses_AgreeWithConstraint",
    ),
    # 条件つき制約。**列挙の値を本文に直書きしている**ので、
    # 値を改名すると列挙側の検査は落ちるのにこちらは古い値を参照したまま
    # 残る。そのとき壊れるのは「その状態の行だけが保存できない」という
    # 見つけにくい形になる。リテラルが既知の値かどうかを見る。
    "images_committed_at_matches_status": (
        "apps/go-api/internal/image/domain/model/image_test.go",
        "TestConditionalConstraints_UseKnownValues",
    ),
    "images_attached_at_requires_commit": (
        "apps/go-api/internal/image/domain/model/image_test.go",
        "TestConditionalConstraints_UseKnownValues",
    ),
    "reports_resolution_complete": (
        "apps/go-api/internal/moderation/domain/model/report_test.go",
        "TestConditionalConstraints_UseKnownValues",
    ),
    "reports_thread_id_matches_target": (
        "apps/go-api/internal/moderation/domain/model/report_test.go",
        "TestConditionalConstraints_UseKnownValues",
    ),
    "contact_sent_complete": (
        "apps/go-api/internal/contact/domain/model/message_test.go",
        "TestConditionalConstraints_UseKnownValues",
    ),
}

# 検査を付けない制約と、その理由。**空にしておくこと自体に意味がある** ——
# ここに足すときは、なぜ検査できないのかを書く。
EXEMPT: dict[str, str] = {}

# 【適用時に落ちうる CHECK 制約】
#
# ALTER TABLE ... ADD CONSTRAINT ... CHECK は**既存の全行を検証する。**
# 同じマイグレーションで足したばかりの列を参照していて、その列に
# DEFAULT も埋め戻しも無いなら、**既存行は NULL のまま検証に入る。**
# 1 行でも違反すれば ALTER が落ち、golang-migrate は dirty で止まる。
#
# 既に適用済みで、もう直せないもの。**理由つきでここに載せる。**
APPLY_UNSAFE_EXEMPT: dict[str, str] = {
    "reports_thread_id_matches_target": (
        "000009 で追加済み。ADD COLUMN target_thread_id の直後に "
        "「comment の通報なら NOT NULL」を要求しており、000008 の時点で "
        "コメントの通報が 1 行でもある環境では ALTER が落ちる (実測)。"
        "000009 自体が『既に develop に入っており適用済みの環境がありうる』"
        "と書いていながら、スキーマの状態だけ見てデータを見ていなかった。"
        "**マイグレーションは前へ直す方式なので、いま書き換えても "
        "適用済みの環境は救えない。** 同じ形を二度と入れないための記録として残す。"
    ),
}


# 制約の生き死にに関わる文。**出現順に畳み込む**ので 1 本の正規表現にする。
#
# 引用符つきの識別子 ("foo") も拾う —— (\w+) だけだと開き引用符に当たって
# 一致せず、**落としたはずの制約が生きていることになる。**
STATEMENT = re.compile(
    r'CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?'
    r'|ALTER\s+TABLE\s+(?:ONLY\s+)?(?:IF\s+EXISTS\s+)?"?(\w+)"?'
    r'|DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?'
    r'|DROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?'
    r'|DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?'
    r'|CONSTRAINT\s+"?(\w+)"?\s+CHECK\s*\(',
    re.I,
)


def _paren_body(text: str, open_index: int) -> str:
    """開き括弧の位置から、対応する閉じ括弧までの中身を返します。

    **括弧の対応を数えます。** 正規表現で閉じ括弧まで取ると、
    入れ子のある制約 (images_committed_at_matches_status) で
    途中まで拾ってしまいます。
    """
    depth = 0
    for i in range(open_index, len(text)):
        if text[i] == "(":
            depth += 1
        elif text[i] == ")":
            depth -= 1
            if depth == 0:
                return text[open_index + 1:i]
    return ""


def alive_constraints() -> dict[str, tuple[str, str]]:
    """生き残った CHECK 制約の 名前 -> (テーブル, 本体) を返します。

    **ADD と DROP を出現順に畳み込みます。** 落としてから足し直した制約は
    生きており、足してから落とした制約は死んでいる ——
    どちらか片方だけを見ると、その区別が付きません。

    **制約が消えるのは DROP CONSTRAINT だけではありません。**

        DROP TABLE   そのテーブルの制約はすべて消える
        DROP COLUMN  その列を参照している CHECK は一緒に消える (Postgres の挙動)

    どちらも追わないと、消えた制約に検査を要求され、同時に
    「もう無い制約が対応表に残っています」と言われて詰みます ——
    このスクリプトが直そうとしている状態そのものになります。
    """
    alive: dict[str, tuple[str, str]] = {}
    for path in sorted(MIGRATIONS.glob("*.up.sql")):
        body = _sql_body(path)
        table = ""
        for m in STATEMENT.finditer(body):
            created, altered, dropped_table, dropped_c, dropped_col, added = m.groups()
            if created or altered:
                table = (created or altered).lower()
            elif dropped_table:
                gone = dropped_table.lower()
                alive = {n: v for n, v in alive.items() if v[0] != gone}
            elif dropped_c:
                alive.pop(dropped_c, None)
            elif dropped_col:
                # **その列を参照している CHECK だけを落とす。**
                # 語として一致させる (status が image_status に当たらないように)。
                col = re.compile(rf"\b{re.escape(dropped_col)}\b", re.I)
                alive = {
                    n: v for n, v in alive.items()
                    if not (v[0] == table and col.search(v[1]))
                }
            elif added:
                alive[added] = (table, _paren_body(body, m.end() - 1))
    return alive


# 同じファイル内で足された列。DEFAULT の有無まで見る。
ADD_COLUMN = re.compile(
    r'ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?([^,;]*)', re.I)

# 埋め戻し。UPDATE が 1 つでもあれば「埋めている」とみなす。
#
# **中身までは見ない。** 見ようとすると SQL を解釈することになり、
# 書き方を変えた瞬間に静かに検出から外れる ——
# このスクリプトが避けたい失敗そのものになる。
# 埋め戻しの正しさは、落ちたときに人が読む。
BACKFILL = re.compile(r'^\s*UPDATE\s+"?(\w+)"?', re.I | re.M)

# ALTER TABLE で足す CHECK 制約 (CREATE TABLE の中のものは対象外 ——
# 新しい表には検証すべき既存行が無い)。
ALTER_ADD_CHECK = re.compile(
    r'ALTER\s+TABLE\s+(?:ONLY\s+)?"?(\w+)"?\s+ADD\s+CONSTRAINT\s+"?(\w+)"?\s+CHECK\s*\(',
    re.I | re.S)


def apply_unsafe() -> list[tuple[str, str, str]]:
    """適用時に落ちうる CHECK 制約を (マイグレーション, 制約名, 理由) で返します。

    判定は狭く取ります —— **同じファイルで足した列**を参照していて、
    その列に DEFAULT が無く、制約より前にその表への UPDATE も無い場合だけ。

    広く「ALTER TABLE ADD CONSTRAINT はすべて危ない」とすると、
    000013 のような**制約を緩める**変更まで鳴り、
    無視される検査になります。狭く取るぶん見逃す形はあります:

      - 既存の列に、既存データが違反する制約を足す場合
      - 埋め戻しの UPDATE が実は対象を埋めていない場合

    どちらも「書いた人が既存データを考えた」形跡はあるので、
    考えた形跡が無いもの (足した直後に NOT NULL を要求する) に絞ります。
    """
    found: list[tuple[str, str, str]] = []
    for path in sorted(MIGRATIONS.glob("*.up.sql")):
        body = _sql_body(path)
        added = {
            name: ("DEFAULT" in rest.upper())
            for name, rest in ADD_COLUMN.findall(body)
        }
        if not added:
            continue
        for m in ALTER_ADD_CHECK.finditer(body):
            table, name = m.group(1), m.group(2)
            inner = _paren_body(body, m.end() - 1)
            # 制約より前に、その表を埋め戻しているか。
            backfilled = any(
                t.lower() == table.lower() and pos < m.start()
                for t, pos in ((mm.group(1), mm.start()) for mm in BACKFILL.finditer(body))
            )
            if backfilled:
                continue
            for col, has_default in added.items():
                if has_default:
                    continue
                if re.search(rf"\b{re.escape(col)}\b", inner):
                    found.append((
                        path.name, name,
                        f"同じ版で足した {col} に DEFAULT も埋め戻しも無い",
                    ))
                    break
    return found


def _sql_body(path: pathlib.Path) -> str:
    """コメント行を落とした SQL を返します。

    制約の書き方を説明している行に char_length(...) が現れるため。
    """
    return "\n".join(
        line for line in path.read_text(encoding="utf-8").split("\n")
        if not line.strip().startswith("--")
    )


def constraints() -> list[tuple[str, str, str]]:
    """(マイグレーション, 制約名, 種別) を返します。

    **括弧の対応を数えて CHECK の本体を切り出します。** 正規表現で
    閉じ括弧まで取ると、入れ子のある制約 (images_committed_at_matches_status)
    で途中まで拾ってしまいます。また索引の WHERE 句にも IN (...) が
    現れるため、CHECK の中だけを見る必要があります。
    """
    found: list[tuple[str, str, str]] = []
    alive = alive_constraints()   # 名前 -> (テーブル, 本体)
    for path in sorted(MIGRATIONS.glob("*.up.sql")):
        body = _sql_body(path)
        for m in re.finditer(r"(?:CONSTRAINT\s+(\w+)\s+)?CHECK\s*\(", body):
            name = m.group(1)
            start = m.end() - 1
            depth = 0
            inner = ""
            for i in range(start, len(body)):
                if body[i] == "(":
                    depth += 1
                elif body[i] == ")":
                    depth -= 1
                    if depth == 0:
                        inner = body[start + 1:i]
                        break
            # 値を持たない制約 (NULL の有無だけを見るものなど) は対象外。
            if not (LITERAL.search(inner) or LENGTH_CALL.search(inner)):
                continue
            kinds = []
            if LENGTH.search(inner):
                kinds.append("長さ")
            if ENUM.search(inner):
                kinds.append("列挙")
            if CONDITIONAL.search(inner):
                kinds.append("条件")
            if not kinds:
                # リテラルはあるのに形が読めない。**素通しにしない。**
                kinds.append("解釈できない")
            if name is None:
                # 無名の CHECK は対応表に載せられない。名前を付けさせる。
                found.append((path.name, "(無名)", " / ".join(kinds)))
                continue
            if name not in alive:
                # 後の版が DROP CONSTRAINT で落としている。
                # **前へ直す方式では、消した制約もここに残り続ける。**
                continue
            found.append((path.name, name, " / ".join(kinds)))

    # **同じ名前は最後の定義だけ残す。** 前へ直す方式では、制約は
    # 後の版で DROP されて張り替えられる (000006 -> 000013)。
    # 全部残すと件数が水増しされ、しかも報告が**古いほうのファイル**を
    # 指す —— 「死んだ定義を見に行かせる」ことになる。
    latest: dict[str, tuple[str, str, str]] = {}
    unnamed = [row for row in found if row[1] == "(無名)"]
    for row in found:
        if row[1] != "(無名)":
            latest[row[1]] = row
    return unnamed + list(latest.values())


def func_body(src: str, name: str) -> str | None:
    """関数 1 つの本文を切り出します。

    **gofmt はトップレベルの閉じ括弧を 1 桁目に置く**ので、
    宣言から次の "\n}" までがその関数になります。
    """
    for head in (f"func {name}(", f"func (t *testing.T) {name}("):
        start = src.find(head)
        if start >= 0:
            end = src.find("\n}", start)
            return src[start:end] if end >= 0 else src[start:]
    return None


def reach(src: str, name: str) -> str | None:
    """テスト本文と、そこから呼んでいる**同じファイル内の関数**を連結します。

    **1 段だけ辿る。** 既存の検査はマイグレーションの読み取りを
    ヘルパに切り出しているものがあり (role_test.go の valuesFromMigration
    など)、本文だけを見ると「読んでいない」と誤検出する。

    一方でファイル全体を見るのは駄目になる —— 1 ファイルに複数の宣言が
    あるとき、片方をリテラル比較に書き換えても、もう片方が残す
    "db/migrations/" でファイル検査が通ってしまう。
    **このスクリプトが見つけようとしている失敗形そのもの**なので、
    「その検査から辿り着ける範囲」に限る。
    """
    body = func_body(src, name)
    if body is None:
        return None
    seen = [body]
    # **呼び先の抽出でもコメントを剥がす。** 剥がさないと、
    # コメントアウトした呼び出し (// readMigration(t)) から
    # ヘルパの本文を引き込めてしまう —— そのヘルパが
    # "db/migrations/" を直書きしていれば、**何も読んでいないのに通る。**
    # 判定の側だけ直しても、同じ形の穴が呼び先の側に残る。
    for callee in sorted(set(
            re.findall(r"\b([a-zA-Z]\w*)\s*\(", _without_comments(body)))):
        if callee == name:
            continue
        called = func_body(src, callee)
        if called is not None:
            seen.append(called)
    return "\n".join(seen)


def _without_comments(src: str) -> str:
    """Go のコメントを落とします。

    **「マイグレーションを読んでいる」の判定にコメントを数えない。**
    判定は "db/migrations/" という文字列の有無で行っているので、
    コメントに書いてあるだけで通ってしまう —— 対応表の側では
    「制約名が出てくるだけのファイルは駄目」と言っておきながら、
    本文の側で同じ抜け道を空けていた。

    このスクリプトが探しているのは「検査したつもりで何も検査していない」
    形そのものなので、ここを緩めると存在意義が消える。

    落としすぎても「読んでいない」と鳴るだけで、見逃す側には倒れない。
    """
    out = []
    for line in src.split("\n"):
        i = line.find("//")
        out.append(line[:i] if i >= 0 else line)
    return re.sub(r"/\*.*?\*/", "", "\n".join(out), flags=re.S)


def main() -> int:
    failed = False
    found = constraints()

    print(f"値を持つ CHECK 制約: {len(found)} 件")

    unnamed = [(f, k) for f, n, k in found if n == "(無名)"]
    if unnamed:
        failed = True
        print(f"NG  名前の無い CHECK 制約が {len(unnamed)} 件あります")
        print("    名前が無いと、検査との対応を宣言できません")
        print()
        for f, k in unnamed:
            print(f"  {f} ({k})")
        print()
        print("CONSTRAINT <名前> CHECK (...) の形にしてください。")
        print()
    else:
        print("OK  値を持つ CHECK 制約はすべて名前が付いています")

    unreadable = [(f, n) for f, n, k in found if "解釈できない" in k]
    if unreadable:
        failed = True
        print(f"NG  リテラルを持つが形を読めない CHECK 制約が {len(unreadable)} 件あります")
        print("    種別が分からないと、どんな検査が要るかを判断できません")
        print()
        for f, n in unreadable:
            print(f"  {n}  ({f})")
        print()
        print("このスクリプトの ENUM / LENGTH / CONDITIONAL に形を足してください。")
        print()
    else:
        print("OK  値を持つ CHECK 制約はすべて形を読み取れています")

    names = {n for _, n, _ in found if n != "(無名)"}

    missing = sorted(names - set(GUARDED) - set(EXEMPT))
    if missing:
        failed = True
        print(f"NG  守る検査が宣言されていない CHECK 制約が {len(missing)} 件あります")
        print("    値がずれても、気づけるのは本番だけになります")
        print()
        for name in missing:
            place = next(f for f, n, _ in found if n == name)
            kind = next(k for _, n, k in found if n == name)
            print(f"  {name}  ({place} / {kind})")
        print()
        print("定数の隣に横断検査を足し、GUARDED に宣言してください。")
        print("検査できない事情がある場合は EXEMPT に理由つきで入れます。")
        print()
    else:
        print("OK  すべての CHECK 制約に、守る検査が宣言されています")

    # 消えた制約が対応表に残っていないか。
    stale = sorted((set(GUARDED) | set(EXEMPT)) - names)
    if stale:
        failed = True
        print(f"NG  もう存在しない CHECK 制約が対応表に {len(stale)} 件残っています")
        print()
        for name in stale:
            print(f"  {name}")
        print()
        print("GUARDED / EXEMPT から消してください。")
        print()
    else:
        print("OK  対応表に、もう無い制約は残っていません")

    # **適用したときに落ちないか。**
    unsafe = apply_unsafe()
    unexpected = [row for row in unsafe if row[1] not in APPLY_UNSAFE_EXEMPT]
    if unexpected:
        failed = True
        print(f"NG  適用時に落ちうる CHECK 制約が {len(unexpected)} 件あります")
        print("    ADD CONSTRAINT ... CHECK は既存の全行を検証します。")
        print("    1 行でも違反すると ALTER が落ち、golang-migrate は dirty で止まります")
        print()
        for place, name, why in unexpected:
            print(f"  {name}  ({place})")
            print(f"    {why}")
        print()
        print("列に DEFAULT を付けるか、制約より前に UPDATE で埋め戻すか、")
        print("NOT VALID で足して後の版で VALIDATE CONSTRAINT してください。")
        print("既に適用済みで直せない場合は APPLY_UNSAFE_EXEMPT に理由つきで入れます。")
        print()
    else:
        print("OK  適用時に落ちうる CHECK 制約はありません"
              + (f" (既知の {len(APPLY_UNSAFE_EXEMPT)} 件を除く)"
                 if APPLY_UNSAFE_EXEMPT else ""))

    # 免除に載せたまま直った / 消えたものが残っていないか。
    stale_exempt = sorted(set(APPLY_UNSAFE_EXEMPT) - {row[1] for row in unsafe})
    if stale_exempt:
        failed = True
        print(f"NG  APPLY_UNSAFE_EXEMPT に、もう当てはまらない制約が "
              f"{len(stale_exempt)} 件あります")
        print()
        for name in stale_exempt:
            print(f"  {name}")
        print()
        print("直った (あるいは消えた) なら、免除も消してください。")
        print()

    # **対応表が嘘になっていないか。** ここが本体になる ——
    # 表だけならテストを消しても残る。
    broken = []
    for name, (rel, test) in sorted(GUARDED.items()):
        path = ROOT / rel
        if not path.exists():
            broken.append((name, rel, "ファイルが無い"))
            continue
        src = path.read_text(encoding="utf-8")
        body = reach(src, test)
        if body is None:
            broken.append((name, rel, f"{test} が無い"))
        elif "db/migrations/" not in _without_comments(body):
            broken.append((name, rel, f"{test} からマイグレーションに辿り着けない"))
    if broken:
        failed = True
        print(f"NG  宣言と実物が食い違っている検査が {len(broken)} 件あります")
        print()
        for name, rel, why in broken:
            print(f"  {name}: {rel} —— {why}")
        print()
    else:
        print("OK  宣言された検査はすべて実在し、マイグレーションを読んでいます")

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
