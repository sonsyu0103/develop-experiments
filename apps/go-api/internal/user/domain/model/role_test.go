package model

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
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

// **ロールの定義が 3 か所で一致していること。**
//
// 初版はこう書いていた。
//
//	all := []Role{RoleUser, RoleModerator, RoleAdmin}
//	if len(all) != 3 { ... }
//
// **これは構造上落ちない。** all が手書きのリテラルなので、
// 定数を 4 つに増やしてもこの行を直さない限り 3 のままになる。
// 「増やしたときに気づかせる」と書いておきながら気づけない、
// 通るだけのテストだった (変異プローブで実証: RoleOwner を足しても緑)。
//
// 実際に守るには、**変わる側から読む**しかない。
// ここでは 3 つの出所をそれぞれ解析して突き合わせる。
//
//	role.go        定数の宣言 (ソースを走査する)
//	000003 の CHECK 制約
//	openapi.yaml の enum
//
// ソースを読むテストは行儀が良くないが、
// この不一致は「保存できない値をアプリが作る」か
// 「アプリが知らない値が DB に入る」形で本番に出る。
// 検出できる場所が他に無い。
func TestRole_DefinitionsAgreeAcrossSources(t *testing.T) {
	t.Parallel()

	declared := rolesFromSource(t)
	registered := make(map[string]bool, len(AllRoles))
	for _, r := range AllRoles {
		registered[string(r)] = true
	}

	// 定数を足して AllRoles に入れ忘れると、ParseRole が弾くようになる。
	// 気づけるようにここで落とす。
	for name, value := range declared {
		if !registered[value] {
			t.Errorf("定数 %s (%q) が AllRoles に登録されていない", name, value)
		}
	}
	if len(declared) != len(AllRoles) {
		t.Errorf("宣言 %d 個 / AllRoles %d 個で数が合わない", len(declared), len(AllRoles))
	}

	assertSameSet(t, "DB の CHECK 制約", registered, valuesFromMigration(t))
	assertSameSet(t, "仕様書の enum", registered, valuesFromOpenAPI(t))
}

// rolesFromSource は role.go の定数宣言を走査します。
// 「定数を足したのに登録し忘れた」を検出するため、リテラルではなくソースから取ります。
func rolesFromSource(t *testing.T) map[string]string {
	t.Helper()

	src := readRepoFile(t, "apps/go-api/internal/user/domain/model/role.go")
	re := regexp.MustCompile(`(?m)^\s*(\w+)\s+Role\s*=\s*"([^"]+)"`)

	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("role.go から定数を 1 つも読み取れなかった (宣言の書き方が変わった?)")
	}
	return out
}

func valuesFromMigration(t *testing.T) map[string]bool {
	t.Helper()

	src := readRepoFile(t, "apps/go-api/db/migrations/000003_add_user_role.up.sql")
	m := regexp.MustCompile(`CHECK \(role IN \(([^)]+)\)\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("000003 から CHECK 制約を読み取れなかった")
	}
	return quotedValues(m[1], `'([^']+)'`)
}

func valuesFromOpenAPI(t *testing.T) map[string]bool {
	t.Helper()

	src := readRepoFile(t, "api/openapi.yaml")
	m := regexp.MustCompile(`(?m)^\s*Role:\n\s*type: string\n\s*enum: \[([^\]]+)\]`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("openapi.yaml から Role の enum を読み取れなかった")
	}
	return quotedValues(m[1], `([a-z_]+)`)
}

func quotedValues(list, pattern string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(list, -1) {
		out[m[1]] = true
	}
	return out
}

// readRepoFile はリポジトリ直下からの相対パスでファイルを読みます。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	// このファイルは apps/go-api/internal/user/domain/model にある。
	root := filepath.Join("..", "..", "..", "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("%s を読めない: %v", rel, err)
	}
	return string(b)
}

func assertSameSet(t *testing.T, name string, want, got map[string]bool) {
	t.Helper()

	for v := range want {
		if !got[v] {
			t.Errorf("%s に %q が無い", name, v)
		}
	}
	for v := range got {
		if !want[v] {
			t.Errorf("%s にだけ %q がある (Go 側に無い)", name, v)
		}
	}
}
