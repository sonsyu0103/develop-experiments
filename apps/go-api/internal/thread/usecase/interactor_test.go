package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/pagination"
	"develop-experiments/apps/go-api/internal/thread/domain/model"
)

// mustPage は「この id より小さい行を size 件」というページ指定を組み立てます。
// カーソルは API 上は不透明トークンなので、テストからも id を直接は渡せません。
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

// wantNextCursor は次ページ用トークンが指す id を検査します。
func wantNextCursor(t *testing.T, token *string, wantID int64) {
	t.Helper()

	if token == nil {
		t.Fatalf("NextCursor = nil, want id=%d を指すトークン", wantID)
	}
	c, err := pagination.DecodeCursor(*token)
	if err != nil {
		t.Fatalf("NextCursor が復号できない (%q): %v", *token, err)
	}
	if c.ID != wantID {
		t.Errorf("NextCursor が指す ID = %d, want %d", c.ID, wantID)
	}
}

func TestThreadInteractor_FetchThreadList(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo(5)
	uc := NewThreadInteractor(repo)

	got, err := uc.FetchThreadList(context.Background(), mustPage(t, nil, 3))
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}

	if len(got.Threads) != 3 {
		t.Fatalf("件数 = %d, want 3", len(got.Threads))
	}
	// 新しい順 (ID 降順) であること。
	if got.Threads[0].ID != 5 || got.Threads[2].ID != 3 {
		t.Errorf("並び順が想定と違う: %+v", got.Threads)
	}
	// コメント数は集計クエリの結果がそのまま入る。
	if got.Threads[0].CommentCount != 15 {
		t.Errorf("CommentCount = %d, want 15", got.Threads[0].CommentCount)
	}
	// size ちょうど返ったので、次ページのカーソルが立つ。
	wantNextCursor(t, got.NextCursor, 3)

	// 集計に N+1 を使っていないことの確認。
	// 本命の経路では CountComments が一度も呼ばれてはいけない。
	if n := repo.countCalls.Load(); n != 0 {
		t.Errorf("CountComments の呼び出し回数 = %d, want 0 (単一クエリで集計するため)", n)
	}
}

func TestThreadInteractor_FetchThreadList_NoCursorOnLastPage(t *testing.T) {
	t.Parallel()

	uc := NewThreadInteractor(newFakeRepo(2))

	got, err := uc.FetchThreadList(context.Background(), mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	if len(got.Threads) != 2 {
		t.Fatalf("件数 = %d, want 2", len(got.Threads))
	}
	if got.NextCursor != nil {
		t.Errorf("NextCursor = %v, want nil", *got.NextCursor)
	}
}

func TestThreadInteractor_FetchThreadList_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	uc := NewThreadInteractor(newFakeRepo(0))

	got, err := uc.FetchThreadList(context.Background(), mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	// nil スライスだと JSON が null になり、フロントで .map() が落ちる。
	if got.Threads == nil {
		t.Error("Threads = nil, want 空スライス (JSON で null にしないため)")
	}
	if len(got.Threads) != 0 {
		t.Errorf("件数 = %d, want 0", len(got.Threads))
	}
}

func TestThreadInteractor_FetchThreadList_WithCursor(t *testing.T) {
	t.Parallel()

	uc := NewThreadInteractor(newFakeRepo(5))

	cursor := int64(4)
	got, err := uc.FetchThreadList(context.Background(), mustPage(t, &cursor, 10))
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	// cursor より小さい ID だけが返る (cursor 自身は含まない)。
	if len(got.Threads) != 3 {
		t.Fatalf("件数 = %d, want 3", len(got.Threads))
	}
	if got.Threads[0].ID != 3 {
		t.Errorf("先頭の ID = %d, want 3", got.Threads[0].ID)
	}
}

func TestThreadInteractor_FetchThreadList_PropagatesRepositoryError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("DB がダウンしています")
	repo := newFakeRepo(3)
	repo.listErr = sentinel

	_, err := NewThreadInteractor(repo).FetchThreadList(context.Background(), mustPage(t, nil, 10))
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

func TestThreadInteractor_FetchThread(t *testing.T) {
	t.Parallel()

	uc := NewThreadInteractor(newFakeRepo(3))

	got, err := uc.FetchThread(context.Background(), 2)
	if err != nil {
		t.Fatalf("FetchThread が失敗した: %v", err)
	}
	if got.ID != 2 || got.CommentCount != 6 {
		t.Errorf("got = %+v, want ID=2 CommentCount=6", got)
	}
}

func TestThreadInteractor_FetchThread_NotFound(t *testing.T) {
	t.Parallel()

	uc := NewThreadInteractor(newFakeRepo(3))

	_, err := uc.FetchThread(context.Background(), 999)
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("err = %v, want apperr.ErrNotFound", err)
	}
}

func TestThreadInteractor_CreateThread(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo(0)
	uc := NewThreadInteractor(repo)

	got, err := uc.CreateThread(context.Background(), "  新しいスレッド  ", nil)
	if err != nil {
		t.Fatalf("CreateThread が失敗した: %v", err)
	}
	// ドメイン層で TrimSpace されていること。
	if got.Title != "新しいスレッド" {
		t.Errorf("Title = %q, want %q", got.Title, "新しいスレッド")
	}
	if got.CommentCount != 0 {
		t.Errorf("CommentCount = %d, want 0", got.CommentCount)
	}
}

func TestThreadInteractor_CreateThread_ValidationStopsBeforeRepository(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo(0)
	uc := NewThreadInteractor(repo)

	cases := map[string]string{
		"空文字":    "",
		"空白のみ":   "   ",
		"長さ上限超過": strings.Repeat("あ", model.TitleMaxLength+1),
	}

	for name, title := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := uc.CreateThread(context.Background(), title, nil)
			if !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
			}
		})
	}

	if len(repo.createdThreads) != 0 {
		t.Errorf("リポジトリが %d 回呼ばれた, want 0 (検証で弾くため)", len(repo.createdThreads))
	}
}
