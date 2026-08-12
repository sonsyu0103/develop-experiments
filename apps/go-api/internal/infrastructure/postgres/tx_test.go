package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
)

// retryEverything はすべての失敗をやり直す判定です。
func retryEverything(error) bool { return true }

// noDelay は待ち時間を消費しない方針です。
// **バックオフの検査は backoff() を直接見る側で行い**、
// リトライの回数と分岐を見るテストでは実時間を待ちません。
func noDelay(attempts int) RetryPolicy {
	return RetryPolicy{
		MaxAttempts: attempts,
		sleep:       func(context.Context, time.Duration) error { return nil },
	}
}

func TestRetrierDoStopsWhenOpSucceeds(t *testing.T) {
	t.Parallel()

	calls := 0
	r := retrier{policy: noDelay(5), retryable: retryEverything, log: discardLogger()}

	attempts, err := r.do(t.Context(), func() error {
		calls++
		return nil
	})

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if attempts != 1 || calls != 1 {
		t.Errorf("attempts = %d, calls = %d, want 1, 1", attempts, calls)
	}
}

func TestRetrierDoRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	calls := 0
	r := retrier{policy: noDelay(5), retryable: retryEverything, log: discardLogger()}

	attempts, err := r.do(t.Context(), func() error {
		calls++
		if calls < 3 {
			return errors.New("競合")
		}
		return nil
	})

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// 上限を超えて試行し続けないことを確かめる。
// **ここが緩むと、競合が続く状況でリトライ自体が DB への負荷になる。**
func TestRetrierDoStopsAtMaxAttempts(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("いつまでも競合")
	calls := 0
	r := retrier{policy: noDelay(3), retryable: retryEverything, log: discardLogger()}

	attempts, err := r.do(t.Context(), func() error {
		calls++
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if attempts != 3 || calls != 3 {
		t.Errorf("attempts = %d, calls = %d, want 3, 3", attempts, calls)
	}
}

// リトライ不可のエラーはやり直さない。
// 一意制約違反を無条件にやり直すと、永久に成功しない処理を繰り返すことになる。
func TestRetrierDoDoesNotRetryUnretryable(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("やり直しても無駄")
	calls := 0
	r := retrier{
		policy:    noDelay(5),
		retryable: func(error) bool { return false },
		log:       discardLogger(),
	}

	attempts, err := r.do(t.Context(), func() error {
		calls++
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if attempts != 1 || calls != 1 {
		t.Errorf("attempts = %d, calls = %d, want 1, 1", attempts, calls)
	}
}

// 待っている間に ctx が終わったら、競合と中断の両方を返す。
//
// 片方だけにすると、ログでは競合が消え (中断しか残らない)、
// 呼び出し側では中断の理由が消える (競合しか残らない)。
func TestRetrierDoJoinsContextErrorWithLastFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("競合")
	r := retrier{
		policy: RetryPolicy{
			MaxAttempts: 5,
			sleep: func(context.Context, time.Duration) error {
				return context.Canceled
			},
		},
		retryable: retryEverything,
		log:       discardLogger(),
	}

	attempts, err := r.do(t.Context(), func() error { return sentinel })

	if !errors.Is(err, sentinel) {
		t.Errorf("競合のエラーが失われている: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("中断のエラーが失われている: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (待ちに入る前に 1 回試している)", attempts)
	}
}

// ログレベルの使い分けは ADR 0010 の 4-3 で決めた仕様そのもの。
//
// **リトライして成功した直列化失敗を ERROR にしてはいけない。**
// ERROR はアラートの閾値と直結しており、
// Phase 2 が正しく動いているほど 40001 は発生するため、
// ここを取り違えると正常な状態でアラートが鳴り続ける。
func TestRetrierDoLogLevels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failures  int
		wantLevel map[string]string
	}{
		{
			name:      "リトライして成功したら ERROR を出さない",
			failures:  2,
			wantLevel: map[string]string{"serialization_failure": "DEBUG"},
		},
		{
			name:     "上限に達したら ERROR",
			failures: 99,
			wantLevel: map[string]string{
				"serialization_failure":         "DEBUG",
				"serialization_retry_exhausted": "ERROR",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			calls := 0
			r := retrier{
				policy:    noDelay(3),
				retryable: retryEverything,
				attrs:     []slog.Attr{slog.Int64("thread_id", 42)},
				log:       logger,
			}
			_, _ = r.do(t.Context(), func() error {
				calls++
				if calls <= tt.failures {
					return &pgconn.PgError{Code: codeSerializationFailure}
				}
				return nil
			})

			levels := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if line == "" {
					continue
				}
				var rec struct {
					Level    string `json:"level"`
					Msg      string `json:"msg"`
					ThreadID int64  `json:"thread_id"`
					SQLState string `json:"sqlstate"`
				}
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("ログが JSON として読めない: %v (%s)", err, line)
				}
				levels[rec.Msg] = rec.Level

				if rec.ThreadID != 42 {
					t.Errorf("%s に thread_id が載っていない (Athena で絞れない)", rec.Msg)
				}
				// 直列化失敗 (40001) と採番の衝突 (23505) を
				// あとから区別できるようにする。
				if rec.SQLState != codeSerializationFailure {
					t.Errorf("%s の sqlstate = %q, want %q",
						rec.Msg, rec.SQLState, codeSerializationFailure)
				}
			}

			for msg, want := range tt.wantLevel {
				if got := levels[msg]; got != want {
					t.Errorf("%s のレベル = %q, want %q (出たログ: %v)", msg, got, want, levels)
				}
			}
			if _, ok := levels["serialization_retry_exhausted"]; ok {
				if _, want := tt.wantLevel["serialization_retry_exhausted"]; !want {
					t.Error("リトライして成功したのに serialization_retry_exhausted が出ている")
				}
			}
		})
	}
}

