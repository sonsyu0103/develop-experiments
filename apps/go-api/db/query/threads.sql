-- name: ListThreadsWithCommentCount :many
-- スレッド一覧 + コメント数を「1 往復」で取得する。
--
-- 【なぜ LEFT JOIN + GROUP BY にしないか】
-- 素直に書くと以下になるが、これは 20 件返すために
-- 20 スレッド分のコメント実データ (数千行) をヒープから読んでしまう。
--
--   SELECT t.*, COUNT(c.id) FILTER (WHERE c.deleted_at IS NULL)
--   FROM threads t LEFT JOIN comments c ON c.thread_id = t.id
--   GROUP BY t.id ORDER BY t.id DESC LIMIT 20
--
-- 先に LIMIT でスレッドを 20 件へ絞り、その各行に対して
-- 相関サブクエリで数えると、部分インデックス
-- comments_alive_thread_id_desc_idx だけで完結しヒープにほぼ触れない。
--
-- 2,005 スレッド / 200,016 コメントでの実測 (EXPLAIN ANALYZE、各 3 回):
--
--   LEFT JOIN + GROUP BY : 5.84 - 7.43 ms / shared buffers 2,054
--   この実装             : 1.04 - 1.11 ms / shared buffers    59
--
-- 約 5.5 倍速く、バッファ読み取りは 35 分の 1。結果は完全に一致する
-- (両者を FULL JOIN して差分 0 件を確認済み)。
-- どちらも DB への往復は 1 回なので、N+1 実装との対比は変わらない。
--
-- 【注意】この差は Index Only Scan が効くことに依存する。
-- バルク INSERT 直後は visibility map が未整備で Heap Fetches が発生し、
-- 一時的に旧実装と同程度まで劣化する。VACUUM (autovacuum を含む) の後に
-- Heap Fetches: 0 となって本来の性能が出る。
-- ベンチマーク (Phase 4) では VACUUM 後に計測すること。
--
-- ページネーションは OFFSET ではなくキーセット (cursor) 方式。
-- OFFSET は「読み飛ばす行を実際に読む」ため、深いページほど線形に遅くなる。
--
-- 【投稿者は LEFT JOIN で解決する】
-- ADR 0014 の選択肢 A。コメント数の集約とは事情が違い、
-- users.id は主キーなので 1:1 の参照になる。
--
-- **LEFT であることが必須。** INNER にすると author_id IS NULL の
-- 匿名投稿が一覧から丸ごと消える (ADR 0005 決定 2)。
--
-- JOIN は page で 20 件に絞ったあとに掛ける。
-- 内側の CTE に混ぜると、絞り込む前の全行に対して結合が走る。
WITH page AS (
    SELECT id, title, created_at, author_id
    FROM threads
    WHERE deleted_at IS NULL
      AND (sqlc.narg('cursor_id')::bigint IS NULL OR id < sqlc.narg('cursor_id')::bigint)
    ORDER BY id DESC
    LIMIT sqlc.arg('page_size')
)
SELECT
    p.id,
    p.title,
    p.created_at,
    (
        SELECT count(*)
        FROM comments c
        WHERE c.thread_id = p.id
          AND c.deleted_at IS NULL
    )::bigint AS comment_count,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at
FROM page p
LEFT JOIN users u ON u.id = p.author_id
ORDER BY p.id DESC;

-- name: GetThreadWithCommentCount :one
-- 一覧と同じ理由で、JOIN + GROUP BY ではなく相関サブクエリで数える。
SELECT
    t.id,
    t.title,
    t.created_at,
    (
        SELECT count(*)
        FROM comments c
        WHERE c.thread_id = t.id
          AND c.deleted_at IS NULL
    )::bigint AS comment_count,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at
FROM threads t
LEFT JOIN users u ON u.id = t.author_id
WHERE t.id = sqlc.arg('id')
  AND t.deleted_at IS NULL;

-- name: CreateThread :one
-- RETURNING により INSERT と採番値の取得が 1 往復で完結する。
-- MySQL では LAST_INSERT_ID() を別クエリで叩く必要がある。
--
-- 【なぜ CTE で LEFT JOIN まで済ませるか】
-- RETURNING は挿入した行しか返せず、users を結合できない。
-- 投稿直後のレスポンスにも投稿者を載せる必要があるため、
-- ここで引かないと「作成時だけ author が null」という不整合になる。
-- 呼び出し側で組み立てる手もあるが、一覧・詳細と組み立て方が 2 通りになる。
--
-- author_id は NULL 許容。NULL が匿名を意味する (ADR 0005 決定 2)。
WITH inserted AS (
    INSERT INTO threads (title, author_id)
    VALUES (sqlc.arg('title'), sqlc.narg('author_id'))
    RETURNING id, title, created_at, author_id
)
SELECT
    i.id,
    i.title,
    i.created_at,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at
FROM inserted i
LEFT JOIN users u ON u.id = i.author_id;

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
