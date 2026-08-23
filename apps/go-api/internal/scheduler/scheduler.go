// Package scheduler は API プロセス内で動く定期処理を束ねます。
//
// **別プロセス (ワーカー) に分けていません** (docs/adr/0003-open-questions.md 未決 #9)。
//
//   - 閲覧数のフラッシュ (Phase 7) は**プロセス内でなければ動きません**。
//     バッファがそのプロセスのメモリにあるためです。つまりスケジューラは
//     どうせ API プロセスに要ります
//   - デプロイ単位を増やさずに済みます (ADR 0004 のモジュラモノリスと揃う)
//
// 代償として、**レプリカの数だけ同じ処理が同時に走ります。**
// そのため、ここに登録する処理は次の 2 つを満たす必要があります。
//
//  1. 何度実行しても結果が変わらない (冪等)
//  2. 同じ行を 2 つのプロセスが掴まない
//     (`SELECT ... FOR UPDATE SKIP LOCKED` または件数上限つきの DELETE)
package scheduler

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// Job は定期的に実行する処理です。
//
// **エラーを返しても止まりません。** 一時的な失敗 (DB の瞬断など) で
// 掃除が永久に止まるほうが害が大きいためです。記録は残します。
type Job struct {
	// Name はログに出す識別子です。
	Name string
	// Interval は実行間隔です。
	Interval time.Duration
	// Run は実処理です。**自分で「終わるまで繰り返す」必要はありません** ——
	// 1 周回ぶんだけ行い、残りは次の実行に任せてください。
	Run func(context.Context) error
}

// Scheduler は登録された Job を回します。
type Scheduler struct {
	jobs []Job
	// inFlight は実行中の Job を数えます。Wait が待ち合わせに使います。
	inFlight sync.WaitGroup
}

// DefaultInterval は Interval が指定されなかった場合の間隔です。
const DefaultInterval = 10 * time.Minute

// New はスケジューラを生成します。
//
// **Interval が 0 以下の Job は既定値へ丸めます。**
// 丸めないと rand.Int64N(0) が panic し、goroutine の中なので
// recover されず**プロセスごと落ちます**。
// Job は「実処理は main が渡す」公開 API なので、
// 設定値由来の間隔 (未設定 = 0) がそのまま来る余地があります。
//
// 起動を止めない側に倒すのは、定期処理が動かないことより
// API 全体が起動しないことのほうが害が大きいためです
// (認証やストレージの設定と同じ考え方)。
func New(jobs ...Job) *Scheduler {
	normalized := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if job.Interval <= 0 {
			slog.Warn("scheduler_interval_defaulted",
				slog.String("job", job.Name),
				// **Duration にしない。** JSON ハンドラではナノ秒の整数になり、
				// Athena で毎回 / 1e6 を書くことになる (ADR 0010 の 4-4)。
				slog.Int64("interval_ms", DefaultInterval.Milliseconds()),
			)
			job.Interval = DefaultInterval
		}
		normalized = append(normalized, job)
	}
	return &Scheduler{jobs: normalized}
}

// Start は各 Job を goroutine で回し始めます。
//
// ctx がキャンセルされるまで動き続けます。
// **終了は Wait で待ち合わせてください** —— 下の Wait の説明を参照。
func (s *Scheduler) Start(ctx context.Context) {
	for _, job := range s.jobs {
		s.inFlight.Go(func() { s.run(ctx, job) })
	}
}

// Wait は実行中の Job が終わるまで待ちます。ctx が先に切れたらその error を返します。
//
// **初版はこれを持っていませんでした。** Start は起動して返るだけで、
// main は HTTP サーバの終了しか待っていなかったため、
// **シャットダウンのたびに実行中の Job がプロセスごと打ち切られていました。**
//
// 打ち切りが単に「次の周回に持ち越し」で済む処理ばかりなら害はありません。
// しかし画像の回収は、
//
//  1. 対象を確保する (object_reclaimed_at を書いて commit)
//  2. S3 のオブジェクトを消す
//  3. DB 行を消す
//
// と進むので、1 の後で打ち切られると**確保済みの行が索引から外れたまま残り、
// 二度と拾われません** (S3 の実体ごと漏れる)。デプロイのたびに
// 最大 100 件です (レビュー指摘)。
//
// そのため待ち合わせを 1 本足しました。**シャットダウンの経路が 2 本に
// なるのを避けて省いていた**のですが、避けた結果が「消し損ねが恒久的に
// 残る」だったので、順番に待つ形に改めます。
func (s *Scheduler) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run は 1 つの Job を回します。
func (s *Scheduler) run(ctx context.Context, job Job) {
	// **起動直後に全レプリカが同時に走らないよう、ばらつかせます。**
	// 揃えると、デプロイのたびに同じ瞬間へ負荷が集中します。
	// 間隔の 0〜100% を初回の待ちにします。
	//
	// 乱数は暗号用途ではないので math/rand/v2 で十分です。
	initial := time.Duration(rand.Int64N(int64(job.Interval))) //nolint:gosec // 分散のための乱数
	timer := time.NewTimer(initial)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "scheduler_job_stopped", slog.String("job", job.Name))
			return
		case <-timer.C:
		}

		start := time.Now()
		if err := job.Run(ctx); err != nil {
			// **止めません。** 一時的な失敗で掃除が永久に止まるほうが害が大きい。
			//
			// ctx のキャンセル由来なら、次の select で抜けます。
			// ここで ERROR を出すと、シャットダウンのたびにアラートが鳴るので
			// 区別します (ADR 0010 の 4-3: ERROR は人が対応するものだけ)。
			level := slog.LevelError
			if ctx.Err() != nil {
				level = slog.LevelInfo
			}
			slog.Log(ctx, level, "scheduler_job_failed",
				slog.String("job", job.Name),
				slog.String("error", err.Error()),
			)
		}
		slog.DebugContext(ctx, "scheduler_job_completed",
			slog.String("job", job.Name),
			slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
		)

		// **タイマーを張り直すのは Run のあと。**
		//
		// 前に置くと、Run が Interval より長引いたときに走行中へ発火し、
		// 次の select で ctx.Done() と timer.C が**どちらも準備完了**になる。
		// select はランダムに選ぶので、**停止のおよそ半分で、死んだ ctx の
		// まま新しい 1 周が始まる** —— DB 呼び出しは全部失敗し、
		// その無駄な 1 周ぶん Wait がシャットダウンの猶予を食う。
		// (レビューは「実行前に ctx を見直していない」と指摘したが、実際は
		//  この順序で防げている。順序が効いていることは変異で実測した ——
		//  **Reset を Run の前へ「移動」すると検査は 5/5 で落ちる。**
		//  「前へ足す」変異は末尾の Reset に打ち消されて通るだけなので、
		//  そちらで確かめると「効かない検査」だと誤読する。)
		timer.Reset(job.Interval)
	}
}
