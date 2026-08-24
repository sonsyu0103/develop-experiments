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

		// naive は並行投稿が一意制約に弾かれて失われる。本番相当の設定では選ばせない。
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

// ログの出力形式 (docs/adr/0010-log-pipeline.md)。
//
// **形式は Debug から独立している**ことがここの主題です。
// 束ねていたせいで、ENV=development のときだけ logfmt になり、
// fluent-bit のパーサが 1 行も展開できませんでした。
// 「開発環境で、DEBUG レベルのまま JSON を出せる」ことを守ります。
func TestLoad_LogFormat(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		devMode bool
		want    LogFormat
		wantErr bool
	}{
		{name: "未設定かつ本番相当なら json", env: "", want: LogFormatJSON},
		{name: "未設定かつ開発なら text (従来の見え方を変えない)", env: "", devMode: true, want: LogFormatText},

		// ここが本題。開発モードのまま JSON を選べる。
		{name: "開発モードでも json を選べる", env: "json", devMode: true, want: LogFormatJSON},
		{name: "本番相当でも text を選べる", env: "text", want: LogFormatText},
		{name: "大文字と空白を許す", env: "  JSON ", want: LogFormatJSON},

		{name: "未知の値は起動時に落とす", env: "logfmt", wantErr: true},
		{name: "空白だけも落とす", env: "   ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
			t.Setenv("LOG_FORMAT", tt.env)
			if tt.devMode {
				t.Setenv("ENV", "development")
			} else {
				t.Setenv("ENV", "")
			}

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("LOG_FORMAT=%q で Load が成功した (format=%q)", tt.env, cfg.LogFormat)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load が失敗した: %v", err)
			}
			if cfg.LogFormat != tt.want {
				t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, tt.want)
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

// ---------------------------------------------------------------------------
// 問い合わせ (docs/adr/0008-contact-and-mail.md)
// ---------------------------------------------------------------------------

// loadWithMinimalEnv は必須の環境変数だけを与えて Load を呼びます。
func loadWithMinimalEnv(t *testing.T) *Config {
	t.Helper()

	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load が失敗した: %v", err)
	}
	return cfg
}

// **メールの設定が無くても起動すること** (ADR 0008 決定 1)。
//
// 揃っていない場合に止まるのは送信ワーカーだけで、受付は動きます。
// 503 で断ると、その間に来た問い合わせがそのまま失われます ——
// 画像やログインと違い、利用者はもう一度送りに来てくれません。
func TestLoad_MailIsOptional(t *testing.T) {
	cfg := loadWithMinimalEnv(t)
	if cfg.Contact.Mail.Enabled() {
		t.Error("設定が無いのに有効と判定された")
	}
}

// **必要な値に既定を持たせないこと** (AuthConfig.RedirectURL と同じ理由)。
//
// 既定を入れると、設定を忘れた本番が Enabled() を満たし、
// どこにも届かないメールを送り続けます。
func TestMailConfig_Enabled(t *testing.T) {
	full := MailConfig{Host: "mailpit", Port: 1025, From: "a@example.com", To: "b@example.com"}
	if !full.Enabled() {
		t.Fatal("揃っているのに無効と判定された")
	}

	for name, mutate := range map[string]func(*MailConfig){
		"host なし": func(m *MailConfig) { m.Host = "" },
		"port なし": func(m *MailConfig) { m.Port = 0 },
		"from なし": func(m *MailConfig) { m.From = "" },
		"to なし":   func(m *MailConfig) { m.To = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := full
			mutate(&cfg)
			if cfg.Enabled() {
				t.Error("欠けているのに有効と判定された")
			}
		})
	}

	// **資格情報は判定に含めない。** Mailpit は認証を要求しないので、
	// 必須にすると手元で経路を確認できなくなります。
	noCreds := full
	noCreds.Username = ""
	noCreds.Password = ""
	if !noCreds.Enabled() {
		t.Error("資格情報が無いだけで無効と判定された")
	}
}

