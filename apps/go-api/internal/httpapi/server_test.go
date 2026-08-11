package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	commentmodel "develop-experiments/apps/go-api/internal/comment/domain/model"
	commentrepo "develop-experiments/apps/go-api/internal/comment/domain/repository"
	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	"develop-experiments/apps/go-api/internal/pagination"
	threadmodel "develop-experiments/apps/go-api/internal/thread/domain/model"
	threadrepo "develop-experiments/apps/go-api/internal/thread/domain/repository"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

type fakeThreadRepo struct {
	summaries []threadmodel.Summary
	err       error
	// created は Create に渡された値です。
	// 「投稿者が実際に紐付いたか」は、返り値ではなく渡された値で見ます。
	created *threadmodel.Thread
}

var _ threadrepo.ThreadRepository = (*fakeThreadRepo)(nil)

func (f *fakeThreadRepo) ListSummaries(context.Context, pagination.Page) ([]threadmodel.Summary, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.summaries, nil
}

func (f *fakeThreadRepo) FindSummaryByID(_ context.Context, id int64) (*threadmodel.Summary, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, s := range f.summaries {
		if s.ID == id {
			return &s, nil
		}
	}
	return nil, fmt.Errorf("fake: %w", apperr.ErrNotFound)
}

func (f *fakeThreadRepo) Create(_ context.Context, th *threadmodel.Thread) (*threadmodel.Thread, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.created = th
	// 実装では LEFT JOIN users が投稿者を解決する。
	// フェイクでも同じ形にしないと、作成レスポンスの詰め替えを検証できない。
	return threadmodel.Reconstruct(99, th.Title, fakeAuthorFor(th.AuthorID), time.Unix(0, 0).UTC()), nil
}

// fakeAuthorFor は author_id から投稿者を解決する DB 側の振る舞いを真似ます。
func fakeAuthorFor(authorID *int64) *threadmodel.Author {
	if authorID == nil {
		return nil
	}
	return threadmodel.NewAuthor(fakeAuthorPublicID, "ホシノ", nil, nil)
}

// fakeAuthorPublicID はフェイクが返す投稿者の公開 ID です。
var fakeAuthorPublicID = uuid.MustParse("01920000-0000-7000-8000-000000000001")

// Exists は summaries に含まれるスレッドだけを「生存している」とみなす。
// 論理削除されたスレッドは summaries から除かれる想定。
func (f *fakeThreadRepo) Exists(_ context.Context, id int64) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	for _, s := range f.summaries {
		if s.ID == id {
			return true, nil
		}
	}
	return false, nil
}

type fakeCommentRepo struct {
	comments []commentmodel.Comment
	err      error
	// created は Create に渡された値です。
	created *commentmodel.Comment
}

var _ commentrepo.CommentRepository = (*fakeCommentRepo)(nil)

func (f *fakeCommentRepo) ListByThreadID(
	context.Context, int64, pagination.Page,
) ([]commentmodel.Comment, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.comments, nil
}

func (f *fakeCommentRepo) Create(_ context.Context, c *commentmodel.Comment) (*commentmodel.Comment, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.created = c

	var author *commentmodel.Author
	if c.AuthorID != nil {
		author = commentmodel.NewAuthor(fakeAuthorPublicID, "ホシノ", nil, nil)
	}
	return commentmodel.Reconstruct(7, c.ThreadID, c.AuthorName, author, c.Body, time.Unix(0, 0).UTC()), nil
}

func (f *fakeCommentRepo) SoftDelete(context.Context, int64, int64) error { return f.err }

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// ---------------------------------------------------------------------------
// ヘルパ
// ---------------------------------------------------------------------------

type testEnv struct {
	router   *gin.Engine
	threads  *fakeThreadRepo
	comments *fakeCommentRepo
	pinger   *fakePinger
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	threads := &fakeThreadRepo{
		summaries: []threadmodel.Summary{
			{Thread: *threadmodel.Reconstruct(2, "2 番目のスレッド", nil, time.Unix(2, 0).UTC()), CommentCount: 5},
			{Thread: *threadmodel.Reconstruct(1, "1 番目のスレッド", nil, time.Unix(1, 0).UTC()), CommentCount: 0},
		},
	}
	comments := &fakeCommentRepo{
		comments: []commentmodel.Comment{
			*commentmodel.Reconstruct(10, 2, "ホシノ", nil, "ふぁ〜", time.Unix(3, 0).UTC()),
		},
	}
	pinger := &fakePinger{}

	router, err := NewRouter(Deps{
		Server: NewServer(
			threadusecase.NewThreadInteractor(threads),
			commentusecase.NewCommentInteractor(comments, threads),
			pinger,
			nil,
			config.AuthConfig{},
		),
		AllowedOrigins: []string{"http://localhost:3000"},
	})
	if err != nil {
		t.Fatalf("NewRouter が失敗した: %v", err)
	}

	return &testEnv{router: router, threads: threads, comments: comments, pinger: pinger}
}

