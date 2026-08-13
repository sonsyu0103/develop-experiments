package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/image/domain/model"
	"develop-experiments/apps/go-api/internal/image/domain/repository"
)

// ---------------------------------------------------------------------------
// 実 DB に対する検証
// ---------------------------------------------------------------------------
//
// **回収バッチの経路は、ここでしか実 DB を通りません。**
//
//   - ユニットテストはフェイクのリポジトリで動く
//   - スモークは HTTP 越しなので、回収を叩く口が無い
//     (定期処理は 10 分間隔 + ばらつきなので、CI の実行時間では発火しない)
//
// つまり ListReclaimable の SQL (FOR UPDATE SKIP LOCKED を含む) と
// pgtype.Interval への変換は、この検査が無いと**一度も実行されません**。
// 「テストがある」と「実行されている」は別、という話がまた出てきます
// (docs/adr/0015-idempotency.md の 6)。
//
// DATABASE_TEST_URL が無ければスキップします。
// CI では migration-ci が設定します (objectstorage と同じ形)。
// **CI ではスキップを失敗として扱います** —— 消しても気づけない穴を作らないため。

// liveDB は実 DB への接続を返します。未設定ならスキップします。
func liveDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_TEST_URL")
	if dsn == "" {
		if requireLiveDB() {
			t.Fatal("DATABASE_TEST_URL が未設定です。" +
				"CI では実 DB に対する検証を省略できません " +
				"(意図的に飛ばすなら DB_TEST_REQUIRE=0)")
		}
		t.Skip("DATABASE_TEST_URL が未設定のためスキップ (実 DB が必要)")
	}

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("接続できませんでした: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// requireLiveDB は実 DB への検証を必須とするかを返します。
//
// **CI 環境変数を既定にしません。** go test は Go Checks でも走り、
// そちらに DB はありません (objectstorage で一度落とした形と同じ)。
func requireLiveDB() bool {
	raw := strings.TrimSpace(os.Getenv("DB_TEST_REQUIRE"))
	return raw != "" && raw != "0" && !strings.EqualFold(raw, "false")
}

// seedOwner は画像の所有者を用意します。
func seedOwner(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()

	ctx := t.Context()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, public_id, google_sub, email, display_name)
		VALUES ($1, gen_random_uuid(), $2, $3, 'live テスト')
		ON CONFLICT (google_sub) DO NOTHING`,
		id, "live-test-sub-"+uuid.NewString(), "live@example.com")
	if err != nil {
		t.Fatalf("所有者を作れませんでした: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `UPDATE users SET avatar_image_id = NULL WHERE id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM images WHERE owner_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	})
}

// insertImage は指定の状態・経過時間で画像を 1 件作ります。
func insertImage(
	t *testing.T, pool *pgxpool.Pool, ownerID int64, status model.Status, age time.Duration,
) uuid.UUID {
	t.Helper()

	id := uuid.New()
	var committedAt any
	if status != model.StatusPending {
		committedAt = time.Now()
	}
	_, err := pool.Exec(t.Context(), `
		INSERT INTO images (id, owner_id, kind, object_key, content_type,
		                    width, height, byte_size, status, created_at, committed_at)
		VALUES ($1, $2, 'comment_attachment', $3, 'image/jpeg', 10, 10, 100, $4,
		        now() - $5::interval, $6)`,
		id, ownerID, "images/"+id.String()+".jpg", string(status),
		age.String(), committedAt)
	if err != nil {
		t.Fatalf("画像を作れませんでした: %v", err)
	}
	return id
}

// **回収の SQL が実 DB で動くこと。**
//
// FOR UPDATE SKIP LOCKED と interval の受け渡しは、
// フェイクでは一切検証できません。
func TestImageRepository_ListReclaimable_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900300)
	seedOwner(t, pool, ownerID)

	repo := NewImageRepository(pool)
	ctx := t.Context()

	pending := insertImage(t, pool, ownerID, model.StatusPending, 2*time.Hour)
	deleted := insertImage(t, pool, ownerID, model.StatusDeleted, 2*time.Hour)
	orphan := insertImage(t, pool, ownerID, model.StatusCommitted, 2*time.Hour)
	recent := insertImage(t, pool, ownerID, model.StatusCommitted, time.Minute)

	var got []model.Image
	err := repo.WithinTx(ctx, func(tx repository.ImageRepository) error {
		var listErr error
		got, listErr = tx.ListReclaimable(ctx, time.Hour, 100)
		return listErr
	})
	if err != nil {
		t.Fatalf("ListReclaimable が失敗した: %v", err)
	}

	found := map[uuid.UUID]model.Status{}
	for _, img := range got {
		if img.OwnerID == ownerID {
			found[img.ID] = img.Status
		}
	}

	for _, want := range []uuid.UUID{pending, deleted, orphan} {
		if _, ok := found[want]; !ok {
			t.Errorf("回収対象に含まれていない: %s", want)
		}
	}
	// **アップロード直後は対象にしない。**
	if _, ok := found[recent]; ok {
		t.Error("経過時間が短い画像が回収対象になっている")
	}
}

