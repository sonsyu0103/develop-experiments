-- 000008 の巻き戻し。
--
-- どちらのテーブルも他から参照されていない (両方とも target_id に
-- 外部キーを張れないため、参照は常にこちらから外へ向く)。
-- 索引と制約はテーブルに依存するので DROP TABLE がまとめて落とす。
DROP TABLE IF EXISTS reports;
DROP TABLE IF EXISTS moderation_actions;
