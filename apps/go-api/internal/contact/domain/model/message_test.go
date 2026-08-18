package model

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

// **知らない値を既定値へ丸めないこと。**
//
// 丸めると、DB に入った未知の値が「未送信」として扱われ、
// 送信済みの問い合わせがもう一度送られます。
func TestParseStatus(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"pending", "sent", "failed"} {
		if _, err := ParseStatus(in); err != nil {
			t.Errorf("ParseStatus(%q) が失敗した: %v", in, err)
		}
	}
	for _, in := range []string{"", "PENDING", " pending", "queued"} {
		if _, err := ParseStatus(in); !errors.Is(err, apperr.ErrInvalidArgument) {
			t.Errorf("ParseStatus(%q) が通ってしまった", in)
		}
	}
}

// **正規化が保存前に効いていること。**
//
// 前後の空白を落とさずに保存すると、空白ぶんだけ上限を超えた入力が
// DB の CHECK 制約に当たって 500 になります。
func TestSubmission_Normalize_Trims(t *testing.T) {
	t.Parallel()

	got, err := validSubmission(func(s *Submission) {
		s.Name = "  ホシノ  "
		s.Email = "  hoshino@example.com "
		s.Subject = "\tログインできません "
		s.Body = "  本文です  "
	}).Normalize()
	if err != nil {
		t.Fatalf("正常な入力が弾かれた: %v", err)
	}

	if got.Name != "ホシノ" || got.Email != "hoshino@example.com" ||
		got.Subject != "ログインできません" || got.Body != "本文です" {
		t.Errorf("正規化されていない: %+v", got)
	}
}

// **改行コードが LF に揃うこと。**
//
// CRLF のまま数えると、同じ本文が入力経路によって別の長さになります ——
// 上限の判定が「どのブラウザから送ったか」で変わることになります。
func TestSubmission_Normalize_NormalizesNewlines(t *testing.T) {
	t.Parallel()

	got, err := validSubmission(func(s *Submission) {
		s.Body = "1 行目\r\n2 行目\r3 行目"
	}).Normalize()
	if err != nil {
		t.Fatalf("正常な入力が弾かれた: %v", err)
	}
	if want := "1 行目\n2 行目\n3 行目"; got.Body != want {
		t.Errorf("本文 = %q, want %q", got.Body, want)
	}
}

// **氏名・件名に改行を入れられないこと** (ADR 0008 決定 3 の二重防御)。
//
// これらの値はメールヘッダに入らない約束ですが、
// **約束が破られた日にヘッダインジェクションになる**のがこの 3 つです。
func TestSubmission_Normalize_RejectsHeaderInjection(t *testing.T) {
	t.Parallel()

	injections := []string{
		"ホシノ\r\nBcc: victim@example.com",
		"ホシノ\nBcc: victim@example.com",
		"ホシノ\x00",
	}
	for _, in := range injections {
		t.Run(strconv.Quote(in), func(t *testing.T) {
			t.Parallel()

			if _, err := validSubmission(func(s *Submission) { s.Name = in }).Normalize(); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("氏名 %q が通ってしまった (err = %v)", in, err)
			}
			if _, err := validSubmission(func(s *Submission) { s.Subject = in }).Normalize(); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("件名 %q が通ってしまった (err = %v)", in, err)
			}
		})
	}
}

// **本文の改行は残ること。** 1 行に潰すと読めなくなります。
// 一方、改行とタブ以外の制御文字は拒否します。
func TestSubmission_Normalize_BodyControlChars(t *testing.T) {
	t.Parallel()

	if _, err := validSubmission(func(s *Submission) {
		s.Body = "1 行目\n2 行目\tタブ"
	}).Normalize(); err != nil {
		t.Errorf("改行とタブが弾かれた: %v", err)
	}

	if _, err := validSubmission(func(s *Submission) {
		s.Body = "本文\x07です"
	}).Normalize(); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Error("制御文字を含む本文が通ってしまった")
	}
}

