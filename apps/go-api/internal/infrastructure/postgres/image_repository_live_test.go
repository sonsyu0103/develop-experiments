package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/image/domain/model"
	"develop-experiments/apps/go-api/internal/image/domain/repository"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
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

// insertImage は指定の状態・経過時間でコメント添付用の画像を 1 件作ります。
func insertImage(
	t *testing.T, pool *pgxpool.Pool, ownerID int64, status model.Status, age time.Duration,
) uuid.UUID {
	t.Helper()
	return insertImageOf(t, pool, ownerID, "comment_attachment", status, age)
}

// insertImageOf は用途 (kind) も指定して画像を 1 件作ります。
func insertImageOf(
	t *testing.T, pool *pgxpool.Pool, ownerID int64, kind string,
	status model.Status, age time.Duration,
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
		VALUES ($1, $2, $3, $4, 'image/jpeg', 10, 10, 100, $5,
		        now() - $6::interval, $7)`,
		id, ownerID, kind, "images/"+id.String()+".jpg", string(status),
		age.String(), committedAt)
	if err != nil {
		t.Fatalf("画像を作れませんでした: %v", err)
	}
	return id
}

// attachedAt は images.attached_at を読みます。
func attachedAt(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) *time.Time {
	t.Helper()

	var at *time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT attached_at FROM images WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("attached_at を読めませんでした: %v", err)
	}
	return at
}

// reclaimableIDs は回収対象の ID 集合を返します。
func reclaimableIDs(t *testing.T, repo *ImageRepository) map[uuid.UUID]model.Status {
	t.Helper()

	var got []model.Image
	if err := repo.WithinTx(t.Context(), func(tx repository.ImageRepository) error {
		var listErr error
		got, listErr = tx.ListReclaimable(t.Context(), time.Hour, 100)
		return listErr
	}); err != nil {
		t.Fatalf("ListReclaimable が失敗した: %v", err)
	}

	found := make(map[uuid.UUID]model.Status, len(got))
	for _, img := range got {
		found[img.ID] = img.Status
	}
	return found
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
//
// **attached_at を書かずに参照だけ張ります。** これは 000007 で足した
// 「索引で絞る列」が壊れた状態そのものです —— 新しい添付経路を足した人が
// attached_at を書き忘れると、まさにこの形になります。
// 索引の述語 (attached_at IS NULL) は通ってしまうので、
// **ここで効くのは NOT EXISTS x3 の側だけ**になります。
// この検査が落ちるときは、安全網が外れているという意味です。
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

// ---------------------------------------------------------------------------
// attached_at (000007)
// ---------------------------------------------------------------------------
//
// **ここはユニットテストでは一切検証できません。** attached_at を書くのは
// 添付する側の SQL (comments / threads / users) であり、Go 側に対応する
// コードが無いためです。フェイクのリポジトリには存在しない列になります。

// **プロフィール画像を設定すると attached_at が入り、回収対象から外れること。**
func TestSetAvatarImage_MarksAttached_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900305)
	seedOwner(t, pool, ownerID)

	images := NewImageRepository(pool)
	sessions := NewSessionRepository(pool)
	ctx := t.Context()

	id := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)

	// 設定する前は「未添付の committed」なので回収対象になる。
	if _, ok := reclaimableIDs(t, images)[id]; !ok {
		t.Fatal("設定前の画像が回収対象に入っていない (前提が崩れている)")
	}

	if _, err := sessions.SetAvatarImage(ctx, ownerID, &id); err != nil {
		t.Fatalf("プロフィール画像を設定できませんでした: %v", err)
	}

	if attachedAt(t, pool, id) == nil {
		t.Error("attached_at が入っていない (添付と同じ 1 文で書けていない)")
	}
	if _, ok := reclaimableIDs(t, images)[id]; ok {
		t.Error("添付済みの画像が回収対象に残っている")
	}
}

// **付け替えると、旧画像の attached_at が NULL に戻ること。**
//
// ここを落とすと、差し替えられた画像が**どこからも参照されないのに
// 永久に回収されない**状態で残ります。
// 参照が消えるだけの NOT EXISTS 方式には無かった、列を持ったことによる
// 新しい失敗の形なので、実 DB で押さえます。
func TestSetAvatarImage_DetachesPrevious_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900306)
	seedOwner(t, pool, ownerID)

	images := NewImageRepository(pool)
	sessions := NewSessionRepository(pool)
	ctx := t.Context()

	old := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	next := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)

	if _, err := sessions.SetAvatarImage(ctx, ownerID, &old); err != nil {
		t.Fatalf("1 枚目を設定できませんでした: %v", err)
	}
	if _, err := sessions.SetAvatarImage(ctx, ownerID, &next); err != nil {
		t.Fatalf("2 枚目を設定できませんでした: %v", err)
	}

	if attachedAt(t, pool, old) != nil {
		t.Error("旧画像の attached_at が残っている (永久に回収されない)")
	}
	if attachedAt(t, pool, next) == nil {
		t.Error("新画像の attached_at が入っていない")
	}

	found := reclaimableIDs(t, images)
	if _, ok := found[old]; !ok {
		t.Error("外された画像が回収対象になっていない")
	}
	if _, ok := found[next]; ok {
		t.Error("設定中の画像が回収対象になっている")
	}
}

// **解除しても旧画像が回収対象に戻ること** (nil を渡す経路)。
func TestSetAvatarImage_DetachesOnClear_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900307)
	seedOwner(t, pool, ownerID)

	images := NewImageRepository(pool)
	sessions := NewSessionRepository(pool)
	ctx := t.Context()

	id := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	if _, err := sessions.SetAvatarImage(ctx, ownerID, &id); err != nil {
		t.Fatalf("設定できませんでした: %v", err)
	}
	if _, err := sessions.SetAvatarImage(ctx, ownerID, nil); err != nil {
		t.Fatalf("解除できませんでした: %v", err)
	}

	if attachedAt(t, pool, id) != nil {
		t.Error("解除したのに attached_at が残っている")
	}
	if _, ok := reclaimableIDs(t, images)[id]; !ok {
		t.Error("解除した画像が回収対象になっていない")
	}
}

// **同じ画像を設定し直しても、添付が外れないこと。**
//
// detached と attached は同じ 1 文の中の別の CTE で、実行順は決まっていません。
// 旧 = 新 のときに両方が同じ行を触ると、順序次第で attached_at が
// NULL のまま残り、**使用中の画像が回収対象になります**。
// IS DISTINCT FROM で対象を重ねないようにしているのを、ここで確かめます。
func TestSetAvatarImage_SameImageTwice_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900308)
	seedOwner(t, pool, ownerID)

	images := NewImageRepository(pool)
	sessions := NewSessionRepository(pool)
	ctx := t.Context()

	id := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	for n := range 2 {
		if _, err := sessions.SetAvatarImage(ctx, ownerID, &id); err != nil {
			t.Fatalf("%d 回目の設定に失敗した: %v", n+1, err)
		}
	}

	if attachedAt(t, pool, id) == nil {
		t.Error("同じ画像を設定し直したら添付が外れた")
	}
	if _, ok := reclaimableIDs(t, images)[id]; ok {
		t.Error("使用中の画像が回収対象になっている")
	}
}

// **添付済みでも deleted なら回収対象になること。**
//
// 索引の述語が「attached_at IS NULL OR status = 'deleted'」である理由。
// モデレーターが消した画像は投稿から参照されたままなので、
// attached_at だけで絞ると S3 の実体が永久に残ります (ADR 0011 決定 5)。
func TestImageRepository_ListReclaimable_IncludesAttachedDeleted_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900309)
	seedOwner(t, pool, ownerID)

	images := NewImageRepository(pool)
	sessions := NewSessionRepository(pool)
	ctx := t.Context()

	id := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	if _, err := sessions.SetAvatarImage(ctx, ownerID, &id); err != nil {
		t.Fatalf("設定できませんでした: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE images SET status = 'deleted' WHERE id = $1`, id); err != nil {
		t.Fatalf("削除済みにできませんでした: %v", err)
	}

	if attachedAt(t, pool, id) == nil {
		t.Fatal("前提が崩れている: 添付されたままであること")
	}
	if got, ok := reclaimableIDs(t, images)[id]; !ok {
		t.Error("添付済みの deleted が回収対象になっていない")
	} else if got != model.StatusDeleted {
		t.Errorf("status = %q, want deleted", got)
	}
}

