package model

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
)

// **用途によって出力形式が変わる** (docs/adr/0007-image-storage.md 決定 6)。
//
// 純 Go の WebP エンコーダは可逆 (VP8L) しか出せない。
// 写真を可逆で保存すると JPEG の 6.7 倍の容量になる (決定 6 の実測)。
// ここが逆になると、コメント添付の帯域が一気に膨らむ。
func TestKind_Format(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind Kind
		want Format
	}{
		{KindCommentAttachment, FormatJPEG},
		{KindAvatar, FormatWebP},
		{KindThreadIcon, FormatWebP},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			t.Parallel()

			if got := tt.kind.Format(); got != tt.want {
				t.Errorf("Format() = %q, want %q", got, tt.want)
			}
		})
	}
}

// 形式と Content-Type / 拡張子の対応。
//
// **DB の CHECK 制約 (images_content_type_valid) と揃っている必要がある。**
// ずれると保存の瞬間に 500 になる。
func TestFormat_ContentTypeAndExtension(t *testing.T) {
	t.Parallel()

	if got := FormatJPEG.ContentType(); got != "image/jpeg" {
		t.Errorf("JPEG の ContentType = %q", got)
	}
	if got := FormatWebP.ContentType(); got != "image/webp" {
		t.Errorf("WebP の ContentType = %q", got)
	}
	if got := FormatJPEG.Extension(); got != ".jpg" {
		t.Errorf("JPEG の Extension = %q", got)
	}
	if got := FormatWebP.Extension(); got != ".webp" {
		t.Errorf("WebP の Extension = %q", got)
	}

	// 未知の形式は空を返す。**既定値に倒さない** ——
	// image/jpeg へ倒すと、別形式のバイト列に JPEG の Content-Type が付く。
	var unknown Format = "avif"
	if got := unknown.ContentType(); got != "" {
		t.Errorf("未知の形式の ContentType = %q, want 空", got)
	}
	if got := unknown.Extension(); got != "" {
		t.Errorf("未知の形式の Extension = %q, want 空", got)
	}
}

func TestKind_Valid(t *testing.T) {
	t.Parallel()

	for _, k := range []Kind{KindCommentAttachment, KindAvatar, KindThreadIcon} {
		if !k.Valid() {
			t.Errorf("%q が無効と判定された", k)
		}
	}
	for _, k := range []Kind{"", "comment", "COMMENT_ATTACHMENT", "banner"} {
		if k.Valid() {
			t.Errorf("%q が有効と判定された", k)
		}
	}
}

// アバターとアイコンの長辺を小さく抑えること。
// 可逆圧縮の容量は画素数にほぼ比例するため、ここが緩むと容量が効く。
func TestKind_MaxDimension(t *testing.T) {
	t.Parallel()

	if KindAvatar.MaxDimension() >= KindCommentAttachment.MaxDimension() {
		t.Errorf("アバター (%d) がコメント添付 (%d) より小さくない",
			KindAvatar.MaxDimension(), KindCommentAttachment.MaxDimension())
	}
	if KindThreadIcon.MaxDimension() != KindAvatar.MaxDimension() {
		t.Error("アイコンとアバターの上限が揃っていない")
	}
}

// ---------------------------------------------------------------------------
// NewPending
// ---------------------------------------------------------------------------

func TestNewPending(t *testing.T) {
	t.Parallel()

	img, err := NewPending(42, KindCommentAttachment, 1200, 800, 143_000)
	if err != nil {
		t.Fatalf("NewPending が失敗した: %v", err)
	}

	if img.Status != StatusPending {
		t.Errorf("Status = %q, want pending", img.Status)
	}
	// **確定していない画像に committed_at を入れない。**
	// DB の CHECK 制約 (images_committed_at_matches_status) が拒否する。
	if img.CommittedAt != nil {
		t.Error("pending なのに CommittedAt が入っている")
	}
	if img.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg (コメント添付は JPEG)", img.ContentType)
	}

	// キーは ID から作る。**利用者の入力を含めない** (ADR 0007 決定 3)。
	if want := "images/" + img.ID.String() + ".jpg"; img.ObjectKey != want {
		t.Errorf("ObjectKey = %q, want %q", img.ObjectKey, want)
	}

	// UUID v7 であること。先頭が時刻なので B-tree の挿入位置が末尾に寄る。
	if v := img.ID.Version(); v != 7 {
		t.Errorf("UUID version = %d, want 7", v)
	}
}

