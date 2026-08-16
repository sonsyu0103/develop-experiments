package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	uc := NewThreadInteractor(repo, nil)

	got, err := uc.FetchThreadList(context.Background(), mustPage(t, nil, 3), nil, nil)
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

	uc := NewThreadInteractor(newFakeRepo(2), nil)

	got, err := uc.FetchThreadList(context.Background(), mustPage(t, nil, 10), nil, nil)
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

	uc := NewThreadInteractor(newFakeRepo(0), nil)

	got, err := uc.FetchThreadList(context.Background(), mustPage(t, nil, 10), nil, nil)
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

	uc := NewThreadInteractor(newFakeRepo(5), nil)

	cursor := int64(4)
	got, err := uc.FetchThreadList(context.Background(), mustPage(t, &cursor, 10), nil, nil)
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

	_, err := NewThreadInteractor(repo, nil).FetchThreadList(context.Background(), mustPage(t, nil, 10), nil, nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

// searchQuery は検索語を渡すためのヘルパです。
// **正規化前の生の文字列を渡します** —— ユースケースが
// model.ParseSearchQuery に委ねる形なので、テストも同じ入口を通ります。
func searchQuery(s string) *string { return &s }

func TestThreadInteractor_FetchThreadList_Search(t *testing.T) {
	t.Parallel()

	repo := newFakeRepoWithTitles("PostgreSQL の話", "Go の話", "PostgreSQL と Go")
	uc := NewThreadInteractor(repo, nil)

	got, err := uc.FetchThreadList(
		context.Background(), mustPage(t, nil, 10), searchQuery("PostgreSQL"), nil)
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}

	if len(got.Threads) != 2 {
		t.Fatalf("件数 = %d, want 2 (%+v)", len(got.Threads), got.Threads)
	}
	// 絞り込んでも新着順のまま (ADR 0012 決定 3)。
	if got.Threads[0].ID != 3 || got.Threads[1].ID != 1 {
		t.Errorf("並び順が想定と違う: %+v", got.Threads)
	}

	// **絞り込みのない一覧に落ちていないこと。**
	// 検索語を受け取りながら ListSummaries を呼ぶと、
	// 全件が返って「検索したのに絞り込まれない」になる。
	if n := repo.listCalls.Load(); n != 0 {
		t.Errorf("ListSummaries の呼び出し回数 = %d, want 0 (検索は SearchSummaries を使う)", n)
	}
	if len(repo.searchCalls) != 1 || repo.searchCalls[0] != "PostgreSQL" {
		t.Errorf("永続化層に届いた検索語 = %v, want [PostgreSQL]", repo.searchCalls)
	}
}

// **指定なしと空白だけは同じ扱い。** どちらも絞り込まない一覧に落ちます。
// 空白を落とす前に「nil かどうか」だけで分岐すると、
// q=" " が「空白 1 文字を含むタイトル」の検索になって 0 件が返ります。
func TestThreadInteractor_FetchThreadList_BlankQueryFallsBackToList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query *string
	}{
		{name: "指定なし", query: nil},
		{name: "空文字", query: searchQuery("")},
		{name: "半角スペースだけ", query: searchQuery("   ")},
		{name: "全角スペースだけ", query: searchQuery("　")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := newFakeRepoWithTitles("PostgreSQL の話", "Go の話")
			uc := NewThreadInteractor(repo, nil)

			got, err := uc.FetchThreadList(context.Background(), mustPage(t, nil, 10), tt.query, nil)
			if err != nil {
				t.Fatalf("FetchThreadList が失敗した: %v", err)
			}
			if len(got.Threads) != 2 {
				t.Fatalf("件数 = %d, want 2 (絞り込みなし)", len(got.Threads))
			}
			if len(repo.searchCalls) != 0 {
				t.Errorf("SearchSummaries が呼ばれた: %v, want 呼ばれない", repo.searchCalls)
			}
		})
	}
}

// 長すぎる検索語は 400 になること。仕様検証ミドルウェアが先に弾きますが、
// ユースケースを直接呼ぶ経路でも同じ判定になることを見ます。
func TestThreadInteractor_FetchThreadList_TooLongQuery(t *testing.T) {
	t.Parallel()

	repo := newFakeRepoWithTitles("Go の話")
	uc := NewThreadInteractor(repo, nil)

	long := strings.Repeat("あ", model.SearchQueryMaxLength+1)
	_, err := uc.FetchThreadList(context.Background(), mustPage(t, nil, 10), &long, nil)
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
	}
	// **弾いたなら DB を引かない。**
	if len(repo.searchCalls) != 0 {
		t.Errorf("SearchSummaries が呼ばれた: %v, want 呼ばれない", repo.searchCalls)
	}
}