// **pending のまま添付できないこと** (CHECK 制約 images_attached_at_requires_commit)。
//
// アプリ側は committed だけを添付しますが、ここは最後の砦になります。
func TestImages_AttachedAtRequiresCommit_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900310)
	seedOwner(t, pool, ownerID)

	id := insertImage(t, pool, ownerID, model.StatusPending, time.Minute)
	if _, err := pool.Exec(t.Context(),
		`UPDATE images SET attached_at = now() WHERE id = $1`, id); err == nil {
		t.Error("pending の画像を添付済みにできてしまった")
	}
}

// **コメントに添付すると attached_at が入ること。**
//
// アバターと違い、こちらは SessionRepository のような入口が無いので
// 生成されたクエリを直接叩きます。**検証したいのは SQL そのもの**なので、
// これで十分になります (Go 側に対応するコードは存在しない)。
func TestCreateComment_MarksImageAttached_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900311)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	q := sqlcgen.New(pool)

	threadID := seedThread(t, pool, nil)
	id := insertImage(t, pool, ownerID, model.StatusCommitted, 2*time.Hour)

	if _, err := q.CreateCommentAutoSeq(ctx, sqlcgen.CreateCommentAutoSeqParams{
		ThreadID:   threadID,
		AuthorName: "名無しさん",
		Body:       "添付の記録を見る",
		ImageID:    &id,
	}); err != nil {
		t.Fatalf("コメントを作れませんでした: %v", err)
	}

	if attachedAt(t, pool, id) == nil {
		t.Error("attached_at が入っていない (回収バッチが実体を消しにいく)")
	}
	if _, ok := reclaimableIDs(t, NewImageRepository(pool))[id]; ok {
		t.Error("添付済みの画像が回収対象に残っている")
	}
}

