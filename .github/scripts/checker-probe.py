#!/usr/bin/env python3
"""検査プローブ —— 静的検査そのものが「落とすべきものを落とす」かを実測する。

【なぜ要るか】

このリポジトリには、検査の検出力を測る仕組みが 2 つある:

    .github/scripts/arch-probe.sh      arch-lint の設定を検査する
    .github/scripts/mutation-probe.py  Go のテストを検査する

**ソースを読む 2 つの静的検査には、それが無かった。**

    .github/scripts/verify-log-events.py
    .github/scripts/verify-constraint-checks.py

どちらも「NG が 0 件」を CI で緑として報告する。しかし
**何も検出できない状態でも、出力は同じ「OK」になる。**
検査が壊れたことに気づく経路が 1 本も無い。

実際に 4 つ穴が空いていた (2026-08-24 のレビューで発見):

    1. フィールド名の正規表現が [a-z_]+ で、**数字を含む名前は
       どの検査にも当たらないまま素通り**していた
    2. DDL を PARTITIONED BY より先まで読んでいて、パーティション列
       (dt / hour) が**レコードの列として宣言済み**に見えていた
    3. slog の型と DDL の型を**誰も突き合わせていなかった**
       (Athena は型の合わない値を静かに NULL にする)
    4. 「テストがマイグレーションを読んでいるか」の判定に**コメントが
       数えられていた** —— このスクリプトが探している失敗形そのもの

1 と 4 は、**壊しても終了コード 0 のまま**だった (実測)。

【何をするか】

宣言した変異を 1 つずつ当て、検査が期待どおりの理由で落ちるかを見る。

  - 変異の前に、対象の検査が**通っている**ことを確認する
    (最初から赤いなら以降の実測は意味を持たない)
  - 置換対象が**ちょうど 1 か所**に当たることを確認する
  - **期待する NG の文言まで見る。** 終了コードだけで判定すると、
    別の理由で落ちたものを「検出できた」と数える ——
    このスクリプトを書く途中で実際に 1 度踏んだ
  - **どう終わっても元に戻す** (例外・Ctrl-C・SIGTERM のいずれでも)

DB もネットワークも要らないので CI で回せる。
"""

from __future__ import annotations

import os
import pathlib
import signal
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPTS = ROOT / ".github" / "scripts"

LOG_EVENTS = "verify-log-events.py"
CONSTRAINTS = "verify-constraint-checks.py"

RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
DIM = "\033[90m"
OFF = "\033[0m"

# 変異を当てる先。**どれも 1 か所にしか当たらない文字列**を選ぶ。
LOGGING = "apps/go-api/internal/logging/logging.go"
MAIN = "apps/go-api/cmd/api/main.go"
ROLE_TEST = "apps/go-api/internal/user/domain/model/role_test.go"
ROLE_MIGRATION = "apps/go-api/db/migrations/000003_add_user_role.up.sql"
DUCKDB_VERIFY = "infra/duckdb/verify.py"
CHECKER = f".github/scripts/{CONSTRAINTS}"
CONTACT_MIGRATION = "apps/go-api/db/migrations/000012_add_contact_messages.up.sql"

# 共通で差し替えるログ行。**起動時に必ず 1 回出る行**なので、
# 消えたときは置換が 0 件になり、このスクリプトが先に気づく。
VERSION_FIELD = 'slog.String("version", Version()),'


class Probe:
    def __init__(self, pid, checker, file, frm, to, expect_text, desc):
        self.id = pid
        self.checker = checker
        self.file = file
        self.frm = frm
        self.to = to
        # 期待する NG 行の文言。**NG で始まる行だけを見る** ——
        # 出力全体への部分一致にすると、**成功したときの OK 行にも
        # 当たってしまう。**
        #
        # 実際に踏んだ (レビュー指摘)。"ログに出してはいけない項目" は
        #   NG  ログに出してはいけない項目が 1 件あります   (検出した)
        #   OK  ログに出してはいけない項目は使っていません   (検出していない)
        # の**両方**に含まれる。しかも変異が別の検査にも当たるため
        # 終了コードは 1 になる。結果、**その検査を完全に殺しても
        # プローブは緑のまま**だった。
        self.expect_text = expect_text
        self.desc = desc


