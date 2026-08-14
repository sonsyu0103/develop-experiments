package model

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

// **知らない値を既定値へ丸めない** (Role と同じ約束)。
//
// 丸めると、記録されるべき操作が別の名前で監査記録に残ります。
func TestParseActionType(t *testing.T) {
	t.Parallel()

	ok := map[string]ActionType{
		"delete_thread":  ActionDeleteThread,
		"delete_comment": ActionDeleteComment,
		"delete_image":   ActionDeleteImage,
		"change_role":    ActionChangeRole,
	}
	for in, want := range ok {
		got, err := ParseActionType(in)
		if err != nil {
			t.Errorf("ParseActionType(%q) が失敗した: %v", in, err)
		}
		if got != want {
			t.Errorf("ParseActionType(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{"", "delete", "DELETE_THREAD", " delete_thread", "ban_user"} {
		if _, err := ParseActionType(in); !errors.Is(err, apperr.ErrInvalidArgument) {
			t.Errorf("ParseActionType(%q) が通ってしまった (err=%v)", in, err)
		}
	}
}

// **削除 API が受け付けてよいのは削除だけであること。**
//
// change_role を通すと、ロールの変更が「対象 ID と理由」だけで
// 実行できる形になります (新しいロールを渡す口が無いので、
// 何も変わらないまま記録だけが残ります)。
func TestActionType_IsDelete(t *testing.T) {
	t.Parallel()

	want := map[ActionType]bool{
		ActionDeleteThread:  true,
		ActionDeleteComment: true,
		ActionDeleteImage:   true,
		ActionChangeRole:    false,
	}
	for a, expected := range want {
		if got := a.IsDelete(); got != expected {
			t.Errorf("%q.IsDelete() = %v, want %v", a, got, expected)
		}
	}
	// 解釈を通っていない値は削除として扱わない。
	if ActionType("ban_user").IsDelete() {
		t.Error("知らない操作が削除として通っている")
	}
}

// **対象種別が操作から一意に決まること。**
//
// リクエストから受け取ると、delete_thread と image のように
// 食い違う組み合わせがそのまま監査記録に残ります。
func TestActionType_TargetType(t *testing.T) {
	t.Parallel()

	want := map[ActionType]TargetType{
		ActionDeleteThread:  TargetThread,
		ActionDeleteComment: TargetComment,
		ActionDeleteImage:   TargetImage,
		ActionChangeRole:    TargetUser,
	}
	for a, expected := range want {
		if got := a.TargetType(); got != expected {
			t.Errorf("%q.TargetType() = %q, want %q", a, got, expected)
		}
	}

	// **知らない操作は空文字。** DB の CHECK 制約が拒否するので、
	// 未知の対象種別が記録に残ることはありません。
	if got := ActionType("ban_user").TargetType(); got != "" {
		t.Errorf("知らない操作の TargetType = %q, want 空文字", got)
	}
}

// **3 か所の定義が揃っていること** (Role と同じ理由)。
//
//	この定数群 / DB の CHECK 制約 / 仕様書の enum
//
// 揃っていないと「保存できない値をアプリが作る」か
// 「アプリが知らない値が DB に入る」形で本番に出ます。
//
// **仕様書の enum だけは意図的に狭い**ことに注意してください ——
// change_role は DB とアプリにありますが、削除の API では受け付けません。
// そのぶんを差し引いて比べます。
func TestActionType_DefinitionsAgreeAcrossSources(t *testing.T) {
	t.Parallel()

	declared := actionsFromSource(t)
	registered := make(map[string]bool, len(AllActionTypes))
	for _, a := range AllActionTypes {
		registered[string(a)] = true
	}

	for name, value := range declared {
		if !registered[value] {
			t.Errorf("定数 %s (%q) が AllActionTypes に登録されていない", name, value)
		}
	}
	if len(declared) != len(AllActionTypes) {
		t.Errorf("宣言 %d 個 / AllActionTypes %d 個で数が合わない",
			len(declared), len(AllActionTypes))
	}

	assertSameSet(t, "DB の CHECK 制約 (moderation_action_valid)",
		registered, valuesFromMigration(t, `CHECK \(\s*action IN \(([^)]+)\)`))

	// 仕様書は change_role を持たない。**それが期待値**なので、
	// 「アプリ側 − change_role」と突き合わせます。
	// ここを registered と比べてしまうと、仕様書に change_role を
	// 足させる方向に誘導することになります。
	exposed := map[string]bool{}
	for v := range registered {
		if v != string(ActionChangeRole) {
			exposed[v] = true
		}
	}
	assertSameSet(t, "仕様書の ModerationActionType",
		exposed, valuesFromOpenAPIEnum(t, "ModerationActionType"))
}

// **対象種別も 3 か所で揃っていること。**
//
// こちらは仕様書もすべての値を持ちます (レスポンスに現れるため)。
func TestTargetType_DefinitionsAgreeAcrossSources(t *testing.T) {
	t.Parallel()

	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\w+)\s+TargetType\s*=\s*"([^"]+)"`).
		FindAllStringSubmatch(readRepoFile(t, "apps/go-api/internal/moderation/domain/model/action.go"), -1) {
		declared[m[2]] = true
	}
	if len(declared) == 0 {
		t.Fatal("action.go から TargetType の定数を 1 つも読み取れなかった")
	}

	assertSameSet(t, "DB の CHECK 制約 (moderation_target_type_valid)",
		declared, valuesFromMigration(t, `CHECK \(\s*target_type IN \(([^)]+)\)`))
	assertSameSet(t, "仕様書の ModerationTargetType",
		declared, valuesFromOpenAPIEnum(t, "ModerationTargetType"))
}

