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
-- idempotency_keys_created_idx (created_at) で引く。
--
-- 【ctid で消す理由】
-- DeleteExpiredSessions と同じ形に揃えてある (実測の表はそちらにある)。
--
-- **ただし、こちらは sessions ほど悪くなかった。**
-- 200,000 行 / バッチ 1000 での実測:
--
--   書き方                              プラン                        Buffers  実行
--   (user_id, key) IN (SELECT ... LIMIT) Nested Loop + pkey 索引引き      4036  1.7ms
--   ctid = ANY(ARRAY(SELECT ctid ...))   Tid Scan                        2020  0.7ms
--
-- 行値の IN でも**主キー索引が効いていた** —— sessions の id IN が
-- 全表走査に落ちたのとは違う。レビューは「行値なので外側に使える索引が無く、
-- 全表走査が確定する」と書いていたが、**実測ではそうならなかった。**
--
-- それでも揃える理由は 2 つ:
--   - ctid のほうが 2 倍速く、バッファも半分になる (上の表)
--   - **プランの選択に依存しない。** 上のプランは統計次第で
--     sessions と同じ全表走査に落ちうる。ARRAY(...) は InitPlan として
--     1 回だけ評価され、走査量がバッチサイズだけで決まる
--
-- 引き受けるものは DeleteExpiredSessions に書いたとおり
-- (UPDATE と競合した行が 1 周ぶん残る。drain が次で拾う)。
DELETE FROM idempotency_keys
WHERE ctid = ANY(ARRAY(
    SELECT ctid FROM idempotency_keys
    WHERE created_at <= now() - sqlc.arg('retention')::interval
    ORDER BY created_at
    LIMIT sqlc.arg('max_rows')
));
