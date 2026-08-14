package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

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
	// images はストレージの設定が無い環境では nil になります。
	images ImageResolver
}

// imageKindAvatar は EnsureOwned に渡す用途です。
//
// **image モジュールの定数を参照しません** (モジュールをまたがないため)。
// 値がずれると添付が常に 404 になるので、スモークが検出します。
const imageKindAvatar = "avatar"

// ImageResolver は画像の解決を担います。
//
// **image モジュールを import しません** (docs/adr/0004-modular-monolith.md)。
// 利用側が必要な操作だけのインターフェースを定義する形は、
// ThreadExistenceChecker と同じです。実装は image のユースケースが満たします。
type ImageResolver interface {
	// EnsureOwned は「その利用者が所有する確定済みの画像か」を確認します。
	// kind は用途 ("comment_attachment" / "avatar" / "thread_icon")。
	// **文字列で渡します。** image モジュールの型を知らないためです。
	EnsureOwned(ctx context.Context, ownerID int64, imageID uuid.UUID, kind string) error
	// URL はオブジェクトキーから配信用の絶対 URL を組み立てます。
	URL(objectKey string) string
}

// NewSessionInteractor は依存を注入してインタラクターを生成します。
// images は nil を許します (ストレージの設定が無い環境)。
func NewSessionInteractor(
	sessions repository.SessionRepository, images ImageResolver,
) *SessionInteractor {
	return &SessionInteractor{sessions: sessions, images: images}
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
			AvatarURL:   i.avatarURL(auth.Owner),
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

// avatarURL は返すプロフィール画像の URL を決めます。
//
// **アップロードした画像があればそちらを優先します。**
// 無ければ Google のものを返します (ADR 0007 のスキーマ)。
// クライアントは 1 つのフィールドだけを見れば済みます ——
// どちらから来たかは表示の関心事ではありません。
func (i *SessionInteractor) avatarURL(owner model.SessionOwner) *string {
	if owner.AvatarObjectKey == nil || i.images == nil {
		return owner.AvatarURL
	}
	url := i.images.URL(*owner.AvatarObjectKey)
	return &url
}

// SetAvatar はプロフィール画像を設定します。imageID が nil なら解除します。
//
// **他人の画像は 404 として扱います** (存在を隠すため。ADR 0013)。
// 所有者の確認をここで行うのは、外部キー違反として DB に弾かせると
// 400 になり、「他人のものだった」と「存在しない」を区別できないためです。
func (i *SessionInteractor) SetAvatar(
	ctx context.Context, userID int64, imageID *uuid.UUID,
) (*MeDTO, error) {
	if imageID != nil {
		if i.images == nil {
			return nil, fmt.Errorf("画像は現在利用できません: %w", apperr.ErrUnavailable)
		}
		if err := i.images.EnsureOwned(ctx, userID, *imageID, imageKindAvatar); err != nil {
			return nil, err
		}
	}

	owner, err := i.sessions.SetAvatarImage(ctx, userID, imageID)
	if err != nil {
		return nil, err
	}

	return &MeDTO{
		PublicID:    owner.PublicID,
		DisplayName: owner.DisplayName,
		Email:       owner.Email,
		AvatarURL:   i.avatarURL(*owner),
		Role:        owner.Role,
	}, nil
}
