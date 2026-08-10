package postgres

import (
	"context"

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
func (r *SessionRepository) FindLive(ctx context.Context, id string) (*model.AuthenticatedSession, error) {
	row, err := r.q.GetLiveSessionWithUser(ctx, id)
	if err != nil {
		return nil, translateError("SessionRepository.FindLive", err)
	}

	return &model.AuthenticatedSession{
		Session: *model.ReconstructSession(row.ID, row.UserID, row.ExpiresAt, row.CreatedAt),
		Owner: model.SessionOwner{
			ID:          row.UserID,
			PublicID:    row.PublicID,
			Email:       row.Email,
			DisplayName: row.DisplayName,
			AvatarURL:   row.AvatarUrl,
		},
	}, nil
}

// Delete は 1 件のセッションを削除します。
//
// 0 件でもエラーにしません。目的は「そのセッションがもう使えないこと」であり、
// 既に無いならその目的は達成されているためです。
func (r *SessionRepository) Delete(ctx context.Context, id string) error {
	if _, err := r.q.DeleteSession(ctx, id); err != nil {
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
	n, err := r.q.DeleteExpiredSessions(ctx, maxRows)
	if err != nil {
		return 0, translateError("SessionRepository.DeleteExpired", err)
	}
	return n, nil
}