func TestThreadInteractor_FetchThreadList_SearchPagesWithSameCursor(t *testing.T) {
	t.Parallel()

	// 一致するのは id 1・3・5 の 3 件。size 2 で切って 2 ページに分ける。
	repo := newFakeRepoWithTitles("Go 入門", "Rust 入門", "Go 中級", "Rust 中級", "Go 上級")
	uc := NewThreadInteractor(repo, nil)

	first, err := uc.FetchThreadList(
		context.Background(), mustPage(t, nil, 2), searchQuery("Go"), nil)
	if err != nil {
		t.Fatalf("1 ページ目が失敗した: %v", err)
	}
	if len(first.Threads) != 2 || first.Threads[0].ID != 5 || first.Threads[1].ID != 3 {
		t.Fatalf("1 ページ目 = %+v, want id 5, 3", first.Threads)
	}
	// **カーソルは検索結果の最後の id を指す。** 絞り込む前の id ではない。
	wantNextCursor(t, first.NextCursor, 3)

	cursor := int64(3)
	second, err := uc.FetchThreadList(
		context.Background(), mustPage(t, &cursor, 2), searchQuery("Go"), nil)
	if err != nil {
		t.Fatalf("2 ページ目が失敗した: %v", err)
	}
	if len(second.Threads) != 1 || second.Threads[0].ID != 1 {
		t.Fatalf("2 ページ目 = %+v, want id 1 のみ", second.Threads)
	}
	if second.NextCursor != nil {
		t.Errorf("NextCursor = %v, want nil (最終ページ)", *second.NextCursor)
	}
}

func TestThreadInteractor_FetchThreadList_SearchNoHit(t *testing.T) {
	t.Parallel()

	uc := NewThreadInteractor(newFakeRepoWithTitles("Go の話"), nil)

	got, err := uc.FetchThreadList(
		context.Background(), mustPage(t, nil, 10), searchQuery("見つからない語"), nil)
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	// 一覧と同じく、0 件でも nil スライスにしない (JSON が null になる)。
	if got.Threads == nil {
		t.Error("Threads = nil, want 空スライス")
	}
	if len(got.Threads) != 0 {
		t.Errorf("件数 = %d, want 0", len(got.Threads))
	}
	if got.NextCursor != nil {
		t.Errorf("NextCursor = %v, want nil", *got.NextCursor)
	}
}

func TestThreadInteractor_FetchThread(t *testing.T) {
	t.Parallel()

	uc := NewThreadInteractor(newFakeRepo(3), nil)

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

	uc := NewThreadInteractor(newFakeRepo(3), nil)

	_, err := uc.FetchThread(context.Background(), 999)
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("err = %v, want apperr.ErrNotFound", err)
	}
}

func TestThreadInteractor_CreateThread(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo(0)
	uc := NewThreadInteractor(repo, nil)

	got, err := uc.CreateThread(context.Background(), "  新しいスレッド  ", nil, nil)
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
	uc := NewThreadInteractor(repo, nil)

	cases := map[string]string{
		"空文字":    "",
		"空白のみ":   "   ",
		"長さ上限超過": strings.Repeat("あ", model.TitleMaxLength+1),
	}

	for name, title := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := uc.CreateThread(context.Background(), title, nil, nil)
			if !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
			}
		})
	}

	if len(repo.createdThreads) != 0 {
		t.Errorf("リポジトリが %d 回呼ばれた, want 0 (検証で弾くため)", len(repo.createdThreads))
	}
}

// ---------------------------------------------------------------------------
// スレッドアイコン (docs/adr/0007-image-storage.md)
// ---------------------------------------------------------------------------

// fakeIconResolver は所有権の判定を差し替えられるフェイクです。
type fakeIconResolver struct{ err error }

func (f *fakeIconResolver) EnsureOwned(context.Context, int64, uuid.UUID, string) error {
	return f.err
}
func (f *fakeIconResolver) URL(key string) string { return "https://cdn.test/" + key }

