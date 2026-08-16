package viewcount

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSink は Flush に渡された引数を記録します。
type fakeSink struct {
	mu    sync.Mutex
	calls [][2][]int64
	err   error
	// onCall はフラッシュ中に割り込むためのフックです。
	onCall func()
}

func (f *fakeSink) IncrementViewCounts(_ context.Context, ids, increments []int64) (int64, error) {
	if f.onCall != nil {
		f.onCall()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// **複製して記録する。** 呼び出し側がスライスを再利用した場合に
	// 記録が後から書き換わると、検査が嘘をつく。
	gotIDs := append([]int64(nil), ids...)
	gotInc := append([]int64(nil), increments...)
	f.calls = append(f.calls, [2][]int64{gotIDs, gotInc})
	if f.err != nil {
		return 0, f.err
	}
	var affected int64
	for range ids {
		affected++
	}
	return affected, nil
}

func (f *fakeSink) lastCall(t *testing.T) ([]int64, []int64) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatalf("Flush が sink を呼んでいない")
	}
	last := f.calls[len(f.calls)-1]
	return last[0], last[1]
}

// clock は差し替え可能な時計です。
//
// **実時間を待つテストにしません** (ADR 0006)。フラッシュ間隔や
// 抑制の窓を time.Sleep で待つと、遅くて不安定なテストになります。
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestBuffer(c *clock, opts ...Option) *Buffer {
	return New(append([]Option{WithClock(c.Now)}, opts...)...)
}

// 同じ訪問者の連打を数え直さないこと。
//
// **これが無いとリロードだけで閲覧数を積める。** 人気順は
// 表示用の指標とはいえ、1 人で上位に押し上げられる形は避けたい。
func TestRecord_SuppressesRepeatVisitsWithinWindow(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c, WithDedupeWindow(10*time.Minute))

	if !b.Record(t.Context(), 1, "u:42") {
		t.Fatal("1 回目が数えられていない")
	}
	if b.Record(t.Context(), 1, "u:42") {
		t.Error("窓の中の 2 回目が数えられている")
	}

	// 窓を出れば再び数える。
	c.advance(10 * time.Minute)
	if !b.Record(t.Context(), 1, "u:42") {
		t.Error("窓を過ぎた再訪が数えられていない")
	}
}

// 抑制は (訪問者, スレッド) の組で効くこと。
//
// **スレッドごとに独立している必要があります。** 訪問者だけで抑制すると、
// 1 つ見た後に別のスレッドを見ても数えられなくなります。
func TestRecord_SuppressionIsPerThread(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c)

	b.Record(t.Context(), 1, "u:42")
	if !b.Record(t.Context(), 2, "u:42") {
		t.Error("別のスレッドの閲覧が数えられていない")
	}
	if !b.Record(t.Context(), 1, "u:99") {
		t.Error("別の訪問者の閲覧が数えられていない")
	}
}

// 訪問者を識別できないときは抑制しないこと。
//
// **捨てないのが要点。** 識別子が無いというだけの理由で
// 閲覧数が伸びなくなるのは、指標として明らかにおかしい。
// 抑制が効かない (毎回数える) 側に倒します。
func TestRecord_CountsWhenVisitorUnknown(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c)

	for i := range 3 {
		if !b.Record(t.Context(), 1, "") {
			t.Fatalf("%d 回目が数えられていない", i+1)
		}
	}
}

// 不正なスレッド ID は数えないこと。
func TestRecord_IgnoresInvalidThreadID(t *testing.T) {
	t.Parallel()

	b := New()
	if b.Record(t.Context(), 0, "u:1") || b.Record(t.Context(), -1, "u:1") {
		t.Error("不正な ID が数えられている")
	}
	if b.Pending() != 0 {
		t.Errorf("Pending = %d, want 0", b.Pending())
	}
}