func (e *testEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) oapigen.Error {
	t.Helper()

	var got oapigen.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("エラーレスポンスの JSON 解析に失敗した: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var got T
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("JSON 解析に失敗した: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

// ---------------------------------------------------------------------------
// 正常系
// ---------------------------------------------------------------------------

func TestListThreads(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/threads", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.ThreadList](t, rec)
	if len(got.Threads) != 2 {
		t.Fatalf("件数 = %d, want 2", len(got.Threads))
	}
	if got.Threads[0].Id != 2 || got.Threads[0].CommentCount != 5 {
		t.Errorf("threads[0] = %+v", got.Threads[0])
	}
	// 2 件 < size(既定 20) なので次ページはない。
	if got.NextCursor != nil {
		t.Errorf("NextCursor = %v, want nil", *got.NextCursor)
	}
}

func TestListThreads_EmptyIsJSONArrayNotNull(t *testing.T) {
	env := newTestEnv(t)
	env.threads.summaries = nil

	rec := env.do(t, http.MethodGet, "/threads", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// フロントで threads.map() が落ちないよう、null ではなく [] を返す。
	if !strings.Contains(rec.Body.String(), `"threads":[]`) {
		t.Errorf("body = %s, want \"threads\":[] を含む", rec.Body.String())
	}
}

func TestGetThread(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/threads/2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.Thread](t, rec)
	if got.Id != 2 || got.Title != "2 番目のスレッド" {
		t.Errorf("got = %+v", got)
	}
}

func TestCreateThread(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodPost, "/threads", `{"title":"新スレッド"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.Thread](t, rec)
	if got.Title != "新スレッド" || got.Id != 99 {
		t.Errorf("got = %+v", got)
	}
}

func TestListComments(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/threads/2/comments", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.CommentList](t, rec)
	if len(got.Comments) != 1 || got.Comments[0].Body != "ふぁ〜" {
		t.Errorf("got = %+v", got.Comments)
	}
}

func TestCreateComment(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodPost, "/threads/2/comments",
		`{"authorName":"先生","body":"おはよう"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.Comment](t, rec)
	if got.AuthorName != "先生" || got.Body != "おはよう" || got.ThreadId != 2 {
		t.Errorf("got = %+v", got)
	}
}

func TestCreateComment_AuthorNameDefaults(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodPost, "/threads/2/comments", `{"body":"名無しで投稿"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.Comment](t, rec)
	if got.AuthorName != commentmodel.DefaultAuthorName {
		t.Errorf("AuthorName = %q, want %q", got.AuthorName, commentmodel.DefaultAuthorName)
	}
}

// ---------------------------------------------------------------------------
// 仕様書によるリクエスト検証
// ---------------------------------------------------------------------------
// api/openapi.yaml に書いた制約が、ドキュメント上の記述にとどまらず
// 実際に強制されていることを確認する。
// これらは全てハンドラに到達する前にミドルウェアが弾いている。

func TestSpecValidation_RejectsInvalidRequests(t *testing.T) {
	longTitle := strings.Repeat("あ", threadmodel.TitleMaxLength+1)
	longBody := strings.Repeat("あ", commentmodel.BodyMaxLength+1)
	longAuthor := strings.Repeat("あ", commentmodel.AuthorNameMaxLength+1)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		// パラメータの型と範囲 (parameters の schema)
		{"threadId が非数値", http.MethodGet, "/threads/abc", ""},
		{"threadId が 0 (minimum: 1 違反)", http.MethodGet, "/threads/0", ""},
		{"threadId が負", http.MethodGet, "/threads/-1", ""},
		{"size が上限超過 (maximum: 100 違反)", http.MethodGet, "/threads?size=101", ""},
		{"size が 0 (minimum: 1 違反)", http.MethodGet, "/threads?size=0", ""},
		{"size が非数値", http.MethodGet, "/threads?size=abc", ""},
		// cursor は不透明トークン (base64url) なので、
		// 仕様書で強制できるのは文字集合と長さまで。
		// 「復号できるか」はミドルウェアではなくハンドラ側の判定になる
		// (TestListThreads_MalformedCursorIsBadRequest)。
		{"cursor に使えない文字 (pattern 違反)", http.MethodGet, "/threads?cursor=abc.def", ""},
		{"cursor が長すぎる (maxLength: 256 違反)", http.MethodGet,
			"/threads?cursor=" + strings.Repeat("A", 257), ""},

		// リクエストボディ (requestBody の schema)
		{"title が空 (minLength: 1 違反)", http.MethodPost, "/threads", `{"title":""}`},
		{"title 未指定 (required 違反)", http.MethodPost, "/threads", `{}`},
		{"title が長すぎる (maxLength 違反)", http.MethodPost, "/threads",
			fmt.Sprintf(`{"title":%q}`, longTitle)},
		{"title の型違い", http.MethodPost, "/threads", `{"title":123}`},
		{"body 未指定 (required 違反)", http.MethodPost, "/threads/2/comments", `{}`},
		{"body が空 (minLength: 1 違反)", http.MethodPost, "/threads/2/comments", `{"body":""}`},
		{"body が長すぎる (maxLength 違反)", http.MethodPost, "/threads/2/comments",
			fmt.Sprintf(`{"body":%q}`, longBody)},
		{"authorName が長すぎる (maxLength 違反)", http.MethodPost, "/threads/2/comments",
			fmt.Sprintf(`{"authorName":%q,"body":"本文"}`, longAuthor)},
		{"JSON が壊れている", http.MethodPost, "/threads", `{"title":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)

			rec := env.do(t, tt.method, tt.path, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if code := decodeError(t, rec).Error.Code; code != oapigen.INVALIDARGUMENT {
				t.Errorf("code = %q, want INVALID_ARGUMENT", code)
			}
		})
	}
}

// 文字集合と長さは満たすが復号できないカーソルは、400 になる。
//
// 仕様書に書けるのは「base64url の文字集合に収まっていること」までなので、
// ここは検証ミドルウェアを通り抜けてハンドラに届く。
// 素通りさせると、壊れたトークンが先頭ページ扱いになり、
// クライアントは「ページ送りしたのに 1 ページ目が返る」無限ループに入る。
func TestListThreads_MalformedCursorIsBadRequest(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/threads?cursor=notAToken", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.INVALIDARGUMENT {
		t.Errorf("code = %q, want INVALID_ARGUMENT", code)
	}
}