// **表示名つきのアドレスを拒否すること。**
//
// 素通しすると「アドレス欄に見える文字列」と「実際のアドレス」がずれます。
// この値は本文にそのまま載るので、読む側が別の宛先へ返信しかねません。
func TestSubmission_Normalize_Email(t *testing.T) {
	t.Parallel()

	ok := []string{"a@b", "hoshino@example.com", "user+tag@example.co.jp"}
	for _, in := range ok {
		if _, err := validSubmission(func(s *Submission) { s.Email = in }).Normalize(); err != nil {
			t.Errorf("正しいアドレス %q が弾かれた: %v", in, err)
		}
	}

	ng := []string{
		"",
		"a@",
		"@b",
		"ホシノ <hoshino@example.com>",
		"a@b, c@d",
		"a@b\r\nBcc: victim@example.com",
	}
	for _, in := range ng {
		if _, err := validSubmission(func(s *Submission) { s.Email = in }).Normalize(); !errors.Is(err, apperr.ErrInvalidArgument) {
			t.Errorf("不正なアドレス %q が通ってしまった", in)
		}
	}
}

// **長さは文字数で数えること。**
//
// バイト数で数えると、PostgreSQL の char_length との解釈がずれ、
// 日本語の入力がアプリを通って DB で落ちます (逆もあります)。
func TestSubmission_Normalize_CountsRunesNotBytes(t *testing.T) {
	t.Parallel()

	// 100 文字ちょうど (UTF-8 では 300 バイト)。
	if _, err := validSubmission(func(s *Submission) {
		s.Name = strings.Repeat("あ", MaxNameLength)
	}).Normalize(); err != nil {
		t.Errorf("%d 文字の氏名が弾かれた: %v", MaxNameLength, err)
	}
	if _, err := validSubmission(func(s *Submission) {
		s.Name = strings.Repeat("あ", MaxNameLength+1)
	}).Normalize(); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Errorf("%d 文字の氏名が通ってしまった", MaxNameLength+1)
	}

	if _, err := validSubmission(func(s *Submission) {
		s.Body = strings.Repeat("あ", MaxBodyLength+1)
	}).Normalize(); !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Error("上限を超える本文が通ってしまった")
	}
}

// **空の入力を弾くこと。** 空白だけも空と同じ扱いになります。
func TestSubmission_Normalize_RejectsEmpty(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*Submission){
		"name":    func(s *Submission) { s.Name = "   " },
		"email":   func(s *Submission) { s.Email = "" },
		"subject": func(s *Submission) { s.Subject = "\t" },
		"body":    func(s *Submission) { s.Body = "\n\n" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := validSubmission(mutate).Normalize(); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("空の %s が通ってしまった", name)
			}
		})
	}
}

// **3 か所の上限が揃っていること。**
//
//	この定数群 / DB の CHECK 制約 (000012) / 仕様書の maxLength
//
// 揃っていないと「アプリが通した入力を DB が拒否して 500」または
// 「仕様書より長い入力が通る」形で表に出ます。
// ADR 0011 の enum 検査 (action_test.go) と同じ考え方です。
func TestLengthLimits_AgreeAcrossSources(t *testing.T) {
	t.Parallel()

	migration := readRepoFile(t, "apps/go-api/db/migrations/000012_add_contact_messages.up.sql")
	spec := readRepoFile(t, "api/openapi.yaml")
	// 仕様書は CreateContactRequest の中だけを見ます ——
	// 他のスキーマにも maxLength があるためです。
	block := regexp.MustCompile(`(?ms)^    CreateContactRequest:\n(.*?)\n    \w+:`).FindStringSubmatch(spec)
	if block == nil {
		t.Fatal("openapi.yaml から CreateContactRequest を読み取れなかった")
	}

	cases := []struct {
		field    string
		declared int
		// constraint は 000012 の CHECK 制約から上限を拾う正規表現です。
		constraint string
	}{
		{"name", MaxNameLength, `char_length\(name\)\s+BETWEEN 1 AND (\d+)`},
		{"email", MaxEmailLength, `char_length\(email\)\s+BETWEEN \d+ AND (\d+)`},
		{"subject", MaxSubjectLength, `char_length\(subject\)\s+BETWEEN 1 AND (\d+)`},
		{"body", MaxBodyLength, `char_length\(body\)\s+BETWEEN 1 AND (\d+)`},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			if got := intFrom(t, migration, tc.constraint); got != tc.declared {
				t.Errorf("DB の CHECK 制約 (%s) = %d, Go 側 = %d", tc.field, got, tc.declared)
			}
			// 仕様書側は「フィールド名の次に現れる maxLength」を読みます。
			pattern := `(?ms)^        ` + tc.field + `:\n.*?maxLength: (\d+)`
			if got := intFrom(t, block[1], pattern); got != tc.declared {
				t.Errorf("仕様書の maxLength (%s) = %d, Go 側 = %d", tc.field, got, tc.declared)
			}
		})
	}
}

