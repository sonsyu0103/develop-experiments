-- =============================================================================
-- 画像 (docs/adr/0007-image-storage.md)
--
-- 【列の順番をテーブルと揃えること】
-- users.sql と同じ理由。SELECT / RETURNING の並びがテーブルの全列と
-- 過不足なく一致していれば、sqlc は共通の行型 (sqlcgen.Image) を再利用する。
-- ずれるとクエリごとに別の行型が生成され、詰め替えが増える。
--
-- 【回収バッチのクエリはここに無い】
-- Phase 6 の後半 (プロフィール画像 / スレッドアイコンと同時) で足す。
-- 先に書くと「実装していないクエリ」が残る。
-- =============================================================================

-- name: CreatePendingImage :one
-- ストレージへ書く前に呼ぶ (ADR 0007 決定 3 の手順 1)。
--
-- **status は 'pending' 固定にする。** 引数にすると、呼び出し側が
-- 誤って 'committed' を渡した瞬間に「ストレージに無い画像が確定済み」に
-- なりうる。手順 3 の UPDATE だけが確定させる経路であるべき。
--
-- committed_at を渡さないのは CHECK 制約 (images_committed_at_matches_status)
-- がそれを要求するため。
INSERT INTO images (
    id, owner_id, kind, object_key, content_type, width, height, byte_size, status
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending')
RETURNING id, owner_id, kind, object_key, content_type, width, height, byte_size, status, created_at, committed_at;

-- name: CommitImage :one
-- ストレージへの PUT が成功したあとに呼ぶ (ADR 0007 決定 3 の手順 3)。
--
-- **status = 'pending' を条件に含める。** 含めないと、
-- モデレーターが削除した画像 ('deleted') を再確定させてしまう経路ができる。
-- 遅れて届いた確定要求が削除を取り消す形になり、
-- 「消したはずの画像が S3 に残り続ける」ことになりうる。
--
-- 0 行になった場合は :one なので sql.ErrNoRows 相当が返る。
-- 呼び出し側はこれを「確定できなかった」として扱う。
UPDATE images
SET status = 'committed',
    committed_at = now()
WHERE id = $1
  AND status = 'pending'
RETURNING id, owner_id, kind, object_key, content_type, width, height, byte_size, status, created_at, committed_at;

-- name: GetImageByID :one
-- 1 件取得。
--
-- **status で絞らない。** 'pending' も 'deleted' も返す。
-- 呼び出し側が「確定していない」「削除された」を区別する必要があり、
-- ここで隠すと「元から存在しない」と同じに見えてしまう
-- (ADR 0016 問題 3 が DB 行を残す理由と同じ話)。
SELECT id, owner_id, kind, object_key, content_type, width, height, byte_size, status, created_at, committed_at
FROM images
WHERE id = $1;
