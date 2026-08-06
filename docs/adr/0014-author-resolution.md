# ADR 0014: 投稿者情報の解決 —— N+1 とモジュール境界の両立

- ステータス: **採用 (実装前)**
- 日付: 2026-08-05

## 背景

[ADR 0005](0005-authentication.md) で `threads` / `comments` に
`author_id` が入る。一覧に投稿者名を表示するには `users` を引く必要がある。

素朴に書くと、スレッド 20 件の一覧で **1 + 20 = 21 クエリ**になる。
典型的な N+1 であり、[ADR 0009](0009-scaling-strategy.md) で
「ピーク読み取り 4,200 RPS」と見積もった負荷の上にこれが乗る。

### 既存の一覧クエリは既に N+1 を潰してある

`db/query/threads.sql` には、コメント数の集計について
**相関サブクエリと `LEFT JOIN + GROUP BY` を実測比較した記録**が残っている
(`5.84 - 7.43 ms` / shared buffers 2,054、両者を `FULL JOIN` して差分 0 件を確認)。

**同じ水準の検討を、投稿者の解決についても行う。**
ただし今回は集約 (`COUNT`) ではなく 1:1 の参照であり、事情が異なる。

## この問題はモジュール境界とぶつかる

[ADR 0004](0004-modular-monolith.md) により、
**`thread` モジュールは `user` モジュールを import できない**。

一方で `infrastructure/postgres` は両方のリポジトリ実装を抱えており、
ADR 0004 は「アダプタが両モジュールを知っていること自体は
依存方向としては正しい (外側が内側を知る)」と明記している。

**JOIN を書ける場所はアダプタだけ**になる。

### 型をどう持つか

`thread` が `user` を知らないまま投稿者を表現するため、
**表示に必要な情報だけを持つ値オブジェクトを `thread` 側に定義する**。

```go
// internal/thread/domain/model
type Author struct {
    PublicID    string   // 内部 ID は出さない (ADR 0005)
    DisplayName string
    AvatarURL   string
}

type Thread struct {
    ID     int64
    Title  string
    Author *Author       // nil = 匿名投稿
    // ...
}
```

これは ADR 0004 の
「必要な操作だけのインターフェースを利用側が定義する」(`ThreadExistenceChecker`)
と同じ考え方を、**インターフェースではなく型に適用した**形になる。

`thread` は `user.User` を知らない。知っているのは
「投稿者には公開 ID と表示名とアバターがある」ことだけになる。

**`Author` が `nil` になりうる**ことが型に現れている点が重要になる。
匿名投稿 ([ADR 0005](0005-authentication.md)) を表示側が
取りこぼすと nil 参照で落ちるため、これを型で強制する。

## 検討した選択肢

### A. `LEFT JOIN users` で 1 クエリにする (採用)

```sql
SELECT t.id, t.title, t.created_at,
       u.public_id, u.display_name, u.avatar_url,
       (SELECT count(*) FROM comments c WHERE ...) AS comment_count
FROM threads t
LEFT JOIN users u ON u.id = t.author_id
WHERE t.deleted_at IS NULL AND t.id < $1
ORDER BY t.id DESC
LIMIT $2;
```

- 利点: 1 往復で済む。`users.id` は主キーなのでインデックスが効く
- 欠点: 同じ投稿者が複数行に出ると、同じユーザー情報が重複して転送される

**`LEFT` であることが必須**になる。`INNER JOIN` にすると
匿名投稿 (`author_id IS NULL`) が一覧から消える。

### B. 投稿者をまとめて引く (2 クエリ)

一覧を取得してから `author_id` を集め、`WHERE id = ANY($1)` で一括取得し、
ユースケース層で突き合わせる。

- 利点: 投稿者の重複が排除される。モジュール境界がより綺麗になる
  (`thread` 側が `AuthorResolver` インターフェースを定義し、
  アダプタは `user` の知識を持たずに済む)
- 欠点: 往復が 2 回になる

### C. 素朴に 1 件ずつ引く

N+1 そのもの。採用しない。
ただし **Phase 4 の比較対象としては実装する** ——
既に `FetchThreadListNPlusOne` が同じ目的で置かれているのと同じ扱いになる。

## 決定

**初手は A (`LEFT JOIN`) を採用する。**

コメント数の集約で `JOIN + GROUP BY` が負けたのは、
**全コメントを読んでから集約する**ためだった。
投稿者の解決は 1:1 の参照であり、主キーへの結合なので事情が違う。

### ただしコメント一覧では B が勝つ可能性がある

| 経路 | 件数 | 投稿者の重複 |
| --- | --- | --- |
| スレッド一覧 | 20 件 | 少ない |
| **コメント一覧** | 最大 100 件 | **多い** (同じ人が連投する) |

コメント一覧では同じ投稿者が何度も現れるため、
A では同一ユーザーの情報を何度も転送することになる。

**両方を実装して Phase 4 で測る。**
[README](../../README.md) の Phase 4 に
「N+1 vs 単一クエリ」の項目が既にあるため、そこに合流させる。

## N+1 を再発させない仕組み

一度潰しても、機能追加のたびに戻ってくるのが N+1 になる。

- `pg_stat_statements` で「呼び出し回数が異常に多いクエリ」を観測する
  (README の「性能の観測」に既に手順がある)
- **スモークテストで、一覧取得 1 回あたりの発行クエリ数を検証する**ことを検討する
  ([ADR 0003](0003-open-questions.md) の未決 #14 と合わせて判断)

クエリ数の検証はフェイクのリポジトリでは書けないため、
実 DB を使うスモークテスト側の話になる。

## 引き受けるコスト

- **アダプタが `users` の列を知ることになる。**
  `infrastructure/postgres` の中で `thread` と `user` の両方の
  テーブルを参照するクエリが生まれる。
  依存方向としては正しいが、sqlc の生成物も両方をまたぐ
- **`Author` 型が `thread` と `comment` の両モジュールに重複する。**
  共有パッケージに切り出すと、そこが新たなモジュール間の結合点になる。
  重複を許容し、それぞれが自分の表示要件を持つ形にする
- **匿名投稿の分岐が全表示経路に入る。**
  `Author` が `nil` のケースを、一覧・詳細・マイページ・
  モデレーション画面のすべてで扱う必要がある
- **投稿者情報の更新が一覧に即座に反映される。**
  表示名を変更すると過去の投稿の表示も変わる。
  「投稿時点の表示名」を保持する要求が出たら、設計が変わる
