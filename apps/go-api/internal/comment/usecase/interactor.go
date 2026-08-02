// Package usecase はコメントに関するアプリケーションロジックを担います。
package usecase

import (
	"context"
	"time"

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
type CommentListResult struct {
	Comments   []CommentDTO `json:"comments"`
	NextCursor *int64       `json:"nextCursor"`
}

// CommentInteractor は「コメントを取得・投稿する」ユースケースを担当します。
type CommentInteractor struct {
	repo repository.CommentRepository
}

// NewCommentInteractor はリポジトリを注入してインタラクターを生成します。
func NewCommentInteractor(repo repository.CommentRepository) *CommentInteractor {
	return &CommentInteractor{repo: repo}
}

// FetchComments は 1 スレッドのコメントを新しい順に取得します。
func (i *CommentInteractor) FetchComments(
	ctx context.Context, threadID int64, page pagination.Page,
) (CommentListResult, error) {
	comments, err := i.repo.ListByThreadID(ctx, threadID, page)
	if err != nil {
		return CommentListResult{}, err
	}

	dtos := make([]CommentDTO, 0, len(comments))
	for _, c := range comments {
		dtos = append(dtos, toDTO(c))
	}

	var next *int64
	if len(dtos) > 0 && len(dtos) == int(page.Size) {
		last := dtos[len(dtos)-1].ID
		next = &last
	}

	return CommentListResult{Comments: dtos, NextCursor: next}, nil
}

// PostComment はコメントを投稿します。
// 親スレッドが存在しない場合は apperr.ErrNotFound を返します
// (外部キー違反をリポジトリ層が翻訳します)。
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
