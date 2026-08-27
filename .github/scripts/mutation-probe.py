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
  - 置換対象が実在し、**意図した件数だけに当たる**ことの確認 (max)
  - **どう終わっても元に戻すこと** (例外・Ctrl-C・kill・失敗のいずれでも)
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
  runner  go (既定) / playwright
  pkg     apps/go-api からの相対パス (go test に渡す)。**go のときだけ**
  test    go test -run / playwright -g に渡すパターン
  expect  fail = 壊したらテストが落ちるべき (既定)
          pass = 壊しても落ちない (= 検出できない) ことを記録として残す
  max     from が一致してよい最大件数 (既定 1)。
          意図せず複数箇所に当たると、測っているものが変わる
  from    置換対象。**そのままの文字列**として探す。前後の空白も含めて一致させる
  to      置換後。空にすると削除になる

--- from と --- to の間の行はそのまま使う。インデントも保たれる。

【runner: playwright を足した理由】

管理画面のレビューで出た指摘 6 件のうち 4 件が、
**「応答が遅れた / 失敗したときのクライアントの状態」**だった
(古い応答が新しい一覧に混ざる、削除の二重送信が通る、など)。
go test では届かず、**変異で測れないテストが増える**形になっていた。

「テストが通った」を信用しない、というこのスクリプトの前提は
フロントでも変わらない。1 変異あたり数十秒かかる (毎回ビルドし直すため)
ので、**go の変異と同じ感覚では並べられない**点にだけ注意する。
"""

import os
import re
import signal
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
API = os.path.join(ROOT, "apps", "go-api")
NEXT = os.path.join(ROOT, "apps", "next-app")

RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
DIM = "\033[90m"
OFF = "\033[0m"


class Mutation:
    def __init__(self, label):
        self.label = label
        self.file = None
        self.runner = "go"
        self.pkg = None
        self.test = None
        self.expect = "fail"
        self.max = "1"
        self.frm = None
        self.to = ""

    def validate(self):
        if self.runner not in ("go", "playwright"):
            raise ValueError(
                f"{self.label}: runner は go か playwright です (got {self.runner})")

        # pkg は go test にだけ渡す。playwright は testDir 全体から
        # -g で絞るので、パッケージという単位が無い。
        required = ("file", "pkg", "test", "frm") if self.runner == "go" \
            else ("file", "test", "frm")
        for name in required:
            if getattr(self, name) is None:
                raise ValueError(f"{self.label}: {name} が指定されていません")
        if self.expect not in ("fail", "pass"):
            raise ValueError(f"{self.label}: expect は fail か pass です (got {self.expect})")
        if not str(self.max).isdigit() or int(self.max) < 1:
            raise ValueError(f"{self.label}: max は 1 以上の整数です (got {self.max})")
        self.max = int(self.max)


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

            m = re.match(r"^(file|runner|pkg|test|expect|max):\s*(.*)$", line)
            if not m:
                raise ValueError(f"{path}:{lineno}: 解釈できません: {line!r}")
            setattr(current, m.group(1), m.group(2).strip())

    if current:
        flush()
        mutations.append(current)

    for mut in mutations:
        mut.validate()
    return mutations


def run_test(mut):
    """テストを 1 回走らせ、(結果, 出力) を返す。

    結果は "pass" / "fail" / "build" / "empty" のいずれか。
    **ビルド失敗を fail と混同しない。** 混同すると、
    「テストが守っている」と「コンパイルが通らなくなった」を取り違える。
    """
    if mut.runner == "playwright":
        return _run_playwright(mut.test)
    return _run_go(mut.pkg, mut.test)


def _run_go(pkg, pattern):
    """-count=1 を付けるのは、壊す前と壊したあとで同じコマンドになるため。
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


