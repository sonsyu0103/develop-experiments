package model

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

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
		// **メールアドレスは使わない。** DisplayName は Author に載って
		// 未ログインの一覧にも出る。仕様書は email を「本人にだけ返します。
		// 投稿一覧には含まれません」と約束しており、ローカル部だけでも
		// 個人を指す文字列がそこへ混ざる。
		{
			// **期待値をリテラルで書く。** fallbackDisplayName を呼んで
			// 比べると実装どうしの比較になり、中身を "利用者" 固定に
			// 変えても緑のまま通る (変異で実測)。
			name: "表示名が空なら google_sub から作る", googleSub: "sub-1",
			email: "hoshino@example.com", displayName: "",
			wantName: "利用者-0a80627d",
		},
		{
			name: "表示名が空白のみでも同じ", googleSub: "sub-1",
			email: "hoshino@example.com", displayName: "   ",
			wantName: "利用者-0a80627d",
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

		{
			// **UTF-8 の途中で割らないだけでは足りない。** 家族の絵文字は
			// ZWJ (U+200D) つなぎなので、境界で切ると行き場のない ZWJ が
			// 末尾に残り、豆腐 (□) になる。**🦊 のような 1 コードポイントの
			// 絵文字では、この形を踏めない** (初版の検査がそうだった)。
			name: "ZWJ の途中で切れたら破片を落とす", googleSub: "sub-1",
			email: "a@example.com",
			displayName: strings.Repeat("あ", DisplayNameMaxLength-1) +
				"\U0001F468\u200D\U0001F469\u200D\U0001F467",
			wantName: strings.Repeat("あ", DisplayNameMaxLength-1) + "\U0001F468",
		},
		{
			// 旗は地域指示符号の 2 個組。片方だけ残さない。
			name: "旗が半分だけ残らない", googleSub: "sub-1", email: "a@example.com",
			displayName: strings.Repeat("あ", DisplayNameMaxLength-1) + "\U0001F1EF\U0001F1F5",
			wantName:    strings.Repeat("あ", DisplayNameMaxLength-1),
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

// **代替名が満たすべき 3 つの性質。**
//
// 表示名が取れないときの代わりなので、次を同時に満たす必要があります。
//
//	一意である      同じ名前が並ばない
//	毎回同じである  Upsert が display_name を上書きするので、揺れると
//	                ログインのたびに表示名が変わる
//	復元できない    google_sub をそのまま出すと外部の識別子が公開される
//
// **メールアドレスを混ぜないこと**も見ます。DisplayName は Author に載って
// 未ログインの一覧にも出るため、初版のようにローカル部を使うと
// 「email は本人にだけ返す」という仕様書の約束を破ります。
func TestFallbackDisplayName(t *testing.T) {
	t.Parallel()

	const (
		subA = "google-sub-aaa"
		subB = "google-sub-bbb"
	)

	a1, a2, b := fallbackDisplayName(subA), fallbackDisplayName(subA), fallbackDisplayName(subB)

	if a1 != a2 {
		t.Errorf("同じ sub で違う名前になった: %q と %q (ログインのたびに変わる)", a1, a2)
	}
	if a1 == b {
		t.Errorf("違う sub で同じ名前になった: %q (全員が同じ表示名で並ぶ)", a1)
	}
	if strings.Contains(a1, subA) {
		t.Errorf("代替名に google_sub がそのまま入っている: %q", a1)
	}
	if n := utf8.RuneCountInString(a1); n > DisplayNameMaxLength {
		t.Errorf("代替名が上限を超えている: %d 文字", n)
	}
}
