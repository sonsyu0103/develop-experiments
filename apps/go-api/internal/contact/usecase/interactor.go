// Package usecase は問い合わせの受付と、運営への送信を担当します。
//
// **2 つに割れています** ([ADR 0008](../../../../../docs/adr/0008-contact-and-mail.md) 決定 1)。
//
//	Interactor  受付。HTTP のリクエストの中で走り、外部サービスに触らない
//	Dispatcher  送信。定期処理の中で走り、失敗したらやり直す
//
// この分割がそのまま「メールプロバイダの障害が問い合わせの喪失にならない」
// という性質になります。受付は DB にしか触らないので、
// メールが 1 通も送れない状況でも問い合わせは受け取れます。
package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/contact/domain/model"
	"develop-experiments/apps/go-api/internal/contact/domain/repository"
)

// レート制限の既定値です (ADR 0008 決定 4 のスパム対策)。
//
// **数値の根拠は「1 日数十件を想定」から来ています。** 正規の利用者が
// 1 時間に 5 件も問い合わせることは、まずありません。連投を止めるのが
// 目的であって、送信の総量を絞るのが目的ではないため、
// 窓は短く・件数は少なくの側に振っています。
//
// 効かなくなったら (= 突破されたら) CAPTCHA を検討します。
// 先に入れないのは、外部サービス依存とプライバシーの話が別途発生し、
// この規模では honeypot + レート制限で足りるためです。
const (
	// DefaultRateLimitWindow はレート制限の窓です。
	DefaultRateLimitWindow = 1 * time.Hour
	// DefaultRateLimitMax は窓の中で許す件数です。
	DefaultRateLimitMax = 5
)

// SubmitCommand は問い合わせ 1 件ぶんの入力です。
type SubmitCommand struct {
	// UserID はログイン済みの場合の内部 ID です。**匿名では nil。**
	UserID *int64

	Name    string
	Email   string
	Subject string
	Body    string

	// Honeypot は画面上非表示のフィールドの値です (ADR 0008 決定 4)。
	//
	// **人間は埋めません。** 埋まっていたら破棄します。
	Honeypot string

	// ClientIP はレート制限の集計に使う送信元です。
	// 取得できなかった場合はゼロ値で構いません (下記 Submit の説明を参照)。
	ClientIP netip.Addr
}

// Interactor は問い合わせの受付です。
//
// **メールを送りません。** 送信は Dispatcher が定期処理の中で行います。
type Interactor struct {
	repo repository.ContactRepository

	rateWindow time.Duration
	rateMax    int64
}

// Option は Interactor の調整です。
type Option func(*Interactor)

// WithRateLimit はレート制限の窓と上限を差し替えます。
//
// **0 以下は既定値へ丸めます。** 設定値由来 (未設定 = 0) の値が
// そのまま来る余地があり、丸めないと
// 「窓 0 秒 = 実質無制限」または「上限 0 件 = 誰も送れない」になります。
// どちらも静かに壊れる形なので、起動を止めるより既定へ寄せます
// (scheduler.New が Interval を丸めるのと同じ判断)。
func WithRateLimit(window time.Duration, maxPerWindow int64) Option {
	return func(i *Interactor) {
		if window > 0 {
			i.rateWindow = window
		}
		if maxPerWindow > 0 {
			i.rateMax = maxPerWindow
		}
	}
}