// **コメントが作られなかったら添付もしないこと。**
//
// CreateCommentAutoSeq は親スレッドが削除済みだと 0 行になります。
// そこで attached_at を書くと、**投稿されていないのに二度と回収されない**
// 画像が残ります。attached CTE が inserted を参照している理由になります。
func TestCreateComment_DoesNotAttachWhenInsertFails_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900312)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	q := sqlcgen.New(pool)

	threadID := seedThread(t, pool, nil)
	if _, err := pool.Exec(ctx,
		`UPDATE threads SET deleted_at = now() WHERE id = $1`, threadID); err != nil {
		t.Fatalf("スレッドを削除できませんでした: %v", err)
	}

	id := insertImage(t, pool, ownerID, model.StatusCommitted, 2*time.Hour)
	if _, err := q.CreateCommentAutoSeq(ctx, sqlcgen.CreateCommentAutoSeqParams{
		ThreadID:   threadID,
		AuthorName: "名無しさん",
		Body:       "作られないはず",
		ImageID:    &id,
	}); err == nil {
		t.Fatal("削除済みスレッドにコメントできてしまった (前提が崩れている)")
	}

	if attachedAt(t, pool, id) != nil {
		t.Error("投稿されていないのに添付済みになっている")
	}
	if _, ok := reclaimableIDs(t, NewImageRepository(pool))[id]; !ok {
		t.Error("宙に浮いた画像が回収対象になっていない")
	}
}

// **スレッドのアイコンでも attached_at が入ること。**
func TestCreateThread_MarksIconAttached_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900313)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	q := sqlcgen.New(pool)

	id := insertImageOf(t, pool, ownerID, "thread_icon", model.StatusCommitted, 2*time.Hour)
	author := ownerID
	row, err := q.CreateThread(ctx, sqlcgen.CreateThreadParams{
		Title:       "アイコン付き",
		AuthorID:    &author,
		IconImageID: &id,
	})
	if err != nil {
		t.Fatalf("スレッドを作れませんでした: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM threads WHERE id = $1`, row.ID)
	})

	if attachedAt(t, pool, id) == nil {
		t.Error("attached_at が入っていない")
	}
	if _, ok := reclaimableIDs(t, NewImageRepository(pool))[id]; ok {
		t.Error("添付済みのアイコンが回収対象に残っている")
	}
}

// seedThread は検証用のスレッドを 1 件作り、後片付けを登録します。
func seedThread(t *testing.T, pool *pgxpool.Pool, authorID *int64) int64 {
	t.Helper()

	var id int64
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO threads (title, author_id) VALUES ('live テスト', $1) RETURNING id`,
		authorID).Scan(&id); err != nil {
		t.Fatalf("スレッドを作れませんでした: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM comments WHERE thread_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM threads WHERE id = $1`, id)
	})
	return id
}

