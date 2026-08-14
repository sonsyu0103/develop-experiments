package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// **本文の上限が「読む前」に効いているかを実測する。**
//
// レビューで指摘されるまで、上限はハンドラの中で張っていた。
// しかし仕様検証ミドルウェアは、ハンドラに入る前に本文を丸ごと読む。
// しかも security の検証が AuthenticationFunc を呼ぶ前に読むため、
// **未ログインの要求でも本文がすべてメモリに載っていた** (実測 30 MiB)。
//
// ステータスだけを見るテストでは検出できない —— 当時も 413 / 401 は
// 正しく返っていた。**何バイト読まれたか**を数える必要がある。

// countingReader は実際に読まれたバイト数を数えます。
type countingReader struct {
	remaining int64
	read      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	r.remaining -= n
	r.read += n
	for i := range p[:n] {
		p[i] = 'x'
	}
	return int(n), nil
}
func (r *countingReader) Close() error { return nil }

// **巨大な本文が丸ごと読まれないこと。**
func TestBodyLimit_DoesNotBufferHugeBody(t *testing.T) {
	env := newImageEnv(t)

	const size = 30 << 20 // 30 MiB
	body := &countingReader{remaining: size}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", body)
	// **Origin が要る** (ADR 0013 決定 1 の CSRF 対策)。
	// 付けないと csrfGuard が 403 で打ち切り、ここから先を検査できない。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.ContentLength = size

	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	t.Logf("status=%d 読まれたバイト数=%d (上限=%d)", rec.Code, body.read, maxImageUploadBytes)
	if body.read > maxImageUploadBytes {
		t.Errorf("上限を超えて読まれた: %d バイト", body.read)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// **未ログインでも読まれないこと。** 認証の判定より前に読まれるのが問題だった。
func TestBodyLimit_DoesNotBufferHugeBodyWhenUnauthenticated(t *testing.T) {
	env := newImageEnv(t)

	const size = 30 << 20
	body := &countingReader{remaining: size}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", body)
	// **Origin が要る** (ADR 0013 決定 1 の CSRF 対策)。
	// 付けないと csrfGuard が 403 で打ち切り、ここから先を検査できない。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.ContentLength = size
	// Cookie を付けない

	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	t.Logf("status=%d 読まれたバイト数=%d", rec.Code, body.read)
	if body.read > maxImageUploadBytes {
		t.Errorf("未ログインなのに %d バイト読まれた", body.read)
	}
}

// Content-Length を偽って chunked で送ってきた場合も止まること。
func TestBodyLimit_StopsChunkedBody(t *testing.T) {
	env := newImageEnv(t)

	const size = 30 << 20
	body := &countingReader{remaining: size}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", body)
	// **Origin が要る** (ADR 0013 決定 1 の CSRF 対策)。
	// 付けないと csrfGuard が 403 で打ち切り、ここから先を検査できない。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.ContentLength = -1 // chunked
	req.AddCookie(sessionCookie(env.token))

	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	t.Logf("status=%d 読まれたバイト数=%d", rec.Code, body.read)
	if body.read > maxImageUploadBytes+(1<<20) {
		t.Errorf("chunked で %d バイト読まれた", body.read)
	}
}

// **csrfGuard で弾かれる要求の本文は 1 バイトも読まないこと。**
//
// この PR で「csrfGuard は bodyLimit より前」と決めたが、
// **上の 3 件はどれも Origin を付けているので、順序を入れ替えても
// 全部緑のまま通る** (レビュー指摘)。
// 順序そのものを見る検査がここにしか無いので、1 件足す。
//
// 画像は最大 5 MiB を受けるので、逆順だと**拒否する要求のために
// 本文を読み切る**ことになる。未認証で 31 MB 読んでいた PR #27 と同じ形。
func TestCSRFGuard_RejectsBeforeReadingBody(t *testing.T) {
	env := newImageEnv(t)

	const size = 30 << 20 // 30 MiB
	body := &countingReader{remaining: size}

	// **Origin も Referer も付けない。** csrfGuard が 403 で打ち切る。
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.ContentLength = size

	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	t.Logf("status=%d 読まれたバイト数=%d", rec.Code, body.read)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if body.read != 0 {
		t.Errorf("本文を %d バイト読んでいる (csrfGuard が bodyLimit より後ろにある)",
			body.read)
	}
}
