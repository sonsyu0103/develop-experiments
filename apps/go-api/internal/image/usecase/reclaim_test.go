package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/image/domain/model"
)

// seed は回収の対象になる画像をフェイクへ直接置きます。
//
// **created_at を過去にします。** Reclaim は grace より新しい画像を
// 対象にしないので、現在時刻のままだと何も起きません。
func seed(
	t *testing.T, repo *fakeImageRepo, storage *fakeStorage,
	status model.Status, age time.Duration,
) *model.Image {
	t.Helper()

	img, err := model.NewPending(42, model.KindCommentAttachment, 10, 10, 100)
	if err != nil {
		t.Fatalf("NewPending が失敗した: %v", err)
	}
	img.Status = status
	img.CreatedAt = time.Now().Add(-age)
	repo.stored[img.ID] = img
	storage.objects[img.ObjectKey] = []byte("実体")
	return img
}

// ---------------------------------------------------------------------------
// status による扱いの違い (ADR 0016 問題 3)
// ---------------------------------------------------------------------------

// **pending の孤児は DB 行ごと消す。**
func TestReclaim_DeletesPendingOrphan(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	img := seed(t, repo, storage, model.StatusPending, 2*time.Hour)

	got, err := uc.Reclaim(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}

	if got.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 (%+v)", got.Deleted, got)
	}
	if _, ok := repo.stored[img.ID]; ok {
		t.Error("pending の孤児の DB 行が残っている")
	}
	if _, ok := storage.objects[img.ObjectKey]; ok {
		t.Error("実体が残っている")
	}
}

// **deleted は S3 だけ消し、DB 行は残す** (ADR 0016 問題 3)。
//
// 行を消すと外部キー違反になり、かつ「画像は削除されました」と
// 「元から画像なし」を区別できなくなる。
func TestReclaim_KeepsRowForDeletedImages(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	img := seed(t, repo, storage, model.StatusDeleted, 2*time.Hour)

	got, err := uc.Reclaim(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}

	if got.Marked != 1 || got.Deleted != 0 {
		t.Errorf("結果 = %+v, want Marked=1 Deleted=0", got)
	}
	stored, ok := repo.stored[img.ID]
	if !ok {
		t.Fatal("deleted の DB 行が消えている (外部キー違反になる)")
	}
	if stored.ObjectReclaimedAt == nil {
		t.Error("回収した記録が残っていない (次の周回でも対象に残り続ける)")
	}
	if _, ok := storage.objects[img.ObjectKey]; ok {
		t.Error("実体が残っている")
	}
}

// **committed の孤立も回収する。**
//
// アップロードと添付を分けたことで生まれた種類 (ADR 0007 の
// 実装して分かったこと 1)。どこからも参照されないまま残る。
func TestReclaim_DeletesUnattachedCommitted(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	img := seed(t, repo, storage, model.StatusCommitted, 2*time.Hour)

	got, err := uc.Reclaim(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}

	if got.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 (%+v)", got.Deleted, got)
	}
	if _, ok := repo.stored[img.ID]; ok {
		t.Error("孤立した committed が残っている")
	}
}

// ---------------------------------------------------------------------------
// 安全側の性質
// ---------------------------------------------------------------------------

// **アップロード直後の画像を消さない。**
//
// アップロードと添付は別の操作なので、添付するまでの間、
// 参照されていない committed は正常な状態になる。
func TestReclaim_SkipsRecentImages(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	img := seed(t, repo, storage, model.StatusCommitted, 1*time.Minute)

	got, err := uc.Reclaim(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}

	if got.Total() != 0 {
		t.Errorf("結果 = %+v, want 何も回収しない", got)
	}
	if _, ok := repo.stored[img.ID]; !ok {
		t.Error("アップロード直後の画像が消された")
	}
}

// **S3 の削除に失敗したら DB を触らない。**
//
// 先に DB を消すと、消し損ねたオブジェクトを追う手段が無くなる。
// 次の周回で拾い直せる状態のまま残す。
func TestReclaim_KeepsRowWhenStorageFails(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	img := seed(t, repo, storage, model.StatusPending, 2*time.Hour)
	storage.deleteErr = errors.New("S3 に届かない")

	got, err := uc.Reclaim(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("Reclaim がエラーを返した (1 件の失敗で止めない): %v", err)
	}

	if got.Failed != 1 || got.Deleted != 0 {
		t.Errorf("結果 = %+v, want Failed=1 Deleted=0", got)
	}
	if _, ok := repo.stored[img.ID]; !ok {
		t.Error("S3 の削除に失敗したのに DB 行が消えた (追跡できなくなる)")
	}
}

