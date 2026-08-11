// Package oidc は Google の OpenID Connect プロバイダとの対話を実装します。
//
// 検証を自前で書かないのは、JWKS の取得・キャッシュ・鍵ローテーション・
// 署名検証・iss/aud/exp の照合のどれか 1 つを落としても静かに脆弱になるためです
// (docs/adr/0005-authentication.md の引き受けるコスト)。
package oidc

import (
	"context"
	"fmt"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/user/usecase"
)

// googleIssuer は Google の OIDC ディスカバリの起点です。
const googleIssuer = "https://accounts.google.com"

// GoogleProvider は usecase.Provider の Google 実装です。
type GoogleProvider struct {
	verifier *coreoidc.IDTokenVerifier
	oauth    *oauth2.Config
}

var _ usecase.Provider = (*GoogleProvider)(nil)

// NewGoogle はディスカバリを実行してプロバイダを組み立てます。
//
// **この関数はネットワークを叩きます。** 起動時に呼ぶため、
// Google へ到達できない環境では起動に失敗します。
// 認証の設定が入っている場合にだけ呼ぶ設計にしてあり
// (cmd/api の cfg.Auth.Enabled())、設定が無い環境は影響を受けません。
func NewGoogle(ctx context.Context, cfg config.AuthConfig) (*GoogleProvider, error) {
	provider, err := coreoidc.NewProvider(ctx, googleIssuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC ディスカバリに失敗しました: %w", err)
	}

	return &GoogleProvider{
		// aud が自分のクライアント ID であることを検証する。
		// これが無いと、他のアプリ向けに発行された ID トークンを受け入れてしまう。
		verifier: provider.Verifier(&coreoidc.Config{ClientID: cfg.GoogleClientID}),
		oauth: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.RedirectURL,
			// ディスカバリで得た値をそのまま使う。
			// golang.org/x/oauth2/google の定数を使うと、
			// cloud.google.com/go/compute/metadata まで依存に入る
			// (GCE のメタデータサーバから資格情報を拾う経路のため)。
			// ここでは要らない。
			Endpoint: provider.Endpoint(),
			// 掲示板に必要なのは本人確認と表示名だけ。
			// 余計なスコープを求めると、同意画面で断られる理由が増える。
			Scopes: []string{coreoidc.ScopeOpenID, "email", "profile"},
		},
	}, nil
}

// AuthCodeURL は認可エンドポイントへの URL を組み立てます。
//
// PKCE の code_challenge は code_verifier から S256 で導出します
// (平文の plain は使わない)。
func (p *GoogleProvider) AuthCodeURL(state, nonce, codeVerifier string) string {
	return p.oauth.AuthCodeURL(state,
		coreoidc.Nonce(nonce),
		oauth2.S256ChallengeOption(codeVerifier),
	)
}

// Exchange は認可コードをトークンへ交換し、ID トークンを検証します。
func (p *GoogleProvider) Exchange(
	ctx context.Context, code, codeVerifier, nonce string,
) (*usecase.IDTokenClaims, error) {
	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return nil, fmt.Errorf("認可コードの交換に失敗しました: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("ID トークンが含まれていません")
	}

	// 署名・iss・aud・exp をここで検証する。
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("ID トークンの検証に失敗しました: %w", err)
	}

	// nonce の照合。ライブラリは検証してくれないので自分で確認する。
	//
	// これが無いと、別のログイン試行向けに発行された ID トークンを
	// 差し込まれても気づけません。
	if idToken.Nonce != nonce {
		return nil, fmt.Errorf("nonce が一致しません")
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("ID トークンの読み取りに失敗しました: %w", err)
	}

	return &usecase.IDTokenClaims{
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Name:          claims.Name,
		Picture:       claims.Picture,
	}, nil
}
