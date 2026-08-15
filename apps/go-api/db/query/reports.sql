-- =============================================================================
-- 通報 (docs/adr/0011-moderation.md 決定 4)
--
-- 【匿名通報を許さない】
-- reporter_id が NOT NULL なのがその表現になる。通報を匿名で受け付けると、
-- 通報そのものが荒らしの手段になる (無限に投げてキューを埋められる)。
--
-- 【閾値で自動削除しない】
-- ここには「同じ対象への通報を数えて閾値と比べる」クエリを置かない。
-- 置いた時点で、それを使う実装への距離がゼロになる。
-- 決定 4 は通報爆撃と少数意見の構造的な排除を理由に、これを明確に避けている。
-- =============================================================================

-- name: CreateReport :one
-- 通報を 1 件積む。
--
-- **重複はエラーにしない** (ADR 0011 の引き受けるコスト)。
-- 一意制約 reports_unique_per_user に当たった場合は 0 行を返し、
-- 呼び出し側が「既に通報済み」として最初の通報を読み直す。
--
-- ON CONFLICT DO NOTHING にしているのは、**更新もしたくない**ため。
-- 2 回目の理由で上書きすると、最初の通報の内容が消える。
--
-- 【target_thread_id は comment のときだけ入る】
-- CHECK 制約 reports_thread_id_matches_target が、
-- 「どちらの通報か」と「スレッド ID を持つか」のずれを拒否する (000009)。
INSERT INTO reports (reporter_id, target_type, target_id, target_thread_id, reason, note)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT ON CONSTRAINT reports_unique_per_user DO NOTHING
RETURNING id, reporter_id, target_type, target_id, target_thread_id, reason, note, status, created_at, resolved_at, resolved_by;

-- name: FindReportByTarget :one
-- 既に積まれている通報を読む。**重複通報のときだけ呼ぶ。**
--
-- CreateReport が 0 行だったあとに引くので、必ず 1 行見つかる想定。
-- 見つからなければ、その間に通報が消えたということになる (行を消す経路は無い)。
SELECT id, reporter_id, target_type, target_id, target_thread_id, reason, note, status, created_at, resolved_at, resolved_by
FROM reports
WHERE reporter_id = sqlc.arg('reporter_id')
  AND target_type = sqlc.arg('target_type')
  AND target_id = sqlc.arg('target_id');

-- name: ListReports :many
-- 通報キュー。**古い順** (id 昇順) に返す。
--
-- 【なぜ created_at ではなく id で並べるか】
-- **created_at は一意ではない。** キーセットページネーションの境界に
-- 一意でない列を使うと、同じ時刻の行が複数あるページ境界で
-- 取りこぼしと重複が起きる。通報は荒らしへの対応という性質上、
-- 短時間に集中して届くので、これは実際に起きる。
--
-- id は IDENTITY なので一意かつ単調増加で、created_at の既定は now()。
-- 順序は実質同じで、一意性だけが得られる (000009 で索引も張り替えた)。
--
-- 【カーソルの向きが他の一覧と逆】
-- スレッド / コメントは新しい順 (id < cursor) だが、キューは古い順なので
-- id > cursor になる。**同じ Cursor 型を使い回すが、比較の向きが違う。**
--
-- 【status で絞る】
-- 既定の 'open' のときだけ部分索引 reports_open_idx が効く。
-- 解決済みを引く場合は全体の走査になる (件数が増え続けるので調査用と割り切る)。
SELECT id, reporter_id, target_type, target_id, target_thread_id, reason, note, status, created_at, resolved_at, resolved_by
FROM reports
WHERE status = sqlc.arg('status')
  AND (sqlc.narg('cursor_id')::bigint IS NULL OR id > sqlc.narg('cursor_id')::bigint)
ORDER BY id
LIMIT sqlc.arg('page_size');

-- name: ResolveReport :one
-- 通報を処理済みにする。
--
-- **status = 'open' を条件に含める。** 含めないと、
-- 既に処理済みの通報の resolved_by が後から来た操作で上書きされる。
-- 0 行なら「無い、または既に処理済み」で、呼び出し側は 404 にする。
--
-- **resolved_at と resolved_by を必ず一緒に書く。**
-- CHECK 制約 reports_resolution_complete がそれを要求する ——
-- 片方だけ入った行は「誰が解決したか分からない解決済み通報」になり、
-- moderation_actions と同じ理由で許容できない。
--
-- **投稿には触れない。** 通報の状態を変えるだけ。
-- 削除は POST /moderation/actions が別に行う ——
-- 「通報を却下する」と「投稿を消す」は別の判断であり、
-- まとめるとキューを片付ける操作がそのまま削除になる。
UPDATE reports
SET status = sqlc.arg('status'),
    resolved_at = now(),
    resolved_by = sqlc.arg('resolved_by')
WHERE id = sqlc.arg('id')
  AND status = 'open'
RETURNING id, reporter_id, target_type, target_id, target_thread_id, reason, note, status, created_at, resolved_at, resolved_by;
