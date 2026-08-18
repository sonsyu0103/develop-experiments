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
	// 8 回・基準 1 分の指数バックオフで、上限 30 分の頭打ちを含めると
	// 打ち切りまで **1+2+4+8+16+30+30 = 91 分**になります
	// (初版のコメントは「2 時間強」と書いていましたが、
	//  頭打ちを勘定に入れていませんでした。レビュー指摘)。
	// プロバイダの一時的な障害はその中に収まり、
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

	claimedAt := time.Now()

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

		// **リースを使い切る前に止めます** (レビュー指摘)。
		//
		// リースは確保の時点でバッチ全件に一括で掛かります。周回の所要が
		// リースを超えると、**まだ処理していない行が他のレプリカから
		// 見えるようになり**、こちらが送っている最中に同じ行を送られます。
		//
		// 超えうる量は設定次第です。MAIL_TIMEOUT_SECONDS = 10 の場合、
		// 1 接続あたり最悪 20 秒 (DialContext 10 秒 + SetDeadline 10 秒)。
		// 控えを足したことで **1 件あたり 2 接続**になったので、
		//
		//	20 件 x 2 接続 x 20 秒 = 800 秒 > リース 300 秒
		//
		// **バッチサイズやリースの既定値をいじって釣り合わせません。**
		// どちらも「速い相手」を前提に選んだ値で、釣り合いは
		// MAIL_TIMEOUT_SECONDS にも依存します —— 3 つの設定の積が
		// 4 つ目を超えない、という不変条件を人が守り続ける形になります。
		//
		// 残りを次の周回に任せるほうが安全です。**何も失われません** ——
		// 未処理の行はリースが切れれば拾い直されます。
		if elapsed := time.Since(claimedAt); elapsed >= d.leaseBudget() {
			slog.InfoContext(ctx, "contact_lease_budget_reached",
				slog.Int64("elapsed_ms", elapsed.Milliseconds()),
				slog.Int("handled", result.Total()),
				slog.Int("claimed", len(messages)),
			)
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
			// **MarkSent の「後」に送ります。** 前に置くと、控えの SMTP の
			// 往復ぶんだけ「運営には届いたが行は pending」の窓が広がり、
			// そこで落ちると**運営への通知がもう 1 通届きます。**
			// 決定 1 が守ると決めたのは運営への通知だけなので、
			// 控えのために重複の窓を広げません。
			d.sendAutoReply(ctx, m)
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

// sendAutoReply は問い合わせをくれた本人へ控えを返します (ADR 0008 決定 2)。
//
// **戻り値がありません。失敗しても行の状態を変えません。**
// この時点で運営への通知は確定済み (MarkSent 済み) で、控えのために
// 問い合わせをもう一度送るのは本末転倒になります。
//
// 【リトライしない理由】
// 行に「控えを送ったか」を持たせればできますが、
//
//   - 列が増える (マイグレーション)
//   - 「運営には届いたが控えは未送信」という 3 つ目の状態が生まれ、
//     status = 'sent' が何を意味するのかが 1 行では言えなくなる
//   - 確保のクエリ (contact_pending_idx で引いている) の条件が増える
//
// のに対し、届かないことの実害は「受け付けたことの再確認ができない」
// だけです。それは 202 を受けた画面が既に伝えています。
//
// **リトライしない代わりに、送れなかったことは必ず記録します。**
// 静かに消えるのが一番まずい形になります。
func (d *Dispatcher) sendAutoReply(ctx context.Context, m model.Message) {
	// **nil なら送りません。** 匿名の問い合わせ、退会済みの利用者、
	// users.email が形式不正、の 3 つがここに来ます。
	//
	// **入力されたアドレス (m.Email) へ落とすフォールバックは書きません** ——
	// それが決定 2 が禁じたことそのものになります。
	if m.ReplyTo == nil {
		return
	}

	// **空の宛先も送りません** (レビュー指摘)。
	//
	// nil と違い、これは**起きてはいけない状態**です ——
	// VerifiedEmail のゼロ値はどのパッケージからでも書けるので、
	// 型だけでは防げません。素通しすると `RCPT TO:<>` になり、
	// **結線の誤りが SMTP の失敗として現れます。**
	// 匿名 (nil) と同じ扱いで黙って飛ばすと、今度は誰も気づきません。
	if m.ReplyTo.IsZero() {
		slog.ErrorContext(ctx, "contact_auto_reply_address_empty",
			slog.Int64("contact_id", m.ID))
		return
	}

	// **中断中は送りません。** シャットダウンの猶予は運営への通知に使います。
	// 送らなかった控えは次回に持ち越されません (上のとおりリトライしない)。
	if isDone(ctx) {
		return
	}

	if err := d.sender.SendAutoReply(ctx, repository.AutoReply{
		ContactID: m.ID,
		To:        *m.ReplyTo,
		Body:      composeAutoReplyBody(m),
	}); err != nil {
		// **WARN です** (ADR 0010 の 4-3)。届かなくても問い合わせは
		// 運営に届いており、人がすぐ対応するものではありません。
		// 頻発するなら送信経路の問題なので、数えられるようにします
		// (infra/athena/queries/contact-auto-reply.sql)。
		//
		// **このイベントの件数だけでは足りません。** ワーカーが止まれば
		// 失敗すら出なくなるので、監視側は
		// **contact_auto_reply_sent が一定時間出ていないこと**も見ます
		// (ADR 0008 の「実装して分かったこと 9」と同じ構造)。
		//
		// **宛先は載せません。** ただしサーバの応答文が error に入るため、
		// 宛先が混じることはあります (smtp.go の RCPT TO の注記)。
		slog.WarnContext(ctx, "contact_auto_reply_failed",
			slog.Int64("contact_id", m.ID),
			slog.String("error", truncate(err.Error(), maxLastErrorLength)),
		)
		return
	}

	slog.InfoContext(ctx, "contact_auto_reply_sent", slog.Int64("contact_id", m.ID))
}

// leaseBudget は 1 周回で使ってよい時間を返します。
//
// **リースより短くします。** ちょうどリースぶん使うと、最後の 1 件を
// 送っている最中にリースが切れます。残す余白は「1 件ぶんの最悪」より
// 大きい必要があり、1 件は最悪 2 接続 (通知 + 控え) ぶんかかります。
//
// 4 分の 3 にすると、既定のリース 5 分に対して余白が 75 秒 ——
// MAIL_TIMEOUT_SECONDS = 10 での 1 件ぶん (最悪 40 秒) を上回ります。
// タイムアウトを 20 秒より長くすると足りなくなりますが、そのときは
// **超えるのが最後の 1 件だけ**になります (超える前に必ず break する)。
func (d *Dispatcher) leaseBudget() time.Duration {
	return d.lease / 4 * 3
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
	// **中断されていたら測りません** (レビュー指摘)。
	//
	// 上のループは isDone で抜けるので、そのまま測ると
	// `context canceled` で必ず失敗し、**デプロイのたびに WARN が出ます。**
	// スケジューラが「シャットダウン由来の失敗を ERROR にしない」
	// (scheduler.go の run) と決めているのと同じ理由で、ここでも黙ります。
	if isDone(ctx) {
		return
	}

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
	// **このアドレスへは何も送っていません** (決定 2)。利用者が入力した
	// 文字列であって、書いた人の持ち物である保証がありません。
	fmt.Fprintf(&b, "メールアドレス (入力値・未検証): %s\n", m.Email)
	// **返信してよいのはこちらです。** IdP が検証し、ログインのたびに
	// 更新しているアドレス (ADR 0005 決定 3)。
	//
	// 2 つ並べて出すのは、**食い違っていること自体が情報**だからです ——
	// 他人のアドレスを入力した問い合わせが、対応する人の目に見えます。
	//
	// **「控えを送った」とは書きません** (レビュー指摘)。この本文は
	// 控えを試みる**前**に組み立てて送っており、成否を知りようがありません。
	// 控えは best-effort (リトライ無し・行に痕跡を残さない) なので、
	// **「届いているはず」と読ませると、届いていない前提の対応ができません。**
	// 実際に送れたかは contact_auto_reply_sent / _failed にしかありません。
	if m.ReplyTo != nil {
		fmt.Fprintf(&b, "メールアドレス (検証済み・返信先): %s\n", m.ReplyTo)
		b.WriteString("  控え: このアドレス宛に送付を試みます" +
			" (best-effort。届いたかは contact_id でログを参照)\n")
	} else {
		b.WriteString("メールアドレス (検証済み): なし (匿名または退会済み)\n")
		b.WriteString("  控え: 送っていません\n")
	}
	fmt.Fprintf(&b, "件名: %s\n", m.Subject)
	b.WriteString("\n---\n")
	b.WriteString(m.Body)
	b.WriteString("\n")

	return b.String()
}

// composeAutoReplyBody は本人へ返す控えの本文を組み立てます。
//
// **入力されたアドレスを載せません。** このメールは登録アドレス宛で、
// 入力欄には他人のアドレスが書かれていることがあります ——
// それをそのまま書き写すと、**「他人のアドレス」を本人以外に
// 見せる経路**を作ることになります。何が食い違っているかは
// 運営宛のほう (composeBody) にだけ出します。
//
// **問い合わせ番号を載せます。** 本人が後から照会するときの手掛かりで、
// これはこちら側が採番した整数です (外部識別子の方針 ADR 0003 未決 #11 は
// URL に出す ID の話で、本人宛のメールはその対象になりません)。
func composeAutoReplyBody(m model.Message) string {
	var b strings.Builder

	b.WriteString("お問い合わせを受け付けました。担当者が内容を確認します。\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "問い合わせ番号: %d\n", m.ID)
	fmt.Fprintf(&b, "受付日時: %s\n", m.CreatedAt.UTC().Format(time.RFC3339))
	b.WriteString("\n")
	// **なぜこのアドレスに届いたのかを書きます。** 書かないと、
	// フォームに別のアドレスを入力した人が「なぜここに来たのか」
	// 分からないままになります。
	b.WriteString("このメールは自動送信で、ご利用のアカウントに登録されている\n")
	b.WriteString("アドレス宛にお送りしています。フォームに入力されたアドレスへは\n")
	b.WriteString("送っていません (入力された値は、こちらで確認できないためです)。\n")
	b.WriteString("\n")
	b.WriteString("心当たりが無い場合、このメールは破棄してください。\n")
	b.WriteString("\n--- お預かりした内容 ---\n")

	fmt.Fprintf(&b, "氏名: %s\n", m.Name)
	fmt.Fprintf(&b, "件名: %s\n", m.Subject)
	b.WriteString("\n")
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
