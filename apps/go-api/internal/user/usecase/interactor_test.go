package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/user/domain/model"
	"develop-experiments/apps/go-api/internal/user/domain/repository"
)

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

// fakeProvider は Google の代わりです。
// テストで外部 IdP を叩かないための差し替え口になります (ADR 0005)。
type fakeProvider struct {
	claims *IDTokenClaims
	err    error

	gotState    string
	gotNonce    string
	gotVerifier string
	gotCode     string
}

var _ Provider = (*fakeProvider)(nil)

func (f *fakeProvider) AuthCodeURL(state, nonce, codeVerifier string) string {
	f.gotState, f.gotNonce, f.gotVerifier = state, nonce, codeVerifier
	return "https://accounts.example.com/authorize"
}

func (f *fakeProvider) Exchange(_ context.Context, code, codeVerifier, nonce string) (*IDTokenClaims, error) {
	f.gotCode, f.gotVerifier, f.gotNonce = code, codeVerifier, nonce
	if f.err != nil {
		return nil, f.err
	}
	return f.claims, nil
}

type fakeUserRepo struct {
	user *model.User
	err  error

	upserted      *model.User
	promotedSubs  []string
	promoteResult bool
	promoteErr    error
}

var _ repository.UserRepository = (*fakeUserRepo)(nil)

func (f *fakeUserRepo) Upsert(_ context.Context, u *model.User) (*model.User, error) {
	f.upserted = u
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}
func (f *fakeUserRepo) FindByID(context.Context, int64) (*model.User, error) { return f.user, f.err }
func (f *fakeUserRepo) FindByPublicID(context.Context, uuid.UUID) (*model.User, error) {
	return f.user, f.err
}

// promoted は PromoteToAdmin が呼ばれた回数です。
// 「昇格させた」ことを返り値ではなく呼び出しの有無で見るため。
func (f *fakeUserRepo) PromoteToAdmin(_ context.Context, googleSub string) (bool, error) {
	f.promotedSubs = append(f.promotedSubs, googleSub)
	return f.promoteResult, f.promoteErr
}

func (f *fakeUserRepo) ListAuthorsByIDs(context.Context, []int64) ([]model.Author, error) {
	return nil, f.err
}

type fakeSessionRepo struct {
	created   *model.Session
	liveToken model.SessionToken
	owner     model.SessionOwner
	findErr   error
	deleted   []model.SessionToken
}

var _ repository.SessionRepository = (*fakeSessionRepo)(nil)

func (f *fakeSessionRepo) Create(_ context.Context, s *model.Session) (*model.Session, error) {
	f.created = s
	return s, nil
}

func (f *fakeSessionRepo) FindLive(
	_ context.Context, token model.SessionToken,
) (*model.AuthenticatedSession, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if token != f.liveToken {
		return nil, apperr.ErrNotFound
	}
	return &model.AuthenticatedSession{
		Session: *model.ReconstructSession(token.Hash(), f.owner.ID, time.Now(), time.Now()),
		Owner:   f.owner,
	}, nil
}

func (f *fakeSessionRepo) Delete(_ context.Context, token model.SessionToken) error {
	f.deleted = append(f.deleted, token)
	return nil
}
func (f *fakeSessionRepo) SetAvatarImage(
	_ context.Context, _ int64, imageID *uuid.UUID,
) (*model.SessionOwner, error) {
	owner := f.owner
	if imageID != nil {
		key := "images/" + imageID.String() + ".webp"
		owner.AvatarObjectKey = &key
	}
	return &owner, nil
}

func (f *fakeSessionRepo) DeleteByUserID(context.Context, int64) (int64, error) { return 0, nil }
func (f *fakeSessionRepo) DeleteExpired(context.Context, int32) (int64, error)  { return 0, nil }

// ---------------------------------------------------------------------------
// ヘルパ
// ---------------------------------------------------------------------------

var fixedNow = time.Unix(1_700_000_000, 0).UTC()

func newTestUser() *model.User {
	return model.Reconstruct(42,
		uuid.MustParse("01920000-0000-7000-8000-000000000001"),
		"sub-1", "h@example.com", "ホシノ", nil, model.RoleUser,
		fixedNow, fixedNow, nil)
}

func newLoginInteractor(users *fakeUserRepo, sessions *fakeSessionRepo, provider *fakeProvider) *LoginInteractor {
	return NewLoginInteractor(users, sessions, provider, func() time.Time { return fixedNow })
}

func validClaims() *IDTokenClaims {
	return &IDTokenClaims{
		Subject: "sub-1", Email: "h@example.com", EmailVerified: true,
		Name: "ホシノ", Picture: "https://ex/a.png",
	}
}

// ---------------------------------------------------------------------------
// ログイン開始
// ---------------------------------------------------------------------------

