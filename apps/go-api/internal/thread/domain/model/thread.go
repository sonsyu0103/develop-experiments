// Package model は掲示板スレッドのドメインモデルを定義します。
package model

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"develop-experiments/apps/go-api/internal/apperr"
)

// TitleMaxLength はスレッドタイトルの最大文字数です。
// DB 側の CHECK 制約 (threads_title_length) と必ず同じ値にしてください。
// アプリ側でも検証するのは、DB 到達前に 400 を返して無駄な往復を避けるためです。
const TitleMaxLength = 200

// Thread は掲示板の「スレッド」を表すエンティティです。
type Thread struct {
	ID        int64
	Title     string
	CreatedAt time.Time
}

// Summary は一覧表示用に、スレッドとそのコメント数を組にした値です。
type Summary struct {
	Thread
	CommentCount int64
}

// NewThread は永続化前の新しいスレッドを組み立てます。
// ID と CreatedAt は DB が採番するため、ここでは設定しません。
func NewThread(title string) (*Thread, error) {
	title = strings.TrimSpace(title)

	if title == "" {
		return nil, fmt.Errorf("タイトルが空です: %w", apperr.ErrInvalidArgument)
	}
	// バイト数ではなく文字数で数える。DB 側の char_length() と揃えるため。
	if n := utf8.RuneCountInString(title); n > TitleMaxLength {
		return nil, fmt.Errorf(
			"タイトルが長すぎます (%d 文字, 上限 %d 文字): %w",
			n, TitleMaxLength, apperr.ErrInvalidArgument,
		)
	}

	return &Thread{Title: title}, nil
}

// Reconstruct は永続化層から読み出した値でスレッドを復元します。
// 保存済みのデータが対象なので、検証は行いません。
func Reconstruct(id int64, title string, createdAt time.Time) *Thread {
	return &Thread{
		ID:        id,
		Title:     title,
		CreatedAt: createdAt,
	}
}
