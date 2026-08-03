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

// respondSpecError は仕様書に基づく検証で弾かれた場合の応答です。
// oapigen の生成コードと、仕様検証ミドルウェアの両方から呼ばれます。
//
// ミドルウェアは未定義パスにのみ 404 を割り当て、
// それ以外 (メソッド不一致を含む) をすべて 400 に丸めてしまいます。
// 「定義済みのパスに未定義のメソッド」は本来 405 が正しいため、
// ここで振り分け直します。
func respondSpecError(c *gin.Context, status int, message string) {
	switch {
	case message == routers.ErrMethodNotAllowed.Error():
		c.AbortWithStatusJSON(http.StatusMethodNotAllowed,
			newErrorBody(oapigen.METHODNOTALLOWED, "そのメソッドは許可されていません"))

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
