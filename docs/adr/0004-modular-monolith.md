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
| `thread-module` | `internal/thread/**` (テスト以外) | `comment` モジュール、`httpapi`、`infrastructure`、`pgx`、`gin` |
| `comment-module` | `internal/comment/**` (テスト以外) | `thread` モジュール、`httpapi`、`infrastructure`、`pgx`、`gin` |
| `thread-module-test` | `internal/thread/**` | `comment` モジュール、`httpapi`、`gin` |
| `comment-module-test` | `internal/comment/**` | `thread` モジュール、`httpapi`、`gin` |
| `httpapi-layer` | `internal/httpapi/**` (テスト以外) | 各モジュールの `domain`、`infrastructure`、`pgx` |
| `httpapi-test` | `internal/httpapi/**` | `infrastructure`、`pgx` |
| `domain-layer` | `internal/*/domain/**` | 各モジュールの `usecase` |

`pgx` と `gin` を禁止対象に含めているのは、依存方向の違反が
自前パッケージ経由だけでなく、ドライバやフレームワークの型が
そのままドメインに現れる形でも起きるため。

`apperr` / `pagination` / `config` はモジュール共有の下地として
どこからでも import を許している。いずれもドメイン知識を持たず、
どのモジュールにも属さない。

### HTTP 層を別ルールにしている理由

`httpapi` がドメインモデルを直接 import できると、内部表現がそのまま
JSON に晒される崩れ方を防げない。これは `api/openapi.yaml` を単一の正とする
[ADR 0002](0002-openapi-direction.md) の前提と衝突する。
ハンドラは `usecase` の DTO を受け取り、`oapigen` の生成型へ詰め替える。

`infrastructure` と `pgx` も禁止している。現在 `httpapi` は永続化層を
`Pinger` インターフェース (`internal/httpapi/server.go`) で受けており、
DB ドライバの型を一切知らない。この状態を維持する。

### テストコードの扱い

テストにも同じルールを適用すると、実 DB を使う統合テストを `usecase` 層に
置けなくなる。Phase 2 (SERIALIZABLE + リトライ) では直列化失敗
(SQLSTATE 40001) を扱うが、これはフェイクでは再現できず実 DB が要る。

そのため **テストでは `infrastructure` と `pgx` を許し、モジュール境界
(相手モジュール) と HTTP 層は本体と同じく閉じたまま**にしている。
「テストなら何でも import してよい」にはしない。それでは境界が実質消える。

`httpapi` のテストだけは `domain` を許している。ハンドラのテストは
フェイクのリポジトリを組むためにドメインの型を必要とし、ここを縛ると
テストのために本番コードを歪めることになるため。永続化層への直接依存は
テストであっても禁止のまま。

#### 落とし穴: `files` は AND ではなく OR

depguard の `files` に複数パターンを並べると **OR** で評価される。
そのため「モジュール配下 かつ テストファイル」を 1 ルールで表現できない。

```yaml
# 誤り: 「全テストファイル」にマッチしてしまう
files:
  - "**/internal/thread/**"
  - "$test"
```

代わりに、本体用ルールに `"!$test"` を付けて非テストに限定し、
緩和用ルールはモジュール配下すべてを対象に書いている。
非テストのファイルには両方のルールが適用され、本体用の deny の方が広いため、
結果として「テストにだけ緩和が効く」形になる。

緩和用ルールを thread 用と comment 用に分けているのは、1 つにまとめると
deny に両モジュールを並べることになり、自モジュールへの参照まで
禁止されてしまうため (thread のテストが `thread/domain/model` を使えない)。

### 検証

現在のコードは `0 issues` で通る。

さらに、**落ちること**と**通ること**の両方を意図的なプローブ 7 件で確認した
(いずれも検証後に削除済み)。

| # | 入れたコード | 期待 | 結果 |
| --- | --- | --- | --- |
| A | 本体 `comment/usecase` → `thread/domain/model` | 落ちる | `comment-module` |
| B | 本体 `thread/domain/model` → `pgx/v5/pgtype` | 落ちる | `thread-module` |
| C | 本体 `thread/usecase` → `infrastructure/postgres` | 落ちる | `thread-module` |
| D | **テスト** `thread/usecase` → `infrastructure/postgres` | **通る** | 検出なし |
| E | **テスト** `comment/usecase` → `thread/domain/model` | 落ちる | `comment-module-test` |
| F | 本体 `httpapi` → `thread/domain/model` | 落ちる | `httpapi-layer` |
| G | **テスト** `httpapi` → `infrastructure/postgres` | 落ちる | `httpapi-test` |

D が「通ること」の確認であるのが要点。緩和が効いていなければ Phase 2 で
統合テストが書けず、そのとき設定を雑に緩める圧力がかかる。

なお E は、緩和用ルールを 1 つにまとめていた初版では**検出できていなかった**。
「相手モジュールは閉じたまま」と書きながら deny に入れ忘れており、
プローブを置いて初めて気づいた。設定だけを読んで正しさを判断していたら
見逃していた欠陥である。

`domain-layer` (前回の検証) だけダミーのパッケージで確認したのは、
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
  **2 つ** (本体用と緩和用) 追加する必要がある。
  忘れると新モジュールだけ無検査になる

### 引き受けるコスト

- モジュールを増やすたびにルールが 2 つ増える。
  `files` を `**/internal/*/domain/**` のように総称で書けるのはレイヤ方向だけで、
  モジュール間の対称な禁止は列挙するしかない。
  現在 7 ルールで、モジュールが 3 つになれば 9 になる。
  この数え方が破綻するようなら、`go-arch-lint` (選択肢 C) を再検討する
- 緩和用ルールは非テストのファイルにも適用される。
  本体用ルールの deny がそれを包含している前提で成り立っており、
  本体用から deny を減らすと緩和用の穴がそのまま開く。
  片方だけを編集しないこと
- import 以外の流出は検出できない。
  型の流出を防ぐのは引き続きレビューの仕事