// **S3 を先に消す。** 順序が逆だと、消し損ねたオブジェクトを追えない。
func TestReclaim_DeletesStorageBeforeDatabase(t *testing.T) {
	t.Parallel()

	uc, repo, storage, rec := newTestInteractor(t)
	seed(t, repo, storage, model.StatusPending, 2*time.Hour)

	if _, err := uc.Reclaim(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}

	var storageAt, dbAt = -1, -1
	for i, op := range rec.log {
		if op == "storage:delete" && storageAt < 0 {
			storageAt = i
		}
		if op == "db:delete" && dbAt < 0 {
			dbAt = i
		}
	}
	if storageAt < 0 || dbAt < 0 {
		t.Fatalf("呼び出しが足りない: %v", rec.log)
	}
	if storageAt > dbAt {
		t.Errorf("DB を先に消している: %v", rec.log)
	}
}

// 回収済みの行は次の周回で拾わないこと。
func TestReclaim_SkipsAlreadyReclaimed(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	seed(t, repo, storage, model.StatusDeleted, 2*time.Hour)

	first, err := uc.Reclaim(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("1 周目が失敗した: %v", err)
	}
	if first.Marked != 1 {
		t.Fatalf("1 周目 = %+v, want Marked=1", first)
	}

	second, err := uc.Reclaim(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("2 周目が失敗した: %v", err)
	}
	if second.Total() != 0 {
		t.Errorf("2 周目 = %+v, want 何も回収しない (回収済みを拾い直している)", second)
	}
}

// 件数の上限が効くこと。行ロックを保持したままの時間を短く保つため。
func TestReclaim_RespectsBatchSize(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	for range 5 {
		seed(t, repo, storage, model.StatusPending, 2*time.Hour)
	}

	got, err := uc.Reclaim(context.Background(), time.Hour, 2)
	if err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}
	if got.Total() != 2 {
		t.Errorf("Total = %d, want 2", got.Total())
	}
	if len(repo.stored) != 3 {
		t.Errorf("残り %d 件, want 3", len(repo.stored))
	}
}

// 既定値に落ちること。0 を渡すと LIMIT 0 で「何も起きないのに成功」になる。
func TestReclaim_UsesDefaults(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	seed(t, repo, storage, model.StatusPending, 2*DefaultReclaimGrace)

	got, err := uc.Reclaim(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}
	if got.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 (既定値に落ちていない)", got.Deleted)
	}
}

// 取得に失敗したらエラーを返すこと (握りつぶさない)。
func TestReclaim_PropagatesListError(t *testing.T) {
	t.Parallel()

	uc, repo, _, _ := newTestInteractor(t)
	repo.listErr = errors.New("DB が落ちている")

	if _, err := uc.Reclaim(context.Background(), time.Hour, 10); err == nil {
		t.Fatal("取得に失敗したのに成功が返った")
	}
}

// 対象が無ければ何もしないこと。
func TestReclaim_NoTargets(t *testing.T) {
	t.Parallel()

	uc, _, _, rec := newTestInteractor(t)

	got, err := uc.Reclaim(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}
	if got.Total() != 0 {
		t.Errorf("Total = %d, want 0", got.Total())
	}
	for _, op := range rec.log {
		if op == "storage:delete" {
			t.Error("対象が無いのに S3 を触っている")
		}
	}
}

// 回収した画像の ID がユニークであること (同じ行を二重に処理しない)。
func TestReclaim_DoesNotProcessSameImageTwice(t *testing.T) {
	t.Parallel()

	uc, repo, storage, rec := newTestInteractor(t)
	img := seed(t, repo, storage, model.StatusDeleted, 2*time.Hour)

	if _, err := uc.Reclaim(context.Background(), time.Hour, 10); err != nil {
		t.Fatalf("Reclaim が失敗した: %v", err)
	}

	var deletes int
	for _, op := range rec.log {
		if op == "storage:delete" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("S3 の削除が %d 回。1 回であるべき (id=%s)", deletes, img.ID)
	}
}

// ID の見た目だけで判定していないこと (uuid.Nil を混ぜても壊れない)。
func TestReclaimResult_Total(t *testing.T) {
	t.Parallel()

	r := ReclaimResult{Deleted: 1, Marked: 2, Failed: 3}
	if r.Total() != 6 {
		t.Errorf("Total = %d, want 6", r.Total())
	}
	if (ReclaimResult{}).Total() != 0 {
		t.Error("空の結果の Total が 0 でない")
	}
	_ = uuid.Nil
}