func TestCreateThread_WithIcon(t *testing.T) {
	t.Parallel()

	authorID := int64(42)
	iconID := uuid.New()

	t.Run("アイコンつきで作れる", func(t *testing.T) {
		t.Parallel()

		uc := NewThreadInteractor(newFakeRepo(0), &fakeIconResolver{})

		got, err := uc.CreateThread(context.Background(), "アイコンつき", &authorID, &iconID)
		if err != nil {
			t.Fatalf("CreateThread が失敗した: %v", err)
		}
		if got.Icon == nil {
			t.Fatal("応答にアイコンが載っていない")
		}
		if got.Icon.ID != iconID {
			t.Errorf("Icon.ID = %v, want %v", got.Icon.ID, iconID)
		}
		// **URL を組み立てて返す** (ADR 0007 決定 5)。
		if got.Icon.URL == "" {
			t.Error("Icon.URL が空 (組み立てていない)")
		}
	})

	// **他人の画像は 404。** 存在を隠すため (ADR 0013)。
	t.Run("他人の画像は拒否される", func(t *testing.T) {
		t.Parallel()

		repo := newFakeRepo(0)
		uc := NewThreadInteractor(repo,
			&fakeIconResolver{err: fmt.Errorf("他人の画像です: %w", apperr.ErrNotFound)})

		_, err := uc.CreateThread(context.Background(), "他人のアイコン", &authorID, &iconID)
		if !errors.Is(err, apperr.ErrNotFound) {
			t.Fatalf("err = %v, want apperr.ErrNotFound", err)
		}
		// **作られていないこと。** アイコンだけ落として作成が通ると、
		// 利用者からは「設定したのに消えた」ようにしか見えない。
		if len(repo.createdThreads) != 0 {
			t.Error("他人の画像を指定したのにスレッドが作られた")
		}
	})

	// **匿名はアイコンを設定できない** (ADR 0007 の背景)。
	t.Run("匿名は設定できない", func(t *testing.T) {
		t.Parallel()

		uc := NewThreadInteractor(newFakeRepo(0), &fakeIconResolver{})

		_, err := uc.CreateThread(context.Background(), "匿名でアイコン", nil, &iconID)
		if !errors.Is(err, apperr.ErrUnauthenticated) {
			t.Errorf("err = %v, want apperr.ErrUnauthenticated", err)
		}
	})

	// ストレージが未設定なら 503。黙って無視しない。
	t.Run("ストレージが未設定なら 503", func(t *testing.T) {
		t.Parallel()

		uc := NewThreadInteractor(newFakeRepo(0), nil)

		_, err := uc.CreateThread(context.Background(), "アイコンつき", &authorID, &iconID)
		if !errors.Is(err, apperr.ErrUnavailable) {
			t.Errorf("err = %v, want apperr.ErrUnavailable", err)
		}
	})

	// アイコンなしはこれまでどおり通ること。
	t.Run("アイコンなしは通る", func(t *testing.T) {
		t.Parallel()

		uc := NewThreadInteractor(newFakeRepo(0), &fakeIconResolver{})

		got, err := uc.CreateThread(context.Background(), "アイコンなし", &authorID, nil)
		if err != nil {
			t.Fatalf("CreateThread が失敗した: %v", err)
		}
		if got.Icon != nil {
			t.Error("指定していないのにアイコンが入っている")
		}
	})
}

// **未ログインを usecase 側でも弾くこと。**
//
// 仕様書の security 宣言が先に 401 を返すので、HTTP 越しの検査では
// **ここが外れていても気づけません** (変異プローブで実測した)。
// 検証ミドルウェアを外した経路やハンドラの直呼びでは、
// actorID = 0 として DB を引くことになります。防御は 2 枚あるべきです。
func TestDeleteOwnThread_RequiresActor(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo(1)
	err := NewThreadInteractor(repo, nil).DeleteOwnThread(t.Context(), 1, nil)
	if !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	if len(repo.deleteCalls) != 0 {
		t.Error("未ログインなのに永続化層まで届いている")
	}
}

// **ログインしていれば内部 ID がそのまま渡ること。**
func TestDeleteOwnThread_PassesActorID(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo(1)
	actor := int64(42)
	if err := NewThreadInteractor(repo, nil).DeleteOwnThread(t.Context(), 7, &actor); err != nil {
		t.Fatalf("DeleteOwnThread が失敗した: %v", err)
	}
	if len(repo.deleteCalls) != 1 || repo.deleteCalls[0] != [2]int64{7, 42} {
		t.Errorf("渡された値 = %v, want [{7 42}]", repo.deleteCalls)
	}
}

// ---------------------------------------------------------------------------
// 人気順 (docs/adr/0006-view-count-and-popularity.md)
// ---------------------------------------------------------------------------

