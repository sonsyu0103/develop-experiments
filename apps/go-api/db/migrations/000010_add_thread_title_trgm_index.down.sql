-- 000010 の巻き戻し。
--
-- **拡張は落とさない。** 作ったのがこのマイグレーションではない
-- (db/init/00-extensions.sql の担当) ため、ここで DROP EXTENSION すると
-- 「作っていないものを消す」ことになる。
-- pg_stat_statements と同じ扱いで、拡張の寿命はデータベースに合わせる。
DROP INDEX IF EXISTS threads_title_trgm_idx;
