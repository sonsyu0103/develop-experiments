package model

import (
	"errors"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

// **知らない値を既定値へ丸めない。**
//
// 丸めると「一般利用者として静かに動く」か「管理者として静かに動く」形になり、
// どちらも気づけない。読めない時点で止める。
func TestParseRole(t *testing.T) {
	t.Parallel()

	ok := map[string]Role{
		"user":      RoleUser,
		"moderator": RoleModerator,
		"admin":     RoleAdmin,
	}
	for in, want := range ok {
		got, err := ParseRole(in)
		if err != nil {
			t.Errorf("ParseRole(%q) が失敗した: %v", in, err)
		}
		if got != want {
			t.Errorf("ParseRole(%q) = %q, want %q", in, got, want)
		}
	}

	// DB の CHECK 制約を外したときだけ到達する経路。
	// 大文字や前後の空白も「知らない値」として扱う。
	for _, in := range []string{"", "superadmin", "Admin", " admin", "owner"} {
		if _, err := ParseRole(in); !errors.Is(err, apperr.ErrInvalidArgument) {
			t.Errorf("ParseRole(%q) が通ってしまった (err=%v)", in, err)
		}
	}
}

// **投稿を消せる権限と、権限を配れる権限が分離していること。**
//
// ADR 0011 決定 1 の表そのもの。モデレーターを増やしても、
// 権限を配れる人間が増えてはいけない。
func TestRole_Permissions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role        Role
		moderate    bool
		changeRoles bool
		privileged  bool
	}{
		{RoleUser, false, false, false},
		{RoleModerator, true, false, true},
		{RoleAdmin, true, true, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()

			if got := tt.role.CanModerate(); got != tt.moderate {
				t.Errorf("CanModerate() = %v, want %v", got, tt.moderate)
			}
			if got := tt.role.CanChangeRoles(); got != tt.changeRoles {
				t.Errorf("CanChangeRoles() = %v, want %v", got, tt.changeRoles)
			}
			// DB の部分索引 users_privileged_idx の述語 (role <> 'user') と対応する。
			if got := tt.role.IsPrivileged(); got != tt.privileged {
				t.Errorf("IsPrivileged() = %v, want %v", got, tt.privileged)
			}
		})
	}
}

// ロールの値が DB の CHECK 制約と一致していること。
//
// **片方だけ増やすと、保存できない値をアプリが作るか、
// アプリが知らない値が DB に入る。** 数を固定して、増やしたときに気づかせる。
func TestRole_ValuesMatchSchema(t *testing.T) {
	t.Parallel()

	all := []Role{RoleUser, RoleModerator, RoleAdmin}
	if len(all) != 3 {
		t.Fatalf("ロールが %d 個ある。db/migrations の users_role_valid と "+
			"api/openapi.yaml の Role enum も一緒に直したか確認すること", len(all))
	}
	for _, r := range all {
		if _, err := ParseRole(string(r)); err != nil {
			t.Errorf("定数 %q が ParseRole を通らない", r)
		}
	}
}
