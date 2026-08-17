# ADR 0016: スキーマの集約とインデックス設計

- ステータス: **採用 (実装前)**
- 日付: 2026-08-06

## 背景

[ADR 0005](0005-authentication.md) 〜 [ADR 0015](0015-idempotency.md) に、
スキーマの断片が分散している。

- 新規テーブル: `users` / `sessions` / `images` / `contact_messages` /
  `reports` / `moderation_actions` / `idempotency_keys` の **7 つ**
- 既存への追加: `threads` に 3 列、`comments` に 2 列、`users` に `role`

各 ADR は単独では整合しているが、**1 枚に集めると衝突する箇所がある**。
この ADR で集約し、インデックスを設計する。

## 集約して初めて見えた問題

**4 件。いずれも個別の ADR を読んでいる限り気づけなかった。**

### 問題 1: `comments.author_name` が既に存在する

```sql
author_name TEXT NOT NULL DEFAULT '名無しさん'
```

[ADR 0005](0005-authentication.md) は `author_id` を追加すると決めたが、
**既存の `author_name` との関係を定義していない**。

両方あると、投稿者の表示名が 2 か所から来ることになる。

#### 決定

**`author_name` を残し、匿名投稿専用の列として扱う。**

| `author_id` | `author_name` | 表示 |
| --- | --- | --- |
| `NULL` (匿名) | 使う | 入力値、既定は「名無しさん」 |
| あり (ログイン済み) | **無視する** | `users.display_name` |

掲示板として、匿名でも名前を名乗れる文化を残す。
ただし**ログイン済みの投稿では `author_name` を読まない** ——
両方を表示に使うと、どちらが正か分からなくなる。

**なりすましの問題が残る。** 匿名で他人の表示名を名乗れる。
UI 側でログイン済み投稿を視覚的に区別する (バッジなど) 必要がある。
これは表示の責任であり、スキーマでは解決しない。

### 問題 2: `comments` は `id` 単独で引けない

主キーが `(thread_id, id)` になっている。
パーティションテーブルの一意制約はパーティションキーを含む必要があるため、
これは避けられない。

```sql
CONSTRAINT comments_pkey PRIMARY KEY (thread_id, id)
```

一方で [ADR 0011](0011-moderation.md) の `reports` と `moderation_actions` は、
**`target_id` だけでコメントを参照する**設計になっている。

`comments_id_seq` は単一シーケンスなので `id` の値自体はグローバルに一意だが、
**`id` だけの索引が無いため `WHERE id = $1` は 8 パーティション全走査**になる。

#### 検討した選択肢

**A. `reports` / `moderation_actions` に `target_thread_id` を持たせる**

- 利点: `(thread_id, id)` でピンポイントに引ける
- 欠点: `target_type` によって使う列が変わる。スキーマが複雑になる

**B. `comments (id)` に非 UNIQUE インデックスを貼る (採用)**

- 利点: スキーマが単純なまま
- 欠点: 8 パーティションそれぞれにインデックスシークが走る

#### 決定

**B を採る。** 通報とモデレーションは頻度が低く、
8 回のインデックスシークは許容できる。
一覧のような高頻度経路ではないため、ここに複雑さを払う価値がない。

```sql
CREATE INDEX comments_id_idx ON comments (id);
```

**この索引は UNIQUE にできない** (パーティションキーを含まないため)。
`id` の一意性はシーケンスによって保たれており、DB は保証しない。

### 問題 3: 画像の物理削除が外部キーと衝突する

[ADR 0007](0007-image-storage.md) は
「回収バッチがストレージ側のオブジェクトと **DB 行の両方を削除する**」と書いている。

しかし [ADR 0011](0011-moderation.md) で `status = 'deleted'` を追加したため、
**削除対象の画像はコメントやスレッドから参照されている**。
外部キーがあるため、そのまま `DELETE` すると失敗する。

#### 決定

**`status` によって扱いを分ける。**

| status | DB 行 | S3 オブジェクト |
| --- | --- | --- |
| `pending` の孤児 (添付されなかった) | **物理削除する** | 削除する |
| `deleted` (モデレーターが削除) | **残す** | 削除する |

`deleted` の行を残すのは、
**「画像は削除されました」を表示するために参照が必要**なため。
行を消して `image_id` を `NULL` にすると、
「元から画像がない」と区別できなくなる。

