package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := NewHandler()
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	return rec
}

func TestEndpoints(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		target   string
		wantCode int
		wantBody string
	}{
		{"liveness", http.MethodGet, "/healthz", http.StatusOK, `"status":"ok"`},
		{"readiness", http.MethodGet, "/readyz", http.StatusOK, `"status":"ok"`},
		{"hello の既定", http.MethodGet, "/hello", http.StatusOK, `"hello, world"`},
		{"hello に名前", http.MethodGet, "/hello?name=gopher", http.StatusOK, `"hello, gopher"`},
		// 仕様書の制約が実際に効いていること (飾りになっていないこと) を見る。
		{"maxLength を超える", http.MethodGet, "/hello?name=" + strings.Repeat("a", 51), http.StatusBadRequest, `"message"`},
		{"仕様に無いパス", http.MethodGet, "/nope", http.StatusNotFound, ""},
		{"仕様に無いメソッド", http.MethodPost, "/hello", http.StatusMethodNotAllowed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, tc.method, tc.target)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantCode, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %s, want contains %s", rec.Body, tc.wantBody)
			}
		})
	}
}
