-- 000006 の定義へ戻す。
--
-- **戻すと 'pending' から 'deleted' になった行が制約に違反する。**
-- そのため先に committed_at を埋める —— 巻き戻し先の制約が
-- 「'pending' 以外は committed_at が要る」と言っている以上、
-- 何か入れるしかない。deleted_at 相当の情報は持っていないので
-- created_at を使う (回収バッチは status で拾うため実害は無い)。
UPDATE images
SET committed_at = created_at
WHERE status = 'deleted' AND committed_at IS NULL;

ALTER TABLE images DROP CONSTRAINT IF EXISTS images_committed_at_matches_status;

ALTER TABLE images ADD CONSTRAINT images_committed_at_matches_status CHECK (
    (status = 'pending'  AND committed_at IS NULL) OR
    (status <> 'pending' AND committed_at IS NOT NULL)
);
