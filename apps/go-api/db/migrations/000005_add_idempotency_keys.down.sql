-- 000005 の巻き戻し。
-- 索引と制約はテーブルに依存するため、DROP TABLE がまとめて落とす。
DROP TABLE IF EXISTS idempotency_keys;
