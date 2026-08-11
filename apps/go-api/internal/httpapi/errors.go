// Package httpapi は HTTP の入出力と、ユースケース層との橋渡しを担います。
//
// ルーティング・パラメータの解析・リクエストの検証は
// api/openapi.yaml から生成された oapigen パッケージが担当します。
// このパッケージが実装するのは oapigen.ServerInterface です。
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/routers"
	"github.com/gin-gonic/gin"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
)

// newErrorBody は仕様書で定義されたエラー表現を組み立てます。
// 生成された型を使うため、仕様と食い違うレスポンスはコンパイルできません。
func newErrorBody(code oapigen.ErrorErrorCode, message string) oapigen.Error {
	var body oapigen.Error
	body.Error.Code = code
	body.Error.Message = message
	return body
}

// respondError はドメイン層のエラーを HTTP ステータスに翻訳して返します。
//
// 500 のときに err.Error() をそのまま返さないのは、
// SQL 文やテーブル名といった内部構造が漏れるのを防ぐためです。
// 詳細はログにだけ出します。
func respondError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, apperr.ErrNotFound):
		c.JSON(http.StatusNotFound, newErrorBody(oapigen.NOTFOUND, "対象のリソースが見つかりません"))

	case errors.Is(err, apperr.ErrInvalidArgument):
		c.JSON(http.StatusBadRequest, newErrorBody(oapigen.INVALIDARGUMENT, err.Error()))

	case errors.Is(err, apperr.ErrUnauthenticated):
		c.JSON(http.StatusUnauthorized, newErrorBody(oapigen.UNAUTHENTICATED, "ログインが必要です"))

	case errors.Is(err, apperr.ErrUnavailable):
		c.JSON(http.StatusServiceUnavailable, newErrorBody(oapigen.UNAVAILABLE, err.Error()))

	case errors.Is(err, apperr.ErrConflict):
		c.JSON(http.StatusConflict, newErrorBody(oapigen.CONFLICT,
			"同時更新が競合しました。時間をおいて再試行してください"))

	default:
		slog.ErrorContext(c.Request.Context(), "未分類のエラー",
			slog.String("path", c.FullPath()),
			slog.String("error", err.Error()),
		)
		c.JSON(http.StatusInternalServerError, newErrorBody(oapigen.INTERNAL,
			"サーバ内部でエラーが発生しました"))
	}
}

// securityFailureMarker は、検証ミドルウェアが security 要件の失敗を
// 報告するときにメッセージへ含める文字列です。
//
// gin-middleware は openapi3filter のエラーを型で渡さず、
// 整形済みの文字列として渡してくるため、ここで文字列照合するしかありません。
// ライブラリ側の書式が変われば追随が要ります
// (server_test.go の TestAuth_MissingCookieIs401 が検出します)。
const securityFailureMarker = "openapi3filter.SecurityRequirementsError"

// respondSpecError は仕様書に基づく検証で弾かれた場合の応答です。
// oapigen の生成コードと、仕様検証ミドルウェアの両方から呼ばれます。
//
// ミドルウェアは未定義パスにのみ 404 を割り当て、
// それ以外 (メソッド不一致や security 要件の失敗を含む) を
// すべて 400 に丸めてしまいます。
// 本来のステータスに振り分け直すのがこの関数の役割です。
func respondSpecError(c *gin.Context, status int, message string) {
	switch {
	case message == routers.ErrMethodNotAllowed.Error():
		c.AbortWithStatusJSON(http.StatusMethodNotAllowed,
			newErrorBody(oapigen.METHODNOTALLOWED, "そのメソッドは許可されていません"))

	// security 要件の失敗は 401。
	//
	// gin-middleware は SecurityRequirementsError も 400 に丸めるため、
	// そのままだと「未ログイン」が INVALID_ARGUMENT で返る。
	// ADR 0013 は 401 と 403 を取り違えないことを求めており、
	// フロントは 401 でログイン画面へ遷移する。
	// 400 のままだとその分岐ができない。
	case strings.Contains(message, securityFailureMarker):
		c.AbortWithStatusJSON(http.StatusUnauthorized,
			newErrorBody(oapigen.UNAUTHENTICATED, "ログインが必要です"))

	case status == http.StatusNotFound:
		c.AbortWithStatusJSON(http.StatusNotFound,
			newErrorBody(oapigen.NOTFOUND, "そのエンドポイントは存在しません"))

	default:
		if status < http.StatusBadRequest {
			status = http.StatusBadRequest
		}
		c.AbortWithStatusJSON(status, newErrorBody(oapigen.INVALIDARGUMENT, message))
	}
}

// respondBadRequest はリクエストボディの解析失敗など、
// 仕様検証より手前で弾く場合の応答です。
func respondBadRequest(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusBadRequest,
		newErrorBody(oapigen.INVALIDARGUMENT, message))
}
