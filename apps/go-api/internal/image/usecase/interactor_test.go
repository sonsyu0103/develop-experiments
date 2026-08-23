package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/image/domain/model"
	"develop-experiments/apps/go-api/internal/image/domain/repository"
)

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

// fakeImageRepo と fakeStorage は**呼ばれた順序**を共有の log に記録します。
// ADR 0007 決定 3 が定めているのは順序そのものなので、
// 「何が呼ばれたか」だけでは検証になりません。
type recorder struct{ log []string }

func (r *recorder) add(op string) { r.log = append(r.log, op) }

type fakeImageRepo struct {
	rec       *recorder
	stored    map[uuid.UUID]*model.Image
	createErr error
	commitErr error
	findErr   error
	listErr   error
	// afterReserve は WithinTx が成功して抜けた直後に呼ばれます。
	// 「確保をコミットした直後に SIGTERM が来た」を作るための穴です。
	afterReserve func()
}

var _ repository.ImageRepository = (*fakeImageRepo)(nil)

func newFakeImageRepo(rec *recorder) *fakeImageRepo {
	return &fakeImageRepo{rec: rec, stored: map[uuid.UUID]*model.Image{}}
}

func (f *fakeImageRepo) CreatePending(_ context.Context, img *model.Image) (*model.Image, error) {
	f.rec.add("db:create_pending")
	if f.createErr != nil {
		return nil, f.createErr
	}
	cp := *img
	f.stored[cp.ID] = &cp
	return &cp, nil
}

func (f *fakeImageRepo) Commit(_ context.Context, id uuid.UUID) (*model.Image, error) {
	f.rec.add("db:commit")
	if f.commitErr != nil {
		return nil, f.commitErr
	}
	img, ok := f.stored[id]
	if !ok {
		return nil, apperr.ErrNotFound
	}
	img.Status = model.StatusCommitted
	return img, nil
}

func (f *fakeImageRepo) FindByID(_ context.Context, id uuid.UUID) (*model.Image, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	img, ok := f.stored[id]
	if !ok {
		return nil, apperr.ErrNotFound
	}
	return img, nil
}

// 回収バッチ用。トランザクションは張らず、そのまま自分を渡します
// (フェイクなので「同じトランザクション」を再現する必要がない)。
func (f *fakeImageRepo) WithinTx(_ context.Context, fn func(repository.ImageRepository) error) error {
	if err := fn(f); err != nil {
		return err
	}
	if f.afterReserve != nil {
		f.afterReserve()
	}
	return nil
}

func (f *fakeImageRepo) ListReclaimable(
	_ context.Context, grace time.Duration, maxRows int32,
) ([]model.Image, error) {
	f.rec.add("db:list_reclaimable")
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []model.Image
	for _, img := range f.stored {
		if len(out) >= int(maxRows) {
			break
		}
		if img.ObjectReclaimedAt != nil {
			continue
		}
		// grace は「作られてからの経過」。フェイクでは createdAt を
		// 明示的に古くした行だけを対象にする。
		if !img.CreatedAt.Before(time.Now().Add(-grace)) {
			continue
		}
		out = append(out, *img)
	}
	return out, nil
}

func (f *fakeImageRepo) MarkReclaimed(ctx context.Context, id uuid.UUID) error {
	f.rec.add("db:mark_reclaimed")
	if err := ctx.Err(); err != nil {
		return err
	}
	if img, ok := f.stored[id]; ok {
		now := time.Now()
		img.ObjectReclaimedAt = &now
	}
	return nil
}

// **ctx を見ます。** pgx はキャンセル済みの ctx で必ず失敗するので、
// 無視するフェイクだと「シャットダウン中に消せていたつもり」を再現できません
// (レビュー指摘 —— 実際にそこが穴になっていました)。
func (f *fakeImageRepo) Delete(ctx context.Context, id uuid.UUID) error {
	f.rec.add("db:delete")
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(f.stored, id)
	return nil
}

type fakeStorage struct {
	rec       *recorder
	putErr    error
	deleteErr error
	// objects は key -> バイト列。
	objects map[string][]byte
}

var _ repository.ObjectStorage = (*fakeStorage)(nil)

