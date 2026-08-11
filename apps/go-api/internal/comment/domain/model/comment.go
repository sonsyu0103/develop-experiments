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
	ID       int64
	ThreadID int64
	// AuthorName は匿名投稿の表示名です。
	// ログイン中の投稿では既定値のまま保存され、表示には使いません
	// (NewComment のコメントを参照)。
	AuthorName string
	// AuthorID は投稿者の内部 ID です。**nil が匿名を意味します**。
	// 書き込み時に使う値で、API には出しません。
	//
	// **読み出し経路では埋まりません** (thread 側と同じ理由)。
	// 匿名かどうかの判定は Author が nil かどうかで行います。
	AuthorID *int64
	// Author は表示用の投稿者情報です。匿名投稿では nil になります。
	Author    *Author
	Body      string
	CreatedAt time.Time
}

// NewComment は永続化前の新しいコメントを組み立てます。
// authorName が空の場合は DefaultAuthorName を使います。
//
// authorID が nil なら匿名投稿になります
// (docs/adr/0005-authentication.md 決定 2)。
//
// **ログイン中は authorName を捨てます。** 表示に使うのは users の表示名で、
// 保存された author_name ではないためです。ここで受け取った値を保存すると、
// 「投稿時点の表示名」が残り、表示名の変更が過去の投稿に反映されなくなります
// (docs/adr/0014-author-resolution.md はその挙動を選んでいません)。
func NewComment(threadID int64, authorName, body string, authorID *int64) (*Comment, error) {
	if threadID <= 0 {
		return nil, fmt.Errorf("スレッド ID が不正です (%d): %w", threadID, apperr.ErrInvalidArgument)
	}

	if authorID != nil {
		authorName = ""
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
		AuthorID:   authorID,
		Body:       body,
	}, nil
}

// Reconstruct は永続化層から読み出した値でコメントを復元します。
func Reconstruct(
	id, threadID int64, authorName string, author *Author, body string, createdAt time.Time,
) *Comment {
	return &Comment{
		ID:         id,
		ThreadID:   threadID,
		AuthorName: authorName,
		Author:     author,
		Body:       body,
		CreatedAt:  createdAt,
	}
}
