package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"develop-experiments/apps/go-api/internal/apperr"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/gin-gonic/gin"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"

	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
)

// Pinger は DB への疎通確認を抽象化したものです。*pgxpool.Pool が満たします。
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server は oapigen.ServerInterface の実装です。
//
// 仕様書に定義されたエンドポイントを実装し忘れると、
// この代入がコンパイルエラーになります。
type Server struct {
	threads  *threadusecase.ThreadInteractor
	comments *commentusecase.CommentInteractor
	db       Pinger
	// auth は認証の設定が無い場合 nil になります。
	// そのとき認証エンドポイントだけが 503 を返します。
	auth    *userusecase.AuthInteractor
	authCfg config.AuthConfig
}

var _ oapigen.ServerInterface = (*Server)(nil)

// NewServer はハンドラ実装を生成します。
// auth が nil の場合、認証エンドポイントは 503 を返します。
func NewServer(
	threads *threadusecase.ThreadInteractor,
	comments *commentusecase.CommentInteractor,
	db Pinger,
	auth *userusecase.AuthInteractor,
	authCfg config.AuthConfig,
) *Server {
	return &Server{threads: threads, comments: comments, db: db, auth: auth, authCfg: authCfg}
}

// ---------------------------------------------------------------------------
// health
// ---------------------------------------------------------------------------

// GetHealthz は GET /healthz を処理します。
//
// プロセスが生きているかだけを返し、DB は見ません。
// ここで DB を見ると、DB の一時的な不調でコンテナが再起動ループに入ります。
func (s *Server) GetHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, oapigen.HealthStatus{Status: "ok"})
}

// GetReadyz は GET /readyz を処理します。
// 依存先を含めてリクエストを受けられるかを返します。
func (s *Server) GetReadyz(c *gin.Context) {
	if err := s.db.Ping(c.Request.Context()); err != nil {
		reason := "database"
		c.JSON(http.StatusServiceUnavailable, oapigen.HealthStatus{
			Status: "unavailable",
			Reason: &reason,
		})
		return
	}
	c.JSON(http.StatusOK, oapigen.HealthStatus{Status: "ok"})
}

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

// StartGoogleLogin は GET /auth/google を処理します。
//
// state / nonce / code_verifier を発行し、短命な Cookie に預けてから
// Google の認可エンドポイントへリダイレクトします。
func (s *Server) StartGoogleLogin(c *gin.Context) {
	if err := s.requireAuthEnabled(); err != nil {
		respondError(c, err)
		return
	}

	req, err := s.auth.StartLogin()
	if err != nil {
		respondError(c, err)
		return
	}

	s.setFlowCookie(c, stateCookieName, req.State)
	s.setFlowCookie(c, nonceCookieName, req.Nonce)
	s.setFlowCookie(c, verifierCookieName, req.CodeVerifier)

	c.Redirect(http.StatusFound, req.AuthURL)
}

// GoogleLoginCallback は GET /auth/google/callback を処理します。
func (s *Server) GoogleLoginCallback(c *gin.Context, params oapigen.GoogleLoginCallbackParams) {
	if err := s.requireAuthEnabled(); err != nil {
		respondError(c, err)
		return
	}

	// Cookie を読むのはここだけ。読み終えたら**検査より先に**捨てる。
	//
	// defer にしてはいけない。defer が走るのはハンドラから戻るときで、
	// その時点では c.Redirect / respondError がヘッダを書き出し済みのため
	// Set-Cookie がレスポンスに載らない。
	// 早期 return する経路 (ログイン CSRF の検知など) では
	// そもそも defer の登録前に抜けてしまう。捨てたいのはむしろその経路。
	wantState, _ := c.Cookie(stateCookieName)
	nonce, _ := c.Cookie(nonceCookieName)
	verifier, _ := c.Cookie(verifierCookieName)
	s.clearFlowCookies(c)

	// state の照合は HTTP 層の仕事。
	// 一致を確認できなければ先へ進まない。これが無いと、攻撃者が用意した
	// 認可コードを被害者のブラウザで交換させられる (ログイン CSRF)。
	//
	// 拒否された場合も Google は state を返すので、error の判定より先に置ける。
	if wantState == "" || wantState != params.State {
		respondError(c, fmt.Errorf("state が一致しません: %w", apperr.ErrUnauthenticated))
		return
	}

	// 同意画面で拒否された場合。code は付かず error だけが返る。
	// ここで 4xx の JSON を返すと、ブラウザにそれが直接表示されてしまう。
	if params.Error != nil && *params.Error != "" {
		c.Redirect(http.StatusFound, s.frontendURLWithError(*params.Error))
		return
	}

	if params.Code == nil || *params.Code == "" {
		respondError(c, fmt.Errorf("認可コードがありません: %w", apperr.ErrUnauthenticated))
		return
	}
	if nonce == "" {
		respondError(c, fmt.Errorf("nonce がありません: %w", apperr.ErrUnauthenticated))
		return
	}
	if verifier == "" {
		respondError(c, fmt.Errorf("code_verifier がありません: %w", apperr.ErrUnauthenticated))
		return
	}

	result, err := s.auth.CompleteLogin(c.Request.Context(), *params.Code, verifier, nonce)
	if err != nil {
		respondError(c, err)
		return
	}

	// MaxAge はインタラクタが返す寿命をそのまま使う。
	// ここで time.Until(ExpiresAt) を計算すると、インタラクタの時計と
	// 壁時計がずれたときに負になり、発行と同時に失効する Cookie ができる。
	s.setSessionCookie(c, result.Token, int(result.TTL.Seconds()))
	c.Redirect(http.StatusFound, s.authCfg.FrontendURL)
}