// バックオフは [0, 指数値] の一様乱数になる (full jitter)。
//
// 上限だけを検査する。**下限を検査してはいけない** ——
// 0 が出るのは仕様であり、たまたま 0 が続くとテストが不安定になる。
func TestRetryPolicyBackoffBounds(t *testing.T) {
	t.Parallel()

	p := RetryPolicy{MaxAttempts: 5, BaseDelay: 2 * time.Millisecond, MaxDelay: 64 * time.Millisecond}

	tests := []struct {
		attempt int
		wantMax time.Duration
	}{
		{attempt: 1, wantMax: 2 * time.Millisecond},
		{attempt: 2, wantMax: 4 * time.Millisecond},
		{attempt: 3, wantMax: 8 * time.Millisecond},
		{attempt: 4, wantMax: 16 * time.Millisecond},
		// 指数が MaxDelay を超えたら、そこで頭打ちになる。
		{attempt: 10, wantMax: 64 * time.Millisecond},
		// シフトが int64 を溢れさせないこと。
		{attempt: 999, wantMax: 64 * time.Millisecond},
	}

	for _, tt := range tests {
		for i := 0; i < 200; i++ {
			got := p.backoff(tt.attempt)
			if got < 0 || got > tt.wantMax {
				t.Fatalf("backoff(%d) = %v, want [0, %v]", tt.attempt, got, tt.wantMax)
			}
		}
	}
}

// BaseDelay が 0 なら待たない (テストと naive モードのため)。
func TestRetryPolicyBackoffZeroBase(t *testing.T) {
	t.Parallel()

	p := RetryPolicy{MaxAttempts: 3, MaxDelay: time.Second}
	for attempt := 1; attempt <= 5; attempt++ {
		if got := p.backoff(attempt); got != 0 {
			t.Errorf("backoff(%d) = %v, want 0", attempt, got)
		}
	}
}

// full jitter が実際に散っていることを確かめる。
//
// **これが無いと「指数バックオフを実装したつもりで固定値を返している」
// 実装がテストを通ってしまう。** 待ち時間が揃うと、
// 弾かれたトランザクションが同じ時刻に再実行され、同じ競合を繰り返す。
func TestRetryPolicyBackoffIsJittered(t *testing.T) {
	t.Parallel()

	p := RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: time.Second}

	seen := map[time.Duration]struct{}{}
	for i := 0; i < 500; i++ {
		seen[p.backoff(5)] = struct{}{}
	}

	// 上限 16ms のナノ秒解像度なので、散っていれば数百通りになる。
	// 固定値なら 1 通りしか出ない。
	if len(seen) < 10 {
		t.Errorf("待ち時間が %d 通りしか出ていない (jitter が効いていない)", len(seen))
	}
}

