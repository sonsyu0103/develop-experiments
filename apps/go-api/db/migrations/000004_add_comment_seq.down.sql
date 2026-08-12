-- 000004 の巻き戻し。
--
-- 索引と CHECK 制約は列に依存するため、DROP COLUMN が両方まとめて落とす。
-- 明示的に消しているのは、何が作られたかをこのファイルだけで読めるようにするため。
ALTER TABLE comments DROP CONSTRAINT IF EXISTS comments_seq_positive;
DROP INDEX IF EXISTS comments_thread_id_seq_idx;
ALTER TABLE comments DROP COLUMN IF EXISTS seq;
