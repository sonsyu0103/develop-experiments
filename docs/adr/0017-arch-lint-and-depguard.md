# ADR 0017: モジュール境界の検査を go-arch-lint に移し、depguard を外す

- ステータス: **採用**
- 日付: 2026-08-06

## 背景

[ADR 0004](0004-modular-monolith.md) は、モジュール境界とレイヤの依存方向を
`depguard` (拒否リスト) で強制すると決めた。同時に
「現在 7 ルールで、モジュールが 3 つになれば 9 になる。
**この数え方が破綻するようなら `go-arch-lint` を再検討する**」と書いている。

[ADR 0005](0005-authentication.md) 〜 [ADR 0011](0011-moderation.md) で
モジュールは 6 つになる見込みになり、
[ADR 0003](0003-open-questions.md) の未決 #13 で
**「B (go-arch-lint へ移す) で進める。ただし決定の前に実地検証を挟む」**
と決めた。この ADR はその検証結果と、結果を受けた最終決定を記録する。

Phase 5 で `user` モジュールを足すと 3 モジュール目になる。
6 モジュール分を手で書いてから捨てるのは無駄なので、移行するなら今が最も安い。

## 検証の方法

**設定を読んで判断しない。** ADR 0004 は、緩和ルールの `deny` に
相手モジュールを入れ忘れた欠陥を、プローブを置いて初めて見つけている。
同じ水準を保つため、意図的な違反コードを置いて実測した。

プローブは ADR 0004 の 7 件 (A〜G) に 9 件を足して **16 件**にした。
足したのは、**書き忘れが「許可」に倒れるかどうか**を直接測るためのもの
(M〜P) が中心になる。

対象は `go-arch-lint v1.17.0` (2026-08-05 リリース)。

## 実測で分かったこと

### 1. テストファイルを区別できない

`go-arch-lint` にはファイル単位の条件が無く、`_test.go` も通常の
ソースとして解析される。実際、最初の設定では
`internal/httpapi/server_test.go` の 4 件が違反として出た。

`excludeFiles` は正規表現だが、Go の正規表現 (RE2) には**否定先読みが無い**ため、
「テストファイルだけを対象にする」逆向きの指定も書けない。

ADR 0004 が決めた緩和 (テストでは `infrastructure` と `pgx` を許す、
ハンドラのテストでは `domain` を許す) は、1 つの設定では表現できない。

**→ arch ファイルを 2 枚にした。** 本体用 (`_test.go` を除外) と
テスト用 (全ファイル対象・緩和あり)。本体コードには両方が適用され、
本体用の方が厳しいので、実質「テストにだけ緩和が効く」形になる。
これは depguard の 本体用 / 緩和用 の対と同じ構造だが、
**中身が許可リストなので記述量が O(n) に収まる**。

### 2. deepScan は守りたい向きの逆を宣言させる

`deepScan` (AST による DI 解析) を有効にすると、`cmd/api` での結線が
「注入元コンポーネント → 注入先コンポーネント」の依存として数えられる。

```
Dependency postgres -\-> comment-usecase not allowed
  ├─ postgres postgres.CommentRepository in /internal/infrastructure/postgres/comment_repository.go:17
  └─ comment-usecase NewCommentInteractor in /internal/comment/usecase/interactor.go:38
     /cmd/api/main.go:58
```

`main` に `anyProjectDeps: true` を付けても、`main` 側で
`deepScan: false` にしても消えない。実測したところ、消すには
**`comment-usecase` の `mayDependOn` に `postgres` を足す**必要があった。

つまり `usecase → infrastructure` を宣言することになる。これは
このアーキテクチャが塞ぎたい向きそのものであり、宣言した瞬間に
**本体コードからの import まで許可される**。

