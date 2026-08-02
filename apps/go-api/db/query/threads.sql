-- name: ListThreadsWithCommentCount :many
-- スレッド一覧 + コメント数を「1 クエリ」で取得する本命の実装。
--
-- goroutine でスレッドごとにコメント数を数える実装 (= N+1) と比べて、
-- ラウンドトリップが 1 回で済むぶん、ほぼ常にこちらが速い。
-- N+1 版は Phase 4 のベンチマークで比較対象として使うため
-- ListThreadIDs / CountCommentsByThreadID として別に残してある。
--
-- ページネーションは OFFSET ではなくキーセット (cursor) 方式。
-- OFFSET は「読み飛ばす行を実際に読む」ため、深いページほど線形に遅くなる。
-- id は単調増加なので id < cursor で「それより古いもの」を索引だけで辿れる。
SELECT
    t.id,
    t.title,
    t.created_at,
    -- FILTER 句は SQL 標準。MySQL には無く、SUM(CASE WHEN ...) で代用する必要がある。
    COUNT(c.id) FILTER (WHERE c.deleted_at IS NULL) AS comment_count
FROM threads t
LEFT JOIN comments c ON c.thread_id = t.id
WHERE t.deleted_at IS NULL
  AND (sqlc.narg('cursor_id')::bigint IS NULL OR t.id < sqlc.narg('cursor_id')::bigint)
-- PostgreSQL は主キーによる関数従属性を認識するので、
-- GROUP BY に t.title / t.created_at を並べる必要がない。
GROUP BY t.id
ORDER BY t.id DESC
LIMIT sqlc.arg('page_size');

-- name: GetThreadWithCommentCount :one
SELECT
    t.id,
    t.title,
    t.created_at,
    COUNT(c.id) FILTER (WHERE c.deleted_at IS NULL) AS comment_count
FROM threads t
LEFT JOIN comments c ON c.thread_id = t.id
WHERE t.id = sqlc.arg('id')
  AND t.deleted_at IS NULL
GROUP BY t.id;

-- name: CreateThread :one
-- RETURNING により INSERT と採番値の取得が 1 往復で完結する。
-- MySQL では LAST_INSERT_ID() を別クエリで叩く必要がある。
INSERT INTO threads (title)
VALUES (sqlc.arg('title'))
RETURNING id, title, created_at;

-- name: ThreadExists :one
SELECT EXISTS (
    SELECT 1 FROM threads
    WHERE id = sqlc.arg('id') AND deleted_at IS NULL
);

-- -----------------------------------------------------------------------------
-- 以下 2 つは Phase 4 のベンチマーク専用 (N+1 実装の再現用)。
-- 本番経路では使わない。
-- -----------------------------------------------------------------------------

-- name: ListThreadIDs :many
SELECT id, title, created_at
FROM threads
WHERE deleted_at IS NULL
  AND (sqlc.narg('cursor_id')::bigint IS NULL OR id < sqlc.narg('cursor_id')::bigint)
ORDER BY id DESC
LIMIT sqlc.arg('page_size');

-- name: CountCommentsByThreadID :one
SELECT COUNT(*)
FROM comments
WHERE thread_id = sqlc.arg('thread_id')
  AND deleted_at IS NULL;
