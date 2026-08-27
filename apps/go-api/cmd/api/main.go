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
	contactusecase "develop-experiments/apps/go-api/internal/contact/usecase"
	"develop-experiments/apps/go-api/internal/httpapi"
	imageusecase "develop-experiments/apps/go-api/internal/image/usecase"
	"develop-experiments/apps/go-api/internal/infrastructure/mail"
	"develop-experiments/apps/go-api/internal/infrastructure/objectstorage"
	oidcprovider "develop-experiments/apps/go-api/internal/infrastructure/oidc"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres"
	"develop-experiments/apps/go-api/internal/logging"
	moderationusecase "develop-experiments/apps/go-api/internal/moderation/usecase"
	"develop-experiments/apps/go-api/internal/scheduler"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
	"develop-experiments/apps/go-api/internal/viewcount"
)

func main() {
	if err := run(); err != nil {
		slog.Error("startup_failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logging.Setup(logging.Options{
		Debug: cfg.Debug,
		JSON:  cfg.LogFormat == config.LogFormatJSON,
	})
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
		slog.Info("login_enabled",
			slog.Bool("bootstrap_admin", cfg.Auth.BootstrapAdminGoogleSub != ""))
	} else {
		// 認証の設定が無い。**/auth/google の 2 経路だけが 503** になり、
		// 発行済みセッションの検証・/me・ログアウトは動く (ADR 0005 決定 4)。
		//
		// msg はイベント名に固定してある (ADR 0010 の 4-2)。
		// 説明を msg に書くと、Athena で数えるたびに LIKE を書くことになる。
		slog.Warn("login_disabled")
	}

	// **プロキシを信じない既定は、逆側にも壊れ方がある。**
	//
	// TRUSTED_PROXIES が空のとき ClientIP は接続元アドレスを返す。
	// 偽装を受け入れないための既定だが (config.TrustedProxies)、
	// ALB や CloudFront の背後では**全員が同じ「プロキシの IP」になる**。
	// 問い合わせのレート制限は既定で 5 件/時なので、
	// **誰か 5 人が送った時点で、その 1 時間は全員が 429** になる。
	//
	// 弾いた事実は contact_rate_limited に client_ip 付きで残る (同じ IP が
	// 並ぶので後からは分かる) が、**起動時には何も出ていなかった。**
	// 開発環境では素の接続元で正しいので、そこでは出さない。
	if len(cfg.TrustedProxies) == 0 && !cfg.Debug {
		slog.Warn("trusted_proxies_unset")
	}

	// **許可オリジンが空になったことも知らせる。**
	//
	// 空にできる形にしたぶん (config.csvEnv)、打ち間違いでも空になる。
	// フロントと API を別オリジンで動かす構成では、**CORS も csrfGuard も
	// 全滅**して書き込みが 403 の山になる —— ADR 0013 の 5 が
	// 「設定漏れが全書き込み停止に化けた」と記録しているのと同じ形。
	// 同一オリジン構成なら csrfGuard が自分自身を許すので生き残る。
	//
	// 「静かに無効化された」状態には必ずログを立てる、の並びに揃える
	// (login_disabled / trusted_proxies_unset / contact_mail_disabled)。
	if len(cfg.AllowedOrigins) == 0 && !cfg.Debug {
		slog.Warn("cors_allowed_origins_empty")
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
		slog.Info("image_upload_enabled",
			slog.String("bucket", cfg.Storage.Bucket))
	} else {
		// ストレージの設定が無い。POST /images だけが 503 になる。
		slog.Warn("image_upload_disabled")
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

	// 閲覧数のバッファ (docs/adr/0006-view-count-and-popularity.md)。
	//
	// **設定で分岐させない。** DB があれば動き、外部サービスも要らない。
	// 「無効化できる」形にすると、無効なまま気づかず運用する余地が生まれる ——
	// 閲覧数が増えていないことは、誰も検算しないので目に見えない。
	//
	// 重複抑制の窓を設定から取れるようにしてあるのは、
	// **Phase 4 で「抑制あり/なし」を比べるため**。抑制はメモリ上の
	// 近似なので、どれくらい効いているかは実測しないと分からない。
	//
	// **sync は Phase 4 の比較専用** (ADR 0006 の選択肢 A)。
	// 本番相当の設定では config が弾く。
	viewBuffer := viewcount.New(viewcount.WithDedupeWindow(cfg.ViewCount.DedupeWindow))
	var viewCounts viewcount.Recorder = viewBuffer
	if cfg.ViewCount.Mode == config.ViewCountModeSync {
		viewCounts = viewcount.NewSync(threadRepo,
			viewcount.WithDedupeWindow(cfg.ViewCount.DedupeWindow))
		slog.Warn("view_count_sync_mode")
	}

	// **セッションの検証は常に結線する。** sessions を引いて期限を見るだけで、
	// Google を必要としない (ADR 0005 決定 4)。
	// ここを認証の設定で分岐させていた頃は、資格情報の無い環境が
	// Cookie を無視して全リクエストを匿名として扱っていた。
	//
	// 画像の解決だけはストレージの設定に依存する
	// (未設定なら /me の avatarUrl は Google のものだけになる)。
	sessionInteractor := userusecase.NewSessionInteractor(sessionRepo, sessionImageResolver)

	// 問い合わせの受付 (docs/adr/0008-contact-and-mail.md)。
	//
	// **メールの設定で分岐させない。** 受付は DB に書くだけで完結し、
	// 送信は下の定期処理が後から追いつく (決定 1)。設定が無い環境で
	// 503 を返す形にすると、**その間に来た問い合わせが失われる** ——
	// ログインや画像と違い、利用者はもう一度送りに来てくれない。
	contactRepo := postgres.NewContactRepository(pool)
	contactInteractor := contactusecase.NewInteractor(contactRepo,
		contactusecase.WithRateLimit(cfg.Contact.RateLimitWindow, cfg.Contact.RateLimitMax))

	server := httpapi.NewServer(
		threadusecase.NewThreadInteractor(threadRepo, threadImageResolver),
		commentusecase.NewCommentInteractor(
			postgres.NewCommentRepository(pool, cfg.CommentPostMode), threadRepo, imageResolver),
		pool,
		sessionInteractor,
		loginInteractor,
		imageInteractor,
		// **設定で分岐させない。** 削除と記録は DB だけで完結するので、
		// login / images のように「設定が無ければ 503」にする理由がない
		// (ADR 0011 決定 3 の記録は外部サービスに依存しない)。
		moderationusecase.NewInteractor(postgres.NewModerationRepository(pool)),
		// 通報も設定に依存しない。対象の存在確認と通報の読み書きを
		// 同じ実体 (ReportRepository) が satisfy する。
		func() *moderationusecase.ReportInteractor {
			repo := postgres.NewReportRepository(pool)
			return moderationusecase.NewReportInteractor(repo, repo)
		}(),
		contactInteractor,
		viewCounts,
		cfg.Auth,
	)

	router, err := httpapi.NewRouter(httpapi.Deps{
		Server:         server,
		AllowedOrigins: cfg.AllowedOrigins,
		// **空なら X-Forwarded-For を信じない** (レビュー指摘)。
		// gin の既定はその逆で、ヘッダ 1 本でレート制限を回避できる。
		TrustedProxies: cfg.TrustedProxies,
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
					slog.WarnContext(ctx, "scheduler_job_capped",
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

	// 閲覧数のフラッシュ (docs/adr/0006-view-count-and-popularity.md)。
	//
	// **drain で包まない。** 他の Job は「上限まで消して次の周回に持ち越す」
	// 形だが、こちらは 1 回のフラッシュでバッファ全体を書き出す。
	// 繰り返すと、2 周目以降は「その間に来た閲覧」を追いかけるだけになり、
	// 止まらなくなる。
	//
	// **レプリカごとに走ってよい。** 各インスタンスは自分のメモリにある
	// 増分だけを書き、加算は差分なので二重計上にならない。
	// 行のロック順は id 昇順に固定してあり、デッドロックしない
	// (viewcount.take と SQL の FOR UPDATE ... ORDER BY)。
	jobs = append(jobs, scheduler.Job{
		Name:     "view_count_flush",
		Interval: cfg.ViewCount.FlushInterval,
		Run: func(ctx context.Context) error {
			// **sync のときは何も貯まっていない。** Flush は空で返る。
			// ここを分岐させないのは、モードごとに Job の有無が変わると
			// 「どちらのモードで測ったか」がスケジューラの構成にも
			// 影響してしまい、比較の条件が 1 つ増えるため。
			_, err := viewBuffer.Flush(ctx, threadRepo)
			return err
		},
	})

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

	// 問い合わせメールの送信 (docs/adr/0008-contact-and-mail.md 決定 1)。
	//
	// **設定が揃っているときだけ登録する。** 画像の回収と同じ形で、
	// 設定が無い環境で回すと毎回 SMTP に届かず ERROR を吐き続ける。
	//
	// **受付 (POST /contact) はこの分岐に関係なく動く。** 未送信のまま
	// 溜まるだけで、設定を入れた後の周回で送られる ——
	// それが「保存と送信を分ける」ことの見返りになる。
	if cfg.Contact.Mail.Enabled() {
		sender, senderErr := mail.NewSMTP(cfg.Contact.Mail)
		if senderErr != nil {
			return fmt.Errorf("メール送信の初期化に失敗しました: %w", senderErr)
		}
		dispatcher := contactusecase.NewDispatcher(contactRepo, sender)
		if err := dispatcher.Validate(); err != nil {
			return err
		}

		jobs = append(jobs, scheduler.Job{
			Name:     "contact_mail_dispatch",
			Interval: cfg.Contact.DispatchInterval,
			// **drain で包む。** 1 周回は batchSize (既定 20) 件までなので、
			// 溜まっている状態では 1 回の実行で捌ききれない。
			//
			// 失敗した行は next_attempt_at が先送りされ、次の周回の
			// 対象から外れる。**だから止まる** —— プロバイダが落ちていても
			// 無限ループにはならない。
			Run: drain("contact_mail_dispatch", func(ctx context.Context) (int64, error) {
				got, err := dispatcher.Dispatch(ctx)
				if err != nil {
					return 0, err
				}
				return int64(got.Total()), nil
			}),
		})
		slog.Info("contact_mail_enabled",
			slog.String("smtp_host", cfg.Contact.Mail.Host),
			slog.Bool("starttls", cfg.Contact.Mail.StartTLS))
	} else {
		// **受付は動く。** 溜まった未送信は設定を入れれば送られる。
		slog.Warn("contact_mail_disabled")
	}

	// 送信元 IP の消し込み (ADR 0008「client_ip は個人データにあたるため、
	// 無期限には保持しない」)。
	//
	// **メールの設定とは無関係に回す。** 消し込みは DB だけで完結し、
	// メールが送れない環境でも個人データは溜まる。
	// 送信の可否と保持期間の管理を同じ条件に束ねると、
	// 「SMTP の設定を入れ忘れた環境だけ IP が消えない」ことになる。
	jobs = append(jobs, scheduler.Job{
		Name:     "contact_ip_scrub",
		Interval: 12 * time.Hour,
		Run: drain("contact_ip_scrub", func(ctx context.Context) (int64, error) {
			return contactRepo.ScrubClientIPs(ctx, cfg.Contact.IPRetention, 0)
		}),
	})

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
		slog.Info("server_started", slog.String("addr", cfg.Addr))
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
		slog.Info("shutdown_started")

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
			slog.Warn("scheduler_wait_timeout",
				slog.String("error", err.Error()))
		}

		// **最後に閲覧数を書き出す** (docs/adr/0006-view-count-and-popularity.md)。
		//
		// バッファはこのプロセスのメモリにあるので、ここで書かなければ
		// 未反映の増分は消えます。デプロイのたびに数秒ぶんの閲覧が
		// 落ちることになり、**再デプロイが多い日ほど閲覧数が伸びない**
		// という、原因の分かりにくい形で効きます。
		//
		// SIGKILL やクラッシュは救えません。それは ADR 0006 が
		// 「値がロストする」として引き受けたコストのままです。
		//
		// スケジューラを待った後に置くのは、フラッシュの Job が
		// 実行中だった場合に二重に走らせないためです。
		if _, err := viewBuffer.Flush(shutdownCtx, threadRepo); err != nil {
			slog.Warn("view_count_flush_failed",
				slog.String("error", err.Error()))
		}

		slog.Info("shutdown_completed")
		return nil
	}
}
