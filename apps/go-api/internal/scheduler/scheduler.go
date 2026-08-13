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
			slog.Warn("定期処理の間隔が未設定のため既定値を使います",
				slog.String("job", job.Name),
				slog.Duration("interval", DefaultInterval),
			)
			job.Interval = DefaultInterval
		}
		normalized = append(normalized, job)
	}
	return &Scheduler{jobs: normalized}
}

// Start は各 Job を goroutine で回し始めます。
//
// ctx がキャンセルされるまで動き続けます。**戻り値はありません** ——
// 呼び出し側 (main) は HTTP サーバの終了を待つので、
// ここで待ち合わせを増やすとシャットダウンの経路が 2 本になります。
func (s *Scheduler) Start(ctx context.Context) {
	for _, job := range s.jobs {
		go s.run(ctx, job)
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
			slog.InfoContext(ctx, "定期処理を停止しました", slog.String("job", job.Name))
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
			slog.Log(ctx, level, "定期処理が失敗しました",
				slog.String("job", job.Name),
				slog.String("error", err.Error()),
			)
		}
		slog.DebugContext(ctx, "定期処理が完了しました",
			slog.String("job", job.Name),
			slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
		)

		timer.Reset(job.Interval)
	}
}
