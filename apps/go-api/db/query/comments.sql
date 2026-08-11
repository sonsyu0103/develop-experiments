-- name: ListCommentsByThreadID :many
-- thread_id を等値で指定しているため、HASH パーティションの pruning が効き、
-- 8 分割中 1 パーティションだけを走査する。
--
-- 【なぜスレッドの存在確認を同じクエリに含めないか】
-- 「存在しないスレッド」と「コメント 0 件のスレッド」を 1 クエリで
-- 区別するには、threads を駆動表にした LEFT JOIN LATERAL が必要になる。
-- しかしその形ではコメント側の列が NULL になりうるのに対し、
-- sqlc は LEFT JOIN の NULL 許容を推論できず int64 / string を生成する
-- (実測: コメント 0 件のスレッドで comment_id と author_name が NULL 返却)。
-- 結果として Scan が実行時に失敗する。
--
-- 往復 1 回を節約するために実行時エラーの危険を持ち込むのは割に合わないため、
-- 存在確認は呼び出し側 (CommentInteractor) の別クエリに分けている。
-- どちらも主キー / 部分インデックスで完結する軽いクエリである。
--
-- 【投稿者は LEFT JOIN で解決する (ADR 0014 の選択肢 A)】
-- **LEFT であることが必須。** INNER にすると匿名コメントが消える。
--
-- ADR 0014 は「コメント一覧では選択肢 B (一括取得) が勝つ可能性がある」と
-- 書いている。同じ人が連投すると、同じ投稿者の情報が最大 100 行ぶん
-- 重複して転送されるため。初手は A で揃え、B (ListAuthorsByIDs) との
-- 比較は Phase 4 のベンチマークで行う。
WITH page AS (
    SELECT id, thread_id, author_name, body, created_at, author_id
    FROM comments
    WHERE thread_id = sqlc.arg('thread_id')
      AND deleted_at IS NULL
      AND (sqlc.narg('cursor_id')::bigint IS NULL OR id < sqlc.narg('cursor_id')::bigint)
    ORDER BY id DESC
    LIMIT sqlc.arg('page_size')
)
SELECT
    p.id,
    p.thread_id,
    p.author_name,
    p.body,
    p.created_at,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at
FROM page p
LEFT JOIN users u ON u.id = p.author_id
ORDER BY p.id DESC;

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
--
-- 投稿者の解決も 1 往復に含める。RETURNING は挿入行しか返せないので
-- CTE で包んで LEFT JOIN する (threads.sql の CreateThread と同じ理由)。
-- 親スレッドが無ければ inserted が 0 行になり、外側も 0 行になるため、
-- pgx.ErrNoRows での検出はそのまま効く。
WITH inserted AS (
    INSERT INTO comments (thread_id, author_name, body, author_id)
    SELECT
        sqlc.arg('thread_id'),
        sqlc.arg('author_name'),
        sqlc.arg('body'),
        sqlc.narg('author_id')
    WHERE EXISTS (
        SELECT 1 FROM threads
        WHERE id = sqlc.arg('thread_id') AND deleted_at IS NULL
    )
    RETURNING id, thread_id, author_name, body, created_at, author_id
)
SELECT
    i.id,
    i.thread_id,
    i.author_name,
    i.body,
    i.created_at,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at
FROM inserted i
LEFT JOIN users u ON u.id = i.author_id;

-- name: SoftDeleteComment :execrows
-- 現時点で HTTP エンドポイントからは呼ばれていない。
-- 削除 API を公開するかは未決 (docs/adr/0003-open-questions.md 項目 7)。
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
