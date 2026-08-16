package model

import (
	"errors"
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