PROBES = [
    # ---- verify-log-events.py -------------------------------------------
    Probe("A", LOG_EVENTS, LOGGING, VERSION_FIELD,
          'slog.String("http2_streams", "x"),',
          "Athena の DDL に無いフィールドが",
          "数字を含む名前 (旧実装は正規表現に当たらず素通りしていた)"),
    Probe("B", LOG_EVENTS, LOGGING, VERSION_FIELD,
          'slog.String("Version", Version()),',
          "フィールド名の形が正しくないものが",
          "大文字を含む名前"),
    Probe("C", LOG_EVENTS, LOGGING, VERSION_FIELD,
          'slog.String("dt", "x"),',
          "パーティション列と同じ名前のフィールドが",
          "パーティション列との衝突 (罠 1。DDL がそもそも作れなくなる)"),
    Probe("D", LOG_EVENTS, LOGGING, VERSION_FIELD,
          'slog.Int64("version", 1),',
          "DDL の型と食い違うフィールドが",
          "DDL は string なのに slog.Int64 で出す (罠 3)"),
    Probe("E", LOG_EVENTS, LOGGING, VERSION_FIELD,
          'slog.String("session_id", "x"),',
          "ログに出してはいけない項目が",
          "ADR 0010 の 4-5 が禁じた項目"),
    Probe("F", LOG_EVENTS, LOGGING, VERSION_FIELD,
          'slog.Duration("version", 0),',
          "Athena に載せられない型のフィールドが",
          "slog.Duration (ナノ秒になる)"),
    Probe("G", LOG_EVENTS, LOGGING, VERSION_FIELD,
          'slog.Int64("addr", 1),',
          "同じ名前で型が違うフィールドが",
          "同じ名前を 2 つの型で出す"),
    Probe("H", LOG_EVENTS, MAIN, 'slog.Info("server_started"',
          'slog.Info("サーバを起動しました"',
          "自由文の msg が",
          "msg を日本語の自由文にする (ADR 0010 の 4-2)"),
    Probe("I", LOG_EVENTS, DUCKDB_VERIFY, '    "cookie",\n', "",
          "禁止項目の一覧が infra/duckdb/verify.py とずれています",
          "禁止項目の一覧を片方だけ削る"),

    # ---- verify-constraint-checks.py ------------------------------------
    Probe("J", CONSTRAINTS, ROLE_MIGRATION, "users_role_valid",
          "users_role_valid_zz",
          "守る検査が宣言されていない CHECK 制約が",
          "検査を宣言していない制約を足す"),
    Probe("K", CONSTRAINTS, ROLE_TEST,
          "func TestRole_DefinitionsAgreeAcrossSources(",
          "func TestRole_DefinitionsAgreeAcrossSourcesZZ(",
          "宣言と実物が食い違っている検査が",
          "対応表が指すテストを改名する"),
    Probe("L", CONSTRAINTS, ROLE_TEST,
          '\tsrc := readRepoFile(t, "apps/go-api/db/migrations/'
          '000003_add_user_role.up.sql")',
          '\tsrc := "" // apps/go-api/db/migrations/000003_add_user_role.up.sql',
          "宣言と実物が食い違っている検査が",
          "マイグレーションの読み取りをコメントへ移す (旧実装は素通り)"),
    Probe("M", CONSTRAINTS, CHECKER,
          '    "users_role_valid": (\n'
          '        "apps/go-api/internal/user/domain/model/role_test.go",\n'
          '        "TestRole_DefinitionsAgreeAcrossSources",\n'
          '    ),\n',
          "",
          "守る検査が宣言されていない CHECK 制約が",
          "対応表から 1 件消す"),
    Probe("N", CONSTRAINTS, CONTACT_MIGRATION,
          "CREATE INDEX contact_ip_scrub_idx",
          "ALTER TABLE contact_messages DROP COLUMN status;\n"
          "CREATE INDEX contact_ip_scrub_idx",
          "もう存在しない CHECK 制約が対応表に",
          "DROP COLUMN で消える CHECK を追えているか"),
    Probe("O", CONSTRAINTS, ROLE_TEST,
          '\tassertSameSet(t, "DB の CHECK 制約", registered, valuesFromMigration(t))',
          '\t// assertSameSet(t, "DB の CHECK 制約", registered, valuesFromMigration(t))',
          "宣言と実物が食い違っている検査が",
          "コメントアウトした呼び出しからヘルパを引き込めないか"),
]


