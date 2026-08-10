package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

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

	// state の照合は HTTP 層の仕事。Cookie を読むのはここだけにする。
	//
	// 一致を確認できなければ先へ進まない。これが無いと、攻撃者が用意した
	// 認可コードを被害者のブラウザで交換させられる (ログイン CSRF)。
	wantState, err := c.Cookie(stateCookieName)
	if err != nil || wantState == "" || wantState != params.State {
		respondError(c, fmt.Errorf("state が一致しません: %w", apperr.ErrUnauthenticated))
		return
	}

	nonce, err := c.Cookie(nonceCookieName)
	if err != nil || nonce == "" {
		respondError(c, fmt.Errorf("nonce がありません: %w", apperr.ErrUnauthenticated))
		return
	}
	verifier, err := c.Cookie(verifierCookieName)
	if err != nil || verifier == "" {
		respondError(c, fmt.Errorf("code_verifier がありません: %w", apperr.ErrUnauthenticated))
		return
	}

	// 使い終わったフロー用 Cookie は、成否にかかわらず必ず捨てる。
	// 残すと再利用の余地ができる。
	defer func() {
		s.clearCookie(c, stateCookieName)
		s.clearCookie(c, nonceCookieName)
		s.clearCookie(c, verifierCookieName)
	}()

	result, err := s.auth.CompleteLogin(c.Request.Context(), params.Code, verifier, nonce)
	if err != nil {
		respondError(c, err)
		return
	}

	s.setSessionCookie(c, result.Token, int(time.Until(result.ExpiresAt).Seconds()))
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
	me := principalFromContext(c.Request.Context())
	if me == nil {
		// 到達しない想定。security 宣言と resolveSession の
		// どちらかが外れたときだけここに来る。
		respondError(c, fmt.Errorf("セッションがありません: %w", apperr.ErrUnauthenticated))
		return
	}

	c.JSON(http.StatusOK, toWireMe(*me))
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

	thread, err := s.threads.CreateThread(c.Request.Context(), req.Title)
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
func (s *Server) CreateComment(c *gin.Context, threadID oapigen.ThreadId) {
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

	comment, err := s.comments.PostComment(c.Request.Context(), threadID, authorName, req.Body)
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusCreated, toWireComment(comment))
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
	r.Use(gin.Recovery(), requestLogger(), cors(deps.AllowedOrigins))

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
