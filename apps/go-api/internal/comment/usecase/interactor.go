// Package usecase はコメントに関するアプリケーションロジックを担います。
package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/comment/domain/repository"
	"develop-experiments/apps/go-api/internal/idempotency"
	"develop-experiments/apps/go-api/internal/pagination"
)

// AuthorDTO はフロントエンドに返す投稿者のデータ構造です。
// 内部 ID (users.id) は含めません。
type AuthorDTO struct {
	// PublicID は退会済みでは nil になります。
	PublicID    *uuid.UUID `json:"publicId"`
	DisplayName string     `json:"displayName"`
	AvatarURL   *string    `json:"avatarUrl"`
	Withdrawn   bool       `json:"withdrawn"`
}

// CommentDTO はフロントエンドに返すコメントのデータ構造です。
type CommentDTO struct {
	ID       int64 `json:"id"`
	ThreadID int64 `json:"threadId"`
	// Seq はスレッド内のレス番号 (`>>1` の 1) です。
	// 削除しても詰めないため、欠番が出ます
	// (docs/adr/0019-comment-concurrency.md 決定 5)。
	Seq int32 `json:"seq"`
	// AuthorName は匿名投稿の表示名です。
	// Author があるときは、そちらを表示してください。
	AuthorName string `json:"authorName"`
	// Author は匿名投稿では nil になります。
	Author    *AuthorDTO `json:"author"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"createdAt"`
}

// CommentListResult はコメント一覧と、次ページ取得用のカーソルです。
// カーソルは不透明トークン (文字列) です。形式は ADR 0018 を参照してください。
type CommentListResult struct {
	Comments   []CommentDTO `json:"comments"`
	NextCursor *string      `json:"nextCursor"`
}

// CommentInteractor は「コメントを取得・投稿する」ユースケースを担当します。
type CommentInteractor struct {
	repo    repository.CommentRepository
	threads repository.ThreadExistenceChecker
}

// NewCommentInteractor はリポジトリを注入してインタラクターを生成します。
func NewCommentInteractor(
	repo repository.CommentRepository,
	threads repository.ThreadExistenceChecker,
) *CommentInteractor {
	return &CommentInteractor{repo: repo, threads: threads}
}

// FetchComments は 1 スレッドのコメントを新しい順に取得します。
// スレッドが存在しない、または論理削除済みの場合は apperr.ErrNotFound を返します。
//
// 存在確認を挟まないと、存在しないスレッドに対して
// 「コメント 0 件」という誤った 200 を返してしまいます。
// また論理削除済みスレッドの中身が読めてしまい、
// 「削除したはずのものが API から見える」状態になります。
//
// 存在確認と取得の間にスレッドが削除される競合は残りますが、
// その場合に起きるのは「削除直後のコメントが 1 回だけ返る」ことだけで、
// 不正な書き込みは生じません。投稿側 (PostComment) は書き込みを伴うため
// SQL 側で 1 文に閉じており、こちらとは要求される厳密さが異なります。
// 1 クエリ化できない理由は db/query/comments.sql のコメントを参照してください。
func (i *CommentInteractor) FetchComments(
	ctx context.Context, threadID int64, page pagination.Page,
) (CommentListResult, error) {
	ok, err := i.threads.Exists(ctx, threadID)
	if err != nil {
		return CommentListResult{}, err
	}
	if !ok {
		return CommentListResult{}, fmt.Errorf("スレッド %d: %w", threadID, apperr.ErrNotFound)
	}

	comments, err := i.repo.ListByThreadID(ctx, threadID, page)
	if err != nil {
		return CommentListResult{}, err
	}

	dtos := make([]CommentDTO, 0, len(comments))
	for _, c := range comments {
		dtos = append(dtos, toDTO(c))
	}

	var lastID int64
	if len(dtos) > 0 {
		lastID = dtos[len(dtos)-1].ID
	}

	next, err := pagination.NextToken(lastID, len(dtos), page.Size)
	if err != nil {
		return CommentListResult{}, err
	}

	return CommentListResult{Comments: dtos, NextCursor: next}, nil
}

