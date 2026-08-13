-- 000007 の巻き戻し。
--
-- **索引を先に戻す。** 000006 の述語は attached_at を参照しないので、
-- 列を落とす前でも作れる。順序を逆にすると、列に依存した索引を
-- DROP COLUMN が巻き添えで落としたあと、同名の索引を作り直すことになる
-- (結果は同じだが、失敗したときにどちらの状態か分からなくなる)。
DROP INDEX images_reclaimable_idx;

CREATE INDEX images_reclaimable_idx
    ON images (created_at)
    WHERE status IN ('pending', 'deleted')
      AND object_reclaimed_at IS NULL;

ALTER TABLE images DROP CONSTRAINT IF EXISTS images_attached_at_requires_commit;
ALTER TABLE images DROP COLUMN IF EXISTS attached_at;