// 正しく発行したトークンは受け付ける。
// 上の 400 系だけだと「常に弾いている」実装でもテストが通ってしまう。
func TestListThreads_ValidCursorIsAccepted(t *testing.T) {
	env := newTestEnv(t)

	token, err := pagination.NewCursor(2).Encode()
	if err != nil {
		t.Fatalf("カーソルの符号化が失敗した: %v", err)
	}

	rec := env.do(t, http.MethodGet, "/threads?cursor="+token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// 空の cursor は「省略」と同じく先頭ページになる。
//
// 仕様書の pattern が空文字を弾く形になっていると、クライアントが素直に
// 「トークンが無ければ空文字」として ?cursor= を組み立てたときに、
// 初回ロードだけが 400 になる。
// pagination 側は空文字を先頭ページとして扱うので、
// 仕様書とハンドラのどちらか片方だけを直すとここがずれる。
func TestListThreads_EmptyCursorIsFirstPage(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/threads?cursor=", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.ThreadList](t, rec)
	if len(got.Threads) != 2 {
		t.Errorf("件数 = %d, want 2 (先頭ページと同じ結果)", len(got.Threads))
	}
}

// 仕様書に無いパスは、ハンドラを書くまでもなく 404 になる。
func TestSpecValidation_UnknownPathIs404(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/threads/2/likes", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.NOTFOUND {
		t.Errorf("code = %q, want NOT_FOUND", code)
	}
}

// 定義済みパスに未定義のメソッドを投げた場合は 405。
//
// 検証ミドルウェアはメソッド不一致も 400 に丸めてしまうため、
// respondSpecError で 405 に振り分け直している。
func TestSpecValidation_UndefinedMethodIs405(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodDelete, "/threads/2", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.METHODNOTALLOWED {
		t.Errorf("code = %q, want METHOD_NOT_ALLOWED", code)
	}
}

// ---------------------------------------------------------------------------
// エラーの翻訳
// ---------------------------------------------------------------------------

func TestGetThread_NotFound(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/threads/404", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.NOTFOUND {
		t.Errorf("code = %q, want NOT_FOUND", code)
	}
}

