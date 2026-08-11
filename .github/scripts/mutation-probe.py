#!/usr/bin/env python3
"""変異プローブ —— 追加したテストが実際に効くかを、実装をわざと壊して実測する。

「テストが通った」は「テストが何かを守っている」ことを意味しない。
実装を壊してもテストが通るなら、そのテストは何も守っていない。

.github/scripts/arch-probe.sh が境界検査そのものを検査するのと同じ思想を、
ユニットテストに当てはめたもの。違いは対象の寿命にある。

  境界プローブ    設定 (arch ファイル) を検査する。設定は安定しているので
                  プローブをリポジトリに固定し、CI で常時回せる
  変異プローブ    実装を検査する。壊し方は変更のたびに陳腐化するので、
                  **変異リストは PR ごとの使い捨て**にする

そのため CI には載せない。このスクリプトが引き受けるのは、
毎回書き捨てていた部分のうち **間違えると害が大きいところだけ**:

  - 壊す前に、そのテストが通っていることの確認 (最初から赤いなら実測に意味がない)
  - 置換対象が実在し、意図した箇所だけに当たることの確認
  - **どう終わっても元に戻すこと** (例外・Ctrl-C・失敗のいずれでも)
  - 「ビルドが落ちた」を「テストが落ちた」と混同しないこと

最後の 1 つは実際に踏んだ。LEFT JOIN を INNER JOIN に変えたところ
sqlc の推論型が変わってビルドが落ちた。テストは赤くなるが、
それは「テストが守っている」ことの証明にはなっていない。

使い方:

    make mutation-probe MUTATIONS=<変異リスト>

変異リストの書式 (空行と # から始まる行は無視):

    === ① フロー Cookie の破棄を defer に戻す
    file: apps/go-api/internal/httpapi/server.go
    test: TestGoogleLoginCallback_IssuesSessionCookie
    pkg: ./internal/httpapi
    expect: fail
    --- from
    	s.clearFlowCookies(c)
    --- to
    	defer s.clearFlowCookies(c)
    --- end

  file    リポジトリ直下からの相対パス
  pkg     apps/go-api からの相対パス (go test に渡す)
  test    go test -run に渡すパターン
  expect  fail = 壊したらテストが落ちるべき (既定)
          pass = 壊しても落ちない (= 検出できない) ことを記録として残す
  from    置換対象。**そのままの文字列**として探す。前後の空白も含めて一致させる
  to      置換後。空にすると削除になる

--- from と --- to の間の行はそのまま使う。インデントも保たれる。
"""

import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
API = os.path.join(ROOT, "apps", "go-api")

RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
DIM = "\033[90m"
OFF = "\033[0m"


class Mutation:
    def __init__(self, label):
        self.label = label
        self.file = None
        self.pkg = None
        self.test = None
        self.expect = "fail"
        self.frm = None
        self.to = ""

    def validate(self):
        for name in ("file", "pkg", "test", "frm"):
            if getattr(self, name) is None:
                raise ValueError(f"{self.label}: {name} が指定されていません")
        if self.expect not in ("fail", "pass"):
            raise ValueError(f"{self.label}: expect は fail か pass です (got {self.expect})")


def parse(path):
    """変異リストを読み取る。

    行指向で読む。YAML を使わないのは、置換文字列に含まれる
    インデント・コロン・引用符をそのまま扱いたいため。
    """
    mutations = []
    current = None
    mode = None  # None / "from" / "to"
    buf = []

    def flush():
        nonlocal buf, mode
        if mode == "from":
            current.frm = "\n".join(buf)
        elif mode == "to":
            current.to = "\n".join(buf)
        buf = []
        mode = None

    with open(path, encoding="utf-8") as f:
        for lineno, raw in enumerate(f, 1):
            line = raw.rstrip("\n")

            if line.startswith("=== "):
                if current:
                    flush()
                    mutations.append(current)
                current = Mutation(line[4:].strip())
                continue

            if current is None:
                if line.strip() and not line.startswith("#"):
                    raise ValueError(f"{path}:{lineno}: === より前に内容があります")
                continue

            if line in ("--- from", "--- to", "--- end"):
                flush()
                mode = {"--- from": "from", "--- to": "to", "--- end": None}[line]
                continue

            if mode:
                buf.append(line)
                continue

            if not line.strip() or line.startswith("#"):
                continue

            m = re.match(r"^(file|pkg|test|expect):\s*(.*)$", line)
            if not m:
                raise ValueError(f"{path}:{lineno}: 解釈できません: {line!r}")
            setattr(current, m.group(1), m.group(2).strip())

    if current:
        flush()
        mutations.append(current)

    for mut in mutations:
        mut.validate()
    return mutations


def run_test(pkg, pattern):
    """go test を 1 回走らせ、(結果, 出力) を返す。

    結果は "pass" / "fail" / "build" / "empty" のいずれか。
    **ビルド失敗を fail と混同しない。** 混同すると、
    「テストが守っている」と「コンパイルが通らなくなった」を取り違える。

    -count=1 を付けるのは、壊す前と壊したあとで同じコマンドになるため。
    キャッシュに当たると、壊したのに前回の結果が返る。
    """
    proc = subprocess.run(
        ["go", "test", "-count=1", pkg, "-run", pattern],
        cwd=API, capture_output=True, text=True,
    )
    out = proc.stdout + proc.stderr

    if "[build failed]" in out or "[setup failed]" in out:
        return "build", out
    if proc.returncode == 0:
        # **「テストが 1 件も走っていない」を「通った」と数えない。**
        # -run のパターンが古くなると go test は何も実行せずに ok を返す。
        # そのまま進むと、壊しても当然赤くならず、
        # 「テストが守っていない」という誤った結論が出る。
        if "no test files" in out or "no tests to run" in out:
            return "empty", out
        return "pass", out
    return "fail", out


