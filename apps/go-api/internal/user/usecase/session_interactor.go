package usecase

import (
	"context"
	"errors"
	"fmt"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/user/domain/model"
	"develop-experiments/apps/go-api/internal/user/domain/repository"
)

// SessionInteractor は発行済みセッションの検証と破棄を担当します。
//
// **IdP を必要としません。** 依存は sessions だけで、Google の資格情報が
// 無い環境でも動きます。これが LoginInteractor と分かれている理由です
// (docs/adr/0005-authentication.md 決定 4)。
//
// 分ける前は、資格情報が無いと認証まわりが丸ごと無効になり、
// CI では **セッションを要する経路が 1 件も検証されていませんでした**
// —— 冪等キーのスモーク 11 件が SKIP されたまま「緑」になっていた
// (docs/adr/0003-open-questions.md 未決 #16)。
type SessionInteractor struct {
	sessions repository.SessionRepository
}

// NewSessionInteractor は依存を注入してインタラクターを生成します。
func NewSessionInteractor(sessions repository.SessionRepository) *SessionInteractor {
	return &SessionInteractor{sessions: sessions}
}

// Authenticate はセッショントークンを検証し、持ち主を返します。
//
// **毎リクエスト通る経路**です。期限切れと退会の判定は SQL 側にあり、
// ここでは再判定しません (判定を 2 か所に置くと片方だけ直したときに食い違うため)。
func (i *SessionInteractor) Authenticate(
	ctx context.Context, token model.SessionToken,
) (*PrincipalDTO, error) {
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

	return &PrincipalDTO{
		UserID: auth.Owner.ID,
		Role:   auth.Owner.Role,
		Me: MeDTO{
			PublicID:    auth.Owner.PublicID,
			DisplayName: auth.Owner.DisplayName,
			Email:       auth.Owner.Email,
			AvatarURL:   auth.Owner.AvatarURL,
			Role:        auth.Owner.Role,
		},
	}, nil
}

// Logout はセッションを削除します。
// 対象が無い場合も成功として扱います (目的は「もう使えないこと」のため)。
//
// **ログインできない環境でも動く必要があります。** 発行済みのセッションを
// 捨てる操作であり、IdP とは関わりません。ここをログインと同じ設定で
// 塞ぐと、資格情報を外した瞬間に「ログアウトできないセッション」が残ります。
func (i *SessionInteractor) Logout(ctx context.Context, token model.SessionToken) error {
	return i.sessions.Delete(ctx, token)
}