// フラッシュは thread_id の昇順で渡すこと。
//
// **順序はデッドロック対策そのものです** (ADR 0006)。
// 複数のインスタンスが異なる順序で同じ行集合を更新すると、
// ロックの循環待ちが起きます。ここが崩れると、
// 「まれにフラッシュだけが刺さる」という再現しにくい形で出ます。
func TestFlush_SendsThreadIDsInAscendingOrder(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c)

	// わざとばらばらの順で入れる。
	for _, id := range []int64{50, 3, 17, 1} {
		b.Record(t.Context(), id, "")
	}

	sink := &fakeSink{}
	if _, err := b.Flush(t.Context(), sink); err != nil {
		t.Fatalf("Flush が失敗した: %v", err)
	}

	ids, increments := sink.lastCall(t)
	want := []int64{1, 3, 17, 50}
	if len(ids) != len(want) {
		t.Fatalf("件数 = %d, want %d", len(ids), len(want))
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids = %v, want %v", ids, want)
			break
		}
	}
	// 増分は id と対応していること。
	if len(increments) != len(ids) {
		t.Fatalf("増分の長さ = %d, want %d", len(increments), len(ids))
	}
	for i, n := range increments {
		if n != 1 {
			t.Errorf("increments[%d] = %d, want 1 (ids=%v)", i, n, ids)
		}
	}
}

// 同じスレッドの複数回の閲覧が 1 行にまとまること。
//
// **これがバッファリングの目的です。** DB への UPDATE 回数が
// 閲覧数ではなくフラッシュ間隔で決まる、という性質そのもの。
func TestFlush_AggregatesRepeatedViews(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c)

	for i := range 5 {
		// 訪問者を変えて抑制を避ける。
		b.Record(t.Context(), 7, string(rune('a'+i)))
	}

	sink := &fakeSink{}
	if _, err := b.Flush(t.Context(), sink); err != nil {
		t.Fatalf("Flush が失敗した: %v", err)
	}

	ids, increments := sink.lastCall(t)
	if len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("ids = %v, want [7]", ids)
	}
	if increments[0] != 5 {
		t.Errorf("increments = %v, want [5]", increments)
	}
}

// 空のバッファでは sink を呼ばないこと。
//
// 呼ぶと、閲覧が 1 件も無い時間帯にも UPDATE が飛びます。
func TestFlush_SkipsWhenEmpty(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	n, err := New().Flush(t.Context(), sink)
	if err != nil {
		t.Fatalf("Flush が失敗した: %v", err)
	}
	if n != 0 {
		t.Errorf("affected = %d, want 0", n)
	}
	if len(sink.calls) != 0 {
		t.Errorf("空なのに sink を呼んでいる: %v", sink.calls)
	}
}

// 書き込みに失敗したら増分を捨てないこと。
//
// **捨てると、DB の一時的な不調がそのまま閲覧数の欠落になります。**
// ADR 0006 が許容しているのは「プロセスが落ちたときのロスト」であって、
// 「DB が数秒詰まったときのロスト」ではありません。
func TestFlush_RestoresIncrementsOnError(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c)
	b.Record(t.Context(), 1, "a")
	b.Record(t.Context(), 1, "b")
	b.Record(t.Context(), 2, "a")

	wantErr := errors.New("DB が落ちている")
	failing := &fakeSink{err: wantErr}
	if _, err := b.Flush(t.Context(), failing); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if b.Pending() != 2 {
		t.Fatalf("失敗後の Pending = %d, want 2 (増分が捨てられている)", b.Pending())
	}

	// 次のフラッシュで同じ増分が届くこと。
	ok := &fakeSink{}
	if _, err := b.Flush(t.Context(), ok); err != nil {
		t.Fatalf("2 回目の Flush が失敗した: %v", err)
	}
	ids, increments := ok.lastCall(t)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("ids = %v, want [1 2]", ids)
	}
	if increments[0] != 2 || increments[1] != 1 {
		t.Errorf("increments = %v, want [2 1]", increments)
	}
}