// スレッドが存在しなければ「コメント 0 件」ではなく 404 を返す。
//
// 存在確認を省くと、存在しない ID に対して 200 {"comments":[]} を返してしまい、
// クライアントは「スレッドはあるがコメントが無い」と誤認する。
func TestListComments_ThreadNotFound(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/threads/999/comments", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.NOTFOUND {
		t.Errorf("code = %q, want NOT_FOUND", code)
	}
}

// 論理削除されたスレッドの中身は読めてはいけない。
// threads は soft delete なので行自体は残っており、
// 存在確認を怠ると「削除したはずの内容が API から見える」状態になる。
func TestListComments_SoftDeletedThreadIsHidden(t *testing.T) {
	env := newTestEnv(t)
	// スレッド 2 が論理削除された状況を再現する。
	env.threads.summaries = env.threads.summaries[1:]

	rec := env.do(t, http.MethodGet, "/threads/2/comments", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (削除済みスレッドの中身が読める)", rec.Code)
	}
}

// 親スレッドが存在しない場合、リポジトリ層が外部キー違反を
// apperr.ErrNotFound に翻訳し、ハンドラが 404 にする。
func TestCreateComment_ParentThreadMissing(t *testing.T) {
	env := newTestEnv(t)
	env.comments.err = fmt.Errorf("fk violation: %w", apperr.ErrNotFound)

	rec := env.do(t, http.MethodPost, "/threads/999/comments", `{"body":"本文"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// 空白のみの本文は仕様書の minLength では弾けない (長さ 3 のため)。
// ドメイン層の TrimSpace 後の検証が最後の砦になる。
func TestCreateComment_WhitespaceOnlyBodyRejectedByDomain(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodPost, "/threads/2/comments", `{"body":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// 直列化失敗は 409 にマップされ、クライアントに再試行を促す。
func TestConflictMapsTo409(t *testing.T) {
	env := newTestEnv(t)
	env.comments.err = fmt.Errorf("serialization failure: %w", apperr.ErrConflict)

	rec := env.do(t, http.MethodPost, "/threads/2/comments", `{"body":"本文"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.CONFLICT {
		t.Errorf("code = %q, want CONFLICT", code)
	}
}

// 500 のときに内部エラーの詳細を漏らさないこと。
// SQL 文やテーブル名が本文に出ると、攻撃の手がかりになる。
func TestInternalErrorDoesNotLeakDetails(t *testing.T) {
	env := newTestEnv(t)
	secret := `pq: relation "threads" does not exist`
	env.threads.err = errors.New(secret)

	rec := env.do(t, http.MethodGet, "/threads", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "does not exist") {
		t.Errorf("内部エラーの詳細が漏れている: %s", rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.INTERNAL {
		t.Errorf("code = %q, want INTERNAL", code)
	}
}

// ---------------------------------------------------------------------------
// health / CORS
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	env := newTestEnv(t)
	// liveness は DB の状態に影響されない。
	env.pinger.err = errors.New("db down")

	rec := env.do(t, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (DB 不調でも liveness は 200)", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	t.Run("DB が正常", func(t *testing.T) {
		env := newTestEnv(t)

		rec := env.do(t, http.MethodGet, "/readyz", "")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("DB が不調", func(t *testing.T) {
		env := newTestEnv(t)
		env.pinger.err = errors.New("db down")

		rec := env.do(t, http.MethodGet, "/readyz", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		got := decodeJSON[oapigen.HealthStatus](t, rec)
		if got.Reason == nil || *got.Reason != "database" {
			t.Errorf("Reason = %v, want \"database\"", got.Reason)
		}
	})
}

func TestCORS(t *testing.T) {
	t.Run("許可オリジンにはヘッダを返す", func(t *testing.T) {
		env := newTestEnv(t)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/threads", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
			t.Errorf("Access-Control-Allow-Origin = %q, want http://localhost:3000", got)
		}
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("Vary = %q, want Origin を含む", got)
		}
		// **セッションは Cookie で運ぶ (ADR 0005)。**
		// これが無いと、ブラウザは credentials 付きの要求への応答を
		// JavaScript に渡さない。Cookie 自体は送られるのでサーバ側は
		// 正常に見え、フロントだけが失敗する。
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
		}
	})

	t.Run("未許可オリジンにはヘッダを返さない", func(t *testing.T) {
		env := newTestEnv(t)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/threads", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want 空 (未許可オリジン)", got)
		}
		// Allow-Credentials だけが漏れると、許可オリジンの判定を
		// 素通ししたときに Cookie 付きの要求が通る余地ができる。
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want 空 (未許可オリジン)", got)
		}
		// 許可ヘッダが無い応答にも Vary は必要。
		// これが無いと共有キャッシュがオリジン非依存として保存し、
		// 許可オリジンからのリクエストにも使い回してしまう。
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("Vary = %q, want Origin を含む (未許可オリジンでも必要)", got)
		}
	})

	t.Run("Origin ヘッダが無くても Vary は付く", func(t *testing.T) {
		env := newTestEnv(t)

		rec := env.do(t, http.MethodGet, "/threads", "")
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("Vary = %q, want Origin を含む", got)
		}
	})

	// プリフライトは仕様書に定義されていないメソッド (OPTIONS) だが、
	// CORS ミドルウェアが検証ミドルウェアより前で応答を打ち切る。
	t.Run("プリフライトは 204", func(t *testing.T) {
		env := newTestEnv(t)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/threads", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
			t.Errorf("Access-Control-Allow-Methods = %q, want POST を含む", got)
		}
	})
}

// **退会した投稿者は、一覧で公開 ID が出ない。**
//
// 表示名の差し替えはドメイン層 (model.NewAuthor) の責務だが、
// 詰め替えが 1 段でも増えると落としやすい。API の出力側で固定しておく。
func TestListThreads_WithdrawnAuthor(t *testing.T) {
	env := newTestEnv(t)

	deletedAt := time.Unix(1_700_000_000, 0).UTC()
	avatar := "https://example.com/a.png"
	active := threadmodel.NewAuthor(fakeAuthorPublicID, "ホシノ", &avatar, nil)
	withdrawn := threadmodel.NewAuthor(fakeAuthorPublicID, "やめた人", &avatar, &deletedAt)

	env.threads.summaries = []threadmodel.Summary{
		{Thread: *threadmodel.Reconstruct(3, "退会者のスレッド", withdrawn, time.Unix(3, 0).UTC())},
		{Thread: *threadmodel.Reconstruct(2, "在籍者のスレッド", active, time.Unix(2, 0).UTC())},
		{Thread: *threadmodel.Reconstruct(1, "匿名のスレッド", nil, time.Unix(1, 0).UTC())},
	}

	rec := env.do(t, http.MethodGet, "/threads", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.ThreadList](t, rec)
	if len(got.Threads) != 3 {
		t.Fatalf("threads = %d 件, want 3", len(got.Threads))
	}

	// 退会者。
	if a := got.Threads[0].Author; a == nil {
		t.Error("退会者の author が null になっている (匿名と区別できない)")
	} else {
		if !a.Withdrawn {
			t.Error("withdrawn = false, want true")
		}
		if a.DisplayName != threadmodel.WithdrawnDisplayName {
			t.Errorf("displayName = %q, want %q", a.DisplayName, threadmodel.WithdrawnDisplayName)
		}
		if a.PublicId != nil {
			t.Errorf("publicId = %v, want null (退会者の識別子は返さない)", *a.PublicId)
		}
		if a.AvatarUrl != nil {
			t.Errorf("avatarUrl = %q, want null", *a.AvatarUrl)
		}
	}

	// 在籍者。退会側だけを見ると「常に伏せる」実装でも通る。
	if a := got.Threads[1].Author; a == nil {
		t.Error("在籍者の author が null になっている")
	} else if a.PublicId == nil || *a.PublicId != fakeAuthorPublicID {
		t.Errorf("publicId = %v, want %v", a.PublicId, fakeAuthorPublicID)
	}

	// 匿名投稿。**LEFT ではなく INNER で結合すると、ここが一覧から消える。**
	if got.Threads[2].Author != nil {
		t.Errorf("匿名スレッドに author が付いている: %+v", *got.Threads[2].Author)
	}
}