// アバターは WebP になり、拡張子も揃うこと。
func TestNewPending_AvatarIsWebP(t *testing.T) {
	t.Parallel()

	img, err := NewPending(1, KindAvatar, 256, 256, 3_000)
	if err != nil {
		t.Fatalf("NewPending が失敗した: %v", err)
	}
	if img.ContentType != "image/webp" {
		t.Errorf("ContentType = %q, want image/webp", img.ContentType)
	}
	if !strings.HasSuffix(img.ObjectKey, ".webp") {
		t.Errorf("ObjectKey = %q, want .webp で終わる", img.ObjectKey)
	}
}

// **キーが毎回違うこと。** 同じなら他人のオブジェクトを上書きできる。
func TestNewPending_KeysAreUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for range 50 {
		img, err := NewPending(1, KindAvatar, 64, 64, 100)
		if err != nil {
			t.Fatalf("NewPending が失敗した: %v", err)
		}
		if seen[img.ObjectKey] {
			t.Fatalf("キーが重複した: %s", img.ObjectKey)
		}
		seen[img.ObjectKey] = true
	}
}

// **匿名は画像を投稿できない** (ADR 0007 の背景)。
//
// 匿名で任意のバイト列をストレージに置けると、容量の消費と
// 違法コンテンツの設置が追跡不能な形で可能になる。
// ここは 400 ではなく 401 で返す —— ログインすれば解決するため。
func TestNewPending_RequiresOwner(t *testing.T) {
	t.Parallel()

	for _, ownerID := range []int64{0, -1} {
		_, err := NewPending(ownerID, KindAvatar, 64, 64, 100)
		if !errors.Is(err, apperr.ErrUnauthenticated) {
			t.Errorf("ownerID=%d: err = %v, want apperr.ErrUnauthenticated", ownerID, err)
		}
	}
}

