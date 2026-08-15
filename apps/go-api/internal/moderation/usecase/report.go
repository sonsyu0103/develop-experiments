package usecase

import (
	"context"
	"fmt"
	"log/slog"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
	"develop-experiments/apps/go-api/internal/moderation/domain/repository"
	"develop-experiments/apps/go-api/internal/pagination"
)

// maxNoteLength は通報の補足の上限です。
//
// DB 側の CHECK 制約 (reports_note_length) と仕様書の maxLength と
// **同じ値**にしてください。3 か所が揃っていることは report_test.go が
// 実際にファイルを読んで検査します。
const maxNoteLength = 1000

// ReportCommand は通報 1 件ぶんの入力です。
type ReportCommand struct {
	// ReporterID は通報する人の内部 ID です。
	// **匿名では通報できません** (ADR 0011 決定 4)。
	ReporterID int64

	Target   model.ReportTargetType
	TargetID int64
	// ThreadID は **Target が comment のときだけ**必須です。
	ThreadID *int64

	Reason model.ReportReason
	Note   *string
}

// ReportInteractor は通報の受付とキューの操作を担当します。
//
// **ModerationInteractor と分けています。** あちらは
// 「削除して記録する」1 種類の操作だけを持ち、
// トランザクションの束ねがその理由でした。通報は
// 「積む」「読む」「閉じる」がそれぞれ独立しており、混ぜると
// 権限の異なる操作 (通報は一般利用者、キューはモデレーター) が
// 1 つの型に同居します。
type ReportInteractor struct {
	reports repository.ReportRepository
	targets repository.TargetExistenceChecker
}

// NewReportInteractor はインタラクタを生成します。
func NewReportInteractor(
	reports repository.ReportRepository, targets repository.TargetExistenceChecker,
) *ReportInteractor {
	return &ReportInteractor{reports: reports, targets: targets}
}

// Report は通報を 1 件積みます (docs/adr/0011-moderation.md 決定 4)。
//
// **重複はエラーになりません。** 同じ人が同じ対象を既に通報している場合は
// created = false で最初の通報を返します。呼び出し側は
// 「既に通報済みです」として正常に扱ってください ——
// 一意制約は「1 人で通報数を積み上げてキューの優先度を操作させない」
// ためのものであり、利用者に見せる失敗ではありません。
func (i *ReportInteractor) Report(
	ctx context.Context, cmd ReportCommand,
) (report *model.Report, created bool, err error) {
	if cmd.ReporterID == 0 {
		return nil, false, fmt.Errorf("通報にはログインが必要です: %w", apperr.ErrUnauthenticated)
	}
	if cmd.TargetID < 1 {
		return nil, false, fmt.Errorf("通報対象の ID が不正です: %w", apperr.ErrInvalidArgument)
	}

	note, err := normalizeNote(cmd.Note)
	if err != nil {
		return nil, false, err
	}

	// **対象が生きていることを確かめてから積む。**
	// 確かめないと、存在しない ID の通報でキューを埋められる。
	threadID, err := i.ensureTargetAlive(ctx, cmd)
	if err != nil {
		return nil, false, err
	}

	saved, created, err := i.reports.Create(ctx, &model.Report{
		ReporterID:     cmd.ReporterID,
		Target:         cmd.Target,
		TargetID:       cmd.TargetID,
		TargetThreadID: threadID,
		Reason:         cmd.Reason,
		Note:           note,
	})
	if err != nil {
		return nil, false, err
	}

	// **新規のときだけログに出す。** 重複通報まで出すと、
	// 連打しただけで件数が膨らむ (ADR 0010 の 4-5 と同じ考え方)。
	if created {
		slog.InfoContext(ctx, "report_created",
			slog.Int64("report_id", saved.ID),
			slog.String("target_type", string(saved.Target)),
			slog.Int64("target_id", saved.TargetID),
			slog.String("reason", string(saved.Reason)),
		)
	}
	return saved, created, nil
}

