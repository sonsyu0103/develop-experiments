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

	s, err := NewSession(42, now, time.Hour)
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
		s, err := NewSession(1, now, ttl)
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
		if _, err := NewSession(id, time.Now(), time.Hour); err == nil {
			t.Errorf("user_id = %d が通ってしまった", id)
		}
	}
}

// セッション ID は「推測できないこと」がこのエンティティの性質そのものです。
// 連番や固定値に退行すると、総当たりで他人になれます。
func TestNewSession_IDIsRandomAndURLSafe(t *testing.T) {
	t.Parallel()

	now := time.Now()
	seen := make(map[string]bool, 500)

	for range 500 {
		s, err := NewSession(1, now, time.Hour)
		if err != nil {
			t.Fatalf("NewSession が失敗した: %v", err)
		}

		if seen[s.ID] {
			t.Fatalf("セッション ID が重複した: %s", s.ID)
		}
		seen[s.ID] = true

		// Cookie に載せるので URL 安全な文字だけ。
		if strings.ContainsAny(s.ID, "=+/") {
			t.Fatalf("URL 安全でない文字が含まれている: %q", s.ID)
		}

		raw, err := base64.RawURLEncoding.DecodeString(s.ID)
		if err != nil {
			t.Fatalf("base64url として復号できない (%q): %v", s.ID, err)
		}
		if len(raw) != sessionIDBytes {
			t.Fatalf("乱数が %d バイト, want %d", len(raw), sessionIDBytes)
		}
	}
}

func TestSession_IsExpired(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	s, err := NewSession(1, now, time.Hour)
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