// newFakeRepoWithViewCounts は閲覧数を指定したフェイクを作ります。
// viewCounts は id 1..n の順に渡します。
func newFakeRepoWithViewCounts(viewCounts ...int64) *fakeRepo {
	threads := make([]model.Thread, 0, len(viewCounts))
	counts := make(map[int64]int64, len(viewCounts))
	for i := len(viewCounts); i >= 1; i-- {
		id := int64(i)
		threads = append(threads, *model.Reconstruct(
			id, fmt.Sprintf("スレッド %d", id), nil, nil,
			time.Unix(int64(i), 0).UTC(), viewCounts[i-1]))
		counts[id] = 0
	}
	return &fakeRepo{threads: threads, counts: counts}
}

func ptr[T any](v T) *T { return &v }

// sort=popular が人気順の経路へ届くこと。
//
// **件数だけでは区別できません。** 新着順に落ちても同じ数が返るので、
// フェイク側で呼び出しを数えています。
func TestFetchThreadList_PopularUsesPopularRepository(t *testing.T) {
	t.Parallel()

	repo := newFakeRepoWithViewCounts(10, 300, 20)
	uc := NewThreadInteractor(repo, nil)

	got, err := uc.FetchThreadList(t.Context(), pagination.Page{Size: 10}, nil, ptr("popular"))
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	if repo.popularCalls.Load() != 1 {
		t.Fatalf("人気順の経路が呼ばれていない (popularCalls = %d)", repo.popularCalls.Load())
	}
	if repo.listCalls.Load() != 0 {
		t.Errorf("新着順の経路に落ちている (listCalls = %d)", repo.listCalls.Load())
	}

	// 閲覧数の多い順。
	wantIDs := []int64{2, 3, 1}
	for i, want := range wantIDs {
		if got.Threads[i].ID != want {
			t.Fatalf("並び = %v, want %v", ids(got.Threads), wantIDs)
		}
	}
	// **閲覧数が API まで運ばれること。** ここが落ちると、
	// 人気順で並んでいるのに理由が画面から見えない。
	if got.Threads[0].ViewCount != 300 {
		t.Errorf("viewCount = %d, want 300", got.Threads[0].ViewCount)
	}
}

func ids(threads []ThreadDTO) []int64 {
	out := make([]int64, 0, len(threads))
	for _, t := range threads {
		out = append(out, t.ID)
	}
	return out
}

// 未指定と "new" は新着順になること。
func TestFetchThreadList_DefaultsToNewest(t *testing.T) {
	t.Parallel()

	for _, sort := range []*string{nil, ptr(""), ptr("new")} {
		repo := newFakeRepoWithViewCounts(10, 300, 20)
		if _, err := NewThreadInteractor(repo, nil).
			FetchThreadList(t.Context(), pagination.Page{Size: 10}, nil, sort); err != nil {
			t.Fatalf("FetchThreadList が失敗した: %v", err)
		}
		if repo.popularCalls.Load() != 0 {
			t.Errorf("sort=%v で人気順の経路が呼ばれている", sort)
		}
	}
}

// 未知の並び順は既定に落とさずエラーにすること。
//
// **黙って新着順で返すと、「人気順で並べたつもりの新着順」になります。**
// 利用者にもこちらにも間違いが見えません。
func TestFetchThreadList_RejectsUnknownSort(t *testing.T) {
	t.Parallel()

	repo := newFakeRepoWithViewCounts(1, 2, 3)
	_, err := NewThreadInteractor(repo, nil).
		FetchThreadList(t.Context(), pagination.Page{Size: 10}, nil, ptr("views"))
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
	if repo.popularCalls.Load() != 0 || repo.listCalls.Load() != 0 {
		t.Error("不正な並び順なのに永続化層まで届いている")
	}
}

// 検索と人気順の同時指定を弾くこと (仕様書の Sort パラメータ)。
func TestFetchThreadList_RejectsSearchWithPopular(t *testing.T) {
	t.Parallel()

	repo := newFakeRepoWithViewCounts(1, 2, 3)
	_, err := NewThreadInteractor(repo, nil).
		FetchThreadList(t.Context(), pagination.Page{Size: 10}, ptr("スレ"), ptr("popular"))
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
	if repo.popularCalls.Load() != 0 {
		t.Error("弾いたはずなのに永続化層まで届いている")
	}
}

