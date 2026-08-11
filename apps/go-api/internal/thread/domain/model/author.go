package model

import (
	"time"

	"github.com/google/uuid"
)

// WithdrawnDisplayName は退会した利用者の表示名です。
//
// comment モジュールにも同じ定数があります。共有パッケージに切り出すと、
// そこが新しいモジュール間の結合点になるため、重複を許容します
// (docs/adr/0014-author-resolution.md の引き受けるコスト)。
const WithdrawnDisplayName = "退会したユーザー"

// Author は一覧や詳細で投稿者を表示するための、最小限の情報です。
//
// **user モジュールの型は持ち込みません** ([ADR 0004] / [ADR 0014])。
// thread が知っているのは「投稿者には公開 ID と表示名とアバターがある」
// ことだけです。
//
// この値が nil になりうること (= 匿名投稿) が型に現れている点が重要です。
// 匿名を取りこぼすと nil 参照で落ちるため、型で扱いを強制します。
//
// [ADR 0004]: ../../../../../../docs/adr/0004-modular-monolith.md
// [ADR 0014]: ../../../../../../docs/adr/0014-author-resolution.md
type Author struct {
	// PublicID は公開用の識別子です。**退会済みでは nil になります。**
	//
	// ポインタにしているのは、表示側が Withdrawn の判定を忘れても
	// 退会した人の識別子が外に出ないようにするためです。
	PublicID    *uuid.UUID
	DisplayName string
	AvatarURL   *string
	// Withdrawn は投稿者が退会済みかを表します。
	// 投稿そのものは匿名化されるまで残るため、表示だけを差し替えます。
	Withdrawn bool
}

// NewAuthor は永続化層から読み出した値で投稿者を組み立てます。
//
// **退会済みの正規化をここで行います。** 表示の差し替えを呼び出し側に任せると、
// 一覧・詳細・作成レスポンスのどれか 1 つで必ず漏れます。
func NewAuthor(
	publicID uuid.UUID, displayName string, avatarURL *string, deletedAt *time.Time,
) *Author {
	if deletedAt != nil {
		return &Author{DisplayName: WithdrawnDisplayName, Withdrawn: true}
	}

	id := publicID
	return &Author{
		PublicID:    &id,
		DisplayName: displayName,
		AvatarURL:   avatarURL,
	}
}
