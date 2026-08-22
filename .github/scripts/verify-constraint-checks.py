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

設計の背景は docs/adr/0003-open-questions.md の#8。
"""

from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
MIGRATIONS = ROOT / "apps" / "go-api" / "db" / "migrations"

# 値を持つ CHECK 制約の形。
LENGTH = re.compile(r"char_length\((\w+)\)\s*(?:BETWEEN\s*\d+\s*AND\s*(\d+)|<=\s*(\d+))")
ENUM = re.compile(r"(\w+)\s+IN\s*\(((?:\s*'[^']+'\s*,?)+)\)")

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
}

# 検査を付けない制約と、その理由。**空にしておくこと自体に意味がある** ——
# ここに足すときは、なぜ検査できないのかを書く。
EXEMPT: dict[str, str] = {}


def constraints() -> list[tuple[str, str, str]]:
    """(マイグレーション, 制約名, 種別) を返します。

    **括弧の対応を数えて CHECK の本体を切り出します。** 正規表現で
    閉じ括弧まで取ると、入れ子のある制約 (images_committed_at_matches_status)
    で途中まで拾ってしまいます。また索引の WHERE 句にも IN (...) が
    現れるため、CHECK の中だけを見る必要があります。
    """
    found = []
    for path in sorted(MIGRATIONS.glob("*.up.sql")):
        # コメント行は落とす。制約の書き方を説明している行に
        # char_length(...) が現れるため。
        body = "\n".join(
            line for line in path.read_text().split("\n")
            if not line.strip().startswith("--")
        )
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
            kinds = []
            if LENGTH.search(inner):
                kinds.append("長さ")
            if ENUM.search(inner):
                kinds.append("列挙")
            if not kinds:
                continue
            if name is None:
                # 無名の CHECK は対応表に載せられない。名前を付けさせる。
                found.append((path.name, "(無名)", " / ".join(kinds)))
                continue
            found.append((path.name, name, " / ".join(kinds)))
    return found


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

    # **対応表が嘘になっていないか。** ここが本体になる ——
    # 表だけならテストを消しても残る。
    broken = []
    for name, (rel, test) in sorted(GUARDED.items()):
        path = ROOT / rel
        if not path.exists():
            broken.append((name, rel, "ファイルが無い"))
            continue
        src = path.read_text()
        if f"func {test}(" not in src:
            broken.append((name, rel, f"{test} が無い"))
        elif "db/migrations/" not in src:
            broken.append((name, rel, "マイグレーションを読んでいない"))
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
