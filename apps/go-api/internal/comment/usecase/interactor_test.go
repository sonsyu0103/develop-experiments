package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/comment/domain/repository"
	"develop-experiments/apps/go-api/internal/pagination"
)

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

// fakeCommentRepo は id 降順で並んだコメントを保持し、
// ListByThreadID でカーソルと件数の絞り込みだけを再現します。
type fakeCommentRepo struct {
	comments []model.Comment
	listErr  error

	// 受け取ったページ指定。カーソルが永続化層まで届いたかの確認に使う。
	gotPage pagination.Page
}

var _ repository.CommentRepository = (*fakeCommentRepo)(nil)

func newFakeCommentRepo(n int) *fakeCommentRepo {
	// id が大きい順に並べる (実際のクエリが ORDER BY id DESC のため)。
	comments := make([]model.Comment, 0, n)
	for id := int64(n); id >= 1; id-- {
		comments = append(comments, model.Comment{
			ID:         id,
			ThreadID:   1,
			AuthorName: "名無しさん",
			Body:       "本文",
			CreatedAt:  time.Unix(0, 0).UTC(),
		})
	}
	return &fakeCommentRepo{comments: comments}
}

func (f *fakeCommentRepo) ListByThreadID(
	_ context.Context, threadID int64, page pagination.Page,
) ([]model.Comment, error) {
	f.gotPage = page
	if f.listErr != nil {
		return nil, f.listErr
	}

	out := make([]model.Comment, 0, len(f.comments))
	for _, c := range f.comments {
		if c.ThreadID != threadID {
			continue
		}
		if cursorID := page.CursorID(); cursorID != nil && c.ID >= *cursorID {
			continue
		}
		out = append(out, c)
		if int32(len(out)) == page.Size {
			break
		}
	}
	return out, nil
}

func (f *fakeCommentRepo) Create(_ context.Context, comment *model.Comment) (*model.Comment, error) {
	created := *comment
	created.ID = int64(len(f.comments) + 1)
	created.CreatedAt = time.Unix(0, 0).UTC()
	return &created, nil
}

func (f *fakeCommentRepo) SoftDelete(context.Context, int64, int64) error { return nil }

type fakeThreadChecker struct {
	exists bool
	err    error
}

var _ repository.ThreadExistenceChecker = (*fakeThreadChecker)(nil)

func (f *fakeThreadChecker) Exists(context.Context, int64) (bool, error) {
	return f.exists, f.err
}

func newInteractor(repo *fakeCommentRepo) *CommentInteractor {
	return NewCommentInteractor(repo, &fakeThreadChecker{exists: true})
}

