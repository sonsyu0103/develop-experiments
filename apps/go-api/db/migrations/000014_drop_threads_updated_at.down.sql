-- 000001 の定義へ戻す。
--
-- **値は戻らない。** 消した時点の値は失われているので、created_at で埋める。
-- 000014 が消す前も値は常に created_at と同じだった (更新する文が無い) ため、
-- 結果として元の状態と一致する。
ALTER TABLE threads ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

UPDATE threads SET updated_at = created_at;