// state / nonce / code_verifier は毎回違い、互いにも異なる必要があります。
// 使い回すと、片方が漏れたときにもう片方も推測できます。
func TestStartLogin_GeneratesDistinctSecrets(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{}
	uc := newLoginInteractor(&fakeUserRepo{}, &fakeSessionRepo{}, provider)

	first, err := uc.StartLogin()
	if err != nil {
		t.Fatalf("StartLogin が失敗した: %v", err)
	}

	if first.State == first.Nonce || first.State == first.CodeVerifier || first.Nonce == first.CodeVerifier {
		t.Error("state / nonce / code_verifier に同じ値が使われている")
	}

	// PKCE の code_verifier は RFC 7636 で 43〜128 文字。
	if n := len(first.CodeVerifier); n < 43 || n > 128 {
		t.Errorf("code_verifier の長さ = %d, want 43〜128 (RFC 7636)", n)
	}

	// プロバイダに渡した値が、返した値と一致していること。
	// ここがずれると、コールバックでの照合が必ず失敗する。
	if provider.gotState != first.State || provider.gotNonce != first.Nonce {
		t.Error("プロバイダに渡した state / nonce が返り値と一致しない")
	}

	second, err := uc.StartLogin()
	if err != nil {
		t.Fatalf("StartLogin が失敗した: %v", err)
	}
	if second.State == first.State {
		t.Error("state が使い回されている")
	}
}

// ---------------------------------------------------------------------------
// コールバック
// ---------------------------------------------------------------------------

func TestCompleteLogin_IssuesSession(t *testing.T) {
	t.Parallel()

	users := &fakeUserRepo{user: newTestUser()}
	sessions := &fakeSessionRepo{}
	uc := newLoginInteractor(users, sessions, &fakeProvider{claims: validClaims()})

	got, err := uc.CompleteLogin(context.Background(), "code-1", "verifier-1", "nonce-1")
	if err != nil {
		t.Fatalf("CompleteLogin が失敗した: %v", err)
	}

	if got.Token == "" {
		t.Fatal("トークンが空")
	}
	if !got.ExpiresAt.Equal(fixedNow.Add(model.DefaultSessionTTL)) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, fixedNow.Add(model.DefaultSessionTTL))
	}
	// TTL は**このインタラクタの時計**から測ったもの。
	// 呼び出し側が time.Until(ExpiresAt) を取ると壁時計との差が混ざり、
	// 時計を固定した環境では負になる (= 即失効の Cookie)。
	if got.TTL != model.DefaultSessionTTL {
		t.Errorf("TTL = %v, want %v", got.TTL, model.DefaultSessionTTL)
	}

	// 保存されたのはハッシュで、返したトークンそのものではないこと。
	if sessions.created == nil {
		t.Fatal("セッションが保存されていない")
	}
	if sessions.created.ID == string(got.Token) {
		t.Error("生のトークンが保存されている")
	}
	if sessions.created.ID != got.Token.Hash() {
		t.Error("保存された ID がトークンのハッシュと一致しない")
	}
	// セッションは upsert 後の利用者に紐づくこと (claims の sub ではない)。
	if sessions.created.UserID != 42 {
		t.Errorf("UserID = %d, want 42", sessions.created.UserID)
	}
}

// **未検証のメールアドレスは受け付けない** (ADR 0005 決定 3)。
//
// ここを通すと、他人のアドレスを名乗ったアカウントを作れてしまう。
func TestCompleteLogin_RejectsUnverifiedEmail(t *testing.T) {
	t.Parallel()

	claims := validClaims()
	claims.EmailVerified = false

	users := &fakeUserRepo{user: newTestUser()}
	sessions := &fakeSessionRepo{}
	uc := newLoginInteractor(users, sessions, &fakeProvider{claims: claims})

	_, err := uc.CompleteLogin(context.Background(), "c", "v", "n")
	if !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want apperr.ErrUnauthenticated", err)
	}
	// 利用者を作ってはいけない。
	if users.upserted != nil {
		t.Error("未検証のアドレスで利用者が作られた")
	}
	if sessions.created != nil {
		t.Error("未検証のアドレスでセッションが発行された")
	}
}

// 退会済みは 401 として扱い、セッションを発行しない。
//
// ErrNotFound のまま返すと HTTP 層で 404 になり、
// 「アカウントが無い」と「閉じられている」を区別できなくなる。
func TestCompleteLogin_RejectsWithdrawnUser(t *testing.T) {
	t.Parallel()

	users := &fakeUserRepo{err: repository.ErrWithdrawn}
	sessions := &fakeSessionRepo{}
	uc := newLoginInteractor(users, sessions, &fakeProvider{claims: validClaims()})

	_, err := uc.CompleteLogin(context.Background(), "c", "v", "n")
	if !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want apperr.ErrUnauthenticated", err)
	}
	if !strings.Contains(err.Error(), "退会") {
		t.Errorf("退会と分かるメッセージになっていない: %v", err)
	}
	if sessions.created != nil {
		t.Error("退会済みなのにセッションが発行された")
	}
}

// ID トークンの検証に失敗したら 401。500 にしない。
func TestCompleteLogin_ProviderFailureIs401(t *testing.T) {
	t.Parallel()

	sessions := &fakeSessionRepo{}
	uc := newLoginInteractor(&fakeUserRepo{}, sessions,
		&fakeProvider{err: errors.New("署名が不正です")})

	_, err := uc.CompleteLogin(context.Background(), "c", "v", "n")
	if !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want apperr.ErrUnauthenticated", err)
	}
	if sessions.created != nil {
		t.Error("検証に失敗したのにセッションが発行された")
	}
}

