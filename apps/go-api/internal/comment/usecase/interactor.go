// Package usecase はコメントに関するアプリケーションロジックを担います。
package usecase

import (
	"context"
	"fmt"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/comment/domain/repository"
	"develop-experiments/apps/go-api/internal/pagination"
)

// CommentDTO はフロントエンドに返すコメントのデータ構造です。
type CommentDTO struct {
	ID         int64     `json:"id"`
	ThreadID   int64     `json:"threadId"`
	AuthorName string    `json:"authorName"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
}

// CommentListResult はコメント一覧と、次ページ取得用のカーソルです。
// カーソルは不透明トークン (文字列) です。理由は ADR 0006 を参照してください。
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

	return CommentListResult{
		Comments:   dtos,
		NextCursor: pagination.NextToken(lastID, len(dtos), page.Size),
	}, nil
}

// PostComment はコメントを投稿します。
// 親スレッドが存在しない、または論理削除済みの場合は apperr.ErrNotFound を返します。
//
// ここで事前に存在確認をしないのは、確認と挿入の間にスレッドが
// 削除される競合を避けるためです。判定は INSERT ... WHERE EXISTS で
// SQL 側に寄せてあります。
func (i *CommentInteractor) PostComment(
	ctx context.Context, threadID int64, authorName, body string,
) (CommentDTO, error) {
	comment, err := model.NewComment(threadID, authorName, body)
	if err != nil {
		return CommentDTO{}, err
	}

	created, err := i.repo.Create(ctx, comment)
	if err != nil {
		return CommentDTO{}, err
	}

	return toDTO(*created), nil
}

func toDTO(c model.Comment) CommentDTO {
	return CommentDTO{
		ID:         c.ID,
		ThreadID:   c.ThreadID,
		AuthorName: c.AuthorName,
		Body:       c.Body,
		CreatedAt:  c.CreatedAt,
	}
}
