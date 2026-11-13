// Command api は go-service の HTTP サーバ。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"example.com/go-service/internal/httpapi"
)

// shutdownTimeout は SIGTERM を受けてから処理中のリクエストを待つ上限。
// k8s の terminationGracePeriodSeconds (既定 30 秒) より短くしておく。
// 長いと、待っている途中で SIGKILL され、レスポンスが途中で切れる。
const shutdownTimeout = 20 * time.Second

func main() {
	// ログは JSON で標準出力へ。集めるのは基盤 (fluent-bit など) の仕事。
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("startup_failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	gin.SetMode(gin.ReleaseMode)

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	h, err := httpapi.NewHandler()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: h,
		// 遅いクライアントがヘッダを送りきらずに接続を握り続ける攻撃 (Slowloris) を切る。
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", slog.String("addr", addr))
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// **SIGTERM で即終了しない。** ローリング更新のたびに処理中のリクエストが落ちる。
	slog.Info("shutting_down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
