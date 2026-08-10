package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
	userrepo "develop-experiments/apps/go-api/internal/user/domain/repository"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
)

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

// fakeProvider は Google の代わりになります。
// テストで外部 IdP を叩かないための差し替え口です (ADR 0005)。
type fakeProvider struct {
	claims *userusecase.IDTokenClaims
	err    error
}

var _ userusecase.Provider = (*fakeProvider)(nil)

func (f *fakeProvider) AuthCodeURL(state, nonce, codeVerifier string) string {
	return "https://accounts.example.com/authorize?state=" + state
}

func (f *fakeProvider) Exchange(context.Context, string, string, string) (*userusecase.IDTokenClaims, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.claims, nil
}

type fakeUserRepo struct {
	user *usermodel.User
	err  error
}

var _ userrepo.UserRepository = (*fakeUserRepo)(nil)

func (f *fakeUserRepo) Upsert(context.Context, *usermodel.User) (*usermodel.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}
func (f *fakeUserRepo) FindByID(context.Context, int64) (*usermodel.User, error) {
	return f.user, f.err
}
func (f *fakeUserRepo) FindByPublicID(context.Context, uuid.UUID) (*usermodel.User, error) {
	return f.user, f.err
}
func (f *fakeUserRepo) ListAuthorsByIDs(context.Context, []int64) ([]usermodel.Author, error) {
	return nil, f.err
}

// fakeSessionRepo は「発行済みのトークン」を 1 つだけ覚えます。
type fakeSessionRepo struct {
	liveToken usermodel.SessionToken
	owner     usermodel.SessionOwner
	deleted   []usermodel.SessionToken
}

var _ userrepo.SessionRepository = (*fakeSessionRepo)(nil)

func (f *fakeSessionRepo) Create(_ context.Context, s *usermodel.Session) (*usermodel.Session, error) {
	return s, nil
}

func (f *fakeSessionRepo) FindLive(
	_ context.Context, token usermodel.SessionToken,
) (*usermodel.AuthenticatedSession, error) {
	if token != f.liveToken || token == "" {
		return nil, apperr.ErrNotFound
	}
	return &usermodel.AuthenticatedSession{
		Session: *usermodel.ReconstructSession(token.Hash(), f.owner.ID, time.Now().Add(time.Hour), time.Now()),
		Owner:   f.owner,
	}, nil
}

func (f *fakeSessionRepo) Delete(_ context.Context, token usermodel.SessionToken) error {
	f.deleted = append(f.deleted, token)
	return nil
}
func (f *fakeSessionRepo) DeleteByUserID(context.Context, int64) (int64, error) { return 0, nil }
func (f *fakeSessionRepo) DeleteExpired(context.Context, int32) (int64, error)  { return 0, nil }

// ---------------------------------------------------------------------------
// ヘルパ
// ---------------------------------------------------------------------------

type authEnv struct {
	router   *gin.Engine
	sessions *fakeSessionRepo
	token    usermodel.SessionToken
}

// newAuthEnv は認証を有効にしたルータを組み立てます。
// authEnabled が false の場合は、設定が無い状態 (認証が 503) を再現します。
func newAuthEnv(t *testing.T, authEnabled bool) *authEnv {
	t.Helper()

	const token = usermodel.SessionToken("test-session-token")
	publicID := uuid.MustParse("01920000-0000-7000-8000-000000000001")

	sessions := &fakeSessionRepo{
		liveToken: token,
		owner: usermodel.SessionOwner{
			ID: 1, PublicID: publicID, Email: "h@example.com", DisplayName: "ホシノ",
		},
	}

	var auth *userusecase.AuthInteractor
	if authEnabled {
		user := usermodel.Reconstruct(1, publicID, "sub-1", "h@example.com", "ホシノ",
			nil, time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), nil)
		auth = userusecase.NewAuthInteractor(
			&fakeUserRepo{user: user},
			sessions,
			&fakeProvider{claims: &userusecase.IDTokenClaims{
				Subject: "sub-1", Email: "h@example.com", EmailVerified: true, Name: "ホシノ",
			}},
			func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		)
	}

	router, err := NewRouter(Deps{
		Server: NewServer(
			threadusecase.NewThreadInteractor(&fakeThreadRepo{}),
			commentusecase.NewCommentInteractor(&fakeCommentRepo{}, &fakeThreadRepo{}),
			&fakePinger{},
			auth,
			config.AuthConfig{FrontendURL: "http://localhost:3000"},
		),
		AllowedOrigins: []string{"http://localhost:3000"},
	})
	if err != nil {
		t.Fatalf("NewRouter が失敗した: %v", err)
	}

	return &authEnv{router: router, sessions: sessions, token: token}
}

func (e *authEnv) do(t *testing.T, method, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(""))
	for _, c := range cookies {
		req.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(token usermodel.SessionToken) *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: string(token)}
}

// ---------------------------------------------------------------------------
// security 宣言が実際に効いているか
// ---------------------------------------------------------------------------