上流の [issue #67](https://github.com/fe3dback/go-arch-lint/issues/67) が
同じ現象を報告しており、v1.14.0 で「注入先が `anyProjectDeps` なら通す」
修正が入っているが、注入先が通常のコンポーネントの場合は残る。

**→ `deepScan: false` にした。** ADR 0004 が「depguard では型の流出を
検出できない」と書いた穴を埋められる機能だったが、
この結線方式とは両立しない。

### 3. コンポーネントは自分自身を参照できない

`internal/thread/domain/**` のように glob で 1 コンポーネントにまとめると、
その中の `repository` → `model` すら違反になる。

**→ `thread-model` / `thread-repository` / `thread-usecase` に分けた。**
結果として `domain → usecase` の禁止 (ADR 0004 の `domain-layer` ルール) が
副産物として表現できている。

### 4. 未宣言のパッケージ・未宣言のライブラリは通知になる

これが移行の決め手になった。

設定を更新せずに `internal/user/usecase` を足すと、
import の中身に関係なく落ちる。

```
File /internal/user/usecase/zz_probe.go not attached to any component in archfile
```

`allow.depOnAnyVendor` を外して `vendors` / `canUse` を書くと、
ライブラリについても同じになる。宣言していない依存を足すと落ちる。

### 5. depguard にも許可リストはある。ただし穴の位置が違う

検証中に `depguard` の `list-mode: strict` (許可リスト) を見つけた。
[ADR 0003](0003-open-questions.md) の未決 #13 は
「depguard = 拒否リストだから書き忘れが許可に倒れる」と書いているが、
**この前提は正確ではない**。許可リストでも書ける。

実際に Strict 版を書いて測った結果:

| プローブ | Strict 版 depguard |
| --- | --- |
| A: `comment/usecase` → `thread/domain/model` | 落ちる |
| M: **設定を更新していない新モジュール** → 他モジュール | **通る** |

`depguard` のルールは `files` の glob に一致したファイルにだけ適用される。
**どのルールにも一致しないファイルは、単に無検査になる。**
「自モジュールだけを許す」は総称パターンで書けないため
(`**/internal/*/usecase/**` に対して「自分の `internal/<自分>/**`」は表現できない)、
ルールはモジュールごとに書くしかなく、
新しいモジュールは常にどのルールにも一致しない状態から始まる。

**穴は「拒否リストであること」ではなく「一致しないファイルが無検査になること」にあった。**
`go-arch-lint` はここが逆で、どのコンポーネントにも属さないファイルを通知にする。

## 検証結果

16 プローブすべてが期待どおりになった。`落ちる` は「どちらかの arch ファイルが
落とす」を意味する。

| # | 置いたコード | 期待 | 本体用 | テスト用 | 旧 depguard |
| --- | --- | --- | --- | --- | --- |
| A | 本体 `comment/usecase` → `thread/domain/model` | 落ちる | 落ちる | 落ちる | 落ちる |
| B | 本体 `thread/domain/model` → `pgx` | 落ちる | 落ちる | 落ちる | 落ちる |
| C | 本体 `thread/usecase` → `infrastructure` | 落ちる | 落ちる | 通る | 落ちる |
| D | **テスト** `thread/usecase` → `infrastructure` | **通る** | 通る | 通る | 通る |
| E | **テスト** `comment/usecase` → `thread/domain/model` | 落ちる | 通る | 落ちる | 落ちる |
| F | 本体 `httpapi` → `thread/domain/model` | 落ちる | 落ちる | 通る | 落ちる |
| G | **テスト** `httpapi` → `infrastructure` | 落ちる | 通る | 落ちる | 落ちる |
| H | 本体 `comment/usecase` → `thread/usecase` | 落ちる | 落ちる | 落ちる | 落ちる |
| I | **テスト** `httpapi` → `thread/domain/model` | **通る** | 通る | 通る | 通る |
| J | 本体 `domain` → `usecase` | 落ちる | 落ちる | 落ちる | 落ちる |
| K | 本体 `thread/usecase` → `gin` | 落ちる | 落ちる | 落ちる | 落ちる |
| L | **テスト** `thread/usecase` → `gin` | 落ちる | 通る | 落ちる | 落ちる |
| M | **設定を更新していない新モジュール** → 他モジュール | 落ちる | 落ちる | 落ちる | **通る** |
| N | **設定を更新していない新モジュール** → `infrastructure` | 落ちる | 落ちる | 落ちる | **通る** |
| O | **宣言していないライブラリ**を本体で使う | 落ちる | 落ちる | 落ちる | **通る** |
| P | **宣言していないライブラリ**をテストで使う | 落ちる | 通る | 落ちる | **通る** |

D と I が「通ること」の確認になる。緩和が効いていなければ Phase 2 で
統合テストが書けず、そのとき設定を雑に緩める圧力がかかる。

**M〜P の 4 件が、旧 depguard 設定を素通りする。**
`go-arch-lint` の守備範囲は旧 depguard を完全に含み、逆は成り立たない。

## 決定

**モジュール境界・レイヤの依存方向・ライブラリの流出を、すべて
`go-arch-lint` の 2 枚の arch ファイルで検査する。`depguard` は外す。**

| ファイル | 役割 |
| --- | --- |
| `apps/go-api/.go-arch-lint.yml` | 本体コード用 (`_test.go` を除外) |
| `apps/go-api/.go-arch-lint.tests.yml` | テストの緩和を含む版 (全ファイル対象) |
| `.github/scripts/arch-probe.sh` | 16 プローブを CI で回す |

[ADR 0003](0003-open-questions.md) の未決 #13 は
「depguard にはレイヤとライブラリの検査だけを残す」という分割を予定していた。
**実測の結果、その分割はやめた。** レイヤもライブラリも
`go-arch-lint` 側で許可リストとして表現でき、両方に置くと
同じ規則が 2 か所に書かれることになるため。

`.golangci.yml` は **205 行から 79 行**になった。
`depguard` 由来の 126 行がそのまま消えている。

### プローブを CI に残す (ADR 0003 の選択肢 A)

`make check` と CI が `.github/scripts/arch-probe.sh` を実行する。
**「落ちるはずのものが通る」を検出するのが目的**であり、
どちらのツールを選んでも価値がある部分になる。

実行時間は 16 プローブ × 2 ファイルで約 19 秒。

前提として、プローブを置く前に両方の検査が通っていることを確認している。
そこが汚れていると以降の結果がすべて無意味になるため。

### モジュールを 1 つ足すときのコスト

arch ファイル 1 枚あたり:

- `components` に 3 行 × 2 (model / repository / usecase)
- `deps` に 7 行 (自分のレイヤ間の許可)
- `httpapi` の `mayDependOn` に 1 行
- `postgres` の `mayDependOn` に 2 行

**約 16 行 × 2 ファイル = 約 32 行。** 既存モジュールの記述には触らない。

depguard の見積もり ([ADR 0003](0003-open-questions.md) の未決 #13) は
6 モジュールで **YAML 700 行前後・`deny` エントリ約 106**、
かつモジュールを 1 つ足すたびに**既存の全モジュールの `deny` にも追記が要る**
というものだった。6 モジュールに達したときの arch ファイルは
2 枚合わせて **400 行弱**になる見込みで、追記は自分の行だけで済む。

## 罠

### arch ファイル 2 枚は必ず対で編集する

本体用から許可を減らしても、テスト用に残っていれば**テストからは通る**。
ADR 0004 が depguard の 本体用 / 緩和用 について書いた注意が、
そのまま当てはまる。

プローブがこの取り違えを検出するための装置になっている。

### `deps` に空の `{}` は書けない

依存を持たないコンポーネントは `deps` から**省く**。
`{}` を書くと設定エラーになる。

### `exclude` で生成コードを外すと、それを import する側が落ちる

`internal/httpapi/oapigen` を `exclude` に入れると、
`httpapi` からの import が「どのコンポーネントにも属さない先への依存」になり
違反として出る。生成コードもコンポーネントとして宣言し、
`canUse` で必要なライブラリだけ許すのが正しい。

### エラーメッセージから「直し方」が消えた

`depguard` は `desc` に直し方を書けた。

```
import '.../internal/thread/domain/model' is not allowed from list 'comment-module':
モジュール境界の違反。相手モジュールを直接 import せず、
必要な操作だけのインターフェースを自モジュール側に定義して
cmd/api で実装を注入する (例: comment/domain/repository.ThreadExistenceChecker)
```

`go-arch-lint` は構造だけを述べる。

```
Component comment-usecase shouldn't depend on
develop-experiments/apps/go-api/internal/thread/domain/model in .../interactor.go:12
```

**これは明確な後退になる。** 対応として、結線の方法を arch ファイルの
コメントと [ADR 0004](0004-modular-monolith.md) に残している。

## 引き受けるコスト

- **ツールとバージョン固定が 1 つ増える** (`GO_ARCH_LINT_VERSION := v1.17.0`)。
  `golangci-lint` と違い、Go の新バージョンへの追随が遅れると
  lint 全体ではなくアーキテクチャ検査だけが止まる
- **`go-arch-lint` は `golangci-lint` ほど広く使われていない** (star 528)。
  ただし放置されてはいない —— 直近 1 年で v1.13.0 〜 v1.17.0 の 5 リリースがあり、
  外部からの PR がマージされている (最終コミット 2026-08-05)
- **型の流出は引き続き検出できない。** `deepScan` を切ったため、
  ADR 0004 と同じく import 以外の流出はレビューの仕事になる
- **設定ファイルが 2 枚になった。** 対で編集する必要があり、
  片方だけ直すと穴が開く
- エラーメッセージから直し方の案内が消えた (上記)

## 決定を覆す条件

- **`go-arch-lint` が Go の新バージョンで動かなくなり、数か月放置された場合** ——
  `depguard` に戻す。[ADR 0003](0003-open-questions.md) の選択肢 A に相当し、
  `list-mode: strict` で書けば ADR 0003 が想定していたより短く書ける。
  ただし新モジュールが無検査になる穴 (プローブ M) は残るため、
  そのときはプローブを「全モジュールにルールがあること」の検査に拡張する
- **`deepScan` が DI 結線を正しく扱えるようになった場合** ——
  型の流出まで検出できるようになるため、有効化を再検討する

## 関連

- [ADR 0004](0004-modular-monolith.md) — モジュラモノリスの構成と結線方式。
  検査ツールの決定 (B: depguard) はこの ADR が置き換える
- [ADR 0003](0003-open-questions.md) — 未決 #13
