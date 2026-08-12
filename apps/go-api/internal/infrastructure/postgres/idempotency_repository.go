package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
)

// IdempotencyRepository は冪等キーの保守を担います。
//
// **投稿の経路はここではありません。** キーの確保と記録は
// 主トランザクションの中で行う必要があるため、CommentRepository が持ちます
// (docs/adr/0015-idempotency.md 決定 3)。
// このリポジトリが持つのは、経路に依存しない後始末だけです。
type IdempotencyRepository struct {
	q *sqlcgen.Queries
}

// NewIdempotencyRepository は接続プールからリポジトリを生成します。
func NewIdempotencyRepository(pool *pgxpool.Pool) *IdempotencyRepository {
	return &IdempotencyRepository{q: sqlcgen.New(pool)}
}

// DefaultRetention は冪等キーを保持する期間です。
//
// 24 時間は「クライアントがリトライを諦めるまでの時間」として十分という判断
// (docs/adr/0015-idempotency.md)。これを過ぎた再送は新規の投稿になります。
const DefaultRetention = 24 * time.Hour

// DeleteExpired は保持期間を過ぎたキーを最大 maxRows 件削除し、削除件数を返します。
//
// **まだどこからも呼ばれていません。**
// 定期処理をどのプロセスで動かすかが未決のためです
// (docs/adr/0003-open-questions.md 未決 #9)。
// 期限切れセッションの削除 (SessionRepository.DeleteExpired) と同じ状態で、
// 決まったときに両方まとめて配線します。
//
// 呼び出し側は「0 件になるまで繰り返す」形で使ってください。
// 1 回あたりの件数を絞っているのは、放置後に大量の行がたまった場合でも
// 1 トランザクションを短く保つためです。
func (r *IdempotencyRepository) DeleteExpired(
	ctx context.Context, retention time.Duration, maxRows int32,
) (int64, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}

	n, err := r.q.DeleteExpiredIdempotencyKeys(ctx, sqlcgen.DeleteExpiredIdempotencyKeysParams{
		Retention: toInterval(retention),
		MaxRows:   clampMaxRows(maxRows),
	})
	if err != nil {
		return 0, translateError("IdempotencyRepository.DeleteExpired", err)
	}
	return n, nil
}

// toInterval は Go の期間を PostgreSQL の interval に変換します。
//
// マイクロ秒で渡すのは、pgtype.Interval が Months / Days / Microseconds の
// 3 つに分かれているためです。**日や月へ丸めません** ——
// interval の日と月は「暦の日」であり、夏時間の切り替えを含む区間では
// 24 時間ちょうどにならないためです。保持期間は絶対時間で扱います。
func toInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
