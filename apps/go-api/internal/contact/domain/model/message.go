// Package model は問い合わせのドメインモデルです。
//
// 【このモジュールが持つ責務】
// [ADR 0008](../../../../../../docs/adr/0008-contact-and-mail.md) は
// 「問い合わせは DB に保存し、メール送信は非同期にする」と決めています。
// そのため問い合わせは**受付と送信の 2 つの状態を持つ 1 つの実体**になり、
// 他のモジュール (thread / comment / image) のように
// 「投稿されたもの」だけを表す型にはなりません。
//
// **メールの組み立てはここにありません。** SMTP のヘッダやエンコーディングは
// 送信手段の都合であり、問い合わせという概念には属しません
// (image モジュールが S3 SDK の型を知らないのと同じ理由)。
// 実装は internal/infrastructure/mail にあります。
package model

import (
	"fmt"
	"net/mail"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"develop-experiments/apps/go-api/internal/apperr"
)

// 入力の長さの上限です。
//
// **3 か所で同じ値を持ちます。**
//
//	この定数群
//	DB の CHECK 制約 (000012_add_contact_messages.up.sql)
//	仕様書の maxLength (api/openapi.yaml の CreateContactRequest)
//
// 揃っていないと「アプリが通した入力を DB が拒否して 500」または
// 「仕様書より長い入力が通る」形で表に出ます。
// 揃っていることは message_test.go が実際にファイルを読んで検査します。
//
// **文字数 (ルーン数) で数えます。** PostgreSQL の char_length も
// バイト数ではなく文字数なので、バイト数で数えると日本語の入力が
// アプリを通って DB で落ちます。
const (
	// MaxNameLength は氏名の上限です。
	MaxNameLength = 100
	// MinEmailLength はアドレスの下限です。`a@b` が最短になります。
	MinEmailLength = 3
	// MaxEmailLength はアドレスの上限です。RFC 5321 のアドレス長の上限。
	MaxEmailLength = 254
	// MaxSubjectLength は件名の上限です。
	MaxSubjectLength = 200
	// MaxBodyLength は本文の上限です。
	MaxBodyLength = 5000
)

// Status は送信の状態です。
//
// **問い合わせそのものの状態ではありません。** 受け付けた事実は
// 行が存在することで表され、ここが表すのは「運営に届いたか」だけです。
type Status string

const (
	// StatusPending は未送信です。**受け付けた直後は必ずこれ。**
	StatusPending Status = "pending"
	// StatusSent は送信済みです。
	StatusSent Status = "sent"
	// StatusFailed は試行回数の上限を超えて打ち切ったものです
	// (ADR 0008 決定 1)。**行は消しません** ——
	// 運用が手で拾い直せなくなるためです。
	StatusFailed Status = "failed"
)

// AllStatuses は取りうる状態です。DB の CHECK 制約と揃えます。
var AllStatuses = []Status{StatusPending, StatusSent, StatusFailed}

// ParseStatus は文字列を状態に解釈します。
//
// **知らない値を既定値へ丸めません** (他のモジュールの Parse と同じ約束)。
// 丸めると、DB に入った未知の値が「未送信」として扱われ、
// 送信済みの問い合わせがもう一度送られます。
func ParseStatus(raw string) (Status, error) {
	for _, s := range AllStatuses {
		if string(s) == raw {
			return s, nil
		}
	}
	return "", fmt.Errorf("不正な状態です (%q): %w", raw, apperr.ErrInvalidArgument)
}

// Submission は受け付ける前の入力です。
//
// **Message と分けています。** こちらは利用者が書いたものだけを持ち、
// ID も状態も持ちません —— 保存されるまでそれらは存在しないためです。
type Submission struct {
	// UserID はログイン済みの場合の内部 ID です。**匿名では nil**
	// (ADR 0008 決定 4)。「ログインできない」という問い合わせが
	// 来る以上、ログインを必須にすると詰みます。
	UserID *int64

	Name    string
	Email   string
	Subject string
	Body    string

	// ClientIP はレート制限の集計に使う送信元です (ADR 0008 決定 4)。
	//
	// **ゼロ値 (未指定) を許します。** 取得できない経路が将来増えたときに
	// 問い合わせ自体を弾くのは行き過ぎで、そのときレート制限が
	// 効かなくなることは Submit 側が判断します。
	ClientIP netip.Addr
}

