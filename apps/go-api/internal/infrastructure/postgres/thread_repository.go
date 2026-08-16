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

// ListPopularSummaries は閲覧数の多い順に取得します
// (docs/adr/0006-view-count-and-popularity.md)。
//
// **カーソルは (view_count, id) の複合キーです。** 片方だけを渡す形は
// 作れません —— SQL 側は行値比較 `(view_count, id) < ($1, $2)` を使うため、
// どちらか一方が NULL だと比較全体が NULL になり、**1 件も返らなくなります。**
//
// 並び順の一致 (新着順のトークンが混ざっていないか) は
// ユースケース層が検査済みなので、ここでは page をそのまま渡します。
func (r *ThreadRepository) ListPopularSummaries(
	ctx context.Context, page pagination.Page,
) ([]model.Summary, error) {
	rows, err := r.q.ListPopularThreadsWithCommentCount(
		ctx, sqlcgen.ListPopularThreadsWithCommentCountParams{
			CursorViewCount: page.CursorViewCount(),
			CursorID:        page.CursorID(),
			PageSize:        page.Size,
		})
	if err != nil {
		return nil, translateError("ThreadRepository.ListPopularSummaries", err)
	}

	// 行の型は同一の SELECT 句から生成されるので変換できます
	// (SearchSummaries と同じ形)。
	listRows := make([]sqlcgen.ListThreadsWithCommentCountRow, 0, len(rows))
	for _, row := range rows {
		listRows = append(listRows, sqlcgen.ListThreadsWithCommentCountRow(row))
	}
	return toSummaries(listRows), nil
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
				row.CreatedAt, row.ViewCount),
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
			row.CreatedAt, row.ViewCount),
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
		row.CreatedAt, row.ViewCount)
	// 書き込み経路の戻り値は、書いた内容を反映させる。
	// Reconstruct は読み出し用で AuthorID を持たないため、ここで補う。
	// 補わないと「ログインして作ったのに AuthorID が nil」になり、
	// 戻り値で紐付けを判定するコードが将来書かれたときに静かに壊れる。
	created.AuthorID = thread.AuthorID
	return created, nil
}

// IncrementViewCounts は閲覧数の増分をまとめて反映します
// (docs/adr/0006-view-count-and-popularity.md)。
//
// **呼び出し側 (viewcount.Buffer) がフラッシュ契機を握っています。**
// ここは「渡された増分を 1 文で足す」だけを担い、いつ呼ぶかは知りません。
//
// 【トランザクションを明示的に張っていない理由】
// pgx は単文をそのまま実行し、PostgreSQL 側が暗黙のトランザクションで包みます。
// **プールから取った接続の既定の分離レベルは READ COMMITTED** なので、
// ADR 0006 が要求する「別トランザクション・READ COMMITTED」を満たします。
//
// **ここで SERIALIZABLE を張ってはいけません。** コメント投稿と
// 同じ土俵に上がることになり、閲覧数を分離した意味が消えます。
//
// 【引数の長さが違ったら】
// SQL 側は短いほうの配列に合わせて NULL を掛け、加算が NULL になります
// (NOT NULL 制約で落ちます)。呼び出し側が同じ長さで渡す契約なので、
// ここでは検査しません —— 検査を足すより、
// take() が 2 本を同時に組み立てる形を崩さないほうが確実です。
func (r *ThreadRepository) IncrementViewCounts(
	ctx context.Context, threadIDs, increments []int64,
) (int64, error) {
	affected, err := r.q.IncrementThreadViewCounts(ctx, sqlcgen.IncrementThreadViewCountsParams{
		ThreadIds:  threadIDs,
		Increments: increments,
	})
	if err != nil {
		return 0, translateError("ThreadRepository.IncrementViewCounts", err)
	}
	return affected, nil
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
		// 閲覧数も引かない —— N+1 の比較に要らず、
		// **列を増やすほど「単一クエリ側だけが重い」比較になる**ため。
		threads = append(threads, *model.Reconstruct(row.ID, row.Title, nil, nil, row.CreatedAt, 0))
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
