-- 冪等キー (docs/adr/0015-idempotency.md)。
--
-- **ここのクエリはすべて主トランザクションの中で実行される** (ADR 0015 決定 3)。
-- 別トランザクションにすると「キーを記録した直後に処理が失敗」したときに、
-- リトライしても「処理済み」と誤判定されて投稿が永久に失われる。

-- name: ClaimIdempotencyKey :one
-- キーを確保する。先着なら行を返し、既に取られていれば 0 行を返す。
--
-- 【ON CONFLICT DO NOTHING が「待つ」ことに意味がある】
-- 競合する行が**未コミット**のとき、この文は相手のトランザクションが
-- 終わるまで待つ。待った結果:
--   コミットされていた → 0 行が返る → 呼び出し側が既存行を読んで応答を再生する
--   巻き戻っていた     → こちらの INSERT が通る → 通常どおり処理する
-- どちらでも正しい結果になる。
--
-- **待ち時間はコネクションを占有する。** 呼び出し側で文のタイムアウトを
-- 設けて、超えたら 409 を返す。
--
-- 応答 (response_status / response_body) はこの時点では NULL。
-- 処理が終わってから CompleteIdempotencyKey で埋める。
INSERT INTO idempotency_keys (user_id, key, endpoint, request_hash)
VALUES (
    sqlc.arg('user_id'),
    sqlc.arg('key'),
    sqlc.arg('endpoint'),
    sqlc.arg('request_hash')
)
ON CONFLICT DO NOTHING
RETURNING user_id, key;

-- name: GetIdempotencyKey :one
-- 確保できなかったときに、既存の記録を読む。
--
-- request_hash が違えば「同じキーで別の内容」なので 422 にする。
-- 一致すれば、記録した応答をそのまま返す。
SELECT user_id, key, endpoint, request_hash, response_status, response_body, created_at, completed_at
FROM idempotency_keys
WHERE user_id = sqlc.arg('user_id')
  AND key = sqlc.arg('key');

-- name: CompleteIdempotencyKey :execrows
-- 処理の結果を記録する。**主トランザクションの中で呼ぶこと。**
--
-- ここまでが 1 つのトランザクションなので、
-- 「キーは記録されたが投稿されていない」は原理的に起きない。
-- 直列化失敗でリトライされた場合も、キーの記録ごと巻き戻って
-- もう一度 INSERT される。それが正しい挙動になる。
UPDATE idempotency_keys
SET response_status = sqlc.arg('response_status'),
    response_body   = sqlc.arg('response_body'),
    completed_at    = now()
WHERE user_id = sqlc.arg('user_id')
  AND key = sqlc.arg('key');

-- name: DeleteExpiredIdempotencyKeys :execrows
-- 保持期間を過ぎたキーの削除。定期処理から呼ぶ
-- (ADR 0003 未決 #9: どのプロセスで動かすかは未決。現時点で未配線)。
--
-- 消さないと単調増加する。保持 24 時間は「クライアントが
-- リトライを諦めるまでの時間」として十分という判断 (ADR 0015)。
--
-- 一度に消す件数を制限しているのは sessions と同じ理由。
-- 呼び出し側は「0 行になるまで繰り返す」形で使う。
--
-- idempotency_keys (created_at) の索引で引く。
DELETE FROM idempotency_keys
WHERE (user_id, key) IN (
    SELECT user_id, key FROM idempotency_keys
    WHERE created_at <= now() - sqlc.arg('retention')::interval
    LIMIT sqlc.arg('max_rows')
);
