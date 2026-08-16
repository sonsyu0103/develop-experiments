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
	// Role は**自分のロールだけ**返します。フロントが管理用の導線を
	// 出し分けるために要ります。他人のロールは Author に含めません。
	Role model.Role `json:"role"`
}

// PrincipalDTO は認証済みリクエストの主体です。
//
// **UserID は内部 ID (users.id) です。API には出しません。**
// 投稿者の紐付け (threads.author_id / comments.author_id) に必要なので、
// Me とは分けて持ちます。Me に混ぜると、投稿一覧の詰め替えを 1 つ間違えた
// だけで内部 ID が外に出ます (docs/adr/0003-open-questions.md 未決 #11)。
type PrincipalDTO struct {
	UserID int64
	// Role は権限判定に使います。**API には出しません** ——
	// 出すのは Me だけで、他人のロールは投稿一覧に載せません
	// (誰がモデレーターかを晒す必要がない)。
	Role model.Role
	Me   MeDTO
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

// LoginInteractor はログイン (認可コードの交換) を担当します。
//
// **セッションの検証はここに置きません** (SessionInteractor を参照)。
// 分けているのは、IdP を必要とするのがログインだけだからです。
// 一体にしていた頃は、Google の資格情報が無い環境では
// セッションの検証ごと動かず、認証を要する経路が CI で
// まったく検証できませんでした
// (docs/adr/0005-authentication.md 決定 4 / ADR 0003 未決 #16)。
type LoginInteractor struct {
	users    repository.UserRepository
	sessions repository.SessionRepository
	provider Provider
	now      Clock
	// bootstrapAdminSub が空でなければ、その Google sub の利用者を
	// ログイン時に admin へ昇格させます (ADR 0011 決定 1)。
	bootstrapAdminSub string
}

// WithBootstrapAdmin は最初の管理者にする Google の sub を設定します。
//
// コンストラクタの引数にしないのは、**認証の主経路とは独立した運用設定**
// だからです。引数に混ぜると、テストのたびにこの値を意識することになります。
func (i *LoginInteractor) WithBootstrapAdmin(googleSub string) *LoginInteractor {
	i.bootstrapAdminSub = googleSub
	return i
}

// NewLoginInteractor は依存を注入してインタラクターを生成します。
// now が nil の場合は time.Now を使います。
func NewLoginInteractor(
	users repository.UserRepository,
	sessions repository.SessionRepository,
	provider Provider,
	now Clock,
) *LoginInteractor {
	if now == nil {
		now = time.Now
	}
	return &LoginInteractor{users: users, sessions: sessions, provider: provider, now: now}
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
func (i *LoginInteractor) StartLogin() (*AuthRequest, error) {
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
func (i *LoginInteractor) CompleteLogin(
	ctx context.Context, code, codeVerifier, nonce string,
) (*LoginResult, error) {
	claims, err := i.provider.Exchange(ctx, code, codeVerifier, nonce)
	if err != nil {
		// 認可コードの不正と、IdP へ到達できないこと (DNS / TLS / 障害) を
		// ここでは区別できない。利用者へは同じ 401 を返すが、
		// 切り分けの手がかりが何も残らないのは困るのでログには出す。
		slog.WarnContext(ctx, "oidc_code_exchange_failed",
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

	i.promoteBootstrapAdmin(ctx, claims.Subject)

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

// promoteBootstrapAdmin は設定された Google sub の利用者を admin にします。
//
// **失敗してもログインは通します。** 昇格は運用の都合であり、
// ここで失敗を返すと「管理者にしたい人だけログインできない」ことになります。
// 記録は残すので、あとから気づけます。
func (i *LoginInteractor) promoteBootstrapAdmin(ctx context.Context, googleSub string) {
	if i.bootstrapAdminSub == "" {
		return
	}
	if i.bootstrapAdminSub != googleSub {
		// **無言で戻らない。** 起動時には bootstrap_admin=true と出るので、
		// 運用者には「効いている」と見える。設定値に打ち間違いや
		// 余分な空白があると、管理者にしたい人が何度ログインしても
		// 昇格せず、手がかりがどこにも残らない。
		// PromoteToAdmin は role に書く唯一の経路なので、
		// その環境には管理者が永久に存在しないことになる。
		//
		// sub 自体はログに出さない (個人を特定する識別子のため)。
		// 長さだけ出せば、空白混入や切り詰めは判別できる。
		slog.WarnContext(ctx, "bootstrap_admin_sub_mismatch",
			slog.Int("configured_len", len(i.bootstrapAdminSub)),
			slog.Int("received_len", len(googleSub)),
		)
		return
	}

	promoted, err := i.users.PromoteToAdmin(ctx, googleSub)
	if err != nil {
		slog.ErrorContext(ctx, "bootstrap_admin_promotion_failed",
			slog.String("error", err.Error()))
		return
	}
	if promoted {
		// **必ず記録に残す。** 権限が動いた事実は、
		// 監査記録 (ADR 0011 決定 3) が入るまでログだけが頼りになる。
		slog.InfoContext(ctx, "bootstrap_admin_promoted")
	}
}

// randomToken は URL 安全な乱数文字列を返します。
func randomToken() (string, error) {
	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
