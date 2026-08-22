package model

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
)

func TestNewComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		threadID   int64
		authorName string
		body       string
		wantAuthor string
		wantBody   string
		wantErr    bool
	}{
		{
			name:       "通常の投稿",
			threadID:   1,
			authorName: "ホシノ",
			body:       "ふぁ〜、眠いよ〜",
			wantAuthor: "ホシノ",
			wantBody:   "ふぁ〜、眠いよ〜",
		},
		{
			name:       "投稿者名が空なら既定値になる",
			threadID:   1,
			authorName: "",
			body:       "本文",
			wantAuthor: DefaultAuthorName,
			wantBody:   "本文",
		},
		{
			name:       "投稿者名が空白のみでも既定値になる",
			threadID:   1,
			authorName: "   ",
			body:       "本文",
			wantAuthor: DefaultAuthorName,
			wantBody:   "本文",
		},
		{
			name:       "前後の空白は除去される",
			threadID:   1,
			authorName: "  名前  ",
			body:       "  本文  ",
			wantAuthor: "名前",
			wantBody:   "本文",
		},
		{
			name:       "上限ちょうどの本文は通る",
			threadID:   1,
			authorName: "x",
			body:       strings.Repeat("あ", BodyMaxLength),
			wantAuthor: "x",
			wantBody:   strings.Repeat("あ", BodyMaxLength),
		},

		{name: "スレッド ID が 0", threadID: 0, body: "本文", wantErr: true},
		{name: "スレッド ID が負", threadID: -1, body: "本文", wantErr: true},
		{name: "本文が空", threadID: 1, body: "", wantErr: true},
		{name: "本文が空白のみ", threadID: 1, body: "  \n ", wantErr: true},
		{
			name:     "本文が長すぎる",
			threadID: 1,
			body:     strings.Repeat("あ", BodyMaxLength+1),
			wantErr:  true,
		},
		{
			name:       "投稿者名が長すぎる",
			threadID:   1,
			authorName: strings.Repeat("あ", AuthorNameMaxLength+1),
			body:       "本文",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewComment(tt.threadID, tt.authorName, tt.body, nil, nil)

			if tt.wantErr {
				if !errors.Is(err, apperr.ErrInvalidArgument) {
					t.Fatalf("err = %v, want apperr.ErrInvalidArgument", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("エラーは期待していないが発生した: %v", err)
			}
			if got.AuthorName != tt.wantAuthor {
				t.Errorf("AuthorName = %q, want %q", got.AuthorName, tt.wantAuthor)
			}
			if got.Body != tt.wantBody {
				t.Errorf("Body = %q, want %q", got.Body, tt.wantBody)
			}
			if got.ThreadID != tt.threadID {
				t.Errorf("ThreadID = %d, want %d", got.ThreadID, tt.threadID)
			}
			// ID と CreatedAt は DB が採番する。
			if got.ID != 0 {
				t.Errorf("ID = %d, want 0 (永続化前なので未採番)", got.ID)
			}
		})
	}
}

// **ログイン中はリクエストの投稿者名を捨てる。**
//
// 保存してしまうと「投稿時点の表示名」が残り、
// 表示名を変えても過去の投稿に反映されなくなる。
// ADR 0014 は即時反映を選んでいるので、表示に使うのは users 側の表示名になる。
//
// 捨てないと、他人の名前を騙った投稿をログイン状態で作れてしまう
// (表示側が author_name を優先した場合)。
func TestNewComment_LoggedInDiscardsAuthorName(t *testing.T) {
	t.Parallel()

	authorID := int64(42)

	got, err := NewComment(1, "別人を名乗る", "本文", &authorID, nil)
	if err != nil {
		t.Fatalf("NewComment が失敗した: %v", err)
	}

	if got.AuthorName != DefaultAuthorName {
		t.Errorf("AuthorName = %q, want %q (ログイン中は捨てる)", got.AuthorName, DefaultAuthorName)
	}
	if got.AuthorID == nil || *got.AuthorID != authorID {
		t.Errorf("AuthorID = %v, want %d", got.AuthorID, authorID)
	}
}

