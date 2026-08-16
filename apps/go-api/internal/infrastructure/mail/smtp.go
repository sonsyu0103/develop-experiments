// Package mail は問い合わせ通知の SMTP 実装です。
//
// contact モジュールが定義した MailSender
// ([ADR 0008](../../../../../docs/adr/0008-contact-and-mail.md)) を満たします。
// **SMTP を知ってよいのはここだけ**で、内側 (usecase / domain) からは
// 「1 通送る」以上のことが見えません —— objectstorage が S3 SDK を
// 閉じ込めているのと同じ形になります。
//
// 【この実装が守っているもの】
// 決定 3「メールヘッダにユーザー入力を入れない」。差出人・宛先・件名は
// すべてこの中で組み立て、渡されるのは本文だけです。
// ヘッダ値に改行 (`\r\n`) を注入されると任意のヘッダを追加でき
// (**メールヘッダインジェクション**)、`Bcc` を足せばこのシステムが
// 迷惑メールの踏み台になります。
//
// 【テストは実際に送りません】
// 送信の検査はメッセージの組み立て (buildMessage) に対して行います。
// SMTP の往復まで検査しようとすると、テストにサーバが要るうえ、
// 守りたい性質 (ヘッダに入力が入らない) はバイト列を見れば分かります。
package mail

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"mime"
	"net"
	netmail "net/mail"
	"net/smtp"
	"strings"
	"time"

	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/contact/domain/repository"
)

// SMTPSender は SMTP でメールを送ります。
//
// **接続を保持しません。** 1 通ごとに接続して切ります。送信は
// 数分に 1 回・数件という頻度なので、接続を維持する利点より
// 「切れた接続を持ち続ける」不具合のほうが確実に高くつきます。
type SMTPSender struct {
	cfg  config.MailConfig
	from netmail.Address
	to   netmail.Address
}

var _ repository.MailSender = (*SMTPSender)(nil)

// NewSMTP は設定から送信器を作ります。
//
// **アドレスは起動時に検査します。** 送信のたびに検査すると、
// 設定の誤りに気づくのが「最初の問い合わせが来たとき」になります ——
// そのとき失敗するのは利用者の問い合わせのほうです。
func NewSMTP(cfg config.MailConfig) (*SMTPSender, error) {
	from, err := parseFixedAddress(cfg.From, "MAIL_FROM")
	if err != nil {
		return nil, err
	}
	to, err := parseFixedAddress(cfg.To, "MAIL_TO")
	if err != nil {
		return nil, err
	}
	return &SMTPSender{cfg: cfg, from: *from, to: *to}, nil
}

// parseFixedAddress は運営側の固定アドレスを解釈します。
//
// **表示名つきの形式を許しません。** `運営 <ops@example.com>` を許すと、
// 表示名の中身がヘッダにそのまま載ります。設定は利用者の入力ではありませんが、
// **ヘッダに入る値の作り方を 1 通りに保つ**ほうが、後から
// 「ここだけ例外」を増やさずに済みます。
func parseFixedAddress(raw, key string) (*netmail.Address, error) {
	addr, err := netmail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("mail: %s が不正です (%q): %w", key, raw, err)
	}
	if addr.Name != "" || addr.Address != strings.TrimSpace(raw) {
		return nil, fmt.Errorf("mail: %s は表示名なしのアドレスにしてください (got %q)", key, raw)
	}
	return addr, nil
}

