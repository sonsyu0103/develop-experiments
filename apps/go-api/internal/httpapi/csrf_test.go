package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CSRF (docs/adr/0013-http-defense.md 決定 1)
// ---------------------------------------------------------------------------
//
// **CORS では防げません。** CORS はブラウザにレスポンスを読ませない仕組みで、
// リクエスト自体は飛びます。しかも multipart/form-data はプリフライトを
// 起こさないので、画像アップロードの経路では CORS が一切効きません。

// csrfRequest は状態変更メソッドを 1 本投げ、status を返します。
//
// **セッションを持たせません。** csrfGuard は認証より前に効くので、
// 未ログインでも 403 が先に返るのが正しい形になります。
func csrfRequest(t *testing.T, method, path string, headers map[string]string) int {
	t.Helper()

	env := newTestEnv(t)
	req := httptest.NewRequestWithContext(t.Context(), method, path,
		strings.NewReader(`{"title":"テスト"}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec.Code
}

// **許可オリジンからの POST は通ること。**
func TestCSRF_AllowsListedOrigin(t *testing.T) {
	t.Parallel()

	if got := csrfRequest(t, http.MethodPost, "/threads",
		map[string]string{"Origin": testOrigin}); got == http.StatusForbidden {
		t.Errorf("許可オリジンが 403 になった")
	}
}

// **知らないオリジンからの POST は 403。**
func TestCSRF_RejectsUnknownOrigin(t *testing.T) {
	t.Parallel()

	if got := csrfRequest(t, http.MethodPost, "/threads",
		map[string]string{"Origin": "https://evil.test"}); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
}

// **Origin も Referer も無ければ 403。**
//
// ここを「無ければ通す」にすると、**送らないだけで検証を迂回できる**ので
// 防御になりません。ブラウザ以外のクライアントにも Origin を要求します。
func TestCSRF_RejectsMissingOriginAndReferer(t *testing.T) {
	t.Parallel()

	if got := csrfRequest(t, http.MethodPost, "/threads", nil); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
}

// Origin が無ければ Referer で照合すること。
func TestCSRF_FallsBackToReferer(t *testing.T) {
	t.Parallel()

	if got := csrfRequest(t, http.MethodPost, "/threads",
		map[string]string{"Referer": testOrigin + "/threads/1"}); got == http.StatusForbidden {
		t.Errorf("許可オリジンの Referer が 403 になった")
	}
}

// **前方一致で判定していないこと。**
//
// "http://localhost:3000.evil.test" は許可値で始まるので、
// 文字列の前方一致だと通ってしまいます。
func TestCSRF_RejectsPrefixLookalikeReferer(t *testing.T) {
	t.Parallel()

	for _, referer := range []string{
		testOrigin + ".evil.test/x",
		"https://localhost:3000/x", // scheme が違う
		"http://localhost:3001/x",  // port が違う
		"http://evil.test/http://localhost:3000",
	} {
		if got := csrfRequest(t, http.MethodPost, "/threads",
			map[string]string{"Referer": referer}); got != http.StatusForbidden {
			t.Errorf("Referer=%q で status = %d, want 403", referer, got)
		}
	}
}

// **Origin があれば Referer は見ないこと。**
// 通す方向に倒れると、正しい Referer を足すだけで迂回できます。
func TestCSRF_OriginWinsOverReferer(t *testing.T) {
	t.Parallel()

	got := csrfRequest(t, http.MethodPost, "/threads", map[string]string{
		"Origin":  "https://evil.test",
		"Referer": testOrigin + "/threads",
	})
	if got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (Referer で迂回できている)", got)
	}
}

// **GET は素通しすること。**
//
// 前提は「GET / HEAD に副作用を持たせない」こと (ADR 0013 決定 1)。
// この不変条件が崩れたらトークン方式の再検討が要ります。
func TestCSRF_AllowsSafeMethods(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if got := csrfRequest(t, method, "/threads", nil); got == http.StatusForbidden {
			t.Errorf("%s が 403 になった", method)
		}
	}
}

// 状態変更メソッドの判定。
func TestIsStateChanging(t *testing.T) {
	t.Parallel()

	for method, want := range map[string]bool{
		http.MethodPost:    true,
		http.MethodPut:     true,
		http.MethodPatch:   true,
		http.MethodDelete:  true,
		http.MethodGet:     false,
		http.MethodHead:    false,
		http.MethodOptions: false,
	} {
		if got := isStateChanging(method); got != want {
			t.Errorf("isStateChanging(%s) = %v, want %v", method, got, want)
		}
	}
}

// originOf の切り出し。
func TestOriginOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		referer string
		want    string
		ok      bool
	}{
		{"http://localhost:3000/threads/1", "http://localhost:3000", true},
		{"https://example.com", "https://example.com", true},
		{"https://example.com:8443/a/b?c=d", "https://example.com:8443", true},
		{"/threads/1", "", false}, // 相対 URL
		{"", "", false},
		{"::not a url", "", false},
	}
	for _, tt := range tests {
		got, ok := originOf(tt.referer)
		if got != tt.want || ok != tt.ok {
			t.Errorf("originOf(%q) = (%q, %v), want (%q, %v)",
				tt.referer, got, ok, tt.want, tt.ok)
		}
	}
}
