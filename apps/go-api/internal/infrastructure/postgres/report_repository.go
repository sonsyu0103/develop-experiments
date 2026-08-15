package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
	"develop-experiments/apps/go-api/internal/moderation/domain/repository"
	"develop-experiments/apps/go-api/internal/pagination"
)

// ReportRepository は repository.ReportRepository と
// repository.TargetExistenceChecker の PostgreSQL 実装です。
//
// **ModerationRepository と分けています。** あちらは削除と記録を
// 1 トランザクションに収めるための束ねでしたが、通報は
// 「積む」「読む」「閉じる」がそれぞれ 1 文で完結し、
// 束ねる理由がありません。
type ReportRepository struct {
	q *sqlcgen.Queries
}

var (
	_ repository.ReportRepository       = (*ReportRepository)(nil)
	_ repository.TargetExistenceChecker = (*ReportRepository)(nil)
)

// NewReportRepository は接続プールからリポジトリを生成します。
func NewReportRepository(pool *pgxpool.Pool) *ReportRepository {
	return &ReportRepository{q: sqlcgen.New(pool)}
}

// Create は通報を 1 件積みます。
//
// **重複はエラーにしません** (ADR 0011 の引き受けるコスト)。
// ON CONFLICT DO NOTHING が 0 行を返したときだけ、既存の通報を読み直します。
func (r *ReportRepository) Create(
	ctx context.Context, rep *model.Report,
) (*model.Report, bool, error) {
	row, err := r.q.CreateReport(ctx, sqlcgen.CreateReportParams{
		ReporterID:     rep.ReporterID,
		TargetType:     string(rep.Target),
		TargetID:       rep.TargetID,
		TargetThreadID: rep.TargetThreadID,
		Reason:         string(rep.Reason),
		Note:           rep.Note,
	})
	if err == nil {
		return toReport(reportRow(row)), true, nil
	}
	// **:one なので、0 行は pgx.ErrNoRows として返る。**
	// それが「既に通報済み」の合図になる。
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, translateError("ReportRepository.Create", err)
	}

	existing, err := r.q.FindReportByTarget(ctx, sqlcgen.FindReportByTargetParams{
		ReporterID: rep.ReporterID,
		TargetType: string(rep.Target),
		TargetID:   rep.TargetID,
	})
	if err != nil {
		return nil, false, translateError("ReportRepository.Create", err)
	}
	return toReport(reportRow(existing)), false, nil
}

// List は通報キューを古い順に返します。
func (r *ReportRepository) List(
	ctx context.Context, status model.ReportStatus, page pagination.Page,
) ([]model.Report, error) {
	rows, err := r.q.ListReports(ctx, sqlcgen.ListReportsParams{
		Status:   string(status),
		CursorID: page.CursorID(),
		PageSize: page.Size,
	})
	if err != nil {
		return nil, translateError("ReportRepository.List", err)
	}

	reports := make([]model.Report, 0, len(rows))
	for _, row := range rows {
		reports = append(reports, *toReport(reportRow(row)))
	}
	return reports, nil
}

// Resolve は通報を処理済みにします。
func (r *ReportRepository) Resolve(
	ctx context.Context, id int64, status model.ReportStatus, actorID int64,
) (*model.Report, error) {
	row, err := r.q.ResolveReport(ctx, sqlcgen.ResolveReportParams{
		ID:         id,
		Status:     string(status),
		ResolvedBy: &actorID,
	})
	if err != nil {
		// 0 行は「無い、または既に処理済み」。translateError が 404 にする。
		return nil, translateError("ReportRepository.Resolve", err)
	}
	return toReport(reportRow(row)), nil
}

// ThreadExists はスレッドが生きているかを返します。
func (r *ReportRepository) ThreadExists(ctx context.Context, id int64) (bool, error) {
	ok, err := r.q.ThreadExists(ctx, id)
	if err != nil {
		return false, translateError("ReportRepository.ThreadExists", err)
	}
	return ok, nil
}

// CommentExists はコメントが生きているかを返します。
//
// **threadID はパーティションキーです。** 無いと 8 パーティションを走査します。
func (r *ReportRepository) CommentExists(ctx context.Context, threadID, id int64) (bool, error) {
	ok, err := r.q.CommentExists(ctx, sqlcgen.CommentExistsParams{ThreadID: threadID, ID: id})
	if err != nil {
		return false, translateError("ReportRepository.CommentExists", err)
	}
	return ok, nil
}

// reportRow は sqlc が生成した 4 つの行型を 1 つに受けるための中間表現です。
//
// **sqlc はクエリごとに別の行型を作ります。** SELECT の並びが同じでも
// CreateReportRow / FindReportByTargetRow / ListReportsRow / ResolveReportRow は
// 別の型になります。詰め替えを 4 回書くと、1 か所直し忘れたときに
// 「一覧だけ note が落ちる」ような形で表に出ます。
//
// **フィールドの名前・型・並びが完全に一致していれば、Go は型変換を許します。**
// つまり reports.sql の SELECT / RETURNING の列がずれた瞬間に、
// この変換がコンパイルエラーになります —— 揃っていることを
// コンパイラに見張らせる形にしてあります。
type reportRow struct {
	ID             int64
	ReporterID     int64
	TargetType     string
	TargetID       int64
	TargetThreadID *int64
	Reason         string
	Note           *string
	Status         string
	CreatedAt      time.Time
	ResolvedAt     *time.Time
	ResolvedBy     *int64
}

// toReport は行をドメインの型へ詰め替えます。
//
// **status などは Parse を通しません。** DB の CHECK 制約が値を保証しており、
// ここで弾くと「保存されているのに読めない行」ができます
// (読み出しは書き込みより後なので、直す手段がありません)。
// 知らない値が入るとしたら制約を外したときで、それは移行の問題になります。
func toReport(r reportRow) *model.Report {
	return &model.Report{
		ID:             r.ID,
		ReporterID:     r.ReporterID,
		Target:         model.ReportTargetType(r.TargetType),
		TargetID:       r.TargetID,
		TargetThreadID: r.TargetThreadID,
		Reason:         model.ReportReason(r.Reason),
		Note:           r.Note,
		Status:         model.ReportStatus(r.Status),
		CreatedAt:      r.CreatedAt,
		ResolvedAt:     r.ResolvedAt,
		ResolvedBy:     r.ResolvedBy,
	}
}
