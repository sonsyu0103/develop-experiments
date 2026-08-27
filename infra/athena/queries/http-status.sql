-- HTTP のステータス分布とレイテンシ (docs/adr/0010-log-pipeline.md 4-6)。
--
-- **手元 (DuckDB) で実行できる形で書いてある。** Athena に持っていくときは
-- 2 か所を読み替える:
--
--   1. dt の絞り込みを必ず足す。無いと全期間をスキャンする (決定 2)。
--        WHERE dt = '2026-08-16'
--   2. quantile_cont(latency_ms, 0.95) → approx_percentile(latency_ms, 0.95)
--
-- 読み替えを忘れても結果は出る。**出るのに高い**のが Athena の怖いところで、
-- ワークグループにスキャン量の上限を設定しておく理由になる。

SELECT
    path,
    method,
    status,
    count(*)                                 AS requests,
    round(avg(latency_ms), 2)                AS avg_ms,
    round(quantile_cont(latency_ms, 0.95), 2) AS p95_ms,
    round(max(latency_ms), 2)                AS max_ms
FROM go_api_logs
WHERE msg = 'http_request'
GROUP BY path, method, status
ORDER BY requests DESC;
