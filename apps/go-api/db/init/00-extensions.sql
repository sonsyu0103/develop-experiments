-- コンテナ初回起動時に一度だけ実行される。
-- 拡張の作成は superuser 権限が要る「運用の作業」なので、
-- アプリのマイグレーション (golang-migrate) とは分けている。

-- クエリ単位の実行統計を集計する。
-- 「どのクエリが何回呼ばれて、合計どれだけ時間を使ったか」が分かるので、
-- N+1 の検出とインデックス改善の効果測定に使う。
--
--   SELECT calls, mean_exec_time, query
--   FROM pg_stat_statements
--   ORDER BY total_exec_time DESC
--   LIMIT 10;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- スレッド検索 (ADR 0012)。文字列を 3 文字のトライグラムに分解し、
-- 中間一致 (LIKE '%語%') に GIN 索引を効かせる。
-- 索引そのものはマイグレーション 000010 が張る。
--
-- **このファイルは初回起動時にしか実行されない。**
-- Phase 11 より前に作ったボリュームには pg_trgm が入っていないため、
-- make clean && make up でデータベースを作り直す必要がある
-- (000010 が拡張の有無を確かめて、その旨のエラーを出す)。
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- 注: auto_explain は SQL 拡張ではなく共有ライブラリなので、
-- ここではなく compose.yaml の shared_preload_libraries で読み込んでいる。
