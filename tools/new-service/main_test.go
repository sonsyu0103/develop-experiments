package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 本物のひな形を使う。小さな偽物で試すと、ひな形に増えたファイルの扱いを見逃す。
const realTemplate = "../../templates/go-service"

func TestGenerate_ReplacesEveryPlaceholder(t *testing.T) {
	out := filepath.Join(t.TempDir(), "svc")
	n, err := generate(options{name: "billing-api", module: "github.com/acme/billing-api", template: realTemplate, out: out})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("1 ファイルも生成されていない")
	}

	// **置き換え漏れが 1 か所でもあれば落とす。** 漏れると、k8s では別サービスの
	// 名前のまま動き、Go では example.com/go-service を import して壊れる。
	err = filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), placeholderName) {
			t.Errorf("%s に %q が残っている", path, placeholderName)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	mod, err := os.ReadFile(filepath.Join(out, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(mod), "module github.com/acme/billing-api\n") {
		t.Errorf("go.mod の module 行が違う:\n%s", mod)
	}
}

// -module にプレースホルダの名前が含まれても、二重に置き換えない。
func TestGenerate_ModuleContainingPlaceholderName(t *testing.T) {
	out := filepath.Join(t.TempDir(), "svc")
	if _, err := generate(options{name: "x", module: "github.com/acme/go-service-x", template: realTemplate, out: out}); err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(out, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(mod), "module github.com/acme/go-service-x\n") {
		t.Errorf("module 行が二重に置き換えられている:\n%s", mod)
	}
}

func TestValidate_Rejects(t *testing.T) {
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := options{name: "svc", module: "example.com/svc", template: realTemplate, out: filepath.Join(t.TempDir(), "new")}

	cases := map[string]func(o *options){
		"大文字":          func(o *options) { o.name = "Billing" },
		"数字で始まる":       func(o *options) { o.name = "1svc" },
		"末尾がハイフン":      func(o *options) { o.name = "svc-" },
		"アンダースコア":      func(o *options) { o.name = "my_svc" },
		"64 文字":        func(o *options) { o.name = "a" + strings.Repeat("b", 63) },
		"空の名前":         func(o *options) { o.name = "" },
		"モジュールに空白":     func(o *options) { o.module = "example.com/my svc" },
		"モジュールが / 終わり": func(o *options) { o.module = "example.com/svc/" },
		"出力先が未指定":      func(o *options) { o.out = "" },
		"出力先が空でない":     func(o *options) { o.out = nonEmpty },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := ok
			mutate(&o)
			if err := validate(o); err == nil {
				t.Fatalf("通ってしまった: %+v", o)
			}
		})
	}
	if err := validate(ok); err != nil {
		t.Fatalf("正しい入力が弾かれた: %v", err)
	}
}

// **置き換えの網に掛からない書き方を、ひな形に入れさせない。**
// ジェネレータが置き換えるのは go-service と example.com/go-service だけ。
// GoService や go_service と書くと、生成したサービスに元の名前のまま残る。
// 生成結果の検査 (TestGenerate_ReplacesEveryPlaceholder) は go-service しか
// 探さないので、ここで塞ぐ。
func TestTemplate_HasNoUnreplaceableVariants(t *testing.T) {
	variants := []string{"go_service", "GoService", "goService", "GO_SERVICE", "goservice", "Go-Service"}
	err := filepath.WalkDir(realTemplate, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, v := range variants {
			if strings.Contains(string(b), v) {
				t.Errorf("%s に %q がある。置き換えられないので go-service と書く", path, v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// **git が無視するファイルは写さない** (.DS_Store・.env・試しに作ったバイナリ)。
// 未追跡でも無視されていないファイルは写す (ひな形に足した直後に試せるように)。
func TestGenerate_SkipsGitIgnoredFiles(t *testing.T) {
	tpl := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(tpl, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", ".DS_Store\n.env\n/api\n")
	write("go.mod", "module example.com/go-service\n")
	write("cmd/api/main.go", "package main\n")
	write("new-untracked.txt", "go-service\n")
	write(".DS_Store", "x")
	write(".env", "SECRET=1\n")
	write("api", "\x00binary")

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", tpl}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".gitignore", "go.mod", "cmd/api/main.go")

	out := filepath.Join(t.TempDir(), "svc")
	if _, err := generate(options{name: "svc", module: "example.com/svc", template: tpl, out: out}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".gitignore", "go.mod", "cmd/api/main.go", "new-untracked.txt"} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("%s が写っていない: %v", want, err)
		}
	}
	for _, unwanted := range []string{".DS_Store", ".env", "api"} {
		if _, err := os.Stat(filepath.Join(out, unwanted)); err == nil {
			t.Errorf("git が無視する %s まで写っている", unwanted)
		}
	}
}