// Normalize は入力を整え、保存できる形かどうかを検査します。
//
// **正規化と検証を 1 つにしています。** 分けると「検証したが正規化前の値を
// 保存する」経路が作れてしまい、前後の空白ぶんだけ上限を超えた入力が
// DB の CHECK 制約に当たって 500 になります。
func (s Submission) Normalize() (Submission, error) {
	out := s

	var err error
	if out.Name, err = normalizeLine(s.Name, "氏名", 1, MaxNameLength); err != nil {
		return Submission{}, err
	}
	if out.Email, err = normalizeEmail(s.Email); err != nil {
		return Submission{}, err
	}
	if out.Subject, err = normalizeLine(s.Subject, "件名", 1, MaxSubjectLength); err != nil {
		return Submission{}, err
	}
	if out.Body, err = normalizeBody(s.Body); err != nil {
		return Submission{}, err
	}
	return out, nil
}

// normalizeLine は 1 行の入力を整えます。
//
// **改行と制御文字を拒否します。** これらの値はメールヘッダには
// 入りませんが (ADR 0008 決定 3 で、ヘッダはすべてこちら側が組み立てると
// 決めている)、**「ヘッダに入れない」という約束が破られた日に
// ヘッダインジェクションになる**のがこの 3 つです。
// 約束と入力検査の二重にしておきます。
//
// 拒否ではなく除去にしないのは、氏名や件名に改行を書く人が
// 「消された」ことに気づけないためです。
func normalizeLine(raw, field string, minLen, maxLen int) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("%sを入力してください: %w", field, apperr.ErrInvalidArgument)
	}
	if strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("%sに改行は使えません: %w", field, apperr.ErrInvalidArgument)
	}
	if i := strings.IndexFunc(v, isControl); i >= 0 {
		return "", fmt.Errorf("%sに使えない文字が含まれています: %w", field, apperr.ErrInvalidArgument)
	}
	if n := utf8.RuneCountInString(v); n < minLen || n > maxLen {
		return "", fmt.Errorf("%sは %d〜%d 文字にしてください (現在 %d 文字): %w",
			field, minLen, maxLen, n, apperr.ErrInvalidArgument)
	}
	return v, nil
}

// normalizeEmail はアドレスを整えます。
//
// **表示名つきの形式 (`ホシノ <a@example.com>`) を拒否します。**
// net/mail はこれを正しく解釈してしまうため、素通しすると
// 「アドレス欄に見える文字列」と「実際のアドレス」がずれます。
// この値は本文にそのまま載るので、読む側が別のアドレスへ返信しかねません。
//
// **ここでの検証は「形式」だけです。** 送達できることも、
// **書いた人がそのアドレスの持ち主であることも**保証しません。
// 存在しないドメインも通ります。
//
// 所有権の確認には確認メールを送ることになりますが、
// **その確認メール自体が未検証のアドレス宛の送信**になるため、
// 決定 2 が禁じたものの中に入ります (循環している)。
// このシステムが検証済みとして扱えるのは、IdP が確認したアドレス
// (users.email / ADR 0005 決定 3) だけです —— VerifiedEmail を参照。
func normalizeEmail(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("メールアドレスを入力してください: %w", apperr.ErrInvalidArgument)
	}
	if n := utf8.RuneCountInString(v); n < MinEmailLength || n > MaxEmailLength {
		return "", fmt.Errorf("メールアドレスは %d〜%d 文字にしてください: %w",
			MinEmailLength, MaxEmailLength, apperr.ErrInvalidArgument)
	}

	addr, err := mail.ParseAddress(v)
	if err != nil || addr.Name != "" || addr.Address != v {
		return "", fmt.Errorf("メールアドレスの形式が正しくありません: %w", apperr.ErrInvalidArgument)
	}
	return v, nil
}

// normalizeBody は本文を整えます。
//
// **改行は残します。** 本文は複数行で書かれるもので、
// 1 行に潰すと読めなくなります。
//
// 改行コードは LF に統一します。CRLF のまま保存すると、
// **同じ本文が入力経路によって別の長さになります** ——
// 上限 5000 文字の判定が「どのブラウザから送ったか」で変わることになります。
// メール送信時に CRLF へ戻すのは送信側の仕事です。
func normalizeBody(raw string) (string, error) {
	v := strings.ReplaceAll(raw, "\r\n", "\n")
	v = strings.ReplaceAll(v, "\r", "\n")
	v = strings.TrimSpace(v)

	if v == "" {
		return "", fmt.Errorf("本文を入力してください: %w", apperr.ErrInvalidArgument)
	}
	// 改行とタブ以外の制御文字は拒否します。
	// **本文は最終的にメールの本体になる**ので、端末やメールクライアントの
	// 解釈を変える文字を通す理由がありません。
	if i := strings.IndexFunc(v, func(r rune) bool { return r != '\n' && r != '\t' && isControl(r) }); i >= 0 {
		return "", fmt.Errorf("本文に使えない文字が含まれています: %w", apperr.ErrInvalidArgument)
	}
	if n := utf8.RuneCountInString(v); n > MaxBodyLength {
		return "", fmt.Errorf("本文は %d 文字までです (現在 %d 文字): %w",
			MaxBodyLength, n, apperr.ErrInvalidArgument)
	}
	return v, nil
}

