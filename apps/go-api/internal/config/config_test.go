package config

import "testing"

// authEnv は Load に渡す認証まわりの環境変数です。
// 空文字にしたものが「未設定」を表します。
type authEnv struct {
	clientID     string
	clientSecret string
	redirectURL  string
	frontendURL  string
}

func fullAuthEnv() authEnv {
	return authEnv{
		clientID:     "id",
		clientSecret: "secret",
		redirectURL:  "http://localhost:8080/auth/google/callback",
		frontendURL:  "http://localhost:3000",
	}
}

// loadWithAuthEnv は環境変数を差し替えて Load を呼びます。
//
// **構造体を直接組み立てて Enabled() を呼んではいけません。**
// Load が既定値を補っていると、構造体で「空」を作れても実運用では空にならず、
// テストは通るのに本番では検出できない、という乖離が生まれます。
// 実際、初版はこれで redirect_url / frontend_url の判定が死んでいることを
// 見逃していました。
func loadWithAuthEnv(t *testing.T, e authEnv) *Config {
	t.Helper()

	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
	t.Setenv("GOOGLE_CLIENT_ID", e.clientID)
	t.Setenv("GOOGLE_CLIENT_SECRET", e.clientSecret)
	t.Setenv("AUTH_REDIRECT_URL", e.redirectURL)
	t.Setenv("AUTH_FRONTEND_URL", e.frontendURL)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load が失敗した: %v", err)
	}
	return cfg
}

// 認証の設定は「揃っていなければ無効」であって、起動を妨げてはいけません。
//
// 必須にすると、Google の資格情報を置くまで CI が落ちます
// (Migration Check は API サーバを起動してスモークテストを回すため)。
// 掲示板の閲覧と匿名投稿は認証に依存しないので、
// 認証だけを 503 にするほうが影響が小さくなります。
func TestAuthConfig_Enabled(t *testing.T) {
	if !loadWithAuthEnv(t, fullAuthEnv()).Auth.Enabled() {
		t.Fatal("すべて揃っているのに無効と判定された")
	}

	// 1 つでも欠けたら無効。半端な設定で起動すると、
	// 「ログインは始まるがコールバックで失敗する」形になる。
	tests := map[string]func(*authEnv){
		"client_id が空":     func(e *authEnv) { e.clientID = "" },
		"client_secret が空": func(e *authEnv) { e.clientSecret = "" },
		"redirect_url が空":  func(e *authEnv) { e.redirectURL = "" },
		"frontend_url が空":  func(e *authEnv) { e.frontendURL = "" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			e := fullAuthEnv()
			mutate(&e)
			if loadWithAuthEnv(t, e).Auth.Enabled() {
				t.Errorf("%s なのに有効と判定された", name)
			}
		})
	}
}

