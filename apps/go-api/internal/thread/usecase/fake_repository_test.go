package usecase

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/pagination"
	"develop-experiments/apps/go-api/internal/thread/domain/model"
	"develop-experiments/apps/go-api/internal/thread/domain/repository"
)

// fakeRepo は ThreadRepository と BenchmarkRepository のテスト用実装です。
// DB を立てずにユースケース層の振る舞いを検証できます。
type fakeRepo struct {
	threads []model.Thread
	// counts は thread ID → コメント数です。
	counts map[int64]int64

	// listErr / countErr を設定すると、該当メソッドがエラーを返します。
	listErr  error
	countErr error
	// countErrForID を設定すると、その ID の CountComments だけが失敗します。
	countErrForID *int64

	// countDelay は CountComments 1 回あたりの疑似レイテンシです。
	// 並列度の検証に使います。
	countDelay time.Duration

	// authorCalls は FindThreadAuthor の呼び出し回数です。
	// **CountComments とは別に数えます** —— N+1 版は 1 スレッドあたり
	// 2 往復するので、片方だけ数えると往復回数を半分に見誤ります。
	authorCalls atomic.Int64

	// 以下は観測用。
	// listCalls は ListSummaries の呼び出し回数です。
	// 検索のとき「絞り込まない一覧」に落ちていないことを見るために使います。
	listCalls        atomic.Int64
	countCalls       atomic.Int64
	concurrentNow    atomic.Int64
	concurrentPeak   atomic.Int64
	createdThreadMux sync.Mutex
	createdThreads   []string
	// deleteCalls は SoftDeleteOwn に渡された (id, actorID) です。
	deleteCallMux sync.Mutex
	deleteCalls   [][2]int64
	// searchCalls は SearchSummaries に渡された検索語です。
	searchCallMux sync.Mutex
	searchCalls   []string
	// popularCalls は ListPopularSummaries の呼び出し回数です。
	// **新着順に落ちていないこと**を見るために要ります ——
	// 落ちても件数は同じになるので、結果だけでは区別できません。
	popularCalls atomic.Int64
}

var (
	_ repository.ThreadRepository    = (*fakeRepo)(nil)
	_ repository.BenchmarkRepository = (*fakeRepo)(nil)
)

// SoftDeleteOwn は呼ばれた引数を記録します。
//
// **生存の再現はしません。** 削除の 3 分岐 (204 / 403 / 404) は
// 永続化層の 1 文が決めるので、フェイクで真似ても
// 実装を検査したことにならない (実 DB の検査が持ちます)。
// ここで見たいのは「ログインしていないと呼ばれないこと」だけです。
func (f *fakeRepo) SoftDeleteOwn(_ context.Context, id, actorID int64) error {
	f.deleteCallMux.Lock()
	defer f.deleteCallMux.Unlock()
	f.deleteCalls = append(f.deleteCalls, [2]int64{id, actorID})
	return nil
}

func (f *fakeRepo) summaries(page pagination.Page) []model.Summary {
	return applyPage(f.summariesAll(), page)
}

// summariesAll は絞り込み前の全件を新しい順で返します。
func (f *fakeRepo) summariesAll() []model.Summary {
	out := make([]model.Summary, 0, len(f.threads))
	for _, t := range f.threads {
		out = append(out, model.Summary{Thread: t, CommentCount: f.counts[t.ID]})
	}
	return out
}

// applyPage はカーソルと件数の絞り込みを行います。
// **一覧と検索で共通です** —— 検索結果も新着順なので、
// カーソルの扱いが変わらないことを実装でも表しています (ADR 0012 決定 3)。
func applyPage(all []model.Summary, page pagination.Page) []model.Summary {
	out := make([]model.Summary, 0, len(all))
	for _, s := range all {
		if cursorID := page.CursorID(); cursorID != nil && s.ID >= *cursorID {
			continue
		}
		out = append(out, s)
		if int32(len(out)) == page.Size {
			break
		}
	}
	return out
}

func (f *fakeRepo) ListSummaries(_ context.Context, page pagination.Page) ([]model.Summary, error) {
	f.listCalls.Add(1)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.summaries(page), nil
}

// ListPopularSummaries は閲覧数の多い順に返します
// (docs/adr/0006-view-count-and-popularity.md)。
//
// **カーソルの扱いが新着順と違うことを、フェイクでも表しています。**
// 境界は (view_count, id) の複合キーで、片方だけでは決まりません ——
// ここを id だけで切ると、同じ閲覧数の塊の途中でページが割れたときに
// 行が重複・欠落します。それは実装のバグとして起きうる形なので、
// フェイクが「id だけ見ても通る」状態にしてしまうと、
// ユースケース層のカーソル組み立ての検査が意味を失います。
func (f *fakeRepo) ListPopularSummaries(
	_ context.Context, page pagination.Page,
) ([]model.Summary, error) {
	f.popularCalls.Add(1)
	if f.listErr != nil {
		return nil, f.listErr
	}

	all := f.summariesAll()
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].ViewCount != all[j].ViewCount {
			return all[i].ViewCount > all[j].ViewCount
		}
		return all[i].ID > all[j].ID
	})

	out := make([]model.Summary, 0, len(all))
	for _, s := range all {
		if vc, id := page.CursorViewCount(), page.CursorID(); vc != nil && id != nil {
			// 行値比較 (view_count, id) < (cursor_view_count, cursor_id)
			if s.ViewCount > *vc || (s.ViewCount == *vc && s.ID >= *id) {
				continue
			}
		}
		out = append(out, s)
		if int32(len(out)) == page.Size {
			break
		}
	}
	return out, nil
}

