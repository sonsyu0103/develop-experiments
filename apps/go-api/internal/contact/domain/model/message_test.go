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

	// **下限も見ます。** 以前は上限だけを拾い、email の行は
	// `BETWEEN \d+ AND (\d+)` と書いて**下限を捨てていました**。
	// DB だけ BETWEEN 6 AND 254 に変えても CI は緑のまま通り、
	// Normalize が通した `a@b` を DB が拒否する ——
	// **アプリが受理した入力で 500** になります。
	// このファイルのコメントが「一番まずい壊れ方」として挙げている形そのものです。
	cases := []struct {
		field string
		// declaredMin / declaredMax は Go 側の定数です。
		declaredMin int
		declaredMax int
		// constraint は 000012 の CHECK 制約から下限と上限を拾う正規表現です。
		constraint string
	}{
		{"name", 1, MaxNameLength, `char_length\(name\)\s+BETWEEN (\d+) AND (\d+)`},
		{"email", MinEmailLength, MaxEmailLength, `char_length\(email\)\s+BETWEEN (\d+) AND (\d+)`},
		{"subject", 1, MaxSubjectLength, `char_length\(subject\)\s+BETWEEN (\d+) AND (\d+)`},
		{"body", 1, MaxBodyLength, `char_length\(body\)\s+BETWEEN (\d+) AND (\d+)`},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			bounds := regexp.MustCompile(tc.constraint).FindStringSubmatch(migration)
			if bounds == nil {
				t.Fatalf("000012 から %s の CHECK 制約を読み取れなかった", tc.field)
			}
			if got := mustAtoi(t, bounds[1]); got != tc.declaredMin {
				t.Errorf("DB の CHECK 制約の下限 (%s) = %d, Go 側 = %d", tc.field, got, tc.declaredMin)
			}
			if got := mustAtoi(t, bounds[2]); got != tc.declaredMax {
				t.Errorf("DB の CHECK 制約の上限 (%s) = %d, Go 側 = %d", tc.field, got, tc.declaredMax)
			}
			// 仕様書側は「フィールド名の次に現れる minLength / maxLength」を読みます。
			minPattern := `(?ms)^        ` + tc.field + `:\n.*?minLength: (\d+)`
			if got := intFrom(t, block[1], minPattern); got != tc.declaredMin {
				t.Errorf("仕様書の minLength (%s) = %d, Go 側 = %d", tc.field, got, tc.declaredMin)
			}
			pattern := `(?ms)^        ` + tc.field + `:\n.*?maxLength: (\d+)`
			if got := intFrom(t, block[1], pattern); got != tc.declaredMax {
				t.Errorf("仕様書の maxLength (%s) = %d, Go 側 = %d", tc.field, got, tc.declaredMax)
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
// mustAtoi は取り出した数字を整数にします。
// intFrom と違って、正規表現の当て方は呼び出し側が決めます
// (下限と上限を 1 回の照合で取りたいため)。
func mustAtoi(t *testing.T, raw string) int {
	t.Helper()

	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%q を整数として読めない: %v", raw, err)
	}
	return n
}

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

// **条件つき制約が、こちらの知っている値だけを使っていること。**
//
//	contact_sent_complete  (status = 'sent' / <> 'sent')
//
// 列挙の CHECK 制約と違い、**値が本文に直書きされています。**
// `StatusSent` を改名すると `contact_status_valid` 側は上の
// TestStatuses_AgreeWithConstraint が落ちて気づけますが、
// **こちらは古い値を参照したまま残ります。**
//
// そのとき壊れるのは「送信が完了した行だけ保存できない」という形になり、
// **ワーカーが送った直後に落ちる** —— 一番気づきにくい壊れ方になります
// (ADR 0003 #8)。
func TestConditionalConstraints_UseKnownValues(t *testing.T) {
	t.Parallel()

	src := readRepoFile(t, "apps/go-api/db/migrations/000012_add_contact_messages.up.sql")

	known := map[string]bool{}
	for _, s := range AllStatuses {
		known[string(s)] = true
	}
	for _, v := range literalsIn(t, src, "contact_sent_complete") {
		if !known[v] {
			t.Errorf("contact_sent_complete が知らない値 %q を使っている", v)
		}
	}
}

// literalsIn は名前つき CHECK 制約の本文から、引用符つきの値を全部拾います。
//
// **括弧の対応を数えます。** 条件つき制約は入れ子になっているため、
// 最初の閉じ括弧までを取ると途中で切れます。
func literalsIn(t *testing.T, sql, constraint string) []string {
	t.Helper()

	head := regexp.MustCompile(`CONSTRAINT\s+` + constraint + `\s+CHECK\s*\(`).
		FindStringIndex(sql)
	if head == nil {
		t.Fatalf("%s を読み取れなかった", constraint)
	}
	start := head[1] - 1
	depth, end := 0, -1
	for i := start; i < len(sql) && end < 0; i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		t.Fatalf("%s の括弧が閉じていない", constraint)
	}

	var out []string
	for _, m := range regexp.MustCompile(`'([^']*)'`).
		FindAllStringSubmatch(sql[start:end], -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s からリテラルを 1 つも拾えなかった", constraint)
	}
	return out
}
