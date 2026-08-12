package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
)

// トランザクションとリトライの土台です。設計は docs/adr/0019-comment-concurrency.md。
//
// **リトライは永続化層に置きます** (ADR 0019 決定 3)。
// トランザクションの境界がこの層にあり、リトライは
// 「トランザクション全体を最初からやり直す」ものだからです。
// ユースケース層はリトライの存在を知りません。

// RetryPolicy は、リトライ可能な競合に当たったときの再実行の方針です。
//
// 既定値は docs/adr/0019-comment-concurrency.md の決定 3 の表と同じです。
type RetryPolicy struct {
	// MaxAttempts は初回を含む最大試行回数です。1 ならリトライしません。
	//
	// **上限を設けない選択肢はありません。** 競合が続く状況で無限に
	// やり直すと、リトライそのものが DB への負荷になって競合をさらに増やします。
	MaxAttempts int
	// BaseDelay は 1 回目の失敗のあとに待つ時間の基準です。
	// 0 以下なら待たずに再実行します (テスト用)。
	BaseDelay time.Duration
	// MaxDelay は 1 回の待ちの上限です。
	MaxDelay time.Duration

	// sleep は待ち時間の実装です。nil なら実時間で待ちます。
	// テストから差し替えるためだけに存在します。
	sleep func(context.Context, time.Duration) error
}

// DefaultRetryPolicy は既定の方針です。
//
// 数値の根拠 (ADR 0019 決定 3):
//   - 待ち時間の基準が短いのは、競合相手のコミットが数ミリ秒で終わるため。
//     64 ms まで伸びる状況は競合率が異常であり、待つより諦めて
//     クライアントに 409 を返したほうが全体のスループットが上がる
//   - **試行回数は実測で決めています。** 初版は 5 でしたが、
//     同一スレッドへ 8 並列で投稿しただけで ssi が上限に張り付き、
//     32 並列では 32 件中 9 件が 409 になりました
//     (make concurrency-probe)。12 まで上げると 32 並列でも
//     全件が通り、実際に使われた試行回数は最大 7 でした
var DefaultRetryPolicy = RetryPolicy{
	MaxAttempts: 12,
	BaseDelay:   2 * time.Millisecond,
	MaxDelay:    64 * time.Millisecond,
}

