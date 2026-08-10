// Package model は利用者と認証セッションのドメインモデルを定義します。
package model

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
)

// DisplayNameMaxLength は表示名の最大文字数です。
// DB 側の CHECK 制約 (users_display_name_length) と必ず同じ値にしてください。
//
// 匿名投稿の author_name (50 文字) より緩いのは、この値が Google から来る
// 外部入力であるためです。揃えると、長い表示名の利用者がログインできなくなります。
// 切り詰めはアプリ側の責任で、DB は暴走を止める上限として持ちます
// (docs/adr/0016-schema-and-indexes.md)。
const DisplayNameMaxLength = 100

// User は Google アカウントに紐づく利用者を表すエンティティです。
//
// ID は内部 ID で、API には出しません。外部に見せるのは PublicID だけです
// (docs/adr/0003-open-questions.md 未決 #11 の決定)。
type User struct {
	ID          int64
	PublicID    uuid.UUID
	GoogleSub   string
	Email       string
	DisplayName string
	AvatarURL   *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Author は投稿一覧で投稿者を表示するための、最小限の情報です。
//
// GoogleSub も Email も持ちません。この値の行き先は
// **他人にも見える投稿一覧**であり、余分な列を運ぶと、
// 詰め替えを 1 つ間違えただけで読み手に渡ります
// (docs/adr/0014-author-resolution.md)。
type Author struct {
	ID          int64
	PublicID    uuid.UUID
	DisplayName string
	AvatarURL   *string
	// DeletedAt は退会済みかの判定に使います。
	// 投稿は匿名化されるまで残るため、表示側で扱いを変える必要があります。
	DeletedAt *time.Time
}

// IsDeleted は退会済みかを返します。
func (a *Author) IsDeleted() bool {
	return a.DeletedAt != nil
}

// SessionOwner はセッション検証で必要になる範囲の利用者情報です。
//
// GoogleSub を含めていません。あれは IdP との照合にだけ使う値であり、
// リクエストのたびに持ち回ると、ログや文脈に載る機会が無駄に増えます。
type SessionOwner struct {
	ID          int64
	PublicID    uuid.UUID
	Email       string
	DisplayName string
	AvatarURL   *string
}

// NewUser は永続化前の新しい利用者を組み立てます。
// ID と各種時刻は DB が採番するため、ここでは設定しません。
//
// PublicID はここで採番します。UUID v7 を使うのは、先頭 48 ビットが
// タイムスタンプなので B-tree の挿入位置が末尾に寄り、
// v4 のようなページ分割が起きにくいためです。
// PostgreSQL 17 には uuidv7() が無いため生成は Go 側で行います。
func NewUser(googleSub, email, displayName string, avatarURL *string) (*User, error) {
	googleSub = strings.TrimSpace(googleSub)
	if googleSub == "" {
		return nil, fmt.Errorf("google_sub が空です: %w", apperr.ErrInvalidArgument)
	}

	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("メールアドレスが空です: %w", apperr.ErrInvalidArgument)
	}

	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil, fmt.Errorf("表示名が空です: %w", apperr.ErrInvalidArgument)
	}
	// バイト数ではなく文字数で数える。DB 側の char_length() と揃えるため。
	if n := utf8.RuneCountInString(displayName); n > DisplayNameMaxLength {
		return nil, fmt.Errorf(
			"表示名が長すぎます (%d 文字, 上限 %d 文字): %w",
			n, DisplayNameMaxLength, apperr.ErrInvalidArgument,
		)
	}

	publicID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("public_id の生成に失敗しました: %w", err)
	}

	return &User{
		PublicID:    publicID,
		GoogleSub:   googleSub,
		Email:       email,
		DisplayName: displayName,
		AvatarURL:   avatarURL,
	}, nil
}

// Reconstruct は永続化層から読み出した値で利用者を復元します。
// 保存済みのデータが対象なので、検証は行いません。
func Reconstruct(
	id int64, publicID uuid.UUID, googleSub, email, displayName string,
	avatarURL *string, createdAt, updatedAt time.Time, deletedAt *time.Time,
) *User {
	return &User{
		ID:          id,
		PublicID:    publicID,
		GoogleSub:   googleSub,
		Email:       email,
		DisplayName: displayName,
		AvatarURL:   avatarURL,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		DeletedAt:   deletedAt,
	}
}

// IsDeleted は退会済みかを返します。
//
// 退会しても行は消しません。投稿の author_id から参照されており、
// 削除は匿名化 (author_id を NULL にする) で行うためです
// (docs/adr/0005-authentication.md)。
func (u *User) IsDeleted() bool {
	return u.DeletedAt != nil
}