// Send は 1 通送ります。
//
// **ここでの失敗は一時的なものとして扱われます。** 呼び出し側
// (contact の Dispatcher) がバックオフして再送し、上限を超えたら
// 打ち切ります。恒久的な失敗 (宛先が存在しないなど) と区別していないのは、
// SMTP の応答からそれを見分けるのが実質不可能なためです ——
// 5xx でも中継の一時的な設定ミスということがあります。
func (s *SMTPSender) Send(ctx context.Context, n repository.Notification) error {
	msg := buildMessage(s.from, s.to, subjectFor(n.ContactID), n.Body, time.Now())

	client, err := s.connect(ctx)
	if err != nil {
		return err
	}
	// **Close は Quit の失敗経路のためにあります。** 正常系では Quit が
	// 接続を閉じますが、途中で失敗して return したときに閉じ忘れると、
	// 送信が失敗するたびにコネクションが残ります。
	defer func() { _ = client.Close() }()

	if authErr := s.authenticate(client); authErr != nil {
		return authErr
	}

	if mailErr := client.Mail(s.from.Address); mailErr != nil {
		return fmt.Errorf("mail: MAIL FROM に失敗しました: %w", mailErr)
	}
	if rcptErr := client.Rcpt(s.to.Address); rcptErr != nil {
		return fmt.Errorf("mail: RCPT TO に失敗しました: %w", rcptErr)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA に失敗しました: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		// **Close しないまま返します。** defer の client.Close() が
		// 接続ごと落とします。書き込みに失敗した DATA を閉じると、
		// 途中まで書いたメッセージが送信されかねません。
		return fmt.Errorf("mail: 本文の送信に失敗しました: %w", err)
	}
	if err := w.Close(); err != nil {
		// **ここが実質のコミットです。** DATA の終端を送ってサーバが
		// 受理を返す点なので、ここまで成功して初めて「送った」と言えます。
		return fmt.Errorf("mail: 本文の確定に失敗しました: %w", err)
	}

	// **QUIT の失敗を送信の失敗にしません** (レビュー指摘)。
	//
	// 上の Close が成功した時点でメールは届いています。ここで返る error は
	// 「221 が返ってこなかった」でしかありません —— サーバが 221 を返さずに
	// 接続を切る、経路の NAT が落とす、締め切り (SetDeadline) が
	// QUIT の途中で来る、のどれでも起きます。
	//
	// **返すと二重送信になります。** 呼び出し側は送信失敗として行を
	// 次の試行へ回すので、QUIT で必ずこける相手に対しては
	// **同じ問い合わせが上限 (8 回) まで届き、そのうえ failed として
	// 打ち切られます** —— 8 通届いているのに ERROR が鳴る形になります。
	//
	// 記録は残します。頻発するなら経路の問題なので、気づけるようにします。
	if err := client.Quit(); err != nil {
		slog.InfoContext(ctx, "mail_quit_failed",
			slog.Int64("contact_id", n.ContactID),
			slog.String("error", err.Error()),
		)
	}
	return nil
}

// connect は SMTP サーバへ接続します。
//
// **smtp.Dial を使いません。** あちらは context を受け取らないので、
// サーバが応答しないときに**シャットダウンの猶予を無視して待ち続けます。**
// 定期処理の中で動く以上、ここが止まるとプロセスの終了も止まります。
func (s *SMTPSender) connect(ctx context.Context) (*smtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)

	dialer := &net.Dialer{Timeout: s.cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mail: %s へ接続できません: %w", addr, err)
	}

	// **接続後の往復にも上限を掛けます。** DialContext が効くのは接続まで。
	// 以降のコマンドが返らない場合に備えて、締め切りを 1 本引いておきます。
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(s.cfg.Timeout))
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mail: SMTP の初期化に失敗しました: %w", err)
	}

	if s.cfg.StartTLS {
		// ServerName を渡すのは証明書の検証に要るためです。
		// **InsecureSkipVerify にしません** —— STARTTLS を有効にしながら
		// 検証を切ると、暗号化しているのに中間者を防げません。
		if err := client.StartTLS(&tls.Config{
			ServerName: s.cfg.Host,
			MinVersion: tls.VersionTLS12,
		}); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("mail: STARTTLS に失敗しました: %w", err)
		}
	}
	return client, nil
}

// authenticate は資格情報があれば認証します。
//
// **無ければ何もしません。** ローカルの Mailpit は認証を要求しません。
// 「認証なしでは送れない」形にすると、手元で経路を確認できなくなります。
func (s *SMTPSender) authenticate(client *smtp.Client) error {
	if s.cfg.Username == "" {
		return nil
	}

	auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	if err := client.Auth(auth); err != nil {
		// **資格情報そのものは載せません。** エラーには
		// サーバの応答文が含まれ、それがログへ流れます。
		return fmt.Errorf("mail: SMTP 認証に失敗しました: %w", err)
	}
	return nil
}

// subjectFor は件名を組み立てます。
//
// **利用者が入力した件名は使いません** (ADR 0008 決定 3)。
// 入力の件名は本文の中に「件名:」として載ります。
//
// 番号を入れるのは、届いたメールと DB の行を突き合わせるためです。
// これはこちら側が採番した整数なので、ヘッダに入れて安全になります。
func subjectFor(contactID int64) string {
	return fmt.Sprintf("[お問い合わせ] #%d", contactID)
}

