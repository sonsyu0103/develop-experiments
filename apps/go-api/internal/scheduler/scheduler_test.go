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
	// 20 個のうち何本かは折り返しまでに走る、という形で見る
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

	// **待つのは間隔の半分。** 初回が一様なら 1 本が走る確率も走らない確率も
	// 1/2 になり、20 本が全部どちらかに偏る確率は 2^-20 (約 100 万分の 1) になる。
	//
	// **1/6 で待っていたときは 2.6% で落ちていた** ((1-1/6)^20)。
	// 実測でも -race で 40 回中 1 回落ちた。ばらつきを見るテストが
	// ばらつきで落ちていたので、両側の余裕が対称になる点まで下げる。
	time.Sleep(interval / 2)

	fires := fired.Load()
	// 揃っていると (全部 interval 後) ここでは 0 本になる。
	if fires == 0 {
		t.Error("初回がばらついていない (間隔の半分で 1 本も走らなかった)")
	}
	// 全部が即時に走ってもばらついていない。
	if fires >= int64(len(jobs)) {
		t.Errorf("初回がばらついていない (%d 本すべてが折り返しまでに走った)", fires)
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
