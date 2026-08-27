// Package usecase は認証に関するアプリケーションロジックを担います。
package usecase

import (
	"context"
	"time"
)

// IDTokenClaims は検証済みの ID トークンから取り出した値です。
//
// 署名・iss・aud・exp・nonce の検証は Provider の実装側で済んでいる前提です。
// ここまで来た値は「Google が本人だと言っている」ものとして扱えます。
type IDTokenClaims struct {
	// Subject は Google の一意識別子 (sub) です。照合はこれで行います。
	// メールアドレスは変更されうるため使いません
	// (docs/adr/0005-authentication.md 決定 3)。
	Subject string
	// Email と EmailVerified。未検証のアドレスは受け付けません。
	Email         string
	EmailVerified bool
	Name          string
	// Picture は未設定のことがあります。
	Picture string
}

// AuthRequest はログイン開始時に生成し、コールバックまで持ち回る値です。
//
// 3 つとも短命な Cookie に入れてブラウザに預けます。サーバ側に保存しないのは、
// 保存するとログイン開始のたびに行が増え、掃除が要るためです。
// 改竄されても困らない設計にしてあります —— state は Cookie 側と
// クエリ側の一致だけを見るので、両方を書き換えられても
// 「攻撃者自身のログインが成立する」だけで、他人のセッションは作れません。
type AuthRequest struct {
	State        string
	Nonce        string
	CodeVerifier string
	// AuthURL はブラウザをリダイレクトさせる先です。
	AuthURL string
}

// Provider は OpenID Connect プロバイダ (Google) との対話を抽象化します。
//
// インターフェースにしているのは、**テストで外部 IdP を叩かないため**です
// (docs/adr/0005-authentication.md の引き受けるコスト)。
// 実装は infrastructure 側に置き、テストではフェイクを差し込みます。
type Provider interface {
	// AuthCodeURL は認可エンドポイントへの URL を組み立てます。
	// PKCE の code_challenge は codeVerifier から実装側で導出します。
	AuthCodeURL(state, nonce, codeVerifier string) string

	// Exchange は認可コードをトークンへ交換し、ID トークンを検証して返します。
	//
	// 署名・iss・aud・exp の検証に加え、nonce が一致することを確認します。
	// nonce を確認しないと、別のセッション向けに発行された ID トークンを
	// 差し込まれても気づけません。
	Exchange(ctx context.Context, code, codeVerifier, nonce string) (*IDTokenClaims, error)
}

// Clock は現在時刻を返します。テストで時刻を固定するために差し替えます。
type Clock func() time.Time
