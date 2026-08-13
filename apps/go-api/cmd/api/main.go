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
	"develop-experiments/apps/go-api/internal/scheduler"
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
	var (
		imageResolver        commentusecase.ImageResolver
		threadImageResolver  threadusecase.ImageResolver
		sessionImageResolver userusecase.ImageResolver
	)
	if imageInteractor != nil {
		imageResolver = imageInteractor
		threadImageResolver = imageInteractor
		sessionImageResolver = imageInteractor
	}

	// **セッションの検証は常に結線する。** sessions を引いて期限を見るだけで、
	// Google を必要としない (ADR 0005 決定 4)。
	// ここを認証の設定で分岐させていた頃は、資格情報の無い環境が
	// Cookie を無視して全リクエストを匿名として扱っていた。
	//
	// 画像の解決だけはストレージの設定に依存する
	// (未設定なら /me の avatarUrl は Google のものだけになる)。
	sessionInteractor := userusecase.NewSessionInteractor(sessionRepo, sessionImageResolver)

	server := httpapi.NewServer(
		threadusecase.NewThreadInteractor(threadRepo, threadImageResolver),
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

	// 定期処理を回す (ADR 0003 未決 #9 の決定: API プロセス内)。
	//
	// **レプリカの数だけ同時に走る**ので、登録する処理は冪等で、
	// 同じ行を掴まない形になっている必要がある (scheduler の doc を参照)。
	//
	// 期限切れセッションと冪等キーの削除は、リポジトリまで実装済みで
	// **配線されていなかった** —— 未決 #9 が決まるのを待っていた。
	sessionCleaner := postgres.NewSessionRepository(pool)
	idempotencyCleaner := postgres.NewIdempotencyRepository(pool)

	// **「0 件になるまで繰り返す」が呼び出し規約になっている。**
	//
	// 1 周回しか呼ばないと、上限 (1000 件) を超える流量では単調増加が続く。
	// ADR 0003 の表が「止まったときの影響: sessions が単調増加する」と
	// 挙げていた事象が、そのまま起きる (レビュー指摘)。
	//
	// 回数に上限を置くのは、消しても減らない状態 (バグ) で
	// 無限に回り続けないため。
	drain := func(name string, step func(context.Context) (int64, error)) func(context.Context) error {
		const maxRounds = 100
		return func(ctx context.Context) error {
			for round := range maxRounds {
				n, err := step(ctx)
				if err != nil {
					return err
				}
				if n == 0 {
					return nil
				}
				if round == maxRounds-1 {
					slog.WarnContext(ctx, "定期処理が上限まで回りました (次の周回に持ち越します)",
						slog.String("job", name), slog.Int("rounds", maxRounds))
				}
			}
			return nil
		}
	}

	jobs := []scheduler.Job{
		{
			Name:     "expired_sessions",
			Interval: 1 * time.Hour,
			Run: drain("expired_sessions", func(ctx context.Context) (int64, error) {
				return sessionCleaner.DeleteExpired(ctx, 0)
			}),
		},
		{
			Name:     "expired_idempotency_keys",
			Interval: 1 * time.Hour,
			Run: drain("expired_idempotency_keys", func(ctx context.Context) (int64, error) {
				return idempotencyCleaner.DeleteExpired(ctx, 0, 0)
			}),
		},
	}

	// 画像の回収はストレージが有効なときだけ。
	// 設定が無い環境で回すと、毎回 S3 に届かず ERROR を吐き続ける。
	if imageInteractor != nil {
		jobs = append(jobs, scheduler.Job{
			Name:     "image_reclaim",
			Interval: 10 * time.Minute,
			// **Failed を終了条件に含めない。** 恒久的に消せない
			// オブジェクトがあると、Total() は 0 にならず回り続ける。
			// 進捗 (Deleted + Marked) が 0 になったら止める。
			Run: drain("image_reclaim", func(ctx context.Context) (int64, error) {
				got, err := imageInteractor.Reclaim(ctx, 0, 0)
				if err != nil {
					return 0, err
				}
				return int64(got.Deleted + got.Marked), nil
			}),
		})
	}

	sched := scheduler.New(jobs...)
	sched.Start(ctx)

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

		// **実行中の定期処理も待つ。**
		//
		// 画像の回収は「確保 -> S3 削除 -> 行削除」と進むので、
		// 確保の直後に打ち切ると**確保済みの行が索引から外れたまま残り、
		// 二度と拾われません** (S3 の実体ごと漏れる。レビュー指摘)。
		//
		// 猶予は HTTP と同じ shutdownCtx から取ります。使い切ったら
		// 待たずに落ちますが、そのときは記録が残ります ——
		// **黙って打ち切っていた**のが元の状態でした。
		if err := sched.Wait(shutdownCtx); err != nil {
			slog.Warn("定期処理の完了を待てませんでした",
				slog.String("error", err.Error()))
		}

		slog.Info("シャットダウンが完了しました")
		return nil
	}
}
