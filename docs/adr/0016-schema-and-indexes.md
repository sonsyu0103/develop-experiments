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

#### ただし、部分インデックスは外部キーの走査には使えない

**この原則と、下の「部分インデックスを既存の流儀に合わせる」は衝突する。**

参照先が削除されるときの整合性確認は
`WHERE author_id = $1` という形で走る。
`WHERE deleted_at IS NULL` で絞った索引は、この述語からは
**索引の条件が導けないため使われない**。論理削除済みの行も
外部キーとしては生きているので、除外した時点で使えなくなる。

| 索引 | 外部キーの走査に使えるか |
| --- | --- |
| `(icon_image_id) WHERE icon_image_id IS NOT NULL` | **使える**。整合性確認は NULL を見ないため |
| `(author_id, id DESC) WHERE deleted_at IS NULL` | **使えない** |

`author_id` については、**それを承知で部分インデックスのままにする**。

- [ADR 0005](0005-authentication.md) が「アカウント削除は `users` の行を消さず
  匿名化する」と決めているため、`users` からの `DELETE` は運用上発生しない
- `id` は `IDENTITY` なので `UPDATE` もされない
- 全体索引を別に足すと、億単位 × 8 パーティションの書き込みコストを
  「起きない操作」のために払うことになる

**`users` から行を物理削除する経路を作るなら、この判断は崩れる。**
そのときは `(author_id)` の全体索引を足すか、削除前に匿名化する。

### 部分インデックスを既存の流儀に合わせる

既存の `threads_alive_id_desc_idx` / `comments_alive_thread_id_desc_idx` は
`WHERE deleted_at IS NULL` で絞っている。
新規のものも、**索引に載せる必要のない行は除外する**。

## インデックス一覧

### `users`

| 索引 | 区分 | 根拠 |
| --- | --- | --- |
| `PRIMARY KEY (id)` | A | 投稿者解決の `WHERE id = ANY($1)` |
| `UNIQUE (google_sub)` | A | ログイン時の照合 ([ADR 0005](0005-authentication.md)) |
| `UNIQUE (public_id)` | A | API からの参照はすべてこちら |
| `(id) WHERE role <> 'user'` | C | 管理者一覧。該当は数十行なので索引は極小 |

> **この表の索引は 2 つのマイグレーションに分かれる。**
> `role` 列を足すのは `000003` ([ADR 0011](0011-moderation.md)) なので、
> `(id) WHERE role <> 'user'` を `000002` に書くと**存在しない列を参照して落ちる**。
> この索引だけは `000003` 側に置くこと。
> 下の「マイグレーションの分割」の表は列の割り当てしか書いておらず、
> 索引がどちらに乗るかを示していない。

`email` に索引を張らない。**照合に使わないため** —— メールアドレスは変更されうる。

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
| `(author_id, id DESC) WHERE deleted_at IS NULL` | C | マイページ + 外部キー |
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
| `(author_id, id DESC) WHERE deleted_at IS NULL` | C | マイページ + 外部キー |
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

Phase 4 の測定対象に加える。

### `images`

| 索引 | 区分 | 根拠 |
| --- | --- | --- |
| `PRIMARY KEY (id)` | A | 表示 |
| `UNIQUE (object_key)` | A | キーの衝突検出 |
| `(created_at) WHERE status IN ('pending','deleted')` | A | 回収バッチ ([ADR 0007](0007-image-storage.md)) |
| `(owner_id, created_at DESC)` | C | マイページ + 外部キー |

### `contact_messages` / `reports` / `moderation_actions` / `idempotency_keys`

各 ADR で決定済みのものに、外部キー分と 1 件を追加する。

| テーブル | 索引 | 区分 |
| --- | --- | --- |
| `contact_messages` | `(next_attempt_at, id) WHERE status = 'pending'` | A |
| | `(client_ip, created_at)` | A |
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

## マイグレーションの分割

**Phase 単位で分ける。** 一括で作らない。

| 番号 | 内容 | Phase |
| --- | --- | --- |
| `000002` | `users` / `sessions`、`threads`・`comments` への `author_id` | 5 |
| `000003` | `users.role` / `reports` / `moderation_actions` | 10 前半 |
| `000004` | `idempotency_keys` | 2 |
| `000005` | `images`、添付列 3 つ | 6 |
| `000006` | `threads.view_count` と人気順索引 | 7 |
| `000007` | `pg_trgm` の索引 | 11 |
| `000008` | `contact_messages` | 8 |

使わないテーブルを先に作ると、
**「設計したが実装していない」がスキーマに残る**。

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
  今回 3 本追加するので、実体としては 24 本増える
- **`view_count` の索引が HOT update を殺す** (上記)
- **`comments (id)` は一意性を保証しない。** シーケンス任せであり、
  DB は重複を防がない。手動で `id` を挿入する経路を作らないこと
- **区分 C の索引は、使われないまま書き込みコストだけ払う可能性がある。**
  `pg_stat_user_indexes` の `idx_scan` が 0 のまま推移するなら削除する
- **マイグレーションを分割したことで、順序依存が生まれる。**
  `000005` (images) より前に添付列を参照するコードを書けない。
  Phase の着手順と一致させる必要がある
