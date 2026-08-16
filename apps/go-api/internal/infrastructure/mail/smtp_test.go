package mail

import (
	"bufio"
	"encoding/base64"
	"mime"
	"net"
	netmail "net/mail"
	"strconv"
	"strings"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/contact/domain/repository"
)

// **実際にメールを送るテストは書きません** (ADR 0008)。
// ここで見るのは組み立てたバイト列で、守りたい性質
// (ヘッダに利用者の入力が入らない) はそこに全部出ます。

var (
	testFrom = netmail.Address{Address: "no-reply@example.com"}
	testTo   = netmail.Address{Address: "ops@example.com"}
	testTime = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
)

// **ヘッダがこちら側の値だけで組み立てられること** (ADR 0008 決定 3)。
func TestBuildMessage_Headers(t *testing.T) {
	t.Parallel()

	raw := string(buildMessage(testFrom, testTo, subjectFor(7), "本文です", testTime))
	head, _, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatalf("ヘッダと本文の境界が無い:\n%s", raw)
	}

	for _, want := range []string{
		"From: no-reply@example.com",
		"To: ops@example.com",
		"MIME-Version: 1.0",
		`Content-Type: text/plain; charset="UTF-8"`,
		"Content-Transfer-Encoding: base64",
		// **自動応答を止める** (RFC 3834)。人が書いたメールではない。
		"Auto-Submitted: auto-generated",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("ヘッダに %q が無い:\n%s", want, head)
		}
	}

	// **件名は RFC 2047 で符号化されていること。**
	// 生の日本語をヘッダに載せると US-ASCII しか許さない規定に反する。
	subject := headerValue(t, head, "Subject")
	decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
	if err != nil {
		t.Fatalf("Subject を復号できない (%q): %v", subject, err)
	}
	if decoded != "[お問い合わせ] #7" {
		t.Errorf("Subject = %q (復号 %q)", subject, decoded)
	}
}

// **利用者の入力は件名に入らないこと** (ADR 0008 決定 3)。
//
// 入力された件名は本文の中に「件名:」として載ります。
// ここが入力を受け取らない形になっていること自体が防御です。
func TestSubjectFor_HasNoUserInput(t *testing.T) {
	t.Parallel()

	if got := subjectFor(12); got != "[お問い合わせ] #12" {
		t.Errorf("subjectFor(12) = %q", got)
	}
}

// **本文が base64 で 76 文字ごとに折り返されること。**
//
// 8bit のままだと、5000 文字を 1 行で書かれた本文が
// RFC 5322 の「1 行 998 オクテット」を超えます。
func TestBuildMessage_BodyIsWrappedBase64(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("あ", 3000)
	raw := string(buildMessage(testFrom, testTo, "件名", long, testTime))
	_, body, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatal("本文が無い")
	}

	for line := range strings.SplitSeq(strings.TrimRight(body, "\r\n"), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("76 文字を超える行がある (%d 文字)", len(line))
		}
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\r\n", ""))
	if err != nil {
		t.Fatalf("本文を復号できない: %v", err)
	}
	if string(decoded) != long {
		t.Error("復号した本文が元と違う")
	}
}

// **本文に何を書かれてもヘッダにならないこと。**
//
// メールヘッダインジェクションの本丸。base64 にしているので
// 改行も `.` だけの行も、そのまま符号化されて本文に留まります。
func TestBuildMessage_BodyCannotForgeHeaders(t *testing.T) {
	t.Parallel()

	hostile := "本文\r\nBcc: victim@example.com\r\n\r\n乗っ取り\r\n.\r\nQUIT"
	raw := string(buildMessage(testFrom, testTo, "件名", hostile, testTime))
	head, body, _ := strings.Cut(raw, "\r\n\r\n")

	if strings.Contains(head, "Bcc") {
		t.Errorf("Bcc がヘッダに現れた:\n%s", head)
	}
	// 本文側にも生の "Bcc:" は出ない (base64 なので)。
	if strings.Contains(body, "Bcc:") {
		t.Errorf("本文が符号化されていない:\n%s", body)
	}
	// **`.` だけの行が無いこと。** あると SMTP の DATA がそこで終わる。
	for line := range strings.SplitSeq(body, "\r\n") {
		if line == "." {
			t.Error("本文に `.` だけの行がある (DATA が途中で終わる)")
		}
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimRight(body, "\r\n"), "\r\n", ""))
	if err != nil {
		t.Fatalf("本文を復号できない: %v", err)
	}
	if string(decoded) != hostile {
		t.Error("本文が改変された (符号化の往復で壊れている)")
	}
}

