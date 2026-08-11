// Package config はアプリケーション起動時の設定を環境変数から読み取ります。
package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config はアプリケーション全体の設定です。
type Config struct {
	// Addr は HTTP サーバの待ち受けアドレスです。
	Addr string
	// DatabaseURL は PostgreSQL への接続文字列 (postgres://...) です。
	DatabaseURL string
	// MaxConns は接続プールの最大接続数です。
	MaxConns int32
	// MinConns はアイドル時に維持する最小接続数です。
	MinConns int32
	// ShutdownTimeout はグレースフルシャットダウンの猶予時間です。
	ShutdownTimeout time.Duration
	// AllowedOrigins は CORS で許可するオリジンの一覧です。
	AllowedOrigins []string
	// Debug は開発モードかどうかです。gin のモード切り替えに使います。
	//
	// ENV=development のときだけ true になります。
	// 未設定なら false (本番扱い) です。設定を忘れた環境が
	// 気づかないうちにデバッグモードで動くほうが危険なため、
	// 安全側に倒しています。
	Debug bool
	// Auth は Google OIDC の設定です。
	Auth AuthConfig
}

// AuthConfig は Google OIDC による認証の設定です。
//
// **すべて任意です。** 揃っていない場合、API は起動しますが
// 認証エンドポイントだけが 503 を返します。
//
// 必須にしない理由:
//   - 掲示板の閲覧と匿名投稿は認証に依存しない。認証の設定が無いだけで
//     API 全体が起動しないのは害のほうが大きい
//   - CI の Migration Check は API サーバを起動してスモークテストを回す。
//     必須にすると、Google の資格情報を CI に置くまで CI が落ちる
type AuthConfig struct {
	// GoogleClientID / GoogleClientSecret は Google Cloud で発行する資格情報です。
	GoogleClientID     string
	GoogleClientSecret string
	// RedirectURL は Google からのコールバック先です。
	// Google Cloud 側の「承認済みのリダイレクト URI」と一致している必要があります。
	//
	// **既定値を持たせません。** localhost を既定にすると、本番で
	// AUTH_REDIRECT_URL を入れ忘れても Enabled() が true になり、
	// Google に redirect_uri=http://localhost:8080/... を送って
	// redirect_uri_mismatch で初めて気づくことになります。
	// 未設定なら認証ごと無効 (503) にするほうが、原因が分かりやすくなります。
	RedirectURL string
	// FrontendURL はログイン完了後に戻す先です。RedirectURL と同じ理由で
	// 既定値を持たせません。
	FrontendURL string
	// SecureCookie は Cookie に Secure 属性を付けるかどうかです。
	//
	// localhost は HTTP なので開発時は付けられません。
	// ENV=development 以外では既定で true になります —— 設定を忘れた本番が
	// Secure なしで動くほうが危険なため、安全側に倒しています。
	SecureCookie bool
}

// Enabled は認証を有効にできるだけの設定が揃っているかを返します。
func (a AuthConfig) Enabled() bool {
	return a.GoogleClientID != "" && a.GoogleClientSecret != "" &&
		a.RedirectURL != "" && a.FrontendURL != ""
}

// Load は環境変数から設定を読み取ります。
// 必須の環境変数が欠けている場合はエラーを返します。
func Load() (*Config, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("config: DATABASE_URL は必須です")
	}

	// 接続プールの上限は 1 以上でなければ意味をなさない。
	maxConns, err := int32Env("DB_MAX_CONNS", 20, 1)
	if err != nil {
		return nil, err
	}
	// 下限の 0 は「アイドル接続を事前に張らない」という正当な設定
	// (pgxpool の既定値でもある) なので許可する。
	minConns, err := int32Env("DB_MIN_CONNS", 2, 0)
	if err != nil {
		return nil, err
	}
	if minConns > maxConns {
		return nil, fmt.Errorf("config: DB_MIN_CONNS (%d) が DB_MAX_CONNS (%d) を超えています", minConns, maxConns)
	}

	// 0 は「猶予を設けず即座に終了する」という正当な設定なので許可する。
	shutdownSec, err := intEnv("SHUTDOWN_TIMEOUT_SECONDS", 10, 0)
	if err != nil {
		return nil, err
	}

	debug := strings.EqualFold(os.Getenv("ENV"), "development")

	return &Config{
		Addr:            stringEnv("ADDR", ":8080"),
		DatabaseURL:     dsn,
		MaxConns:        maxConns,
		MinConns:        minConns,
		ShutdownTimeout: time.Duration(shutdownSec) * time.Second,
		AllowedOrigins:  csvEnv("CORS_ALLOWED_ORIGINS", []string{"http://localhost:3000"}),
		Debug:           debug,
		Auth: AuthConfig{
			GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			// 既定値を入れない (AuthConfig のコメントを参照)。
			// compose.yaml と apps/go-api/.env.example が明示的に渡す。
			RedirectURL: os.Getenv("AUTH_REDIRECT_URL"),
			FrontendURL: os.Getenv("AUTH_FRONTEND_URL"),
			// 開発時だけ Secure を外す。未設定の環境は本番扱いで付ける。
			SecureCookie: !debug,
		},
	}, nil
}

// csvEnv はカンマ区切りの環境変数を文字列スライスとして読み取ります。
func csvEnv(key string, fallback []string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func stringEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback, minimum int) (int, error) {
	v, err := parseIntEnv(key, int64(fallback), int64(minimum), 64)
	return int(v), err
}

// int32Env は int32 に収まることを保証して読み取ります。
// pgxpool の設定値が int32 なので、ここで範囲を確定させておくと
// 呼び出し側で範囲外を気にする必要がなくなります。
func int32Env(key string, fallback, minimum int32) (int32, error) {
	v, err := parseIntEnv(key, int64(fallback), int64(minimum), 32)
	if err != nil {
		return 0, err
	}
	// bitSize=32 で解析しているので到達しないはずだが、
	// 変換の安全性がコード上で自明になるよう明示的に検査する。
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("config: %s が int32 の範囲を超えています (got %d)", key, v)
	}
	return int32(v), nil
}

// parseIntEnv は環境変数を整数として読み取り、minimum 以上であることを確認します。
// bitSize は strconv.ParseInt に渡す値で、これにより桁あふれを防ぎます。
func parseIntEnv(key string, fallback, minimum int64, bitSize int) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, bitSize)
	if err != nil {
		return 0, fmt.Errorf("config: %s は %d ビット整数である必要があります: %w", key, bitSize, err)
	}
	if v < minimum {
		return 0, fmt.Errorf("config: %s は %d 以上である必要があります (got %d)", key, minimum, v)
	}
	return v, nil
}
