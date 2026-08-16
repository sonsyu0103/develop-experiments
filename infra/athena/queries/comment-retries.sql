-- 直列化失敗が集中しているスレッド (docs/adr/0019-comment-concurrency.md 決定 4)。
--
-- **起点は comment_created であって serialization_failure ではない。**
-- ADR 0010 の 4-6 は serialization_failure を集計する例を挙げていたが、
-- 同じ ADR の 4-3 が「各試行は DEBUG」「DEBUG は本番で出さない」と
-- 定めているため、**本番の S3 にその行は 1 件も届かない。**
-- クエリは常に 0 行を返し、しかも「競合が無い」と読めてしまう。
--
-- INFO の comment_created に attempts を載せてあるので、
-- attempts > 1 が「リトライして成功した投稿」になる。
--
-- 上限に達して失敗した分はここに出ない。serialization_retry_exhausted
-- (ERROR) を別に数える —— errors.sql で拾える。
--
-- Athena で使うときは dt の絞り込みを足す。

SELECT
    thread_id,
    count(*)                AS retried_posts,
    sum(attempts - 1)       AS retries,
    max(attempts)           AS max_attempts,
    -- モード別に見られるようにしてある (Phase 4)。
    -- ログ側だけで ssi / pessimistic / unique を切り分けられる。
    any_value(mode)         AS mode
FROM go_api_logs
WHERE msg = 'comment_created'
  AND attempts > 1
GROUP BY thread_id
ORDER BY retries DESC
LIMIT 10;
