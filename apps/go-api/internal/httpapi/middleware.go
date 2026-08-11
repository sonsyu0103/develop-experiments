package httpapi

import (
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
)

// cors は許可オリジンからのクロスオリジン要求を通すミドルウェアです。
//
// Next.js の Server Components はサーバ側から fetch するため CORS は不要ですが、
// コメント投稿のようにブラウザから直接叩く経路では必要になります。
//
// ワイルドカード (*) を使わず許可リスト方式にしているのは、
// Cookie 認証で必要な Access-Control-Allow-Credentials と
// 併用できるようにするためです (* との併用はブラウザが拒否します)。
func cors(allowedOrigins []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()

		// Vary は「許可したときだけ」ではなく、このミドルウェアが触る
		// 全レスポンスに付ける必要がある。
		// 許可ヘッダの無い応答に Vary が無いと、共有キャッシュ / CDN が
		// それをオリジン非依存として保存し、許可オリジンからの
		// リクエストにも使い回してしまう。
		// ブラウザ側では CORS ヘッダ欠落として弾かれ、断続的に失敗する。
		h.Add("Vary", "Origin")

		origin := c.GetHeader("Origin")
		if origin != "" && slices.Contains(allowedOrigins, origin) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			// セッションは Cookie で運ぶ (ADR 0005 決定 1)。
			// これが無いと、ブラウザは credentials 付きの要求に対する
			// 応答を JavaScript へ渡さない。Cookie は送られるので
			// サーバ側のログは正常に見え、フロントだけが失敗する。
			h.Set("Access-Control-Allow-Credentials", "true")
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// requestLogger は 1 リクエストにつき 1 行の構造化ログを出します。
// gin 標準の Logger は平文なので、slog による JSON 出力に置き換えています。
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		level := slog.LevelInfo
		if c.Writer.Status() >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		slog.Log(c.Request.Context(), level, "http_request",
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()),
			slog.Duration("latency", time.Since(start)),
		)
	}
}
