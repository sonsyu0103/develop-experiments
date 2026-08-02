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
	Debug bool
}

// Load は環境変数から設定を読み取ります。
// 必須の環境変数が欠けている場合はエラーを返します。
func Load() (*Config, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("config: DATABASE_URL は必須です")
	}

	maxConns, err := int32Env("DB_MAX_CONNS", 20)
	if err != nil {
		return nil, err
	}
	minConns, err := int32Env("DB_MIN_CONNS", 2)
	if err != nil {
		return nil, err
	}
	if minConns > maxConns {
		return nil, fmt.Errorf("config: DB_MIN_CONNS (%d) が DB_MAX_CONNS (%d) を超えています", minConns, maxConns)
	}

	shutdownSec, err := intEnv("SHUTDOWN_TIMEOUT_SECONDS", 10)
	if err != nil {
		return nil, err
	}

	return &Config{
		Addr:            stringEnv("ADDR", ":8080"),
		DatabaseURL:     dsn,
		MaxConns:        maxConns,
		MinConns:        minConns,
		ShutdownTimeout: time.Duration(shutdownSec) * time.Second,
		AllowedOrigins:  csvEnv("CORS_ALLOWED_ORIGINS", []string{"http://localhost:3000"}),
		Debug:           stringEnv("ENV", "development") == "development",
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

func intEnv(key string, fallback int) (int, error) {
	v, err := parseIntEnv(key, int64(fallback), 64)
	return int(v), err
}

// int32Env は int32 に収まることを保証して読み取ります。
// pgxpool の設定値が int32 なので、ここで範囲を確定させておくと
// 呼び出し側で範囲外を気にする必要がなくなります。
func int32Env(key string, fallback int32) (int32, error) {
	v, err := parseIntEnv(key, int64(fallback), 32)
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

// parseIntEnv は環境変数を正の整数として読み取ります。
// bitSize は strconv.ParseInt に渡す値で、これにより桁あふれを防ぎます。
func parseIntEnv(key string, fallback int64, bitSize int) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, bitSize)
	if err != nil {
		return 0, fmt.Errorf("config: %s は %d ビット整数である必要があります: %w", key, bitSize, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("config: %s は正の整数である必要があります (got %d)", key, v)
	}
	return v, nil
}
