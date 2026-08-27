// Package model はコメントのドメインモデルを定義します。
package model

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

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
	// Seq はスレッド内のレス番号 (`>>1` の 1) です。**1 から始まります。**
	//
	// 採番は永続化層が行います。NewComment を通した時点ではまだ 0 です。
	// スレッドごとに独立しており、削除しても詰めません
	// (docs/adr/0019-comment-concurrency.md 決定 5)。
	//
	// **この採番がこのリポジトリの主題 (並行制御) の題材です。**
	// 「読んでから書く」処理なので、同時投稿で必ず競合します。
	Seq int32
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
	Author *Author
	// ImageID は添付画像の ID です。**nil が「画像なし」を意味します**。
	// 書き込み時に使う値で、読み出し経路では埋まりません
	// (AuthorID と同じ扱い。判定は Image が nil かどうかで行います)。
	ImageID *uuid.UUID
	// Image は表示用の添付画像です。画像がなければ nil になります。
	//
	// **モジュールをまたがないため、image モジュールの型は使いません**
	// (docs/adr/0004-modular-monolith.md / ADR 0014 の Author と同じ形)。
	Image     *Image
	Body      string
	CreatedAt time.Time
}

// Image は表示用の添付画像です。
//
// **URL ではなくオブジェクトキーを持ちます。** URL の組み立ては
// 環境ごとの設定 (CDN のドメイン) を要するため、ドメインの外
// (ユースケース層) で行います —— ここに URL を持たせると、
// ドメインが配信基盤を知ることになります。
type Image struct {
	ID        uuid.UUID
	ObjectKey string
	Width     int
	Height    int
}

// NewImage は永続化層が読み出した行から添付画像を組み立てます。
//
// **id がゼロ値なら nil を返します。** LEFT JOIN が成立しなかった
// (画像が添付されていない) 場合がこれに当たります。
func NewImage(id uuid.UUID, objectKey string, width, height int) *Image {
	if id == uuid.Nil {
		return nil
	}
	return &Image{ID: id, ObjectKey: objectKey, Width: width, Height: height}
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
// **画像は認証済みでなければ添付できません** (ADR 0007 の背景)。
// 匿名の投稿に imageId が付いていたら、無視ではなくエラーにします ——
// 冪等キー (無視する) と違い、こちらは「画像が消えた」ように見えるためです。
func NewComment(
	threadID int64, authorName, body string, authorID *int64, imageID *uuid.UUID,
) (*Comment, error) {
	if threadID <= 0 {
		return nil, fmt.Errorf("スレッド ID が不正です (%d): %w", threadID, apperr.ErrInvalidArgument)
	}

	if imageID != nil && authorID == nil {
		return nil, fmt.Errorf(
			"画像を添付するにはログインが必要です: %w", apperr.ErrUnauthenticated)
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
		ImageID:    imageID,
		Body:       body,
	}, nil
}

// Reconstruct は永続化層から読み出した値でコメントを復元します。
func Reconstruct(
	id, threadID int64, seq int32, authorName string,
	author *Author, image *Image, body string, createdAt time.Time,
) *Comment {
	return &Comment{
		ID:         id,
		ThreadID:   threadID,
		Seq:        seq,
		AuthorName: authorName,
		Author:     author,
		Image:      image,
		Body:       body,
		CreatedAt:  createdAt,
	}
}
