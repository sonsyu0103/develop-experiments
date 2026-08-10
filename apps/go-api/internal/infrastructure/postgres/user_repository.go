package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
	"develop-experiments/apps/go-api/internal/user/domain/model"
	"develop-experiments/apps/go-api/internal/user/domain/repository"
)

// UserRepository は repository.UserRepository の PostgreSQL 実装です。
type UserRepository struct {
	q *sqlcgen.Queries
}

var _ repository.UserRepository = (*UserRepository)(nil)

// NewUserRepository は接続プールからリポジトリを生成します。
func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{q: sqlcgen.New(pool)}
}

// Upsert はログイン時に呼び、google_sub で照合して無ければ作成します。
//
// 退会済みの利用者に一致した場合、SQL 側の WHERE で DO UPDATE が実行されず
// 0 行になります。pgx.ErrNoRows が返るため translateError が
// apperr.ErrNotFound に翻訳し、呼び出し側が「退会済み」として扱えます。
func (r *UserRepository) Upsert(ctx context.Context, user *model.User) (*model.User, error) {
	row, err := r.q.UpsertUser(ctx, sqlcgen.UpsertUserParams{
		PublicID:    user.PublicID,
		GoogleSub:   user.GoogleSub,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		AvatarUrl:   user.AvatarURL,
	})
	if err != nil {
		return nil, translateError("UserRepository.Upsert", err)
	}
	return toUserModel(row), nil
}

// FindByID は内部 ID で取得します。
func (r *UserRepository) FindByID(ctx context.Context, id int64) (*model.User, error) {
	row, err := r.q.GetUserByID(ctx, id)
	if err != nil {
		return nil, translateError("UserRepository.FindByID", err)
	}
	return toUserModel(row), nil
}

// FindByPublicID は公開用の識別子で取得します。
func (r *UserRepository) FindByPublicID(ctx context.Context, publicID uuid.UUID) (*model.User, error) {
	row, err := r.q.GetUserByPublicID(ctx, publicID)
	if err != nil {
		return nil, translateError("UserRepository.FindByPublicID", err)
	}
	return toUserModel(row), nil
}

// ListByIDs は複数の利用者をまとめて取得します。
func (r *UserRepository) ListByIDs(ctx context.Context, ids []int64) ([]model.User, error) {
	rows, err := r.q.ListUsersByIDs(ctx, ids)
	if err != nil {
		return nil, translateError("UserRepository.ListByIDs", err)
	}

	users := make([]model.User, 0, len(rows))
	for _, row := range rows {
		users = append(users, *toUserModel(row))
	}
	return users, nil
}

func toUserModel(row sqlcgen.User) *model.User {
	return model.Reconstruct(
		row.ID, row.PublicID, row.GoogleSub, row.Email, row.DisplayName,
		row.AvatarUrl, row.CreatedAt, row.UpdatedAt, row.DeletedAt,
	)
}