// Logout は POST /auth/logout を処理します。
func (s *Server) Logout(c *gin.Context) {
	if err := s.requireAuthEnabled(); err != nil {
		respondError(c, err)
		return
	}

	// ここに到達している時点で、仕様書の security 要件は満たされている。
	if raw, err := c.Cookie(sessionCookieName); err == nil && raw != "" {
		if err := s.auth.Logout(c.Request.Context(), usermodel.SessionToken(raw)); err != nil {
			respondError(c, err)
			return
		}
	}

	s.clearCookie(c, sessionCookieName)
	c.Status(http.StatusNoContent)
}

// GetMe は GET /me を処理します。
//
// セッションの解決は resolveSession が済ませており、
// 未ログインなら検証ミドルウェアが 401 で弾いています。
// ここまで来た時点で principal は必ず存在します。
func (s *Server) GetMe(c *gin.Context) {
	p := principalFromContext(c.Request.Context())
	if p == nil {
		// 到達しない想定。security 宣言と resolveSession の
		// どちらかが外れたときだけここに来る。
		respondError(c, fmt.Errorf("セッションがありません: %w", apperr.ErrUnauthenticated))
		return
	}

	c.JSON(http.StatusOK, toWireMe(p.Me))
}

// ---------------------------------------------------------------------------
// threads
// ---------------------------------------------------------------------------

// ListThreads は GET /threads を処理します。
func (s *Server) ListThreads(c *gin.Context, params oapigen.ListThreadsParams) {
	page, err := toPage(params.Cursor, params.Size)
	if err != nil {
		respondError(c, err)
		return
	}

	result, err := s.threads.FetchThreadList(c.Request.Context(), page)
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, toWireThreadList(result))
}

// CreateThread は POST /threads を処理します。
func (s *Server) CreateThread(c *gin.Context) {
	var req oapigen.CreateThreadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "リクエストボディが不正です: "+err.Error())
		return
	}

	// ログイン中なら投稿者を紐付ける。未ログインなら nil = 匿名投稿。
	thread, err := s.threads.CreateThread(c.Request.Context(), req.Title,
		authorIDFromContext(c.Request.Context()))
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusCreated, toWireThread(thread))
}

// GetThread は GET /threads/{threadId} を処理します。
func (s *Server) GetThread(c *gin.Context, threadID oapigen.ThreadId) {
	if err := validateThreadID(threadID); err != nil {
		respondError(c, err)
		return
	}

	thread, err := s.threads.FetchThread(c.Request.Context(), threadID)
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, toWireThread(thread))
}

// ---------------------------------------------------------------------------
// comments
// ---------------------------------------------------------------------------

// ListComments は GET /threads/{threadId}/comments を処理します。
func (s *Server) ListComments(
	c *gin.Context, threadID oapigen.ThreadId, params oapigen.ListCommentsParams,
) {
	if err := validateThreadID(threadID); err != nil {
		respondError(c, err)
		return
	}

	page, err := toPage(params.Cursor, params.Size)
	if err != nil {
		respondError(c, err)
		return
	}

	result, err := s.comments.FetchComments(c.Request.Context(), threadID, page)
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusOK, toWireCommentList(result))
}

