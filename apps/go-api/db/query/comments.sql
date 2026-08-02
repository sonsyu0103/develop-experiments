-- name: ListCommentsByThreadID :many
-- thread_id を等値で指定しているため、HASH パーティションの pruning が効き、
-- 8 分割中 1 パーティションだけを走査する。
-- EXPLAIN で "Partitions removed" が確認できることを README に記録している。
SELECT
    id,
    thread_id,
    author_name,
    body,
    created_at
FROM comments
WHERE thread_id = sqlc.arg('thread_id')
  AND deleted_at IS NULL
  AND (sqlc.narg('cursor_id')::bigint IS NULL OR id < sqlc.narg('cursor_id')::bigint)
ORDER BY id DESC
LIMIT sqlc.arg('page_size');

-- name: CreateComment :one
INSERT INTO comments (thread_id, author_name, body)
VALUES (sqlc.arg('thread_id'), sqlc.arg('author_name'), sqlc.arg('body'))
RETURNING id, thread_id, author_name, body, created_at;

-- name: SoftDeleteComment :execrows
UPDATE comments
SET deleted_at = now()
WHERE thread_id = sqlc.arg('thread_id')
  AND id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- name: LockThreadForUpdate :one
-- スレッド行に行ロックを取る。Phase 2 の排他制御で、
-- 「コメント投稿と同時にスレッドの集計列を更新する」ようなケースに使う。
--
-- SSI (SERIALIZABLE) を使うなら本来この明示ロックは不要だが、
-- 悲観ロック版と楽観 (SSI) 版を比較実装して、
-- スループット差を計測できるようにするために両方用意している。
SELECT id
FROM threads
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL
FOR UPDATE;
