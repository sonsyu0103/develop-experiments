-- =============================================================================
-- 【列の順番をテーブルと揃えること】
--
-- role は ALTER TABLE ADD COLUMN で足したため、**テーブルでは末尾**にある。
-- SELECT / RETURNING の並びをテーブルと一致させると、sqlc は
-- 共通の行型 (sqlcgen.User) を再利用する。
--
-- ずらすとクエリごとに別の行型 (GetUserByIDRow / UpsertUserRow ...) が生成され、
-- ドメインへの詰め替えが 1 か所から 4 か所に増える。
-- 実際に一度そうなった (role を created_at の前に置いていた)。
--
-- 【並び替えだけでなく、列を足したときにも壊れる】
-- 000006 で avatar_image_id を足したとき、ここを直さなかったため
-- **一致が崩れて行型が 4 つに分かれた** (ビルドが落ちて気づいた)。
-- 揃える条件は「順番が同じ」ではなく「テーブルの全列と過不足なく一致する」。
--
-- そのため、まだ読み出さない列もここに並べる必要がある。
-- avatar_image_id を実際に使うのは Phase 6 の後半 (プロフィール画像) になる。
-- =============================================================================

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
    -- **role は絶対に更新しない。** INSERT 側の列にも含めていない。
    -- クライアント由来の値がロールに触れる経路を 1 か所でも作ると、
    -- そこが権限昇格の入口になる (ADR 0011 決定 1「権限昇格を作り込まない」)。
    -- 新規は DEFAULT 'user'、既存はログインしても変わらない。
    updated_at   = now()
WHERE users.deleted_at IS NULL
RETURNING id, public_id, google_sub, email, display_name, avatar_url, created_at, updated_at, deleted_at, role, avatar_image_id;