// code_verifier と nonce がプロバイダへそのまま渡ること。
// ここで取り違えると、PKCE と nonce の検証が意味をなさなくなる。
func TestCompleteLogin_PassesVerifierAndNonceThrough(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{claims: validClaims()}
	uc := newLoginInteractor(&fakeUserRepo{user: newTestUser()}, &fakeSessionRepo{}, provider)

	if _, err := uc.CompleteLogin(context.Background(), "code-x", "verifier-x", "nonce-x"); err != nil {
		t.Fatalf("CompleteLogin が失敗した: %v", err)
	}

	if provider.gotCode != "code-x" {
		t.Errorf("code = %q, want code-x", provider.gotCode)
	}
	if provider.gotVerifier != "verifier-x" {
		t.Errorf("code_verifier = %q, want verifier-x", provider.gotVerifier)
	}
	if provider.gotNonce != "nonce-x" {
		t.Errorf("nonce = %q, want nonce-x", provider.gotNonce)
	}
}

// picture が空なら avatar は nil で渡す。
// 空文字を渡すと、DB に "" が入って「画像あり」と誤認される。
func TestCompleteLogin_EmptyPictureBecomesNil(t *testing.T) {
	t.Parallel()

	claims := validClaims()
	claims.Picture = ""

	users := &fakeUserRepo{user: newTestUser()}
	uc := newLoginInteractor(users, &fakeSessionRepo{}, &fakeProvider{claims: claims})

	if _, err := uc.CompleteLogin(context.Background(), "c", "v", "n"); err != nil {
		t.Fatalf("CompleteLogin が失敗した: %v", err)
	}
	if users.upserted.AvatarURL != nil {
		t.Errorf("AvatarURL = %v, want nil", *users.upserted.AvatarURL)
	}
}

// ---------------------------------------------------------------------------
// 最初の管理者 (ADR 0011 決定 1)
// ---------------------------------------------------------------------------

// **設定した sub の利用者だけが昇格すること。**
func TestCompleteLogin_PromotesBootstrapAdmin(t *testing.T) {
	t.Parallel()

	users := &fakeUserRepo{user: newTestUser(), promoteResult: true}
	uc := newLoginInteractor(users, &fakeSessionRepo{}, &fakeProvider{claims: validClaims()}).
		WithBootstrapAdmin("sub-1")

	if _, err := uc.CompleteLogin(context.Background(), "code", "verifier", "nonce"); err != nil {
		t.Fatalf("CompleteLogin が失敗した: %v", err)
	}

	if len(users.promotedSubs) != 1 || users.promotedSubs[0] != "sub-1" {
		t.Errorf("昇格した sub = %v, want [sub-1]", users.promotedSubs)
	}
}

// **別人は昇格しない。** ここが緩むと、設定を書いた時点で全員が管理者になる。
func TestCompleteLogin_DoesNotPromoteOthers(t *testing.T) {
	t.Parallel()

	users := &fakeUserRepo{user: newTestUser()}
	uc := newLoginInteractor(users, &fakeSessionRepo{}, &fakeProvider{claims: validClaims()}).
		WithBootstrapAdmin("だれか別の sub")

	if _, err := uc.CompleteLogin(context.Background(), "code", "verifier", "nonce"); err != nil {
		t.Fatalf("CompleteLogin が失敗した: %v", err)
	}

	if len(users.promotedSubs) != 0 {
		t.Errorf("別人が昇格された: %v", users.promotedSubs)
	}
}

// 未設定なら誰も昇格しない。
func TestCompleteLogin_NoBootstrapAdminConfigured(t *testing.T) {
	t.Parallel()

	users := &fakeUserRepo{user: newTestUser()}
	uc := newLoginInteractor(users, &fakeSessionRepo{}, &fakeProvider{claims: validClaims()})

	if _, err := uc.CompleteLogin(context.Background(), "code", "verifier", "nonce"); err != nil {
		t.Fatalf("CompleteLogin が失敗した: %v", err)
	}

	if len(users.promotedSubs) != 0 {
		t.Errorf("未設定なのに昇格が走った: %v", users.promotedSubs)
	}
}

// **昇格に失敗してもログインは通ること。**
//
// ここで失敗を返すと「管理者にしたい人だけログインできない」ことになる。
// 昇格は運用の都合であって、認証の成否ではない。
func TestCompleteLogin_PromotionFailureDoesNotBlockLogin(t *testing.T) {
	t.Parallel()

	users := &fakeUserRepo{user: newTestUser(), promoteErr: errors.New("DB が落ちている")}
	uc := newLoginInteractor(users, &fakeSessionRepo{}, &fakeProvider{claims: validClaims()}).
		WithBootstrapAdmin("sub-1")

	got, err := uc.CompleteLogin(context.Background(), "code", "verifier", "nonce")
	if err != nil {
		t.Fatalf("昇格の失敗でログインまで落ちた: %v", err)
	}
	if got.Token == "" {
		t.Error("セッションが発行されていない")
	}
}
