-- =============================================================================
-- 000013: 'pending' の画像を削除できるようにする
-- =============================================================================
-- **db/query/images.sql の MarkImageDeleted が、制約に阻まれて動かなかった**
-- (レビュー指摘)。
--
-- 000006 の制約はこうなっている:
--
--   CONSTRAINT images_committed_at_matches_status CHECK (
--       (status = 'pending'  AND committed_at IS NULL) OR
--       (status <> 'pending' AND committed_at IS NOT NULL)
--   )
--
-- 'pending' の行は committed_at IS NULL なので、status を 'deleted' に
-- するとどちらの枝も満たさない。実測:
--
--   ERROR:  new row for relation "images" violates check constraint
--           "images_committed_at_matches_status"
--
-- MarkImageDeleted は **'pending' も消せることを前提に書かれている。**
-- クエリのコメントは「アップロード中に通報が入る余地があり、そこで弾くと
-- 説明のつかない拒否になる」と書き、そのうえで
-- 「pending を消すと回収の猶予が外れる」競合まで分析して**残す**と決めている。
-- 制約だけがその決定に追随していなかった。
--
-- 誰も踏まなかったのは、実 DB を使う検査 (moderation_repository_live_test.go)
-- が **StatusCommitted しか作っていない**ため。'pending' の画像を消す経路は
-- CI で 1 度も通っていなかった。
--
-- 【なぜ committed_at を書かないのか】
-- UPDATE 側で committed_at = now() を入れれば制約は通るが、
-- **committed_at は「アップロードが完了した時刻」**であって、
-- 完了していない画像に入れるのは嘘になる。回収バッチも孤児の判定に使う。
--
-- そこで制約のほうを直す。状態ごとに書き下すと、意図がそのまま読める:
--
--   pending    まだ完了していない       -> committed_at IS NULL
--   committed  完了した                -> committed_at IS NOT NULL
--   deleted    どちらから来たかによる    -> 問わない
--
-- 'deleted' で問わないのは、**元が 'pending' なら NULL、'committed' なら
-- その時刻**が入っているのが正しいため。値を捨てずに残せる。
--
-- **000006 は書き換えない。** 既に適用済みの環境がありうる (000009 と同じ)。
-- =============================================================================

ALTER TABLE images DROP CONSTRAINT images_committed_at_matches_status;

ALTER TABLE images ADD CONSTRAINT images_committed_at_matches_status CHECK (
    (status = 'pending'   AND committed_at IS NULL)     OR
    (status = 'committed' AND committed_at IS NOT NULL) OR
    (status = 'deleted')
);
