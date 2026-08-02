package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/pagination"
)

// go test -race で実行することを前提にしたテスト。
// 各 goroutine が summaries の異なる添字だけに書き込む前提が
// 崩れていれば、race detector が検出する。
func TestNPlusOneInteractor_AggregatesCorrectlyInParallel(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo(50)
	uc := NewNPlusOneInteractor(repo, 8)

	got, err := uc.FetchThreadListNPlusOne(context.Background(), mustPage(t, nil, 50))
	if err != nil {
		t.Fatalf("FetchThreadListNPlusOne が失敗した: %v", err)
	}

	if len(got.Threads) != 50 {
		t.Fatalf("件数 = %d, want 50", len(got.Threads))
	}

	// 並列で書き込んでも、添字と内容の対応が崩れていないこと。
	// fakeRepo は ID 降順に並べ、コメント数を ID*3 にしている。
	for i, th := range got.Threads {
		wantID := int64(50 - i)
		if th.ID != wantID {
			t.Fatalf("threads[%d].ID = %d, want %d (並列書き込みで順序が壊れている)", i, th.ID, wantID)
		}
		if th.CommentCount != wantID*3 {
			t.Fatalf("threads[%d].CommentCount = %d, want %d (集計値が取り違えられている)",
				i, th.CommentCount, wantID*3)
		}
	}

	// N+1 なので、スレッド件数ぶんだけ COUNT が飛んでいるはず。
	// これが本命実装 (0 回) との差であり、ベンチマークで比較する対象。
	if n := repo.countCalls.Load(); n != 50 {
		t.Errorf("CountComments の呼び出し回数 = %d, want 50", n)
	}
}

func TestNPlusOneInteractor_RespectsConcurrencyLimit(t *testing.T) {
	t.Parallel()

	const limit = 4

	repo := newFakeRepo(40)
	// 同時実行のピークを観測できるよう、各クエリに疑似レイテンシを入れる。
	repo.countDelay = 5 * time.Millisecond

	uc := NewNPlusOneInteractor(repo, limit)

	if _, err := uc.FetchThreadListNPlusOne(context.Background(), mustPage(t, nil, 40)); err != nil {
		t.Fatalf("FetchThreadListNPlusOne が失敗した: %v", err)
	}

	peak := repo.concurrentPeak.Load()
	if peak > limit {
		t.Errorf("同時実行数のピーク = %d, want <= %d (errgroup.SetLimit が効いていない)", peak, limit)
	}
	// 上限まで使い切れていることも確認する。
	// ここが 1 なら、そもそも並列化できていない。
	if peak < 2 {
		t.Errorf("同時実行数のピーク = %d, 並列化されていない", peak)
	}
}

func TestNPlusOneInteractor_DefaultConcurrency(t *testing.T) {
	t.Parallel()

	uc := NewNPlusOneInteractor(newFakeRepo(1), 0)
	if uc.concurrency != DefaultAggregationConcurrency {
		t.Errorf("concurrency = %d, want %d", uc.concurrency, DefaultAggregationConcurrency)
	}

	uc = NewNPlusOneInteractor(newFakeRepo(1), -5)
	if uc.concurrency != DefaultAggregationConcurrency {
		t.Errorf("concurrency = %d, want %d", uc.concurrency, DefaultAggregationConcurrency)
	}
}

func TestNPlusOneInteractor_FailsIfAnyQueryFails(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("COUNT が失敗しました")
	target := int64(25)

	repo := newFakeRepo(50)
	repo.countErr = sentinel
	repo.countErrForID = &target

	uc := NewNPlusOneInteractor(repo, 8)

	_, err := uc.FetchThreadListNPlusOne(context.Background(), mustPage(t, nil, 50))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

// errgroup.WithContext がエラー発生時に残りの goroutine を打ち切ることの確認。
// 打ち切られなければ pagination.MaxSize 件すべての COUNT が実行されてしまう。
func TestNPlusOneInteractor_CancelsRemainingOnError(t *testing.T) {
	t.Parallel()

	const total = int(pagination.MaxSize)

	sentinel := errors.New("即座に失敗")

	repo := newFakeRepo(total)
	repo.countErr = sentinel // ID 指定なし = すべて失敗
	repo.countDelay = 2 * time.Millisecond

	uc := NewNPlusOneInteractor(repo, 4)

	if _, err := uc.FetchThreadListNPlusOne(context.Background(), mustPage(t, nil, pagination.MaxSize)); err == nil {
		t.Fatal("エラーを期待したが nil だった")
	}

	// 最初の失敗が確定した時点で ctx がキャンセルされるため、
	// 全件が呼ばれることはない。
	if n := repo.countCalls.Load(); n >= int64(total) {
		t.Errorf("CountComments の呼び出し回数 = %d, want < %d (エラー後に打ち切られていない)", n, total)
	}
}

func TestNPlusOneInteractor_RespectsParentCancellation(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo(100)
	repo.countDelay = 10 * time.Millisecond

	uc := NewNPlusOneInteractor(repo, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := uc.FetchThreadListNPlusOne(ctx, mustPage(t, nil, pagination.MaxSize))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}
