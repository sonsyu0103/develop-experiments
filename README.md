# 掲示板 API & Web アプリ

Go (Gin) + PostgreSQL + Next.js による掲示板アプリケーション。
並行処理・排他制御・集計クエリの設計と、その**実測**を主題としている。

## 構成

```
.
├── api/
│   └── openapi.yaml             【唯一の正】手書きの API 仕様書
├── apps/
│   ├── go-api/                  Go 製 API サーバ
│   │   ├── cmd/api/             HTTP サーバのエントリポイント
│   │   ├── db/
│   │   │   ├── migrations/      golang-migrate のマイグレーション
│   │   │   ├── query/           sqlc が読む SQL (Go コードの発生源)
│   │   │   ├── init/            コンテナ初回起動時の拡張作成
│   │   │   └── seed/            開発用シードデータ
│   │   └── internal/
│   │       ├── apperr/          層をまたぐ番兵エラー
│   │       ├── config/          環境変数の読み取り
│   │       ├── httpapi/         ServerInterface の実装 + oapigen/ (生成物)
│   │       ├── pagination/      キーセットページネーション
│   │       ├── thread/          スレッドドメイン (model / repository / usecase)
│   │       ├── comment/         コメントドメイン
│   │       └── infrastructure/postgres/   pgx によるリポジトリ実装 + sqlc 生成物
│   └── next-app/                Next.js (App Router / RSC)
├── docs/
│   ├── adr/                     設計判断の記録
│   └── postgres-ssi.md          SSI (直列化スナップショット分離) の解説
└── compose.yaml
```

## 起動

```bash
make up      # PostgreSQL 起動 → マイグレーション適用 → API と Web を起動
make seed    # 開発用データを投入
```

- API: http://localhost:8080
- Web: http://localhost:3000

```bash
make help    # 使えるコマンド一覧
make down    # 停止 (データは残る)
make clean   # 停止 + データ削除
```

## 開発

```bash
make tools             # sqlc / oapi-codegen / golangci-lint を導入
make generate          # 仕様書と SQL から生成物をすべて作り直す
make check             # 静的検査 + ユニットテスト + 生成物のドリフト検出 (DB 不要)
make smoke             # 実 DB を立てて API を起動し、HTTP 越しに疎通を検証
make check-all         # check + smoke
```

### コード生成の流れ

**人間が編集するのは 2 箇所だけ**で、型定義はすべてそこから生成される。
手書きの型はどこにも無い。

```
apps/go-api/db/query/*.sql ──sqlc──────────────→ Go の型付きクエリ

api/openapi.yaml ──oapi-codegen ───────────────→ Go のサーバスタブ・型
                 └─openapi-typescript ─────────→ TypeScript の型
```

エンドポイントを追加したいときは `api/openapi.yaml` を編集して
`make generate` を実行する。ルーティングは書かない。

これにより 3 つの保証が得られる。

1. **実装漏れがコンパイルエラーになる** —
   `Server` は生成された `oapigen.ServerInterface` を実装するため、
   仕様書に追加して実装を忘れるとビルドが通らない
2. **レスポンスの形が仕様と一致する** —
   ハンドラは生成された型を返す。仕様からフィールドを消すと
   `mapping.go` がコンパイルエラーになる
3. **仕様書の制約がランタイムで強制される** —
   `minimum` / `maxLength` / `required` / enum が実際に検査される。
   仕様書に無いパスは 404 になる

CI の `generated-ci` ジョブが再生成して差分を検査するため、
生成物の更新漏れはビルドが落ちて気づける。

詳細は [ADR 0002](docs/adr/0002-openapi-direction.md)。

## API

| メソッド | パス | 説明 |
| --- | --- | --- |
| GET | `/healthz` | Liveness (DB は見ない) |
| GET | `/readyz` | Readiness (DB 疎通を含む) |
| GET | `/threads` | スレッド一覧 (コメント数つき) |
| POST | `/threads` | スレッド作成 |
| GET | `/threads/{threadId}` | スレッド 1 件 |
| GET | `/threads/{threadId}/comments` | コメント一覧 |
| POST | `/threads/{threadId}/comments` | コメント投稿 |

一覧系はキーセットページネーション (`?cursor=&size=`、既定 20 件 / 上限 100 件)。
完全な仕様は [`api/openapi.yaml`](api/openapi.yaml) を参照。
このファイルが唯一の正であり、Go と TypeScript の型はここから生成される。

### エラー形式

```json
{ "error": { "code": "NOT_FOUND", "message": "対象のリソースが見つかりません" } }
```

`code` は以下のいずれか。`message` は変わりうるので分岐に使わないこと。

| code | HTTP | 意味 |
| --- | --- | --- |
| `INVALID_ARGUMENT` | 400 | パラメータやボディが仕様を満たさない |
| `NOT_FOUND` | 404 | 対象が存在しない、またはパスが未定義 |
| `METHOD_NOT_ALLOWED` | 405 | パスは存在するがそのメソッドは未定義 |
| `CONFLICT` | 409 | 同時更新の競合。再試行可能 |
| `INTERNAL` | 500 | サーバ内部エラー |

## 設計上の判断

判断の根拠は `docs/adr/` に記録している。要点だけ挙げる。