// SearchSummaries は検索語を記録したうえで、タイトルの部分一致で絞り込みます。
//
// **ILIKE の意味を真似はしません。** ここで見たいのは
// 「検索語が永続化層まで正規化された形で届くこと」と
// 「絞り込んでもページ送りの組み立てが変わらないこと」の 2 つで、
// 一致の判定そのものは実 DB の検査が持ちます (ADR 0012)。
//
// **エスケープ済みの文字列は届きません。** LIKE のワイルドカードを
// 打ち消すのは PostgreSQL 実装の中だけの話なので、
// ここに `\%` が来たらそれは層の切り分けが崩れた合図になります。
func (f *fakeRepo) SearchSummaries(
	_ context.Context, query model.SearchQuery, page pagination.Page,
) ([]model.Summary, error) {
	f.searchCallMux.Lock()
	f.searchCalls = append(f.searchCalls, query.Keyword())
	f.searchCallMux.Unlock()

	if f.listErr != nil {
		return nil, f.listErr
	}

	matched := make([]model.Summary, 0, len(f.threads))
	for _, s := range f.summariesAll() {
		if strings.Contains(s.Title, query.Keyword()) {
			matched = append(matched, s)
		}
	}
	return applyPage(matched, page), nil
}

func (f *fakeRepo) FindSummaryByID(_ context.Context, id int64) (*model.Summary, error) {
	for _, t := range f.threads {
		if t.ID == id {
			return &model.Summary{Thread: t, CommentCount: f.counts[id]}, nil
		}
	}
	return nil, fmt.Errorf("fakeRepo.FindSummaryByID: %w", apperr.ErrNotFound)
}

func (f *fakeRepo) Create(_ context.Context, thread *model.Thread) (*model.Thread, error) {
	f.createdThreadMux.Lock()
	defer f.createdThreadMux.Unlock()

	f.createdThreads = append(f.createdThreads, thread.Title)
	// 実装では LEFT JOIN images がアイコンを解決する。フェイクでも同じ形にする。
	var icon *model.Image
	if thread.IconImageID != nil {
		icon = model.NewImage(*thread.IconImageID,
			"images/"+thread.IconImageID.String()+".webp", 64, 64)
	}
	// 作成直後の閲覧数は必ず 0。加算はフラッシュだけが行う。
	return model.Reconstruct(int64(len(f.createdThreads)), thread.Title, thread.Author,
		icon, time.Unix(0, 0).UTC(), 0), nil
}

func (f *fakeRepo) Exists(_ context.Context, id int64) (bool, error) {
	for _, t := range f.threads {
		if t.ID == id {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRepo) ListThreadsOnly(_ context.Context, page pagination.Page) ([]model.Thread, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]model.Thread, 0, len(f.threads))
	for _, t := range f.threads {
		if cursorID := page.CursorID(); cursorID != nil && t.ID >= *cursorID {
			continue
		}
		out = append(out, t)
		if int32(len(out)) == page.Size {
			break
		}
	}
	return out, nil
}

func (f *fakeRepo) CountComments(ctx context.Context, threadID int64) (int64, error) {
	f.countCalls.Add(1)

	// 同時実行数のピークを記録する。errgroup の SetLimit が
	// 実際に効いているかを検証するために使う。
	now := f.concurrentNow.Add(1)
	for {
		peak := f.concurrentPeak.Load()
		if now <= peak || f.concurrentPeak.CompareAndSwap(peak, now) {
			break
		}
	}
	defer f.concurrentNow.Add(-1)

	if f.countDelay > 0 {
		select {
		case <-time.After(f.countDelay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	if f.countErr != nil {
		if f.countErrForID == nil || *f.countErrForID == threadID {
			return 0, f.countErr
		}
	}

	return f.counts[threadID], nil
}

// FindThreadAuthor は投稿者を 1 件ずつ返します (ベンチマーク用の N+1 経路)。
//
// **偶数 ID にだけ投稿者を付けます。** 全件に付けると匿名の分岐
// (行が無い = nil を返す) が一度も通らず、
// 「投稿者が nil のスレッドで落ちる」実装ミスを検査できません。
func (f *fakeRepo) FindThreadAuthor(ctx context.Context, threadID int64) (*model.Author, error) {
	f.authorCalls.Add(1)

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.countErr != nil && f.countErrForID != nil && *f.countErrForID == threadID {
		return nil, f.countErr
	}
	if threadID%2 != 0 {
		return nil, nil
	}

	id := uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("author-%d", threadID)))
	return model.NewAuthor(id, fmt.Sprintf("投稿者 %d", threadID), nil, nil), nil
}

// newFakeRepoWithTitles は指定したタイトルのスレッドを持つフェイクを作ります。
// titles は id の昇順で渡し、保持は新しい順 (降順) になります。
func newFakeRepoWithTitles(titles ...string) *fakeRepo {
	threads := make([]model.Thread, 0, len(titles))
	counts := make(map[int64]int64, len(titles))
	for i := len(titles); i >= 1; i-- {
		id := int64(i)
		threads = append(threads,
			*model.Reconstruct(id, titles[i-1], nil, nil, time.Unix(int64(i), 0).UTC(), 0))
		counts[id] = 0
	}
	return &fakeRepo{threads: threads, counts: counts}
}

// newFakeRepo は id が 1..n のスレッドを新しい順 (降順) に持つフェイクを作ります。
func newFakeRepo(n int) *fakeRepo {
	threads := make([]model.Thread, 0, n)
	counts := make(map[int64]int64, n)
	for i := n; i >= 1; i-- {
		id := int64(i)
		threads = append(threads, *model.Reconstruct(id, fmt.Sprintf("スレッド %d", id), nil, nil, time.Unix(int64(i), 0).UTC(), 0))
		counts[id] = id * 3
	}
	return &fakeRepo{threads: threads, counts: counts}
}