// **状態の定義が DB の CHECK 制約と揃っていること。**
func TestStatuses_AgreeWithConstraint(t *testing.T) {
	t.Parallel()

	src := readRepoFile(t, "apps/go-api/db/migrations/000012_add_contact_messages.up.sql")
	m := regexp.MustCompile(`CHECK \(status IN \(([^)]+)\)\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("000012 から status の CHECK 制約を読み取れなかった")
	}

	inDB := map[string]bool{}
	for _, v := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
		inDB[v[1]] = true
	}
	for _, s := range AllStatuses {
		if !inDB[string(s)] {
			t.Errorf("DB の CHECK 制約に %q が無い", s)
		}
		delete(inDB, string(s))
	}
	for v := range inDB {
		t.Errorf("DB にだけ %q がある (Go 側に無い)", v)
	}
}

// validSubmission は正しい入力を作り、必要な項目だけを差し替えます。
func validSubmission(mutate func(*Submission)) Submission {
	s := Submission{
		Name:    "ホシノ",
		Email:   "hoshino@example.com",
		Subject: "ログインできません",
		Body:    "Google でログインすると画面が戻ってきます。",
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

// intFrom は正規表現の 1 つ目のグループを整数として読みます。
func intFrom(t *testing.T, src, pattern string) int {
	t.Helper()

	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%q に一致しなかった", pattern)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%q を整数として読めない: %v", m[1], err)
	}
	return n
}

// readRepoFile はリポジトリ直下からの相対パスでファイルを読みます。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	// このファイルは apps/go-api/internal/contact/domain/model にある。
	root := filepath.Join("..", "..", "..", "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("%s を読めない: %v", rel, err)
	}
	return string(b)
}

// **検証済みアドレスの関門が、ヘッダを壊す値を通さないこと** (ADR 0008 決定 2)。
//
// VerifiedEmail は「所有権が確認済み」を表す型ですが、
// 中身は DB から来た文字列です (users.email に CHECK 制約は無く、
// IdP が返した値がそのまま入っています)。
// **出どころが信用できることと、形が正しいことは別**になります。
func TestNewVerifiedEmail_RejectsHeaderBreakers(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		"user@example.net\r\nBcc: victim@example.com",
		// 表示名つきは「見えるアドレス」と「実際の宛先」がずれる。
		"ホシノ <user@example.net>",
		"こわれている",
		strings.Repeat("a", 250) + "@example.com", // 254 文字の上限超え
	} {
		if _, err := NewVerifiedEmail(raw); err == nil {
			t.Errorf("不正なアドレスが検証済みとして通った: %q", raw)
		}
	}

	v, err := NewVerifiedEmail("user@example.net")
	if err != nil {
		t.Fatalf("正しいアドレスが弾かれた: %v", err)
	}
	if v.String() != "user@example.net" {
		t.Errorf("String() = %q", v.String())
	}
}
