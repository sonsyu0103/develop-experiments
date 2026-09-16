package postgres

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
	"develop-experiments/apps/go-api/internal/user/domain/model"
	"develop-experiments/apps/go-api/internal/user/domain/repository"
)

// SessionRepository は repository.SessionRepository の PostgreSQL 実装です。
type SessionRepository struct {
	q *sqlcgen.Queries
	// pool はトランザクションを開始するために持ちます (SetAvatarImage)。
	pool *pgxpool.Pool
}

var _ repository.SessionRepository = (*SessionRepository)(nil)

// NewSessionRepository は接続プールからリポジトリを生成します。
func NewSessionRepository(pool *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{q: sqlcgen.New(pool), pool: pool}
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
		slog.ErrorContext(ctx, "role_parse_failed",
			slog.Int64("owner_id", row.UserID), slog.String("role", row.Role))
		role = model.RoleUser
	}

	return &model.AuthenticatedSession{
		Session: *model.ReconstructSession(row.ID, row.UserID, row.ExpiresAt, row.CreatedAt),
		Owner: model.SessionOwner{
			ID:              row.UserID,
			PublicID:        row.PublicID,
			Email:           row.Email,
			DisplayName:     row.DisplayName,
			AvatarURL:       row.AvatarUrl,
			Role:            role,
			AvatarObjectKey: row.AvatarObjectKey,
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

// SetAvatarImage はプロフィール画像を設定します。
//
// **所有者の確認はユースケース層が済ませています** (repository.go の doc を参照)。
//
// 【利用者の行ロックを先に取る】(DB レビュー)
// 同じ利用者への付け替えが同時に走ると、SetUserAvatarImage の previous が
// 両方とも同じ旧画像を読み、片方が付けた画像を外し損ねます
// (attached_at が入ったまま参照されず、永久に回収されない)。
// 先に LockUserForAvatarChange で行ロックを取り、**READ COMMITTED の
// 次の文のスナップショットを相手のコミット後にする**ことで防ぎます。
// SQL 側の説明は users.sql にあります。
//
// 画像のキーを引き直す文も同じトランザクションに入れます。
// 外に出すと、付け替えた直後に回収バッチが行を消した場合に
// 「設定は成功したのに 404」という応答になります。
func (r *SessionRepository) SetAvatarImage(
	ctx context.Context, userID int64, imageID *uuid.UUID,
) (*model.SessionOwner, error) {
	const op = "SessionRepository.SetAvatarImage"

	var (
		row       sqlcgen.User
		objectKey *string
	)
	err := runInTx(ctx, op, r.pool, pgx.ReadCommitted, func(_ pgx.Tx, q *sqlcgen.Queries) error {
		// 退会済みならここで 0 行になり、404 に翻訳されます。
		if _, err := q.LockUserForAvatarChange(ctx, userID); err != nil {
			return translateError(op, err)
		}

		// 画像が使えない状態 (回収の確保済みなど) なら 0 行になり、404 に翻訳されます。
		var err error
		row, err = q.SetUserAvatarImage(ctx, sqlcgen.SetUserAvatarImageParams{
			ID:            userID,
			AvatarImageID: imageID,
		})
		if err != nil {
			return translateError(op, err)
		}

		// **設定した画像のキーを引き直す。** UPDATE の RETURNING は
		// images を結合できないため、URL の組み立てに要るキーが手に入らない。
		// 解除 (nil) のときは引かない。
		if imageID != nil {
			img, findErr := q.GetImageByID(ctx, *imageID)
			if findErr != nil {
				return translateError(op, findErr)
			}
			objectKey = &img.ObjectKey
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	role, err := model.ParseRole(row.Role)
	if err != nil {
		slog.ErrorContext(ctx, "role_parse_failed",
			slog.Int64("owner_id", row.ID), slog.String("role", row.Role))
		role = model.RoleUser
	}

	return &model.SessionOwner{
		ID:              row.ID,
		PublicID:        row.PublicID,
		Email:           row.Email,
		DisplayName:     row.DisplayName,
		AvatarURL:       row.AvatarUrl,
		Role:            role,
		AvatarObjectKey: objectKey,
	}, nil
}
