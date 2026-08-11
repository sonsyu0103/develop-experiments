package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/comment/domain/repository"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
	"develop-experiments/apps/go-api/internal/pagination"
)

// CommentRepository は repository.CommentRepository の PostgreSQL 実装です。
type CommentRepository struct {
	q *sqlcgen.Queries
}

var _ repository.CommentRepository = (*CommentRepository)(nil)

// NewCommentRepository は接続プールからリポジトリを生成します。
func NewCommentRepository(pool *pgxpool.Pool) *CommentRepository {
	return &CommentRepository{q: sqlcgen.New(pool)}
}

// ListByThreadID は 1 スレッドのコメントを新しい順に取得します。
func (r *CommentRepository) ListByThreadID(
	ctx context.Context, threadID int64, page pagination.Page,
) ([]model.Comment, error) {
	rows, err := r.q.ListCommentsByThreadID(ctx, sqlcgen.ListCommentsByThreadIDParams{
		ThreadID: threadID,
		CursorID: page.CursorID(),
		PageSize: page.Size,
	})
	if err != nil {
		return nil, translateError("CommentRepository.ListByThreadID", err)
	}

	comments := make([]model.Comment, 0, len(rows))
	for _, row := range rows {
		comments = append(comments, *model.Reconstruct(
			row.ID, row.ThreadID, row.AuthorName,
			toCommentAuthor(row.AuthorPublicID, row.AuthorDisplayName,
				row.AuthorAvatarUrl, row.AuthorDeletedAt),
			row.Body, row.CreatedAt,
		))
	}
	return comments, nil
}

// Create はコメントを保存し、採番済みの値を返します。
//
// 親スレッドの存在確認を事前の SELECT で行わないのは、
// 「確認したあとに削除される」競合 (TOCTOU) を避けるためです。
// 外部キー制約に違反させて、その違反を 404 に翻訳するほうが確実で、
// クエリも 1 往復で済みます。
func (r *CommentRepository) Create(ctx context.Context, comment *model.Comment) (*model.Comment, error) {
	row, err := r.q.CreateComment(ctx, sqlcgen.CreateCommentParams{
		ThreadID:   comment.ThreadID,
		AuthorName: comment.AuthorName,
		Body:       comment.Body,
		AuthorID:   comment.AuthorID,
	})
	if err != nil {
		return nil, translateError("CommentRepository.Create", err)
	}

	return model.Reconstruct(row.ID, row.ThreadID, row.AuthorName,
		toCommentAuthor(row.AuthorPublicID, row.AuthorDisplayName,
			row.AuthorAvatarUrl, row.AuthorDeletedAt),
		row.Body, row.CreatedAt), nil
}

// SoftDelete はコメントを論理削除します。
func (r *CommentRepository) SoftDelete(ctx context.Context, threadID, id int64) error {
	affected, err := r.q.SoftDeleteComment(ctx, sqlcgen.SoftDeleteCommentParams{
		ThreadID: threadID,
		ID:       id,
	})
	if err != nil {
		return translateError("CommentRepository.SoftDelete", err)
	}
	if affected == 0 {
		return fmt.Errorf("CommentRepository.SoftDelete: %w", apperr.ErrNotFound)
	}
	return nil
}
