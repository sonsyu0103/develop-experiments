package model

import (
	"errors"
	"strings"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

func TestParseSearchQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "そのまま", raw: "PostgreSQL", want: "PostgreSQL"},
		{name: "前後の空白は落とす", raw: "  Go  ", want: "Go"},
		// TrimSpace は unicode.IsSpace で判定するので、全角スペースも落ちる。
		// 日本語入力では前後に U+3000 が紛れやすい。
		{name: "全角スペースも落とす", raw: "　検索　", want: "検索"},
		{name: "内側の空白は残す", raw: "  Go と Rust  ", want: "Go と Rust"},
		// ワイルドカードはドメインでは特別扱いしない。
		// LIKE のエスケープは永続化層の仕事になる。
		{name: "パーセントはそのまま通す", raw: "100%", want: "100%"},
		{name: "アンダースコアもそのまま通す", raw: "a_b", want: "a_b"},
		{name: "バックスラッシュもそのまま通す", raw: `a\b`, want: `a\b`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSearchQuery(&tt.raw)
			if err != nil {
				t.Fatalf("ParseSearchQuery(%q) が失敗した: %v", tt.raw, err)
			}
			if got == nil {
				t.Fatalf("ParseSearchQuery(%q) = nil, want %q", tt.raw, tt.want)
			}
			if got.Keyword() != tt.want {
				t.Errorf("Keyword() = %q, want %q", got.Keyword(), tt.want)
			}
		})
	}
}

// 「指定なし」になる入力。**400 にはしません。**
// 検索欄を空のまま送信したフォームが弾かれるのを避けるためです。
func TestParseSearchQuery_NoFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  *string
	}{
		{name: "nil", raw: nil},
		{name: "空文字", raw: strPtr("")},
		{name: "半角スペースだけ", raw: strPtr("   ")},
		{name: "全角スペースだけ", raw: strPtr("　")},
		{name: "タブと改行だけ", raw: strPtr("\t\n")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSearchQuery(tt.raw)
			if err != nil {
				t.Fatalf("err = %v, want nil (空は 400 にしない)", err)
			}
			if got != nil {
				t.Errorf("SearchQuery = %q, want nil (絞り込みなし)", got.Keyword())
			}
		})
	}
}

func TestParseSearchQuery_TooLong(t *testing.T) {
	t.Parallel()

	// 上限ちょうどは通る。
	ok := strings.Repeat("あ", SearchQueryMaxLength)
	if _, err := ParseSearchQuery(&ok); err != nil {
		t.Errorf("%d 文字が拒否された: %v", SearchQueryMaxLength, err)
	}

	// **バイト数ではなく文字数で数えること。** バイト数で見ていると
	// 日本語では上限の 3 分の 1 で弾かれる。
	tooLong := strings.Repeat("あ", SearchQueryMaxLength+1)
	_, err := ParseSearchQuery(&tooLong)
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
	}
}

// **制御文字は 400。** とくに NUL (U+0000)。
//
// PostgreSQL の text は NUL を格納できず、パラメータとして送ると
// サーバが SQLSTATE 22021 を返す。ここで弾かないと
// `?q=%00` の一撃で未ログインの誰でも 500 を作れる (レビュー指摘)。
func TestParseSearchQuery_RejectsControlCharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "NUL だけ", raw: "\x00"},
		{name: "語の内側の NUL", raw: "a\x00b"},
		{name: "語の内側の改行", raw: "a\nb"},
		{name: "語の内側のタブ", raw: "a\tb"},
		{name: "DEL", raw: "a\x7fb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseSearchQuery(&tt.raw)
			if !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
			}
		})
	}
}

// **端の改行やタブは「指定なし」のまま。** 制御文字の検査より
// TrimSpace が先に効くので、400 にはならない。
// 順序を入れ替えると、空欄のフォーム送信が 400 になりうる。
func TestParseSearchQuery_TrimmedControlCharactersAreNoFilter(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"\n", "\t", " \n\t "} {
		got, err := ParseSearchQuery(&raw)
		if err != nil {
			t.Errorf("ParseSearchQuery(%q) が失敗した: %v", raw, err)
		}
		if got != nil {
			t.Errorf("SearchQuery = %q, want nil (絞り込みなし)", got.Keyword())
		}
	}
}

// 空白を落とした結果が上限に収まるなら通ること。
// 空白を落とす前に長さを見ていると、ここで落ちる。
func TestParseSearchQuery_TrimBeforeLengthCheck(t *testing.T) {
	t.Parallel()

	raw := "  " + strings.Repeat("あ", SearchQueryMaxLength) + "  "
	if _, err := ParseSearchQuery(&raw); err != nil {
		t.Errorf("前後の空白込みで上限を超える入力が拒否された: %v", err)
	}
}

func strPtr(s string) *string { return &s }
