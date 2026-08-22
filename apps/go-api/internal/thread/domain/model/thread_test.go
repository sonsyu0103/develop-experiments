package model

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
)

func TestNewThread(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		title     string
		wantTitle string
		wantErr   error
	}{
		{
			name:      "通常のタイトル",
			title:     "並列処理のテスト空間",
			wantTitle: "並列処理のテスト空間",
		},
		{
			name:      "前後の空白は除去される",
			title:     "  余白つき  ",
			wantTitle: "余白つき",
		},
		{
			name:      "上限ちょうどの長さは通る",
			title:     strings.Repeat("あ", TitleMaxLength),
			wantTitle: strings.Repeat("あ", TitleMaxLength),
		},
		{
			name:    "空文字は拒否される",
			title:   "",
			wantErr: apperr.ErrInvalidArgument,
		},
		{
			name:    "空白のみは拒否される",
			title:   "   \t\n ",
			wantErr: apperr.ErrInvalidArgument,
		},
		{
			// バイト数ではなく文字数で数えていることの確認。
			// マルチバイト文字を上限+1 個にする。
			name:    "上限を超える文字数は拒否される",
			title:   strings.Repeat("あ", TitleMaxLength+1),
			wantErr: apperr.ErrInvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewThread(tt.title, nil, nil)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("エラー %v を期待したが、got err=%v", tt.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("エラーは期待していないが発生した: %v", err)
			}
			if got.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tt.wantTitle)
			}
			// ID と CreatedAt は DB が採番するので、未設定であるべき。
			if got.ID != 0 {
				t.Errorf("ID = %d, want 0 (永続化前なので未採番)", got.ID)
			}
			if !got.CreatedAt.IsZero() {
				t.Errorf("CreatedAt = %v, want ゼロ値 (永続化前なので未設定)", got.CreatedAt)
			}
		})
	}
}

func TestReconstruct(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	got := Reconstruct(42, "復元されたスレッド", nil, nil, createdAt, 0)

	if got.ID != 42 {
		t.Errorf("ID = %d, want 42", got.ID)
	}
	if got.Title != "復元されたスレッド" {
		t.Errorf("Title = %q, want %q", got.Title, "復元されたスレッド")
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, createdAt)
	}
}

// Reconstruct は保存済みデータの復元なので、
// NewThread が弾く値でも検証せずに通すことを明示しておく。
func TestReconstruct_SkipsValidation(t *testing.T) {
	t.Parallel()

	got := Reconstruct(1, "", nil, nil, time.Now(), 0)
	if got.Title != "" {
		t.Errorf("Title = %q, want 空文字 (復元時は検証しない)", got.Title)
	}
}

// 投稿者の紐付け。nil が匿名を意味する (ADR 0005 決定 2)。
func TestNewThread_AuthorID(t *testing.T) {
	t.Parallel()

	authorID := int64(42)

	withAuthor, err := NewThread("ログインして立てたスレッド", &authorID, nil)
	if err != nil {
		t.Fatalf("NewThread が失敗した: %v", err)
	}
	if withAuthor.AuthorID == nil || *withAuthor.AuthorID != authorID {
		t.Errorf("AuthorID = %v, want %d", withAuthor.AuthorID, authorID)
	}

	anonymous, err := NewThread("匿名で立てたスレッド", nil, nil)
	if err != nil {
		t.Fatalf("NewThread が失敗した: %v", err)
	}
	if anonymous.AuthorID != nil {
		t.Errorf("AuthorID = %v, want nil (匿名)", *anonymous.AuthorID)
	}
}

// **タイトルの上限が 3 か所で一致していること。**
//
//	この定数 / DB の CHECK 制約 (000001) / 仕様書の maxLength
//
// 揃っていないと「アプリが通した入力を DB が拒否して 500」または
// 「仕様書より長い入力が通る」形で表に出ます。
// contact の TestLengthLimits_AgreeAcrossSources と同じ考え方です
// (ADR 0003 #8)。
//
// **検索語の上限も一緒に見ます。** SearchQueryMaxLength は
// TitleMaxLength から導いているので、タイトルを変えると仕様書側の
// SearchQuery も同時に変える必要があります。片方だけ直すと、
// 「どのタイトルにも一致しない検索語」を受け付ける状態に戻ります。
func TestTitleLengthLimit_AgreesAcrossSources(t *testing.T) {
	t.Parallel()

	migration := readRepoFile(t, "apps/go-api/db/migrations/000001_init_schema.up.sql")
	spec := readRepoFile(t, "api/openapi.yaml")

	if got := intFrom(t, migration,
		`char_length\(title\)\s+BETWEEN 1 AND (\d+)`); got != TitleMaxLength {
		t.Errorf("DB の CHECK 制約 = %d, Go 側 = %d", got, TitleMaxLength)
	}
	if got := intFrom(t, spec,
		`(?ms)^    CreateThreadRequest:\n.*?^        title:\n.*?maxLength: (\d+)`,
	); got != TitleMaxLength {
		t.Errorf("仕様書の maxLength = %d, Go 側 = %d", got, TitleMaxLength)
	}
	if got := intFrom(t, spec,
		`(?ms)^    SearchQuery:\n.*?maxLength: (\d+)`); got != SearchQueryMaxLength {
		t.Errorf("仕様書の SearchQuery = %d, Go 側 = %d", got, SearchQueryMaxLength)
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

	// このファイルは apps/go-api/internal/thread/domain/model にある。
	root := filepath.Join("..", "..", "..", "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("%s を読めない: %v", rel, err)
	}
	return string(b)
}
