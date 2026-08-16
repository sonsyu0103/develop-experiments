package mail

import (
	"encoding/base64"
	"mime"
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/config"
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
