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
-- 親スレッドが「生存している」場合にだけ挿入する。
--
-- 外部キー制約だけでは不十分である。threads は論理削除 (deleted_at) なので、
-- 削除済みスレッドでも行は残っており FK は満たされてしまう。
-- その結果「GET /threads/{id} は 404 なのにコメントは投稿できる」という
-- 矛盾が生じる。
--
-- 事前に SELECT で存在確認してから INSERT する方法は、
-- 確認と挿入の間に削除される競合 (TOCTOU) を許してしまう。
-- INSERT ... SELECT ... WHERE EXISTS なら 1 文で完結し、競合しない。
-- 挿入されなかった場合は 0 行が返るため、pgx.ErrNoRows として検出できる。
INSERT INTO comments (thread_id, author_name, body)
SELECT sqlc.arg('thread_id'), sqlc.arg('author_name'), sqlc.arg('body')
WHERE EXISTS (
    SELECT 1 FROM threads
    WHERE id = sqlc.arg('thread_id') AND deleted_at IS NULL
)
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
