# PostgreSQL の SSI (Serializable Snapshot Isolation)

本プロジェクトが PostgreSQL を採用した最大の理由
([ADR 0001](adr/0001-why-postgresql.md)) を、実装者向けにまとめる。

---

## 1. 前提: 分離レベルと異常

複数トランザクションを同時に走らせると、直列に実行した場合と違う結果が出うる。
これを「異常 (anomaly)」と呼ぶ。SQL 標準が定義する代表的なものは 3 つ。

| 異常 | 内容 |
| --- | --- |
| ダーティリード | 未コミットの値が読める |
| ノンリピータブルリード | 同じ行を 2 回読むと値が変わっている |
| ファントムリード | 同じ条件で 2 回検索すると行数が変わっている |

PostgreSQL の分離レベルと、防げる異常は以下のとおり。

| 分離レベル | ダーティ | ノンリピータブル | ファントム | 直列化異常 |
| --- | --- | --- | --- | --- |
| READ COMMITTED (既定) | 防ぐ | 起きる | 起きる | 起きる |
| REPEATABLE READ | 防ぐ | 防ぐ | **防ぐ** | 起きる |
| SERIALIZABLE | 防ぐ | 防ぐ | 防ぐ | **防ぐ** |

PostgreSQL の `REPEATABLE READ` は実際にはスナップショット分離であり、
標準が要求する以上に強く、**ファントムリードまで防ぐ**。

しかしスナップショット分離でも防げない異常が残る。それが次の「書き込みスキュー」。

---

## 2. スナップショット分離で防げない異常: 書き込みスキュー

### 掲示板での具体例

「1 スレッドのコメントは 1000 件まで」という制約があるとする。
アプリはこう実装する。

```sql
-- 1. 現在の件数を数える
SELECT count(*) FROM comments WHERE thread_id = 1;
-- 2. 1000 未満なら挿入する
INSERT INTO comments (thread_id, body) VALUES (1, '...');
```

いま 999 件の状態で、A さんと B さんが同時に投稿したとする。

```
時刻   トランザクション A            トランザクション B
────────────────────────────────────────────────────────
 t1    BEGIN
 t2                                 BEGIN
 t3    SELECT count(*) → 999
 t4                                 SELECT count(*) → 999
 t5    999 < 1000 なので INSERT
 t6                                 999 < 1000 なので INSERT
 t7    COMMIT
 t8                                 COMMIT
                                    → 1001 件になる (制約違反)
```

**両者は別の行を挿入しているので、行ロックでは衝突しない。**
A が読んだ「件数」を B が書き換え、B が読んだ「件数」を A が書き換えている。
互いに相手の書き込みを見ていないだけで、
個々のトランザクションから見れば矛盾はどこにも無い。

これが**書き込みスキュー (write skew)** で、
スナップショット分離 (= PostgreSQL の `REPEATABLE READ`) では防げない。

### 従来の対処法

アプリ側で明示的にロックを取る。

```sql
-- 親行をロックして、同じスレッドへの投稿を直列化する
SELECT id FROM threads WHERE id = 1 FOR UPDATE;
```

確実だが、**書き込み同士が完全に直列化される**ため、
同一スレッドへの同時投稿がすべて待たされる。
しかも「どこにロックを置くべきか」の判断を人間が誤ると、
静かにバグが残る (テストでは再現しにくい)。

---

## 3. SSI が解く問題

PostgreSQL 9.1 以降の `SERIALIZABLE` は
**SSI (Serializable Snapshot Isolation)** で実装されている。

考え方はこうである。

> スナップショット分離のまま動かす。
> ただし「直列化できない実行順序」が生まれたことを検出したら、
> あとからトランザクションを中止する。

つまり**楽観的**な方式である。事前にロックを取らず、事後に検出する。

### どうやって検出するか

SSI は実行中に **rw-依存 (read-write conflict)** を追跡する。
「T1 が読んだデータを、T2 が後から書き換えた」という関係のことである。

理論的に、直列化異常が起きるときは必ず、
依存グラフの中に**連続する 2 本の rw-依存**を含む構造が現れることが分かっている。

```
     rw          rw
T_in ───→ T_pivot ───→ T_out
```

SSI はこの形 (dangerous structure) を監視し、
見つけたらどれかのトランザクションを中止する。

先の掲示板の例では:

- A は「thread_id = 1 のコメント集合」を読んだ → B がそこに書いた (rw-依存)
- B も同じ集合を読んだ → A がそこに書いた (rw-依存)

2 本の rw-依存が揃うので、**後からコミットしようとした側が中止される**。

### 述語ロック (predicate lock)

「読んだ範囲」を追跡するために、SSI は **SIREAD ロック**という印を付ける。
これは他のトランザクションを**ブロックしない**。
待たせるためのロックではなく、競合検出のための記録である。

そのため、`SELECT ... FOR UPDATE` のような明示ロックと違い、
**読み取りが書き込みを待たせることがない**。ここが性能上の利点になる。

---

## 4. 引き受けるコスト: 直列化失敗のリトライ

SSI を使うと、正常系でも次のエラーが返りうる。

```
ERROR:  could not serialize access due to read/write dependencies among transactions
SQLSTATE: 40001
```

これは**バグではなく想定内の事象**である。
アプリは同じトランザクションを**最初からやり直す**必要がある。

> **重要:** 中止されたトランザクションの途中結果は使えない。
> 「失敗した箇所から再開」ではなく、`BEGIN` からやり直す。

