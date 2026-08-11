package model

import (
	"fmt"

	"develop-experiments/apps/go-api/internal/apperr"
)

// Role は利用者の権限です。
//
// 3 段階に分けるのは、**投稿を消せる権限と、権限を配れる権限を分離する**ためです
// (docs/adr/0011-moderation.md 決定 1)。
// モデレーターを増やしても、権限を配れる人間は増えません。
//
// DB 側の CHECK 制約 (users_role_valid) と**必ず同じ 3 値**にしてください。
// 片方だけ増やすと、保存できない値をアプリが作るか、
// アプリが知らない値が DB に入ります。
type Role string

const (
	// RoleUser は一般利用者です。DB 側の DEFAULT と同じ値です。
	RoleUser Role = "user"
	// RoleModerator は他人・匿名の投稿を削除でき、通報キューを見られます。
	RoleModerator Role = "moderator"
	// RoleAdmin はロールを変更できます。
	RoleAdmin Role = "admin"
)

// ParseRole は永続化層から読み出した文字列を Role にします。
//
// **知らない値はエラーにします。** 既定値へ丸めると、
// DB に想定外の値が入ったときに「一般利用者として静かに動く」か、
// 逆に「管理者として静かに動く」形になります。
// どちらも気づけないので、読めない時点で止めます。
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleUser, RoleModerator, RoleAdmin:
		return r, nil
	default:
		return "", fmt.Errorf("不正なロールです (%q): %w", s, apperr.ErrInvalidArgument)
	}
}

// CanModerate は他人・匿名の投稿を削除できるかを返します。
//
// 匿名投稿は投稿者が居ないため、**モデレーター以上でなければ誰も消せません**
// (ADR 0011 決定 2)。この ADR の初版には管理者が存在せず、
// 匿名投稿を誰も削除できない設計になっていました。
func (r Role) CanModerate() bool {
	return r == RoleModerator || r == RoleAdmin
}

// CanChangeRoles はロールを変更できるかを返します。**admin だけ**です。
func (r Role) CanChangeRoles() bool {
	return r == RoleAdmin
}

// IsPrivileged は一般利用者より強い権限を持つかを返します。
// DB 側の部分索引 users_privileged_idx の述語 (role <> 'user') と対応します。
func (r Role) IsPrivileged() bool {
	return r != RoleUser
}