func newFakeStorage(rec *recorder) *fakeStorage {
	return &fakeStorage{rec: rec, objects: map[string][]byte{}}
}

func (f *fakeStorage) Put(_ context.Context, key, _ string, body []byte) error {
	f.rec.add("storage:put")
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[key] = body
	return nil
}

// ctx を見ます (Delete と同じ理由。AWS SDK もキャンセルで失敗します)。
func (f *fakeStorage) Delete(ctx context.Context, key string) error {
	f.rec.add("storage:delete")
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.objects, key)
	return nil
}

func (f *fakeStorage) URL(key string) string { return "https://cdn.example.test/" + key }

func newTestInteractor(t *testing.T) (*ImageInteractor, *fakeImageRepo, *fakeStorage, *recorder) {
	t.Helper()

	rec := &recorder{}
	repo := newFakeImageRepo(rec)
	storage := newFakeStorage(rec)
	return NewImageInteractor(repo, storage, nil), repo, storage, rec
}

// ---------------------------------------------------------------------------
// 順序 (ADR 0007 決定 3)
// ---------------------------------------------------------------------------

// **DB を先に書き、そのあとストレージへ書き、最後に確定させる。**
//
// 逆順 (ストレージが先) にすると、DB に記録の無いオブジェクトが残り、
// 全件リストと突き合わせないと見つけられなくなる。
func TestUpload_WritesDatabaseBeforeStorage(t *testing.T) {
	t.Parallel()

	uc, _, _, rec := newTestInteractor(t)

	if _, err := uc.Upload(context.Background(), 42, "comment_attachment", makePNG(t, 64, 64)); err != nil {
		t.Fatalf("Upload が失敗した: %v", err)
	}

	want := []string{"db:create_pending", "storage:put", "db:commit"}
	if len(rec.log) != len(want) {
		t.Fatalf("呼び出し = %v, want %v", rec.log, want)
	}
	for i := range want {
		if rec.log[i] != want[i] {
			t.Fatalf("呼び出し = %v, want %v", rec.log, want)
		}
	}
}

// **PUT に失敗しても pending の行を消さない** (ADR 0007 決定 3)。
//
// 消すと、PUT が実は成功していた場合に「DB に記録の無いオブジェクト」が残る。
// 残しておけば回収バッチが両方を消せる。
func TestUpload_KeepsPendingRowWhenStorageFails(t *testing.T) {
	t.Parallel()

	uc, repo, storage, rec := newTestInteractor(t)
	storage.putErr = errors.New("S3 に届かない")

	if _, err := uc.Upload(context.Background(), 42, "comment_attachment", makePNG(t, 64, 64)); err == nil {
		t.Fatal("PUT が失敗したのに成功が返った")
	}

	if len(repo.stored) != 1 {
		t.Fatalf("pending の行が %d 件。1 件残っているべき", len(repo.stored))
	}
	for _, img := range repo.stored {
		if img.Status != model.StatusPending {
			t.Errorf("status = %q, want pending", img.Status)
		}
	}
	// **後始末で削除を呼ばないこと。**
	for _, op := range rec.log {
		if op == "storage:delete" {
			t.Error("PUT の失敗時に Delete を呼んでいる (回収バッチの仕事)")
		}
	}
}

// 確定に失敗した場合も pending のまま残る。
// オブジェクトと行が対で残るので、回収バッチが両方を消せる。
func TestUpload_KeepsPendingRowWhenCommitFails(t *testing.T) {
	t.Parallel()

	uc, repo, _, _ := newTestInteractor(t)
	repo.commitErr = errors.New("DB が落ちている")

	if _, err := uc.Upload(context.Background(), 42, "comment_attachment", makePNG(t, 64, 64)); err == nil {
		t.Fatal("確定が失敗したのに成功が返った")
	}
	for _, img := range repo.stored {
		if img.Status != model.StatusPending {
			t.Errorf("status = %q, want pending", img.Status)
		}
	}
}

