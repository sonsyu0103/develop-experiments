// Command new-service は templates/go-service から新しい Go サービスを作る。
//
//	make new-service NAME=billing-api MODULE=github.com/acme/billing-api   # リポジトリ直下から
//	cd tools/new-service && go run . -name billing-api -out ../../../billing-api
//
// **go run はこのディレクトリの中で叩く。** リポジトリ直下には go.mod も go.work も無い。
// -template の既定値も、このディレクトリから見た相対パスにしてある。
//
// **ひな形は `{{ }}` を埋め込んだテンプレートではなく、そのままビルドできるサービス**
// にしてある (gonew と同じ考え方)。ここでやるのは 2 つの文字列の置き換えだけ:
//
//	example.com/go-service → -module の値 (Go のモジュールパス)
//	go-service             → -name の値   (Docker・k8s・ログに出る名前)
//
// こうしておくと、ひな形そのものに lint とテストをかけられる。
// テンプレート言語で書くと、生成してみるまで壊れているか分からない。
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	placeholderModule = "example.com/go-service"
	placeholderName   = "go-service"
)

// k8s の名前 (Namespace・Service・Deployment) に使うので、DNS-1123 label に収める:
// 63 文字まで、英小文字・数字・ハイフン、末尾はハイフン以外。
//
// **先頭を英字に限るのは k8s の要求ではなく、こちらの方針。** Service 名は以前
// DNS-1035 (先頭が英字) だったが、kubectl v1.37 のクラスタでは数字始まりの
// Namespace も Service も通った (2026-11-14 実測)。古いクラスタや
// 他の道具 (Docker のタグ、ログの集計) でも困らない側に倒している。
var namePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Go のモジュールパスとして最低限おかしくないか。厳密な検査は go mod tidy に任せる。
var modulePattern = regexp.MustCompile(`^[a-zA-Z0-9._~/-]+$`)

type options struct {
	name     string
	module   string
	template string
	out      string
}

func main() {
	var o options
	flag.StringVar(&o.name, "name", "", "サービス名 (例: billing-api)。k8s の名前にも使う")
	flag.StringVar(&o.module, "module", "", "Go のモジュールパス (既定: example.com/<name>)")
	flag.StringVar(&o.template, "template", "../../templates/go-service", "ひな形のディレクトリ (既定は tools/new-service から見た位置)")
	flag.StringVar(&o.out, "out", "", "生成先 (空か、存在しないディレクトリ)")
	flag.Parse()

	if o.module == "" && o.name != "" {
		o.module = "example.com/" + o.name
	}
	n, err := generate(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "new-service:", err)
		os.Exit(1)
	}
	fmt.Printf("%s に %d ファイルを生成した (module %s)\n\n", o.out, n, o.module)
	fmt.Printf("次にやること:\n  cd %s\n  git init && git add -A && git commit -m 'new-service %s'\n  make test && make image\n", o.out, o.name)
}

func validate(o options) error {
	if !namePattern.MatchString(o.name) {
		return fmt.Errorf("-name %q は使えない: 英小文字で始まり、英小文字・数字・ハイフンだけの 63 文字以内 (末尾はハイフン以外)", o.name)
	}
	if !modulePattern.MatchString(o.module) || strings.HasPrefix(o.module, "/") || strings.HasSuffix(o.module, "/") {
		return fmt.Errorf("-module %q はモジュールパスとして使えない", o.module)
	}
	if o.out == "" {
		return errors.New("-out を指定する")
	}
	entries, err := os.ReadDir(o.out)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case len(entries) > 0:
		// **上書きしない。** 既存のサービスに重ねると、手で直したファイルが黙って消える。
		return fmt.Errorf("%s は空ではない。上書きはしない", o.out)
	}
	return nil
}

// generate はひな形を out に写し、置き換えたファイル数を返す。
func generate(o options) (int, error) {
	if err := validate(o); err != nil {
		return 0, err
	}
	info, err := os.Stat(o.template)
	if err != nil || !info.IsDir() {
		return 0, fmt.Errorf("ひな形 %s が見つからない (tools/new-service の中で実行するか、-template を指定する)", o.template)
	}

	// **1 回の走査で置き換える。** 2 回に分けると、-module に go-service が
	// 含まれるとき (github.com/acme/go-service-x など) 1 回目の結果を
	// 2 回目が書き換えてしまう。strings.Replacer は置き換え後の文字列を再走査しない。
	r := strings.NewReplacer(placeholderModule, o.module, placeholderName, o.name)

	files, err := listFiles(o.template)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, rel := range files {
		src := filepath.Join(o.template, rel)
		fi, err := os.Lstat(src)
		if errors.Is(err, fs.ErrNotExist) {
			// git には載っているが作業ツリーで消したファイル。消した側を正とする。
			continue
		}
		if err != nil {
			return 0, err
		}
		if !fi.Mode().IsRegular() {
			return 0, fmt.Errorf("%s: 通常のファイル以外はひな形に置かない", rel)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return 0, err
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return 0, fmt.Errorf("%s: バイナリはひな形に置かない (置き換えで壊れる)", rel)
		}
		dst := filepath.Join(o.out, r.Replace(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(dst, []byte(r.Replace(string(b))), fi.Mode().Perm()); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

// listFiles は写すファイルを、ひな形からの相対パスで返す。
//
// **git の管理下なら、git が無視するファイルは写さない。** ディレクトリを丸ごと歩くと、
// Finder が作る .DS_Store、手元の .env、ひな形の中で試しに作ったバイナリまで
// 生成先に写る (レビュー指摘)。CI はクリーンな checkout なので、この経路は
// CI では見えない。
//
// 未追跡でも無視されていないファイル (--others --exclude-standard) は含める。
// ひな形にファイルを足した直後、コミット前に試せるようにするため。
// git の外 (ひな形だけを配った場合など) では、ディレクトリを丸ごと歩く。
func listFiles(template string) ([]string, error) {
	out, err := exec.Command("git", "-C", template, "ls-files", "-z",
		"--cached", "--others", "--exclude-standard").Output()
	if err == nil {
		var files []string
		seen := map[string]bool{}
		for _, f := range strings.Split(string(out), "\x00") {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			files = append(files, filepath.FromSlash(f))
		}
		return files, nil
	}

	var files []string
	err = filepath.WalkDir(template, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(template, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}
