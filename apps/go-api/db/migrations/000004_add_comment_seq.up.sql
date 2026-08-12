-- =============================================================================
-- 000004: comments にレス番号 (スレッド内連番) を足す
-- =============================================================================
-- 判断の記録は docs/adr/0019-comment-concurrency.md。
--
-- この列は Phase 2 (並行制御) の題材である。
-- 採番が「読んでから書く」処理になるため、同時投稿で必ず競合する。
-- 一意制約がその競合を DB 側で観測可能にする。
-- =============================================================================

-- 一旦 NULL 許容で足す。DEFAULT を持たせられない列なので、
-- ここで NOT NULL にすると既存行が違反する。
--
-- PostgreSQL 11 以降、DEFAULT 付きの ADD COLUMN はテーブルを書き換えないが、
-- **DEFAULT の無い ADD COLUMN も書き換えない** (全行が NULL になるだけ)。
-- 重いのは下の SET NOT NULL のほう。
ALTER TABLE comments ADD COLUMN seq INTEGER;

-- 既存行を埋める。id はグローバルに単調増加するので、
-- スレッド内で id 順に並べれば投稿順になる。
--
-- 【本番規模でこれをやってはいけない】
-- パーティション親への UPDATE は 8 パーティションすべてを書き換え、
-- 行数ぶんの WAL を生む。現時点で実データがほぼ無いから許容している。
-- 億行に育ったあとで同じことをするなら、バッチで刻むか、
-- 「NULL を許して読み出し側で吸収する」ほうへ倒すことになる。
UPDATE comments AS c
SET seq = numbered.rn
FROM (
    SELECT
        thread_id,
        id,
        row_number() OVER (PARTITION BY thread_id ORDER BY id) AS rn
    FROM comments
) AS numbered
WHERE c.thread_id = numbered.thread_id
  AND c.id = numbered.id;

-- SET NOT NULL は全行スキャンと ACCESS EXCLUSIVE ロックを取る
-- (docs/adr/0016-schema-and-indexes.md の「既存テーブルへの ALTER は安全」の裏側)。
-- 実データがほぼ無い現時点だから素直に書ける。
ALTER TABLE comments ALTER COLUMN seq SET NOT NULL;

-- レス番号の一意性。
--
-- パーティションキー thread_id を先頭に含むため、
-- パーティションテーブルにそのまま張れる。
-- comments (id) を UNIQUE にできなかったのとは事情が違う
-- (docs/adr/0016-schema-and-indexes.md 問題 2)。
--
-- この索引は 2 つの役割を兼ねる:
--   1. 二重採番を DB が拒否する (壊れたことを検出できる)
--   2. 採番の SELECT COALESCE(MAX(seq), 0) + 1 WHERE thread_id = $1 を
--      索引の逆順スキャン 1 回で解く
--
-- 【CONCURRENTLY を使っていない理由】
-- 現時点のデータ量なら一瞬で終わる。運用が始まったあとに同じ書き方をすると
-- 構築時間まるごと読み書きが止まる (親 + 子 8 つが対象)。
-- そのときは単独のマイグレーションに切り出すこと。
--
-- 【名前を _idx で終わらせているのは偶然ではない】
-- パーティション親に張った索引への違反は、実際には子の索引で検出される。
-- SQLSTATE 23505 の CONSTRAINT NAME に入るのは
-- **comments_p5_thread_id_seq_idx のような子の名前**になる (実測)。
-- 親を comments_thread_id_seq_key と名付けると、親子で共通の接尾辞が無くなり、
-- アプリ側の「これは採番の衝突か」の判定が名前で書けなくなる。
CREATE UNIQUE INDEX comments_thread_id_seq_idx ON comments (thread_id, seq);

-- レス番号は 1 から振る。0 や負数はアプリのバグでしか入らない。
-- 既存の CHECK 制約 (comments_body_length など) と同じく「最後の砦」であり、
-- 通常の入力検証はアプリ側で済ませている。
ALTER TABLE comments ADD CONSTRAINT comments_seq_positive CHECK (seq >= 1);