// ---------------------------------------------------------------------------
// 実体の無い画像を配らないこと (レビュー指摘)
// ---------------------------------------------------------------------------
//
// status = 'deleted' と回収済みの画像は、行こそ残りますが
// **S3 の実体は消えています。** URL を返すとブラウザには壊れた画像が出ます。
// 結合の ON に条件を置いて、画像なしとして扱います。

// **削除済みのアイコンはスレッドの応答に出ないこと。**
func TestGetThread_SkipsDeletedIcon_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900314)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	q := sqlcgen.New(pool)

	id := insertImageOf(t, pool, ownerID, "thread_icon", model.StatusCommitted, 2*time.Hour)
	author := ownerID
	row, err := q.CreateThread(ctx, sqlcgen.CreateThreadParams{
		Title: "アイコン付き", AuthorID: &author, IconImageID: &id,
	})
	if err != nil {
		t.Fatalf("スレッドを作れませんでした: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM threads WHERE id = $1`, row.ID)
	})

	// 作った直後は出る (前提の確認)。
	got, err := q.GetThreadWithCommentCount(ctx, row.ID)
	if err != nil {
		t.Fatalf("引けませんでした: %v", err)
	}
	if got.IconID == nil {
		t.Fatal("前提が崩れている: 通常はアイコンが出ること")
	}

	// モデレーターが消すと出なくなる。
	if _, execErr := pool.Exec(ctx,
		`UPDATE images SET status = 'deleted' WHERE id = $1`, id); execErr != nil {
		t.Fatalf("削除済みにできませんでした: %v", execErr)
	}
	got, err = q.GetThreadWithCommentCount(ctx, row.ID)
	if err != nil {
		t.Fatalf("引けませんでした: %v", err)
	}
	if got.IconID != nil {
		t.Error("削除済みのアイコンが応答に出ている (実体はもう無い)")
	}
	// **スレッド自体は消えないこと** (LEFT が INNER に化けていないか)。
	if got.ID != row.ID {
		t.Errorf("スレッドが引けなくなっている (ON ではなく WHERE に置いていないか)")
	}
}

// **回収済みのアイコンも同じく出ないこと。**
func TestGetThread_SkipsReclaimedIcon_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900315)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	q := sqlcgen.New(pool)

	id := insertImageOf(t, pool, ownerID, "thread_icon", model.StatusCommitted, 2*time.Hour)
	author := ownerID
	row, err := q.CreateThread(ctx, sqlcgen.CreateThreadParams{
		Title: "アイコン付き", AuthorID: &author, IconImageID: &id,
	})
	if err != nil {
		t.Fatalf("スレッドを作れませんでした: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM threads WHERE id = $1`, row.ID)
	})

	if _, execErr := pool.Exec(ctx,
		`UPDATE images SET object_reclaimed_at = now() WHERE id = $1`, id); execErr != nil {
		t.Fatalf("回収済みにできませんでした: %v", execErr)
	}
	got, err := q.GetThreadWithCommentCount(ctx, row.ID)
	if err != nil {
		t.Fatalf("引けませんでした: %v", err)
	}
	if got.IconID != nil {
		t.Error("回収済みのアイコンが応答に出ている")
	}
}

// ---------------------------------------------------------------------------
// 回収の確保済みの画像を添付させないこと (DB レビュー)
// ---------------------------------------------------------------------------
//
// EnsureOwned はロックを取らない読み取りなので、「確認したあと・書く前」に
// 回収バッチが画像を確保する窓があります。
//
// **以前は「添付済みにしない」だけを確かめていました。** それでは足りず、
// 投稿やアバターは回収済みの画像を参照したまま作られ、回収バッチの
// DeleteImage が外部キー違反で止まっていました (実測で再現)。
// 添付する文が画像の行をロックして条件を確かめ、**書き込みごと 0 行に
// する**ことを、ここで押さえます。
//
// 窓そのもの (確認と書き込みの間に確保が挟まる) はここでは作りません。
// 確保済みの画像を直接渡せば、文の中の確認だけが頼りになるためです。