// PostComment はコメントを投稿します。
// 親スレッドが存在しない、または論理削除済みの場合は apperr.ErrNotFound を返します。
//
// ここで事前に存在確認をしないのは、確認と挿入の間にスレッドが
// 削除される競合を避けるためです。判定は INSERT ... WHERE EXISTS で
// SQL 側に寄せてあります。
func (i *CommentInteractor) PostComment(
	ctx context.Context, threadID int64, authorName, body string, authorID *int64,
) (CommentDTO, error) {
	comment, err := model.NewComment(threadID, authorName, body, authorID)
	if err != nil {
		return CommentDTO{}, err
	}

	created, err := i.repo.Create(ctx, comment)
	if err != nil {
		return CommentDTO{}, err
	}

	return toDTO(*created), nil
}

// PostCommentIdempotent は冪等キーつきで投稿します。
//
// 同じキーで再送された場合は**投稿せずに**前回の応答を返します
// (docs/adr/0015-idempotency.md)。
//
// **匿名では冪等キーを使えません。** キーの名前空間を分ける手段が無く、
// IP で分けると NAT の背後で他人のキーと衝突するためです (同 決定 4)。
// 呼び出し側 (HTTP 層) が匿名のヘッダを落とす前提ですが、
// 他人の結果を返す事故に直結するのでここでも弾きます。
func (i *CommentInteractor) PostCommentIdempotent(
	ctx context.Context, threadID int64, authorName, body string, authorID *int64,
	key, endpoint string,
) (CommentDTO, error) {
	if authorID == nil {
		return CommentDTO{}, fmt.Errorf(
			"匿名では Idempotency-Key を使えません: %w", apperr.ErrInvalidArgument)
	}

	comment, err := model.NewComment(threadID, authorName, body, authorID)
	if err != nil {
		return CommentDTO{}, err
	}

	// **指紋は正規化したあとの値から作ります。**
	//
	// 受け取ったままの値を使うと、結果に影響しない差で 422 になります。
	//   - authorName はログイン中には捨てられる (NewComment)。
	//     入っていても投稿結果は変わらないのに、指紋だけが変わる
	//   - body は TrimSpace される。末尾の空白が落ちただけで別物になる
	//
	// **422 は再試行では絶対に解けません** (キーを作り直すしかない)。
	// 「1 回目は名前欄あり、タイムアウト後の再送では名前欄が空」という、
	// 利用者から見て同じ操作が弾かれることになります。
	//
	// 渡すのは投稿結果を決める値だけ。スレッドは endpoint に含まれています。
	req, err := idempotency.New(key, endpoint, comment.Body)
	if err != nil {
		return CommentDTO{}, err
	}

	// **記録するのは応答そのもの** (ADR 0015)。
	// 永続化層に DTO の形を知らせないため、詰め替えと符号化はここで行い、
	// バイト列だけを渡します。
	encode := func(c *model.Comment) ([]byte, error) {
		return json.Marshal(toDTO(*c))
	}

	created, replayed, err := i.repo.CreateIdempotent(ctx, comment, *req, encode)
	if err != nil {
		return CommentDTO{}, err
	}

	if replayed != nil {
		var dto CommentDTO
		if err := json.Unmarshal(replayed, &dto); err != nil {
			// 記録した本文が読めない = こちらが書式を変えた可能性が高い。
			// **前回と違う応答を返すくらいなら失敗させる。**
			return CommentDTO{}, fmt.Errorf(
				"記録済みの応答を復元できませんでした: %w", err)
		}
		return dto, nil
	}

	return toDTO(*created), nil
}

func toDTO(c model.Comment) CommentDTO {
	return CommentDTO{
		ID:         c.ID,
		ThreadID:   c.ThreadID,
		Seq:        c.Seq,
		AuthorName: c.AuthorName,
		Author:     toAuthorDTO(c.Author),
		Body:       c.Body,
		CreatedAt:  c.CreatedAt,
	}
}

// toAuthorDTO は投稿者を詰め替えます。匿名投稿では nil のまま返します。
//
// 退会済みの表示の差し替えはドメイン側 (model.NewAuthor) で済んでいます。
func toAuthorDTO(a *model.Author) *AuthorDTO {
	if a == nil {
		return nil
	}
	return &AuthorDTO{
		PublicID:    a.PublicID,
		DisplayName: a.DisplayName,
		AvatarURL:   a.AvatarURL,
		Withdrawn:   a.Withdrawn,
	}
}