func mustPage(t *testing.T, cursorID *int64, size int32) pagination.Page {
	t.Helper()

	var token *string
	if cursorID != nil {
		s, err := pagination.NewCursor(*cursorID).Encode()
		if err != nil {
			t.Fatalf("カーソルの符号化が失敗した: %v", err)
		}
		token = &s
	}

	p, err := pagination.NewPage(token, size)
	if err != nil {
		t.Fatalf("pagination.NewPage が失敗した: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// ページ送り
// ---------------------------------------------------------------------------

// 次ページのカーソルは「最後に返した行」を指す必要があります。
//
// コメントは id 降順で返るため、先頭 (最大 id) を指してしまうと
// 2 ページ目が 1 ページ目と同じ内容になり、ページ送りが止まらなくなります。
// 型が同じ int64 なので、取り違えてもコンパイルは通ります。
func TestFetchComments_NextCursorPointsAtLastRow(t *testing.T) {
	t.Parallel()

	uc := newInteractor(newFakeCommentRepo(10))

	got, err := uc.FetchComments(context.Background(), 1, mustPage(t, nil, 3))
	if err != nil {
		t.Fatalf("FetchComments が失敗した: %v", err)
	}

	if len(got.Comments) != 3 {
		t.Fatalf("件数 = %d, want 3", len(got.Comments))
	}
	if got.Comments[0].ID != 10 || got.Comments[2].ID != 8 {
		t.Fatalf("並び順が想定と違う: %+v", got.Comments)
	}

	if got.NextCursor == nil {
		t.Fatal("NextCursor = nil, want トークン")
	}
	c, err := pagination.DecodeCursor(*got.NextCursor)
	if err != nil {
		t.Fatalf("NextCursor が復号できない (%q): %v", *got.NextCursor, err)
	}
	if c.ID != 8 {
		t.Errorf("NextCursor が指す ID = %d, want 8 (最後に返した行)", c.ID)
	}
}

// 実際に 2 ページ目を引いて、1 ページ目と重ならないことを確認します。
// カーソルの向き (id < cursor) が逆になっていると、ここで気づけます。
func TestFetchComments_SecondPageDoesNotOverlap(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(10)
	uc := newInteractor(repo)
	ctx := context.Background()

	page1, err := uc.FetchComments(ctx, 1, mustPage(t, nil, 3))
	if err != nil {
		t.Fatalf("1 ページ目の取得に失敗した: %v", err)
	}

	next, err := pagination.DecodeCursor(*page1.NextCursor)
	if err != nil {
		t.Fatalf("NextCursor が復号できない: %v", err)
	}

	page2, err := uc.FetchComments(ctx, 1, mustPage(t, &next.ID, 3))
	if err != nil {
		t.Fatalf("2 ページ目の取得に失敗した: %v", err)
	}

	ids1 := idsOf(page1.Comments)
	ids2 := idsOf(page2.Comments)
	if ids2[0] != 7 {
		t.Errorf("2 ページ目の先頭 = %d, want 7", ids2[0])
	}
	for _, a := range ids1 {
		for _, b := range ids2 {
			if a == b {
				t.Fatalf("ページ間で ID %d が重複した (1: %v, 2: %v)", a, ids1, ids2)
			}
		}
	}
}

func TestFetchComments_NoCursorOnLastPage(t *testing.T) {
	t.Parallel()

	uc := newInteractor(newFakeCommentRepo(2))

	got, err := uc.FetchComments(context.Background(), 1, mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchComments が失敗した: %v", err)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("件数 = %d, want 2", len(got.Comments))
	}
	if got.NextCursor != nil {
		t.Errorf("NextCursor = %q, want nil", *got.NextCursor)
	}
}

// カーソルは永続化層まで id として届く必要があります。
// ここが nil のままだと、常に先頭ページが返り続けます。
func TestFetchComments_CursorReachesRepository(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(10)
	uc := newInteractor(repo)

	cursorID := int64(8)
	if _, err := uc.FetchComments(context.Background(), 1, mustPage(t, &cursorID, 3)); err != nil {
		t.Fatalf("FetchComments が失敗した: %v", err)
	}

	gotID := repo.gotPage.CursorID()
	if gotID == nil || *gotID != 8 {
		t.Errorf("リポジトリが受け取ったカーソル = %v, want 8", gotID)
	}
}

func TestFetchComments_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	uc := newInteractor(newFakeCommentRepo(0))

	got, err := uc.FetchComments(context.Background(), 1, mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchComments が失敗した: %v", err)
	}
	// nil スライスだと JSON が null になり、フロントで .map() が落ちる。
	if got.Comments == nil {
		t.Error("Comments = nil, want 空スライス (JSON で null にしないため)")
	}
}

// ---------------------------------------------------------------------------
// 存在確認とエラーの伝播
// ---------------------------------------------------------------------------

// 存在しないスレッドは「コメント 0 件」ではなく 404 相当になります。
func TestFetchComments_ThreadNotFound(t *testing.T) {
	t.Parallel()

	uc := NewCommentInteractor(newFakeCommentRepo(3), &fakeThreadChecker{exists: false})

	_, err := uc.FetchComments(context.Background(), 999, mustPage(t, nil, 10))
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("err = %v, want apperr.ErrNotFound", err)
	}
}

func TestFetchComments_PropagatesRepositoryError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("DB がダウンしています")
	repo := newFakeCommentRepo(3)
	repo.listErr = sentinel

	_, err := newInteractor(repo).FetchComments(context.Background(), 1, mustPage(t, nil, 10))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func idsOf(dtos []CommentDTO) []int64 {
	ids := make([]int64, 0, len(dtos))
	for _, d := range dtos {
		ids = append(ids, d.ID)
	}
	return ids
}