// buildMessage は RFC 5322 のメッセージを組み立てます。
//
// **この関数がヘッダを組み立てる唯一の場所です。** 引数のうち
// 利用者の入力を含むのは body だけで、それは本文にしか現れません。
//
// 【本文を base64 にする理由】
// 8bit のまま送ると、次の 2 つを自分で守る必要があります。
//
//  1. **1 行 998 オクテットの上限** (RFC 5322)。本文は 5000 文字まで
//     許しており、改行を 1 つも入れずに書けば必ず超えます
//  2. **行頭の `.`** (RFC 5321 のドット詰め)。`.` だけの行が
//     メッセージの終端として解釈されると、そこで本文が切れます
//
// base64 は 76 文字で折り返すので 1 が消え、`.` が行頭に出ないので
// 2 も消えます。日本語が UTF-8 で載ることも保証されます。
//
// 2 については Go の `smtp.Client.Data()` (textproto の DotWriter) が
// ドット詰めを行うため、実は素通しでも壊れません。**それでも base64 に
// するのは、正しさが送信ライブラリの実装依存にならないようにするため**です。
func buildMessage(from, to netmail.Address, subject, body string, now time.Time) []byte {
	var b strings.Builder

	// ヘッダの順序に意味はありませんが、読む人のために
	// 「誰から誰へ・いつ・何を」の順に並べます。
	writeHeader(&b, "From", from.Address)
	writeHeader(&b, "To", to.Address)
	writeHeader(&b, "Date", now.Format(time.RFC1123Z))
	// **RFC 2047 で符号化します。** 件名は日本語を含むので、
	// 生のまま載せると US-ASCII しか許さないヘッダの規定に反します。
	writeHeader(&b, "Subject", mime.QEncoding.Encode("utf-8", subject))
	writeHeader(&b, "Message-ID", messageID(from.Address, now))
	// **Auto-Submitted を付けます** (RFC 3834)。これは人が書いたメールでは
	// ないので、受け取り側の自動応答 (不在通知など) を止められます。
	writeHeader(&b, "Auto-Submitted", "auto-generated")
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", `text/plain; charset="UTF-8"`)
	writeHeader(&b, "Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")

	// **ヘッダと本文の境界より後は、何を書いても新しいヘッダになりません。**
	// base64 の出力に改行を含めても本文のままです。
	b.WriteString(wrapBase64(body))

	return []byte(b.String())
}

// writeHeader は 1 つのヘッダを書きます。
//
// **値から改行を落とします。** ここが最後の砦になります ——
// 呼び出し側は「入力をヘッダに入れない」約束ですが、約束が破られた日に
// ヘッダインジェクションが成立しないよう、値の側でも閉じておきます
// (model の normalizeLine と二重にしてあるのと同じ考え方)。
func writeHeader(b *strings.Builder, name, value string) {
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(stripCRLF(value))
	b.WriteString("\r\n")
}

// stripCRLF は改行 (とその代わりに使える文字) を空白に置き換えます。
func stripCRLF(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, v)
}

// messageID は Message-ID を組み立てます。
//
// **差出人アドレスのドメインを使います。** 受け取り側のスパム判定で、
// 無関係なドメインの Message-ID は減点材料になります。
//
// 一意性は「問い合わせごとに送信時刻が違う」ことに頼っています。
// UUID を使わないのは、この 1 か所のために uuid への依存を
// mail パッケージへ持ち込みたくないためです (ナノ秒まで含めれば十分に一意)。
func messageID(fromAddr string, now time.Time) string {
	domain := "localhost"
	if at := strings.LastIndex(fromAddr, "@"); at >= 0 && at+1 < len(fromAddr) {
		domain = fromAddr[at+1:]
	}
	return fmt.Sprintf("<contact.%d@%s>", now.UnixNano(), domain)
}

// wrapBase64 は本文を base64 にして 76 文字ごとに折り返します。
//
// 76 は RFC 2045 が定める 1 行の上限です。折り返さない実装でも
// 多くのサーバは受け取りますが、**受け取らないサーバがある**以上、
// 規定どおりにしておきます。
func wrapBase64(body string) string {
	const lineLen = 76

	encoded := base64.StdEncoding.EncodeToString([]byte(body))

	var b strings.Builder
	for i := 0; i < len(encoded); i += lineLen {
		end := min(i+lineLen, len(encoded))
		b.WriteString(encoded[i:end])
		b.WriteString("\r\n")
	}
	return b.String()
}
