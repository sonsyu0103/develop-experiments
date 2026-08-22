package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor は cond が真になるまで待ちます。時間で決め打ちしないための道具です。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// Job が繰り返し実行されること。
func TestScheduler_RunsRepeatedly(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	s := New(Job{
		Name:     "probe",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			calls.Add(1)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	if !waitFor(t, 2*time.Second, func() bool { return calls.Load() >= 3 }) {
		t.Fatalf("3 回以上実行されなかった (実測 %d 回)", calls.Load())
	}
}

// **エラーを返しても止まらないこと。**
//
// 一時的な失敗 (DB の瞬断など) で掃除が永久に止まるほうが害が大きい。
func TestScheduler_ContinuesAfterError(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	s := New(Job{
		Name:     "always-fails",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			calls.Add(1)
			return errors.New("いつも失敗する")
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	if !waitFor(t, 2*time.Second, func() bool { return calls.Load() >= 3 }) {
		t.Fatalf("失敗のあと止まっている (実測 %d 回)", calls.Load())
	}
}

// ctx をキャンセルすると止まること。
func TestScheduler_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	s := New(Job{
		Name:     "probe",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			calls.Add(1)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	if !waitFor(t, 2*time.Second, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("1 度も実行されなかった")
	}
	cancel()

	// 停止までに走りうるぶんを見込んでから、増えないことを確かめる。
	time.Sleep(50 * time.Millisecond)
	stopped := calls.Load()
	time.Sleep(100 * time.Millisecond)

	if got := calls.Load(); got > stopped {
		t.Errorf("キャンセル後も実行された (%d -> %d)", stopped, got)
	}
}

// 複数の Job が独立して回ること。
func TestScheduler_RunsMultipleJobs(t *testing.T) {
	t.Parallel()

	var a, b atomic.Int64
	s := New(
		Job{Name: "a", Interval: time.Millisecond, Run: func(context.Context) error {
			a.Add(1)
			return nil
		}},
		Job{Name: "b", Interval: time.Millisecond, Run: func(context.Context) error {
			b.Add(1)
			return nil
		}},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	if !waitFor(t, 2*time.Second, func() bool { return a.Load() >= 2 && b.Load() >= 2 }) {
		t.Fatalf("両方が回っていない (a=%d b=%d)", a.Load(), b.Load())
	}
}

// **初回の実行時刻がばらつくこと。**
//
// 揃えると、デプロイのたびに全レプリカが同じ瞬間へ負荷を集中させる。
// 間隔を長くしておくと、初回は「0〜間隔」のどこかになる。
//
// **時刻を実測して散らばりを見る。** 「ある瞬間に何本走ったか」を数える形だと、
// time.Sleep が**超過側にしかぶれない**ぶんが片側のリスクとして残る:
//
//	1/6 で待つ  -> 20 本すべてが外れる確率 (1-1/6)^20 = 2.6% で偽陽性
//	              (実測でも -race で 40 回中 1 回落ちた)
//	1/2 で待つ  -> 理論上は 2^-20 だが、sleep が 100ms でなく 160ms 返れば
//	              「全部走った」側が 0.8^20 = 1.2% になる。**対称ではない**
//
// 初回の待ち時間そのものを測れば、sleep の伸びは測定値に等しく乗るだけで、
// 散らばりを潰さない。
func TestScheduler_JittersFirstRun(t *testing.T) {
	t.Parallel()

	const (
		interval = 200 * time.Millisecond
		jobCount = 20
	)

	var mu sync.Mutex
	first := make(map[int]time.Duration, jobCount)

	start := time.Now()
	jobs := make([]Job, 0, jobCount)
	for i := range jobCount {
		jobs = append(jobs, Job{
			Name:     "jitter",
			Interval: interval,
			Run: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				// 2 回目以降は初回のばらつきと関係がないので捨てる。
				if _, ok := first[i]; !ok {
					first[i] = time.Since(start)
				}
				return nil
			},
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	New(jobs...).Start(ctx)

	// 初回は必ず interval 未満に来る。遅い環境ぶんの余裕を足して待つ。
	if !waitFor(t, 3*interval, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(first) == jobCount
	}) {
		mu.Lock()
		got := len(first)
		mu.Unlock()
		t.Fatalf("初回が %d/%d 本しか走っていない", got, jobCount)
	}

	mu.Lock()
	lo, hi := first[0], first[0]
	for _, d := range first {
		lo = min(lo, d)
		hi = max(hi, d)
	}
	mu.Unlock()

	// **散らばりの下限を置く。** 初回が一様なら 20 本すべてが interval の
	// 1/4 幅へ収まる確率は 20 * (1/4)^19 ≒ 10^-10 になる。
	// 揃っている実装 (初回が全部 0、または全部 interval) では
	// 幅がスケジューリング誤差ぶんしか出ないので、ここで落ちる。
	if spread := hi - lo; spread < interval/4 {
		t.Errorf("初回のばらつきが %v しかない (want >= %v, 最短 %v / 最長 %v)",
			spread, interval/4, lo, hi)
	}
}

// **Interval が 0 の Job でプロセスを落とさないこと。**
//
// rand.Int64N(0) は panic し、goroutine の中なので recover されない。
// Job は公開 API なので、設定値由来の間隔 (未設定 = 0) が来る余地がある。
func TestNew_NormalizesZeroInterval(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	s := New(Job{
		Name:     "interval-なし",
		Interval: 0,
		Run: func(context.Context) error {
			calls.Add(1)
			return nil
		},
	})

	if got := s.jobs[0].Interval; got != DefaultInterval {
		t.Errorf("Interval = %v, want %v", got, DefaultInterval)
	}

	// **panic しないこと。** ここで落ちるとテストごと巻き込まれる。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	time.Sleep(20 * time.Millisecond)
}

// 負の間隔も同じ扱い。
func TestNew_NormalizesNegativeInterval(t *testing.T) {
	t.Parallel()

	s := New(Job{Name: "負", Interval: -time.Second, Run: func(context.Context) error { return nil }})
	if got := s.jobs[0].Interval; got != DefaultInterval {
		t.Errorf("Interval = %v, want %v", got, DefaultInterval)
	}
}

// **Wait は実行中の Job が終わるまで返らないこと。**
//
// これが無いと、シャットダウンで実行中の処理がプロセスごと打ち切られます。
// 画像の回収は「確保 -> S3 削除 -> 行削除」と進むので、
// 途中で落とすと確保済みの行が恒久的に漏れます (レビュー指摘)。
func TestScheduler_WaitBlocksUntilRunningJobFinishes(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool

	s := New(Job{
		Name:     "long",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
			finished.Store(true)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	<-started

	// 実行中にシャットダウンが始まる。
	cancel()

	waited := make(chan error, 1)
	go func() { waited <- s.Wait(context.Background()) }()

	select {
	case <-waited:
		t.Fatal("実行中の Job を待たずに Wait が返った")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-waited; err != nil {
		t.Errorf("Wait が error を返した: %v", err)
	}
	if !finished.Load() {
		t.Error("Job が最後まで走っていない")
	}
}

// **待ち切れなければ ctx の error を返すこと。**
//
// 呼び出し側 (main) は猶予を使い切ったら記録を残して落ちます。
// **黙って打ち切る**のが元の状態でした。
func TestScheduler_WaitRespectsDeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	s := New(Job{
		Name:     "stuck",
		Interval: time.Millisecond,
		Run: func(context.Context) error {
			<-release
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer waitCancel()

	if err := s.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait = %v, want context.DeadlineExceeded", err)
	}
}