// **ヘッダの値に改行が混ざっても、行が増えないこと。**
//
// 呼び出し側は「入力をヘッダに入れない」約束ですが、
// 約束が破られた日に成立しないよう、値の側でも閉じてあります。
func TestWriteHeader_StripsCRLF(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writeHeader(&b, "Subject", "件名\r\nBcc: victim@example.com")

	got := b.String()
	if strings.Count(got, "\r\n") != 1 {
		t.Errorf("行が増えている: %q", got)
	}
	if strings.Contains(got, "\nBcc") {
		t.Errorf("ヘッダを注入できた: %q", got)
	}
}

// **Message-ID が差出人のドメインを名乗ること。**
//
// 無関係なドメインの Message-ID は、受け取り側のスパム判定で減点材料になります。
func TestMessageID(t *testing.T) {
	t.Parallel()

	if got := messageID("no-reply@example.com", testTime); !strings.HasSuffix(got, "@example.com>") {
		t.Errorf("messageID = %q", got)
	}
	// ドメインを読み取れない場合も壊れないこと。
	if got := messageID("no-at-sign", testTime); !strings.HasSuffix(got, "@localhost>") {
		t.Errorf("messageID = %q", got)
	}
}

// **アドレスの検査が起動時に効くこと。**
//
// 送信のたびに検査すると、設定の誤りに気づくのが
// 「最初の問い合わせが来たとき」になります。
func TestNewSMTP_ValidatesAddresses(t *testing.T) {
	t.Parallel()

	base := config.MailConfig{Host: "localhost", Port: 1025, From: "a@example.com", To: "b@example.com"}
	if _, err := NewSMTP(base); err != nil {
		t.Fatalf("正しい設定が弾かれた: %v", err)
	}

	bad := []config.MailConfig{
		{Host: "localhost", Port: 1025, From: "こわれている", To: "b@example.com"},
		{Host: "localhost", Port: 1025, From: "a@example.com", To: ""},
		// **表示名つきは許さない。** 表示名の中身がヘッダにそのまま載る。
		{Host: "localhost", Port: 1025, From: "運営 <a@example.com>", To: "b@example.com"},
	}
	for _, cfg := range bad {
		if _, err := NewSMTP(cfg); err == nil {
			t.Errorf("不正な設定が通ってしまった: from = %q, to = %q", cfg.From, cfg.To)
		}
	}
}

// headerValue はヘッダ部から 1 つの値を取り出します。
func headerValue(t *testing.T, head, name string) string {
	t.Helper()

	for line := range strings.SplitSeq(head, "\r\n") {
		if after, ok := strings.CutPrefix(line, name+": "); ok {
			return after
		}
	}
	t.Fatalf("ヘッダ %q が無い:\n%s", name, head)
	return ""
}

// **QUIT に失敗しても送信は成功として扱うこと** (レビュー指摘)。
//
// DATA の終端をサーバが受理した時点でメールは届いています。そのあと
// 221 が返らなかっただけで失敗を返すと、呼び出し側が行を次の試行へ回し、
// **同じ問い合わせが上限まで届いたうえで failed として打ち切られます。**
//
// ここだけは実際に SMTP を話す相手が要ります (組み立てたバイト列を
// 見ても分からない性質のため)。**外へは出ません** ——
// ループバックに立てた偽サーバが相手です。
func TestSend_QuitFailureIsNotASendFailure(t *testing.T) {
	t.Parallel()

	addr, received := startFakeSMTP(t, fakeSMTPOptions{dropOnQuit: true})
	sender := newTestSender(t, addr)

	if err := sender.Send(t.Context(), repository.Notification{ContactID: 7, Body: "本文"}); err != nil {
		t.Fatalf("QUIT の失敗が送信の失敗になった: %v", err)
	}
	if got := <-received; !strings.Contains(got, "Subject:") {
		t.Errorf("サーバがメッセージを受け取っていない:\n%s", got)
	}
}