class Restorer:
    """壊したファイルを必ず戻すための後始末。

    **finally だけでは足りない。** SIGTERM では finally が走らない
    (mutation-probe.py の Restorer に実測がある)。
    このスクリプトは**検査そのもの**を壊すので、戻し損ねると
    以降の CI がすべて嘘の結果を返す。
    """

    def __init__(self):
        self.pending: dict[pathlib.Path, str] = {}

    def hold(self, path: pathlib.Path, content: str) -> None:
        self.pending[path] = content

    def restore(self) -> None:
        for path, content in self.pending.items():
            path.write_text(content, encoding="utf-8")
        self.pending = {}

    def install(self) -> None:
        def handler(signum, _frame):
            self.restore()
            print(f"\n{YELLOW}シグナル {signum} を受けたので、"
                  f"壊したファイルを元に戻して終了します。{OFF}", file=sys.stderr)
            signal.signal(signum, signal.SIG_DFL)
            os.kill(os.getpid(), signum)

        for sig in (signal.SIGTERM, signal.SIGHUP):
            signal.signal(sig, handler)


def run_checker(name: str) -> tuple[int, str]:
    proc = subprocess.run([str(SCRIPTS / name)], capture_output=True, text=True)
    return proc.returncode, proc.stdout + proc.stderr


def main() -> int:
    # 壊す前の状態を確認する。ここが赤いと以降の結果はすべて意味を失う。
    print(f"{DIM}壊す前の検査を確認しています...{OFF}", flush=True)
    for checker in (LOG_EVENTS, CONSTRAINTS):
        rc, out = run_checker(checker)
        if rc != 0:
            print(f"{RED}壊す前の時点で {checker} が落ちています。"
                  f"先にそちらを直してください。{OFF}", file=sys.stderr)
            print(out[-2000:], file=sys.stderr)
            return 1

    restorer = Restorer()
    restorer.install()

    print()
    print(f"{'ID':<4}{'結果':<8}{'検査':<30}内容")
    print("-" * 96)

    failed = 0
    for probe in PROBES:
        path = ROOT / probe.file
        original = path.read_text(encoding="utf-8")

        # 置換対象が実在し、1 か所だけに当たるか。
        # **0 件を素通しすると、何も壊さずに「落ちた」を得る**ことになり、
        # 検出力の証拠が捏造される。
        count = original.count(probe.frm)
        if count != 1:
            print(f"{probe.id:<4}{RED}NG{OFF}      "
                  f"{probe.checker:<30}置換対象が {count} 箇所 (1 であるべき): "
                  f"{probe.file}")
            failed += 1
            continue

        restorer.hold(path, original)
        try:
            path.write_text(original.replace(probe.frm, probe.to), encoding="utf-8")
            rc, out = run_checker(probe.checker)
        finally:
            restorer.restore()

        if rc == 0:
            ok, note = False, "落ちなかった (この検査は壊れても気づけない)"
        elif not any(probe.expect_text in line
                     for line in out.splitlines() if line.startswith("NG")):
            # **NG 行だけを見る。** 出力全体への部分一致だと、
            # 同じ語を含む OK 行に当たって素通りする (Probe の説明を参照)。
            ok, note = False, f"落ちたが理由が違う (NG 行に {probe.expect_text!r} が無い)"
        else:
            ok, note = True, ""

        mark = f"{GREEN}OK{OFF}" if ok else f"{RED}NG{OFF}"
        print(f"{probe.id:<4}{mark:<17}{probe.checker:<30}{probe.desc}")
        if not ok:
            failed += 1
            print(f"    {YELLOW}└ {note}{OFF}")

    print()
    if failed:
        print(f"{RED}{failed} / {len(PROBES)} 件の変異を検出できませんでした{OFF}")
        print("検査が壊れているか、変異が古くなっています。")
        return 1
    print(f"{GREEN}変異 {len(PROBES)} 件すべてを、期待どおりの理由で検出しました{OFF}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