// フラッシュ中に来た閲覧を失わないこと。
//
// **取り出してから書き終えるまでに時間がある**ので、その間の Record が
// 消えると、負荷が高いときほど閲覧数が落ちます。
// しかも落ちた量は誰にも見えません。
func TestFlush_KeepsViewsRecordedDuringFlush(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c)
	b.Record(t.Context(), 1, "a")

	sink := &fakeSink{}
	// sink の実行中に新しい閲覧を入れる。
	sink.onCall = func() { b.Record(t.Context(), 1, "b") }

	if _, err := b.Flush(t.Context(), sink); err != nil {
		t.Fatalf("Flush が失敗した: %v", err)
	}

	if b.Pending() != 1 {
		t.Fatalf("フラッシュ中の閲覧が消えている (Pending = %d, want 1)", b.Pending())
	}
	if _, err := b.Flush(t.Context(), sink); err != nil {
		t.Fatalf("2 回目の Flush が失敗した: %v", err)
	}
	ids, increments := sink.lastCall(t)
	if len(ids) != 1 || ids[0] != 1 || increments[0] != 1 {
		t.Errorf("ids = %v, increments = %v, want [1] [1]", ids, increments)
	}
}

// 失敗の戻しがフラッシュ中の閲覧を上書きしないこと。
//
// restore が加算ではなく代入だと、ここで 1 件消えます。
func TestFlush_RestoreAddsInsteadOfOverwriting(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c)
	b.Record(t.Context(), 1, "a")

	wantErr := errors.New("DB が落ちている")
	sink := &fakeSink{err: wantErr}
	sink.onCall = func() { b.Record(t.Context(), 1, "b") }

	if _, err := b.Flush(t.Context(), sink); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}

	ok := &fakeSink{}
	if _, err := b.Flush(t.Context(), ok); err != nil {
		t.Fatalf("2 回目の Flush が失敗した: %v", err)
	}
	_, increments := ok.lastCall(t)
	if len(increments) != 1 || increments[0] != 2 {
		t.Errorf("increments = %v, want [2] (戻しが上書きになっている)", increments)
	}
}

// 抑制の記憶が上限に達したら、抑制をやめて数えること。
//
// **逆にしてはいけません。** カウントをやめる形にすると、
// アクセスが多いときほど閲覧数が伸びなくなり、人気順が壊れます。
// 数え過ぎのほうが害が小さい。
func TestRecord_StopsSuppressingWhenVisitorLimitReached(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c, WithMaxVisitors(2))

	b.Record(t.Context(), 1, "a")
	b.Record(t.Context(), 1, "b")
	// ここで上限。新しい訪問者は覚えられないが、数えはする。
	if !b.Record(t.Context(), 1, "c") {
		t.Fatal("上限到達後の閲覧が数えられていない")
	}
	if !b.Record(t.Context(), 1, "c") {
		t.Error("覚えていない訪問者の再訪が抑制されている")
	}
	// 既に覚えている訪問者の抑制は効き続ける。
	if b.Record(t.Context(), 1, "a") {
		t.Error("既知の訪問者の抑制が外れている")
	}
}

// 期限切れの抑制記録がフラッシュで捨てられること。
//
// 捨てないと、seen が上限まで埋まったまま戻らず、
// **抑制が事実上効かなくなります。**
func TestFlush_EvictsExpiredVisitors(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c, WithDedupeWindow(time.Minute), WithMaxVisitors(2))

	b.Record(t.Context(), 1, "a")
	b.Record(t.Context(), 1, "b")

	// 窓を過ぎてからフラッシュすると、記録が捨てられる。
	c.advance(2 * time.Minute)
	if _, err := b.Flush(t.Context(), &fakeSink{}); err != nil {
		t.Fatalf("Flush が失敗した: %v", err)
	}

	// 空きができたので、新しい訪問者を覚えられる。
	b.Record(t.Context(), 2, "c")
	if b.Record(t.Context(), 2, "c") {
		t.Error("空きができたのに新しい訪問者を覚えていない")
	}
}

// 並行に呼んでも壊れないこと。
//
// **Record は全リクエストから同時に呼ばれます。**
// race detector 付きで回すことに意味があります (make test)。
func TestRecord_IsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	b := New()
	var wg sync.WaitGroup
	const workers, perWorker = 8, 50

	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				// 訪問者を全部変えて抑制を避ける (数えた総数を確定させる)。
				b.Record(t.Context(), int64(i%5+1), string(rune('a'+w))+string(rune('0'+i%10))+string(rune(i)))
			}
		})
	}
	wg.Wait()

	sink := &fakeSink{}
	if _, err := b.Flush(t.Context(), sink); err != nil {
		t.Fatalf("Flush が失敗した: %v", err)
	}
	_, increments := sink.lastCall(t)
	var total int64
	for _, n := range increments {
		total += n
	}
	if total != workers*perWorker {
		t.Errorf("合計 = %d, want %d", total, workers*perWorker)
	}
}

