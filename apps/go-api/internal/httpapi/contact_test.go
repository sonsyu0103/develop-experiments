package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"

	contactmodel "develop-experiments/apps/go-api/internal/contact/domain/model"
	contactrepo "develop-experiments/apps/go-api/internal/contact/domain/repository"
	contactusecase "develop-experiments/apps/go-api/internal/contact/usecase"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	moderationusecase "develop-experiments/apps/go-api/internal/moderation/usecase"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
	"develop-experiments/apps/go-api/internal/viewcount"
)

// fakeContactRepo は受付の検査に使うフェイクです。
//
// **送信側 (ClaimPending 以降) は呼ばれません。** ハンドラは受付しか
// 呼ばないので、呼ばれたら結線を間違えています —— リクエストの中で
// メールを送る実装になっていないことを、ここでも見張ります。
type fakeContactRepo struct {
	mu    sync.Mutex
	saved []contactmodel.Submission
	// recent は CountRecent が返す件数です。レート制限の検査で使います。
	recent int64
	// t が非 nil なら、送信側のメソッドが呼ばれたときに失敗させます。
	t *testing.T
}

var _ contactrepo.ContactRepository = (*fakeContactRepo)(nil)

func newFakeContactRepo() *fakeContactRepo { return &fakeContactRepo{} }

func (f *fakeContactRepo) Save(
	_ context.Context, s contactmodel.Submission,
) (*contactmodel.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, s)
	return &contactmodel.Message{
		ID:        int64(len(f.saved)),
		UserID:    s.UserID,
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}, nil
}

func (f *fakeContactRepo) CountRecent(context.Context, netip.Addr, time.Duration) (int64, error) {
	return f.recent, nil
}

func (f *fakeContactRepo) ClaimPending(
	context.Context, time.Duration, int32,
) ([]contactmodel.Message, error) {
	f.dispatchCalled("ClaimPending")
	return nil, nil
}

func (f *fakeContactRepo) MarkSent(context.Context, int64) error {
	f.dispatchCalled("MarkSent")
	return nil
}

func (f *fakeContactRepo) Reschedule(context.Context, int64, time.Duration, string) error {
	f.dispatchCalled("Reschedule")
	return nil
}

func (f *fakeContactRepo) Fail(context.Context, int64, string) error {
	f.dispatchCalled("Fail")
	return nil
}

func (f *fakeContactRepo) OldestPending(context.Context) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (f *fakeContactRepo) ScrubClientIPs(context.Context, time.Duration, int32) (int64, error) {
	return 0, nil
}

// dispatchCalled は「HTTP の経路から送信側が呼ばれた」ことを報告します。
func (f *fakeContactRepo) dispatchCalled(name string) {
	if f.t != nil {
		f.t.Errorf("HTTP のリクエストから送信側 (%s) が呼ばれた", name)
	}
}

// contactEnv は問い合わせの検査に使う最小の環境です。
//
// **フェイクの受付だけを差し替えます。** 他は newTestEnv と同じ結線で、
// 仕様検証ミドルウェアと csrfGuard を通した経路を見ます ——
// ハンドラを直接呼ぶと、仕様書の maxLength も Origin の検査も通りません。
type contactEnv struct {
	*testEnv
	repo *fakeContactRepo
}

func newContactEnv(t *testing.T) *contactEnv {
	t.Helper()

	repo := newFakeContactRepo()
	// **送信側が呼ばれたら失敗させます。** ハンドラがメールを送る実装に
	// なっていないことを、テストの側から見張ります。
	repo.t = t

	router, err := NewRouter(Deps{
		Server: NewServer(
			threadusecase.NewThreadInteractor(&fakeThreadRepo{}, nil),
			commentusecase.NewCommentInteractor(&fakeCommentRepo{}, &fakeThreadRepo{}, nil),
			&fakePinger{},
			userusecase.NewSessionInteractor(&fakeSessionRepo{}, nil),
			nil,
			nil,
			moderationusecase.NewInteractor(newFakeModerationRepo()),
			moderationusecase.NewReportInteractor(newFakeReportRepo(), newFakeReportRepo()),
			contactusecase.NewInteractor(repo),
			viewcount.New(),
			config.AuthConfig{}),
		AllowedOrigins: []string{testOrigin},
	})
	if err != nil {
		t.Fatalf("NewRouter が失敗した: %v", err)
	}

	return &contactEnv{testEnv: &testEnv{router: router}, repo: repo}
}

