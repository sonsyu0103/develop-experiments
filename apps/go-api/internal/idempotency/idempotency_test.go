package idempotency

import (
	"errors"
	"strings"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

func TestNew_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     string
		wantKey string
		wantErr bool
	}{
		{name: "通常のキー", key: "6f0c2a1e-6e6a", wantKey: "6f0c2a1e-6e6a"},
		{name: "前後の空白は落とす", key: "  abc  ", wantKey: "abc"},
		{name: "空は弾く", key: "", wantErr: true},
		{name: "空白だけも弾く", key: "   ", wantErr: true},
		{
			name:    "上限ちょうどは通る",
			key:     strings.Repeat("a", MaxKeyLength),
			wantKey: strings.Repeat("a", MaxKeyLength),
		},
		{
			name:    "上限超過は弾く (DB の CHECK 制約に到達させない)",
			key:     strings.Repeat("a", MaxKeyLength+1),
			wantErr: true,
		},
		{
			// **バイト数ではなく文字数で数える。**
			// バイト数で見ると、日本語のキーが 85 文字で弾かれる。
			name:    "マルチバイトでも文字数で数える",
			key:     strings.Repeat("あ", MaxKeyLength),
			wantKey: strings.Repeat("あ", MaxKeyLength),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := New(tt.key, "POST /threads/1/comments", "本文")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("エラーにならなかった (key=%q)", tt.key)
				}
				if !errors.Is(err, apperr.ErrInvalidArgument) {
					t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New が失敗した: %v", err)
			}
			if got.Key != tt.wantKey {
				t.Errorf("Key = %q, want %q", got.Key, tt.wantKey)
			}
			if got.RequestHash == "" {
				t.Error("RequestHash が空 (同じキーで別の内容を検出できない)")
			}
		})
	}
}

// 内容が変われば指紋も変わること。
//
// ここが効かないと、**同じキーで別の内容を送っても 422 にならず、
// 前回の結果が黙って返る**。投稿が 1 件失われる。
func TestNew_HashChangesWithContent(t *testing.T) {
	t.Parallel()

	base := mustNew(t, "k1", "POST /threads/1/comments", "ホシノ", "ふぁ〜")

	tests := []struct {
		name     string
		endpoint string
		fields   []string
	}{
		{
			name:     "本文が違う",
			endpoint: "POST /threads/1/comments",
			fields:   []string{"ホシノ", "おはよう"},
		},
		{
			name:     "投稿者名が違う",
			endpoint: "POST /threads/1/comments",
			fields:   []string{"先生", "ふぁ〜"},
		},
		{
			// **スレッドが違えば別の要求。**
			// 経路を雛形 (/threads/:threadId/comments) にすると
			// ここが同じ指紋になり、別スレッドへの投稿が握りつぶされる。
			name:     "スレッドが違う",
			endpoint: "POST /threads/2/comments",
			fields:   []string{"ホシノ", "ふぁ〜"},
		},
		{
			name:     "項目が減る",
			endpoint: "POST /threads/1/comments",
			fields:   []string{"ホシノ"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			other := mustNew(t, "k1", tt.endpoint, tt.fields...)
			if other.RequestHash == base.RequestHash {
				t.Errorf("指紋が同じ (%s を変えても検出できない)", tt.name)
			}
		})
	}
}

// 同じ内容なら指紋も同じであること。
// ここが不安定だと、正しいクライアントが再送のたびに 422 を受け取る。
func TestNew_HashIsStable(t *testing.T) {
	t.Parallel()

	a := mustNew(t, "k1", "POST /threads/1/comments", "ホシノ", "ふぁ〜")
	b := mustNew(t, "k1", "POST /threads/1/comments", "ホシノ", "ふぁ〜")

	if a.RequestHash != b.RequestHash {
		t.Errorf("同じ内容で指紋が違う: %s vs %s", a.RequestHash, b.RequestHash)
	}
}

// **区切りの無い連結を使っていないこと。**
//
// 単純に連結すると ("ab", "c") と ("a", "bc") が同じ指紋になる。
// 別の内容の要求が「同じ内容の再送」と判定され、
// **2 通目の投稿が失われたうえで 1 通目の結果が返る。**
// 気づくのは「投稿したのに増えていない」という報告が来たときになる。
func TestNew_HashIsNotAmbiguousAcrossFieldBoundaries(t *testing.T) {
	t.Parallel()

	a := mustNew(t, "k1", "POST /x", "ab", "c")
	b := mustNew(t, "k1", "POST /x", "a", "bc")

	if a.RequestHash == b.RequestHash {
		t.Error(`("ab","c") と ("a","bc") の指紋が同じ (項目の区切りが無い)`)
	}
}

func mustNew(t *testing.T, key, endpoint string, fields ...string) *Request {
	t.Helper()

	got, err := New(key, endpoint, fields...)
	if err != nil {
		t.Fatalf("New が失敗した: %v", err)
	}
	return got
}
