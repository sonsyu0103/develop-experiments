#!/usr/bin/env python3
"""読み取りのスケール限界を実測する (Phase 4 / ADR 0009)。

**README の Phase 4 で「未測定」として残っていた 1 件。**
理由は「単一マシンの compose では**負荷生成側が先に飽和する**」だった。

先に飽和するなら、**それを数字で示せばよい。** そのために 3 段構えで測る:

    1. 校正   /healthz (DB を触らない) を叩いて、負荷生成側 + HTTP 層の天井を出す
    2. HTTP   /threads (本番の読み取り経路) を叩いて、実際に出る RPS を出す
    3. DB     pgbench で同じ SQL を直接流し、PostgreSQL 単体の天井を出す

**1 と 2 の比が答えになる。** 2 が 1 に近ければ「生成側が飽和した」であり、
2 の数字は下限としてしか読めない。離れていれば 2 は API の実力になる。
3 は「HTTP を全部剥がすとどこまで出るか」で、
ADR 0009 の「読み取り 4,200 RPS」に対する上限側の見積もりになる。

【負荷生成をコンテナの中で走らせる理由】
ホストから叩くと Docker Desktop のポート転送が挟まり、macOS ではそこが
最初の律速になる。go-api コンテナには Go 一式が入っている (compose の dev)
ので、その中でビルドして localhost を叩く。

【どちらが飽和したかを CPU でも見る】
条件ごとに docker stats を 1 回取る。RPS が頭打ちになったとき、
go-api が張り付いているのか postgres が張り付いているのかで打ち手が変わる
(前者ならアプリを増やす、後者ならレプリカが要る)。

使い方:
    make bench-dataset    2,000 スレッド / 20 万コメントを投入する
    make scale-probe      測る

**CI には載せない。** 数字が環境の性能に左右されるため。
"""

from __future__ import annotations

import json
import os
import re
import shlex
import subprocess
import sys
import time

COMPOSE = os.environ.get("COMPOSE", "docker compose")
DOCKER = os.environ.get("DOCKER", "docker")
API_SERVICE = os.environ.get("API_SERVICE", "go-api")
DB_SERVICE = os.environ.get("DB_SERVICE", "postgres")
BASE_URL = os.environ.get("PROBE_BASE_URL", "http://localhost:8080")

def int_list(key: str, fallback: str) -> list[int]:
    """カンマ区切りの環境変数を整数の並びとして読む。

    **空文字を未設定として扱う。** `make scale-probe PROBE_LEVELS=` や、
    空で export された変数で、素の ValueError を出さないため。
    """
    raw = os.environ.get(key, "").strip() or fallback
    try:
        return [int(x) for x in raw.split(",") if x.strip()]
    except ValueError as err:
        raise SystemExit(f"{key} の書式が不正です (カンマ区切りの整数): {raw!r}") from err


DURATION = os.environ.get("PROBE_DURATION", "").strip() or "5s"
LEVELS = int_list("PROBE_LEVELS", "1,2,4,8,16,32,64,128")
# pgbench は接続を並列数ぶん張るので、max_connections を超えられない。
DB_LEVELS = int_list("PROBE_DB_LEVELS", "1,2,4,8,16,32,64")

LOADGEN_BIN = "/tmp/loadgen"
# コンテナを作り直すと /tmp が消えるので、ホスト側に実体を置いておく。
HOST_BIN = os.environ.get("PROBE_LOADGEN_BIN", "/tmp/bbs-loadgen")

# 接続プールの上限を振る段 (5 番)。コンテナを作り直すので時間がかかる。
POOL_LEVELS = int_list("PROBE_POOL_LEVELS", "4,8,12,16,20,32,64")
POOL_TEST_LEVELS = int_list("PROBE_POOL_TEST_LEVELS", "16,64")

TPS = re.compile(r"^tps = ([\d.]+)", re.MULTILINE)
LATENCY = re.compile(r"^latency average = ([\d.]+) ms", re.MULTILINE)