// 人気順のカーソルに閲覧数が入ること。
//
// **id だけだと、同じ閲覧数の塊の途中で境界を作れません。**
// 次ページが「閲覧数 0 の位置から」始まったり、同値の行を
// 飛ばしたりします (ADR 0018 の規則)。
func TestFetchThreadList_PopularCursorCarriesViewCount(t *testing.T) {
	t.Parallel()

	repo := newFakeRepoWithViewCounts(10, 300, 20)
	got, err := NewThreadInteractor(repo, nil).
		FetchThreadList(t.Context(), pagination.Page{Size: 2}, nil, ptr("popular"))
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	if got.NextCursor == nil {
		t.Fatal("nextCursor が nil")
	}

	c, err := pagination.DecodeCursor(*got.NextCursor)
	if err != nil {
		t.Fatalf("カーソルを復号できない: %v", err)
	}
	if c.Sort != "popular" {
		t.Errorf("cursor.Sort = %q, want popular", c.Sort)
	}
	if c.ViewCount == nil {
		t.Fatal("cursor に閲覧数が入っていない")
	}
	// 2 件目 (id=3, viewCount=20) が境界になる。
	if *c.ViewCount != 20 || c.ID != 3 {
		t.Errorf("cursor = (vc=%v, id=%d), want (20, 3)", *c.ViewCount, c.ID)
	}
}

// 新着順のカーソルには閲覧数を入れないこと。
//
// 入れると、並び順が 1 つだった頃のトークンと形が変わります。
func TestFetchThreadList_NewCursorHasNoViewCount(t *testing.T) {
	t.Parallel()

	repo := newFakeRepoWithViewCounts(10, 300, 20)
	got, err := NewThreadInteractor(repo, nil).
		FetchThreadList(t.Context(), pagination.Page{Size: 2}, nil, nil)
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	c, err := pagination.DecodeCursor(*got.NextCursor)
	if err != nil {
		t.Fatalf("カーソルを復号できない: %v", err)
	}
	if c.Sort != "" || c.ViewCount != nil {
		t.Errorf("cursor = (sort=%q, vc=%v), want (\"\", nil)", c.Sort, c.ViewCount)
	}
}

// **並び順の違うカーソルを弾くこと** (ADR 0018「並び順を足すときの規則」)。
//
// 弾かないと、新着順が発行したトークン (閲覧数なし) を人気順に渡したとき、
// 「閲覧数 0 の位置から」ページングが始まります。
// 400 も出ず、黙って誤ったページが返ります。
func TestFetchThreadList_RejectsCursorFromOtherSort(t *testing.T) {
	t.Parallel()

	repo := newFakeRepoWithViewCounts(10, 300, 20)
	uc := NewThreadInteractor(repo, nil)

	// 新着順のトークンを取る。
	first, err := uc.FetchThreadList(t.Context(), pagination.Page{Size: 2}, nil, nil)
	if err != nil {
		t.Fatalf("FetchThreadList が失敗した: %v", err)
	}
	page, err := pagination.NewPage(first.NextCursor, 2)
	if err != nil {
		t.Fatalf("NewPage が失敗した: %v", err)
	}

	// それを人気順に渡す。
	_, err = uc.FetchThreadList(t.Context(), page, nil, ptr("popular"))
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}

	// 逆向き (人気順のトークンを新着順へ) も弾く。
	popular, err := uc.FetchThreadList(t.Context(), pagination.Page{Size: 2}, nil, ptr("popular"))
	if err != nil {
		t.Fatalf("FetchThreadList (popular) が失敗した: %v", err)
	}
	popularPage, err := pagination.NewPage(popular.NextCursor, 2)
	if err != nil {
		t.Fatalf("NewPage が失敗した: %v", err)
	}
	if _, err := uc.FetchThreadList(t.Context(), popularPage, nil, nil); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

// 人気順のページ送りが、同じ閲覧数の塊をまたいでも重複・欠落しないこと。
func TestFetchThreadList_PopularPaginationHasNoGapOrOverlap(t *testing.T) {
	t.Parallel()

	// **同値を混ぜる。** 閲覧数が同じスレッドは必ず存在するので、
	// そこが境界になったときの振る舞いが本番の形になる。
	repo := newFakeRepoWithViewCounts(5, 5, 5, 5, 5)
	uc := NewThreadInteractor(repo, nil)

	seen := make([]int64, 0, 5)
	var cursor *string
	for range 5 {
		page, err := pagination.NewPage(cursor, 2)
		if err != nil {
			t.Fatalf("NewPage が失敗した: %v", err)
		}
		got, err := uc.FetchThreadList(t.Context(), page, nil, ptr("popular"))
		if err != nil {
			t.Fatalf("FetchThreadList が失敗した: %v", err)
		}
		seen = append(seen, ids(got.Threads)...)
		if got.NextCursor == nil {
			break
		}
		cursor = got.NextCursor
	}

	want := []int64{5, 4, 3, 2, 1}
	if len(seen) != len(want) {
		t.Fatalf("辿った件数 = %d (%v), want %d", len(seen), seen, len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("辿った順 = %v, want %v", seen, want)
		}
	}
}
