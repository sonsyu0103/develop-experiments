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
    u.avatar_url,
    -- 権限判定は毎リクエスト必要になる (ADR 0011 決定 1)。
    -- ここで一緒に引かないと、削除やモデレーションのたびに
    -- users をもう一度引くことになり、1 往復に畳んだ意味が薄れる。
    u.role,
    -- アップロードしたプロフィール画像 (ADR 0007)。
    -- **LEFT であることが必須** —— INNER にすると、画像を設定していない
    -- 利用者のセッションが 1 件も引けなくなる (= 全員ログアウト)。
    --
    -- ここで一緒に引くのは、/me が毎回返す値だからになる。
    -- 別途引くと、認証つきリクエストのたびに 1 往復増える。
    img.object_key AS avatar_object_key
FROM sessions s
JOIN users u ON u.id = s.user_id
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
LEFT JOIN images img ON img.id = u.avatar_image_id
    AND img.status <> 'deleted' AND img.object_reclaimed_at IS NULL
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
-- sessions_expires_at_idx (expires_at) で引く。
--
-- 【ctid で消す理由 —— 素直な書き方はどれも全表を走査する】
--
-- 初版は `WHERE id IN (SELECT ... LIMIT $1)` だった。**件数の上限は
-- 効いていた**が (実測)、外側が絞り込みの無い全表走査になる。
-- cmd/api/main.go の drain は 1 周 1000 行を最大 100 周するので、
-- **毎時 100 回の全表走査**になる (レビュー指摘)。
--
-- 200,000 行 / バッチ 1000 で 4 通り測った (EXPLAIN ANALYZE, BUFFERS):
--
--   書き方                              プラン                    Buffers  実行
--   IN (SELECT ... LIMIT n)             Hash Semi Join + Seq Scan    4256  27.1ms
--   WITH ... DELETE ... USING           Hash Join + Seq Scan         4268  26.0ms
--   id   = ANY(ARRAY(SELECT ...))       Bitmap Heap Scan (pkey)      3619   3.1ms
--   ctid = ANY(ARRAY(SELECT ctid ...))  Tid Scan                     2052   0.7ms
--
-- **CTE + USING では直らない。** ScrubContactClientIPs と同じ形にしても、
-- プランナは結局 sessions 全体を走査して 1000 行のハッシュに突き合わせる ——
-- 「消す 1000 行を選ぶために 200,000 行を読む」構造が変わらない。
-- 実測でも 27.1ms → 26.0ms で、誤差の範囲だった。
--
-- ARRAY(SELECT ...) は InitPlan として**1 回だけ**評価される。そのため
--   - LIMIT が半結合の再評価で無効化されない
--     (contact.sql が踏んだ罠が、原理的に起きない形になる)
--   - 外側が Tid Scan になり、走査量が**バッチサイズだけ**で決まる
--
-- 【ctid を使うことで引き受けるもの】
-- ctid は物理位置なので、InitPlan の評価後にその行が UPDATE されると
-- 位置が変わり、**その行は消えずに残る。** 一掃処理としては無害で、
-- drain が次の周回で拾い直す (0 行になるまで回す形なので終端も変わらない)。
-- **消しすぎる方向には倒れない** —— 同一スナップショット内で
-- ctid が別の行を指すことは無い。
--
-- **ORDER BY は古いものから消すため。** sessions_expires_at_idx の
-- 並びと同じなので追加のコストは無い。
DELETE FROM sessions
WHERE ctid = ANY(ARRAY(
    SELECT ctid FROM sessions
    WHERE expires_at <= now()
    ORDER BY expires_at
    LIMIT sqlc.arg('max_rows')
));
