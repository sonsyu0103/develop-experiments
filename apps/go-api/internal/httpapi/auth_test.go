package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	commentmodel "develop-experiments/apps/go-api/internal/comment/domain/model"
	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	threadmodel "develop-experiments/apps/go-api/internal/thread/domain/model"
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
	user          *usermodel.User
	err           error
	promotedSubs  []string
	promoteResult bool
	promoteErr    error
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

// promoted は PromoteToAdmin が呼ばれた回数です。
// 「昇格させた」ことを返り値ではなく呼び出しの有無で見るため。
func (f *fakeUserRepo) PromoteToAdmin(_ context.Context, googleSub string) (bool, error) {
	f.promotedSubs = append(f.promotedSubs, googleSub)
	return f.promoteResult, f.promoteErr
}

func (f *fakeUserRepo) ListAuthorsByIDs(context.Context, []int64) ([]usermodel.Author, error) {
	return nil, f.err
}

// fakeSessionRepo は「発行済みのトークン」を 1 つだけ覚えます。
type fakeSessionRepo struct {
	liveToken usermodel.SessionToken
	owner     usermodel.SessionOwner
	deleted   []usermodel.SessionToken
	// findErr が非 nil なら FindLive がそれを返します。DB 障害の再現に使います。
	findErr error
}

var _ userrepo.SessionRepository = (*fakeSessionRepo)(nil)

func (f *fakeSessionRepo) Create(_ context.Context, s *usermodel.Session) (*usermodel.Session, error) {
	return s, nil
}

func (f *fakeSessionRepo) FindLive(
	_ context.Context, token usermodel.SessionToken,
) (*usermodel.AuthenticatedSession, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
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
	threads  *fakeThreadRepo
	comments *fakeCommentRepo
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
			nil, usermodel.RoleUser, time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), nil)
		auth = userusecase.NewAuthInteractor(
			&fakeUserRepo{user: user},
			sessions,
			&fakeProvider{claims: &userusecase.IDTokenClaims{
				Subject: "sub-1", Email: "h@example.com", EmailVerified: true, Name: "ホシノ",
			}},
			func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		)
	}

	// コメント投稿の親スレッド確認に使うので、生存しているスレッドを 1 件持たせる。
	threads := &fakeThreadRepo{
		summaries: []threadmodel.Summary{
			{Thread: *threadmodel.Reconstruct(1, "スレッド", nil, time.Unix(1, 0).UTC())},
		},
	}
	comments := &fakeCommentRepo{}

	router, err := NewRouter(Deps{
		Server: NewServer(
			threadusecase.NewThreadInteractor(threads),
			commentusecase.NewCommentInteractor(comments, threads),
			&fakePinger{},
			auth,
			config.AuthConfig{FrontendURL: "http://localhost:3000"},
		),
		AllowedOrigins: []string{"http://localhost:3000"},
	})
	if err != nil {
		t.Fatalf("NewRouter が失敗した: %v", err)
	}

	return &authEnv{
		router: router, sessions: sessions,
		threads: threads, comments: comments, token: token,
	}
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

// flowCookies はコールバックに持ち込むフロー用 Cookie 一式です。
func flowCookies(state string) []*http.Cookie {
	return []*http.Cookie{
		{Name: stateCookieName, Value: state},
		{Name: nonceCookieName, Value: "n1"},
		{Name: verifierCookieName, Value: "v1"},
	}
}

// findCookie はレスポンスから名前で Cookie を探します。無ければ nil。
func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	var found *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			found = c
		}
	}
	return found
}

// assertFlowCookiesCleared はフロー用 Cookie 3 つが破棄されたことを確かめます。
//
// **破棄はレスポンスを書き出す前に行う必要があります。**
// defer で書くとハンドラ復帰時、つまり c.Redirect / respondError が
// ヘッダを送出したあとに走るため、Set-Cookie がレスポンスに載りません。
func assertFlowCookiesCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	for _, name := range []string{stateCookieName, nonceCookieName, verifierCookieName} {
		c := findCookie(rec, name)
		if c == nil {
			t.Errorf("%s の破棄が返っていない", name)
			continue
		}
		if c.MaxAge >= 0 {
			t.Errorf("%s が破棄されていない (MaxAge=%d)", name, c.MaxAge)
		}
	}
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

