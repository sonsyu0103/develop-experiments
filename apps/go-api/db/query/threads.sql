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
--   相関サブクエリ       : 1.04 - 1.11 ms / shared buffers    59
--
-- 約 5.5 倍速く、バッファ読み取りは 35 分の 1。結果は完全に一致する
-- (両者を FULL JOIN して差分 0 件を確認済み)。
-- どちらも DB への往復は 1 回なので、N+1 実装との対比は変わらない。
--
-- 【重要】**この測定は LEFT JOIN users を足す前のもの。**
-- 投稿者の解決を加えた現在の形では測り直していない
-- (Phase 4 のデータセットが要る。開発環境は 7 スレッド / users 0 件で、
-- この規模では何を測っても意味が無い)。
-- 上の数値を「投稿者の解決を含めたコスト」として読まないこと。
-- Phase 4 で測り直し、この節を更新する。
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
    SELECT id, title, created_at, author_id, icon_image_id
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
    u.deleted_at   AS author_deleted_at,
    img.id         AS icon_id,
    img.object_key AS icon_object_key,
    img.width      AS icon_width,
    img.height     AS icon_height
FROM page p
LEFT JOIN users u ON u.id = p.author_id
-- **実体が無い画像は結合しない** (レビュー指摘)。
--   status = 'deleted'          モデレーターが消した。回収バッチが S3 から実体を消す
--   object_reclaimed_at IS NOT NULL  回収済み。実体はもう無い
-- どちらも URL を返すと、ブラウザには壊れた画像が出る。
-- **条件は ON に置くこと。** WHERE に置くと LEFT が INNER に化けて、
-- 画像なしの行 (大多数) が消える。
--
-- ADR 0016 問題 3 は「画像は削除されました」と「元から画像なし」を
-- 区別するために行を残すと決めているが、**その区別を表す API の項目がまだ無い。**
-- Phase 10 後半で削除の UI を入れるとき、ここを status の受け渡しに変える。
LEFT JOIN images img ON img.id = p.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL
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
    u.deleted_at   AS author_deleted_at,
    img.id         AS icon_id,
    img.object_key AS icon_object_key,
    img.width      AS icon_width,
    img.height     AS icon_height
FROM threads t
LEFT JOIN users u ON u.id = t.author_id
LEFT JOIN images img ON img.id = t.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL
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
    INSERT INTO threads (title, author_id, icon_image_id)
    VALUES (sqlc.arg('title'), sqlc.narg('author_id'), sqlc.narg('icon_image_id'))
    RETURNING id, title, created_at, author_id, icon_image_id
), attached AS (
    -- アイコンを「添付済み」にする (000007)。理由は comments.sql と同じ。
    -- スレッドのアイコンは作成時にしか指定できないので、解除の経路は無い。
    UPDATE images
    SET attached_at = now()
    WHERE id = (SELECT icon_image_id FROM inserted)
      AND attached_at IS NULL
      -- **確保済みの画像は添付済みにしない** (レビュー指摘)。
      -- EnsureOwned はロックを取らない読み取りなので、
      -- 「確認したあと・書く前」に回収バッチが確保する窓がある。
      -- この UPDATE は images の行ロックで待たされてから最新版を読むため、
      -- ここに条件を置くと確保を追い越せない。
      AND object_reclaimed_at IS NULL
)
SELECT
    i.id,
    i.title,
    i.created_at,
    u.public_id    AS author_public_id,
    u.display_name AS author_display_name,
    u.avatar_url   AS author_avatar_url,
    u.deleted_at   AS author_deleted_at,
    img.id         AS icon_id,
    img.object_key AS icon_object_key,
    img.width      AS icon_width,
    img.height     AS icon_height
FROM inserted i
LEFT JOIN users u ON u.id = i.author_id
LEFT JOIN images img ON img.id = i.icon_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL;

-- name: ThreadExists :one
SELECT EXISTS (
    SELECT 1 FROM threads
    WHERE id = sqlc.arg('id') AND deleted_at IS NULL
);

-- name: SoftDeleteOwnThread :execrows
-- 投稿者が自分のスレッドを論理削除する (ADR 0005 の権限モデル / ADR 0003 未決 #7)。
--
-- **消えるのは「生きている自分のスレッド」だけ。** 3 つの条件が揃わないと
-- 0 行になる。0 行だった理由 (無い / 他人のもの / 匿名) は
-- ThreadOwnership が別に答える。
--
-- 【匿名投稿は当たらない】
-- author_id が NULL のとき `author_id = $2` は NULL (真ではない) になるので、
-- **明示的な IS NOT NULL は要らない。** 三値論理がそのまま
-- 「本人であることを示せない」を表している。
-- 匿名投稿を消せるのはモデレーターだけ (ADR 0011 決定 2)。
--
-- 【コメントも添付画像も消さない】
-- スレッドが論理削除されると一覧・取得のどちらからも消えるため、
-- コメントを個別に消して回る必要がない (8 パーティションすべてに
-- UPDATE を投げることにもなる)。画像を実際に消すのはモデレーションの操作
-- (ADR 0011 決定 5) で、自分の投稿を消しただけで実体まで消すと
-- 誤操作の巻き戻しができなくなる。
UPDATE threads
SET deleted_at = now()
WHERE id = sqlc.arg('id')
  AND author_id = sqlc.arg('actor_id')
  AND deleted_at IS NULL;

-- name: ThreadOwnership :one
-- 削除が 0 行だったときに、その理由を答える。
--
-- **成功したときには引かない。** 呼ぶのは失敗の分類のためだけで、
-- 常に 2 往復させるためではない。
--
-- 行が返らなければ「無い、または既に消えている」= 404。
-- 返って owned = false なら「他人のもの、または匿名」= 403。
--
-- 【1 文にまとめなかった理由】
-- 判定と更新を CTE 1 本に畳む形も書けるが、**sqlc の解析器が
-- CTE と更新対象テーブルのスコープを混ぜてしまい、
-- author_id を ambiguous として生成に失敗する** (PostgreSQL は通る)。
-- 生成器に通らない形を無理に維持するより、失敗経路でだけ
-- もう 1 往復するほうが読みやすい。
--
-- 分けたことで「消せなかった理由」の判定には競合の余地が残るが、
-- **削除そのものは 1 文で閉じている**ので、二重削除や
-- 他人の投稿が消えることは起きない。ずれても
-- 403 と 404 を取り違えるだけになる。
-- **::boolean が要る。** 付けないと sqlc が型を推論できず、
-- 生成される戻り値が interface{} になる (実測)。
SELECT COALESCE(author_id = sqlc.arg('actor_id'), false)::boolean AS owned
FROM threads
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- name: SoftDeleteThread :execrows
-- スレッドを論理削除する (ADR 0011 決定 2)。
--
-- **deleted_at IS NULL を条件に含める。** 含めないと 2 回目以降も
-- 1 行を返し、呼び出し側が「今回消した」と判断してしまう。
-- モデレーション記録は「実際に起きた変化」に対してだけ書きたい。
-- 副次的に、消した時刻が後から呼ばれた削除で上書きされるのも防ぐ。
--
-- **コメントは消さない。** スレッドが論理削除されると一覧・取得の
-- どちらからも消えるため、コメントを個別に消して回る必要がない。
-- 8 パーティションすべてに UPDATE を投げることにもなる。
--
-- **添付画像も消さない。** アイコンやコメントの画像を
-- ストレージから消すかはモデレーターの別の判断であり
-- (スレッドの主題が不適切でも画像は問題ないことがある)、
-- delete_image として個別に記録されるべきものになる。
UPDATE threads
SET deleted_at = now()
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;

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
