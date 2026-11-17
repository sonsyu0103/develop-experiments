// Package httpapi は HTTP の境界。生成された oapigen.ServerInterface を実装する。
package httpapi

import (
	"fmt"
	"net/http"

	"github.com/getkin/kin-openapi/routers"
	"github.com/gin-gonic/gin"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"

	"example.com/go-service/internal/httpapi/oapigen"
)

// Server は oapigen.ServerInterface の実装。
type Server struct{}

// 仕様書に足したエンドポイントを実装し忘れると、この代入がコンパイルエラーになる。
var _ oapigen.ServerInterface = (*Server)(nil)

// NewHandler はルーティングとミドルウェアを組み立てる。
func NewHandler() (http.Handler, error) {
	spec, err := oapigen.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("埋め込みの仕様書を読めない: %w", err)
	}
	// servers を消さないと、検証ミドルウェアがホスト名まで照合して全件 404 になる。
	spec.Servers = nil

	r := gin.New()
	r.Use(gin.Recovery())

	// **プロキシを信じない。** gin の既定は全アドレスを信じる設定で、
	// X-Forwarded-For を誰でも偽装できる。前段のプロキシが決まったら
	// その CIDR だけを渡す。
	if err := r.SetTrustedProxies(nil); err != nil {
		return nil, fmt.Errorf("信頼するプロキシの設定に失敗: %w", err)
	}

	// 仕様書そのものでリクエストを検証する。maxLength などが実際に強制され、
	// 仕様に無いパスは 404 になる。
	r.Use(ginmiddleware.OapiRequestValidatorWithOptions(spec, &ginmiddleware.Options{
		ErrorHandler: func(c *gin.Context, message string, status int) {
			// **仕様に無いメソッドは 405 に直す。** ミドルウェアは 400 で返すので、
			// そのままだと「パラメータが悪い」と区別できない (実測)。
			if message == routers.ErrMethodNotAllowed.Error() {
				status = http.StatusMethodNotAllowed
			}
			c.AbortWithStatusJSON(status, oapigen.Error{Message: message})
		},
	}))

	oapigen.RegisterHandlers(r, &Server{})
	return r, nil
}

// GetHealthz は GET /healthz。プロセスが生きているかだけを返す。
func (s *Server) GetHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, oapigen.HealthStatus{Status: "ok"})
}

// GetReadyz は GET /readyz。依存先を足したら、ここで疎通を見て 503 を返す。
func (s *Server) GetReadyz(c *gin.Context) {
	c.JSON(http.StatusOK, oapigen.HealthStatus{Status: "ok"})
}

// GetHello は GET /hello (見本)。
func (s *Server) GetHello(c *gin.Context, params oapigen.GetHelloParams) {
	name := "world"
	if params.Name != nil {
		name = *params.Name
	}
	c.JSON(http.StatusOK, oapigen.Greeting{Message: "hello, " + name})
}
