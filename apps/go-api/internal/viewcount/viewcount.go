// Package viewcount は閲覧数をメモリ上に貯め、まとめて反映します。
//
// 設計は docs/adr/0006-view-count-and-popularity.md の決定 D。
//
// **これは性能上の最適化ではありません。** 閲覧のたびに
// `UPDATE threads SET view_count = view_count + 1` を打つと、
// 3 つのことが同時に起きます。
//
//  1. 人気スレッドほど同じ行に更新が集中する (ホットロウ)
//  2. **Phase 2 の SERIALIZABLE を殺す。** コメント投稿は親スレッドの
//     存在確認で threads を読むため、そこに閲覧数の UPDATE が混ざると
//     rw-conflict が大量に生まれ、**閲覧というまったく無関係な操作が
//     コメント投稿の直列化失敗率を押し上げます**
//  3. view_count に索引があると HOT update が効かなくなる
//
// 2 が決定的な理由です。ここを分離しないと、Phase 2 で測ろうとしている
// スループットが、測定対象と無関係な要因で汚染されます。
//
// # 引き受けるコスト
//
//   - **値がロストします。** プロセスが異常終了するとバッファ内の増分は消えます。
//     graceful shutdown ではフラッシュしてから終了しますが、SIGKILL は救えません
//   - **値が最新ではありません。** 最大でフラッシュ間隔ぶん遅れます
//   - **水平スケールでフラッシュ頻度がインスタンス数に比例します**
//
// 閲覧数は表示用の指標であり、課金や順位の確定には使いません。
package viewcount

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultDedupeWindow は同一の訪問者を数え直さない時間です。
	//
	// 厳密に弾こうとすると閲覧履歴を全件持つことになり、
	// 「閲覧イベントを追記専用テーブルに INSERT する」(選択肢 B) の
	// 欠点に合流します。**メモリ上の近似**に留めます。
	DefaultDedupeWindow = 10 * time.Minute

	// DefaultMaxVisitors は重複抑制のために覚えておく訪問者の上限です。
	//
	// **上限に達したら、抑制をやめてカウントします。** 逆 (カウントを
	// やめる) にすると、アクセスが多いときほど閲覧数が伸びなくなり、
	// 人気順が壊れます。数え過ぎのほうが害が小さい。
	DefaultMaxVisitors = 100_000
)

// Recorder は閲覧を数えます。
//
// **実装は 2 つあります** (docs/adr/0006-view-count-and-popularity.md)。
//
//	Buffer       メモリに貯めて定期的に反映する (選択肢 D。**本番の実装**)
//	SyncCounter  その場で UPDATE する (選択肢 A。**Phase 4 の比較用**)
//
// インターフェースにしてあるのは、**A が本当に問題を起こすことを
// 実測するため**です。「D にしたら速くなった」は、A が遅いことを
// 示さない限り主張になりません (ADR 0019 で naive を残したのと同じ形)。
type Recorder interface {
	// Record は 1 件の閲覧を数えます。数えたら true を返します。
	Record(ctx context.Context, threadID int64, visitor string) bool
}

// Sink はバッファの内容を永続化層へ反映します。
//
// **実装は必ず別トランザクション・READ COMMITTED で書くこと。**
// コメント投稿やスレッド作成のトランザクションに混ぜると、
// このパッケージが存在する理由 (冒頭の 2) が消えます。
type Sink interface {
	// IncrementViewCounts は thread_ids[i] に increments[i] を加算します。
	// 2 つのスライスは同じ長さで、thread_ids は昇順です。
	IncrementViewCounts(ctx context.Context, threadIDs, increments []int64) (int64, error)
}

// visitKey は「誰がどのスレッドを見たか」です。
//
// **訪問者は識別子そのものではなくハッシュで持ちます。**
// セッション ID を長時間メモリに残すと、パニック時のダンプや
// プロファイルの出力に載る余地が生まれます。ここで必要なのは
// 「同じ人か」の判定だけで、元の値を復元する必要はありません
// (ログにセッション ID を出さないのと同じ考え方。ADR 0010 の 4-5)。
type visitKey struct {
	visitor  uint64
	threadID int64
}

