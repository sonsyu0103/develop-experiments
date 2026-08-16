package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"develop-experiments/apps/go-api/internal/apperr"
)

// **符号化できない文字は 400 に翻訳すること** (レビュー指摘)。
//
// PostgreSQL の text は NUL (U+0000) を格納できず、パラメータとして送ると
// SQLSTATE 22021 を返します。拾わないと 500 になり、
// **利用者が送った 1 文字でサーバ内部エラーが出ます。**
//
// 入口 (model.ParseSearchQuery) でも弾いていますが、こちらは
// 検証をすり抜けた経路のための最後の砦です。
func TestTranslateError_CharacterNotInRepertoire(t *testing.T) {
	t.Parallel()

	pgErr := &pgconn.PgError{
		Code:    codeCharacterNotInRepertoire,
		Message: `invalid byte sequence for encoding "UTF8": 0x00`,
	}

	err := translateError("TestRepository.Search", pgErr)
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
	}
	// **操作名を文言に含めないこと。**
	// ErrInvalidArgument のメッセージはそのままクライアントへ返ります
	// (codeCheckViolation と同じ扱い)。
	if got := err.Error(); strings.Contains(got, "TestRepository.Search") {
		t.Errorf("応答の文言に操作名が漏れている: %q", got)
	}
}

// escapeLikePattern は「利用者の 1 文字で全件一致を作られる」のを防ぎます
// (docs/adr/0012-search.md の罠)。
//
// **実 DB を要さない検査です。** 実際に索引が使われるか・一致するかは
// live 検査 (thread_search_live_test.go) 側で見ます。
func TestEscapeLikePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "普通の語はそのまま", in: "PostgreSQL", want: "PostgreSQL"},
		{name: "日本語もそのまま", in: "掲示板", want: "掲示板"},
		// これを打ち消さないと、q=% の 1 文字で全件が返る。
		{name: "パーセント", in: "%", want: `\%`},
		{name: "アンダースコア", in: "_", want: `\_`},
		{name: "パーセントを含む語", in: "100%", want: `100\%`},
		// **エスケープ文字そのものを二重にしないこと。**
		// `\` を素通しすると、続く 1 文字のエスケープを利用者に奪われる。
		{name: "バックスラッシュ", in: `\`, want: `\\`},
		// **1 段だけエスケープされること。** `%` を先に `\%` へ変えてから
		// `\` を `\\` へ変える実装だと、自分が挿入した `\` を拾って
		// `\\%` (「バックスラッシュに続く任意の文字列」) になる。
		{name: "バックスラッシュとパーセント", in: `\%`, want: `\\\%`},
		{name: "全部入り", in: `a\b%c_d`, want: `a\\b\%c\_d`},
		{name: "空文字", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := escapeLikePattern(tt.in); got != tt.want {
				t.Errorf("escapeLikePattern(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