// CreateComment は POST /threads/{threadId}/comments を処理します。
func (s *Server) CreateComment(
	c *gin.Context, threadID oapigen.ThreadId, params oapigen.CreateCommentParams,
) {
	if err := validateThreadID(threadID); err != nil {
		respondError(c, err)
		return
	}

	var req oapigen.CreateCommentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "リクエストボディが不正です: "+err.Error())
		return
	}

	var authorName string
	if req.AuthorName != nil {
		authorName = *req.AuthorName
	}

	ctx := c.Request.Context()
	authorID := authorIDFromContext(ctx)

	// **匿名では冪等キーを無視する** (docs/adr/0015-idempotency.md 決定 4)。
	// キーの名前空間を分ける手段が無く、IP で分けると NAT の背後で
	// 他人のキーと衝突して「他人の投稿結果が返る」ことになる。
	//
	// エラーにせず無視するのは、ログインの有無でクライアントの実装を
	// 分けさせないため。この非対称は仕様書に明記してある。
	if key := params.IdempotencyKey; key != nil && authorID != nil {
		// **指紋はここで作りません。** ユースケース層が正規化したあとの値で
		// 組み立てます (受け取ったままの値だと、結果に影響しない差で
		// 422 になる。ADR 0015 の「実装して分かったこと」7)。
		comment, err := s.comments.PostCommentIdempotent(
			ctx, threadID, authorName, req.Body, authorID,
			*key, idempotencyEndpoint(c))
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toWireComment(comment))
		return
	}

	comment, err := s.comments.PostComment(ctx, threadID, authorName, req.Body, authorID)
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusCreated, toWireComment(comment))
}

// idempotencyEndpoint は冪等キーの指紋に含める経路名です。
//
// **具体的なパスを使います。** ルートの雛形 (/threads/:threadId/comments) だと
// スレッドが違っても同じ値になり、同じキーで別スレッドへ投稿したときに
// 「同じ内容の再送」と判定されてしまいます。
func idempotencyEndpoint(c *gin.Context) string {
	return c.Request.Method + " " + c.Request.URL.Path
}

// ---------------------------------------------------------------------------
// ルータの組み立て
// ---------------------------------------------------------------------------

// Deps はルータの組み立てに必要な依存です。
type Deps struct {
	Server         *Server
	AllowedOrigins []string
}

// NewRouter はルーティングを組み立てた gin.Engine を返します。
//
// ルーティング定義は書きません。api/openapi.yaml から生成された
// oapigen.RegisterHandlersWithOptions が行います。
// パスを追加したければ仕様書を編集してください。
func NewRouter(deps Deps) (*gin.Engine, error) {
	spec, err := oapigen.GetSwagger()
	if err != nil {
		return nil, err
	}
	// servers に書いた URL でリクエストを絞られると、
	// コンテナ名でアクセスした場合などに 400 になってしまうため無効化する。
	spec.Servers = nil

	r := gin.New()
	// 並びの意味は middlewares のコメントを参照。
	r.Use(middlewares(deps.AllowedOrigins)...)

	// セッションの解決は仕様検証より前に置く。
	// AuthenticationFunc がここで載せた結果を見るため、順序が逆だと
	// security を宣言したエンドポイントが常に 401 になる。
	r.Use(deps.Server.resolveSession())

	// 仕様書そのものをリクエスト検証に使う。
	// minimum / maximum / maxLength / required といった制約が、
	// ドキュメント上の飾りではなく実際に強制される。
	r.Use(ginmiddleware.OapiRequestValidatorWithOptions(spec, &ginmiddleware.Options{
		ErrorHandler: func(c *gin.Context, message string, status int) {
			respondSpecError(c, status, message)
		},
		Options: openapi3filter.Options{
			// security を宣言したオペレーションでは、これが未設定だと
			// kin-openapi が ErrAuthenticationServiceMissing を返して
			// リクエストを弾く。仕様書に security を書いた時点で結線は必須になる。
			AuthenticationFunc: newAuthenticationFunc(),
		},
	}))

	oapigen.RegisterHandlersWithOptions(r, deps.Server, oapigen.GinServerOptions{
		ErrorHandler: func(c *gin.Context, err error, status int) {
			respondSpecError(c, status, err.Error())
		},
	})

	return r, nil
}