// **DB 障害を 401 に化けさせない。**
//
// セッションの解決に失敗したのが「無効なセッション」なのか
// 「DB に届かなかった」のかを区別せず素通しすると、障害中は
// 全利用者が突然ログアウトされたように見え、痕跡も残らない。
func TestAuth_SessionLookupFailureIs500(t *testing.T) {
	env := newAuthEnv(t, true)
	env.sessions.findErr = errors.New("connection refused")

	rec := env.do(t, http.MethodGet, "/me", sessionCookie(env.token))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.INTERNAL {
		t.Errorf("code = %q, want INTERNAL", code)
	}
}

// 一方、Cookie が無効なだけなら従来どおり 401。
// 500 に倒しすぎると、期限切れの Cookie を持つ利用者に 500 を返してしまう。
func TestAuth_ExpiredSessionIsStill401(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/me", sessionCookie("期限切れのトークン"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
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
		flowCookies("victim")...,
	)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}

	// **攻撃を検知した経路でこそ、古い state を捨てたい。**
	// 破棄を state の照合より後ろに置くと、ここだけ 10 分残ってしまう。
	assertFlowCookiesCleared(t, rec)
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
		flowCookies("s1")...,
	)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "http://localhost:3000" {
		t.Errorf("Location = %q", loc)
	}

	session := findCookie(rec, sessionCookieName)
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

	// **MaxAge を検査する。** 値と属性だけを見ていると、
	// 発行と同時に失効する Cookie (MaxAge が負) を見逃す。
	// このテストはインタラクタの時計を 2023 年に固定しているので、
	// 壁時計との差で計算する実装ではここが負になる。
	wantMaxAge := int(usermodel.DefaultSessionTTL.Seconds())
	if session.MaxAge != wantMaxAge {
		t.Errorf("MaxAge = %d, want %d", session.MaxAge, wantMaxAge)
	}

	// 使い終わったフロー用 Cookie は残さない。残すと再利用の余地ができる。
	assertFlowCookiesCleared(t, rec)
}

// **同意画面で拒否されたら、API のエラー JSON をブラウザに見せない。**
//
// Google は code を付けず error だけを返す。code を必須にしていると
// 仕様検証が 400 で弾き、生の JSON がアドレスバーの下に表示される。
func TestGoogleLoginCallback_UserDeniedRedirectsToFrontend(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/auth/google/callback?error=access_denied&state=s1",
		flowCookies("s1")...,
	)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "http://localhost:3000?login_error=access_denied" {
		t.Errorf("Location = %q", loc)
	}
	assertFlowCookiesCleared(t, rec)

	// 拒否されただけなのでセッションは発行しない。
	if c := findCookie(rec, sessionCookieName); c != nil && c.MaxAge > 0 {
		t.Errorf("拒否したのにセッションが発行された: %+v", c)
	}
}

// IdP が返した文字列をそのままリダイレクト先へ載せない。
// フロントでそのまま描画されると反射型 XSS の入口になる。
func TestGoogleLoginCallback_SanitizesErrorParam(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet,
		"/auth/google/callback?error=%3Cscript%3Ealert(1)%3C/script%3E&state=s1",
		flowCookies("s1")...,
	)
	if loc := rec.Header().Get("Location"); loc != "http://localhost:3000?login_error=login_failed" {
		t.Errorf("Location = %q, want 既知の識別子に丸める", loc)
	}
}

