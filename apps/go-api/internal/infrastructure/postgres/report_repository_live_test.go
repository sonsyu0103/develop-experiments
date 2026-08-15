package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
	moderationrepo "develop-experiments/apps/go-api/internal/moderation/domain/repository"
	"develop-experiments/apps/go-api/internal/pagination"
)

// ---------------------------------------------------------------------------
// 実 DB に対する検証: 通報 (docs/adr/0011-moderation.md 決定 4)
// ---------------------------------------------------------------------------
//
// **フェイクでは測れないものが 3 つあります。**
//
//   - 一意制約 (reports_unique_per_user) と ON CONFLICT DO NOTHING の組み合わせ。
//     「0 行が返る」を「既に通報済み」として扱う経路は、実 DB でしか通りません
//   - CHECK 制約 (reports_thread_id_matches_target)。
//     「どちらの通報か」と「スレッド ID を持つか」のずれを DB が拒否すること
//   - 部分索引 (reports_open_idx) を使ったキューの並びとキーセット
//
// **キーセットの向きが他の一覧と逆**である点も、ここで確かめます。

// cleanupReports は検証で積んだ通報を消します。
func cleanupReports(t *testing.T, pool *pgxpool.Pool, reporterID int64) {
	t.Helper()

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM reports WHERE reporter_id = $1 OR resolved_by = $1`, reporterID)
	})
}

// **通報が積めること。重複はエラーにならないこと。**
//
// 一意制約は「1 人で通報数を積み上げてキューの優先度を操作させない」
// ためのもので、利用者に見せる失敗ではありません
// (ADR 0011 の引き受けるコスト)。
func TestReportRepository_CreateAndDuplicate_Live(t *testing.T) {
	pool := liveDB(t)
	const reporterID = int64(900600)
	seedOwner(t, pool, reporterID)
	cleanupReports(t, pool, reporterID)

	repo := NewReportRepository(pool)
	threadID := seedThread(t, pool, nil)
	note := "同じ文面を連投しています"

	first, created, err := repo.Create(t.Context(), &model.Report{
		ReporterID: reporterID,
		Target:     model.ReportTargetThread,
		TargetID:   threadID,
		Reason:     model.ReasonSpam,
		Note:       &note,
	})
	if err != nil {
		t.Fatalf("通報を積めなかった: %v", err)
	}
	if !created {
		t.Fatal("1 回目が created = false になっている")
	}
	if first.Status != model.ReportOpen {
		t.Errorf("status = %q, want open", first.Status)
	}
	if first.Note == nil || *first.Note != note {
		t.Errorf("note = %v, want %q", first.Note, note)
	}
	// スレッドの通報なので NULL。
	if first.TargetThreadID != nil {
		t.Errorf("target_thread_id = %v, want nil", first.TargetThreadID)
	}

	// **2 回目は created = false で、最初の通報が返る。**
	// 理由を変えても上書きされない (ON CONFLICT DO NOTHING)。
	other := "別の理由"
	second, created, err := repo.Create(t.Context(), &model.Report{
		ReporterID: reporterID,
		Target:     model.ReportTargetThread,
		TargetID:   threadID,
		Reason:     model.ReasonAbuse,
		Note:       &other,
	})
	if err != nil {
		t.Fatalf("2 回目でエラーになった: %v", err)
	}
	if created {
		t.Error("2 回目が created = true になっている")
	}
	if second.ID != first.ID {
		t.Errorf("2 回目の id = %d, want %d (最初の通報を返すべき)", second.ID, first.ID)
	}
	if second.Reason != model.ReasonSpam {
		t.Errorf("reason = %q, want spam (上書きされている)", second.Reason)
	}
}

// **コメントの通報にスレッド ID が入ること。ずれた組み合わせは DB が拒否すること。**
//
// 000009 で足した CHECK 制約 (reports_thread_id_matches_target) の検査です。
func TestReportRepository_CommentTargetRequiresThreadID_Live(t *testing.T) {
	pool := liveDB(t)
	const reporterID = int64(900601)
	seedOwner(t, pool, reporterID)
	cleanupReports(t, pool, reporterID)

	repo := NewReportRepository(pool)
	threadID := seedThread(t, pool, nil)
	commentID := seedComment(t, pool, threadID)

	saved, created, err := repo.Create(t.Context(), &model.Report{
		ReporterID:     reporterID,
		Target:         model.ReportTargetComment,
		TargetID:       commentID,
		TargetThreadID: &threadID,
		Reason:         model.ReasonAbuse,
	})
	if err != nil || !created {
		t.Fatalf("コメントの通報を積めなかった: err=%v created=%v", err, created)
	}
	if saved.TargetThreadID == nil || *saved.TargetThreadID != threadID {
		t.Fatalf("target_thread_id = %v, want %d", saved.TargetThreadID, threadID)
	}

	// **コメントなのにスレッド ID が無い** → CHECK 制約が拒否する。
	if _, _, err := repo.Create(t.Context(), &model.Report{
		ReporterID: reporterID,
		Target:     model.ReportTargetComment,
		TargetID:   commentID + 1,
		Reason:     model.ReasonAbuse,
	}); err == nil {
		t.Error("スレッド ID の無いコメント通報を DB が受け入れてしまった")
	}

	// **スレッドなのにスレッド ID がある** → 同じく拒否。
	if _, _, err := repo.Create(t.Context(), &model.Report{
		ReporterID:     reporterID,
		Target:         model.ReportTargetThread,
		TargetID:       threadID,
		TargetThreadID: &threadID,
		Reason:         model.ReasonAbuse,
	}); err == nil {
		t.Error("スレッド ID つきのスレッド通報を DB が受け入れてしまった")
	}
}

// **キューが古い順で、カーソルが前へ進むこと。**
//
// 他の一覧は新しい順 (id < cursor) ですが、キューは古い順 (id > cursor) です。
// **向きを取り違えると「次ページが常に空」**になります。
//
// あわせて、処理済みがキューから外れることも見ます。
func TestReportRepository_QueueOrderAndCursor_Live(t *testing.T) {
	pool := liveDB(t)
	const reporterID = int64(900602)
	seedOwner(t, pool, reporterID)
	cleanupReports(t, pool, reporterID)

	repo := NewReportRepository(pool)

	// 3 件積む。id は IDENTITY なので積んだ順に増える。
	var ids []int64
	for range 3 {
		threadID := seedThread(t, pool, nil)
		r, created, err := repo.Create(t.Context(), &model.Report{
			ReporterID: reporterID,
			Target:     model.ReportTargetThread,
			TargetID:   threadID,
			Reason:     model.ReasonOther,
		})
		if err != nil || !created {
			t.Fatalf("通報を積めなかった: err=%v created=%v", err, created)
		}
		ids = append(ids, r.ID)
	}

	// **他の検証が残した通報と混ざらないよう、自分のぶんだけ見る。**
	mine := func(rs []model.Report) []int64 {
		var out []int64
		for _, r := range rs {
			if r.ReporterID == reporterID {
				out = append(out, r.ID)
			}
		}
		return out
	}

	page := pagination.Page{Size: 100}
	open, err := repo.List(t.Context(), model.ReportOpen, page)
	if err != nil {
		t.Fatalf("キューを読めなかった: %v", err)
	}
	got := mine(open)
	if len(got) != 3 || got[0] != ids[0] || got[2] != ids[2] {
		t.Fatalf("キュー = %v, want %v (古い順)", got, ids)
	}

	// **カーソルは前へ進む。** 1 件目より大きい id だけが返る。
	cursor := pagination.NewCursor(ids[0])
	next, err := repo.List(t.Context(), model.ReportOpen,
		pagination.Page{Cursor: &cursor, Size: 100})
	if err != nil {
		t.Fatalf("2 ページ目を読めなかった: %v", err)
	}
	if g := mine(next); len(g) != 2 || g[0] != ids[1] {
		t.Errorf("2 ページ目 = %v, want %v (カーソルの向きが逆かもしれない)", g, ids[1:])
	}

	// 処理済みにするとキューから外れる。
	if _, resolveErr := repo.Resolve(
		t.Context(), ids[0], model.ReportResolved, reporterID); resolveErr != nil {
		t.Fatalf("解決できなかった: %v", resolveErr)
	}
	open, err = repo.List(t.Context(), model.ReportOpen, page)
	if err != nil {
		t.Fatalf("キューを読み直せなかった: %v", err)
	}
	if g := mine(open); len(g) != 2 {
		t.Errorf("処理後のキュー = %v, want 2 件", g)
	}
}

// **2 回目の処理は 404 で、解決者と時刻が揃うこと。**
//
// status = 'open' を条件から外すと、既に処理済みの通報の
// resolved_by が後から来た操作で上書きされます。
func TestReportRepository_Resolve_Live(t *testing.T) {
	pool := liveDB(t)
	const (
		reporterID = int64(900603)
		actorID    = int64(900604)
	)
	seedOwner(t, pool, reporterID)
	seedOwner(t, pool, actorID)
	cleanupReports(t, pool, reporterID)

	repo := NewReportRepository(pool)
	threadID := seedThread(t, pool, nil)
	created, _, err := repo.Create(t.Context(), &model.Report{
		ReporterID: reporterID,
		Target:     model.ReportTargetThread,
		TargetID:   threadID,
		Reason:     model.ReasonIllegal,
	})
	if err != nil {
		t.Fatalf("通報を積めなかった: %v", err)
	}

	resolved, err := repo.Resolve(t.Context(), created.ID, model.ReportRejected, actorID)
	if err != nil {
		t.Fatalf("解決できなかった: %v", err)
	}
	if resolved.Status != model.ReportRejected {
		t.Errorf("status = %q, want rejected", resolved.Status)
	}
	// **CHECK 制約 reports_resolution_complete が両方を要求する。**
	if resolved.ResolvedAt == nil || resolved.ResolvedBy == nil {
		t.Fatalf("解決者と時刻が揃っていない: at=%v by=%v",
			resolved.ResolvedAt, resolved.ResolvedBy)
	}
	if *resolved.ResolvedBy != actorID {
		t.Errorf("resolved_by = %d, want %d", *resolved.ResolvedBy, actorID)
	}

	if _, err := repo.Resolve(t.Context(), created.ID, model.ReportResolved, actorID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("2 回目の err = %v, want ErrNotFound", err)
	}
}

// **DB 側の CHECK 制約が効いていること。**
//
// アプリと DB のどちらか片方だけ値を増やすと、ここで気づけます。
func TestReportRepository_RejectsUnknownValues_Live(t *testing.T) {
	pool := liveDB(t)
	const reporterID = int64(900605)
	seedOwner(t, pool, reporterID)
	cleanupReports(t, pool, reporterID)

	repo := NewReportRepository(pool)
	threadID := seedThread(t, pool, nil)
	long := make([]rune, 1001)
	for i := range long {
		long[i] = 'あ'
	}
	tooLong := string(long)

	tests := []struct {
		name   string
		report model.Report
	}{
		{"知らない target_type", model.Report{
			ReporterID: reporterID, Target: "image", TargetID: threadID, Reason: model.ReasonSpam,
		}},
		{"知らない reason", model.Report{
			ReporterID: reporterID, Target: model.ReportTargetThread,
			TargetID: threadID, Reason: "harassment",
		}},
		{"長すぎる補足", model.Report{
			ReporterID: reporterID, Target: model.ReportTargetThread,
			TargetID: threadID, Reason: model.ReasonSpam, Note: &tooLong,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.report
			if _, _, err := repo.Create(t.Context(), &r); err == nil {
				t.Error("DB が受け入れてしまった (CHECK 制約が効いていない)")
			}
		})
	}
}

// **ロール変更が実 DB で通ること。**
//
// 退会済みは対象外、居なければ 404。
func TestModerationRepository_ChangeRole_Live(t *testing.T) {
	pool := liveDB(t)
	const (
		actorID  = int64(900606)
		targetID = int64(900607)
	)
	seedOwner(t, pool, actorID)
	seedOwner(t, pool, targetID)
	cleanupActions(t, pool, actorID)

	repo := NewModerationRepository(pool)

	// **公開 ID は DB から読む。** seedOwner が gen_random_uuid() で採るので、
	// テスト側で決め打ちできない。
	var parsed uuid.UUID
	if err := pool.QueryRow(t.Context(),
		`SELECT public_id FROM users WHERE id = $1`, targetID).Scan(&parsed); err != nil {
		t.Fatalf("public_id を読めなかった: %v", err)
	}

	gotID, err := repo.ChangeRole(t.Context(), parsed, "moderator")
	if err != nil {
		t.Fatalf("ChangeRole が失敗した: %v", err)
	}
	if gotID != targetID {
		t.Errorf("返った内部 ID = %d, want %d", gotID, targetID)
	}

	var role string
	if err := pool.QueryRow(t.Context(),
		`SELECT role FROM users WHERE id = $1`, targetID).Scan(&role); err != nil {
		t.Fatalf("role を読めなかった: %v", err)
	}
	if role != "moderator" {
		t.Errorf("role = %q, want moderator", role)
	}

	// **退会済みは対象外。**
	if _, err := pool.Exec(t.Context(),
		`UPDATE users SET deleted_at = now() WHERE id = $1`, targetID); err != nil {
		t.Fatalf("退会させられなかった: %v", err)
	}
	if _, err := repo.ChangeRole(t.Context(), parsed, "admin"); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("退会済みの err = %v, want ErrNotFound", err)
	}
}

// **admin の同時降格が直列化されること** (レビュー指摘)。
//
// **フェイクでは測れません。** 並行性そのものが検査対象で、
// 「両方が通って admin が 0 人になる」は行ロックの有無で決まります。
//
// 2 つのトランザクションが同時に admin を降格させようとすると、
// LockAdminsAndCount が admin の行をすべてロックするため直列化され、
// 後から来たほうは先のコミット後に数え直して 1 人だと分かります。
func TestModerationRepository_LockAdminsAndCount_Serializes_Live(t *testing.T) {
	pool := liveDB(t)
	const (
		adminA = int64(900610)
		adminB = int64(900611)
	)
	seedOwner(t, pool, adminA)
	seedOwner(t, pool, adminB)

	for _, id := range []int64{adminA, adminB} {
		if _, err := pool.Exec(t.Context(),
			`UPDATE users SET role = 'admin' WHERE id = $1`, id); err != nil {
			t.Fatalf("admin にできなかった: %v", err)
		}
	}

	repo := NewModerationRepository(pool)

	// 前提: この 2 人ぶんは数に入る。
	// **他の検証が admin を残していないこと**もここで効く。
	var total int64
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE role = 'admin' AND deleted_at IS NULL`).
		Scan(&total); err != nil {
		t.Fatalf("admin を数えられなかった: %v", err)
	}
	if total < 2 {
		t.Fatalf("前提が壊れている: admin = %d 人", total)
	}

	// 1 つ目のトランザクションでロックを取り、握ったままにする。
	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- repo.WithinTx(t.Context(), func(tx moderationrepo.Repository) error {
			if _, err := tx.LockAdminsAndCount(t.Context()); err != nil {
				return err
			}
			close(held)
			// **握ったまま少し待つ。** この間に 2 つ目が来る。
			time.Sleep(300 * time.Millisecond)
			return nil
		})
	}()
	<-held

	// 2 つ目は待たされる。**待たされること自体が検査**になる ——
	// ロックが効いていなければ即座に返る。
	start := time.Now()
	if err := repo.WithinTx(t.Context(), func(tx moderationrepo.Repository) error {
		_, err := tx.LockAdminsAndCount(t.Context())
		return err
	}); err != nil {
		t.Fatalf("2 つ目のトランザクションが失敗した: %v", err)
	}
	waited := time.Since(start)

	if err := <-done; err != nil {
		t.Fatalf("1 つ目のトランザクションが失敗した: %v", err)
	}

	// **100ms 以上待っていれば直列化されている。**
	// 閾値を 300ms ちょうどにしないのは、スケジューリングの揺れを見込むため。
	if waited < 100*time.Millisecond {
		t.Errorf("2 つ目が %v で返った。admin の行がロックされていない", waited)
	}
}
