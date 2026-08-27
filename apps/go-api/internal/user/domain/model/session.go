package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
)

const (
	// sessionTokenBytes はセッショントークンの乱数のバイト数です。
	// 32 バイト (256 ビット) あれば総当たりは現実的でなくなります。
	sessionTokenBytes = 32

	// DefaultSessionTTL はセッションの既定の有効期間です。
	//
	// 長くすると、盗まれたトークンが使える時間も伸びます。
	// 短くすると、利用者が頻繁に再ログインすることになります。
	// リフレッシュの仕組みを入れるまでは 14 日で運用します。
	DefaultSessionTTL = 14 * 24 * time.Hour
)

// tokenEncoding は Cookie に載せるため、URL 安全かつパディングなしにします。
// パディングの '=' は Cookie の値としても扱いが面倒になるためです。
var tokenEncoding = base64.RawURLEncoding

// SessionToken はクライアントに渡す生のセッショントークンです。
//
// **この値は DB に保存しません。** 保存するのは Hash() の結果です。
//
// 生の値を保存すると、テーブルを読めるだけの穴
// (無関係なクエリの SQL インジェクション、バックアップの流出、
// 調査用のダンプ) が、そのまま全利用者へのなりすましになります。
// ハッシュを保存しておけば、漏れても原像を求められません。
//
// 独立した型にしているのは、ハッシュ済みの文字列と取り違えないためです。
// リポジトリは SessionToken を受け取り、内部で Hash() を呼びます
// —— 呼び出し側がハッシュ化を忘れる余地を型で塞いでいます。
type SessionToken string

// Hash は DB に保存する形へ変換します。
//
// SHA-256 をそのまま使い、ソルトも伸長 (bcrypt など) も行いません。
// トークンは 256 ビットの乱数であり、辞書攻撃も総当たりも成立しないためです。
// パスワードとは前提が違います。
func (t SessionToken) Hash() string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// Session はサーバ側で保持する認証セッションです。
//
// ステートレスな JWT ではなくこの形にしたのは、失効させるためです。
// ログアウト・退会・権限剥奪が即座に効く必要があります
// (docs/adr/0005-authentication.md 決定 1)。
type Session struct {
	// ID は保存される値、すなわちトークンのハッシュです。
	// **Cookie に入れる値ではありません。**
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

// NewSession は新しいセッションと、クライアントに渡すトークンを組み立てます。
//
// トークンの生成をドメイン側に置いているのは、
// **「推測できない値であること」がこのエンティティの性質そのもの**だからです。
// 呼び出し側に任せると、テスト用の固定値や連番が本番経路に紛れ込みうる形になります。
//
// 戻り値のトークンは**この瞬間にしか手に入りません**。
// 保存されるのはハッシュなので、後から復元することはできません。
//
// ttl が 0 以下の場合は DefaultSessionTTL を使います。
func NewSession(userID int64, now time.Time, ttl time.Duration) (*Session, SessionToken, error) {
	if userID <= 0 {
		return nil, "", fmt.Errorf("user_id が不正です (%d): %w", userID, apperr.ErrInvalidArgument)
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}

	buf := make([]byte, sessionTokenBytes)
	// crypto/rand の Read は、読み切れなければエラーを返す。
	// math/rand は絶対に使わない (予測可能なのでセッションには使えない)。
	if _, err := rand.Read(buf); err != nil {
		return nil, "", fmt.Errorf("セッショントークンの生成に失敗しました: %w", err)
	}

	token := SessionToken(tokenEncoding.EncodeToString(buf))

	return &Session{
		ID:        token.Hash(),
		UserID:    userID,
		ExpiresAt: now.Add(ttl),
	}, token, nil
}

// ReconstructSession は永続化層から読み出した値でセッションを復元します。
// id はハッシュ済みの値です。
func ReconstructSession(id string, userID int64, expiresAt, createdAt time.Time) *Session {
	return &Session{
		ID:        id,
		UserID:    userID,
		ExpiresAt: expiresAt,
		CreatedAt: createdAt,
	}
}

// IsExpired は指定時刻の時点で期限切れかを返します。
//
// 【重要】これは表示や早期リターンのための補助であり、認可の判定に使わないこと。
// 期限の判定は SQL 側 (expires_at > now()) が正であり、
// アプリ側で持つと「判定を書き忘れた経路」が穴になります
// (db/query/sessions.sql の GetLiveSessionWithUser)。
func (s *Session) IsExpired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}