// **正常系も一度は通しておくこと。**
//
// 上の検査は「失敗しても成功扱い」を見るので、これが無いと
// Send が常に nil を返す実装でも緑になります。
func TestSend_Succeeds(t *testing.T) {
	t.Parallel()

	addr, received := startFakeSMTP(t, fakeSMTPOptions{})
	sender := newTestSender(t, addr)

	if err := sender.Send(t.Context(), repository.Notification{ContactID: 1, Body: "本文"}); err != nil {
		t.Fatalf("送信に失敗した: %v", err)
	}
	got := <-received
	// **ヘッダはこちら側の値だけ** (ADR 0008 決定 3)。
	if !strings.Contains(got, "From: no-reply@example.com") ||
		!strings.Contains(got, "To: ops@example.com") {
		t.Errorf("ヘッダが届いていない:\n%s", got)
	}
}

// **DATA の受理に失敗したら、送信の失敗として扱うこと。**
//
// こちらは本当に届いていないので、次の試行へ回す必要があります。
func TestSend_DataRejectionIsAFailure(t *testing.T) {
	t.Parallel()

	addr, _ := startFakeSMTP(t, fakeSMTPOptions{rejectData: true})
	sender := newTestSender(t, addr)

	if err := sender.Send(t.Context(), repository.Notification{ContactID: 1, Body: "本文"}); err == nil {
		t.Fatal("届いていないのに成功として扱われた")
	}
}

func newTestSender(t *testing.T, addr string) *SMTPSender {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("アドレスを分解できない: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("ポートを読めない: %v", err)
	}

	sender, err := NewSMTP(config.MailConfig{
		Host: host, Port: p,
		From: "no-reply@example.com", To: "ops@example.com",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("送信器を作れない: %v", err)
	}
	return sender
}

type fakeSMTPOptions struct {
	// dropOnQuit は QUIT に 221 を返さず接続を切ります。
	dropOnQuit bool
	// rejectData は DATA の終端に 5xx を返します。
	rejectData bool
}

// startFakeSMTP はループバックに最小限の SMTP サーバを立てます。
//
// **1 接続だけ受けて終わります。** 受け取ったメッセージ本体を
// チャネルへ流すので、呼び出し側はそれを検査できます。
func startFakeSMTP(t *testing.T, opts fakeSMTPOptions) (addr string, received chan string) {
	t.Helper()

	// **ListenConfig を使います。** 素の net.Listen は ctx を取らないため
	// noctx が禁じています (テストの締め切りが待ち受けに伝わらない)。
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("ループバックに待ち受けできないためスキップ: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	received = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		serveFakeSMTP(conn, opts, received)
	}()
	return ln.Addr().String(), received
}

func serveFakeSMTP(conn net.Conn, opts fakeSMTPOptions, received chan<- string) {
	r := bufio.NewReader(conn)
	write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }

	write("220 fake ESMTP")
	var body strings.Builder
	inData := false

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				inData = false
				received <- body.String()
				if opts.rejectData {
					write("554 transaction failed")
					continue
				}
				write("250 OK")
				continue
			}
			body.WriteString(line + "\n")
			continue
		}

		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// **拡張は 1 つも名乗りません。** STARTTLS も AUTH も
			// 使わない経路を検査したいためです。
			write("250 fake")
		case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
			write("250 OK")
		case strings.HasPrefix(line, "DATA"):
			inData = true
			write("354 send data")
		case strings.HasPrefix(line, "QUIT"):
			if opts.dropOnQuit {
				// **221 を返さずに切る。** これが指摘された経路になります。
				return
			}
			write("221 bye")
			return
		default:
			write("250 OK")
		}
	}
}
