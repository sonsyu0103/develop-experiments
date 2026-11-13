// Command new-service は templates/go-service から新しい Go サービスを作る。
//
//	go run ./tools/new-service -name billing-api -module github.com/acme/billing-api -out ../billing-api
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
	flag.StringVar(&o.template, "template", "templates/go-service", "ひな形のディレクトリ")
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
		return 0, fmt.Errorf("ひな形 %s が見つからない (リポジトリ直下で実行する)", o.template)
	}

	// **1 回の走査で置き換える。** 2 回に分けると、-module に go-service が
	// 含まれるとき (github.com/acme/go-service-x など) 1 回目の結果を
	// 2 回目が書き換えてしまう。strings.Replacer は置き換え後の文字列を再走査しない。
	r := strings.NewReplacer(placeholderModule, o.module, placeholderName, o.name)

	count := 0
	err = filepath.WalkDir(o.template, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(o.template, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(o.out, r.Replace(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: 通常のファイル以外はひな形に置かない", rel)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return fmt.Errorf("%s: バイナリはひな形に置かない (置き換えで壊れる)", rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		count++
		return os.WriteFile(dst, []byte(r.Replace(string(b))), fi.Mode().Perm())
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
