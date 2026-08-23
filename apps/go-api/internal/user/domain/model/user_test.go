package model

import (
	"errors"
	"regexp"
	"strconv"
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
		// **弾かずに直す。** ここに来るのは Google の claims で、
		// 利用者がこのアプリから直せる値ではない。落とすと
		// **その人は永久にログインできず、手の打ちようがない。**
		{
			name: "表示名が空ならメールのローカル部で代用する", googleSub: "sub-1",
			email: "hoshino@example.com", displayName: "", wantName: "hoshino",
		},
		{
			name: "表示名が空白のみでも代用する", googleSub: "sub-1",
			email: "hoshino@example.com", displayName: "   ", wantName: "hoshino",
		},
		{
			name: "長すぎる表示名は切り詰める", googleSub: "sub-1", email: "a@example.com",
			displayName: strings.Repeat("あ", DisplayNameMaxLength+1),
			wantName:    strings.Repeat("あ", DisplayNameMaxLength),
		},
		{
			// **バイトではなく文字で切る。** 途中で割ると不正な UTF-8 になり、
			// DB の char_length と数え方もずれる。
			name: "絵文字でも文字単位で切る", googleSub: "sub-1", email: "a@example.com",
			displayName: strings.Repeat("🦊", DisplayNameMaxLength+5),
			wantName:    strings.Repeat("🦊", DisplayNameMaxLength),
		},

		// ローカル部も取れないときだけ落とす (メールの検査が先に通っている以上、
		// ここに来るのは `@example.com` のような壊れた値だけ)。
		{
			name: "表示名もローカル部も空", googleSub: "sub-1",
			email: "@example.com", displayName: "", wantErr: true,
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

// **表示名の上限が DB の CHECK 制約と一致していること。**
//
//	この定数 / DB の CHECK 制約 (000002)
//
// **仕様書には無い。** 表示名は API の入力ではなく、
// OIDC プロバイダが返した値をそのまま受けるためです (ADR 0005)。
// そのため 3 か所ではなく 2 か所になりますが、ずれたときの出方は同じ ——
// **長い表示名の利用者がログインした瞬間に、保存で 500 になる**
// (ADR 0003 #8)。
func TestDisplayNameMaxLength_AgreesWithConstraint(t *testing.T) {
	t.Parallel()

	migration := readRepoFile(t, "apps/go-api/db/migrations/000002_add_users_and_sessions.up.sql")
	m := regexp.MustCompile(`char_length\(display_name\)\s+BETWEEN 1 AND (\d+)`).
		FindStringSubmatch(migration)
	if m == nil {
		t.Fatal("000002 から display_name の CHECK 制約を読み取れなかった")
	}
	got, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%q を整数として読めない: %v", m[1], err)
	}
	if got != DisplayNameMaxLength {
		t.Errorf("DB の CHECK 制約 = %d, Go 側 = %d", got, DisplayNameMaxLength)
	}
}