func TestNewPending_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		kind     Kind
		width    int
		height   int
		byteSize int64
	}{
		{name: "未知の用途", kind: "banner", width: 10, height: 10, byteSize: 1},
		{name: "幅が 0", kind: KindAvatar, width: 0, height: 10, byteSize: 1},
		{name: "高さが 0", kind: KindAvatar, width: 10, height: 0, byteSize: 1},
		{name: "幅が負", kind: KindAvatar, width: -1, height: 10, byteSize: 1},
		// 0 バイトは「PUT したつもりで何も書けていない」状態。
		{name: "空のバイト列", kind: KindAvatar, width: 10, height: 10, byteSize: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewPending(1, tt.kind, tt.width, tt.height, tt.byteSize)
			if !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Commit
// ---------------------------------------------------------------------------

// **status と committed_at は必ず同時に動く。**
// 片方だけ書こうとすると DB の CHECK 制約が拒否する。
func TestImage_Commit(t *testing.T) {
	t.Parallel()

	img, err := NewPending(1, KindAvatar, 64, 64, 100)
	if err != nil {
		t.Fatalf("NewPending が失敗した: %v", err)
	}

	at := time.Unix(1_700_000_000, 0).UTC()
	img.Commit(at)

	if img.Status != StatusCommitted {
		t.Errorf("Status = %q, want committed", img.Status)
	}
	if img.CommittedAt == nil {
		t.Fatal("CommittedAt が入っていない")
	}
	if !img.CommittedAt.Equal(at) {
		t.Errorf("CommittedAt = %v, want %v", *img.CommittedAt, at)
	}
}

// 上限の値そのものを固定する。
//
// **画素数の上限を別に持つことが要点** (ADR 0007 決定 2)。
// バイト数だけでは decompression bomb を防げない。
func TestUploadLimits(t *testing.T) {
	t.Parallel()

	if MaxUploadBytes != 5<<20 {
		t.Errorf("MaxUploadBytes = %d, want 5 MiB", MaxUploadBytes)
	}
	// **ADR 0007 決定 2 の「5000 万」から下げてある。**
	// 5000 万画素は RGBA で 191 MB / 枚になり、5 MiB の入力上限を
	// 守ったまま到達できる (平坦な PNG)。同時 10 件で 1.9 GB。
	if MaxPixels != 25_000_000 {
		t.Errorf("MaxPixels = %d, want 2500 万", MaxPixels)
	}

	// 上限どうしの関係を固定する。**バイト数の上限が画素数の上限を
	// 含意しないこと**がこの 2 つを別に持つ理由になる。
	// 1 画素 4 バイトで見ても、MaxUploadBytes は MaxPixels に遠く届かない。
	if MaxUploadBytes >= MaxPixels {
		t.Error("バイト数の上限が画素数の上限を上回っている (画素数の検査が無意味になる)")
	}
}

// **画像の定義が 3 か所で一致していること。**
//
//	image.go の定数宣言 (ソースを走査する)
//	000006 の CHECK 制約
//	openapi.yaml の enum
//
// **上の TestFormat_ContentTypeAndExtension は、これを守っていなかった。**
// 「DB の CHECK 制約 (images_content_type_valid) と揃っている必要がある」と
// 書きながら、比較していたのはテスト内のリテラル同士でしかない。
// 制約から image/webp を消してもあのテストは緑のままになる ——
// role_test.go が「構造上落ちない」として一度直したのと同じ形が、
// このパッケージに残っていた。
//
// **守るには変わる側から読むしかない。** ここでは 3 つの出所を解析して
// 突き合わせる。ソースを読むテストは行儀が良くないが、この不一致は
// 「保存できない値をアプリが作る」か「アプリが知らない値が DB に入る」形で
// 本番に出る。検出できる場所が他に無い (ADR 0003 #8)。
func TestImageDefinitions_AgreeAcrossSources(t *testing.T) {
	t.Parallel()

	migration := readRepoFile(t, "apps/go-api/db/migrations/000006_add_images.up.sql")
	spec := readRepoFile(t, "api/openapi.yaml")
	src := readRepoFile(t, "apps/go-api/internal/image/domain/model/image.go")

	t.Run("kind", func(t *testing.T) {
		t.Parallel()

		declared := valuesFromSource(t, src, `(?m)^\s*(\w+)\s+Kind\s*=\s*"([^"]+)"`)
		assertSameSet(t, "DB の CHECK 制約",
			declared, valuesInList(t, migration, `CHECK \(kind IN \(([^)]+)\)\)`, `'([^']+)'`))
		assertSameSet(t, "仕様書の enum",
			declared, valuesInList(t, spec,
				`(?ms)^    ImageKind:\n.*?\n      enum:\n((?:\s*- \w+\n)+)`, `- (\w+)`))
	})

	t.Run("status", func(t *testing.T) {
		t.Parallel()

		// **仕様書には無い。** status は API に出さないため
		// (利用者から見えるのは「画像がある / 無い」だけになる)。
		// 2 か所しか無いことを、無いまま突き合わせる。
		declared := valuesFromSource(t, src, `(?m)^\s*(\w+)\s+Status\s*=\s*"([^"]+)"`)
		assertSameSet(t, "DB の CHECK 制約",
			declared, valuesInList(t, migration, `CHECK \(status IN \(([^)]+)\)\)`, `'([^']+)'`))
	})

	t.Run("content_type", func(t *testing.T) {
		t.Parallel()

		// **Format の定数ではなく ContentType() の出力を集める。**
		// DB に入るのはこちらであり、定数値 ("jpeg") ではない。
		//
		// **一覧を手で書かない** (valuesFromSource と同じ理由)。
		// []Format{FormatJPEG, FormatWebP} と書くと、形式を足したときに
		// この行を直さない限り古い 2 つのままになり、
		// **新しい形式の Content-Type だけが CHECK 制約に無い**状態を
		// 素通しする。宣言はソースから取る。
		produced := map[string]bool{}
		for v := range valuesFromSource(t, src, `(?m)^\s*(\w+)\s+Format\s*=\s*"([^"]+)"`) {
			produced[Format(v).ContentType()] = true
		}
		if produced[""] {
			t.Error("ContentType() が空を返す Format がある (対応を書き忘れている)")
		}
		assertSameSet(t, "DB の CHECK 制約",
			produced, valuesInList(t, migration,
				`CHECK \(content_type IN \(([^)]+)\)\)`, `'([^']+)'`))
	})
}

// valuesFromSource は「定数名 定数型 = "値"」の宣言を走査して値の集合を返します。
//
// **リテラルの一覧を書かない**のは、定数を足したときに気づくためです
// (書き写す形にすると、増えた定数は永久に検査されません)。
func valuesFromSource(t *testing.T, src, pattern string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(src, -1) {
		out[m[2]] = true
	}
	if len(out) == 0 {
		t.Fatalf("%q に一致する宣言が 1 つも無い (書き方が変わった?)", pattern)
	}
	return out
}

// valuesInList は block で囲みを取り出し、その中から item を全部拾います。
func valuesInList(t *testing.T, src, block, item string) map[string]bool {
	t.Helper()

	m := regexp.MustCompile(block).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%q を読み取れなかった", block)
	}
	out := map[string]bool{}
	for _, v := range regexp.MustCompile(item).FindAllStringSubmatch(m[1], -1) {
		out[v[1]] = true
	}
	if len(out) == 0 {
		t.Fatalf("%q の中身が空だった", block)
	}
	return out
}

func assertSameSet(t *testing.T, label string, want, got map[string]bool) {
	t.Helper()

	for v := range want {
		if !got[v] {
			t.Errorf("%s に %q が無い", label, v)
		}
	}
	for v := range got {
		if !want[v] {
			t.Errorf("%s にだけ %q がある (定数の側が知らない値)", label, v)
		}
	}
}

// readRepoFile はリポジトリ直下からの相対パスでファイルを読みます。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	// このファイルは apps/go-api/internal/image/domain/model にある。
	root := filepath.Join("..", "..", "..", "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("%s を読めない: %v", rel, err)
	}
	return string(b)
}

