// Package model はコメントのドメインモデルを定義します。
package model

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"develop-experiments/apps/go-api/internal/apperr"
)

// DB 側の CHECK 制約 (comments_body_length / comments_author_length) と
// 必ず同じ値にしてください。
const (
	BodyMaxLength       = 2000
	AuthorNameMaxLength = 50
	// DefaultAuthorName は投稿者名が省略されたときの既定値です。
	// DB 側の DEFAULT と同じ値です。
	DefaultAuthorName = "名無しさん"
)

// Comment はスレッドに紐づく「コメント」を表すエンティティです。
type Comment struct {
	ID         int64
	ThreadID   int64
	AuthorName string
	Body       string
	CreatedAt  time.Time
}

// NewComment は永続化前の新しいコメントを組み立てます。
// authorName が空の場合は DefaultAuthorName を使います。
func NewComment(threadID int64, authorName, body string) (*Comment, error) {
	if threadID <= 0 {
		return nil, fmt.Errorf("スレッド ID が不正です (%d): %w", threadID, apperr.ErrInvalidArgument)
	}

	authorName = strings.TrimSpace(authorName)
	if authorName == "" {
		authorName = DefaultAuthorName
	}
	if n := utf8.RuneCountInString(authorName); n > AuthorNameMaxLength {
		return nil, fmt.Errorf(
			"投稿者名が長すぎます (%d 文字, 上限 %d 文字): %w",
			n, AuthorNameMaxLength, apperr.ErrInvalidArgument,
		)
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("本文が空です: %w", apperr.ErrInvalidArgument)
	}
	if n := utf8.RuneCountInString(body); n > BodyMaxLength {
		return nil, fmt.Errorf(
			"本文が長すぎます (%d 文字, 上限 %d 文字): %w",
			n, BodyMaxLength, apperr.ErrInvalidArgument,
		)
	}

	return &Comment{
		ThreadID:   threadID,
		AuthorName: authorName,
		Body:       body,
	}, nil
}

// Reconstruct は永続化層から読み出した値でコメントを復元します。
func Reconstruct(id, threadID int64, authorName, body string, createdAt time.Time) *Comment {
	return &Comment{
		ID:         id,
		ThreadID:   threadID,
		AuthorName: authorName,
		Body:       body,
		CreatedAt:  createdAt,
	}
}