// visitorGate は「同じ人が同じスレッドを短時間に何度も見た」を弾きます。
//
// **Buffer と SyncCounter で共有します。** Phase 4 で 2 つを比べるとき、
// 抑制の有無が違うと「同期だから遅い」のか「抑制が無いから更新が多い」のか
// 区別できません。**差を反映方式だけに絞る**ための共有になります。
type visitorGate struct {
	mu sync.Mutex
	// seen は訪問者ごとの最終計上時刻です。
	seen   map[visitKey]time.Time
	window time.Duration
	max    int
	now    func() time.Time
	// lastEvict は最後に期限切れを捨てた時刻です。
	//
	// **掃除の契機を gate 自身が持ちます。** 以前は Buffer.Flush だけが
	// 呼んでおり、**SyncCounter (選択肢 A) では一度も掃除されませんでした**
	// (レビュー指摘)。seen が上限に達すると新しい訪問者を覚えなくなるため、
	// **抑制が静かに無効になった状態で測定が続く** ——
	// Phase 4 で「抑制あり」の条件を作ったつもりが「抑制なし」になります。
	lastEvict time.Time
}

func newVisitorGate(window time.Duration, max int, now func() time.Time) *visitorGate {
	return &visitorGate{
		seen:      make(map[visitKey]time.Time),
		window:    window,
		max:       max,
		now:       now,
		lastEvict: now(),
	}
}

// allow は数えてよいかを返します。数える場合は今回の閲覧を記録します。
//
// **visitor が空なら常に許します。** 識別できない訪問者を捨てると、
// 抑制のためのキーが無いというだけの理由で閲覧数が伸びなくなります。
func (g *visitorGate) allow(threadID int64, visitor string) bool {
	if visitor == "" {
		return true
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	// **窓が 1 周するごとに掃除する。** 毎回走査すると訪問者の数だけ
	// 回ることになり、Record が重くなります。
	if now.Sub(g.lastEvict) >= g.window {
		g.evictExpiredLocked(now)
	}

	key := visitKey{visitor: hashVisitor(visitor), threadID: threadID}
	if last, ok := g.seen[key]; ok && now.Sub(last) < g.window {
		return false
	}
	// 上限に達したら抑制をやめる (DefaultMaxVisitors の説明)。
	// 既知のキーの更新は上限に関係なく行う —— そうしないと、
	// 上限到達後に「一度覚えた人だけ永久に抑制される」ことになる。
	if _, known := g.seen[key]; known || len(g.seen) < g.max {
		g.seen[key] = now
	}
	return true
}

// evictExpired は期限切れの記録を捨てます。
//
// **allow が窓ごとに自動で呼ぶので、通常は外から呼ぶ必要はありません。**
// フラッシュの契機でも呼んでいるのは、閲覧が止まっている間に
// 記録を抱えたままにしないためです (allow が呼ばれなければ掃除も走らない)。
func (g *visitorGate) evictExpired() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.evictExpiredLocked(g.now())
}

// evictExpiredLocked は呼び出し側がロックを保持している前提で掃除します。
//
// **全件走査します。** 訪問者の数だけ回るので、上限 (max) が
// そのまま 1 回のコストの上限になります。期限切れを別の索引で
// 管理する形にはしていません —— 数万件の走査は数 ms で終わり、
// 掃除は窓ごとにしか走らないためです。
func (g *visitorGate) evictExpiredLocked(now time.Time) {
	g.lastEvict = now
	for key, last := range g.seen {
		if now.Sub(last) >= g.window {
			delete(g.seen, key)
		}
	}
}