// ensureTargetAlive は通報対象が生きていることを確かめ、
// コメントの場合はスレッド ID を返します。
func (i *ReportInteractor) ensureTargetAlive(
	ctx context.Context, cmd ReportCommand,
) (*int64, error) {
	switch cmd.Target {
	case model.ReportTargetThread:
		// **スレッドの通報にスレッド ID は指定させない。**
		// 受け取ると targetId と threadId が食い違う組み合わせが表現でき、
		// DB の CHECK 制約 (reports_thread_id_matches_target) に当たって
		// 500 になる。手前で 400 にする。
		if cmd.ThreadID != nil {
			return nil, fmt.Errorf(
				"スレッドの通報に threadId は指定できません: %w", apperr.ErrInvalidArgument)
		}
		ok, err := i.targets.ThreadExists(ctx, cmd.TargetID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("通報対象が見つかりません: %w", apperr.ErrNotFound)
		}
		return nil, nil

	case model.ReportTargetComment:
		// **パーティションキーが要る。** 主キーが (thread_id, id) なので、
		// 無いと存在確認の時点で 8 パーティションを走査する。
		if cmd.ThreadID == nil || *cmd.ThreadID < 1 {
			return nil, fmt.Errorf(
				"コメントの通報にはスレッド ID が必要です: %w", apperr.ErrInvalidArgument)
		}
		ok, err := i.targets.CommentExists(ctx, *cmd.ThreadID, cmd.TargetID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("通報対象が見つかりません: %w", apperr.ErrNotFound)
		}
		return cmd.ThreadID, nil

	default:
		return nil, fmt.Errorf("不正な通報対象です: %w", apperr.ErrInvalidArgument)
	}
}

// ReportListResult は通報キューの 1 ページです。
type ReportListResult struct {
	Reports []model.Report
	// NextCursor は次ページの不透明トークンです。これ以上なければ nil。
	NextCursor *string
}

// ListQueue は通報キューを返します。
//
// **moderator 以上だけが読めます** (ADR 0011 決定 1)。
// 権限の判定は Actor が持ちます —— 削除と同じ形にしてあります。
func (i *ReportInteractor) ListQueue(
	ctx context.Context, actor model.Actor, status model.ReportStatus, page pagination.Page,
) (ReportListResult, error) {
	if !actor.CanModerate {
		return ReportListResult{},
			fmt.Errorf("通報キューを読む権限がありません: %w", apperr.ErrPermissionDenied)
	}

	// **1 件多く取って「次がある」を判定する。** 件数を数える問い合わせを
	// 足すと 1 往復増えるうえ、境界で数がずれる (他の一覧と同じ形)。
	page.Size++
	reports, err := i.reports.List(ctx, status, page)
	if err != nil {
		return ReportListResult{}, err
	}

	limit := int(page.Size) - 1
	var next *string
	if len(reports) > limit {
		reports = reports[:limit]
		token, encErr := pagination.NewCursor(reports[len(reports)-1].ID).Encode()
		if encErr != nil {
			return ReportListResult{}, encErr
		}
		next = &token
	}
	return ReportListResult{Reports: reports, NextCursor: next}, nil
}

// Resolve は通報を処理済みにします。
//
// **投稿には触れません。** 削除は ModerationInteractor.Delete が別に行います
// ——「通報を却下する」と「投稿を消す」は別の判断であり、
// まとめるとキューを片付ける操作がそのまま削除になります。
func (i *ReportInteractor) Resolve(
	ctx context.Context, actor model.Actor, id int64, status model.ReportStatus,
) (*model.Report, error) {
	if !actor.CanModerate {
		return nil, fmt.Errorf("通報を処理する権限がありません: %w", apperr.ErrPermissionDenied)
	}
	// **open は受け付けない。** 未処理へ戻す経路は作っていない。
	if !status.IsResolution() {
		return nil, fmt.Errorf("その状態には変更できません: %w", apperr.ErrInvalidArgument)
	}
	if id < 1 {
		return nil, fmt.Errorf("通報 ID が不正です: %w", apperr.ErrInvalidArgument)
	}

	resolved, err := i.reports.Resolve(ctx, id, status, actor.UserID)
	if err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "report_resolved",
		slog.Int64("report_id", resolved.ID),
		slog.Int64("actor_id", actor.UserID),
		slog.String("status", string(resolved.Status)),
	)
	return resolved, nil
}

// normalizeNote は補足を整えます。理由の正規化と同じ扱いです。
func normalizeNote(raw *string) (*string, error) {
	return normalizeText(raw, maxNoteLength, "補足")
}