// code も error も無い場合は 401。仕様検証を緩めた分をここで受け止める。
func TestGoogleLoginCallback_RejectsMissingCode(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/auth/google/callback?state=s1", flowCookies("s1")...)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	assertFlowCookiesCleared(t, rec)
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

// ---------------------------------------------------------------------------
// 投稿者の紐付け (ADR 0005 決定 2 / ADR 0014)
// ---------------------------------------------------------------------------

func postJSON(t *testing.T, e *authEnv, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// **ログイン中の投稿には投稿者が紐付く。**
//
// ここが繋がっていないと、ログインしていても全部が匿名投稿になる。
// 返り値ではなくリポジトリが受け取った値を見ているのは、
// 「詰め替えは正しいが author_id を渡していない」を検出するため。
func TestCreateThread_AttachesAuthorWhenLoggedIn(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := postJSON(t, env, "/threads", `{"title":"ログインして立てたスレッド"}`,
		sessionCookie(env.token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	if env.threads.created == nil {
		t.Fatal("スレッドが保存されていない")
	}
	// fakeSessionRepo の owner.ID が 1。
	if got := env.threads.created.AuthorID; got == nil || *got != 1 {
		t.Errorf("AuthorID = %v, want 1", got)
	}

	got := decodeJSON[oapigen.Thread](t, rec)
	if got.Author == nil {
		t.Fatal("レスポンスに author が無い")
	}
	if got.Author.DisplayName != "ホシノ" {
		t.Errorf("author.displayName = %q", got.Author.DisplayName)
	}
	if got.Author.Withdrawn {
		t.Error("withdrawn = true, want false")
	}
}

// **匿名投稿は匿名のまま。** author_id を勝手に埋めない。
func TestCreateThread_AnonymousHasNoAuthor(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := postJSON(t, env, "/threads", `{"title":"匿名で立てたスレッド"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	if env.threads.created == nil {
		t.Fatal("スレッドが保存されていない")
	}
	if got := env.threads.created.AuthorID; got != nil {
		t.Errorf("AuthorID = %d, want nil (匿名)", *got)
	}
	if got := decodeJSON[oapigen.Thread](t, rec); got.Author != nil {
		t.Errorf("author = %+v, want null", *got.Author)
	}
}

// コメントも同じ。あわせて、**ログイン中は authorName が捨てられる**ことを見る。
//
// 保存されると「投稿時点の表示名」が残り、表示名の変更が
// 過去の投稿に反映されなくなる (ADR 0014 はそれを選んでいない)。
func TestCreateComment_AttachesAuthorAndDiscardsAuthorName(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := postJSON(t, env, "/threads/1/comments",
		`{"authorName":"別人を名乗る","body":"ログインして書いたコメント"}`,
		sessionCookie(env.token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	if env.comments.created == nil {
		t.Fatal("コメントが保存されていない")
	}
	if got := env.comments.created.AuthorID; got == nil || *got != 1 {
		t.Errorf("AuthorID = %v, want 1", got)
	}
	if got := env.comments.created.AuthorName; got != commentmodel.DefaultAuthorName {
		t.Errorf("AuthorName = %q, want %q (ログイン中は捨てる)", got, commentmodel.DefaultAuthorName)
	}

	got := decodeJSON[oapigen.Comment](t, rec)
	if got.Author == nil || got.Author.DisplayName != "ホシノ" {
		t.Errorf("author = %+v, want ホシノ", got.Author)
	}
}

// 匿名のコメントは投稿者名がそのまま残り、author は null になる。
func TestCreateComment_AnonymousKeepsAuthorName(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := postJSON(t, env, "/threads/1/comments",
		`{"authorName":"通りすがり","body":"匿名のコメント"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	if got := env.comments.created.AuthorName; got != "通りすがり" {
		t.Errorf("AuthorName = %q, want 通りすがり", got)
	}
	if got := env.comments.created.AuthorID; got != nil {
		t.Errorf("AuthorID = %d, want nil (匿名)", *got)
	}
	if got := decodeJSON[oapigen.Comment](t, rec); got.Author != nil {
		t.Errorf("author = %+v, want null", *got.Author)
	}
}
