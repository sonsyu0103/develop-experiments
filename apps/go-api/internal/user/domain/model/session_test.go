package model

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestNewSession(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()

	s, _, err := NewSession(42, now, time.Hour)
	if err != nil {
		t.Fatalf("NewSession が失敗した: %v", err)
	}
	if s.UserID != 42 {
		t.Errorf("UserID = %d, want 42", s.UserID)
	}
	if !s.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("ExpiresAt = %v, want %v", s.ExpiresAt, now.Add(time.Hour))
	}
}

func TestNewSession_DefaultTTL(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()

	for _, ttl := range []time.Duration{0, -time.Hour} {
		s, _, err := NewSession(1, now, ttl)
		if err != nil {
			t.Fatalf("NewSession が失敗した: %v", err)
		}
		if !s.ExpiresAt.Equal(now.Add(DefaultSessionTTL)) {
			t.Errorf("ttl=%v のとき ExpiresAt = %v, want 既定値の %v", ttl, s.ExpiresAt, DefaultSessionTTL)
		}
	}
}

func TestNewSession_RejectsInvalidUserID(t *testing.T) {
	t.Parallel()

	for _, id := range []int64{0, -1} {
		if _, _, err := NewSession(id, time.Now(), time.Hour); err == nil {
			t.Errorf("user_id = %d が通ってしまった", id)
		}
	}
}

// セッション ID は「推測できないこと」がこのエンティティの性質そのものです。
// 連番や固定値に退行すると、総当たりで他人になれます。
func TestNewSession_TokenIsRandomAndURLSafe(t *testing.T) {
	t.Parallel()

	now := time.Now()
	seen := make(map[string]bool, 500)

	for range 500 {
		_, token, err := NewSession(1, now, time.Hour)
		if err != nil {
			t.Fatalf("NewSession が失敗した: %v", err)
		}

		if seen[string(token)] {
			t.Fatalf("セッショントークンが重複した: %s", token)
		}
		seen[string(token)] = true

		// Cookie に載せるので URL 安全な文字だけ。
		if strings.ContainsAny(string(token), "=+/") {
			t.Fatalf("URL 安全でない文字が含まれている: %q", token)
		}

		raw, err := base64.RawURLEncoding.DecodeString(string(token))
		if err != nil {
			t.Fatalf("base64url として復号できない (%q): %v", token, err)
		}
		if len(raw) != sessionTokenBytes {
			t.Fatalf("乱数が %d バイト, want %d", len(raw), sessionTokenBytes)
		}
	}
}

// **保存する値と Cookie に入れる値が別であること。**
//
// ここが同じに戻ると、DB を読めるだけの穴 (無関係なクエリの SQL インジェクション、
// バックアップの流出、調査用のダンプ) がそのまま全利用者へのなりすましになる。
func TestNewSession_StoresHashNotToken(t *testing.T) {
	t.Parallel()

	s, token, err := NewSession(1, time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("NewSession が失敗した: %v", err)
	}

	if s.ID == string(token) {
		t.Fatal("保存される ID が生のトークンと同じになっている")
	}
	if s.ID != token.Hash() {
		t.Errorf("ID = %q, want トークンのハッシュ %q", s.ID, token.Hash())
	}
	// SHA-256 の 16 進表現は 64 文字。
	if len(s.ID) != 64 {
		t.Errorf("ID の長さ = %d, want 64 (SHA-256 の hex)", len(s.ID))
	}
	// 生のトークンが ID に含まれていないこと (前方一致などの取り違え防止)。
	if strings.Contains(s.ID, string(token)) {
		t.Error("ID に生のトークンが含まれている")
	}
}

func TestSessionToken_HashIsStable(t *testing.T) {
	t.Parallel()

	const token = SessionToken("abc")

	first, second := token.Hash(), token.Hash()
	if first != second {
		t.Fatalf("同じトークンから違うハッシュが出た: %q と %q", first, second)
	}
	if other := SessionToken("abd").Hash(); other == first {
		t.Fatal("違うトークンから同じハッシュが出た")
	}
}

func TestSession_IsExpired(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	s, _, err := NewSession(1, now, time.Hour)
	if err != nil {
		t.Fatalf("NewSession が失敗した: %v", err)
	}

	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{name: "発行直後", at: now, want: false},
		{name: "期限の 1 秒前", at: now.Add(time.Hour - time.Second), want: false},
		{name: "期限ちょうどは切れている", at: now.Add(time.Hour), want: true},
		{name: "期限後", at: now.Add(2 * time.Hour), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.IsExpired(tt.at); got != tt.want {
				t.Errorf("IsExpired(%v) = %v, want %v", tt.at, got, tt.want)
			}
		})
	}
}