// **仕様書の security が 401 として強制されること。**
//
// gin-middleware は SecurityRequirementsError を 400 に丸めるため、
// respondSpecError で振り分け直していなければここが 400 になる。
// ADR 0013 は「フロントは 401 でログイン画面へ」と決めており、
// 400 だとその分岐ができない。
func TestAuth_MissingCookieIs401(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/me")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.UNAUTHENTICATED {
		t.Errorf("code = %q, want UNAUTHENTICATED", code)
	}
}

// 無効なトークンも 401。resolveSession は弾かずに素通しし、
// security 宣言のあるエンドポイントだけが落とす形になっている。
func TestAuth_InvalidCookieIs401(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/me", sessionCookie("でたらめなトークン"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

// 有効なセッションでは 200 になり、内部 ID を含まない。
//
// 401 側だけをテストすると「常に弾く」実装でも通ってしまう。
func TestAuth_ValidCookieReturnsMe(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/me", sessionCookie(env.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.Me](t, rec)
	if got.DisplayName != "ホシノ" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	// 内部 ID (users.id = 1) が漏れていないこと。
	if strings.Contains(rec.Body.String(), `"id"`) {
		t.Errorf("内部 ID が漏れている: %s", rec.Body.String())
	}
}

// **匿名投稿は認証必須にならないこと。**
//
// OpenAPI の security は「必須」しか表現できないため、
// 投稿系には宣言していない。ここが 401 になると匿名投稿ができなくなる。
func TestAuth_AnonymousPostIsStillAllowed(t *testing.T) {
	env := newAuthEnv(t, true)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/threads",
		strings.NewReader(`{"title":"匿名のスレッド"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("匿名投稿が 401 になった (body=%s)", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 設定が無いときの振る舞い
// ---------------------------------------------------------------------------

// 認証の設定が無くても API は起動し、認証経路だけが 503 になること。
//
// 必須にすると、Google の資格情報を置くまで CI が落ちる
// (Migration Check は API を起動してスモークテストを回すため)。
func TestAuth_DisabledReturns503(t *testing.T) {
	env := newAuthEnv(t, false)

	rec := env.do(t, http.MethodGet, "/auth/google")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.UNAVAILABLE {
		t.Errorf("code = %q, want UNAVAILABLE", code)
	}
}

// 設定が無くても掲示板は読めること。認証だけを落とす設計の要点。
func TestAuth_DisabledStillServesThreads(t *testing.T) {
	env := newAuthEnv(t, false)

	rec := env.do(t, http.MethodGet, "/threads")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// ログインフロー
// ---------------------------------------------------------------------------

// ログイン開始で state / nonce / code_verifier が Cookie に載ること。
func TestStartGoogleLogin_SetsFlowCookies(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/auth/google")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}

	want := map[string]bool{stateCookieName: false, nonceCookieName: false, verifierCookieName: false}
	for _, c := range rec.Result().Cookies() {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
			if c.Value == "" {
				t.Errorf("%s が空", c.Name)
			}
			if !c.HttpOnly {
				t.Errorf("%s に HttpOnly が付いていない", c.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s が発行されていない", name)
		}
	}
}

// **state が一致しなければ 401。**
//
// これが無いと、攻撃者が用意した認可コードを被害者のブラウザで交換させられる
// (ログイン CSRF)。
func TestGoogleLoginCallback_RejectsStateMismatch(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/auth/google/callback?code=xyz&state=attacker",
		&http.Cookie{Name: stateCookieName, Value: "victim"},
		&http.Cookie{Name: nonceCookieName, Value: "n"},
		&http.Cookie{Name: verifierCookieName, Value: "v"},
	)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

// state の Cookie が無い場合も 401。
func TestGoogleLoginCallback_RejectsMissingStateCookie(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/auth/google/callback?code=xyz&state=whatever")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

// 正しい state ならセッション Cookie が発行され、フロントへ戻る。
func TestGoogleLoginCallback_IssuesSessionCookie(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/auth/google/callback?code=xyz&state=s1",
		&http.Cookie{Name: stateCookieName, Value: "s1"},
		&http.Cookie{Name: nonceCookieName, Value: "n1"},
		&http.Cookie{Name: verifierCookieName, Value: "v1"},
	)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "http://localhost:3000" {
		t.Errorf("Location = %q", loc)
	}

	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("セッション Cookie が発行されていない")
	}
	if session.Value == "" {
		t.Error("セッション Cookie が空")
	}
	if !session.HttpOnly {
		t.Error("セッション Cookie に HttpOnly が付いていない")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", session.SameSite)
	}
}

// ログアウトはセッションを消し、Cookie を破棄する。
func TestLogout_DeletesSessionAndClearsCookie(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodPost, "/auth/logout", sessionCookie(env.token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}

	if len(env.sessions.deleted) != 1 || env.sessions.deleted[0] != env.token {
		t.Errorf("削除されたトークン = %v, want [%s]", env.sessions.deleted, env.token)
	}

	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge >= 0 {
			t.Errorf("セッション Cookie が破棄されていない (MaxAge=%d)", c.MaxAge)
		}
	}
}

// ログアウトも security 宣言があるので、未ログインは 401。
func TestLogout_RequiresSession(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodPost, "/auth/logout")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}