def _run_playwright(pattern):
    """Playwright を 1 回走らせる。

    **毎回 webServer が本番ビルドをやり直します** (playwright.config.ts)。
    壊したソースを確実に反映させるためで、そのぶん 1 変異あたり
    数十秒かかります。`next start` を使い回すと、
    **壊す前のバンドルを検査してしまう** —— go の -count=1 と同じ理由になります。

    `-g` に一致しないと Playwright は "No tests found" を出して
    **終了コード 1 で終わります。** 落ちたのと見分けが付かないので、
    ここで "empty" に分けます。分けないと、検査名を書き換えたときに
    「壊したら落ちた」と誤って数えます。
    """
    proc = subprocess.run(
        ["npx", "playwright", "test", "-g", pattern],
        cwd=NEXT, capture_output=True, text=True,
        # Node のラッパが差し込む NODE_OPTIONS を外す (Makefile の NODE と同じ)。
        env={k: v for k, v in os.environ.items() if k != "NODE_OPTIONS"},
    )
    out = proc.stdout + proc.stderr

    if "No tests found" in out:
        return "empty", out
    # webServer の起動に失敗した場合は、実装が壊れて型検査に落ちたか、
    # ビルドが通らなくなったかのどちらか。fail と混同しない。
    if "Error: Process from config.webServer" in out or "error TS" in out:
        return "build", out
    return ("pass", out) if proc.returncode == 0 else ("fail", out)


class Restorer:
    """壊したファイルを必ず戻すための後始末。

    finally だけでは足りない。**SIGTERM では finally が走らない** ——
    Python は SIGTERM の既定ハンドラでそのままプロセスを終えるため、
    例外が上がらず finally に到達しない。実測すると、
    kill したあとファイルは壊れたまま残る。

    このスクリプトの存在意義の大半は「確実に戻すこと」なので、
    シグナルを捕まえて戻してから終了する。
    兄弟の arch-probe.sh も trap ... EXIT INT TERM で同じことをしている。
    """

    def __init__(self):
        self.pending = {}  # path -> 元の内容

    def hold(self, path, content):
        self.pending[path] = content

    def restore(self):
        for path, content in self.pending.items():
            with open(path, "w", encoding="utf-8") as f:
                f.write(content)
        self.pending = {}

    def install(self):
        def handler(signum, _frame):
            self.restore()
            print(f"\n{YELLOW}シグナル {signum} を受けたので、"
                  f"壊したファイルを元に戻して終了します。{OFF}", file=sys.stderr)
            # 既定の動作で終わり直す。終了コードをシグナル由来のまま残すため。
            signal.signal(signum, signal.SIG_DFL)
            os.kill(os.getpid(), signum)

        for sig in (signal.SIGTERM, signal.SIGHUP):
            signal.signal(sig, handler)


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
        key = (mut.runner, mut.pkg, mut.test)
        if key in baseline:
            continue
        result, out = run_test(mut)
        baseline[key] = result
        where = f"{mut.pkg} -run {mut.test}" if mut.runner == "go" \
            else f"playwright -g {mut.test}"
        if result == "empty":
            print(f"{RED}{where} に一致するテストがありません。"
                  f"テスト名が変わったか、まだ書かれていません。{OFF}", file=sys.stderr)
            return 1
        if result != "pass":
            print(f"{RED}壊す前の時点で {where} が通っていません "
                  f"({result})。先にそちらを直してください。{OFF}", file=sys.stderr)
            print(out[-2000:], file=sys.stderr)
            return 1

    results = []
    failed = 0
    restorer = Restorer()
    restorer.install()

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
        if occurrences > mut.max:
            # 意図せず複数箇所に当たると、測っているものが変わる。
            # 「1 箇所を壊したらテストが落ちた」のつもりで
            # 実は 5 箇所壊していた、では検出力の証明にならない。
            print(f"{RED}{mut.label}: from が {occurrences} 箇所に一致します "
                  f"(max={mut.max})。範囲を狭めるか、意図どおりなら "
                  f"max: {occurrences} と宣言してください。{OFF}", file=sys.stderr)
            return 1

        mutated = original.replace(mut.frm, mut.to)

        # 壊す前に「戻すべき内容」を預ける。
        # 例外・Ctrl-C は finally が、SIGTERM / SIGHUP は Restorer が戻す。
        # 戻し忘れたまま次の作業に入るのが、この手順で一番害の大きい失敗になる。
        restorer.hold(path, original)
        try:
            with open(path, "w", encoding="utf-8") as f:
                f.write(mutated)
            result, out = run_test(mut)
        finally:
            restorer.restore()

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
            where = f"{mut.pkg} -run {mut.test}" if mut.runner == "go" \
                else f"playwright -g {mut.test}"
            print(f"\n--- {mut.label} ({where}) ---")
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
