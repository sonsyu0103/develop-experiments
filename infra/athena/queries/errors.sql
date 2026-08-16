-- ERROR の一覧 (docs/adr/0010-log-pipeline.md 4-3)。
--
-- **ERROR = 人が対応する必要がある**、という意味を持たせている。
-- CloudWatch のアラームはこのレベルを起点に組むので、
-- ここに「人が対応しなくてよいもの」が混ざっていたら、
-- それはレベルの付け方が間違っている。
--
-- 特に見るべき混入:
--
--   - リトライして成功した直列化失敗 (40001)。これは ERROR ではない。
--     Phase 2 の設計が正しく動いているほど発生するので、
--     ERROR にするとアラートが鳴り続ける (ADR 0019 決定 4)
--   - シャットダウン時に打ち切られた定期処理。
--     ctx のキャンセル由来は INFO に落としてある (scheduler.run)
--
-- Athena で使うときは dt の絞り込みを足す。

SELECT
    msg,
    count(*)      AS occurrences,
    min(time)     AS first_seen,
    max(time)     AS last_seen,
    -- 代表例を 1 件だけ。全文を並べると読めなくなる。
    any_value(error) AS example_error
FROM go_api_logs
WHERE level = 'ERROR'
GROUP BY msg
ORDER BY occurrences DESC;