// markReclaimed は回収バッチの確保と同じ状態にします。
func markReclaimed(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()

	if _, err := pool.Exec(t.Context(),
		`UPDATE images SET object_reclaimed_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("確保できませんでした: %v", err)
	}
}

// assertReclaimCanFinish は、回収バッチが行ごと消せる (= どこからも参照されていない) ことを確かめます。
func assertReclaimCanFinish(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()

	if attachedAt(t, pool, id) != nil {
		t.Error("確保済みの画像が添付済みになった")
	}
	if err := NewImageRepository(pool).Delete(t.Context(), id); err != nil {
		t.Errorf("回収バッチが行を消せない (どこかが参照したまま): %v", err)
	}
}

// **確保済みの画像はプロフィール画像に設定できず、元の画像が残ること。**
func TestSetAvatarImage_RejectsReclaimed_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900316)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	sessions := NewSessionRepository(pool)

	current := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	if _, err := sessions.SetAvatarImage(ctx, ownerID, &current); err != nil {
		t.Fatalf("前提の設定に失敗した: %v", err)
	}

	id := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	markReclaimed(t, pool, id)

	_, err := sessions.SetAvatarImage(ctx, ownerID, &id)
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("確保済みの画像を設定できてしまった (err=%v, want ErrNotFound)", err)
	}

	var avatar *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT avatar_image_id FROM users WHERE id = $1`, ownerID).Scan(&avatar); err != nil {
		t.Fatalf("利用者を読めませんでした: %v", err)
	}
	if avatar == nil || *avatar != current {
		t.Errorf("元のプロフィール画像が変わった (got=%v want=%v)", avatar, current)
	}
	if attachedAt(t, pool, current) == nil {
		t.Error("設定に失敗したのに元の画像の添付が外れた")
	}
	assertReclaimCanFinish(t, pool, id)
}

// **文だけで呼んでも、設定に失敗したときに元の画像が外れないこと。**
//
// detached は参照されていない CTE なので、主文が 0 行でも実行されます。
// 画像の条件を置き忘れると「設定は失敗したのに元の画像だけ外れる」形になります。
//
// **SessionRepository 経由では検出できません。** トランザクションの中で
// 0 行がエラーになり、ロールバックで元に戻るためです
// (条件を外す変異を入れても上の検査は通りました)。文を直接叩いて押さえます。
func TestSetUserAvatarImage_KeepsPreviousWhenRejected_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900324)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	q := sqlcgen.New(pool)

	current := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	if _, err := NewSessionRepository(pool).SetAvatarImage(ctx, ownerID, &current); err != nil {
		t.Fatalf("前提の設定に失敗した: %v", err)
	}

	id := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	markReclaimed(t, pool, id)

	_, err := q.SetUserAvatarImage(ctx, sqlcgen.SetUserAvatarImageParams{
		ID: ownerID, AvatarImageID: &id,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("確保済みの画像を設定できてしまった (err=%v, want pgx.ErrNoRows)", err)
	}
	if attachedAt(t, pool, current) == nil {
		t.Error("設定に失敗したのに元の画像の添付が外れた (detached に画像の条件が無い)")
	}
}

// **確保済みの画像はコメントに添付できないこと** (unique モードの 1 文)。
func TestCreateCommentAutoSeq_RejectsReclaimedImage_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900320)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	threadID := seedThread(t, pool, nil)
	id := insertImage(t, pool, ownerID, model.StatusCommitted, 2*time.Hour)
	markReclaimed(t, pool, id)

	author := ownerID
	_, err := sqlcgen.New(pool).CreateCommentAutoSeq(ctx, sqlcgen.CreateCommentAutoSeqParams{
		ThreadID:   threadID,
		AuthorName: "名無しさん",
		Body:       "確保済みの画像を添付する",
		AuthorID:   &author,
		ImageID:    &id,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("確保済みの画像を添付できてしまった (err=%v, want pgx.ErrNoRows)", err)
	}
	assertReclaimCanFinish(t, pool, id)
}

// **採番を呼び出し側が持つ 3 モードの文でも同じこと。**
func TestCreateCommentWithSeq_RejectsReclaimedImage_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900321)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	threadID := seedThread(t, pool, nil)
	id := insertImage(t, pool, ownerID, model.StatusCommitted, 2*time.Hour)
	markReclaimed(t, pool, id)

	author := ownerID
	_, err := sqlcgen.New(pool).CreateCommentWithSeq(ctx, sqlcgen.CreateCommentWithSeqParams{
		ThreadID:   threadID,
		Seq:        1,
		AuthorName: "名無しさん",
		Body:       "確保済みの画像を添付する",
		AuthorID:   &author,
		ImageID:    &id,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("確保済みの画像を添付できてしまった (err=%v, want pgx.ErrNoRows)", err)
	}
	assertReclaimCanFinish(t, pool, id)

	// 画像が無ければ同じ文で普通に投稿できること (条件の置き場所の確認)。
	if _, err := sqlcgen.New(pool).CreateCommentWithSeq(ctx, sqlcgen.CreateCommentWithSeqParams{
		ThreadID:   threadID,
		Seq:        1,
		AuthorName: "名無しさん",
		Body:       "画像なし",
	}); err != nil {
		t.Errorf("画像なしの投稿が失敗した: %v", err)
	}
}