// 環境変数を空にしたとき、Load が既定値で埋め戻さないこと。
//
// **Enabled() のテストだけでは、この乖離を検出できません。**
// 既定値が入っていても Enabled() は「揃っている」と見えるためです。
func TestLoad_AuthURLsHaveNoDefaults(t *testing.T) {
	cfg := loadWithAuthEnv(t, authEnv{clientID: "id", clientSecret: "secret"})

	if cfg.Auth.RedirectURL != "" {
		t.Errorf("RedirectURL = %q, want 空 (既定値で埋めない)", cfg.Auth.RedirectURL)
	}
	if cfg.Auth.FrontendURL != "" {
		t.Errorf("FrontendURL = %q, want 空 (既定値で埋めない)", cfg.Auth.FrontendURL)
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

// コメント投稿の並行制御モード (docs/adr/0019-comment-concurrency.md 決定 2)。
//
// **Load を通して検査します。** parseCommentPostMode を直接呼ぶと、
// 環境変数を読む経路そのものが検査から外れ、
// 「関数は正しいが Load が呼んでいない」状態を見逃します。
func TestLoad_CommentPostMode(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		devMode bool
		want    CommentPostMode
		wantErr bool
	}{
		{name: "未設定なら unique (実測で最速)", env: "", want: CommentPostModeUnique},
		{name: "ssi", env: "ssi", want: CommentPostModeSSI},
		{name: "pessimistic", env: "pessimistic", want: CommentPostModePessimistic},
		{name: "unique", env: "unique", want: CommentPostModeUnique},
		{name: "大文字と空白を許す", env: "  SSI ", want: CommentPostModeSSI},

		// naive はレス番号が重複する。本番相当の設定では選ばせない。
		{name: "naive は開発モードでのみ選べる", env: "naive", devMode: true, want: CommentPostModeNaive},
		{name: "naive は本番相当だと起動しない", env: "naive", wantErr: true},

		// **未知の値を既定値に落とさない。** 綴りを間違えたまま起動すると、
		// ベンチマークで「pessimistic を測ったつもりの ssi の値」が出る。
		{name: "未知の値は起動時に落とす", env: "serializable", wantErr: true},
		{name: "空白だけも落とす", env: "   ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
			t.Setenv("COMMENT_POST_MODE", tt.env)
			if tt.devMode {
				t.Setenv("ENV", "development")
			} else {
				t.Setenv("ENV", "")
			}

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("COMMENT_POST_MODE=%q で Load が成功した (mode=%q)", tt.env, cfg.CommentPostMode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load が失敗した: %v", err)
			}
			if cfg.CommentPostMode != tt.want {
				t.Errorf("CommentPostMode = %q, want %q", cfg.CommentPostMode, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 画像のストレージ (docs/adr/0007-image-storage.md)
// ---------------------------------------------------------------------------

// storageEnv は Load に渡すストレージまわりの環境変数です。
type storageEnv struct {
	bucket        string
	publicBaseURL string
	accessKeyID   string
	secretKey     string
	usePathStyle  string
}

func fullStorageEnv() storageEnv {
	return storageEnv{
		bucket:        "bbs-images",
		publicBaseURL: "http://localhost:9000/bbs-images",
	}
}

// loadWithStorageEnv は環境変数を差し替えて Load を呼びます。
//
// **構造体を直接組み立てて Enabled() を呼んではいけません。**
// AuthConfig と同じ理由です —— Load が既定値を補っていると、
// 構造体では「空」を作れても実運用では空にならず、
// テストは通るのに本番では検出できない乖離が生まれます。
// 認証では実際にこれで redirect_url / frontend_url の判定が死んでいました。
func loadWithStorageEnv(t *testing.T, e storageEnv) *Config {
	t.Helper()

	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
	t.Setenv("S3_BUCKET", e.bucket)
	t.Setenv("S3_PUBLIC_BASE_URL", e.publicBaseURL)
	t.Setenv("S3_ACCESS_KEY_ID", e.accessKeyID)
	t.Setenv("S3_SECRET_ACCESS_KEY", e.secretKey)
	t.Setenv("S3_USE_PATH_STYLE", e.usePathStyle)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load が失敗した: %v", err)
	}
	return cfg
}

func TestStorageConfig_Enabled(t *testing.T) {
	if !loadWithStorageEnv(t, fullStorageEnv()).Storage.Enabled() {
		t.Fatal("すべて揃っているのに無効と判定された")
	}

	tests := map[string]func(*storageEnv){
		"bucket が空":          func(e *storageEnv) { e.bucket = "" },
		"public_base_url が空": func(e *storageEnv) { e.publicBaseURL = "" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			e := fullStorageEnv()
			mutate(&e)
			if loadWithStorageEnv(t, e).Storage.Enabled() {
				t.Errorf("%s なのに有効と判定された", name)
			}
		})
	}
}

// **判定に使う値へ既定値を入れないこと。**
//
// Enabled() のテストだけでは、この乖離を検出できません
// (既定値が入っていても「揃っている」と見えるため)。
// Region にだけ既定値があるのは、SDK が空リージョンを許さないためです。
func TestLoad_StorageHasNoDefaultsForRequiredValues(t *testing.T) {
	cfg := loadWithStorageEnv(t, storageEnv{})

	if cfg.Storage.Bucket != "" {
		t.Errorf("Bucket = %q, want 空 (既定値で埋めない)", cfg.Storage.Bucket)
	}
	if cfg.Storage.PublicBaseURL != "" {
		t.Errorf("PublicBaseURL = %q, want 空 (既定値で埋めない)", cfg.Storage.PublicBaseURL)
	}
	if cfg.Storage.Region == "" {
		t.Error("Region が空。SDK が空リージョンを許さないので既定値が要る")
	}
}

// 資格情報が無くても Load は成功する必要があります。
// 本番では空にして SDK の既定チェーン (タスクロール) に任せるためです。
func TestLoad_StorageSucceedsWithoutCredentials(t *testing.T) {
	cfg := loadWithStorageEnv(t, fullStorageEnv())

	if cfg.Storage.AccessKeyID != "" || cfg.Storage.SecretAccessKey != "" {
		t.Error("資格情報が空のはずなのに値が入っている")
	}
	if !cfg.Storage.Enabled() {
		t.Error("資格情報が無いだけで無効と判定された")
	}
}

// S3_USE_PATH_STYLE の解釈。MinIO では true が要ります。
func TestLoad_StorageUsePathStyle(t *testing.T) {
	tests := map[string]bool{
		"true": true, "1": true, "TRUE": true, "yes": true, "on": true,
		"": false, "false": false, "0": false,
		// **未知の値は false に倒す。** 起動は止めない ——
		// この値は「動くかどうか」ではなくアクセス形式の選択であり、
		// 誤りは MinIO への接続失敗として即座に現れる。
		"maybe": false,
	}

	for raw, want := range tests {
		t.Run("S3_USE_PATH_STYLE="+raw, func(t *testing.T) {
			e := fullStorageEnv()
			e.usePathStyle = raw
			if got := loadWithStorageEnv(t, e).Storage.UsePathStyle; got != want {
				t.Errorf("UsePathStyle = %v, want %v", got, want)
			}
		})
	}
}
