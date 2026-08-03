# ADR 0004: モジュラモノリスとレイヤ境界を depguard で強制する

- ステータス: **採用**
- 日付: 2026-08-03

## 背景

`apps/go-api/internal` は当初から、機能単位の縦割り (モジュラモノリス) と
レイヤ単位の依存方向 (クリーンアーキテクチャ) を組み合わせた構成になっていた。

```
internal/
├── thread/                     ← 機能モジュール
│   ├── domain/model/           ← エンティティ・不変条件
│   ├── domain/repository/      ← ポート (interface)
│   └── usecase/                ← インタラクター
├── comment/                    ← 同じ構成
├── infrastructure/postgres/    ← アダプタ (ポートの実装)
├── httpapi/                    ← HTTP 境界
├── apperr/ pagination/ config/ ← モジュール共有の下地
└── ...
```

ただしこの構成は、**記録も強制もされていなかった。**

- なぜ縦割りなのか、モジュール間をどう繋ぐのかがどこにも書かれていない
- 境界を守っているのはコードの規律だけで、CI は何も検査していない

全体で 3000 行 (テスト含む) の現在は人力で守れている。しかし機能が増えれば
必ず崩れる —— 相手モジュールを直接 import する方が数行短く書けてしまう
瞬間が来る。そのとき判断はレビュー時の温度に委ねられる。

## モジュール間の繋ぎ方

コメント取得・投稿は「親スレッドが存在するか」を知る必要がある。
素朴に書けば `comment` から `thread` を import することになるが、
そうすると 2 モジュールは一体化し、縦割りは名前だけになる。

現在は **必要な操作だけのインターフェースを、利用側 (comment) が定義する**
方式を採っている。

```go
// internal/comment/domain/repository/repository.go
type ThreadExistenceChecker interface {
    Exists(ctx context.Context, id int64) (bool, error)
}
```

```go
// internal/comment/usecase/interactor.go
type CommentInteractor struct {
    repo    repository.CommentRepository
    threads repository.ThreadExistenceChecker  // comment 側のインターフェース
}
```

実装は `postgres.ThreadRepository` が満たし、結線は `cmd/api/main.go` が行う。
`comment` は `thread` の存在を知らないまま、必要な情報だけを得る。

この結果、`thread` と `comment` の間には相互 import が 1 件もない。

## 検討した選択肢

### A. 規約のみ (現状維持) + レビューで守る

- 利点: 追加コストゼロ
- 欠点: 崩れても気づけない。崩れ方は「一度に大きく」ではなく
  「毎回 1 行ずつ」なので、レビューでは通ってしまう

### B. `depguard` で import を検査する

- 利点: 既に導入済みの golangci-lint の linter を 1 つ増やすだけ。
  違反箇所と直し方が違反した行に出る
- 欠点: 検査できるのは import の有無だけ。
  型の流出 (アダプタ由来の型がドメインに現れるなど) は捕まえられない

### C. `go-arch-lint` などの専用ツールを入れる

- 利点: レイヤをより厳密に宣言できる
- 欠点: ツールとバージョン固定が 1 つ増える。
  この規模で B より多くを捕まえられるとは考えにくい

### D. モジュールごとに別の Go module へ分割する

- 利点: 境界が言語機能で強制される。最も硬い
- 欠点: 2 モジュールで `go.work` と多重の go.mod を管理することになる。
  段階的に切り出す前提が要る規模ではない

## 決定

**B (depguard) を採用した。**

`apps/go-api/.golangci.yml` に 3 つのルールを追加している。

| ルール | 対象 | 禁止するもの |
| --- | --- | --- |
| `thread-module` | `internal/thread/**` | `comment` モジュール、`httpapi`、`infrastructure`、`pgx`、`gin` |
| `comment-module` | `internal/comment/**` | `thread` モジュール、`httpapi`、`infrastructure`、`pgx`、`gin` |
| `domain-layer` | `internal/*/domain/**` | 各モジュールの `usecase` |

`pgx` と `gin` を禁止対象に含めているのは、依存方向の違反が
自前パッケージ経由だけでなく、ドライバやフレームワークの型が
そのままドメインに現れる形でも起きるため。

`apperr` / `pagination` / `config` はモジュール共有の下地として
どこからでも import を許している。いずれもドメイン知識を持たず、
どのモジュールにも属さない。

### 検証

現在のコードは `0 issues` で通る。

さらに、ルールが実際に発火することを意図的な違反 4 件で確認した
(いずれも検証後に削除済み)。

| 入れた違反 | 発火したルール |
| --- | --- |
| `comment/usecase` → `thread/domain/model` | `comment-module` |
| `thread/domain/model` → `pgx/v5/pgtype` | `thread-module` |
| `thread/usecase` → `infrastructure/postgres` | `thread-module` |
| ダミーの `*/domain/**` → `thread/usecase` | `domain-layer` |

`domain-layer` だけダミーのパッケージで検証したのは、
現在の `usecase` が `domain` を import しているため、
実コードで `domain` → `usecase` を書くと depguard より先に
循環 import のコンパイルエラーになるから。
このルールは現状では実質的に将来への保険である。

## 意図的に守っていない境界

**`infrastructure/postgres` は `thread` と `comment` の両方の
リポジトリ実装を 1 パッケージに抱えている。** 縦割りとしては純度が落ちる。

これを `thread/infrastructure` と `comment/infrastructure` に割らないのは、
`sqlc` が全クエリを単一パッケージ (`postgres/sqlcgen`) に生成するため。
割るには生成器の設定と戦うことになり、得られる純度に見合わない。

アダプタが両モジュールを知っていること自体は、依存方向としては正しい
(外側が内側を知る)。塞ぐべきは内側から外側への参照であり、そちらは
`thread-module` / `comment-module` で塞いである。

## 結果

- `make lint` (と CI の同じ手順) で境界違反が落ちる
- 違反したとき、直し方が lint のメッセージとして出る
  (例: 「必要な操作だけのインターフェースを自モジュール側に定義して
  cmd/api で実装を注入する」)
- 新しい機能モジュールを足すときは、`.golangci.yml` にルールを
  1 つ追加する必要がある。忘れると新モジュールだけ無検査になる

### 引き受けるコスト

- モジュールを増やすたびにルールを増やす手作業が残る
  (`files` を `**/internal/*/domain/**` のように総称で書けるのは
  レイヤ方向だけで、モジュール間の対称な禁止は列挙が必要)
- import 以外の流出は検出できない。
  型の流出を防ぐのは引き続きレビューの仕事
