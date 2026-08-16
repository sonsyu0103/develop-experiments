-- 遅かったリクエストを request_id で串刺しにする
-- (docs/adr/0010-log-pipeline.md 4-1)。
--
-- **相関 ID を入れた理由がこれ。** 以前の requestLogger は
-- method / path / status / latency しか出しておらず、
-- 同時刻の別リクエストと区別できなかった。1 リクエストの中で
-- 何が起きていたか (リトライ、DB の待ち、画像の再エンコード) を
-- 追うには、時刻ではなく ID で束ねるしかない。
--
-- Athena で使うときは dt の絞り込みを足す。

WITH slow AS (
    SELECT request_id, latency_ms, path, status
    FROM go_api_logs
    WHERE msg = 'http_request'
      AND request_id IS NOT NULL
    ORDER BY latency_ms DESC
    LIMIT 5
)
SELECT
    s.latency_ms,
    s.path,
    s.status,
    l.time,
    l.level,
    l.msg,
    -- イベント固有の項目。無いものは NULL になる (スキーマオンリード)。
    l.thread_id,
    l.attempts
FROM slow s
JOIN go_api_logs l ON l.request_id = s.request_id
ORDER BY s.latency_ms DESC, l.time;
