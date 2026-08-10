package model

import (
	"errors"
	"strings"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

func TestNewUser(t *testing.T) {
	t.Parallel()

	avatar := "https://example.com/a.png"

	tests := []struct {
		name        string
		googleSub   string
		email       string
		displayName string
		wantName    string
		wantErr     bool
	}{
		{name: "通常", googleSub: "sub-1", email: "a@example.com", displayName: "ホシノ", wantName: "ホシノ"},
		{
			name: "前後の空白は落とす", googleSub: " sub-1 ", email: " a@example.com ",
			displayName: "  ホシノ  ", wantName: "ホシノ",
		},
		{
			name: "上限ちょうどは通る", googleSub: "sub-1", email: "a@example.com",
			displayName: strings.Repeat("あ", DisplayNameMaxLength),
			wantName:    strings.Repeat("あ", DisplayNameMaxLength),
		},

		{name: "google_sub が空", googleSub: "", email: "a@example.com", displayName: "ホシノ", wantErr: true},
		{name: "google_sub が空白のみ", googleSub: "   ", email: "a@example.com", displayName: "ホシノ", wantErr: true},
		{name: "メールが空", googleSub: "sub-1", email: "", displayName: "ホシノ", wantErr: true},
		{name: "表示名が空", googleSub: "sub-1", email: "a@example.com", displayName: "", wantErr: true},
		{
			name: "表示名が長すぎる", googleSub: "sub-1", email: "a@example.com",
			displayName: strings.Repeat("あ", DisplayNameMaxLength+1), wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewUser(tt.googleSub, tt.email, tt.displayName, &avatar)

			if tt.wantErr {
				if !errors.Is(err, apperr.ErrInvalidArgument) {
					t.Fatalf("err = %v, want apperr.ErrInvalidArgument", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("エラーは期待していないが発生した: %v", err)
			}
			if got.DisplayName != tt.wantName {
				t.Errorf("DisplayName = %q, want %q", got.DisplayName, tt.wantName)
			}
			if got.GoogleSub != strings.TrimSpace(tt.googleSub) {
				t.Errorf("GoogleSub = %q", got.GoogleSub)
			}
		})
	}
}

// 表示名の上限は DB の CHECK と揃っている必要があります。
// バイト数で数えていると、日本語で 3 倍ずれます。
func TestNewUser_CountsRunesNotBytes(t *testing.T) {
	t.Parallel()

	// 100 文字 = UTF-8 で 300 バイト。バイト数で数える実装だと弾かれる。
	_, err := NewUser("sub-1", "a@example.com", strings.Repeat("あ", DisplayNameMaxLength), nil)
	if err != nil {
		t.Fatalf("100 文字は通るはずが弾かれた: %v", err)
	}
}

// public_id は採番のたびに違う値でなければなりません。
// 固定値や衝突があると、別人のマイページを指せてしまいます。
func TestNewUser_PublicIDIsUniqueAndSortable(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 100)
	var prev string

	for range 100 {
		u, err := NewUser("sub-1", "a@example.com", "ホシノ", nil)
		if err != nil {
			t.Fatalf("NewUser が失敗した: %v", err)
		}

		got := u.PublicID.String()
		if seen[got] {
			t.Fatalf("public_id が重複した: %s", got)
		}
		seen[got] = true

		// UUID v7 は先頭 48 ビットがミリ秒のタイムスタンプなので、
		// 連続して採番すると文字列としても概ね昇順になる。
		// v4 に戻すとここが崩れ、B-tree の挿入位置が散る。
		if prev != "" && got < prev {
			t.Errorf("public_id が単調増加していない: %s の後に %s", prev, got)
		}
		prev = got

		if v := u.PublicID.Version(); v != 7 {
			t.Fatalf("UUID version = %d, want 7", v)
		}
	}
}

func TestUser_IsDeleted(t *testing.T) {
	t.Parallel()

	u, err := NewUser("sub-1", "a@example.com", "ホシノ", nil)
	if err != nil {
		t.Fatalf("NewUser が失敗した: %v", err)
	}
	if u.IsDeleted() {
		t.Error("新規の利用者が退会済みと判定された")
	}
}
