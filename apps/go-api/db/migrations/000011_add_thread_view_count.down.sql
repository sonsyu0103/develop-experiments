-- 000011 の巻き戻し。
--
-- **索引を先に落とす。** 列を DROP すれば索引も一緒に消えるが、
-- 順序を明示しておくと「索引だけ残った」状態を疑わずに済む。
--
-- 巻き戻すと閲覧数は失われる。ADR 0006 が「値がロストする」ことを
-- 織り込んだ設計なので、ここで退避はしない。
DROP INDEX IF EXISTS threads_alive_popular_idx;

ALTER TABLE threads
    DROP CONSTRAINT IF EXISTS threads_view_count_non_negative;

ALTER TABLE threads
    DROP COLUMN IF EXISTS view_count;