// Buffer は閲覧数をメモリ上に貯めます。ゼロ値では使えません。New を使ってください。
//
// **これが本番の実装です** (選択肢 D)。
type Buffer struct {
	mu sync.Mutex
	// counts は thread_id ごとの未反映の増分です。
	counts map[int64]int64
	gate   *visitorGate

	dedupeWindow time.Duration
	maxVisitors  int

	// now は時刻の取得です。**テストから差し替えます。**
	// フラッシュ間隔を待つテストは遅く不安定になるため、
	// 時刻とフラッシュ契機の両方を注入可能にしてあります (ADR 0006)。
	now func() time.Time
}

var _ Recorder = (*Buffer)(nil)

// Option は Buffer の設定です。
type Option func(*Buffer)

// WithDedupeWindow は重複抑制の時間を指定します。
//
// **0 は「抑制しない」を意味します。** 未指定と区別する必要があるため、
// 負値だけを無視します。0 を未指定として既定 (10 分) に戻していた頃は、
// **Phase 4 で閲覧の負荷をかけようとしても 1 回しか数えられず、
// DB への UPDATE がほとんど発生しませんでした** ——
// 同期版とバッファ版の比較が成立しません。
func WithDedupeWindow(d time.Duration) Option {
	return func(b *Buffer) {
		if d >= 0 {
			b.dedupeWindow = d
		}
	}
}

// WithClock は時刻の取得を差し替えます (テスト用)。
func WithClock(now func() time.Time) Option {
	return func(b *Buffer) {
		if now != nil {
			b.now = now
		}
	}
}

// WithMaxVisitors は重複抑制の記憶量の上限を指定します。
func WithMaxVisitors(n int) Option {
	return func(b *Buffer) {
		if n > 0 {
			b.maxVisitors = n
		}
	}
}

// New はバッファを生成します。
func New(opts ...Option) *Buffer {
	b := &Buffer{
		counts:       make(map[int64]int64),
		dedupeWindow: DefaultDedupeWindow,
		maxVisitors:  DefaultMaxVisitors,
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(b)
	}
	// **オプションを適用してから作る。** 先に作ると、
	// WithDedupeWindow などが gate に反映されない。
	b.gate = newVisitorGate(b.dedupeWindow, b.maxVisitors, b.now)
	return b
}

// NewSync はその場で UPDATE する実装を作ります (選択肢 A)。
//
// **Phase 4 の比較用であり、本番では使いません。**
// これを本番経路に入れると、閲覧というまったく無関係な操作が
// コメント投稿の直列化失敗率を押し上げます。それが起きることを
// 数字で示すために残してあります。
func NewSync(sink Sink, opts ...Option) *SyncCounter {
	// 抑制の設定は Buffer と揃える (visitorGate の説明)。
	tmp := &Buffer{dedupeWindow: DefaultDedupeWindow, maxVisitors: DefaultMaxVisitors, now: time.Now}
	for _, opt := range opts {
		opt(tmp)
	}
	return &SyncCounter{
		sink: sink,
		gate: newVisitorGate(tmp.dedupeWindow, tmp.maxVisitors, tmp.now),
	}
}

// SyncCounter は閲覧のたびに UPDATE を打ちます (ADR 0006 の選択肢 A)。
//
// **本番では使いません。** ADR 0006 が挙げた 3 つの問題を、
// そのまま踏むための実装です。
type SyncCounter struct {
	sink Sink
	gate *visitorGate
}

var _ Recorder = (*SyncCounter)(nil)

// Record はその場で UPDATE を打ちます。
//
// **リクエストの応答時間に DB への往復がそのまま乗ります。**
// 加えて、同じ行への更新が閲覧のたびに走るため、人気スレッドほど
// 行ロックの待ちが伸びます (ADR 0006 の問題 1)。
//
// 失敗しても閲覧そのものは成功扱いにします —— 数えられなかっただけで、
// スレッドの取得は既に終わっているためです。
func (c *SyncCounter) Record(ctx context.Context, threadID int64, visitor string) bool {
	if threadID <= 0 || !c.gate.allow(threadID, visitor) {
		return false
	}
	if _, err := c.sink.IncrementViewCounts(ctx, []int64{threadID}, []int64{1}); err != nil {
		slog.WarnContext(ctx, "view_count_sync_failed",
			slog.Int64("thread_id", threadID),
			slog.String("error", err.Error()),
		)
		return false
	}
	return true
}

