package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"

	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
)

// **クライアントが決めた値で、応答のステータスを動かせないこと。**
//
// respondSpecError は検証ミドルウェアから**整形済みの文字列**を受け取る。
// これを strings.Contains で見ていたため、要求側が判定を動かせた (実測):
//
//	GET /threads?size=openapi3filter.SecurityRequirementsError -> 401
//	GET /threads?size=abc+request+body+too+large               -> 413
//
// 401 は「ログインすれば解決する」の意味を持ち、フロントはこれで
// ログイン画面へ遷移する (ADR 0013 決定 3)。**このリンクを踏ませるだけで、
// ログイン中の利用者をログイン画面へ飛ばせる**うえ、401 を数えている
// 認証失敗の監視も汚れる。本文が 1 バイトも無い GET が 413 になるほうも、
// 「存在しない本文を縮めろ」と案内することになる。
func TestSpecError_ClientValueCannotForceStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
	}{
		{name: "security の型名を値に入れる", query: securityFailureMarker},
		{name: "型名だけを値に入れる", query: "openapi3filter.SecurityRequirementsError"},
		{name: "本文超過の文言を値に入れる", query: "abc " + maxBytesErrorMarker},
		{name: "前置きごと値に入れる", query: specErrorPrefix + "X: " + maxBytesErrorMarker},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newTestEnv(t)
			rec := env.do(t, http.MethodGet, "/threads?size="+url.QueryEscape(tt.query), "")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (クライアントの値で振り分けが動いた) body=%s",
					rec.Code, rec.Body.String())
			}
			if code := decodeError(t, rec).Error.Code; code != oapigen.INVALIDARGUMENT {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
		})
	}
}

// **ライブラリが実際に返す書式を、そのまま振り分けられること。**
//
// 上の検査だけだと「全部 400 にする」でも通ってしまう。
// ここに並べた文字列は**実測値**で、kin-openapi / gin-middleware が
// 書式を変えたらこのテストが落ちる (照合を前置きと末尾に寄せた根拠でもある)。
func TestRespondSpecError_MapsRealLibraryMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		message    string
		wantStatus int
		wantCode   oapigen.ErrorErrorCode
	}{
		{
			name: "Cookie が無い (GET /me の実測)",
			message: specErrorPrefix + "SecurityRequirementsError: security requirements failed: " +
				"authorization failed: セッションがありません",
			wantStatus: http.StatusUnauthorized,
			wantCode:   oapigen.UNAUTHENTICATED,
		},
		{
			// **security の検証が本文を先に読む**ため、本文超過も
			// SecurityRequirementsError として届く (POST /images の実測)。
			name: "chunked の本文超過 (POST /images の実測)",
			message: specErrorPrefix + "SecurityRequirementsError: security requirements failed: " +
				"reading failed: http: " + maxBytesErrorMarker,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   oapigen.PAYLOADTOOLARGE,
		},
		{
			// security を宣言していないエンドポイントではこちらの形になる。
			name:       "security の無い経路の本文超過",
			message:    specErrorPrefix + "RequestError: request body has an error: reading failed: http: " + maxBytesErrorMarker,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   oapigen.PAYLOADTOOLARGE,
		},
		{
			name:       "パラメータの型違い",
			message:    specErrorPrefix + `RequestError: parameter "threadId" in path has an error: value abc: an invalid integer: invalid syntax`,
			wantStatus: http.StatusBadRequest,
			wantCode:   oapigen.INVALIDARGUMENT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			respondSpecError(c, http.StatusBadRequest, tt.message)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if code := decodeError(t, rec).Error.Code; code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

// **400 の本文に、検証ライブラリの型名を出さないこと。**
// ここはそのままクライアントへ返るため、公開 API の文言が
// ライブラリの実装名に縛られる。
func TestRespondSpecError_HidesLibraryTypeName(t *testing.T) {
	t.Parallel()

	const raw = specErrorPrefix + `RequestError: parameter "size" in query has an error: number must be at most 100`

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	respondSpecError(c, http.StatusBadRequest, raw)

	got := decodeError(t, rec).Error.Message
	if got != `parameter "size" in query has an error: number must be at most 100` {
		t.Errorf("message = %q (前置きが残っている、または直すべき内容まで落ちている)", got)
	}
}
