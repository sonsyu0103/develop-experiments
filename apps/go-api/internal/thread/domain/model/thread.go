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
	ID    int64
	Title string
	// AuthorID は投稿者の内部 ID です。**nil が匿名を意味します**
	// (docs/adr/0005-authentication.md 決定 2)。
	// 書き込み時に使う値で、API には出しません。
	//
	// **読み出し経路では埋まりません。** 一覧・詳細のクエリは author_id を
	// 選ばず、代わりに JOIN 済みの Author を返すためです。
	// 「匿名かどうか」の判定に読み出し側でこれを使わないでください
	// (常に nil に見えます)。判定は Author が nil かどうかで行います。
	AuthorID *int64
	// Author は表示用の投稿者情報です。読み出し時に解決されます。
	// 匿名投稿では nil になります。
	Author    *Author
	CreatedAt time.Time
}

// Summary は一覧表示用に、スレッドとそのコメント数を組にした値です。
type Summary struct {
	Thread
	CommentCount int64
}

// NewThread は永続化前の新しいスレッドを組み立てます。
// ID と CreatedAt は DB が採番するため、ここでは設定しません。
//
// authorID が nil なら匿名投稿になります。匿名投稿を残すのは決定事項です
// (docs/adr/0005-authentication.md 決定 2)。
func NewThread(title string, authorID *int64) (*Thread, error) {
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

	return &Thread{Title: title, AuthorID: authorID}, nil
}

// Reconstruct は永続化層から読み出した値でスレッドを復元します。
// 保存済みのデータが対象なので、検証は行いません。
func Reconstruct(id int64, title string, author *Author, createdAt time.Time) *Thread {
	return &Thread{
		ID:        id,
		Title:     title,
		Author:    author,
		CreatedAt: createdAt,
	}
}