// 直列化失敗とデッドロックはやり直す。それ以外はやり直さない。
func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "直列化失敗", err: &pgconn.PgError{Code: codeSerializationFailure}, want: true},
		{name: "デッドロック", err: &pgconn.PgError{Code: codeDeadlockDetected}, want: true},
		{name: "一意制約違反", err: &pgconn.PgError{Code: codeUniqueViolation}, want: false},
		{name: "外部キー違反", err: &pgconn.PgError{Code: codeForeignKeyViolation}, want: false},
		{name: "apperr.ErrConflict", err: apperr.ErrConflict, want: true},
		{name: "ただのエラー", err: errors.New("boom"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsRetryable(tt.err); got != tt.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// レス番号の衝突だけをリトライ可能として扱う。
//
// **制約名は子パーティションのものが返る** (実測)。
// 親の名前だけで一致を見ると、この判定が永久に偽になり、
// unique モードが競合のたびに 409 を返すようになる。
func TestIsCommentSeqConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "子パーティションの索引名",
			err:  &pgconn.PgError{Code: codeUniqueViolation, ConstraintName: "comments_p5_thread_id_seq_idx"},
			want: true,
		},
		{
			name: "親の索引名",
			err:  &pgconn.PgError{Code: codeUniqueViolation, ConstraintName: "comments_thread_id_seq_idx"},
			want: true,
		},
		{
			name: "別の一意制約 (やり直しても永久に失敗する)",
			err:  &pgconn.PgError{Code: codeUniqueViolation, ConstraintName: "users_google_sub_key"},
			want: false,
		},
		{
			name: "主キー違反",
			err:  &pgconn.PgError{Code: codeUniqueViolation, ConstraintName: "comments_pkey"},
			want: false,
		},
		{
			name: "直列化失敗はこの判定では偽",
			err:  &pgconn.PgError{Code: codeSerializationFailure},
			want: false,
		},
		{name: "PostgreSQL 由来でない", err: errors.New("boom"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isCommentSeqConflict(tt.err); got != tt.want {
				t.Errorf("isCommentSeqConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// discardLogger は出力を捨てるロガーです。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// ---------------------------------------------------------------------------
// runInTx
// ---------------------------------------------------------------------------

// fakeTx は Commit / Rollback だけを差し替えたトランザクションです。
//
// pgx.Tx を埋め込んでいるので、それ以外のメソッドを呼ぶと nil で落ちます。
// **意図的にそうしています** —— runInTx が使っていないはずのメソッドを
// 使い始めたら、テストが静かに通るのではなく落ちてほしいためです。
type fakeTx struct {
	pgx.Tx

	commitErr  error
	committed  bool
	rolledBack bool
}

func (f *fakeTx) Commit(context.Context) error {
	f.committed = true
	return f.commitErr
}

func (f *fakeTx) Rollback(context.Context) error {
	f.rolledBack = true
	return nil
}

type fakeBeginner struct {
	tx       *fakeTx
	beginErr error
	gotIso   pgx.TxIsoLevel
}

func (f *fakeBeginner) BeginTx(_ context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	f.gotIso = opts.IsoLevel
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return f.tx, nil
}

// COMMIT が返した直列化失敗も apperr.ErrConflict へ翻訳されること。
//
// **実 DB を立てないと踏めない経路。** SERIALIZABLE の 40001 は
// 文の実行中とは限らず、COMMIT で初めて返ることがあります。
// ここを素通しすると、リトライを使い切った最後の失敗が COMMIT 由来だったとき、
// 409 であるべき応答が 500 になります (試行中は IsRetryable が
// 生の PgError を見るので、この不具合は最終試行でしか表に出ません)。
func TestRunInTx_TranslatesCommitError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		commitErr error
		wantErrIs error
		wantRetry bool
	}{
		{
			name:      "直列化失敗",
			commitErr: &pgconn.PgError{Code: codeSerializationFailure},
			wantErrIs: apperr.ErrConflict,
			wantRetry: true,
		},
		{
			name:      "デッドロック",
			commitErr: &pgconn.PgError{Code: codeDeadlockDetected},
			wantErrIs: apperr.ErrConflict,
			wantRetry: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db := &fakeBeginner{tx: &fakeTx{commitErr: tt.commitErr}}

			err := runInTx(t.Context(), "テスト", db, pgx.Serializable,
				func(pgx.Tx, *sqlcgen.Queries) error { return nil })

			if !errors.Is(err, tt.wantErrIs) {
				t.Errorf("err = %v, want %v を含む (409 にならず 500 になる)", err, tt.wantErrIs)
			}
			if got := IsRetryable(err); got != tt.wantRetry {
				t.Errorf("IsRetryable = %v, want %v", got, tt.wantRetry)
			}
			if db.gotIso != pgx.Serializable {
				t.Errorf("分離レベル = %q, want %q", db.gotIso, pgx.Serializable)
			}
		})
	}
}

// COMMIT が成功したらエラーを作らないこと (translateError の nil 素通し)。
func TestRunInTx_CommitSuccess(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}

	if err := runInTx(t.Context(), "テスト", db, pgx.ReadCommitted,
		func(pgx.Tx, *sqlcgen.Queries) error { return nil }); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !tx.committed {
		t.Error("COMMIT されていない")
	}
	// Commit 済みへの Rollback は pgx が no-op にするので、呼ばれること自体は正しい。
	if !tx.rolledBack {
		t.Error("defer の Rollback が呼ばれていない (panic 時に巻き戻らない)")
	}
}

// fn が失敗したら COMMIT しないこと。
// **途中まで書いた結果を残してはいけません。**
func TestRunInTx_RollsBackOnError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("採番に失敗")
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}

	err := runInTx(t.Context(), "テスト", db, pgx.Serializable,
		func(pgx.Tx, *sqlcgen.Queries) error { return sentinel })

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if tx.committed {
		t.Error("fn が失敗したのに COMMIT された")
	}
	if !tx.rolledBack {
		t.Error("ロールバックされていない")
	}
}

