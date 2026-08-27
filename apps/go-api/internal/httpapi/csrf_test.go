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

// ---------------------------------------------------------------------------
// 同一オリジン構成 (レビュー指摘)
// ---------------------------------------------------------------------------
//
// フロントと API を 1 つのホストに置く構成では CORS が本当に不要なので、
// 運用者が CORS_ALLOWED_ORIGINS を設定する理由がありません。
// 既定は開発用の http://localhost:3000 なので、**設定漏れのまま本番へ出すと
// 書き込みが全滅します。** 自分自身のオリジンは常に許します。

// csrfRequestTo は Host を指定して 1 本投げます。
func csrfRequestTo(t *testing.T, host string, headers map[string]string) int {
	t.Helper()

	env := newTestEnv(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/threads",
		strings.NewReader(`{"title":"テスト"}`))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec.Code
}

// **許可リストに無くても、自分自身のオリジンなら通ること。**
func TestCSRF_AllowsSameOrigin(t *testing.T) {
	t.Parallel()

	got := csrfRequestTo(t, "app.example.com",
		map[string]string{"Origin": "http://app.example.com"})
	if got == http.StatusForbidden {
		t.Error("同一オリジンからの POST が 403 になった (設定漏れで全滅する)")
	}
}

// X-Forwarded-Proto があれば scheme をそちらから取ること。
func TestCSRF_AllowsSameOriginBehindProxy(t *testing.T) {
	t.Parallel()

	got := csrfRequestTo(t, "app.example.com", map[string]string{
		"Origin":            "https://app.example.com",
		"X-Forwarded-Proto": "https",
	})
	if got == http.StatusForbidden {
		t.Error("ALB の内側で同一オリジンが 403 になった")
	}
}

// 多段プロキシではカンマ区切りで積まれるので、手前のものを使うこと。
func TestCSRF_UsesFirstForwardedProto(t *testing.T) {
	t.Parallel()

	got := csrfRequestTo(t, "app.example.com", map[string]string{
		"Origin":            "https://app.example.com",
		"X-Forwarded-Proto": "https, http",
	})
	if got == http.StatusForbidden {
		t.Error("X-Forwarded-Proto が複数あると 403 になった")
	}
}

// **別ホストは自分自身にならないこと。**
// ここが緩むと「Origin さえ付いていれば通る」になり、防御が消える。
func TestCSRF_SameOriginDoesNotAllowOtherHosts(t *testing.T) {
	t.Parallel()

	for _, origin := range []string{
		"http://evil.test",
		"https://app.example.com",     // scheme 違い (X-Forwarded-Proto なし = http)
		"http://app.example.com:8443", // port 違い
		"http://app.example.com.evil.test",
	} {
		if got := csrfRequestTo(t, "app.example.com",
			map[string]string{"Origin": origin}); got != http.StatusForbidden {
			t.Errorf("Origin=%q で status = %d, want 403", origin, got)
		}
	}
}

// **ログに残すヘッダ値を丸めること。**
//
// Referer は完全に相手が決める値で、長さは MaxHeaderBytes までしか
// 縛られていない。未認証の POST 1 本ごとに WARN が 1 行出るので、
// 大きな値を撒かれると S3 の保管コストがそのまま膨らむ。
func TestTruncateForLog(t *testing.T) {
	t.Parallel()

	short := strings.Repeat("a", maxLoggedHeaderBytes)
	if got := truncateForLog(short); got != short {
		t.Error("上限ちょうどの値が丸められた")
	}

	// **上限のすぐ上では、印のぶん元より長くなりうる。**
	// 見たいのは「入力に比例して伸びないこと」なので、長さで測る。
	long := strings.Repeat("a", 100_000)
	got := truncateForLog(long)
	if len(got) > maxLoggedHeaderBytes+32 {
		t.Errorf("入力に比例して伸びている (len=%d)", len(got))
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("丸めた印が無い: %q", got[max(0, len(got)-20):])
	}
}
