-- パーティションは親テーブルの DROP で一緒に消える。
DROP TABLE IF EXISTS comments;
DROP SEQUENCE IF EXISTS comments_id_seq;
DROP TABLE IF EXISTS threads;
