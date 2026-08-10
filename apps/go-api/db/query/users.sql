-- name: UpsertUser :one
-- ログイン時に呼ぶ。google_sub で照合し、無ければ作る。
--
-- 【なぜ upsert か】
-- 「SELECT して無ければ INSERT」だと、同じ利用者が同時に 2 回ログインしたときに
-- 両方が「無い」と判定して INSERT が衝突する。1 文に閉じれば競合しない。
--
-- 【public_id を更新しない】
-- ON CONFLICT 側の SET に public_id を含めていない。
-- 既存の利用者がログインし直すたびに公開 ID が変わると、
-- 配布済みのマイページ URL が壊れる。採番は INSERT の 1 回きり。
--
-- 【退会済みの行には当たらない】
-- WHERE users.deleted_at IS NULL を付けているため、退会済みの行に
-- 衝突した場合は DO UPDATE が実行されず、0 行が返る (pgx.ErrNoRows)。
--
-- これが無いと、退会したのと同じ Google アカウントで再ログインしたときに
-- email / display_name が入り直して同一の users.id が黙って復活する。
-- 「復活させる」か「拒否する」かは未決なので、
-- ここでは安全側 (呼び出し側に判断させる) に倒している。
-- 決めたら ADR に記録すること (docs/adr/0005-authentication.md)。
INSERT INTO users (public_id, google_sub, email, display_name, avatar_url)
VALUES (
    sqlc.arg('public_id'),
    sqlc.arg('google_sub'),
    sqlc.arg('email'),
    sqlc.arg('display_name'),
    sqlc.narg('avatar_url')
)
ON CONFLICT (google_sub) DO UPDATE
SET email        = EXCLUDED.email,
    display_name = EXCLUDED.display_name,
    -- COALESCE で既存値を残す。EXCLUDED をそのまま入れると、
    -- Google が picture を返さなかった回のログインで avatar_url が NULL に潰れる。
    -- 実測: 2 回目のログインで消えることを確認済み。
    -- public_id を守っているのと同じ理由で、こちらも上書きさせない。
    avatar_url   = COALESCE(EXCLUDED.avatar_url, users.avatar_url),
    updated_at   = now()
WHERE users.deleted_at IS NULL
RETURNING id, public_id, google_sub, email, display_name, avatar_url, created_at, updated_at, deleted_at;

-- name: GetUserByID :one
-- 内部 ID での取得。外部キーからの解決に使う。
SELECT id, public_id, google_sub, email, display_name, avatar_url, created_at, updated_at, deleted_at
FROM users
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- name: GetUserByPublicID :one
-- API から来る識別子は public_id だけ (ADR 0003 未決 #11 の決定)。
-- 内部 ID を URL に出すとユーザーを列挙できるため。
SELECT id, public_id, google_sub, email, display_name, avatar_url, created_at, updated_at, deleted_at
FROM users
WHERE public_id = sqlc.arg('public_id')
  AND deleted_at IS NULL;

-- name: ListAuthorsByIDs :many
-- 投稿者の一括解決。
--
-- 一覧に載ったコメントの author_id を集めて 1 回で引く。
-- コメント 1 件ごとに users を引くと N+1 になり、
-- スレッド一覧のコメント数集計で避けたのと同じ問題が投稿者表示で再発する
-- (db/query/threads.sql の冒頭コメントを参照)。
--
-- 【表示に要る列だけを選ぶ】
-- google_sub と email を返さない。この経路の行き先は「他人にも見える投稿一覧」であり、
-- 全列を返すと、DTO の詰め替えを 1 つ間違えただけで
-- 投稿者のメールアドレスが読み手に渡る。
-- ADR 0014 の Author も PublicID / DisplayName / AvatarURL しか持たない。
--
-- 退会済みも返す。投稿は匿名化されるまで残るため、
-- 「退会済みなので表示を変える」の判断は呼び出し側が行う。
--
-- 主キー索引で完結する (ADR 0016 の users の索引一覧)。
SELECT id, public_id, display_name, avatar_url, deleted_at
FROM users
WHERE id = ANY(sqlc.arg('ids')::bigint[]);