// **レート制限を実質無効にする設定を作れないこと** (ADR 0008 決定 4)。
//
//	窓 0 秒   数える対象が常に空になり、実質無制限
//	上限 0 件 誰も送れない
//
// どちらも「無効化したつもりはないのに無効」という形で静かに壊れます。
func TestLoad_ContactRateLimitRejectsZero(t *testing.T) {
	for _, key := range []string{"CONTACT_RATE_LIMIT_WINDOW_SECONDS", "CONTACT_RATE_LIMIT_MAX"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
			t.Setenv(key, "0")
			if _, err := Load(); err == nil {
				t.Errorf("%s=0 が通ってしまった", key)
			}
		})
	}
}

// **送信間隔に 0 を許さないこと** (VIEW_COUNT_FLUSH_SECONDS と同じ理由)。
//
// 0 だとスケジューラが既定値 (10 分) へ丸めるので、
// 「速くするつもりで 0 にしたら、いちばん遅くなった」が起きます。
func TestLoad_ContactDispatchIntervalRejectsZero(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
	t.Setenv("CONTACT_DISPATCH_INTERVAL_SECONDS", "0")
	if _, err := Load(); err == nil {
		t.Error("CONTACT_DISPATCH_INTERVAL_SECONDS=0 が通ってしまった")
	}
}

// **既定値が ADR と揃っていること。**
//
// client_ip の保持期間はログの保持期間 (ADR 0010 決定 6 の 400 日) と揃えます。
// 同じ IP がログ側にも出ているので、片方だけ短くしても消したことになりません。
func TestLoad_ContactDefaults(t *testing.T) {
	cfg := loadWithMinimalEnv(t)

	if got := cfg.Contact.IPRetention.Hours() / 24; got != 400 {
		t.Errorf("IPRetention = %v 日, want 400 日", got)
	}
	if cfg.Contact.RateLimitMax != 5 || cfg.Contact.RateLimitWindow.Hours() != 1 {
		t.Errorf("レート制限の既定 = %d 件 / %v", cfg.Contact.RateLimitMax, cfg.Contact.RateLimitWindow)
	}
}

// **接続プールの既定は実測で決めた値であること** (ADR 0009 の「実測」3-2)。
//
// 20 から 8 に下げてある。20 は測らずに置いた数字で、
// 実測すると 8 以降はスループットが伸びず、
// **遅い側の応答だけが悪化していた** (並列 64 の p95 が 36ms → 122ms)。
//
// ここで固定しているのは「8 が唯一の正解だから」ではありません。
// 適正値は DB のコア数に比例し、アプリからは見えないので、
// **どの数字も推測になります。** 検査があると、次に変えるときに
// 「また測らずに戻す」ができなくなります —— それがこのテストの役目です。
func TestLoad_PoolDefaults(t *testing.T) {
	cfg := loadWithMinimalEnv(t)

	if cfg.MaxConns != 8 {
		t.Errorf("MaxConns の既定 = %d, want 8 "+
			"(変えるなら make scale-probe で測り直し、ADR 0009 を更新すること)",
			cfg.MaxConns)
	}
	if cfg.MinConns != 2 {
		t.Errorf("MinConns の既定 = %d, want 2", cfg.MinConns)
	}
	if cfg.MinConns > cfg.MaxConns {
		t.Errorf("MinConns (%d) が MaxConns (%d) を超えている", cfg.MinConns, cfg.MaxConns)
	}
}

// 上書きが実際に効くこと。**compose から振れることが測定の前提**になります
// (make scale-probe の 5 番が DB_MAX_CONNS を差し替えて計測します)。
func TestLoad_PoolOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
	t.Setenv("DB_MAX_CONNS", "32")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load が失敗した: %v", err)
	}
	if cfg.MaxConns != 32 {
		t.Errorf("MaxConns = %d, want 32 (DB_MAX_CONNS が効いていない)", cfg.MaxConns)
	}
}

