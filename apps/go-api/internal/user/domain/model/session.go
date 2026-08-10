package model

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"
)

const (
	// sessionIDBytes はセッション ID の乱数のバイト数です。
	// 32 バイト (256 ビット) あれば総当たりは現実的でなくなります。
	sessionIDBytes = 32

	// DefaultSessionTTL はセッションの既定の有効期間です。
	//
	// 長くすると、盗まれた ID が使える時間も伸びます。
	// 短くすると、利用者が頻繁に再ログインすることになります。
	// リフレッシュの仕組みを入れるまでは 14 日で運用します。
	DefaultSessionTTL = 14 * 24 * time.Hour
)

// sessionIDEncoding は Cookie に載せるため、URL 安全かつパディングなしにします。
// パディングの '=' は Cookie の値としても扱いが面倒になるためです。
var sessionIDEncoding = base64.RawURLEncoding

// Session はサーバ側で保持する認証セッションです。
//
// ステートレスな JWT ではなくこの形にしたのは、失効させるためです。
// ログアウト・退会・権限剥奪が即座に効く必要があります
// (docs/adr/0005-authentication.md 決定 1)。
type Session struct {
	ID        string
	UserID    int64
	ExpiresAt time.Time
	CreatedAt time.Time
}

// AuthenticatedSession は検証済みのセッションと、その持ち主を組にした値です。
// 認証つきリクエストで DB を 2 往復しないよう、1 クエリで取得します。
type AuthenticatedSession struct {
	Session Session
	Owner   SessionOwner
}

// NewSession は新しいセッションを組み立てます。
//
// ID の生成をドメイン側に置いているのは、
// **「推測できない値であること」がこのエンティティの性質そのもの**だからです。
// 呼び出し側に任せると、テスト用の固定値や連番が本番経路に紛れ込みうる形になります。
//
// ttl が 0 以下の場合は DefaultSessionTTL を使います。
func NewSession(userID int64, now time.Time, ttl time.Duration) (*Session, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id が不正です (%d)", userID)
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}

	buf := make([]byte, sessionIDBytes)
	// crypto/rand の Read は、読み切れなければエラーを返す。
	// math/rand は絶対に使わない (予測可能なのでセッション ID には使えない)。
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("セッション ID の生成に失敗しました: %w", err)
	}

	return &Session{
		ID:        sessionIDEncoding.EncodeToString(buf),
		UserID:    userID,
		ExpiresAt: now.Add(ttl),
	}, nil
}

// ReconstructSession は永続化層から読み出した値でセッションを復元します。
func ReconstructSession(id string, userID int64, expiresAt, createdAt time.Time) *Session {
	return &Session{
		ID:        id,
		UserID:    userID,
		ExpiresAt: expiresAt,
		CreatedAt: createdAt,
	}
}

// IsExpired は指定時刻の時点で期限切れかを返します.
//
// 【重要】これは表示や早期リターンのための補助であり、認可の判定に使わないこと。
// 期限の判定は SQL 側 (expires_at > now()) が正であり、
// アプリ側で持つと「判定を書き忘れた経路」が穴になります
// (db/query/sessions.sql の GetLiveSessionWithUser)。
func (s *Session) IsExpired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}