// ---------------------------------------------------------------------------
// 同期 UPDATE 版 (ADR 0006 の選択肢 A / Phase 4 の比較用)
// ---------------------------------------------------------------------------

// その場で反映すること。
//
// **これが D との違いそのものです。** バッファに貯めず、
// 1 回の閲覧が 1 回の UPDATE になります。
func TestSyncCounter_WritesImmediately(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	c := viewcountSync(t, sink)

	if !c.Record(t.Context(), 7, "a") {
		t.Fatal("数えられていない")
	}
	ids, increments := sink.lastCall(t)
	if len(ids) != 1 || ids[0] != 7 || increments[0] != 1 {
		t.Errorf("ids = %v, increments = %v, want [7] [1]", ids, increments)
	}
}

// 抑制は Buffer と同じであること。
//
// **ここが揃っていないと Phase 4 の比較が濁ります。** 「同期だから遅い」のか
// 「抑制が無いから更新が多い」のか区別できなくなります。
func TestSyncCounter_SharesSuppressionWithBuffer(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	c := viewcountSync(t, sink)

	c.Record(t.Context(), 7, "a")
	if c.Record(t.Context(), 7, "a") {
		t.Error("窓の中の 2 回目が数えられている")
	}
	if len(sink.calls) != 1 {
		t.Errorf("UPDATE の回数 = %d, want 1", len(sink.calls))
	}
}

// 書き込みに失敗しても落ちないこと。
//
// 数えられなかっただけで、スレッドの取得は既に終わっています。
func TestSyncCounter_SurvivesSinkFailure(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{err: errors.New("DB が落ちている")}
	c := viewcountSync(t, sink)

	if c.Record(t.Context(), 7, "a") {
		t.Error("失敗したのに数えたことになっている")
	}
}

// 不正なスレッド ID では UPDATE を打たないこと。
func TestSyncCounter_IgnoresInvalidThreadID(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	c := viewcountSync(t, sink)

	if c.Record(t.Context(), 0, "a") {
		t.Error("不正な ID が数えられている")
	}
	if len(sink.calls) != 0 {
		t.Errorf("UPDATE が飛んでいる: %v", sink.calls)
	}
}

func viewcountSync(t *testing.T, sink Sink) *SyncCounter {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	return NewSync(sink, WithClock(c.Now), WithDedupeWindow(10*time.Minute))
}

// 抑制の窓に 0 を指定したら、抑制しないこと。
//
// **未指定 (既定 10 分) と区別する必要があります。** 0 を「未指定」として
// 既定に戻していた頃は、Phase 4 で閲覧の負荷をかけようとしても
// 同じ訪問者から 1 回しか数えられず、**DB への UPDATE がほとんど
// 発生しませんでした** —— 同期版との比較が成立しません。
func TestWithDedupeWindow_ZeroDisablesSuppression(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c, WithDedupeWindow(0))

	for i := range 5 {
		if !b.Record(t.Context(), 1, "u:42") {
			t.Fatalf("%d 回目が抑制された (窓 0 は抑制しない設定)", i+1)
		}
	}

	sink := &fakeSink{}
	if _, err := b.Flush(t.Context(), sink); err != nil {
		t.Fatalf("Flush が失敗した: %v", err)
	}
	_, increments := sink.lastCall(t)
	if increments[0] != 5 {
		t.Errorf("increments = %v, want [5]", increments)
	}
}

// 負値は無視して既定のままにすること。
func TestWithDedupeWindow_NegativeIsIgnored(t *testing.T) {
	t.Parallel()

	c := &clock{now: time.Unix(1_700_000_000, 0)}
	b := newTestBuffer(c, WithDedupeWindow(-time.Minute))

	b.Record(t.Context(), 1, "u:42")
	if b.Record(t.Context(), 1, "u:42") {
		t.Error("負値で抑制が外れている (既定のままであるべき)")
	}
}