// ---------------------------------------------------------------------------
// ヘルパ
// ---------------------------------------------------------------------------

// actionsFromSource は action.go の定数宣言を走査します。
// 「定数を足したのに登録し忘れた」を検出するため、ソースから取ります。
func actionsFromSource(t *testing.T) map[string]string {
	t.Helper()

	src := readRepoFile(t, "apps/go-api/internal/moderation/domain/model/action.go")
	re := regexp.MustCompile(`(?m)^\s*(\w+)\s+ActionType\s*=\s*"([^"]+)"`)

	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("action.go から定数を 1 つも読み取れなかった (宣言の書き方が変わった?)")
	}
	return out
}

func valuesFromMigration(t *testing.T, pattern string) map[string]bool {
	t.Helper()

	src := readRepoFile(t, "apps/go-api/db/migrations/000008_add_moderation.up.sql")
	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("000008 から %s を読み取れなかった", pattern)
	}
	return matchSet(m[1], `'([^']+)'`)
}

// valuesFromOpenAPIEnum は仕様書のスキーマ名から enum の値を拾います。
//
// **Role の検査と書き方が違います。** こちらの enum は
// 複数行のリスト形式 (- delete_thread) なので、
// スキーマ名から次のスキーマ名までを切り出してから読みます。
func valuesFromOpenAPIEnum(t *testing.T, schema string) map[string]bool {
	t.Helper()

	src := readRepoFile(t, "api/openapi.yaml")
	block := regexp.MustCompile(`(?ms)^    ` + schema + `:\n(.*?)\n    \w+:`).FindStringSubmatch(src)
	if block == nil {
		t.Fatalf("openapi.yaml から %s を読み取れなかった", schema)
	}
	enum := regexp.MustCompile(`(?ms)\n      enum:\n((?:\s+- \w+\n)+)`).FindStringSubmatch(block[1])
	if enum == nil {
		t.Fatalf("%s の enum を読み取れなかった", schema)
	}
	return matchSet(enum[1], `- (\w+)`)
}

func matchSet(list, pattern string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(list, -1) {
		out[m[1]] = true
	}
	return out
}

// readRepoFile はリポジトリ直下からの相対パスでファイルを読みます。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	// このファイルは apps/go-api/internal/moderation/domain/model にある。
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