// backoff は attempt 回目 (1 起点) の失敗のあとに待つ時間を返します。
//
// **full jitter を使います。** 指数バックオフだけでは、
// 同時に弾かれた複数のトランザクションが同じ時刻に揃って再実行され、
// 同じ競合をもう一度起こします。待ち時間を [0, 指数値] の一様乱数にすると、
// 再実行の位相がばらけます。
func (p RetryPolicy) backoff(attempt int) time.Duration {
	if p.BaseDelay <= 0 {
		return 0
	}

	ceiling := p.MaxDelay
	// 1 << 62 を超えると time.Duration (int64) が溢れるため、
	// シフトする前に打ち切ります。attempt は MaxAttempts で抑えられている
	// はずですが、方針を外から渡せる以上ここで閉じておきます。
	if shift := attempt - 1; shift < 62 {
		if d := p.BaseDelay << shift; d > 0 && d < ceiling {
			ceiling = d
		}
	}
	if ceiling <= 0 {
		return 0
	}

	// 上限そのものも選ばれうるように +1 します。
	//
	//nolint:gosec // 待ち時間を散らすためのゆらぎであり、秘密ではない。
	// 予測されて困る値ではないので、crypto/rand を使う理由がない
	// (セッション ID などとは要求が違う。ADR 0005)。
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// sleepFor は待ち時間を消費します。ctx が終了したらそれを返します。
func (p RetryPolicy) sleepFor(ctx context.Context, d time.Duration) error {
	if p.sleep != nil {
		return p.sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retrier は「どのエラーをやり直すか」と「観測にどう出すか」を束ねたものです。
type retrier struct {
	policy RetryPolicy
	// retryable はやり直す価値のあるエラーかを判定します。
	retryable func(error) bool
	// attrs は競合のログに載せる追加フィールドです (thread_id / mode など)。
	attrs []slog.Attr
	// log は出力先です。nil なら既定のロガーを使います。
	//
	// **ログレベルの使い分けそのものが ADR の決定事項**なので
	// (ADR 0010 の 4-3)、それを検査できるように差し替え口を開けてあります。
	// 既定ロガーをテストから差し替える形にすると、
	// 並列実行したテストどうしが互いの出力を拾ってしまいます。
	log *slog.Logger
}

// logger は出力先を返します。
func (r retrier) logger() *slog.Logger {
	if r.log != nil {
		return r.log
	}
	return slog.Default()
}

// do は op を実行し、リトライ可能な失敗の間だけやり直します。
// 戻り値の 1 つ目は実際の試行回数です (成功時も含めて必ず 1 以上)。
//
// ログの出し方は docs/adr/0010-log-pipeline.md の 4-3 と
// docs/adr/0019-comment-concurrency.md の決定 4 に従います。
//
//   - 各試行の失敗は DEBUG。**リトライして成功したなら処理は完了しており、
//     人が対応する必要はありません。** ERROR にするとアラートが鳴り続けます
//   - 上限に達した失敗だけが ERROR
//
// 成功したことの記録 (INFO) は呼び出し側が出します。
// 何を投稿したかはこの層の関心ではないためです。
func (r retrier) do(ctx context.Context, op func() error) (int, error) {
	attempts := max(r.policy.MaxAttempts, 1)

	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = op()
		if err == nil {
			return attempt, nil
		}
		if !r.retryable(err) {
			return attempt, err
		}

		if attempt == attempts {
			r.logger().LogAttrs(ctx, slog.LevelError, "serialization_retry_exhausted",
				append(r.logAttrs(err), slog.Int("attempts", attempt))...)
			return attempt, err
		}

		r.logger().LogAttrs(ctx, slog.LevelDebug, "serialization_failure",
			append(r.logAttrs(err), slog.Int("attempt", attempt))...)

		if waitErr := r.policy.sleepFor(ctx, r.policy.backoff(attempt)); waitErr != nil {
			// クライアントが切った、あるいは締め切りが来た。
			// **競合と中断の両方を残します。** 片方だけにすると、
			// ログでは競合が消え、呼び出し側では中断の理由が消えます。
			return attempt, errors.Join(err, waitErr)
		}
	}

	// MaxAttempts >= 1 なのでループ内で必ず return します。
	return attempts, err
}

// logAttrs は競合のログに載せるフィールドを組み立てます。
// SQLSTATE を載せるのは、直列化失敗 (40001) と採番の衝突 (23505) を
// あとから区別できるようにするためです。
func (r retrier) logAttrs(err error) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(r.attrs)+1)
	attrs = append(attrs, r.attrs...)
	if code := sqlState(err); code != "" {
		attrs = append(attrs, slog.String("sqlstate", code))
	}
	return attrs
}

// runInTx は fn を 1 つのトランザクションの中で実行します。
//
// **fn の中で返したエラーはロールバックになります。**
// 途中まで書いた結果は残りません。SSI で中断されたトランザクションの
// 途中結果は使えないため、これが唯一正しい扱いになります。
//
// 【コミット時にも直列化失敗が返る】
// SERIALIZABLE の 40001 は文の実行中とは限らず、COMMIT で初めて返ることがあります。
// そのため**リトライは runInTx の呼び出しごと**包む必要があります
// (この関数の中でリトライしてはいけません)。
func runInTx(
	ctx context.Context, pool *pgxpool.Pool, iso pgx.TxIsoLevel,
	fn func(*sqlcgen.Queries) error,
) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
	if err != nil {
		return fmt.Errorf("トランザクションを開始できませんでした: %w", err)
	}
	// Commit 済みのトランザクションへの Rollback は pgx が no-op にするため、
	// 正常系でも安全に呼べます。panic した場合もここで巻き戻ります。
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(sqlcgen.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