// **DB の書き込みに失敗したら、ストレージには触らない。**
// 触ると、記録の無いオブジェクトが生まれる。
func TestUpload_DoesNotTouchStorageWhenDatabaseFails(t *testing.T) {
	t.Parallel()

	uc, repo, storage, _ := newTestInteractor(t)
	repo.createErr = errors.New("DB が落ちている")

	if _, err := uc.Upload(context.Background(), 42, "comment_attachment", makePNG(t, 64, 64)); err == nil {
		t.Fatal("DB が失敗したのに成功が返った")
	}
	if len(storage.objects) != 0 {
		t.Errorf("ストレージに %d 件書かれた。0 件であるべき", len(storage.objects))
	}
}

// 成功時は、保存されたバイト列が**再エンコード後のもの**であること。
func TestUpload_StoresReencodedBytes(t *testing.T) {
	t.Parallel()

	uc, _, storage, _ := newTestInteractor(t)
	src := makePNG(t, 64, 64)

	dto, err := uc.Upload(context.Background(), 42, "comment_attachment", src)
	if err != nil {
		t.Fatalf("Upload が失敗した: %v", err)
	}

	if len(storage.objects) != 1 {
		t.Fatalf("ストレージの件数 = %d, want 1", len(storage.objects))
	}
	for key, body := range storage.objects {
		if string(body) == string(src) {
			t.Error("入力のバイト列がそのまま保存されている")
		}
		// キーは UUID + 拡張子。コメント添付なので .jpg。
		if want := "images/" + dto.ID.String() + ".jpg"; key != want {
			t.Errorf("key = %q, want %q", key, want)
		}
	}
	// URL は絶対 URL (ADR 0007 決定 5)。
	if dto.URL == "" || dto.URL[:5] != "https" {
		t.Errorf("URL = %q, want 絶対 URL", dto.URL)
	}
}

// ---------------------------------------------------------------------------
// 用途の検証
// ---------------------------------------------------------------------------

func TestParseKind(t *testing.T) {
	t.Parallel()

	// **3 つの用途をすべて受け付ける** (PR 4 で開けた)。
	for _, raw := range []string{"comment_attachment", "avatar", "thread_icon"} {
		if _, err := ParseKind(raw); err != nil {
			t.Errorf("kind=%q が拒否された: %v", raw, err)
		}
	}

	// 未知の値は受け付けない。
	for _, raw := range []string{"", "banner", "COMMENT_ATTACHMENT", " avatar"} {
		if _, err := ParseKind(raw); !errors.Is(err, apperr.ErrInvalidArgument) {
			t.Errorf("kind=%q: err = %v, want apperr.ErrInvalidArgument", raw, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 所有者の確認
// ---------------------------------------------------------------------------

// **他人の画像は 404。** 403 を返すと「その ID の画像が存在すること」が漏れる
// (docs/adr/0013-http-defense.md)。
func TestEnsureOwned(t *testing.T) {
	t.Parallel()

	uc, repo, _, _ := newTestInteractor(t)

	dto, err := uc.Upload(context.Background(), 42, "comment_attachment", makePNG(t, 32, 32))
	if err != nil {
		t.Fatalf("Upload が失敗した: %v", err)
	}

	t.Run("所有者なら通る", func(t *testing.T) {
		if err := uc.EnsureOwned(context.Background(), 42, dto.ID, "comment_attachment"); err != nil {
			t.Errorf("所有者なのに拒否された: %v", err)
		}
	})

	t.Run("他人は 404", func(t *testing.T) {
		if err := uc.EnsureOwned(context.Background(), 99, dto.ID, "comment_attachment"); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("err = %v, want apperr.ErrNotFound", err)
		}
	})

	t.Run("存在しない画像も 404", func(t *testing.T) {
		if err := uc.EnsureOwned(context.Background(), 42, uuid.New(), "comment_attachment"); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("err = %v, want apperr.ErrNotFound", err)
		}
	})

	// **用途が違う画像は添付できない** (ADR 0007 決定 6)。
	// コメント添付として上げた JPEG をアバターに使えると、
	// 仕様書の説明と実装が食い違う。
	t.Run("用途が違うと 404", func(t *testing.T) {
		if err := uc.EnsureOwned(context.Background(), 42, dto.ID, "avatar"); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("err = %v, want apperr.ErrNotFound", err)
		}
	})

	// **回収が始まった画像は添付できない。**
	// 確保のあとは実体が消えている可能性がある。
	t.Run("回収済みは 404", func(t *testing.T) {
		for _, img := range repo.stored {
			now := time.Now()
			img.ObjectReclaimedAt = &now
		}
		err := uc.EnsureOwned(context.Background(), 42, dto.ID, "comment_attachment")
		if !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("err = %v, want apperr.ErrNotFound", err)
		}
		for _, img := range repo.stored {
			img.ObjectReclaimedAt = nil
		}
	})

	t.Run("確定していない画像は添付できない", func(t *testing.T) {
		// **ストレージにバイト列が無い可能性がある。**
		// 添付できてしまうと、表示時に 404 になる画像が投稿に残る。
		for _, img := range repo.stored {
			img.Status = model.StatusPending
		}
		if err := uc.EnsureOwned(context.Background(), 42, dto.ID, "comment_attachment"); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("err = %v, want apperr.ErrNotFound", err)
		}
	})
}