// Record は 1 件の閲覧を数えます。数えたら true を返します。
//
// visitor はセッション ID か、未ログインなら IP アドレスです。
// **空文字でも数えます** —— 識別できない訪問者を捨てると、
// 抑制のためのキーが無いというだけの理由で閲覧数が伸びなくなります。
// その場合は抑制が効かない (毎回数える) だけになります。
//
// **DB には触れません。** ここが冒頭の 2 を成立させている境界です。
func (b *Buffer) Record(_ context.Context, threadID int64, visitor string) bool {
	if threadID <= 0 || !b.gate.allow(threadID, visitor) {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.counts[threadID]++
	return true
}

// Flush は貯まった増分をまとめて反映します。反映した件数を返します。
//
// **フラッシュ中に来た閲覧は失いません。** 先にバッファを取り出して
// 空にするので、書き込みを待っている間の Record は新しいバッファに入ります。
//
// **書き込みに失敗したら増分を戻します。** 捨てると、DB の一時的な不調が
// そのまま閲覧数の欠落になります。戻した増分は次のフラッシュで再度試されます
// (加算は「差分を足す」操作なので、二重に足さない限り何度でも安全です)。
func (b *Buffer) Flush(ctx context.Context, sink Sink) (int64, error) {
	// **期限切れの抑制記録はここで捨てる。** 捨てないと seen が
	// 上限まで埋まったまま戻らず、抑制が事実上効かなくなる。
	b.gate.evictExpired()

	ids, increments := b.take()
	if len(ids) == 0 {
		return 0, nil
	}

	started := b.now()
	affected, err := sink.IncrementViewCounts(ctx, ids, increments)
	if err != nil {
		b.restore(ids, increments)
		return 0, err
	}

	var total int64
	for _, n := range increments {
		total += n
	}
	// 業務イベントとして残す (ADR 0010 の 4-6)。
	// **msg はイベント名。** 可変の情報はフィールドに出す。
	slog.LogAttrs(ctx, slog.LevelInfo, "view_count_flushed",
		slog.Int64("threads", int64(len(ids))),
		slog.Int64("increments", total),
		slog.Int64("elapsed_ms", b.now().Sub(started).Milliseconds()),
		slog.Int64("affected", affected),
	)
	return affected, nil
}

// take はバッファの中身を取り出して空にします。
//
// **thread_id の昇順で返します。** 複数のインスタンスが異なる順序で
// 同じ行集合を更新するとデッドロックします。順序を固定しておくと、
// ロックの獲得順が全インスタンスで一致し、循環待ちが構造的に起きません
// (SQL 側でも FOR UPDATE ... ORDER BY で担保しています)。
func (b *Buffer) take() (ids []int64, increments []int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.counts) == 0 {
		return nil, nil
	}

	ids = make([]int64, 0, len(b.counts))
	for id := range b.counts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	increments = make([]int64, 0, len(ids))
	for _, id := range ids {
		increments = append(increments, b.counts[id])
	}

	b.counts = make(map[int64]int64)
	return ids, increments
}

// restore は書き込みに失敗した増分をバッファへ戻します。
//
// **上書きではなく加算します。** 取り出してから失敗を知るまでの間に
// 新しい閲覧が入っている可能性があり、上書きするとそれを消してしまいます。
func (b *Buffer) restore(ids, increments []int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, id := range ids {
		b.counts[id] += increments[i]
	}
}

// Pending は未反映の増分を持つスレッド数を返します (テストと観測用)。
func (b *Buffer) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.counts)
}

// hashVisitor は訪問者の識別子をハッシュにします。
//
// 暗号用途ではありません。**元の値を保持しないため**であって、
// 秘匿のためではないので、FNV で十分です。
func hashVisitor(visitor string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(visitor))
	return h.Sum64()
}
