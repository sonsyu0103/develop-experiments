// Package repository は問い合わせの永続化とメール送信の契約を定義します。
//
// **SMTP も SES も現れません**
// ([ADR 0008](../../../../../../docs/adr/0008-contact-and-mail.md) の
// 「送信は MailSender インターフェースの背後に置き、SMTP 実装と SES 実装を
// 差し替えられるようにする」)。image モジュールが S3 SDK の型を知らないのと
// 同じ形で、実装は internal/infrastructure/mail に置きます。
//
// テストではフェイクを使い、**実際にメールを送るテストは書きません。**
package repository

import (
	"context"
	"net/netip"
	"time"

	"develop-experiments/apps/go-api/internal/contact/domain/model"
)

// ContactRepository は contact_messages への操作です。
//
// **受付側と送信側が 1 つのインターフェースに同居しています。**
// 分けることも考えましたが、両者が同じ 1 テーブルの別の列を触るだけで、
// 実装も 1 つ (PostgreSQL) しかありません。moderation の
// ReportRepository / TargetExistenceChecker のように分けたのは
// **実装の実体が別々になりうる**場合で、ここは当てはまりません。
type ContactRepository interface {
	// Save は問い合わせを 1 件保存します。**この時点では送りません。**
	//
	// 保存が成功した時点で内容は失われません (ADR 0008 決定 1)。
	// これが「同期でメールを送らない」ことの見返りになります。
	Save(ctx context.Context, s model.Submission) (*model.Message, error)

	// CountRecent は同じ送信元からの直近 window の件数を返します。
	//
	// **アプリのメモリで数えません** (ADR 0008 決定 4)。
	// レプリカが N 個あれば実質 N 倍まで通ってしまい、
	// レート制限がずれると意味を失います。
	CountRecent(ctx context.Context, ip netip.Addr, window time.Duration) (int64, error)

	// ClaimPending は送信する行を確保します。
	//
	// **複数のインスタンスが同時に呼んでも、同じ行は返りません**
	// (`FOR UPDATE SKIP LOCKED`)。確保した行は lease のあいだ
	// 他のインスタンスから見えなくなり、その間に送信します。
	//
	// **確保した時点で attempt_count が増えます。** 送信を試みる前に
	// 増やすので、クラッシュを繰り返す行も上限に達して打ち切られます。
	ClaimPending(ctx context.Context, lease time.Duration, batchSize int32) ([]model.Message, error)

	// MarkSent は送信できた行を確定します。
	//
	// **既に pending でない行には効きません** (0 行更新)。
	// リース切れで 2 つのワーカーが同じ行を送った場合に、
	// 後から来たほうが送信時刻を上書きしないためです。
	MarkSent(ctx context.Context, id int64) error

	// Reschedule は失敗した行を次の試行へ回します。
	//
	// backoff は「今から」の待ち時間です。**時刻ではありません** ——
	// アプリの時計と DB の時計のずれを持ち込まないため、
	// 起点は DB 側の now() になります。
	Reschedule(ctx context.Context, id int64, backoff time.Duration, lastErr string) error

	// Fail は試行回数の上限を超えた行を打ち切ります (ADR 0008 決定 1)。
	//
	// **行は消しません。** 運用が手で拾い直せなくなります。
	Fail(ctx context.Context, id int64, lastErr string) error

	// OldestPending は最も古い未送信の受付時刻を返します。
	//
	// **ワーカーが止まったことに気づくために要ります**
	// (ADR 0008 の「引き受けるコスト」)。止まっても API は正常に
	// 202 を返し続けるので、溜まっていることは外から見えません。
	//
	// 未送信が無ければ ok = false です。
	OldestPending(ctx context.Context) (oldest time.Time, ok bool, err error)

	// ScrubClientIPs は保持期間を過ぎた行から送信元を消します。
	//
	// **個人データを無期限に持たないため**です (ADR 0008)。
	// 消すのは列の値だけで、行は残します。
	//
	// 呼び出し側は「0 件になるまで繰り返す」形で使ってください。
	ScrubClientIPs(ctx context.Context, retention time.Duration, maxRows int32) (int64, error)
}

// Notification は運営へ届ける 1 通ぶんの内容です。
//
// **件名も宛先も差出人も含みません** (ADR 0008 決定 3)。
// ヘッダはすべて実装側が組み立てます —— ヘッダ値に改行 (`\r\n`) を
// 注入されると任意のヘッダを追加でき (**メールヘッダインジェクション**)、
// `Bcc` を足せばこのシステムが迷惑メールの踏み台になります。
//
// **利用者の入力が入るのは Body だけ**、というのがこの型の形そのものです。
type Notification struct {
	// ContactID は件名に載せる問い合わせの内部 ID です。
	// **こちら側が採番した整数**なので、ヘッダに入れて安全です。
	ContactID int64
	// Body は本文です。**ここだけが利用者の入力を含みます。**
	Body string
}

// AutoReply は問い合わせをくれた本人へ返す控えです。
//
// **Notification と別の型にしています。** 宛先を持つかどうかが
// 決定的に違うためで、1 つの型に `To *string` を足すと
// 「nil なら運営、非 nil ならその宛先」という形になり、
// **入力値を詰めた経路と区別がつかなくなります。**
//
// **利用者の入力が入るのは Body だけ**なのは Notification と同じです。
type AutoReply struct {
	// ContactID は件名に載せる問い合わせの内部 ID です。
	ContactID int64

	// To は送り先です。
	//
	// **model.VerifiedEmail しか入りません** (決定 2)。生成できるのは
	// users.email を読むアダプタだけなので、**利用者が入力したアドレスを
	// ここへ渡す経路はコンパイルを通りません。**
	// 「入力されたアドレスへ送らない」を約束ではなく型で持たせた形になります。
	To model.VerifiedEmail

	// Body は本文です。**ここだけが利用者の入力を含みます。**
	Body string
}

// MailSender はメールを送ります。SMTP と SES の差はこの背後に閉じます。
//
// **宛先を自由に指定させません。** 運営への通知 (Send) は実装が持つ
// 固定アドレスへ、控え (SendAutoReply) は検証済みのアドレスへしか
// 送れません —— 任意の文字列を宛先に取るメソッドを 1 つでも置くと、
// **入力されたアドレスへ送る実装を書けてしまいます** (決定 2 が禁じたこと)。
type MailSender interface {
	// Send は運営へ 1 通送ります。
	//
	// **失敗は一時的なものとして扱われます。** 呼び出し側は
	// バックオフして試行回数の上限まで再送します。
	Send(ctx context.Context, n Notification) error

	// SendAutoReply は問い合わせをくれた本人へ控えを 1 通送ります。
	//
	// **失敗しても再送されません** (ADR 0008 決定 2 / best-effort)。
	// 呼び出し側はこの時点で運営への通知を確定させており、
	// 控えのために問い合わせをもう一度送ることはしません。
	SendAutoReply(ctx context.Context, r AutoReply) error
}