// 閲覧数の集計モード (docs/adr/0006-view-count-and-popularity.md)。
//
// **COMMENT_POST_MODE と同じ形なのに、検査が 1 本も無かった** (レビュー指摘)。
// sync は Phase 4 の比較専用で、本番相当で選ぶと
// **閲覧というまったく無関係な操作がコメント投稿の直列化失敗率を押し上げます。**
// それを止めているのがこの検査になります。
func TestLoad_ViewCountMode(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		devMode bool
		want    ViewCountMode
		wantErr bool
	}{
		{name: "未設定なら buffered", env: "", want: ViewCountModeBuffered},
		{name: "buffered", env: "buffered", want: ViewCountModeBuffered},
		{name: "大文字と空白を許す", env: "  BUFFERED ", want: ViewCountModeBuffered},

		{name: "sync は開発モードでのみ選べる", env: "sync", devMode: true, want: ViewCountModeSync},
		{name: "sync は本番相当だと起動しない", env: "sync", wantErr: true},

		// **未知の値を既定へ落とさない** (COMMENT_POST_MODE と同じ理由)。
		{name: "未知の値は起動時に落とす", env: "async", wantErr: true},
		{name: "空白だけも落とす", env: "   ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
			t.Setenv("VIEW_COUNT_MODE", tt.env)
			if tt.devMode {
				t.Setenv("ENV", "development")
			} else {
				t.Setenv("ENV", "")
			}

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("VIEW_COUNT_MODE=%q で Load が成功した (mode=%q)", tt.env, cfg.ViewCount.Mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load が失敗した: %v", err)
			}
			if cfg.ViewCount.Mode != tt.want {
				t.Errorf("ViewCount.Mode = %q, want %q", cfg.ViewCount.Mode, tt.want)
			}
		})
	}
}

// **数値の設定は、下限と上限の両方で落ちること。**
//
// 上限が無かったころは、秒数を time.Duration に掛ける側で int64 があふれた。
// SHUTDOWN_TIMEOUT_SECONDS=10000000000 は下限 (0 以上) を通り、
// `time.Duration(v) * time.Second` が**負に折り返して**
// context.WithTimeout が最初から期限切れになる ——
// **長い猶予を設定した運用者に、猶予ゼロが返る。**
//
// MAIL_SMTP_PORT=0 も同じ形。検証は通るのに Enabled() が false になり、
// **メール経路だけが静かに止まる** (contact の行は溜まり続ける)。
func TestLoad_NumericBounds(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{name: "猶予の上限を超える", key: "SHUTDOWN_TIMEOUT_SECONDS", value: "10000000000", wantErr: true},
		{name: "猶予の上限ちょうどは通る", key: "SHUTDOWN_TIMEOUT_SECONDS", value: "3600"},
		{name: "SMTP ポートに 0 は使えない", key: "MAIL_SMTP_PORT", value: "0", wantErr: true},
		{name: "SMTP ポートの範囲外", key: "MAIL_SMTP_PORT", value: "70000", wantErr: true},
		{name: "SMTP ポートの上限ちょうどは通る", key: "MAIL_SMTP_PORT", value: "65535"},
		{name: "フラッシュ間隔の上限を超える", key: "VIEW_COUNT_FLUSH_SECONDS", value: "100000", wantErr: true},
		{name: "IP の保持日数の上限を超える", key: "CONTACT_IP_RETENTION_DAYS", value: "100000", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			if tt.wantErr && err == nil {
				t.Fatalf("%s=%s で Load が成功した", tt.key, tt.value)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("%s=%s で Load が失敗した: %v", tt.key, tt.value, err)
			}
		})
	}
}

// **ENV の前後の空白を落とすこと。**
//
// このファイルの他の値はすべて TrimSpace している (".env から貼り付けたときの
// 末尾空白" が理由) のに、ENV だけ抜けていた。`ENV=development ` だと
// debug=false になり、**SecureCookie が true になってブラウザが
// http://localhost の Cookie を捨てる** —— サーバ側にはエラーが出ない。
func TestLoad_EnvIsTrimmed(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
	t.Setenv("ENV", "development ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load が失敗した: %v", err)
	}
	if !cfg.Debug {
		t.Error("末尾の空白で開発モードにならなかった (SecureCookie が本番扱いになる)")
	}
}

// **設定されているのに空、を既定へ戻さないこと。**
//
// `CORS_ALLOWED_ORIGINS=","` は「クロスオリジンを許さない」の自然な書き方。
// 既定へ落とすと、**本番で開発用の localhost:3000 が復活する**うえ、
// 設定した値が捨てられたことはどこにも出ない。
func TestLoad_EmptyAllowedOriginsStayEmpty(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://app:password@localhost:5432/bbs")
	t.Setenv("CORS_ALLOWED_ORIGINS", " , ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load が失敗した: %v", err)
	}
	if len(cfg.AllowedOrigins) != 0 {
		t.Errorf("AllowedOrigins = %v, want 空 (既定が復活している)", cfg.AllowedOrigins)
	}
}