// 中断されたコンテキストではデコードを始めないこと。
func TestUpload_RespectsCanceledContext(t *testing.T) {
	t.Parallel()

	uc, _, _, rec := newTestInteractor(t)

	// スロットを使い切ってから、キャンセル済みの ctx で呼ぶ。
	for range defaultMaxConcurrentDecodes {
		uc.decodeSlots <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := uc.Upload(ctx, 42, "comment_attachment", makePNG(t, 32, 32)); err == nil {
		t.Fatal("中断済みなのに成功した")
	}
	if len(rec.log) != 0 {
		t.Errorf("中断済みなのに %v が呼ばれた", rec.log)
	}
}

// **同時デコードの本数 × 1 本の見積もりが、予算に収まること。**
//
// 直す前のコメントは「2500 万画素は 1 枚 95 MB、4 本で約 380 MB」だったが、
// **実測は 1 本 456 MB / 4 本 1821 MB** で、約 4.8 倍ずれていた。
// 16bit PNG が 8 バイト/画素でデコードされること、縮小の一時バッファ
// (dw x sh x [4]float64) が入っていないことの 2 つを見落としていた。
//
// **算術をコメントに置いておくと、ずれても誰も落とせない。**
// 本数・画素数・長辺のどれを上げてもここが落ちる。
func TestDecodeConcurrency_FitsMemoryBudget(t *testing.T) {
	t.Parallel()

	// 長辺がいちばん大きい用途で見る (コメント添付 = 1600)。
	dstLong := int64(model.KindCommentAttachment.MaxDimension())
	perDecode := estimatedPeakBytesPerDecode(model.MaxPixels, dstLong)
	total := perDecode * defaultMaxConcurrentDecodes

	t.Logf("1 本 %.0f MB x %d 本 = %.0f MB (予算 %.0f MB)",
		float64(perDecode)/(1<<20), defaultMaxConcurrentDecodes,
		float64(total)/(1<<20), float64(decodeMemoryBudget)/(1<<20))

	if total > decodeMemoryBudget {
		t.Errorf("同時 %d 本で %.0f MB になり、予算 %.0f MB を超える "+
			"(本数を減らすか、MaxPixels か長辺を下げる)",
			defaultMaxConcurrentDecodes,
			float64(total)/(1<<20), float64(decodeMemoryBudget)/(1<<20))
	}
}

// **見積もりの式が実測から離れていないこと。**
//
// 実測はテストの中では取らない —— 456 MB を CI で確保するのは高いうえ、
// -race だとさらに増える。代わりに**実測値をここに固定**して、
// 式やライブラリが変わったときに「測り直せ」と言わせる。
//
//	実測 (5000x5000 の 16bit PNG、長辺 1600): 456 MB
func TestEstimatedPeak_MatchesMeasurement(t *testing.T) {
	t.Parallel()

	const measuredMB = 456.0
	got := float64(estimatedPeakBytesPerDecode(25_000_000, 1600)) / (1 << 20)

	if diff := got - measuredMB; diff < -measuredMB*0.15 || diff > measuredMB*0.15 {
		t.Errorf("見積もり %.0f MB は実測 %.0f MB から 15%% 以上ずれている "+
			"(式かライブラリが変わった。測り直すこと)", got, measuredMB)
	}
}
