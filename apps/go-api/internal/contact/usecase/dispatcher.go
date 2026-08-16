package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"develop-experiments/apps/go-api/internal/contact/domain/model"
	"develop-experiments/apps/go-api/internal/contact/domain/repository"
)

// 送信ワーカーの既定値です。
const (
	// DefaultMaxAttempts は 1 件あたりの試行回数の上限です。
	//
	// **上限を設けないという選択肢はありません** (ADR 0008 決定 1)。
	// 送信できないメールが延々とプロバイダを叩き続けると、
	// まともなメールの送達にも響きます。
	//
	// 8 回・基準 1 分の指数バックオフだと、打ち切りまでおよそ 2 時間強に
	// なります。プロバイダの一時的な障害はその中に収まり、
	// 収まらない障害は人が気づくべき事象になります。
	DefaultMaxAttempts int32 = 8

	// DefaultBatchSize は 1 周回で確保する件数です。
	//
	// **小さく保ちます。** SMTP の往復を含むので 1 件あたりの時間が読めず、
	// 大きくするとシャットダウンの待ち合わせが長くなります
	// (画像の回収が 100 件で切っているのと同じ理由)。
	DefaultBatchSize int32 = 20

	// DefaultLease は確保した行を他のインスタンスから隠す時間です。
	//
	// **送信 1 件にかかる時間より十分に長く**します。短いと、送信中の行を
	// 別のインスタンスが拾って二重送信になります。
	// 長すぎると、プロセスが落ちたときの再送が遅れます。
	DefaultLease = 5 * time.Minute

	// DefaultBaseBackoff は 1 回目の失敗のあとに待つ時間です。
	DefaultBaseBackoff = 1 * time.Minute
	// DefaultMaxBackoff は 1 回の待ちの上限です。
	DefaultMaxBackoff = 30 * time.Minute

	// DefaultPendingAgeWarn は未送信の滞留を警告する閾値です。
	//
	// ADR 0008 の「引き受けるコスト」が挙げている
	// 「ワーカーが止まっていることに気づく仕組み」がこれになります。
	DefaultPendingAgeWarn = 30 * time.Minute
)

// maxLastErrorLength は last_error に残す長さの上限です。
//
// **SMTP のエラーは長くなりえます** (サーバの応答文がそのまま乗る)。
// 上限を置かないと、DB の 1 列に任意長の外部入力が入ります。
const maxLastErrorLength = 500

// DispatchResult は 1 周回の結果です。
type DispatchResult struct {
	// Sent は送信できた件数です。
	Sent int
	// Retried は失敗して次回へ回した件数です。
	Retried int
	// Failed は試行回数の上限を超えて打ち切った件数です。
	Failed int
}

// Total は扱った件数です。0 なら送るものが無かったことを意味します。
func (r DispatchResult) Total() int { return r.Sent + r.Retried + r.Failed }

// Dispatcher は未送信の問い合わせを運営へ送ります。
//
// **API プロセスの中で、定期処理として動きます** (ADR 0003 未決 #9)。
// つまり**レプリカの数だけ同時に走る**ので、確保は
// `FOR UPDATE SKIP LOCKED` で行い、同じ行を 2 つのインスタンスが
// 掴まないようにしてあります。
type Dispatcher struct {
	repo   repository.ContactRepository
	sender repository.MailSender

	maxAttempts int32
	batchSize   int32
	lease       time.Duration
	baseBackoff time.Duration
	maxBackoff  time.Duration
	warnAge     time.Duration
}

// DispatcherOption は Dispatcher の調整です。
type DispatcherOption func(*Dispatcher)

// WithMaxAttempts は試行回数の上限を差し替えます。0 以下は既定値へ丸めます。
func WithMaxAttempts(n int32) DispatcherOption {
	return func(d *Dispatcher) {
		if n > 0 {
			d.maxAttempts = n
		}
	}
}

// WithBatchSize は 1 周回で確保する件数を差し替えます。0 以下は既定値へ丸めます。
func WithBatchSize(n int32) DispatcherOption {
	return func(d *Dispatcher) {
		if n > 0 {
			d.batchSize = n
		}
	}
}

// WithBackoff はリトライの待ち時間を差し替えます。
//
// **テストのためにあります。** 既定値のままだと、リトライの検査に
// 実時間で 1 分かかります。
func WithBackoff(base, maximum time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if base > 0 {
			d.baseBackoff = base
		}
		if maximum > 0 {
			d.maxBackoff = maximum
		}
	}
}

