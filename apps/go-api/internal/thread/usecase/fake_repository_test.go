package usecase

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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

	// 以下は観測用。
	countCalls       atomic.Int64
	concurrentNow    atomic.Int64
	concurrentPeak   atomic.Int64
	createdThreadMux sync.Mutex
	createdThreads   []string
}

var (
	_ repository.ThreadRepository    = (*fakeRepo)(nil)
	_ repository.BenchmarkRepository = (*fakeRepo)(nil)
)

func (f *fakeRepo) summaries(page pagination.Page) []model.Summary {
	out := make([]model.Summary, 0, len(f.threads))
	for _, t := range f.threads {
		if cursorID := page.CursorID(); cursorID != nil && t.ID >= *cursorID {
			continue
		}
		out = append(out, model.Summary{Thread: t, CommentCount: f.counts[t.ID]})
		if int32(len(out)) == page.Size {
			break
		}
	}
	return out
}

func (f *fakeRepo) ListSummaries(_ context.Context, page pagination.Page) ([]model.Summary, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.summaries(page), nil
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
	return model.Reconstruct(int64(len(f.createdThreads)), thread.Title, time.Unix(0, 0).UTC()), nil
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

// newFakeRepo は id が 1..n のスレッドを新しい順 (降順) に持つフェイクを作ります。
func newFakeRepo(n int) *fakeRepo {
	threads := make([]model.Thread, 0, n)
	counts := make(map[int64]int64, n)
	for i := n; i >= 1; i-- {
		id := int64(i)
		threads = append(threads, *model.Reconstruct(id, fmt.Sprintf("スレッド %d", id), time.Unix(int64(i), 0).UTC()))
		counts[id] = id * 3
	}
	return &fakeRepo{threads: threads, counts: counts}
}