// BEGIN の失敗も翻訳されること。
func TestRunInTx_TranslatesBeginError(t *testing.T) {
	t.Parallel()

	db := &fakeBeginner{beginErr: &pgconn.PgError{Code: codeSerializationFailure}}

	err := runInTx(t.Context(), "テスト", db, pgx.Serializable,
		func(pgx.Tx, *sqlcgen.Queries) error {
			t.Error("BEGIN に失敗したのに fn が呼ばれた")
			return nil
		})

	if !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("err = %v, want apperr.ErrConflict を含む", err)
	}
}

// MaxDelay を設定していない方針で、待ち時間が丸ごと 0 にならないこと。
//
// **上限側から下げる書き方をすると、ここが静かに 0 になります。**
// 指数バックオフを設定したつもりで full jitter の目的が消え、
// 競合中の DB を全員で叩き続けることになります。
func TestRetryPolicyBackoff_NoMaxDelay(t *testing.T) {
	t.Parallel()

	p := RetryPolicy{MaxAttempts: 5, BaseDelay: 10 * time.Millisecond}

	seen := map[time.Duration]struct{}{}
	for i := 0; i < 500; i++ {
		got := p.backoff(3)
		// 3 回目の指数値は 10ms << 2 = 40ms。
		if got < 0 || got > 40*time.Millisecond {
			t.Fatalf("backoff(3) = %v, want [0, 40ms]", got)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 10 {
		t.Errorf("待ち時間が %d 通りしか出ていない (上限が 0 に潰れている)", len(seen))
	}
}

// 試行を重ねるほど待ち時間が伸びること。
//
// **上限の検査だけでは、指数で伸ばすのをやめても気づけません。**
// 常に BaseDelay を返す実装は [0, 上限] にすべて収まるため、
// TestRetryPolicyBackoffBounds を素通りします (変異プローブで実測)。
// 伸びないと、競合が続くときに全員が同じ短い間隔で再実行を繰り返します。
func TestRetryPolicyBackoff_GrowsWithAttempt(t *testing.T) {
	t.Parallel()

	// MaxDelay を十分大きく取り、頭打ちの影響を受けないようにする。
	p := RetryPolicy{MaxAttempts: 10, BaseDelay: time.Millisecond, MaxDelay: time.Hour}

	maxOf := func(attempt int) time.Duration {
		var got time.Duration
		for i := 0; i < 500; i++ {
			got = max(got, p.backoff(attempt))
		}
		return got
	}

	// 1 回目の上限は 1ms、5 回目は 16ms。
	// full jitter なので観測値は上限そのものではないが、
	// 500 回も引けば桁が違うことは確実に出る。
	first, fifth := maxOf(1), maxOf(5)
	if fifth <= first*4 {
		t.Errorf("attempt=1 の最大 %v に対して attempt=5 の最大が %v しかない "+
			"(指数で伸びていない)", first, fifth)
	}
}
