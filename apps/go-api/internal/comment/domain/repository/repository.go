// Package repository はコメントの永続化に対するインターフェースを定義します。
package repository

import (
	"context"

	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/pagination"
)

// CommentRepository はコメントの読み書きを担います。
type CommentRepository interface {
	// ListByThreadID は 1 スレッドのコメントを新しい順に取得します。
	// thread_id を等値で指定するため、HASH パーティションの pruning が効きます。
	ListByThreadID(ctx context.Context, threadID int64, page pagination.Page) ([]model.Comment, error)

	// Create はコメントを保存し、採番済みの値を返します。
	// 親スレッドが存在しない場合は apperr.ErrNotFound を返します。
	Create(ctx context.Context, comment *model.Comment) (*model.Comment, error)

	// SoftDelete はコメントを論理削除します。
	// 対象が存在しない (すでに削除済みを含む) 場合は apperr.ErrNotFound を返します。
	SoftDelete(ctx context.Context, threadID, id int64) error
}