// isControl は制御文字かどうかを返します。
//
// **unicode.IsControl を使いません。** あちらは C1 制御文字 (U+0080〜U+009F) を
// 含みますが、DEL (U+007F) も含む一方で、ここで拾いたいのは
// 「表示されないのに解釈を変える文字」です。範囲で書くほうが
// 何を拒否しているかが読んで分かります。
func isControl(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

// VerifiedEmail は**所有権が確認済みの**アドレスです。
//
// 【なぜ string ではなく型なのか】
// 決定 2 が禁じているのは「検証されていないアドレスへ送ること」で、
// これは本来「送り先に利用者の入力を渡さない」という**約束**でしか
// 守られません。約束は破られます —— Message.Email も
// VerifiedEmail も同じ `string` なら、取り違えはコンパイルを通ります。
//
// 型にしておくと、`Email` を送り先へ渡す経路が**書けなくなります。**
// normalizeLine が「ヘッダに入れない」約束と入力検査を二重にしているのと
// 同じ考え方を、値の出どころに適用した形になります。
//
// 【このシステムで「検証済み」と言えるもの】
// **1 つだけです。** IdP が `email_verified = true` として渡し、
// ログインのたびに `users.email` へ書き直しているアドレス
// (ADR 0005 決定 3 / db/query/users.sql の UpsertUser)。
//
// 利用者がフォームに入力した値は、**たまたま同じ文字列でも**
// 検証済みにはなりません。入力欄に自分のアドレスを書いたのか
// 他人のアドレスを書いたのかを、こちらは区別できないためです。
type VerifiedEmail struct {
	addr string
}

// NewVerifiedEmail は検証済みアドレスを組み立てます。
//
// **呼んでよいのは「所有権が確認済みだと言い切れる場所」だけです。**
// 現在は users.email を読むアダプタ (infrastructure/postgres) の 1 か所で、
// それ以外から呼ぶときは**何が所有権を保証しているのか**を先に書いてください。
//
// 形式をここでも検査するのは、**DB の値が常に正しいと仮定しないため**です。
// users.email に入っているのは IdP が返した文字列そのままで、
// contact_messages と違って CHECK 制約もありません。
func NewVerifiedEmail(raw string) (VerifiedEmail, error) {
	v, err := normalizeEmail(raw)
	if err != nil {
		return VerifiedEmail{}, err
	}
	return VerifiedEmail{addr: v}, nil
}

// String はアドレスを返します。
//
// **メールの宛先ヘッダに入れてよい唯一のアドレス**になります
// (運営の固定アドレスを除く)。
func (v VerifiedEmail) String() string { return v.addr }

// Message は保存された問い合わせです。
//
// **client_ip を持ちません。** 保存したあとにこの値を読む必要があるのは
// レート制限の集計 (SQL の中で完結する) と消し込み (同上) だけで、
// ドメインの型に載せると、メールの本文や API の応答へ回す経路が
// 作れてしまいます。個人データは運べる場所を狭くしておきます。
type Message struct {
	ID     int64
	UserID *int64

	Name    string
	Email   string
	Subject string
	Body    string

	// AttemptCount は確保された回数です。**送信を試みた回数ではありません** ——
	// 確保の直後にプロセスが落ちた場合も 1 増えます。
	// リトライの上限判定はこちらで行います (無限に居座る行を作らないため)。
	AttemptCount int32
	// CreatedAt は受け付けた時刻です。
	CreatedAt time.Time

	// ReplyTo は自動返信 (控え) を送ってよいアドレスです。
	// **nil なら送りません。**
	//
	// 入るのは `users.email` —— IdP が検証し、ログインのたびに
	// 更新しているアドレスだけです。**上の Email (利用者が入力した値) が
	// ここに入ることはありません** (決定 2)。
	//
	// nil になるのは 3 つの場合です。
	//
	//	匿名の問い合わせ           user_id が NULL
	//	退会済みの利用者           users.deleted_at が非 NULL
	//	users.email が形式不正     NewVerifiedEmail が通らなかった
	//
	// **保存 (Save) の戻り値では常に nil です。** 受付は自動返信を
	// 送らないので引く必要がなく、引かないぶん受付のクエリが軽く保たれます。
	// 値が入るのは送信ワーカーが確保した行 (ClaimPending) だけになります。
	ReplyTo *VerifiedEmail
}
