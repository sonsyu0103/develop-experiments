-- 000009 の巻き戻し。
--
-- **索引を先に戻す。** 000008 の述語は target_thread_id を参照しないので、
-- 列を落とす前でも作れる。順序を逆にすると、列に依存しない索引まで
-- DROP COLUMN の巻き添えを考える必要が出る (000007 の down と同じ理由)。
DROP INDEX reports_open_idx;

CREATE INDEX reports_open_idx ON reports (created_at) WHERE status = 'open';

ALTER TABLE reports DROP CONSTRAINT IF EXISTS reports_thread_id_matches_target;
ALTER TABLE reports DROP COLUMN IF EXISTS target_thread_id;