本リポジトリでは、この判定を
[`internal/infrastructure/postgres/errors.go`](../apps/go-api/internal/infrastructure/postgres/errors.go)
の `IsRetryable` に集約している。
デッドロック (`40P01`) も同じくリトライ可能として扱う。

```go
// SQLSTATE 40001 / 40P01 を「一時的な失敗」として識別する
func IsRetryable(err error) bool { ... }
```

HTTP 層では `apperr.ErrConflict` 経由で **409 Conflict** にマップされ、
クライアントに再試行を促す。

### リトライを書くときの注意

- **指数バックオフを入れる。** 即座に再試行すると同じ競合を繰り返す
- **上限回数を決める。** 無限リトライは障害時に DB を押し潰す
- **トランザクションを短くする。** 長いほど競合確率が上がる
- **トランザクション内で外部 API を呼ばない。** リトライで二重実行される

---

## 5. MySQL との比較

| | PostgreSQL | MySQL (InnoDB) |
| --- | --- | --- |
| 既定の分離レベル | READ COMMITTED | REPEATABLE READ |
| REPEATABLE READ の実装 | スナップショット分離 | MVCC + **ギャップロック** |
| SERIALIZABLE の実装 | **SSI (楽観的)** | 全 SELECT を共有ロック化 (**悲観的**) |
| 読み取りが書き込みを待たせるか | 待たせない | SERIALIZABLE では待たせる |
| 直列化失敗のリトライ | **必要** | 基本的に不要 (代わりに待つ) |

### MySQL のギャップロック

MySQL の既定 `REPEATABLE READ` はファントムを防ぐために
**ギャップロック**(行と行の「隙間」に対するロック) を取る。

これは本アプリのアクセスパターン
——「同一スレッドに複数ユーザーが同時にコメントする」——
と相性が悪く、**別の行を挿入しているだけでデッドロックが発生しうる**。

PostgreSQL の既定 `READ COMMITTED` にギャップロックは存在しない。

### どちらが優れているか

**ワークロードによる。**

- 競合が**少ない**なら SSI が有利。ロック待ちが発生しないぶんスループットが出る
- 競合が**多い**なら悲観ロックが有利。SSI はリトライが多発して仕事が無駄になる

本プロジェクトが PostgreSQL を選んだのは
「SSI のほうが速いから」ではなく、
**同一 DB 上で楽観 (SSI) と悲観 (`FOR UPDATE`) の両方を実装して比較計測できるから**である。
`db/query/comments.sql` に `LockThreadForUpdate` を残してあるのはそのため。

### 実際に測ったら SSI が負けた

Phase 2 でレス番号 (スレッド内連番) の採番を題材に 4 実装を比較した結果、
**SSI が正しい 3 モードの中で最も遅かった**
([ADR 0019](adr/0019-comment-concurrency.md) の「実測」節)。

理由は上の基準どおりである。採番は
**投稿者全員が同じ集合 (`MAX(seq)`) を読み、全員がそこに書く**ため、
rw 依存が事実上すべての組み合わせで成立する。
つまり「競合が多い」側の極端な例になっている。

**SSI が向くかどうかは、分離レベルの優劣ではなくワークロードで決まる。**
この文書の 5 節はそう書いていたが、
それを自分のコードで確かめるまでは一般論のままだった。

---

## 6. 実際に使う

### トランザクションの開始

```go
tx, err := pool.BeginTx(ctx, pgx.TxOptions{
    IsoLevel: pgx.Serializable,
})
```

### 読み取り専用トランザクション

読み取りしかしないなら、これを付けると SSI のオーバーヘッドが下がる。

```sql
BEGIN TRANSACTION ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE;
```

`DEFERRABLE` は「安全なスナップショットが取れるまで待つ」指定で、
**待つ代わりに直列化失敗が絶対に起きない**。
夜間バッチの集計など、待てる処理に向く。

### 手元で異常を再現する

```bash
make up
make psql   # ターミナル 1
make psql   # ターミナル 2 (別ウィンドウ)
```

両方で以下を実行すると、後からコミットした側が 40001 で落ちる。

```sql
BEGIN TRANSACTION ISOLATION LEVEL SERIALIZABLE;
SELECT count(*) FROM comments WHERE thread_id = 1;
INSERT INTO comments (thread_id, body) VALUES (1, 'test');
COMMIT;
```

---

## 7. 注意点

- **リードレプリカでは使えない。** ホットスタンバイ上で `SERIALIZABLE` は
  サポートされない (`READ ONLY DEFERRABLE` も含む)
- **述語ロックはメモリを消費する。** 追跡量が上限を超えると、
  ロックが粒度の粗いもの (ページ単位、テーブル単位) に昇格し、
  誤検出による中止が増える。`max_pred_locks_per_transaction` で調整する
- **すべてのトランザクションが SERIALIZABLE である必要がある。**
  一部が READ COMMITTED で動いていると、SSI の保証は成立しない
- **順序は保証されない。** SSI が保証するのは「何らかの直列実行と等価であること」であり、
  「コミット順どおりの直列実行と等価であること」ではない

---

## 参考

- PostgreSQL 公式ドキュメント: Transaction Isolation
- Ports & Grittner, *Serializable Snapshot Isolation in PostgreSQL* (VLDB 2012)
- Cahill et al., *Serializable Isolation for Snapshot Databases* (SIGMOD 2008)