// **受理は 202 であること** (ADR 0008 決定 1)。
//
// 200 は「送信完了」を含意します。この時点で終わっているのは受理までです。
func TestCreateContact_Accepted(t *testing.T) {
	t.Parallel()

	env := newContactEnv(t)
	res := env.do(t, http.MethodPost, "/contact", `{
		"name": "ホシノ",
		"email": "hoshino@example.com",
		"subject": "ログインできません",
		"body": "画面が戻ってきます。"
	}`)

	if res.Code != http.StatusAccepted {
		t.Fatalf("ステータス = %d, want 202: %s", res.Code, res.Body.String())
	}

	got := decodeJSON[oapigen.ContactAccepted](t, res)
	if got.Status != oapigen.Accepted {
		t.Errorf("status = %q, want accepted", got.Status)
	}
	if got.ReceivedAt.IsZero() {
		t.Error("receivedAt が空")
	}

	if len(env.repo.saved) != 1 {
		t.Fatalf("保存件数 = %d, want 1", len(env.repo.saved))
	}
	// **匿名で受け付けること** (Cookie を送っていない)。
	if env.repo.saved[0].UserID != nil {
		t.Error("匿名なのに投稿者が入っている")
	}
	// **送信元が記録されること。** レート制限の集計に使う。
	if !env.repo.saved[0].ClientIP.IsValid() {
		t.Error("送信元が記録されていない")
	}
}

// **honeypot は 202 のまま破棄されること** (ADR 0008 決定 4)。
//
// 400 にすると「この項目が引き金だ」とボット側に教えることになります。
func TestCreateContact_HoneypotLooksIdentical(t *testing.T) {
	t.Parallel()

	env := newContactEnv(t)
	res := env.do(t, http.MethodPost, "/contact", `{
		"name": "ホシノ",
		"email": "hoshino@example.com",
		"subject": "件名",
		"body": "本文",
		"website": "http://spam.example.com"
	}`)

	if res.Code != http.StatusAccepted {
		t.Fatalf("ステータス = %d, want 202 (成功と区別できてしまう)", res.Code)
	}
	if len(env.repo.saved) != 0 {
		t.Errorf("破棄されていない: %d 件保存された", len(env.repo.saved))
	}
}

// **レート制限は 429 / RESOURCE_EXHAUSTED であること**
// (docs/adr/0013-http-defense.md 決定 3 の表)。
func TestCreateContact_RateLimited(t *testing.T) {
	t.Parallel()

	env := newContactEnv(t)
	env.repo.recent = 1000

	res := env.do(t, http.MethodPost, "/contact", `{
		"name": "ホシノ",
		"email": "hoshino@example.com",
		"subject": "件名",
		"body": "本文"
	}`)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("ステータス = %d, want 429: %s", res.Code, res.Body.String())
	}
	if code := decodeError(t, res).Error.Code; code != oapigen.RESOURCEEXHAUSTED {
		t.Errorf("code = %q, want RESOURCE_EXHAUSTED", code)
	}

	// **上限や窓を応答に載せないこと。** 「何件までなら通るか」は
	// 攻撃側にだけ有用な情報になります。
	if body := res.Body.String(); strings.Contains(body, "1000") || strings.Contains(body, "3600") {
		t.Errorf("上限や窓が漏れている: %s", body)
	}
}

// **仕様書の maxLength が実際に強制されること。**
//
// 検証ミドルウェアが仕様書そのものを使うので、ここが通ると
// 「ドキュメント上の飾り」になっていないことが確かめられます。
func TestCreateContact_SpecValidation(t *testing.T) {
	t.Parallel()

	env := newContactEnv(t)

	cases := map[string]string{
		// body が無い。
		"missing body": `{"name":"ホシノ","email":"a@example.com","subject":"件名"}`,
		// 件名が 200 文字を超える (201 文字)。
		"subject too long": `{"name":"ホシノ","email":"a@example.com","subject":"` +
			strings.Repeat("x", 201) + `","body":"本文"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			res := env.do(t, http.MethodPost, "/contact", body)
			if res.Code != http.StatusBadRequest {
				t.Errorf("ステータス = %d, want 400: %s", res.Code, res.Body.String())
			}
		})
	}
}

// **状態変更なので Origin の検査が効くこと** (ADR 0013 決定 1)。
//
// 匿名で叩ける口ほど、CSRF の対象として意味を持ちます。
func TestCreateContact_RejectsCrossOrigin(t *testing.T) {
	t.Parallel()

	env := newContactEnv(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/contact",
		strings.NewReader(`{"name":"ホシノ","email":"a@example.com","subject":"件名","body":"本文"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	res := httptest.NewRecorder()
	env.router.ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("ステータス = %d, want 403", res.Code)
	}
	if len(env.repo.saved) != 0 {
		t.Error("弾いたのに保存されている")
	}
}

// newTestContactInteractor は他のテストが NewServer へ渡す受付です。
//
// **問い合わせの受付は常に結線されます** (メールの設定に依存しません)。
// nil を渡せないので、どのテストもこれを使います。
func newTestContactInteractor() *contactusecase.Interactor {
	return contactusecase.NewInteractor(newFakeContactRepo())
}