// **条件つき制約が、こちらの知っている値だけを使っていること。**
//
//	images_committed_at_matches_status  (status = 'pending' ...)
//	images_attached_at_requires_commit  (status <> 'pending')
//
// 列挙の CHECK 制約と違い、**値が本文に直書きされています。**
// そのため Status を改名すると、`images_status_valid` 側は上の検査が
// 落ちて気づけるのに、**こちらは古い値を参照したまま残ります。**
//
// そのとき壊れるのは「その状態の行だけが保存できない」という形になり、
// 経路を踏むまで表に出ません (ADR 0003 #8)。
func TestConditionalConstraints_UseKnownValues(t *testing.T) {
	t.Parallel()

	src := readRepoFile(t, "apps/go-api/internal/image/domain/model/image.go")
	known := valuesFromSource(t, src, `(?m)^\s*(\w+)\s+Status\s*=\s*"([^"]+)"`)

	for _, name := range []string{
		"images_committed_at_matches_status",
		"images_attached_at_requires_commit",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, v := range literalsIn(t, latestConstraintSQL(t, name), name) {
				if !known[v] {
					t.Errorf("%s が知らない値 %q を使っている", name, v)
				}
			}
		})
	}
}

// latestConstraintSQL は、その制約を**最後に定義しているマイグレーション**を返します。
//
// **ファイル名を直書きしない。** マイグレーションは前へ直す方式なので、
// 制約は後の版で DROP されて別の定義に置き換わる ——
// 実際 images_committed_at_matches_status は 000006 で作られ、
// 000013 で張り替えられている ('pending' の画像を削除できるようにするため)。
//
// 直書きしていると、**死んだ定義を検査し続けて緑のまま**になる。
// 生きている定義のほうは誰も見ていない、という形になります。
func latestConstraintSQL(t *testing.T, constraint string) string {
	t.Helper()

	root := filepath.Join("..", "..", "..", "..", "..", "..")
	// **"db/migrations/" を文字列として残す。**
	// .github/scripts/verify-constraint-checks.py が「この検査は本当に
	// マイグレーションを読んでいるか」をこの部分文字列で見ている。
	// 分割して組み立てると、読んでいるのに「読んでいない」と鳴る。
	dir := filepath.Join(root, "apps/go-api/db/migrations/")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("マイグレーションを読めない: %v", err)
	}

	head := regexp.MustCompile(`CONSTRAINT\s+` + constraint + `\s+CHECK\s*\(`)
	latest := ""
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s を読めない: %v", name, err)
		}
		// **コメントを落としてから探す。** マイグレーションの説明文には
		// 旧定義をそのまま貼ることがある (000013 がそう)。落とさないと
		// **コメント内の死んだ定義を検査してしまう。**
		body := stripSQLComments(string(b))
		if head.MatchString(body) {
			latest = body
		}
	}
	if latest == "" {
		t.Fatalf("%s を定義しているマイグレーションが無い", constraint)
	}
	return latest
}

// stripSQLComments は行コメント (--) を落とします。
func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// literalsIn は名前つき CHECK 制約の本文から、引用符つきの値を全部拾います。
//
// **括弧の対応を数えます。** 条件つき制約は入れ子になっているため、
// 最初の閉じ括弧までを取ると途中で切れます。
func literalsIn(t *testing.T, sql, constraint string) []string {
	t.Helper()

	head := regexp.MustCompile(`CONSTRAINT\s+` + constraint + `\s+CHECK\s*\(`).
		FindStringIndex(sql)
	if head == nil {
		t.Fatalf("%s を読み取れなかった", constraint)
	}
	start := head[1] - 1
	depth, end := 0, -1
	for i := start; i < len(sql); i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		t.Fatalf("%s の括弧が閉じていない", constraint)
	}

	var out []string
	for _, m := range regexp.MustCompile(`'([^']*)'`).
		FindAllStringSubmatch(sql[start:end], -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s からリテラルを 1 つも拾えなかった", constraint)
	}
	return out
}