-- name: GetUserByID :one
-- 内部 ID での取得。外部キーからの解決に使う。
SELECT id, public_id, google_sub, email, display_name, avatar_url, created_at, updated_at, deleted_at, role, avatar_image_id
FROM users
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- name: GetUserByPublicID :one
-- API から来る識別子は public_id だけ (ADR 0003 未決 #11 の決定)。
-- 内部 ID を URL に出すとユーザーを列挙できるため。
SELECT id, public_id, google_sub, email, display_name, avatar_url, created_at, updated_at, deleted_at, role, avatar_image_id
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

-- name: PromoteToAdmin :one
-- 最初の管理者を作る唯一の経路 (ADR 0011 決定 1「最初の管理者をどう作るか」)。
--
-- **UI からは作れない。** 「最初の 1 人」を作る機能は、そのまま
-- 「誰でも管理者になれる」機能になりうるため。
-- 環境変数 BOOTSTRAP_ADMIN_GOOGLE_SUB に一致する利用者がログインしたときだけ、
-- アプリがこれを呼ぶ。
--
-- google_sub で指定するのは、内部 ID も public_id も
-- 「先に一度ログインしてもらわないと分からない」ため。
-- Google の sub なら、アカウントが決まった時点で確定する。
--
-- 既に admin なら更新しない (role <> 'admin')。
-- 毎ログインで UPDATE を撃つと、更新日時だけが動いて監査の邪魔になる。
-- 該当が無ければ 0 行が返るので、呼び出し側は「昇格したか」を判定できる。
UPDATE users
SET role = 'admin', updated_at = now()
WHERE google_sub = sqlc.arg('google_sub')
  AND deleted_at IS NULL
  AND role <> 'admin'
RETURNING id, public_id, google_sub, email, display_name, avatar_url, created_at, updated_at, deleted_at, role, avatar_image_id;

-- name: SetUserAvatarImage :one
-- プロフィール画像を設定する / 外す (ADR 0007)。
--
-- **所有者の確認はここで行わない。** 画像が自分のものかは
-- ユースケース層が先に確かめる (他人の画像は 404 にする必要があり、
-- ここで弾くと外部キー違反として 400 になってしまう)。
--
-- NULL を渡すと解除になり、Google のプロフィール画像に戻る。
--
-- 【添付先 3 つのうち、ここだけ「解除」がある】
-- コメントとスレッドは作成時にしか画像を指定できないので、
-- 一度 attached_at を書いたら戻すことはない。アバターは付け替えられる。
-- **旧画像の attached_at を NULL に戻さないと、差し替えた画像が
-- どこからも参照されないまま永久に回収されない** (000007)。
--
-- previous は主文と同じ条件 (id と deleted_at IS NULL) で引く。
-- 退会済みの利用者を指定した場合、主文は 0 行になるので
-- **画像側も何も触らない**必要がある —— previous が空になることで揃う。
--
-- **列は表名で修飾する。** この 1 文には users と images の 2 つが登場し、
-- どちらにも id 列がある。修飾しないと sqlc の解析が
-- "column reference \"id\" is ambiguous" で止まる (実測)。
WITH previous AS (
    SELECT users.avatar_image_id AS image_id
    FROM users
    WHERE users.id = sqlc.arg('id')
      AND users.deleted_at IS NULL
), detached AS (
    -- 旧画像を未添付に戻す。同じ画像を指定し直した場合は触らない
    -- (IS DISTINCT FROM は NULL 同士も「同じ」と扱うので、
    --  解除の解除で余計な更新が走らない)。
    UPDATE images
    SET attached_at = NULL
    WHERE images.id = (SELECT previous.image_id FROM previous)
      AND images.id IS DISTINCT FROM sqlc.narg('avatar_image_id')
), attached AS (
    UPDATE images
    SET attached_at = now()
    WHERE images.id = sqlc.narg('avatar_image_id')
      AND images.attached_at IS NULL
      -- **確保済みの画像は添付済みにしない** (レビュー指摘)。
      -- EnsureOwned はロックを取らない読み取りなので、
      -- 「確認したあと・書く前」に回収バッチが確保する窓がある。
      -- この UPDATE は images の行ロックで待たされてから最新版を読むため、
      -- ここに条件を置くと確保を追い越せない。
      AND images.object_reclaimed_at IS NULL
      AND EXISTS (SELECT 1 FROM previous)
)
UPDATE users
SET avatar_image_id = sqlc.narg('avatar_image_id'),
    updated_at = now()
WHERE users.id = sqlc.arg('id')
  AND users.deleted_at IS NULL
RETURNING users.id, users.public_id, users.google_sub, users.email, users.display_name, users.avatar_url, users.created_at, users.updated_at, users.deleted_at, users.role, users.avatar_image_id;

-- name: ChangeUserRole :one
-- 利用者のロールを変更する (ADR 0011 決定 1)。
--
-- **呼べるのは admin だけ**だが、その判定はここではなくユースケース側にある
-- (SQL は「誰が呼んだか」を知らない)。
--
-- 【PromoteToAdmin と分けている理由】
-- あちらは google_sub を鍵にした「最初の 1 人」専用の経路で、
-- **UI から到達できないことに意味がある。**
-- こちらは public_id を鍵にした通常の管理操作になる。
-- 1 つにまとめると、UI から google_sub を指定する形が生まれうる。
--
-- 【role <> 'admin' のような条件は付けない】
-- PromoteToAdmin は「毎ログインで撃たれる」ので冪等性のために絞っているが、
-- こちらは明示的な操作なので、同じロールへの変更も 1 行として扱う。
-- **記録には残る** —— 「変えようとした」ことも監査の対象になる。
--
-- 退会済み (deleted_at IS NOT NULL) は対象外。
-- 0 行なら「居ない、または退会済み」で、呼び出し側は 404 にする。
UPDATE users
SET role = sqlc.arg('role'), updated_at = now()
WHERE public_id = sqlc.arg('public_id')
  AND deleted_at IS NULL
RETURNING id, public_id, google_sub, email, display_name, avatar_url, created_at, updated_at, deleted_at, role, avatar_image_id;
