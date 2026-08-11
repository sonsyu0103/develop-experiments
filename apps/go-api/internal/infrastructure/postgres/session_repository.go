package postgres

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
	"develop-experiments/apps/go-api/internal/user/domain/model"
	"develop-experiments/apps/go-api/internal/user/domain/repository"
)

// SessionRepository は repository.SessionRepository の PostgreSQL 実装です。
type SessionRepository struct {
	q *sqlcgen.Queries
}

var _ repository.SessionRepository = (*SessionRepository)(nil)

// NewSessionRepository は接続プールからリポジトリを生成します。
func NewSessionRepository(pool *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{q: sqlcgen.New(pool)}
}

// Create はセッションを保存します。
func (r *SessionRepository) Create(ctx context.Context, session *model.Session) (*model.Session, error) {
	row, err := r.q.CreateSession(ctx, sqlcgen.CreateSessionParams{
		ID:        session.ID,
		UserID:    session.UserID,
		ExpiresAt: session.ExpiresAt,
	})
	if err != nil {
		return nil, translateError("SessionRepository.Create", err)
	}
	return model.ReconstructSession(row.ID, row.UserID, row.ExpiresAt, row.CreatedAt), nil
}

// FindLive は有効なセッションを、持ち主の情報つきで取得します。
//
// 期限切れと退会の判定は SQL 側にあります。ここで再判定しないのは、
// 判定を 2 か所に置くと片方だけ直したときに食い違うためです。
func (r *SessionRepository) FindLive(ctx context.Context, token model.SessionToken) (*model.AuthenticatedSession, error) {
	// 保存されているのはハッシュ。生のトークンでは引けない。
	row, err := r.q.GetLiveSessionWithUser(ctx, token.Hash())
	if err != nil {
		return nil, translateError("SessionRepository.FindLive", err)
	}

	// ロールが読めない値なら一般利用者へ倒し、記録に残す。
	// **昇格側へ倒さない。** 読めない値を管理者として扱うと、
	// 制約を外した瞬間に権限が広がる。
	role, err := model.ParseRole(row.Role)
	if err != nil {
		slog.ErrorContext(ctx, "ロールを解釈できません",
			slog.Int64("owner_id", row.UserID), slog.String("role", row.Role))
		role = model.RoleUser
	}

	return &model.AuthenticatedSession{
		Session: *model.ReconstructSession(row.ID, row.UserID, row.ExpiresAt, row.CreatedAt),
		Owner: model.SessionOwner{
			ID:          row.UserID,
			PublicID:    row.PublicID,
			Email:       row.Email,
			DisplayName: row.DisplayName,
			AvatarURL:   row.AvatarUrl,
			Role:        role,
		},
	}, nil
}

// Delete は 1 件のセッションを削除します。
//
// 0 件でもエラーにしません。目的は「そのセッションがもう使えないこと」であり、
// 既に無いならその目的は達成されているためです。
func (r *SessionRepository) Delete(ctx context.Context, token model.SessionToken) error {
	if _, err := r.q.DeleteSession(ctx, token.Hash()); err != nil {
		return translateError("SessionRepository.Delete", err)
	}
	return nil
}

// DeleteByUserID は利用者のセッションをすべて削除します。
func (r *SessionRepository) DeleteByUserID(ctx context.Context, userID int64) (int64, error) {
	n, err := r.q.DeleteSessionsByUserID(ctx, userID)
	if err != nil {
		return 0, translateError("SessionRepository.DeleteByUserID", err)
	}
	return n, nil
}

// DeleteExpired は期限切れのセッションを最大 maxRows 件削除します。
func (r *SessionRepository) DeleteExpired(ctx context.Context, maxRows int32) (int64, error) {
	n, err := r.q.DeleteExpiredSessions(ctx, clampMaxRows(maxRows))
	if err != nil {
		return 0, translateError("SessionRepository.DeleteExpired", err)
	}
	return n, nil
}

// defaultDeleteExpiredMaxRows は maxRows が指定されなかった場合の 1 回あたりの上限です。
const defaultDeleteExpiredMaxRows int32 = 1000

// clampMaxRows は LIMIT に渡す件数を安全な範囲へ丸めます。
//
// 0 をそのまま流すと LIMIT 0 になり、1 件も消えないまま戻り値も 0 になります。
// 呼び出し規約が「0 件になるまで繰り返す」なので、
// **バグの症状と正常な終了条件が区別できません。**
// 掃除が止まっていることに誰も気づけないため、境界でここを塞ぎます。
//
// 負の値は PostgreSQL が "LIMIT must not be negative" で実行時に落とします。
func clampMaxRows(maxRows int32) int32 {
	if maxRows <= 0 {
		return defaultDeleteExpiredMaxRows
	}
	return maxRows
}