### コメント数の集計に goroutine を使っていない

スレッドごとに `COUNT` を投げて goroutine で並列化しても、
**DB へのラウンドトリップ回数は N+1 回のまま**であり、
レイテンシは「最も遅いクエリ + 接続プール待ち」に律速される。
DB 側の負荷も N 倍になる。

本番経路は単一クエリで集計している。ただし素直な `LEFT JOIN` + `GROUP BY` は
**20 件返すために 20 スレッド分のコメント実データを読んでしまう**ため、
先に `LIMIT` でスレッドを絞ってから相関サブクエリで数える形にしている。

2,005 スレッド / 200,016 コメントでの実測 (`EXPLAIN ANALYZE`、各 3 回):

| 実装 | 実行時間 | shared buffers |
| --- | --- | --- |
| `LEFT JOIN` + `GROUP BY` | 5.84 – 7.43 ms | 2,054 |
| **採用した形** | **1.04 – 1.11 ms** | **59** |

約 5.5 倍速く、バッファ読み取りは 35 分の 1。結果が一致することも確認済み。
部分インデックスによる `Index Only Scan` (`Heap Fetches: 0`) が効くためで、
この差は visibility map が整備された状態 (VACUUM 後) で現れる。

比較用の N+1 実装は
[`interactor_nplus1.go`](apps/go-api/internal/thread/usecase/interactor_nplus1.go)
に残してあり、Phase 4 のベンチマークで両者を計測する。

### ページネーションが OFFSET でない

`OFFSET N` は読み飛ばす N 行を実際に読むため、深いページほど線形に遅くなる。
また、ページ送り中に行が挿入されると重複や取りこぼしが起きる。

`id` は単調増加なので `WHERE id < cursor` で索引だけを辿れる。

### パーティショニングは「必要だから」入れたのではない

`comments` は `thread_id` による HASH パーティション (8 分割) だが、
**現在のデータ量では性能上まったく不要**である。
インデックスだけで十分な規模であり、むしろプランニングコストの分だけ不利になる。

導入しているのは「**どの規模から効き始めるか**」を Phase 4 で実測するためであり、
必要だと誤認して入れたものではない。
詳細は [ADR 0001](docs/adr/0001-why-postgresql.md)。

CI ではパーティション pruning が実際に効いていること
(8 分割中 1 つだけを走査すること) を `EXPLAIN` で検証している。

### PostgreSQL を選んだ理由

最大の理由は **SERIALIZABLE の実装方式が SSI (楽観的) であること**。
本プロジェクトの主題が並行制御である以上、
悲観ロック版と楽観版を同一 DB 上で比較計測できることに価値がある。

MySQL の `SERIALIZABLE` は全 SELECT を共有ロック化する悲観方式で、
既定の `REPEATABLE READ` はギャップロックにより
「同一スレッドへの同時コメント」でデッドロックを起こしやすい。

→ [SSI の解説](docs/postgres-ssi.md) / [ADR 0001](docs/adr/0001-why-postgresql.md)

## 性能の観測

`pg_stat_statements` と `auto_explain` を有効化してある。

```bash
make psql
```

```sql
-- 累積実行時間の重いクエリ上位 10 件
SELECT calls, round(mean_exec_time::numeric, 2) AS mean_ms, query
FROM pg_stat_statements
ORDER BY total_exec_time DESC
LIMIT 10;

-- パーティション pruning の確認 (1 パーティションのみ走査されるはず)
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM comments WHERE thread_id = 1;
```

100ms を超えたクエリは実行計画つきで `docker compose logs postgres` に出る。

## テストの層

| 層 | 対象 | DB | コマンド |
| --- | --- | --- | --- |
| ユニット | ドメイン / ユースケース / HTTP ハンドラ | フェイク | `make test` |
| スモーク | API 全体を HTTP 越しに | **実 DB** | `make smoke` |

ユニットテストはフェイクのリポジトリで動くため、
**SQL が実際に意図どおり動くかは検証できない**。
論理削除の除外、キーセットページネーションの遷移、
仕様書によるリクエスト検証といった「DB とアプリの前提が噛み合っているか」は
スモークテスト (40 項目) で確認している。CI の Migration Check でも実行される。

## ロードマップ

| | 内容 | 状態 |
| --- | --- | --- |
| Phase 0 | PostgreSQL / マイグレーション / sqlc の基盤構築 | 完了 |
| Phase 1 | OpenAPI → Go スタブ / TypeScript 型の自動生成 | 完了 (`make generate`) |
| Phase 2 | コメント投稿の並行制御強化 (SSI + リトライ、悲観ロック版との比較) | 未着手 |
| Phase 3 | Next.js のスレッド一覧・詳細画面 | 一覧のみ実装 |
| Phase 4 | ベンチマーク (N+1 vs 単一クエリ、パーティションの損益分岐点) | 未着手 |

未決事項は [ADR 0003](docs/adr/0003-open-questions.md) に一覧化している。

## 技術スタック

Go 1.25 / Gin / pgx v5 / sqlc / oapi-codegen / PostgreSQL 17 /
Next.js 16 (App Router, RSC) / React 19 / TypeScript 5.9 / ESLint 9
