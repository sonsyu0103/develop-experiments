package scheduler

import (
	"context"
	"errors"
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
func TestScheduler_JittersFirstRun(t *testing.T) {
	t.Parallel()

	// 十分に長い間隔にすると、待ちが 0 に近い実行と遠い実行に分かれる。
	// 20 個のうち少なくとも 1 つは 30ms 以内に走る、という形で見る
	// (すべてが間隔の末尾に寄っていたら、ばらついていない)。
	const interval = 200 * time.Millisecond

	var fired atomic.Int64
	jobs := make([]Job, 0, 20)
	for range 20 {
		jobs = append(jobs, Job{
			Name:     "jitter",
			Interval: interval,
			Run: func(context.Context) error {
				fired.Add(1)
				return nil
			},
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	New(jobs...).Start(ctx)

	// ばらついていれば、間隔の 1/6 のうちに何本かは走る。
	// 揃っていると (全部 interval 後) ここでは 0 本になる。
	time.Sleep(interval / 6)
	if fired.Load() == 0 {
		t.Error("初回がばらついていない (間隔の 1/6 で 1 本も走らなかった)")
	}
	// 全部が即時に走ってもばらついていない。
	if fired.Load() >= int64(len(jobs)) {
		t.Error("初回がばらついていない (全部が即座に走った)")
	}
}
