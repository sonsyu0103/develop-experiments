// Package repository は利用者とセッションの永続化に対するインターフェースを定義します。
//
// インターフェースをドメイン側に置き、実装を infrastructure 側に置くことで、
// ユースケース層が PostgreSQL にも sqlc にも依存しなくなります (依存性逆転)。
package repository

import (
	"context"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/user/domain/model"
)

// UserRepository は利用者の読み書きを担います。
type UserRepository interface {
	// Upsert はログイン時に呼び、google_sub で照合して無ければ作成します。
	//
	// 退会済みの利用者に一致した場合は apperr.ErrNotFound を返します。
	// 「復活させる」か「拒否する」かが未決のため、黙って復活させない側に
	// 倒してあります (db/query/users.sql の UpsertUser を参照)。
	Upsert(ctx context.Context, user *model.User) (*model.User, error)

	// FindByID は内部 ID で取得します。退会済みは見つかりません。
	FindByID(ctx context.Context, id int64) (*model.User, error)

	// FindByPublicID は公開用の識別子で取得します。
	// API から来る識別子は常にこちらです。
	FindByPublicID(ctx context.Context, publicID uuid.UUID) (*model.User, error)

	// ListByIDs は複数の利用者をまとめて取得します。
	//
	// 投稿一覧の投稿者表示で使います。1 件ずつ引くと N+1 になるため、
	// 呼び出し側は author_id を集めてから 1 回で引いてください。
	// 退会済みの利用者も含めて返します —— 投稿は匿名化されるまで残るため、
	// 「退会したので表示できない」を呼び出し側が判断できる必要があります。
	ListByIDs(ctx context.Context, ids []int64) ([]model.User, error)
}

// SessionRepository はセッションの読み書きを担います。
type SessionRepository interface {
	// Create はセッションを保存します。
	Create(ctx context.Context, session *model.Session) (*model.Session, error)

	// FindLive は有効なセッションを、持ち主の情報つきで取得します。
	//
	// 期限切れ・退会済みの場合は apperr.ErrNotFound を返します。
	// この判定は SQL 側に置いてあり、アプリ側では行いません。
	FindLive(ctx context.Context, id string) (*model.AuthenticatedSession, error)

	// Delete は 1 件のセッションを削除します (ログアウト)。
	// 対象が無い場合も成功として扱います —— 目的は「もう使えないこと」であり、
	// 既に無いならその目的は達成されているためです。
	Delete(ctx context.Context, id string) error

	// DeleteByUserID は利用者のセッションをすべて削除します
	// (全端末からのログアウト、権限剥奪の直後)。削除件数を返します。
	DeleteByUserID(ctx context.Context, userID int64) (int64, error)

	// DeleteExpired は期限切れのセッションを最大 maxRows 件削除し、
	// 削除件数を返します。定期処理から「0 件になるまで」呼びます。
	//
	// 一度に消す件数を区切るのは、放置後に大量にたまっていた場合でも
	// 1 トランザクションを短く保つためです。
	DeleteExpired(ctx context.Context, maxRows int32) (int64, error)
}
