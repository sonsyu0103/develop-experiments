package config

import "testing"

// 認証の設定は「揃っていなければ無効」であって、起動を妨げてはいけません。
//
// 必須にすると、Google の資格情報を置くまで CI が落ちます
// (Migration Check は API サーバを起動してスモークテストを回すため)。
// 掲示板の閲覧と匿名投稿は認証に依存しないので、
// 認証だけを 503 にするほうが影響が小さくなります。
func TestAuthConfig_Enabled(t *testing.T) {
	t.Parallel()

	full := AuthConfig{
		GoogleClientID:     "id",
		GoogleClientSecret: "secret",
		RedirectURL:        "http://localhost:8080/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	}

	if !full.Enabled() {
		t.Fatal("すべて揃っているのに無効と判定された")
	}

	// 1 つでも欠けたら無効。半端な設定で起動すると、
	// 「ログインは始まるがコールバックで失敗する」形になる。
	tests := map[string]func(*AuthConfig){
		"client_id が空":     func(a *AuthConfig) { a.GoogleClientID = "" },
		"client_secret が空": func(a *AuthConfig) { a.GoogleClientSecret = "" },
		"redirect_url が空":  func(a *AuthConfig) { a.RedirectURL = "" },
		"frontend_url が空":  func(a *AuthConfig) { a.FrontendURL = "" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			a := full
			mutate(&a)
			if a.Enabled() {
				t.Errorf("%s なのに有効と判定された", name)
			}
		})
	}
}

// Secure 属性は「開発時だけ外す」。
// 未設定の環境が Secure なしで動くほうが危険なので、既定は付ける側。
func TestLoad_SecureCookieDefaultsToOn(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")

	t.Run("ENV 未設定なら Secure を付ける", func(t *testing.T) {
		t.Setenv("ENV", "")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load が失敗した: %v", err)
		}
		if !cfg.Auth.SecureCookie {
			t.Error("SecureCookie = false, want true (本番扱い)")
		}
	})

	t.Run("ENV=development なら外す", func(t *testing.T) {
		t.Setenv("ENV", "development")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load が失敗した: %v", err)
		}
		if cfg.Auth.SecureCookie {
			t.Error("SecureCookie = true, want false (localhost は HTTP)")
		}
	})
}

// 資格情報が無くても Load は成功する必要があります。
func TestLoad_SucceedsWithoutGoogleCredentials(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("GOOGLE_CLIENT_SECRET", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("資格情報が無いだけで Load が失敗した: %v", err)
	}
	if cfg.Auth.Enabled() {
		t.Error("資格情報が無いのに認証が有効と判定された")
	}
}