DB 行は数百バイトで、実体は S3 にある。行を残す費用は無視できる。

**[ADR 0007](0007-image-storage.md) の記述はこの ADR で上書きする。**

### 問題 4: `sessions` の DDL がどこにも無い

[ADR 0005](0005-authentication.md) は
「セッションの実体は PostgreSQL に置く」と文章で書いているだけで、
**テーブル定義を書いていない**。集約時に落としかけた。

```sql
CREATE TABLE sessions (
    id         TEXT        PRIMARY KEY,   -- 暗号論的乱数。連番は論外
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

## 命名の統一

「誰が」を指す列が 5 種類に分かれていた。**役割で使い分ける**と決める。

| 列名 | 意味 | 使うテーブル |
| --- | --- | --- |
| `author_id` | **その内容を書いた人** | `threads` / `comments` |
| `owner_id` | **その資源を所有する人** | `images` |
| `user_id` | 単に紐づくユーザー | `sessions` / `contact_messages` / `idempotency_keys` |
| `reporter_id` | 通報した人 | `reports` |
| `actor_id` | 操作を実行した人 | `moderation_actions` |

`author_id` と `owner_id` を分けるのは、
**画像は「書いたもの」ではなく「置いたもの」**であるため。
所有者は投稿を消しても変わらず、画像単体で権限判定の対象になる。

## インデックス設計の原則

**インデックスを増やすことは、性能を上げることと同義ではない。**

- 書き込みのたびに全インデックスが更新される
- プランナの選択肢が増え、誤選択の余地も増える
- メモリ (shared buffers) を奪い合う

このリポジトリは既に**実測してから選ぶ**文化がある
(`db/query/threads.sql` の相関サブクエリ vs `JOIN + GROUP BY` の比較記録)。
同じ基準を保つため、インデックスを 3 段階に分ける。

| 区分 | 方針 |
| --- | --- |
| **A. 測らずに貼る** | 主キー・一意制約・**外部キー**・一覧のソート順・定期処理が拾う行 |
| **B. 設計上必要** | 人気順・全文検索。ADR で決定済み |
| **C. 測ってから貼る** | マイページ・管理画面など、頻度が読めないもの |

### 外部キーには必ず索引を貼る

**PostgreSQL は外部キーを張っても、参照する側に索引を自動作成しない。**

参照先の行が削除・更新されるとき、参照元を走査して整合を確認する。
索引が無いと**全表走査**になる。

`comments` は 8 パーティション・将来的に億単位の行を持つため、
ここを落とすと画像 1 枚の削除がテーブル全体の走査になる。

#### 部分インデックスの述語は「何を除外するか」で選ぶ

**外部キー用の索引に `deleted_at IS NULL` を使ってはいけない。**

整合性確認も、アカウント削除時の匿名化も、
`WHERE author_id = $1` という形で走る。
`WHERE deleted_at IS NULL` で絞った索引はこの述語から条件を導けず、
**候補にすら入らない**。論理削除済みの行も外部キーとしては生きているため。

正しい述語は `WHERE author_id IS NOT NULL` になる。
`author_id = $1` は strict な演算子なので `IS NOT NULL` が導け、
索引が使える。

| 索引 | 外部キーの走査に使えるか |
| --- | --- |
| `(icon_image_id) WHERE icon_image_id IS NOT NULL` | **使える** |
| `(author_id, id DESC) WHERE author_id IS NOT NULL` | **使える** |
| `(author_id, id DESC) WHERE deleted_at IS NULL` | **使えない** |

しかも**索引が小さくなる**。ログイン必須ではない設計
([ADR 0005](0005-authentication.md) 決定 2) では
`author_id IS NULL` の匿名投稿が多数派になり、
B-tree は NULL を格納するため、`deleted_at` で絞っただけでは
生存行のほぼ全件が索引に載る。

実測 (コメント 50,000 件・匿名 90%、8 パーティション合計):

| 述語 | サイズ |
| --- | --- |
| `WHERE deleted_at IS NULL` | 2,856 kB |
| `WHERE author_id IS NOT NULL` | **352 kB** (約 1/8) |

どちらも**空の状態で索引を作ってから行を入れた**値になる
(マイグレーションが通る実際の経路と同じ)。
既存データの上に後から作ると、ページが密に詰まるぶんさらに小さくなる
(同じデータで 232 kB)。条件を混ぜると比較にならないので揃えてある。

> **この節の初版は誤っていた。**
> 「外部キーには索引を貼る」と「部分インデックスを既存の流儀に合わせる」が
> `author_id` では両立しない、と書いていた。両立する。
> さらに免責として挙げた 2 点も誤っている。
>
> - 「`users` からの `DELETE` は起きない」 ——
>   問題になるのは参照先の `DELETE` だけではない。
>   [ADR 0005](0005-authentication.md) が決めたアカウント削除は
>   `UPDATE comments SET author_id = NULL WHERE author_id = $1` であり、
>   論理削除済みの行も対象にする必要がある。**確実に起きる**
> - 「`id` は `IDENTITY` なので `UPDATE` もされない」 ——
>   `GENERATED BY DEFAULT AS IDENTITY` は `UPDATE ... SET id = ...` を拒否しない
>   (拒否するのは `GENERATED ALWAYS`)。実測で確認済み。規約であって保証ではない

### 大きなテーブルへ後から索引を足すときは `CONCURRENTLY` を使う

`CREATE INDEX` は対象テーブルに `ACCESS EXCLUSIVE` を取り、
構築が終わるまで読み書きを止める。`comments` はパーティション親と
8 つの子すべてが対象になる。

`000002` の時点では実データがほぼ無いため素の `CREATE INDEX` でよいが、
**運用が始まったあとに同じ書き方をすると、構築時間まるごと停止する。**

`CONCURRENTLY` はトランザクション内で実行できず、
golang-migrate はマイグレーションをトランザクションで包む。
そのため、そのマイグレーションだけ
`-- migrate:no-transaction` 相当の分離が要る
(golang-migrate では、そのファイルを単独のマイグレーションとして切り出す)。

**億単位に育ったテーブルへ索引を足すマイグレーションは、必ず単独に分ける。**

### 部分インデックスを既存の流儀に合わせる

既存の `threads_alive_id_desc_idx` / `comments_alive_thread_id_desc_idx` は
`WHERE deleted_at IS NULL` で絞っている。
新規のものも、**索引に載せる必要のない行は除外する**。

ただし**この流儀を外部キー用の索引に持ち込まない**。
上の「部分インデックスの述語は『何を除外するか』で選ぶ」を参照。
一覧のソート順に使う索引 (`deleted_at` で絞る) と、
外部キーの走査に使う索引 (`IS NOT NULL` で絞る) は目的が違う。

## インデックス一覧

### `users`

| 索引 | 区分 | 根拠 |
| --- | --- | --- |
| `PRIMARY KEY (id)` | A | 投稿者解決の `WHERE id = ANY($1)` |
| `UNIQUE (google_sub)` | A | ログイン時の照合 ([ADR 0005](0005-authentication.md)) |
| `UNIQUE (public_id)` | A | API からの参照はすべてこちら |
| `(id) WHERE role <> 'user'` | C | 管理者一覧。該当は数十行なので索引は極小 |
| `(avatar_image_id) WHERE avatar_image_id IS NOT NULL` | **A** | **外部キー**。画像削除時の走査 |

> **最後の 1 行は初版に無かった** (`000006` の実装時に足した)。
> `comments.image_id` と `threads.icon_image_id` は下の表に載せていたのに、
> `users.avatar_image_id` だけ落としていた。
> **この ADR 自身の「外部キーには必ず索引を貼る」に反していた。**
>
> 落ちると、回収バッチが画像 1 枚を消すたびに `users` の全表走査になる
> ([ADR 0009](0009-scaling-strategy.md) の見積もりでは登録 100 万人)。
>
> 3 つの列が 3 つの別々の表に分かれて書かれていたことが原因になる。
> **「集約して初めて見える」は、集約した表の内部でも起きる。**

> **この表の索引は 2 つのマイグレーションに分かれる。**
> `role` 列を足すのは `000003` ([ADR 0011](0011-moderation.md)) なので、
> `(id) WHERE role <> 'user'` を `000002` に書くと**存在しない列を参照して落ちる**。
> この索引だけは `000003` 側に置くこと。
> 下の「マイグレーションの分割」の表は列の割り当てしか書いておらず、
> 索引がどちらに乗るかを示していない。

`email` に索引を張らない。**照合に使わないため** —— メールアドレスは変更されうる。

#### `users` に 2 つ足した ([ADR 0005](0005-authentication.md) の DDL からの差分)

| 追加 | 理由 |
| --- | --- |
| `deleted_at TIMESTAMPTZ` | 退会の表現。無いと、退会したのと同じ Google アカウントで再ログインしたときに upsert が同じ行に当たり、**同一の `users.id` が復活する** |
| `CHECK (char_length(display_name) BETWEEN 1 AND 100)` | 匿名の `author_name` は 1〜50 文字なのに、ログイン済みの表示名だけ無制限だった |

`display_name` を 50 に揃えないのは、**この値が Google から来る外部入力**であるため。
揃えると長い表示名の利用者がログインできなくなる。
切り詰めはアプリ側の責任とし、DB は暴走を止める上限だけを持つ。

**退会後の再ログインを「拒否する」のか「復帰させる」のかは未決** (Phase 5 で決める)。
`google_sub` の `UNIQUE` は全体に効いたままなので、
「別人として作り直す」だけはこのスキーマでは選べない
(選ぶなら `UNIQUE (google_sub) WHERE deleted_at IS NULL` に変える必要がある)。

### `sessions`

| 索引 | 区分 | 根拠 |
| --- | --- | --- |
| `PRIMARY KEY (id)` | A | **毎リクエスト引く**。最も高頻度の経路 |
| `(user_id)` | A | 外部キー + 「全端末からログアウト」 |
| `(expires_at)` | A | 期限切れ削除の定期処理 |

主キーがランダム値なので B-tree の挿入位置は散る。
ただし行数はアクティブユーザー数に比例する程度で、寿命も短いため許容する。

### `threads` (追加分)

| 索引 | 区分 | 根拠 |
| --- | --- | --- |
| `(view_count DESC, id DESC) WHERE deleted_at IS NULL` | B | 人気順 ([ADR 0006](0006-view-count-and-popularity.md)) |
| `USING gin (title gin_trgm_ops) WHERE deleted_at IS NULL` | B | 検索 ([ADR 0012](0012-search.md)) |
| `(author_id, id DESC) WHERE author_id IS NOT NULL` | **A** | 外部キー + 匿名化 (`UPDATE ... WHERE author_id = $1`) + マイページ |
| `(icon_image_id) WHERE icon_image_id IS NOT NULL` | A | **外部キー**。画像削除時の走査 |

**`view_count` の索引が HOT update を無効化する**
([ADR 0006](0006-view-count-and-popularity.md) の問題 3)。
定期フラッシュにしたことで更新頻度は下がっているが、
フラッシュのたびに索引更新が発生することは変わらない。
これは人気順を索引で解くための代償として受け入れる。

### `comments` (追加分)

**パーティションテーブルなので、親に作った索引は 8 つすべてに作られる。**

| 索引 | 区分 | 根拠 |
| --- | --- | --- |
| `(id)` | A | 問題 2。通報・モデレーションからの参照 |
| **`UNIQUE (thread_id, seq)`** | **A** | レス番号の一意性 ([ADR 0019](0019-comment-concurrency.md))。採番の `MAX(seq)` もこれを逆順に辿る |
| `(author_id, id DESC) WHERE author_id IS NOT NULL` | **A** | 外部キー + 匿名化 (`UPDATE ... WHERE author_id = $1`) + マイページ |
| `(image_id) WHERE image_id IS NOT NULL` | A | **外部キー**。ここが最も重要 |

#### パーティションキーを含まない検索は全パーティションを走る

`author_id` での検索は `thread_id` を含まないため、
**partition pruning が効かず 8 パーティションすべてを走査する**。

マイページの「自分のコメント一覧」がこれに当たる。
各パーティションでの索引スキャンなので致命的ではないが、
**スレッド内のコメント一覧 (pruning が効く) とはコストが桁で違う**。

これは `HASH (thread_id)` を選んだことの代償であり、
[ADR 0001](0001-why-postgresql.md) が
「参照系がほぼ全て特定スレッドのコメント」と想定した前提が、
マイページの追加で崩れたことを意味する。

#### 実測 (Phase 4 / `make partition-probe`)

`bench_part` スキーマに同じ列・同じ索引の表を「分割なし」「HASH 8」
「HASH 32」で作り、同一データを入れて比べた
(200 万行、各 5 回の中央値、`計画 + 実行` ms)。

| クエリ | 分割なし | HASH 8 | HASH 32 |
| --- | --- | --- | --- |
| 1 スレッドのコメント一覧 | **1.36** | 2.04 | 2.46 |
| 20 スレッドぶんの集計 (除外が効かない) | **1.84** | 7.04 | 11.63 |
| 時刻で絞る全体集計 (除外が効かない) | **151.8** | 214.4 | 209.5 |
| 1 行 INSERT | **1.11** | 1.43 | 1.66 |

**負けの内訳はほとんど計画時間になる。** 20 スレッドの集計では
HASH 8 の 7.04 ms のうち 5.00 ms が計画で、実行は 2.04 ms しかない。
プリペアドステートメントを使う経路 (本番の pgx はこちら) では
計画が 1 接続につき 1 回に償却されるので、この差は表に出ない。

2,000 万行での結果と、8 分割そのものの是非は
[ADR 0009](0009-scaling-strategy.md) の「実測」節にまとめてある。

#### 実測 (本番の `comments` に対して / `make query-probe` の 5 番)

上の `bench_part` は**合成した表**で、比べているのは「分割の有無」になる。
**この節の主張 (マイページとスレッド内でコストが桁で違う) は、
本番の表で直接測らないと確かめたことにならない。**

2,000 スレッド / 20 万コメント / 200 利用者、投稿数が最多の利用者
(スレッド 10 件 / コメント 1,600 件)、各 5 回の中央値:

| クエリ | 実行 | buffers | 触れた区画 |
| --- | --- | --- | --- |
| 自分のスレッド 先頭ページ | **0.37 ms** | 18 | — (分割なし) |
| 自分のコメント 先頭ページ | 1.53 ms | 78 | **8** |
| 自分のコメント 深いページ | 1.06 ms | 57 | **8** |
| スレッド内のコメント一覧 | **0.16 ms** | 6 | 1 |

**「桁で違う」は当たっていた。** 1.53 ms 対 0.16 ms で 9.6 倍、
バッファも 78 対 6 で 13 倍になる。8 区画すべてに索引スキャンが走り、
それぞれで開始位置を探すぶんが素直に積み上がる。

**ただし絶対値としては十分に速い。** 1.5 ms はマイページの一覧として
問題にならない。**索引を足す必要は無く**、`comments_author_id_desc_idx`
(000002) で足りている。

**深いページのほうが速いのは、区画あたりの候補が減るため。**
先頭ページは 8 区画それぞれから 20 件ずつ読んで併合し、
そのうち 20 件だけを残す。カーソルで絞ると各区画の走査が短くなる。

> **この節の初版の注記は誤っていた。**
> `bench_part` の「20 スレッドぶんの集計」が 3.8 倍だったことを根拠に、
> 「『桁で違う』は過大だった」と書いていた。**別のクエリの数字で
> この節の主張を否定していた。**
> 20 スレッドの集計は「20 個の thread_id がハッシュで散る」話で、
> ここが言っているのは「`author_id` に区画キーが含まれない」話になる。
> 後者を直接測ったら 9.6 倍で、元の記述のほうが正しかった。

### `images`

| 索引 | 区分 | 根拠 |
| --- | --- | --- |
| `PRIMARY KEY (id)` | A | 表示 |
| `UNIQUE (object_key)` | A | キーの衝突検出 |
| `(created_at) WHERE object_reclaimed_at IS NULL AND (attached_at IS NULL OR status = 'deleted')` | A | 回収バッチ ([ADR 0007](0007-image-storage.md)) |
| `(owner_id, created_at DESC)` | C | マイページ + 外部キー |

> **回収バッチの索引は述語を 1 度書き直している** (`000006` → `000007`)。
>
> 初版は `WHERE status IN ('pending','deleted') AND object_reclaimed_at IS NULL`
> だった。回収の対象が「放置された `pending`」と「モデレーターが消した
> `deleted`」の 2 種類しかなかった頃の述語になる。
>
> Phase 6 後半で 3 種類目 —— **`committed` の孤立**
> (アップロードしたが投稿をやめた画像) が増えた時点で成立しなくなった。
> 判定は「3 つの添付先のどれからも参照されていない」であり、
> **これは部分索引の述語に書けない**。述語はその行だけで判定できる式に
> 限られ、他テーブルを見る副問い合わせは使えないため。
> 結果、対象行の一部が索引に載らず、プランナが部分索引を選べなくなった。
>
> `attached_at`（添付先から参照された時刻）を `images` 自身に持たせて、
> 「参照されていないこと」を行の中で判定できるようにした。実測 (5 万件):
>
> | | `000006` の述語 + 初版クエリ | `000007` |
> | --- | --- | --- |
> | プラン | `Seq Scan` + `quicksort` | `Index Scan using images_reclaimable_idx` |
> | 走査して捨てた行 | 50,000 | 0 |
> | 実行時間 | 382.6 ms | 0.31 ms |
>
> **列を足したので、書き忘れという新しい失敗の形が増える。**
> 添付する側の SQL が `attached_at` を書き損ねると、参照中の画像が
> 索引の述語を通ってしまう。そのため回収クエリからは `NOT EXISTS` 3 本を
> 消していない —— 索引で候補を数件に絞ったあと、行を消す直前に確かめる。
> **列は速さのため、`NOT EXISTS` は正しさのため**と役割を分けている。

### `contact_messages` / `reports` / `moderation_actions` / `idempotency_keys`

各 ADR で決定済みのものに、外部キー分と 1 件を追加する。

| テーブル | 索引 | 区分 |
| --- | --- | --- |
| `contact_messages` | `(next_attempt_at, id) WHERE status = 'pending'` | A |
| | `(client_ip, created_at)` | A |
| | **`(user_id) WHERE user_id IS NOT NULL`** | A |
| | **`(created_at) WHERE client_ip IS NOT NULL`** | A |
| `reports` | `UNIQUE (reporter_id, target_type, target_id)` | A |
| | `(created_at) WHERE status = 'open'` | A |
| | **`(target_type, target_id) WHERE status = 'open'`** | A |
| `moderation_actions` | `(actor_id, created_at DESC)` | C |
| | `(target_type, target_id)` | A |
| `idempotency_keys` | `PRIMARY KEY (user_id, key)` | A |
| | `(created_at)` | A |

`reports` に `(target_type, target_id)` を足すのは、
一意制約の先頭が `reporter_id` のため
**「この投稿への通報を全部見る」に使えない**ため。
モデレーターが投稿を判断するときの主要な経路になる。

> **`contact_messages` の下 2 本は Phase 8 の実装時に足している。**
> 初版は [ADR 0008](0008-contact-and-mail.md) のスキーマから索引だけを
> 転記しており、**外部キー (`user_id`) の索引が漏れていた** ——
> 同じ節の「外部キーには必ず索引を貼る」に反している。
> 転記は漏れる。`reports.resolved_by` で一度踏んだのと同じ形になる。
>
> 4 本目は `client_ip` の消し込み (個人データの保持期間) が拾う行のための
> もので、**述語に `client_ip IS NOT NULL` を含めると消し込みが進むほど
> 索引が小さくなる** (`images_reclaimable_idx` と同じ考え方)。

## マイグレーションの分割

**Phase 単位で分ける。** 一括で作らない。

**番号は [README](../../README.md) の着手順に一致させる。**
Phase 番号の昇順ではない。

| 番号 | 内容 | Phase | 着手順 |
| --- | --- | --- | --- |
| `000002` | `users` / `sessions`、`threads`・`comments` への `author_id` | 5 | 1 |
| `000003` | `users.role` (+ 部分索引) | 10 前半 | 1 |
| `000004` | **`comments.seq` と `UNIQUE (thread_id, seq)`** | **2** | 2 |
| `000005` | `idempotency_keys` | 2 | 2 |
| `000006` | `images`、添付列 3 つ | 6 | 3 |
| `000007` | **`images.attached_at` と回収索引の張り直し** | **6** | 3 |
| `000008` | `reports` / `moderation_actions` | 10 後半 | 4 |
| `000009` | **`reports.target_thread_id` とキュー索引の張り替え** | **10 後半** | 4 |
| `000010` | `pg_trgm` の索引 | 11 | 5 |
| `000011` | `threads.view_count` と人気順索引 | 7 | 6 |
| `000012` | `contact_messages` | 8 | 7 |

使わないテーブルを先に作ると、
**「設計したが実装していない」がスキーマに残る**。

> **この表は 2 回直している。どちらも同じ誤りだった。**
>
> - **初版は `000003` に `reports` と `moderation_actions` を入れていた。**
>   しかし [README](../../README.md) の着手順は Phase 10 を
>   前半 (`role` と権限判定) と後半 (通報・管理画面) に割っており、
>   通報は後半になる。上の原則に自分で反していたので分けた
> - **2 版は `000004` を Phase 10 後半、`000005` を Phase 2 に割り当てていた。**
>   着手順では Phase 2 が 2 番、Phase 10 後半は 4 番なので、番号が逆だった。
>   Phase 2 のマイグレーションを `000004` / `000005` に置き直し、
>   以降を 1 つずつ繰り下げた ([ADR 0019](0019-comment-concurrency.md) 決定 6)
>
> **Phase 番号の昇順で並べようとすると、毎回同じ間違いをする。**
> 着手順の列を足したのはそのためになる。
>
> **3 度目の繰り下げは理由が違う** —— Phase 6 が 2 本目のマイグレーションを
> 要求した (`000007` = `images.attached_at`)。上の索引の節に書いたとおり、
> 述語を決めた時点では対象が 2 種類しか無く、3 種類目が増えて破綻した。
> 「1 Phase = 1 マイグレーション」を前提に番号を予約すると、
> **予約した番号が後続の Phase のものと衝突する**。
> 番号は着手順を表すだけで、本数を保証しない。
>
> **4 度目 (`000009` = `reports.target_thread_id`) も同じ形になった。**
> Phase 10 後半が 2 本目を要求している。しかも `000007` と同じく、
> **1 本目の設計が使い始めた時点で足りないと分かった**ケースになる ——
> `reports` は決定 4 の DDL をそのまま写して作ったが、
> コメントの通報からスレッド ID が辿れず、キューの索引も
> 一意でない列に張られていた (詳細は [ADR 0011](0011-moderation.md) の
> 実装して分かったこと 6)。
>
> **「設計した DDL をそのまま写す」ことの限界がここにある。**
> ADR に書いた時点では、その表を誰がどう引くかまでは決まっていない。

### `000004` は Phase 2 が題材のために足す列

`comments.seq` (レス番号) は [ADR 0019](0019-comment-concurrency.md) の決定 1 で、
**並行制御の題材として**導入する。他の ADR から来た断片ではないため、
この ADR の「集約して初めて見えた問題」には含まれていない。

一意制約 `UNIQUE (thread_id, seq)` はパーティションキーを先頭に含むので、
**問題 2 の制約 (`comments (id)` を UNIQUE にできない) には当たらない。**

### 既存テーブルへの `ALTER` は安全

PostgreSQL 11 以降、**`DEFAULT` 付きの `ADD COLUMN` はテーブルを書き換えない**
(メタデータのみ更新)。今回追加する列はすべて
`NULL` 許容か `DEFAULT` 付きなので、ロックは一瞬で済む。

**後から `NOT NULL` を足す場合は全行スキャン + `ACCESS EXCLUSIVE` ロック**になる。
現時点で `NOT NULL` にする必要のある列は無い。

`pg_trgm` の `CREATE EXTENSION` は `db/init/` に置く
(superuser 権限が要る運用作業であり、アプリのマイグレーションとは分ける
—— 既存の方針に従う)。

## 引き受けるコスト

- **`comments` の索引は 8 倍になる。** 親に 1 本作ると 8 パーティションに作られる。
  この ADR で 3 本、[ADR 0019](0019-comment-concurrency.md) で 1 本
  (`UNIQUE (thread_id, seq)`) 追加するので、実体としては 32 本増える
- **`view_count` の索引が HOT update を殺す** (上記)
- **`comments (id)` は一意性を保証しない。** シーケンス任せであり、
  DB は重複を防がない。手動で `id` を挿入する経路を作らないこと
- **区分 C の索引は、使われないまま書き込みコストだけ払う可能性がある。**
  `pg_stat_user_indexes` の `idx_scan` が 0 のまま推移するなら削除する
- **マイグレーションを分割したことで、順序依存が生まれる。**
  `000006` (images) より前に添付列を参照するコードを書けない。
  Phase の着手順と一致させる必要がある
