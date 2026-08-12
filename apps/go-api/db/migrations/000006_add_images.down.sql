-- 000006 の巻き戻し。
--
-- **列を先に落とす。** images を参照している間は DROP TABLE が
-- 外部キー違反で失敗する。CASCADE で押し切ることもできるが、
-- それは「何が消えるか」を巻き戻し側が把握していないという意味になる。
--
-- 列に付けた索引は DROP COLUMN がまとめて落とす。
-- comments は親から落とせば 8 パーティションに伝播する。
ALTER TABLE threads  DROP COLUMN IF EXISTS icon_image_id;
ALTER TABLE users    DROP COLUMN IF EXISTS avatar_image_id;
ALTER TABLE comments DROP COLUMN IF EXISTS image_id;

-- images 側の索引と制約はテーブルに依存するため、DROP TABLE が落とす。
DROP TABLE IF EXISTS images;