def sh(cmd: str, **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(shlex.split(cmd), capture_output=True, text=True, **kwargs)


def compose_exec(service: str, command: str) -> str:
    out = sh(f"{COMPOSE} exec -T {service} {command}")
    if out.returncode != 0:
        raise RuntimeError(f"失敗しました: {command}\n{out.stderr.strip()[:500]}")
    return out.stdout


def progress(msg: str) -> None:
    print(f"  [{time.strftime('%H:%M:%S')}] {msg}", flush=True)


# ---------------------------------------------------------------------------
# CPU の観測
# ---------------------------------------------------------------------------

def cpu_snapshot() -> dict[str, float]:
    """コンテナごとの CPU 使用率を 1 回取る。

    **100% = 1 コア。** 4 vCPU の環境なら合計 400% が上限になる。
    """
    # **docker compose ではなく docker stats。** compose 側に stats は無く、
    # 失敗しても標準エラーに出るだけなので、CPU 欄が黙って空になる。
    #
    # **shlex.split に渡さない。** 書式に含まれるタブが区切りとして食われ、
    # `--format {{.Name}}` だけが渡る。エラーにはならず、
    # 出力にタブが無くなって CPU 欄が黙って空になる (実際に一度そうなった)。
    out = subprocess.run(
        shlex.split(f"{DOCKER} stats --no-stream --format") + ["{{.Name}}\t{{.CPUPerc}}"],
        capture_output=True, text=True)
    result = {}
    for line in out.stdout.splitlines():
        parts = line.split("\t")
        if len(parts) != 2:
            continue
        name, perc = parts[0].strip(), parts[1].strip().rstrip("%")
        try:
            result[name] = float(perc)
        except ValueError:
            continue
    return result


def short(name: str) -> str:
    """develop-experiments-go-api-1 → go-api"""
    return name.removeprefix("develop-experiments-").rsplit("-", 1)[0]


# ---------------------------------------------------------------------------
# 1 / 2. HTTP
# ---------------------------------------------------------------------------

def build_loadgen() -> None:
    progress("負荷生成をコンテナ内でビルド中...")
    compose_exec(API_SERVICE, f"go build -o {LOADGEN_BIN} ./tools/loadgen")
    # **ホストに退避しておく。** プール上限を振るときにコンテナを作り直すと
    # /tmp ごと消え、ビルドキャッシュも消えているので毎回 1 分かかる。
    docker_cp(f"{container_id()}:{LOADGEN_BIN}", HOST_BIN)


def container_id() -> str:
    """go-api コンテナの ID。空なら落とす。

    **空のまま docker cp に渡すと `:/tmp/loadgen` になり、黙って失敗する。**
    表面化するのは数十秒あとの「loadgen が失敗しました」で、原因が読めない。
    """
    cid = sh(f"{COMPOSE} ps -q {API_SERVICE}").stdout.strip()
    if not cid:
        raise RuntimeError(
            f"{API_SERVICE} コンテナが見つかりません。make up を先に流してください")
    return cid


def docker_cp(src: str, dst: str) -> None:
    out = sh(f"{DOCKER} cp {src} {dst}")
    if out.returncode != 0:
        raise RuntimeError(f"docker cp に失敗しました ({src} → {dst}): "
                           f"{out.stderr.strip()[:300]}")


def restore_loadgen() -> None:
    """作り直したコンテナに負荷生成を戻す。"""
    docker_cp(HOST_BIN, f"{container_id()}:{LOADGEN_BIN}")


def recreate_api(pool: int | None) -> None:
    """DB_MAX_CONNS を差し替えて go-api を作り直し、起動を待つ。"""
    env = dict(os.environ)
    if pool is None:
        env.pop("DB_MAX_CONNS", None)
    else:
        env["DB_MAX_CONNS"] = str(pool)

    subprocess.run(shlex.split(f"{COMPOSE} up -d --force-recreate {API_SERVICE}"),
                   env=env, capture_output=True, text=True, check=True)
    for _ in range(60):
        if sh("curl -sf -o /dev/null http://localhost:8080/healthz").returncode == 0:
            break
        time.sleep(1)
    else:
        raise RuntimeError("API が起動しませんでした")
    restore_loadgen()


def open_connections() -> str:
    """API が実際に張っている接続数。

    **設定が届いているかを毎回確かめる。** 環境変数の渡し忘れは
    エラーにならず、「上限を振ったのに全部同じ数字」という形で出る。
    """
    # **自分自身を除く。** この問い合わせを投げている psql も
    # datname='bbs' の client backend なので、必ず +1 されて数えられる。
    # 「上限 4 なのに 5 接続」がきれいに全行で出ていて気づいた。
    out = sh(f"{COMPOSE} exec -T {DB_SERVICE} psql -U app -d bbs -X -t -A -c "
             f"\"SELECT count(*) FROM pg_stat_activity "
             f"WHERE datname='bbs' AND backend_type='client backend' "
             f"AND pid <> pg_backend_pid()\"")
    return out.stdout.strip()


def pool_ladder() -> list[dict]:
    """接続プールの上限を振って、折り返し点を出す。"""
    print("\n■ 5. API の接続プール上限 vs スループット (/threads?size=20)")
    print("  **DB_MAX_CONNS の既定を決めるための測定。**")
    print("  上の 3 で pgbench が示した最適点が、API 経由でも同じ位置に出るかを見る")
    # **err / 非2xx を必ず出す。** loadgen は 5xx も requests に数えるので
    # (Errors から除かれるのは接続失敗だけ)、**速く失敗する条件ほど
    # RPS が高く出る**。列が無いと、500 を返し始めた上限が
    # 「スループットが落ちていない」として最適点に選ばれる ——
    # そしてそれがそのまま本番の既定になる。
    print(f"{'上限':>5}{'並列':>5}{'RPS':>9}{'p50ms':>8}{'p95ms':>8}"
          f"{'err':>5}{'非2xx':>6}{'接続':>6}  postgres CPU")
    print("-" * 73)

    rows = []
    try:
        for pool in POOL_LEVELS:
            recreate_api(pool)
            for c in POOL_TEST_LEVELS:
                r = http_run("/threads?size=20", c)
                conns = open_connections()
                pg = next((v for n, v in r["cpu"].items() if "postgres" in n), 0.0)
                print(f"{pool:>5}{c:>5}{r['rps']:>9.0f}{r['p50_ms']:>8.2f}"
                      f"{r['p95_ms']:>8.2f}{r['errors']:>5}{r['non2xx']:>6}"
                      f"{conns:>6}  {pg:.0f}%")
                rows.append({"pool": pool, "concurrency": c, **r})
    finally:
        # **既定に戻してから抜ける。** 途中で落ちても、
        # 上限を振ったままの go-api を置き去りにしない。
        progress("go-api を既定の設定に戻しています...")
        recreate_api(None)
    return rows


def http_run(path: str, concurrency: int) -> dict:
    """loadgen を 1 条件ぶん走らせ、途中で CPU を 1 回取る。"""
    url = f"http://localhost:8080{path}"
    cmd = shlex.split(
        f"{COMPOSE} exec -T {API_SERVICE} {LOADGEN_BIN} "
        f"-url {url} -c {concurrency} -d {DURATION}")

    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    # 準備運転 (1 秒) + 計測の中ほどで CPU を取る。
    time.sleep(1 + parse_seconds(DURATION) / 2)
    cpu = cpu_snapshot()
    stdout, stderr = proc.communicate()

    if proc.returncode != 0:
        raise RuntimeError(f"loadgen が失敗しました:\n{stderr.strip()[:500]}")

    result = json.loads(stdout)
    result["cpu"] = cpu
    return result


def parse_seconds(spec: str) -> float:
    if spec.endswith("ms"):
        return float(spec[:-2]) / 1000
    if spec.endswith("s"):
        return float(spec[:-1])
    return float(spec)


def http_ladder(title: str, note: str, path: str) -> list[dict]:
    print(f"\n{title}")
    print(f"  {note}")
    print(f"  対象: {path}")
    # **go-api の CPU には負荷生成のぶんが混ざる。** 同じコンテナで走らせて
    # いるので分けられない。切り分けに使えるのは postgres 側の数字と、
    # 上の校正 (/healthz) の天井になる。
    print(f"{'並列':>5}{'RPS':>10}{'p50ms':>9}{'p95ms':>9}{'p99ms':>9}"
          f"{'err':>6}{'非2xx':>7}   CPU(100%=1コア / go-api は負荷生成を含む)")
    print("-" * 82)

    rows = []
    for c in LEVELS:
        r = http_run(path, c)
        rows.append(r)
        cpu = r["cpu"]
        # 上位 2 つだけ出す。**張り付いている側を見るのが目的**で、
        # 全コンテナを並べると読めなくなる。
        top = sorted(cpu.items(), key=lambda kv: -kv[1])[:2]
        cpu_text = "  ".join(f"{short(n)} {v:.0f}%" for n, v in top)
        print(f"{c:>5}{r['rps']:>10.0f}{r['p50_ms']:>9.2f}{r['p95_ms']:>9.2f}"
              f"{r['p99_ms']:>9.2f}{r['errors']:>6}{r['non2xx']:>7}   {cpu_text}")
    return rows


# ---------------------------------------------------------------------------
# 3. pgbench
# ---------------------------------------------------------------------------

# 本番の一覧クエリ (db/query/threads.sql の ListThreadsWithCommentCount) と
# **同じ形にする。** 簡略化すると「PostgreSQL の限界」ではなく
# 「簡略化したクエリの限界」を測ることになる。
LIST_SQL = r"""\set cursor random(100, 2000)
WITH page AS (
    SELECT id, title, created_at, view_count, author_id, icon_image_id
    FROM threads
    WHERE deleted_at IS NULL AND id < :cursor
    ORDER BY id DESC
    LIMIT 20
)
SELECT
    p.id, p.title, p.created_at, p.view_count,
    (SELECT count(*) FROM comments c
      WHERE c.thread_id = p.id AND c.deleted_at IS NULL)::bigint AS comment_count,
    u.public_id, u.display_name, u.avatar_url, u.deleted_at,
    img.id, img.object_key, img.width, img.height
FROM page p
LEFT JOIN users u ON u.id = p.author_id
LEFT JOIN images img ON img.id = p.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL
ORDER BY p.id DESC;
"""

# スレッド詳細 + コメント 1 ページ。**1 PV あたり 2 クエリ**の側になる。
DETAIL_SQL = r"""\set tid random(1, 2000)
SELECT t.id, t.title, t.created_at, t.view_count,
    (SELECT count(*) FROM comments c
      WHERE c.thread_id = t.id AND c.deleted_at IS NULL)::bigint
FROM threads t WHERE t.id = :tid AND t.deleted_at IS NULL;
SELECT id, seq, author_name, body, created_at
FROM comments
WHERE thread_id = :tid AND deleted_at IS NULL
ORDER BY id DESC LIMIT 50;
"""


def write_pgbench_script(name: str, body: str) -> str:
    """pgbench のスクリプトをコンテナ内に置く。"""
    path = f"/tmp/{name}.sql"
    proc = subprocess.Popen(
        shlex.split(f"{COMPOSE} exec -T {DB_SERVICE} sh -c 'cat > {path}'"),
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    _, stderr = proc.communicate(body)
    if proc.returncode != 0:
        raise RuntimeError(f"スクリプトの配置に失敗しました: {stderr.strip()[:300]}")
    return path


def pgbench_ladder(title: str, note: str, script: str) -> list[dict]:
    print(f"\n{title}")
    print(f"  {note}")
    print(f"{'接続':>5}{'TPS':>10}{'平均ms':>9}   CPU(100%=1コア)")
    print("-" * 58)

    rows = []
    seconds = int(parse_seconds(DURATION))
    for c in DB_LEVELS:
        threads = min(c, 4)
        cmd = shlex.split(
            f"{COMPOSE} exec -T {DB_SERVICE} pgbench -n -f {script} "
            f"-c {c} -j {threads} -T {seconds} -U app bbs")
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        time.sleep(min(seconds / 2, 3))
        cpu = cpu_snapshot()
        stdout, stderr = proc.communicate()

        if proc.returncode != 0:
            # **接続数の上限に当たるのも結果のうち。** 落とさずに記録する。
            print(f"{c:>5}   失敗: {stderr.strip().splitlines()[-1][:60] if stderr.strip() else '不明'}")
            continue

        tps_m = TPS.search(stdout)
        lat_m = LATENCY.search(stdout)
        # **読み取れなかったら落とす。** 0.0 で先へ進めると
        # 「TPS 0」が表に並び、pgbench が壊れたのか DB が遅いのか読めない。
        if tps_m is None:
            raise RuntimeError(
                f"pgbench の tps を読み取れませんでした:\n{stdout[:400]}")
        tps = float(tps_m.group(1))
        lat = float(lat_m.group(1)) if lat_m else 0.0
        top = sorted(cpu.items(), key=lambda kv: -kv[1])[:2]
        cpu_text = "  ".join(f"{short(n)} {v:.0f}%" for n, v in top)
        print(f"{c:>5}{tps:>10.0f}{lat:>9.2f}   {cpu_text}")
        rows.append({"concurrency": c, "tps": tps, "latency_ms": lat, "cpu": cpu})
    return rows


# ---------------------------------------------------------------------------

def environment() -> None:
    print("環境")
    cpus = compose_exec(DB_SERVICE, "nproc").strip()
    settings = compose_exec(
        DB_SERVICE,
        "psql -U app -d bbs -X -t -A -c "
        "\"SELECT current_setting('max_connections') || ' / ' || "
        "current_setting('shared_buffers');\"").strip()
    threads = compose_exec(
        DB_SERVICE,
        "psql -U app -d bbs -X -t -A -c \"SELECT count(*) FROM threads;\"").strip()
    comments = compose_exec(
        DB_SERVICE,
        "psql -U app -d bbs -X -t -A -c \"SELECT count(*) FROM comments;\"").strip()

    print(f"  コンテナの CPU: {cpus} (全サービスで共有)")
    print(f"  max_connections / shared_buffers: {settings}")
    print(f"  データ: {threads} スレッド / {comments} コメント")
    print(f"  1 条件あたり {DURATION} (準備運転 1s は捨てる)")

    if int(threads) < 1000:
        print("\n**データが少なすぎます。** make bench-dataset を先に流してください。",
              file=sys.stderr)
        raise SystemExit(1)


def summarize(calib: list[dict], api: list[dict], db: list[dict],
              pool: list[dict] | None = None) -> None:
    peak_calib = max((r["rps"] for r in calib), default=0)
    peak_api = max((r["rps"] for r in api), default=0)
    peak_db = max((r["tps"] for r in db), default=0)

    print(f"\n{'=' * 72}")
    print("まとめ")
    print(f"{'=' * 72}")
    print(f"  負荷生成 + HTTP の天井 (/healthz)  : {peak_calib:>8.0f} RPS")
    print(f"  本番の読み取り経路 (/threads)      : {peak_api:>8.0f} RPS")
    print(f"  PostgreSQL 単体 (pgbench / 同じ SQL): {peak_db:>8.0f} TPS")

    if peak_calib <= 0:
        return
    ratio = peak_api / peak_calib
    print()
    if ratio > 0.8:
        print(f"  **/threads の数字は下限としてしか読めません** "
              f"(校正の {ratio * 100:.0f}%)。")
        print("  負荷生成側が先に飽和しています。API の実力はこれ以上です。")
    else:
        print(f"  /threads は校正の {ratio * 100:.0f}% で頭打ちになりました。")
        print("  負荷生成側には余裕があるので、この数字は API 側の限界として読めます。")

    print()
    print("  ADR 0009 の見積もり: 読み取りピーク 約 4,200 RPS (攻めた前提) /")
    print("                       約 550 RPS (控えめな前提)")

    if not pool:
        return

    # **失敗を含む条件は最適点の候補から外す。** 5xx は requests に数えられる
    # ので、速く失敗した条件ほど RPS が高く出る。除かずに最大値を取ると、
    # 「500 を返し始めた上限」が既定に選ばれうる。
    clean = [r for r in pool if r["errors"] == 0 and r["non2xx"] == 0]
    dropped = sorted({r["pool"] for r in pool} - {r["pool"] for r in clean})
    if dropped:
        print()
        print(f"  **失敗が出た上限を候補から外した: {dropped}** "
              f"(err または非 2xx が 1 件以上)")
    if not clean:
        print("\n  すべての条件で失敗が出たため、最適点を判定できません。")
        return

    # 上限ごとに「最良の RPS」と「最悪の p95」を並べる。
    #
    # **RPS だけで最適点を決めない。** 走らせるたびに最大値の位置が動く
    # ほど差が小さく (実測で 2 回まわして 8 と 64 に割れた)、
    # 一方で **p95 は上限を上げるほど一貫して悪化する**。
    # スクリプトが片方だけ見て「最適点はここ」と言い切ると、
    # データが支えていない結論を出すことになる。
    best: dict[int, float] = {}
    worst_p95: dict[int, float] = {}
    for r in clean:
        best[r["pool"]] = max(best.get(r["pool"], 0.0), r["rps"])
        worst_p95[r["pool"]] = max(worst_p95.get(r["pool"], 0.0), r["p95_ms"])
    top = max(best.values())

    print()
    print("  接続プール上限ごとの折り合い (RPS は最良、p95 は最悪の条件):")
    print(f"    {'上限':>5}{'最良RPS':>9}{'ピーク比':>9}{'最悪p95':>9}")
    for p in sorted(best):
        print(f"    {p:>5}{best[p]:>9.0f}{best[p] / top * 100:>8.0f}%"
              f"{worst_p95[p]:>9.1f}")
    print()
    print("  **スループットだけでは決まらない。** 上限を上げても RPS は"
          f" {top / min(best.values()):.2f} 倍にしかならず、")
    print("  その差は実行ごとのばらつきと同程度になる。一方で p95 は"
          f" {max(worst_p95.values()) / min(worst_p95.values()):.1f} 倍に開く。")
    print("  既定はこのトレードオフで決める (docs/adr/0009-scaling-strategy.md)。")


def main() -> int:
    environment()
    build_loadgen()

    calib = http_ladder(
        "■ 1. 校正: 負荷生成側の天井 (/healthz)",
        "**DB を触らない経路。** ここが低ければ、下の数字は全部その天井に頭を打つ",
        "/healthz")

    api = http_ladder(
        "■ 2. 本番の読み取り経路 (/threads?size=20)",
        "一覧 1 ページ。相関サブクエリ + LEFT JOIN users + LEFT JOIN images",
        "/threads?size=20")

    list_script = write_pgbench_script("list", LIST_SQL)
    detail_script = write_pgbench_script("detail", DETAIL_SQL)

    db = pgbench_ladder(
        "■ 3. PostgreSQL 単体: 一覧クエリ (pgbench)",
        "HTTP と Go を全部剥がした上限側。**接続数 vs スループット**も兼ねる",
        list_script)

    pgbench_ladder(
        "■ 4. PostgreSQL 単体: 詳細 + コメント 1 ページ (pgbench)",
        "1 PV あたり 2 クエリの側",
        detail_script)

    pool = pool_ladder()

    summarize(calib, api, db, pool)
    return 0


if __name__ == "__main__":
    sys.exit(main())
