package usecase

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/user/domain/model"
	"develop-experiments/apps/go-api/internal/user/domain/repository"
)

// randomBytes は state / nonce / code_verifier の長さです。
// PKCE の code_verifier は RFC 7636 で 43〜128 文字と定められており、
// 32 バイトを base64url にすると 43 文字でちょうど下限を満たします。
const randomBytes = 32

// MeDTO はログイン中の利用者としてフロントに返すデータ構造です。
//
// 内部 ID を含めません。API が扱う識別子は PublicID だけです
// (docs/adr/0003-open-questions.md 未決 #11)。
type MeDTO struct {
	PublicID    uuid.UUID `json:"publicId"`
	DisplayName string    `json:"displayName"`
	Email       string    `json:"email"`
	AvatarURL   *string   `json:"avatarUrl"`
}

// LoginResult はログイン成功時に、ハンドラが Cookie を組み立てるための値です。
type LoginResult struct {
	Token     model.SessionToken
	ExpiresAt time.Time
	// TTL はセッションの寿命です。Cookie の MaxAge にそのまま使えます。
	//
	// ExpiresAt だけを渡すと、呼び出し側が time.Until で差を取ることになり、
	// **このインタラクタの時計と壁時計がずれた分だけ MaxAge がずれます**。
	// 時計を固定したテストでは負の値になり、発行と同時に失効する Cookie が
	// できていました。時計は 1 か所に閉じます。
	TTL time.Duration
}

// AuthInteractor は認証のユースケースを担当します。
type AuthInteractor struct {
	users    repository.UserRepository
	sessions repository.SessionRepository
	provider Provider
	now      Clock
}

// NewAuthInteractor は依存を注入してインタラクターを生成します。
// now が nil の場合は time.Now を使います。
func NewAuthInteractor(
	users repository.UserRepository,
	sessions repository.SessionRepository,
	provider Provider,
	now Clock,
) *AuthInteractor {
	if now == nil {
		now = time.Now
	}
	return &AuthInteractor{users: users, sessions: sessions, provider: provider, now: now}
}

// StartLogin はログインを開始し、リダイレクト先と持ち回る値を返します。
//
// state / nonce / code_verifier をここで生成します。
//   - state:         CSRF 対策。コールバックで Cookie 側と照合する
//   - nonce:         ID トークンの差し替え対策。トークン検証で照合する
//   - code_verifier: PKCE。認可コードを横取りされても交換できないようにする
//
// どれか 1 つでも落とすと静かに脆弱になります
// (docs/adr/0005-authentication.md の引き受けるコスト)。
func (i *AuthInteractor) StartLogin() (*AuthRequest, error) {
	state, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("state の生成に失敗しました: %w", err)
	}
	nonce, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("nonce の生成に失敗しました: %w", err)
	}
	verifier, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("code_verifier の生成に失敗しました: %w", err)
	}

	return &AuthRequest{
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
		AuthURL:      i.provider.AuthCodeURL(state, nonce, verifier),
	}, nil
}

// CompleteLogin はコールバックを処理し、セッションを発行します。
//
// state の照合は呼び出し側 (ハンドラ) が Cookie と突き合わせて行います。
// ここに持ち込まないのは、Cookie の読み取りが HTTP 層の責務だからです。
func (i *AuthInteractor) CompleteLogin(
	ctx context.Context, code, codeVerifier, nonce string,
) (*LoginResult, error) {
	claims, err := i.provider.Exchange(ctx, code, codeVerifier, nonce)
	if err != nil {
		// 認可コードの不正と、IdP へ到達できないこと (DNS / TLS / 障害) を
		// ここでは区別できない。利用者へは同じ 401 を返すが、
		// 切り分けの手がかりが何も残らないのは困るのでログには出す。
		slog.WarnContext(ctx, "認可コードの交換に失敗しました",
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("ID トークンの検証に失敗しました: %w", errors.Join(err, apperr.ErrUnauthenticated))
	}

	// 未検証のアドレスは受け付けない (ADR 0005 決定 3)。
	// 検証されていないと、他人のアドレスを名乗ったアカウントを作れてしまう。
	if !claims.EmailVerified {
		return nil, fmt.Errorf("メールアドレスが未検証です: %w", apperr.ErrUnauthenticated)
	}

	var avatar *string
	if claims.Picture != "" {
		avatar = &claims.Picture
	}

	user, err := model.NewUser(claims.Subject, claims.Email, claims.Name, avatar)
	if err != nil {
		return nil, err
	}

	saved, err := i.users.Upsert(ctx, user)
	if err != nil {
		// 退会済みは「アカウントが無い」ではなく「閉じられている」。
		// 復活させるかは未決なので、ここでは拒否する。
		if errors.Is(err, repository.ErrWithdrawn) {
			return nil, fmt.Errorf("退会済みのアカウントです: %w", apperr.ErrUnauthenticated)
		}
		return nil, err
	}

	now := i.now()
	session, token, err := model.NewSession(saved.ID, now, model.DefaultSessionTTL)
	if err != nil {
		return nil, err
	}
	if _, err := i.sessions.Create(ctx, session); err != nil {
		return nil, err
	}

	return &LoginResult{
		Token:     token,
		ExpiresAt: session.ExpiresAt,
		// NewSession が既定値へ丸める場合があるので、引数ではなく結果から取る。
		TTL: session.ExpiresAt.Sub(now),
	}, nil
}

// Authenticate はセッショントークンを検証し、持ち主を返します。
//
// **毎リクエスト通る経路**です。期限切れと退会の判定は SQL 側にあり、
// ここでは再判定しません (判定を 2 か所に置くと片方だけ直したときに食い違うため)。
func (i *AuthInteractor) Authenticate(ctx context.Context, token model.SessionToken) (*MeDTO, error) {
	if token == "" {
		return nil, fmt.Errorf("セッションがありません: %w", apperr.ErrUnauthenticated)
	}

	auth, err := i.sessions.FindLive(ctx, token)
	if err != nil {
		// 「見つからない」は 404 ではなく 401 として扱う。
		// セッションの有無は認証の問題であり、リソースの有無ではない。
		if errors.Is(err, apperr.ErrNotFound) {
			return nil, fmt.Errorf("セッションが無効です: %w", apperr.ErrUnauthenticated)
		}
		return nil, err
	}

	return &MeDTO{
		PublicID:    auth.Owner.PublicID,
		DisplayName: auth.Owner.DisplayName,
		Email:       auth.Owner.Email,
		AvatarURL:   auth.Owner.AvatarURL,
	}, nil
}

// Logout はセッションを削除します。
// 対象が無い場合も成功として扱います (目的は「もう使えないこと」のため)。
func (i *AuthInteractor) Logout(ctx context.Context, token model.SessionToken) error {
	return i.sessions.Delete(ctx, token)
}

// randomToken は URL 安全な乱数文字列を返します。
func randomToken() (string, error) {
	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