def main():
    if len(sys.argv) != 2:
        print("使い方: mutation-probe.py <変異リスト>", file=sys.stderr)
        return 2

    list_path = sys.argv[1]
    if not os.path.exists(list_path):
        print(f"変異リストが見つかりません: {list_path}", file=sys.stderr)
        return 2

    try:
        mutations = parse(list_path)
    except ValueError as e:
        print(f"{RED}変異リストを読めません: {e}{OFF}", file=sys.stderr)
        return 2

    if not mutations:
        print("変異が 1 件も定義されていません", file=sys.stderr)
        return 2

    # 壊す前の状態を確認する。ここが赤いと以降の結果はすべて意味を失う。
    # flush する。stderr のエラーが先に出ると、どの段階で落ちたか読めなくなる。
    print(f"{DIM}壊す前のテストを確認しています...{OFF}", flush=True)
    baseline = {}
    for mut in mutations:
        key = (mut.pkg, mut.test)
        if key in baseline:
            continue
        result, out = run_test(*key)
        baseline[key] = result
        if result == "empty":
            print(f"{RED}{mut.pkg} -run {mut.test} に一致するテストがありません。"
                  f"テスト名が変わったか、まだ書かれていません。{OFF}", file=sys.stderr)
            return 1
        if result != "pass":
            print(f"{RED}壊す前の時点で {mut.pkg} -run {mut.test} が通っていません "
                  f"({result})。先にそちらを直してください。{OFF}", file=sys.stderr)
            print(out[-2000:], file=sys.stderr)
            return 1

    results = []
    failed = 0

    for mut in mutations:
        path = os.path.join(ROOT, mut.file)
        if not os.path.exists(path):
            print(f"{RED}{mut.label}: ファイルがありません: {mut.file}{OFF}", file=sys.stderr)
            return 1

        with open(path, encoding="utf-8") as f:
            original = f.read()

        # 置換対象が実在するか。0 件なら「壊し方が古い」。
        # ここを黙って素通しすると、**何も壊さずに「落ちた」を得る**ことになり、
        # テストが効いている証拠が捏造される。
        occurrences = original.count(mut.frm)
        if occurrences == 0:
            print(f"{RED}{mut.label}: from に一致する箇所がありません "
                  f"({mut.file})。実装が変わって変異が古くなっています。{OFF}", file=sys.stderr)
            return 1

        mutated = original.replace(mut.frm, mut.to)

        try:
            with open(path, "w", encoding="utf-8") as f:
                f.write(mutated)
            result, out = run_test(mut.pkg, mut.test)
        finally:
            # **何があっても戻す。** 例外でも Ctrl-C でも finally は通る。
            # 戻し忘れたまま次の作業に入るのが、この手順で一番害の大きい失敗になる。
            with open(path, "w", encoding="utf-8") as f:
                f.write(original)

        want = "落ちる" if mut.expect == "fail" else "通る"
        actual = {"pass": "通る", "fail": "落ちる",
                  "build": "ビルド失敗", "empty": "テスト無し"}[result]

        if result == "empty":
            ok = False
            note = "テストが 1 件も走っていない (-run のパターンが古い)"
        elif result == "build":
            # ビルドが落ちるのは「テストが守っている」ことの証明にならない。
            ok = False
            note = "ビルドが落ちた (テストの検出力は測れていない)"
        else:
            ok = (result == "fail") == (mut.expect == "fail")
            note = ""

        if not ok:
            failed += 1
        results.append((mut, want, actual, ok, note, occurrences, out))

    print()
    print(f"{'期待':<8} {'結果':<6} {'実測':<12} {'置換':<6} 内容")
    print("-" * 92)
    for mut, want, actual, ok, note, occurrences, _ in results:
        mark = f"{GREEN}OK{OFF}" if ok else f"{RED}NG{OFF}"
        color = "" if ok else YELLOW
        print(f"{want:<8} {mark:<15} {color}{actual:<12}{OFF} {occurrences:<6} {mut.label}")
        if note:
            print(f"{' ' * 8} {YELLOW}└ {note}{OFF}")

    print()
    if failed:
        print(f"{RED}{failed} / {len(results)} 件が期待どおりではありません{OFF}")
        for mut, want, actual, ok, _, _, out in results:
            if ok:
                continue
            print(f"\n--- {mut.label} ({mut.pkg} -run {mut.test}) ---")
            print(out[-1500:])
        return 1

    print(f"{GREEN}変異 {len(results)} 件すべてが期待どおりでした{OFF}")
    expected_pass = [m for m, _, _, _, _, _, _ in results if m.expect == "pass"]
    if expected_pass:
        print(f"{YELLOW}うち {len(expected_pass)} 件は「壊してもテストが落ちない」ことの記録です。"
              f"守れていない箇所なので、PR 本文に残してください。{OFF}")
        for m in expected_pass:
            print(f"  - {m.label}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