// NewDispatcher は送信ワーカーを生成します。
func NewDispatcher(
	repo repository.ContactRepository, sender repository.MailSender, opts ...DispatcherOption,
) *Dispatcher {
	d := &Dispatcher{
		repo:        repo,
		sender:      sender,
		maxAttempts: DefaultMaxAttempts,
		batchSize:   DefaultBatchSize,
		lease:       DefaultLease,
		baseBackoff: DefaultBaseBackoff,
		maxBackoff:  DefaultMaxBackoff,
		warnAge:     DefaultPendingAgeWarn,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Dispatch は未送信の問い合わせを 1 周回ぶん送ります。
//
// **戻り値のエラーは DB の失敗だけです。** メールの送信失敗は
// エラーになりません —— 失敗は行に記録され、次の周回でやり直されます。
// ここでエラーを返すと、プロバイダが落ちている間じゅう
// スケジューラが ERROR を吐き続けることになります
// (ADR 0010 の 4-3: ERROR は人が対応するものだけ)。
//
// **1 周回ぶんだけ行います。** 残りは次の実行に任せてください
// (scheduler.Job の約束)。
func (d *Dispatcher) Dispatch(ctx context.Context) (DispatchResult, error) {
	var result DispatchResult

	messages, err := d.repo.ClaimPending(ctx, d.lease, d.batchSize)
	if err != nil {
		return result, err
	}

	for _, m := range messages {
		// **中断されたら、そこで止めます。**
		//
		// 確保済みの行はリースが切れれば次のプロセスが拾い直すので、
		// 打ち切っても失われません。逆に、シャットダウン中に
		// 残り全件の SMTP を待つと猶予を使い切ります。
		if isDone(ctx) {
			break
		}

		sendErr := d.sender.Send(ctx, repository.Notification{
			ContactID: m.ID,
			Body:      composeBody(m),
		})
		if sendErr == nil {
			if markErr := d.repo.MarkSent(ctx, m.ID); markErr != nil {
				// **送信は済んでいます。** ここで返すと、この行は
				// pending のままリース切れを待ち、もう一度送られます
				// (at-least-once として引き受けたとおり)。
				return result, markErr
			}
			slog.InfoContext(ctx, "contact_mail_sent",
				slog.Int64("contact_id", m.ID),
				slog.Int("attempt", int(m.AttemptCount)),
			)
			result.Sent++
			continue
		}

		outcome, recordErr := d.recordFailure(ctx, m, sendErr)
		if recordErr != nil {
			return result, recordErr
		}
		switch outcome {
		case outcomeFailed:
			result.Failed++
		default:
			result.Retried++
		}
	}

	d.observePending(ctx)
	return result, nil
}

// isDone は ctx が終了しているかを返します。
//
// **`ctx.Err() != nil` を直接書きません。** 直後に `return result, nil` が
// 来るため、「エラーが非 nil なのに nil を返している」形として
// 静的解析 (nilerr) に見えます。
//
// 指摘としては正しくありませんが、**ここでの中断は失敗ではない**という
// 意図を型で表しておくほうが読む側にも伝わります ——
// 確保済みの行はリース切れで拾い直されるので、何も失われません。
func isDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// failureOutcome は送信失敗をどう扱ったかです。
type failureOutcome int

const (
	outcomeRetried failureOutcome = iota
	outcomeFailed
)

// recordFailure は送信の失敗を行に記録します。
//
// **試行回数の判定は確保時の値で行います。** ClaimPending が確保の時点で
// attempt_count を増やしているので、ここに届いている
// m.AttemptCount は「今回を含めた試行回数」になります。
func (d *Dispatcher) recordFailure(
	ctx context.Context, m model.Message, sendErr error,
) (failureOutcome, error) {
	reason := truncate(sendErr.Error(), maxLastErrorLength)

	// **イベント名は ADR 0010 の 4-6 が例示しているものに揃えます。**
	// (contact_mail_failed / contact_id / attempt / error)
	// Athena から数えるときに、名前が揃っていないと探せません。
	slog.WarnContext(ctx, "contact_mail_failed",
		slog.Int64("contact_id", m.ID),
		slog.Int("attempt", int(m.AttemptCount)),
		slog.String("error", reason),
	)

	if m.AttemptCount >= d.maxAttempts {
		if err := d.repo.Fail(ctx, m.ID, reason); err != nil {
			return outcomeFailed, err
		}
		// **ここだけ ERROR です。** 打ち切りは人が対応するもの ——
		// 受け付けた問い合わせが 1 件、誰にも届かないまま残ります
		// (ADR 0010 の 4-3)。
		slog.ErrorContext(ctx, "contact_mail_gave_up",
			slog.Int64("contact_id", m.ID),
			slog.Int("attempts", int(m.AttemptCount)),
			slog.String("error", reason),
		)
		return outcomeFailed, nil
	}

	if err := d.repo.Reschedule(ctx, m.ID, d.backoff(m.AttemptCount), reason); err != nil {
		return outcomeRetried, err
	}
	return outcomeRetried, nil
}

// backoff は attempt 回目 (1 起点) の失敗のあとに待つ時間を返します。
//
// **ゆらぎ (jitter) を入れていません。** postgres の retrier は
// full jitter を使いますが、あちらは**同じ行を取り合う**トランザクションを
// ばらすためでした。こちらは SKIP LOCKED で行が重ならないので、
// 揃って再実行しても互いに競合しません。
//
// 待ち時間が DB の列 (next_attempt_at) にあるので、
// **プロセスが落ちても待ちは失われません。** Phase 2 のプロセス内リトライ
// との違いがここに出ます。
func (d *Dispatcher) backoff(attempt int32) time.Duration {
	delay := d.baseBackoff
	for i := int32(1); i < attempt; i++ {
		if delay >= d.maxBackoff {
			return d.maxBackoff
		}
		delay *= 2
	}
	if delay > d.maxBackoff {
		return d.maxBackoff
	}
	return delay
}

// observePending は未送信の滞留を観測します。
//
// **これが無いと、送信が止まっても誰も気づきません** ——
// API は 202 を返し続け、問い合わせは静かに溜まります
// (ADR 0008 の「引き受けるコスト」)。
//
// **観測できるのはワーカーが動いているときだけ**です。
// ワーカーごと止まればこのログも出なくなるので、
// 監視側では「値が閾値を超えたこと」ではなく
// 「一定時間このイベントが出ていないこと」も見る必要があります。
//
// 失敗しても Dispatch は成功のままにします。観測のために
// 送信そのものを失敗扱いにする理由がありません。
func (d *Dispatcher) observePending(ctx context.Context) {
	oldest, ok, err := d.repo.OldestPending(ctx)
	if err != nil {
		slog.WarnContext(ctx, "contact_pending_probe_failed", slog.String("error", err.Error()))
		return
	}
	if !ok {
		return
	}

	age := time.Since(oldest)
	// **ミリ秒の整数で出します** (ADR 0010 の 4-4)。
	// slog.Duration は JSON ハンドラでナノ秒の整数になり、
	// Athena で毎回 / 1e6 を書くことになります。
	level := slog.LevelInfo
	if age >= d.warnAge {
		level = slog.LevelWarn
	}
	slog.Log(ctx, level, "contact_pending_age",
		slog.Int64("oldest_age_ms", age.Milliseconds()))
}

// composeBody は運営へ送る本文を組み立てます。
//
// **利用者の入力が入るのはここだけです** (ADR 0008 決定 3)。
// 差出人・宛先・件名は送信側 (internal/infrastructure/mail) が
// こちらの入力を一切使わずに組み立てます。
//
// **ラベルを付けて並べるだけにします。** 引用符で囲ったり整形したりすると、
// 入力に同じ記号が含まれたときに境界が曖昧になります ——
// 読む相手は人間なので、区切りは行で十分です。
func composeBody(m model.Message) string {
	var b strings.Builder

	fmt.Fprintf(&b, "問い合わせ番号: %d\n", m.ID)
	fmt.Fprintf(&b, "受付日時: %s\n", m.CreatedAt.UTC().Format(time.RFC3339))
	// **ログイン済みかどうかを書きます。** 本文中のアドレスは検証されて
	// いないので、「名乗っている人」と「ログインしている人」は別です。
	// 対応する側がそれを取り違えないよう、内部 ID を添えます。
	if m.UserID != nil {
		fmt.Fprintf(&b, "ログイン: あり (user_id=%d)\n", *m.UserID)
	} else {
		b.WriteString("ログイン: なし (匿名)\n")
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "氏名: %s\n", m.Name)
	// **このアドレスへ自動返信は送られていません** (決定 2)。
	// 返信は人間が手で行います。
	fmt.Fprintf(&b, "メールアドレス (未検証): %s\n", m.Email)
	fmt.Fprintf(&b, "件名: %s\n", m.Subject)
	b.WriteString("\n---\n")
	b.WriteString(m.Body)
	b.WriteString("\n")

	return b.String()
}

// truncate は文字列を上限まで切り詰めます。
//
// **ルーン境界で切ります。** バイト数で切ると、日本語のエラーメッセージが
// 途中で壊れて不正な UTF-8 になり、DB への保存で落ちます。
func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}

// errNoSender は送信手段が結線されていない場合のエラーです。
//
// **Dispatcher を nil の sender で作らせないための番兵**ではなく、
// 「設定が無い環境では Job ごと登録しない」ことの裏取りとして置いてあります
// (main.go を参照)。ここに来るのは結線の誤りです。
var errNoSender = errors.New("contact: MailSender が結線されていません")

// Validate は結線が済んでいるかを返します。main が起動時に呼びます。
func (d *Dispatcher) Validate() error {
	if d.sender == nil {
		return errNoSender
	}
	return nil
}
