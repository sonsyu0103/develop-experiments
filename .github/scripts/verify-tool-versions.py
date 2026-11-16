#!/usr/bin/env python3
"""Makefile と CI (ci.yml) に書いたツールの版が同じかを検査する。

【なぜ要るか】

ツールの版は Makefile の冒頭と ci.yml の env の 2 か所に書いてある。
手元 (make tools / make vulncheck) と CI で別の版が入ると、手元で通った検査が
CI で落ちる (またはその逆)。2026-11 まで突き合わせる検査は無く、
govulncheck は CI だけ @latest だった (ADR 0027「ほかに見つかったこと」)。

【何を見るか】

  1. ci.yml の env の値が Makefile の値と同じか
     (値の引用符と行末のコメントは許す。GO_VERSION は '1.26.11' と書いている)
  2. ci.yml で各ツールを入れる行が、**1 行残らず** env の版を使っているか
  3. Makefile で各ツールを入れる行が、1 行残らず Makefile の変数を使っているか

初版は 2 を「env を使う文字列が ci.yml のどこかにあるか」で見ていた。
**別のジョブに @latest の行を足しても通る**ので、行ごとに見るようにした
(PR #74 のレビュー指摘)。env の値が正しくても、install の行が
直書きなら意味が無い。
"""

from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
MAKEFILE = ROOT / "Makefile"
CI = ROOT / ".github" / "workflows" / "ci.yml"

# 版の変数名 → go install / go run で指すパス
TOOLS = {
    "GOLANGCI_LINT_VERSION": "github.com/golangci/golangci-lint/v2/cmd/golangci-lint",
    "SQLC_VERSION": "github.com/sqlc-dev/sqlc/cmd/sqlc",
    "OAPI_CODEGEN_VERSION": "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen",
    "GO_ARCH_LINT_VERSION": "github.com/fe3dback/go-arch-lint",
    "GOVULNCHECK_VERSION": "golang.org/x/vuln/cmd/govulncheck",
}


def code_lines(path: pathlib.Path):
    """コメント行を除いた (行番号, 行) を返す。コメントの中の例文で通らないように。"""
    for n, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if line.lstrip().startswith("#"):
            continue
        yield n, line


def main() -> int:
    make_text = MAKEFILE.read_text(encoding="utf-8")
    ci_text = CI.read_text(encoding="utf-8")
    ng: list[str] = []

    for key, module in TOOLS.items():
        m = re.search(rf"^{key}\s*:=\s*(\S+)\s*$", make_text, re.M)
        if not m:
            ng.append(f"Makefile に {key} が無い")
            continue
        want = m.group(1)

        # 1. env の値
        m = re.search(rf"^  {key}:\s*['\"]?([^'\"\s#]+)['\"]?\s*(#.*)?$", ci_text, re.M)
        got = m.group(1) if m else None
        if got != want:
            ng.append(f"ci.yml の {key} が {got or '(無い)'} (Makefile は {want})")

        # 2・3. 入れる行が、1 行残らず変数の版を使っているか
        for path, label, use in (
            (CI, "ci.yml", f"{module}@${{{{ env.{key} }}}}"),
            (MAKEFILE, "Makefile", f"{module}@$({key})"),
        ):
            hits = [(n, line) for n, line in code_lines(path) if f"{module}@" in line]
            if not hits:
                ng.append(f"{label} に {module} を入れる行が無い ({key} が使われていない)")
            for n, line in hits:
                if use not in line:
                    ng.append(f"{label} の {n} 行目が {key} を使っていない: {line.strip()}")

    for msg in ng:
        print(f"NG: {msg}")
    if ng:
        return 1
    print(f"OK: Makefile と ci.yml のツールの版は同じ ({len(TOOLS)} 個。入れる行もすべて変数の版を使っている)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
