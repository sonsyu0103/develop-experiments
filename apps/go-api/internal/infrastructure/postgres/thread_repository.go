package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
	"develop-experiments/apps/go-api/internal/pagination"
	"develop-experiments/apps/go-api/internal/thread/domain/model"
	"develop-experiments/apps/go-api/internal/thread/domain/repository"
)

// ThreadRepository は repository.ThreadRepository の PostgreSQL 実装です。
type ThreadRepository struct {
	q *sqlcgen.Queries
}

// コンパイル時にインターフェースの充足を確認する。
var (
	_ repository.ThreadRepository    = (*ThreadRepository)(nil)
	_ repository.BenchmarkRepository = (*ThreadRepository)(nil)
)

// NewThreadRepository は接続プールからリポジトリを生成します。
func NewThreadRepository(pool *pgxpool.Pool) *ThreadRepository {
	return &ThreadRepository{q: sqlcgen.New(pool)}
}

// ListSummaries はスレッドとコメント数の組を新しい順に取得します。
func (r *ThreadRepository) ListSummaries(ctx context.Context, page pagination.Page) ([]model.Summary, error) {
	rows, err := r.q.ListThreadsWithCommentCount(ctx, sqlcgen.ListThreadsWithCommentCountParams{
		CursorID: page.CursorID(),
		PageSize: page.Size,
	})
	if err != nil {
		return nil, translateError("ThreadRepository.ListSummaries", err)
	}

	summaries := make([]model.Summary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, model.Summary{
			Thread:       *model.Reconstruct(row.ID, row.Title, row.CreatedAt),
			CommentCount: row.CommentCount,
		})
	}
	return summaries, nil
}

// FindSummaryByID は 1 件のスレッドをコメント数つきで取得します。
func (r *ThreadRepository) FindSummaryByID(ctx context.Context, id int64) (*model.Summary, error) {
	row, err := r.q.GetThreadWithCommentCount(ctx, id)
	if err != nil {
		return nil, translateError("ThreadRepository.FindSummaryByID", err)
	}

	return &model.Summary{
		Thread:       *model.Reconstruct(row.ID, row.Title, row.CreatedAt),
		CommentCount: row.CommentCount,
	}, nil
}

// Create はスレッドを保存し、採番済みの値を返します。
func (r *ThreadRepository) Create(ctx context.Context, thread *model.Thread) (*model.Thread, error) {
	row, err := r.q.CreateThread(ctx, thread.Title)
	if err != nil {
		return nil, translateError("ThreadRepository.Create", err)
	}
	return model.Reconstruct(row.ID, row.Title, row.CreatedAt), nil
}

// Exists はスレッドが存在するかを返します。
func (r *ThreadRepository) Exists(ctx context.Context, id int64) (bool, error) {
	ok, err := r.q.ThreadExists(ctx, id)
	if err != nil {
		return false, translateError("ThreadRepository.Exists", err)
	}
	return ok, nil
}

// ListThreadsOnly はコメント数を含めずにスレッドだけを取得します (ベンチマーク用)。
func (r *ThreadRepository) ListThreadsOnly(ctx context.Context, page pagination.Page) ([]model.Thread, error) {
	rows, err := r.q.ListThreadIDs(ctx, sqlcgen.ListThreadIDsParams{
		CursorID: page.CursorID(),
		PageSize: page.Size,
	})
	if err != nil {
		return nil, translateError("ThreadRepository.ListThreadsOnly", err)
	}

	threads := make([]model.Thread, 0, len(rows))
	for _, row := range rows {
		threads = append(threads, *model.Reconstruct(row.ID, row.Title, row.CreatedAt))
	}
	return threads, nil
}

// CountComments は 1 スレッド分のコメント数を数えます (ベンチマーク用)。
func (r *ThreadRepository) CountComments(ctx context.Context, threadID int64) (int64, error) {
	n, err := r.q.CountCommentsByThreadID(ctx, threadID)
	if err != nil {
		return 0, translateError("ThreadRepository.CountComments", err)
	}
	return n, nil
}
