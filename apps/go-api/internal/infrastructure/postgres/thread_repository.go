package postgres

import (
	"context"
	"strings"

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
	return toSummaries(rows), nil
}

// SearchSummaries はタイトルの中間一致で絞り込んで取得します
// (docs/adr/0012-search.md)。
//
// **ワイルドカードのエスケープはここで行います。** 検索語をそのまま
// LIKE に渡すと、`%` の 1 文字で全件一致を作られます (ADR 0012 の罠)。
// ドメイン側でやらないのは、`%` と `_` が特別なのは PostgreSQL の LIKE の
// 都合であって、検索語そのものの性質ではないためです。
func (r *ThreadRepository) SearchSummaries(
	ctx context.Context, query model.SearchQuery, page pagination.Page,
) ([]model.Summary, error) {
	rows, err := r.q.SearchThreadsWithCommentCount(ctx, sqlcgen.SearchThreadsWithCommentCountParams{
		TitleQuery: escapeLikePattern(query.Keyword()),
		CursorID:   page.CursorID(),
		PageSize:   page.Size,
	})
	if err != nil {
		return nil, translateError("ThreadRepository.SearchSummaries", err)
	}

	// **行の型は同一の SELECT 句から生成されるので変換できます。**
	// 片方の列を増やした時点でこの変換はコンパイルエラーになり、
	// 詰め替えの取りこぼしに気づけます。
	listRows := make([]sqlcgen.ListThreadsWithCommentCountRow, 0, len(rows))
	for _, row := range rows {
		listRows = append(listRows, sqlcgen.ListThreadsWithCommentCountRow(row))
	}
	return toSummaries(listRows), nil
}

// escapeLikePattern は LIKE のワイルドカードを打ち消します。
//
// エスケープ文字は `\` で、SQL 側の ESCAPE '\' と対になっています。
//
// **順に置換していく実装にしないこと。** `%` を先に `\%` へ変えてから
// `\` を `\\` へ変えると、**自分が挿入した `\` をもう一度エスケープ**して
// `\\%` (「バックスラッシュに続く任意の文字列」) に化けます。
// strings.NewReplacer は入力を 1 回だけ走査し、挿入した文字を読み直さないので、
// この形なら順序を気にする必要がありません
// (この誤りは変異プローブで実測しました。ADR 0012 の 6)。
func escapeLikePattern(keyword string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return r.Replace(keyword)
}

// toSummaries は取得した行を一覧の要素に詰め替えます。
func toSummaries(rows []sqlcgen.ListThreadsWithCommentCountRow) []model.Summary {
	summaries := make([]model.Summary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, model.Summary{
			Thread: *model.Reconstruct(row.ID, row.Title,
				toThreadAuthor(row.AuthorPublicID, row.AuthorDisplayName,
					row.AuthorAvatarUrl, row.AuthorDeletedAt),
				toThreadIcon(row.IconID, row.IconObjectKey, row.IconWidth, row.IconHeight),
				row.CreatedAt),
			CommentCount: row.CommentCount,
		})
	}
	return summaries
}

// FindSummaryByID は 1 件のスレッドをコメント数つきで取得します。
func (r *ThreadRepository) FindSummaryByID(ctx context.Context, id int64) (*model.Summary, error) {
	row, err := r.q.GetThreadWithCommentCount(ctx, id)
	if err != nil {
		return nil, translateError("ThreadRepository.FindSummaryByID", err)
	}

	return &model.Summary{
		Thread: *model.Reconstruct(row.ID, row.Title,
			toThreadAuthor(row.AuthorPublicID, row.AuthorDisplayName,
				row.AuthorAvatarUrl, row.AuthorDeletedAt),
			toThreadIcon(row.IconID, row.IconObjectKey, row.IconWidth, row.IconHeight),
			row.CreatedAt),
		CommentCount: row.CommentCount,
	}, nil
}

// Create はスレッドを保存し、採番済みの値を返します。
func (r *ThreadRepository) Create(ctx context.Context, thread *model.Thread) (*model.Thread, error) {
	row, err := r.q.CreateThread(ctx, sqlcgen.CreateThreadParams{
		Title:       thread.Title,
		AuthorID:    thread.AuthorID,
		IconImageID: thread.IconImageID,
	})
	if err != nil {
		return nil, translateError("ThreadRepository.Create", err)
	}
	created := model.Reconstruct(row.ID, row.Title,
		toThreadAuthor(row.AuthorPublicID, row.AuthorDisplayName,
			row.AuthorAvatarUrl, row.AuthorDeletedAt),
		toThreadIcon(row.IconID, row.IconObjectKey, row.IconWidth, row.IconHeight),
		row.CreatedAt)
	// 書き込み経路の戻り値は、書いた内容を反映させる。
	// Reconstruct は読み出し用で AuthorID を持たないため、ここで補う。
	// 補わないと「ログインして作ったのに AuthorID が nil」になり、
	// 戻り値で紐付けを判定するコードが将来書かれたときに静かに壊れる。
	created.AuthorID = thread.AuthorID
	return created, nil
}

// Exists はスレッドが存在するかを返します。
func (r *ThreadRepository) Exists(ctx context.Context, id int64) (bool, error) {
	ok, err := r.q.ThreadExists(ctx, id)
	if err != nil {
		return false, translateError("ThreadRepository.Exists", err)
	}
	return ok, nil
}

// SoftDeleteOwn は投稿者本人がスレッドを論理削除します。
//
// **成功したときは 1 往復で終わります。** 2 本目 (ThreadOwnership) を
// 引くのは 0 行だったときだけで、失敗の理由を撃ち分けるためです。
func (r *ThreadRepository) SoftDeleteOwn(ctx context.Context, id, actorID int64) error {
	affected, err := r.q.SoftDeleteOwnThread(ctx, sqlcgen.SoftDeleteOwnThreadParams{
		ID:      id,
		ActorID: &actorID,
	})
	if err != nil {
		return translateError("ThreadRepository.SoftDeleteOwn", err)
	}
	if affected > 0 {
		return nil
	}

	owned, err := r.q.ThreadOwnership(ctx, sqlcgen.ThreadOwnershipParams{
		ID:      id,
		ActorID: &actorID,
	})
	return classifyDeleteFailure("ThreadRepository.SoftDeleteOwn", owned, err)
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
		// ベンチマーク専用の経路。投稿者は引かないので nil のままにする。
		threads = append(threads, *model.Reconstruct(row.ID, row.Title, nil, nil, row.CreatedAt))
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