// **参照されている committed は対象にならないこと。**
// NOT EXISTS の 3 テーブルが効いているかを実 DB で見ます。
func TestImageRepository_ListReclaimable_SkipsReferenced_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900301)
	seedOwner(t, pool, ownerID)

	repo := NewImageRepository(pool)
	ctx := t.Context()

	referenced := insertImage(t, pool, ownerID, model.StatusCommitted, 2*time.Hour)
	if _, err := pool.Exec(ctx,
		`UPDATE users SET avatar_image_id = $1 WHERE id = $2`, referenced, ownerID); err != nil {
		t.Fatalf("参照を張れませんでした: %v", err)
	}

	var got []model.Image
	if err := repo.WithinTx(ctx, func(tx repository.ImageRepository) error {
		var listErr error
		got, listErr = tx.ListReclaimable(ctx, time.Hour, 100)
		return listErr
	}); err != nil {
		t.Fatalf("ListReclaimable が失敗した: %v", err)
	}

	for _, img := range got {
		if img.ID == referenced {
			t.Error("参照されている画像が回収対象になっている")
		}
	}
}

// MarkReclaimed が対象から外すこと。
func TestImageRepository_MarkReclaimed_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900302)
	seedOwner(t, pool, ownerID)

	repo := NewImageRepository(pool)
	ctx := t.Context()
	id := insertImage(t, pool, ownerID, model.StatusDeleted, 2*time.Hour)

	if err := repo.WithinTx(ctx, func(tx repository.ImageRepository) error {
		return tx.MarkReclaimed(ctx, id)
	}); err != nil {
		t.Fatalf("MarkReclaimed が失敗した: %v", err)
	}

	// **行は残る** (ADR 0016 問題 3)。
	img, err := repo.FindByID(ctx, id)
	if err != nil {
		t.Fatalf("記録したあとに引けない (行が消えている): %v", err)
	}
	if img.ObjectReclaimedAt == nil {
		t.Error("object_reclaimed_at が入っていない")
	}

	// 次の周回で拾わないこと。
	var got []model.Image
	if err := repo.WithinTx(ctx, func(tx repository.ImageRepository) error {
		var listErr error
		got, listErr = tx.ListReclaimable(ctx, time.Hour, 100)
		return listErr
	}); err != nil {
		t.Fatalf("ListReclaimable が失敗した: %v", err)
	}
	for _, g := range got {
		if g.ID == id {
			t.Error("回収済みの行を拾い直している")
		}
	}
}

// Delete が行ごと消すこと。
func TestImageRepository_Delete_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900303)
	seedOwner(t, pool, ownerID)

	repo := NewImageRepository(pool)
	ctx := t.Context()
	id := insertImage(t, pool, ownerID, model.StatusPending, 2*time.Hour)

	if err := repo.WithinTx(ctx, func(tx repository.ImageRepository) error {
		return tx.Delete(ctx, id)
	}); err != nil {
		t.Fatalf("Delete が失敗した: %v", err)
	}
	if _, err := repo.FindByID(ctx, id); err == nil {
		t.Error("消したはずの行が引けた")
	}
}

// **入れ子のトランザクションを作れないこと。**
// 中で使うリポジトリに pool を渡していないことの確認になります。
func TestImageRepository_NestedTxIsRejected_Live(t *testing.T) {
	pool := liveDB(t)
	repo := NewImageRepository(pool)

	err := repo.WithinTx(t.Context(), func(tx repository.ImageRepository) error {
		return tx.WithinTx(t.Context(), func(repository.ImageRepository) error { return nil })
	})
	if err == nil {
		t.Error("入れ子のトランザクションが通った")
	}
}

// **確定できない状態の画像を再確定させないこと** (実 DB で確認)。
// CommitImage が status = 'pending' を条件に含めているかを見ます。
func TestImageRepository_CommitRejectsDeleted_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900304)
	seedOwner(t, pool, ownerID)

	repo := NewImageRepository(pool)
	ctx := t.Context()
	id := insertImage(t, pool, ownerID, model.StatusDeleted, time.Minute)

	if _, err := repo.Commit(ctx, id); err == nil {
		t.Fatal("削除済みの画像を確定できてしまった")
	}

	img, err := repo.FindByID(ctx, id)
	if err != nil {
		t.Fatalf("引けなかった: %v", err)
	}
	if img.Status != model.StatusDeleted {
		t.Errorf("status = %q, want deleted (再確定されている)", img.Status)
	}
}
