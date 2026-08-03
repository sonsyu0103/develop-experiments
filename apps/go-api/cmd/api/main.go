// Command api は掲示板 API の HTTP サーバです。
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

	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/httpapi"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
)

func main() {
	if err := run(); err != nil {
		slog.Error("起動に失敗しました", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	setupLogger(cfg.Debug)
	if cfg.Debug {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// SIGINT / SIGTERM を受けたら ctx がキャンセルされる。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// スレッドのリポジトリは、コメント側の親スレッド存在確認にも使い回す。
	threadRepo := postgres.NewThreadRepository(pool)

	server := httpapi.NewServer(
		threadusecase.NewThreadInteractor(threadRepo),
		commentusecase.NewCommentInteractor(postgres.NewCommentRepository(pool), threadRepo),
		pool,
	)

	router, err := httpapi.NewRouter(httpapi.Deps{
		Server:         server,
		AllowedOrigins: cfg.AllowedOrigins,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: router,
		// ヘッダを小出しに送り続けて接続を占有する Slowloris 攻撃への対策。
		ReadHeaderTimeout: 10 * time.Second,
	}

	// サーバは別 goroutine で動かし、メインでシグナルを待つ。
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("サーバを起動しました", slog.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		return err

	case <-ctx.Done():
		slog.Info("シャットダウンを開始します")

		// 処理中のリクエストを取りこぼさないよう、猶予を与えて終了する。
		// この ctx は親から切り離す (親はすでにキャンセル済みのため)。
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		slog.Info("シャットダウンが完了しました")
		return nil
	}
}

// setupLogger は slog のデフォルトロガーを設定します。
// 本番は JSON、開発は人間が読みやすいテキスト形式にします。
func setupLogger(debug bool) {
	level := slog.LevelInfo
	var handler slog.Handler

	if debug {
		level = slog.LevelDebug
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}

	slog.SetDefault(slog.New(handler))
}