// NewInteractor は受付のインタラクタを生成します。
func NewInteractor(repo repository.ContactRepository, opts ...Option) *Interactor {
	i := &Interactor{
		repo:       repo,
		rateWindow: DefaultRateLimitWindow,
		rateMax:    DefaultRateLimitMax,
	}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// Submit は問い合わせを受け付けます。
//
// **戻り値は受理の時刻だけです。** 問い合わせの ID は返しません ——
// 内部 ID を外に出さない方針 (ADR 0003 未決 #11) に加えて、
// **honeypot に引っかかった要求と応答が区別できなくなる**ためです。
//
// 検査の順序には理由があります。
//
//  1. honeypot   ボットは以降の処理を 1 つも起こさない
//  2. 入力の検証 DB に触る前に弾く
//  3. レート制限 集計クエリを 1 本打つので、正しい入力にだけ払う
//  4. 保存
//
// 3 を 2 より先にすると、**でたらめな入力を投げるだけで
// 集計クエリを叩かせられます** —— レート制限そのものが負荷になります。
func (i *Interactor) Submit(ctx context.Context, cmd SubmitCommand) (time.Time, error) {
	// **honeypot は最初に見ます。**
	//
	// 破棄しますが、エラーは返しません (ADR 0008 決定 4)。
	// 400 にすると「この項目が引き金だ」とボット側に教えることになり、
	// 次の版では空で送られてきます。
	//
	// **代償: 自動入力に埋められた人間の問い合わせも静かに消えます。**
	// フォーム側で autocomplete を切って確率を下げますが、
	// 0 にはなりません。honeypot を選ぶ以上ここは引き受けます。
	if cmd.Honeypot != "" {
		// **中身は出しません** (ADR 0010 の 4-5)。
		// 何が入っていたかは対策の役に立たず、個人データが混ざりえます。
		slog.InfoContext(ctx, "contact_honeypot_dropped")
		return time.Now().UTC(), nil
	}

	submission, err := model.Submission{
		UserID:   cmd.UserID,
		Name:     cmd.Name,
		Email:    cmd.Email,
		Subject:  cmd.Subject,
		Body:     cmd.Body,
		ClientIP: cmd.ClientIP,
	}.Normalize()
	if err != nil {
		return time.Time{}, err
	}

	if limitErr := i.ensureUnderRateLimit(ctx, submission.ClientIP); limitErr != nil {
		return time.Time{}, limitErr
	}

	saved, err := i.repo.Save(ctx, submission)
	if err != nil {
		return time.Time{}, err
	}

	// **氏名・メールアドレス・本文は出しません** (ADR 0010 の 4-5)。
	// DB にあるものをログに複製すると、保持期間の違う場所に
	// 個人データの写しが増えます。
	//
	// user_id は出します。内部 ID は個人データではなく、
	// 「ログイン済みの問い合わせがどれくらいあるか」は運用の判断に要ります。
	attrs := []any{slog.Int64("contact_id", saved.ID)}
	if saved.UserID != nil {
		attrs = append(attrs, slog.Int64("user_id", *saved.UserID))
	}
	slog.InfoContext(ctx, "contact_received", attrs...)

	return saved.CreatedAt, nil
}

// ensureUnderRateLimit は同じ送信元からの連投を弾きます。
//
// **送信元が分からない場合は素通しします。** ClientIP のゼロ値は
// 「取得できなかった」を表し、そこで問い合わせ自体を拒否すると、
// 経路の都合で正規の利用者が詰みます。**警告だけ残します** ——
// 全件が素通しになっている状態に気づけるようにするためです。
func (i *Interactor) ensureUnderRateLimit(ctx context.Context, ip netip.Addr) error {
	if !ip.IsValid() {
		slog.WarnContext(ctx, "contact_rate_limit_skipped")
		return nil
	}

	n, err := i.repo.CountRecent(ctx, ip, i.rateWindow)
	if err != nil {
		return err
	}
	if n < i.rateMax {
		return nil
	}

	// **IP はログに出します** (ADR 0010 の 4-5: レート制限の調査に要るため
	// 出すが、保持期間で管理する)。弾いた事実だけ残しても、
	// 誰を弾いたのかが分からなければ攻撃の観測になりません。
	slog.WarnContext(ctx, "contact_rate_limited",
		slog.String("client_ip", ip.String()),
		slog.Int64("count", n),
		slog.Int64("limit", i.rateMax),
	)
	return fmt.Errorf(
		"問い合わせの送信が多すぎます。時間をおいて送り直してください: %w",
		apperr.ErrResourceExhausted)
}