// **確保済みの画像はスレッドのアイコンにできないこと。**
func TestCreateThread_RejectsReclaimedIcon_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900322)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	id := insertImageOf(t, pool, ownerID, "thread_icon", model.StatusCommitted, 2*time.Hour)
	markReclaimed(t, pool, id)

	author := ownerID
	row, err := sqlcgen.New(pool).CreateThread(ctx, sqlcgen.CreateThreadParams{
		Title:       "確保済みのアイコン",
		AuthorID:    &author,
		IconImageID: &id,
	})
	if err == nil {
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM threads WHERE id = $1`, row.ID)
		})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("確保済みのアイコンでスレッドを作れてしまった (err=%v, want pgx.ErrNoRows)", err)
	}
	assertReclaimCanFinish(t, pool, id)
}

// ---------------------------------------------------------------------------
// プロフィール画像の同時付け替え (DB レビュー)
// ---------------------------------------------------------------------------

// **同じ利用者への付け替えが重なっても、負けた側の画像が外れること。**
//
// SetUserAvatarImage の previous はその文のスナップショットで旧画像を読みます。
// 行ロックを先に取らないと、2 本目は 1 本目が付けた画像を知らないまま
// 元の画像を外し、**1 本目の画像は attached_at が入ったまま参照されず、
// 永久に回収されません** (実測で 3 回とも再現)。
//
// 1 本目をコミット前で止め、2 本目が行ロックで待たされたのを
// pg_stat_activity で確かめてからコミットします。sleep で順序を作ると
// 環境の速さ次第で順序が入れ替わり、何も検査しない回が混ざります。
func TestSetAvatarImage_ConcurrentChangeDetachesLoser_Live(t *testing.T) {
	pool := liveDB(t)
	const ownerID = int64(900323)
	seedOwner(t, pool, ownerID)

	ctx := t.Context()
	sessions := NewSessionRepository(pool)

	original := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	first := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	second := insertImageOf(t, pool, ownerID, "avatar", model.StatusCommitted, 2*time.Hour)
	if _, err := sessions.SetAvatarImage(ctx, ownerID, &original); err != nil {
		t.Fatalf("前提の設定に失敗した: %v", err)
	}

	// 1 本目: SessionRepository.SetAvatarImage と同じ手順を、コミットの手前で止める。
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("トランザクションを開始できませんでした: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := sqlcgen.New(tx)
	if _, err := q.LockUserForAvatarChange(ctx, ownerID); err != nil {
		t.Fatalf("1 本目のロックに失敗した: %v", err)
	}
	if _, err := q.SetUserAvatarImage(ctx, sqlcgen.SetUserAvatarImageParams{
		ID: ownerID, AvatarImageID: &first,
	}); err != nil {
		t.Fatalf("1 本目の付け替えに失敗した: %v", err)
	}

	// 2 本目: 行ロックで待たされる。
	done := make(chan error, 1)
	go func() {
		_, setErr := sessions.SetAvatarImage(ctx, ownerID, &second)
		done <- setErr
	}()
	waitForAvatarLockWait(t, pool)

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("1 本目をコミットできませんでした: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("2 本目の付け替えに失敗した: %v", err)
	}

	if attachedAt(t, pool, first) != nil {
		t.Error("1 本目の画像が添付済みのまま残った (どこからも参照されず、永久に回収されない)")
	}
	if attachedAt(t, pool, second) == nil {
		t.Error("2 本目の画像が添付済みになっていない")
	}
	if attachedAt(t, pool, original) != nil {
		t.Error("元の画像の添付が外れていない")
	}
}

// waitForAvatarLockWait は、プロフィール画像の付け替えが行ロックで待たされるまで待ちます。
func waitForAvatarLockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND pid <> pg_backend_pid()
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%Avatar%'
			)`).Scan(&waiting); err != nil {
			t.Fatalf("待ち状態を読めませんでした: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("2 本目が行ロックで待たされなかった (ロックを先に取っていない)")
}
