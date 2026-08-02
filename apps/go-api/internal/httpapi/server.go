package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"

	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
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
}

var _ oapigen.ServerInterface = (*Server)(nil)

// NewServer はハンドラ実装を生成します。
func NewServer(
	threads *threadusecase.ThreadInteractor,
	comments *commentusecase.CommentInteractor,
	db Pinger,
) *Server {
	return &Server{threads: threads, comments: comments, db: db}
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

	// 仕様書そのものをリクエスト検証に使う。
	// minimum / maximum / maxLength / required といった制約が、
	// ドキュメント上の飾りではなく実際に強制される。
	r.Use(ginmiddleware.OapiRequestValidatorWithOptions(spec, &ginmiddleware.Options{
		ErrorHandler: func(c *gin.Context, message string, status int) {
			respondSpecError(c, status, message)
		},
	}))

	oapigen.RegisterHandlersWithOptions(r, deps.Server, oapigen.GinServerOptions{
		ErrorHandler: func(c *gin.Context, err error, status int) {
			respondSpecError(c, status, err.Error())
		},
	})

	return r, nil
}
