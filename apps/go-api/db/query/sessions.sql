-- name: CreateSession :one
-- セッション ID は Go 側で生成した暗号論的乱数を渡す。
-- DB 側で採番しないのは、連番や推測可能な値になると
-- 総当たりで他人のセッションを引けてしまうため。
INSERT INTO sessions (id, user_id, expires_at)
VALUES (sqlc.arg('id'), sqlc.arg('user_id'), sqlc.arg('expires_at'))
RETURNING id, user_id, expires_at, created_at;

-- name: GetLiveSessionWithUser :one
-- セッションの検証。**毎リクエスト通る、最も高頻度の経路**になる。
--
-- 【なぜ JOIN するか】
-- セッションを引いてから users を引くと、認証つきリクエストのたびに
-- DB へ 2 往復する。ここは 1 往復に畳む。
--
-- comments 側で LEFT JOIN を避けた理由 (db/query/comments.sql) はここには当たらない。
-- sessions.user_id は NOT NULL かつ外部キーなので INNER JOIN になり、
-- 列が NULL になりうる状況が生じないため、sqlc の NULL 推論の問題は起きない。
--
-- 【期限切れと退会をここで弾く】
-- expires_at の判定を SQL 側に置く。アプリ側で比較すると、
-- 判定を書き忘れた経路が「期限切れでも通る」穴になる。
-- 退会済み (users.deleted_at) も同じ理由でここに含める。
--
-- 期限切れの行は定期処理が消すが、消える前に引かれても通してはいけない。
-- 掃除は容量のための処理であり、認可の判定に使うものではない。
SELECT
    s.id,
    s.user_id,
    s.expires_at,
    s.created_at,
    u.public_id,
    u.email,
    u.display_name,
    u.avatar_url
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = sqlc.arg('id')
  AND s.expires_at > now()
  AND u.deleted_at IS NULL;

-- name: DeleteSession :execrows
-- ログアウト。0 行なら「既に無い」なので、呼び出し側で 404 にするかは任意。
DELETE FROM sessions WHERE id = sqlc.arg('id');

-- name: DeleteSessionsByUserID :execrows
-- 「全端末からログアウト」。パスワード変更に相当する操作や、
-- 権限剥奪の直後に呼ぶ。sessions (user_id) の索引で引く。
DELETE FROM sessions WHERE user_id = sqlc.arg('user_id');

-- name: DeleteExpiredSessions :execrows
-- 期限切れの削除。定期処理から呼ぶ (ADR 0003 未決 #9: どのプロセスで動かすかは未決)。
--
-- 一度に消す件数を制限しているのは、放置後に大量の行がたまった場合でも
-- 1 トランザクションを短く保つため。長時間のロックと WAL の急増を避ける。
-- 呼び出し側は「0 行になるまで繰り返す」形で使う。
--
-- sessions (expires_at) の索引で引く。
DELETE FROM sessions
WHERE id IN (
    SELECT id FROM sessions
    WHERE expires_at <= now()
    LIMIT sqlc.arg('max_rows')
);
