// Command api は掲示板 API の HTTP サーバです。
package main

import (
	"context"
	"errors"
	"fmt"
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
	imageusecase "develop-experiments/apps/go-api/internal/image/usecase"
	"develop-experiments/apps/go-api/internal/infrastructure/objectstorage"
	oidcprovider "develop-experiments/apps/go-api/internal/infrastructure/oidc"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres"
	"develop-experiments/apps/go-api/internal/logging"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
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

	logging.Setup(cfg.Debug)
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

	sessionRepo := postgres.NewSessionRepository(pool)

	// **セッションの検証は常に結線する。** sessions を引いて期限を見るだけで、
	// Google を必要としない (ADR 0005 決定 4)。
	// ここを認証の設定で分岐させていた頃は、資格情報の無い環境が
	// Cookie を無視して全リクエストを匿名として扱っていた。
	sessionInteractor := userusecase.NewSessionInteractor(sessionRepo)

	// **ログインだけが設定を要する。** 認可コードの交換に IdP が要るため。
	//
	// 揃っていなくても API は起動する。掲示板の閲覧と匿名投稿は認証に依存せず、
	// 設定漏れで全体が落ちるほうが害が大きい (config.AuthConfig を参照)。
	var loginInteractor *userusecase.LoginInteractor
	if cfg.Auth.Enabled() {
		provider, providerErr := oidcprovider.NewGoogle(ctx, cfg.Auth)
		if providerErr != nil {
			return fmt.Errorf("OIDC プロバイダの初期化に失敗しました: %w", providerErr)
		}
		loginInteractor = userusecase.NewLoginInteractor(
			postgres.NewUserRepository(pool),
			sessionRepo,
			provider,
			nil,
		).WithBootstrapAdmin(cfg.Auth.BootstrapAdminGoogleSub)
		slog.Info("ログインを有効にしました",
			slog.Bool("bootstrap_admin", cfg.Auth.BootstrapAdminGoogleSub != ""))
	} else {
		slog.Warn("認証の設定が無いため、/auth/google の 2 経路は 503 を返します " +
			"(発行済みセッションの検証・/me・ログアウトは動きます)")
	}

	// 画像はストレージの設定が揃っているときだけ有効にする。
	//
	// 認証と同じ形 (ADR 0005 決定 4)。揃っていなくても API は起動し、
	// **画像の経路だけが 503** になる。掲示板の閲覧・投稿・ログインは
	// 画像に依存しない。
	var imageInteractor *imageusecase.ImageInteractor
	if cfg.Storage.Enabled() {
		storage, storageErr := objectstorage.New(ctx, cfg.Storage)
		if storageErr != nil {
			return fmt.Errorf("オブジェクトストレージの初期化に失敗しました: %w", storageErr)
		}
		imageInteractor = imageusecase.NewImageInteractor(
			postgres.NewImageRepository(pool), storage, nil)
		slog.Info("画像アップロードを有効にしました",
			slog.String("bucket", cfg.Storage.Bucket))
	} else {
		slog.Warn("ストレージの設定が無いため、POST /images は 503 を返します")
	}

	// **nil のポインタをインターフェースへ入れない。**
	//
	// commentusecase.ImageResolver に (*imageusecase.ImageInteractor)(nil) を
	// 代入すると、**インターフェース値としては非 nil** になる
	// (型情報を持つため)。受け取った側の `if i.images == nil` は偽になり、
	// 設定が無い環境で画像を指定した投稿が 503 ではなく nil 参照で落ちる。
	//
	// Go でよく踏む形なので、代入をここで明示的に分岐させる。
	var imageResolver commentusecase.ImageResolver
	if imageInteractor != nil {
		imageResolver = imageInteractor
	}

	server := httpapi.NewServer(
		threadusecase.NewThreadInteractor(threadRepo),
		commentusecase.NewCommentInteractor(
			postgres.NewCommentRepository(pool, cfg.CommentPostMode), threadRepo, imageResolver),
		pool,
		sessionInteractor,
		loginInteractor,
		imageInteractor,
		cfg.Auth,
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
