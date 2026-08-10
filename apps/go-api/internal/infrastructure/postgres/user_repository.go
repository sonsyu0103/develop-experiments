package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
// 0 行になります。
//
// この 0 行を repository.ErrWithdrawn に翻訳します。
// 汎用の translateError に任せると apperr.ErrNotFound になり、
// HTTP 層で 404 に落ちて「アカウントが無い」と「閉じられている」を
// 区別できなくなるためです。
//
// このクエリで 0 行になるのは退会済みの場合だけです
// (新規なら INSERT が成立し、生存中なら DO UPDATE が成立するため)。
func (r *UserRepository) Upsert(ctx context.Context, user *model.User) (*model.User, error) {
	row, err := r.q.UpsertUser(ctx, sqlcgen.UpsertUserParams{
		PublicID:    user.PublicID,
		GoogleSub:   user.GoogleSub,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		AvatarUrl:   user.AvatarURL,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("UserRepository.Upsert: %w", repository.ErrWithdrawn)
	}
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

// ListAuthorsByIDs は投稿者の表示に必要な情報だけをまとめて取得します。
func (r *UserRepository) ListAuthorsByIDs(ctx context.Context, ids []int64) ([]model.Author, error) {
	rows, err := r.q.ListAuthorsByIDs(ctx, ids)
	if err != nil {
		return nil, translateError("UserRepository.ListAuthorsByIDs", err)
	}

	authors := make([]model.Author, 0, len(rows))
	for _, row := range rows {
		authors = append(authors, model.Author{
			ID:          row.ID,
			PublicID:    row.PublicID,
			DisplayName: row.DisplayName,
			AvatarURL:   row.AvatarUrl,
			DeletedAt:   row.DeletedAt,
		})
	}
	return authors, nil
}

func toUserModel(row sqlcgen.User) *model.User {
	return model.Reconstruct(
		row.ID, row.PublicID, row.GoogleSub, row.Email, row.DisplayName,
		row.AvatarUrl, row.CreatedAt, row.UpdatedAt, row.DeletedAt,
	)
}
