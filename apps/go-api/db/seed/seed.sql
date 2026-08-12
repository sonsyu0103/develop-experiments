-- 開発用のシードデータ。
-- 何度実行しても同じ状態になるよう、先に全消ししてから入れ直す。
-- 本番では絶対に実行しないこと (make seed は開発専用)。

BEGIN;

TRUNCATE TABLE comments, threads RESTART IDENTITY CASCADE;

INSERT INTO threads (title) VALUES
    ('Go の並列処理を学ぶ部屋'),
    ('PostgreSQL のパーティショニング検証'),
    ('キーセットページネーションの話'),
    ('sqlc と型安全な SQL'),
    ('コメントが 0 件のスレッド');

-- 各スレッドに、スレッド ID に応じた件数のコメントを入れる。
-- (コメント数の集計が正しく効いているかを目視で確認するため)
-- レス番号 (seq) は生成列の n をそのまま使う。
-- スレッドごとに 1..4 になり、投稿順と一致する。
INSERT INTO comments (thread_id, seq, author_name, body)
SELECT
    t.id,
    n,
    CASE WHEN n % 3 = 0 THEN 'ホシノ' ELSE '名無しさん' END,
    format('スレッド %s への %s 番目のコメント', t.id, n)
FROM threads t
CROSS JOIN generate_series(1, 4) AS n
-- 最後のスレッドだけコメント 0 件のままにする
WHERE t.title <> 'コメントが 0 件のスレッド';

-- 論理削除された行が集計から除かれることの確認用に 1 件だけ消しておく。
--
-- **レス番号は詰めない** (docs/adr/0019-comment-concurrency.md 決定 5)。
-- ここで消える 1 件はスレッド 1 の 1 番なので、
-- スレッド 1 は「2, 3, 4 番があって 1 番が欠番」という状態になる。
-- 欠番がそのまま残ることをスモークテストが検査している。
UPDATE comments
SET deleted_at = now()
WHERE id = (SELECT min(id) FROM comments);

COMMIT;
