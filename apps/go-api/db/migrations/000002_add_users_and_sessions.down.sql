-- 逆順に戻す。
--
-- author_id を先に落とすのは、users への外部キーが残っていると
-- DROP TABLE users が失敗するため。
-- 索引は列の DROP に追随して消えるので、明示的に落とす必要はない。
ALTER TABLE comments DROP COLUMN IF EXISTS author_id;
ALTER TABLE threads  DROP COLUMN IF EXISTS author_id;

-- sessions は users を参照しているので先に落とす。
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
