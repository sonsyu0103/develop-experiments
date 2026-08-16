-- 000012 の巻き戻し。
--
-- **索引を明示的に落とす。** テーブルごと消えるので不要だが、
-- 順序を書いておくと「索引だけ残った」状態を疑わずに済む
-- (000011 と同じ書き方)。
--
-- **巻き戻すと未送信の問い合わせが消える。** ADR 0008 決定 1 が
-- 「保存が成功した時点で内容は失われない」としてこのテーブルを置いた以上、
-- ここで消える内容がそのまま失われる問い合わせになる。
-- 本番で巻き戻す場合は、先に status = 'pending' の行を退避すること。
DROP INDEX IF EXISTS contact_ip_scrub_idx;
DROP INDEX IF EXISTS contact_user_idx;
DROP INDEX IF EXISTS contact_rate_limit_idx;
DROP INDEX IF EXISTS contact_pending_idx;

DROP TABLE IF EXISTS contact_messages;
