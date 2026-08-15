package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
	"develop-experiments/apps/go-api/internal/moderation/domain/repository"
)

// ModerationRepository は repository.Repository の PostgreSQL 実装です。
//
// **削除の口 (threads / comments / images) もここが実装します。**
// モジュールごとにリポジトリを分けると、削除と記録が別の
// トランザクションになってしまうためです (ADR 0011 決定 3 が
// 「誰が消したか分からない」を許容できないと決めている)。
//
// 対応する UPDATE は各テーブルのクエリファイルにあります ——
// この構造体が持つのは呼び出しだけで、SQL は 1 か所に集まったままです。
type ModerationRepository struct {
	q *sqlcgen.Queries
	// pool はトランザクションを開始するために持ちます。
	// **トランザクションの中で作った実体では nil になります**
	// (入れ子のトランザクションを作らせないため)。
	pool *pgxpool.Pool
}

var _ repository.Repository = (*ModerationRepository)(nil)

// NewModerationRepository は接続プールからリポジトリを生成します。
func NewModerationRepository(pool *pgxpool.Pool) *ModerationRepository {
	return &ModerationRepository{q: sqlcgen.New(pool), pool: pool}
}

// WithinTx は 1 つのトランザクションの中でリポジトリを使います。
//
// **削除と記録を分けないために要ります。** 別トランザクションにすると、
// 片方だけ成功した状態が作れます。
//
// READ COMMITTED でよい。触るのは対象 1 行と記録 1 行だけで、
// どちらも「読んでから書く」判断を含みません
// (削除は UPDATE ... WHERE deleted_at IS NULL の 1 文で完結し、
// 更新行数がそのまま「今回消したか」になる)。
func (r *ModerationRepository) WithinTx(
	ctx context.Context, fn func(repository.Repository) error,
) error {
	if r.pool == nil {
		return fmt.Errorf("ModerationRepository.WithinTx: トランザクションの入れ子は作れません")
	}
	return runInTx(ctx, "ModerationRepository.WithinTx", r.pool, pgx.ReadCommitted,
		func(_ pgx.Tx, q *sqlcgen.Queries) error {
			// pool を渡さない = この中でさらに WithinTx は呼べない。
			return fn(&ModerationRepository{q: q})
		})
}

// RecordAction はモデレーション操作を記録します。
func (r *ModerationRepository) RecordAction(
	ctx context.Context, a *model.Action,
) (*model.Action, error) {
	row, err := r.q.CreateModerationAction(ctx, sqlcgen.CreateModerationActionParams{
		ActorID:    a.ActorID,
		Action:     string(a.Type),
		TargetType: string(a.Target),
		TargetID:   a.TargetID,
		Reason:     a.Reason,
	})
	if err != nil {
		return nil, translateError("ModerationRepository.RecordAction", err)
	}
	return &model.Action{
		ID:        row.ID,
		ActorID:   row.ActorID,
		Type:      model.ActionType(row.Action),
		Target:    model.TargetType(row.TargetType),
		TargetID:  row.TargetID,
		Reason:    row.Reason,
		CreatedAt: row.CreatedAt,
	}, nil
}

// SoftDeleteThread はスレッドを論理削除します。
func (r *ModerationRepository) SoftDeleteThread(ctx context.Context, id int64) error {
	affected, err := r.q.SoftDeleteThread(ctx, id)
	if err != nil {
		return translateError("ModerationRepository.SoftDeleteThread", err)
	}
	return requireAffected("ModerationRepository.SoftDeleteThread", affected)
}

// SoftDeleteComment はコメントを論理削除します。
//
// **threadID はパーティションキーです。** 省くと 8 パーティションすべてに
// UPDATE が飛びます (主キーが (thread_id, id) のため)。
func (r *ModerationRepository) SoftDeleteComment(ctx context.Context, threadID, id int64) error {
	affected, err := r.q.SoftDeleteComment(ctx, sqlcgen.SoftDeleteCommentParams{
		ThreadID: threadID,
		ID:       id,
	})
	if err != nil {
		return translateError("ModerationRepository.SoftDeleteComment", err)
	}
	return requireAffected("ModerationRepository.SoftDeleteComment", affected)
}

// MarkImageDeleted は images.status を 'deleted' にします。
//
// **ストレージの実体はここでは消えません。** 回収バッチが後から消します
// (ADR 0011 決定 5)。この UPDATE で回収の対象に入ります ——
// images_reclaimable_idx の述語が status = 'deleted' を含むためです。
func (r *ModerationRepository) MarkImageDeleted(ctx context.Context, id uuid.UUID) error {
	affected, err := r.q.MarkImageDeleted(ctx, id)
	if err != nil {
		return translateError("ModerationRepository.MarkImageDeleted", err)
	}
	return requireAffected("ModerationRepository.MarkImageDeleted", affected)
}

// ChangeRole は利用者のロールを変更し、その内部 ID を返します。
//
// **記録と同じトランザクションで呼ばれます** (ADR 0011 決定 1
// 「変更は監査記録に残す」)。分けると「権限が変わったのに
// 誰がやったか分からない」状態が作れます。
//
// 返すのが内部 ID なのは、moderation_actions.target_id に書くためです。
// **公開 ID (public_id) ではありません** —— 監査記録から users を
// 辿るのに内部 ID が要ります。
func (r *ModerationRepository) ChangeRole(
	ctx context.Context, publicID uuid.UUID, role string,
) (int64, error) {
	row, err := r.q.ChangeUserRole(ctx, sqlcgen.ChangeUserRoleParams{
		PublicID: publicID,
		Role:     role,
	})
	if err != nil {
		// 0 行は「居ない、または退会済み」。translateError が 404 にする。
		return 0, translateError("ModerationRepository.ChangeRole", err)
	}
	return row.ID, nil
}

// requireAffected は更新行数が 0 なら apperr.ErrNotFound にします。
//
// **「今回この操作で変化した」ことをここで確かめます。**
// どの削除クエリも「まだ消えていないこと」を WHERE に含めているため、
// 0 行は「対象が無い」か「既に消えている」のどちらかです。
// 呼び出し側から見ればどちらも 404 で、区別して伝える理由がありません
// (区別すると、消された投稿の存在を確かめる手段になります)。
//
// 成功にしてしまうと、実際には何も起きていない操作が
// モデレーション記録に残ります。
func requireAffected(op string, affected int64) error {
	if affected == 0 {
		return fmt.Errorf("%s: %w", op, apperr.ErrNotFound)
	}
	return nil
}