// 匿名投稿では従来どおり投稿者名が残る。
// ログイン側だけを見ると「常に捨てる」実装でも通ってしまう。
func TestNewComment_AnonymousKeepsAuthorName(t *testing.T) {
	t.Parallel()

	got, err := NewComment(1, "ホシノ", "本文", nil, nil)
	if err != nil {
		t.Fatalf("NewComment が失敗した: %v", err)
	}

	if got.AuthorName != "ホシノ" {
		t.Errorf("AuthorName = %q, want ホシノ", got.AuthorName)
	}
	if got.AuthorID != nil {
		t.Errorf("AuthorID = %v, want nil (匿名)", *got.AuthorID)
	}
}

// **匿名は画像を添付できない** (docs/adr/0007-image-storage.md の背景)。
//
// 冪等キー (匿名では無視する) と違い、こちらはエラーにします ——
// 無視すると、利用者からは「添付したのに画像が消えた」としか見えません。
//
// HTTP 層にも同じ検査がありますが、**ドメインの不変条件を HTTP 層に
// 預けない**ためにここでも弾きます。
func TestNewComment_AnonymousCannotAttachImage(t *testing.T) {
	t.Parallel()

	imageID := uuid.New()

	_, err := NewComment(1, "名無しさん", "画像つき", nil, &imageID)
	if !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want apperr.ErrUnauthenticated", err)
	}
}

// ログイン中なら添付できること。
// 上のテストだけだと「常に拒否する」実装でも通ってしまいます。
func TestNewComment_LoggedInCanAttachImage(t *testing.T) {
	t.Parallel()

	imageID := uuid.New()
	authorID := int64(42)

	c, err := NewComment(1, "", "画像つき", &authorID, &imageID)
	if err != nil {
		t.Fatalf("NewComment が失敗した: %v", err)
	}
	if c.ImageID == nil || *c.ImageID != imageID {
		t.Errorf("ImageID = %v, want %v", c.ImageID, imageID)
	}
}

// **本文と投稿者名の上限が 3 か所で一致していること。**
//
//	この定数群 / DB の CHECK 制約 (000001) / 仕様書の maxLength
//
// 揃っていないと「アプリが通した入力を DB が拒否して 500」または
// 「仕様書より長い入力が通る」形で表に出ます (ADR 0003 #8)。
//
// **DB は char_length、Go は utf8.RuneCountInString で、どちらも
// コードポイントを数えます。** 数え方が違うと、同じ数字でも境界がずれます。
func TestLengthLimits_AgreeAcrossSources(t *testing.T) {
	t.Parallel()

	migration := readRepoFile(t, "apps/go-api/db/migrations/000001_init_schema.up.sql")
	spec := readRepoFile(t, "api/openapi.yaml")

	cases := []struct {
		field      string
		declared   int
		constraint string
	}{
		{"body", BodyMaxLength, `char_length\(body\)\s+BETWEEN 1 AND (\d+)`},
		{"authorName", AuthorNameMaxLength, `char_length\(author_name\)\s+BETWEEN 1 AND (\d+)`},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			if got := intFrom(t, migration, tc.constraint); got != tc.declared {
				t.Errorf("DB の CHECK 制約 (%s) = %d, Go 側 = %d", tc.field, got, tc.declared)
			}
			pattern := `(?ms)^    CreateCommentRequest:\n.*?^        ` + tc.field +
				`:\n.*?maxLength: (\d+)`
			if got := intFrom(t, spec, pattern); got != tc.declared {
				t.Errorf("仕様書の maxLength (%s) = %d, Go 側 = %d", tc.field, got, tc.declared)
			}
		})
	}
}

func intFrom(t *testing.T, src, pattern string) int {
	t.Helper()

	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%q に一致しなかった", pattern)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%q を整数として読めない: %v", m[1], err)
	}
	return n
}

// readRepoFile はリポジトリ直下からの相対パスでファイルを読みます。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	// このファイルは apps/go-api/internal/comment/domain/model にある。
	root := filepath.Join("..", "..", "..", "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("%s を読めない: %v", rel, err)
	}
	return string(b)
}
