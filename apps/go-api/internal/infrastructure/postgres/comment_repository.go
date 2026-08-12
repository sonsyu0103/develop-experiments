package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/comment/domain/repository"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
	"develop-experiments/apps/go-api/internal/pagination"
)

// CommentRepository は repository.CommentRepository の PostgreSQL 実装です。
//
// **投稿だけが 4 通りの実装を持ちます** (docs/adr/0019-comment-concurrency.md 決定 2)。
// レス番号の採番が「読んでから書く」処理であり、
// 同時投稿で必ず競合するためです。取得と削除は競合しないので 1 通りです。
type CommentRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
	mode config.CommentPostMode
	// retry は tx を伴うモードで使う方針です。テストから差し替えます。
	retry RetryPolicy
}

var _ repository.CommentRepository = (*CommentRepository)(nil)

// NewCommentRepository は接続プールからリポジトリを生成します。
//
// mode が空文字の場合は既定 (unique) として扱います。
// 値の検証は config 側で済んでいます (未知の値は起動時にエラー)。
func NewCommentRepository(pool *pgxpool.Pool, mode config.CommentPostMode) *CommentRepository {
	if mode == "" {
		mode = config.CommentPostModeUnique
	}
	return &CommentRepository{
		pool:  pool,
		q:     sqlcgen.New(pool),
		mode:  mode,
		retry: DefaultRetryPolicy,
	}
}

// ListByThreadID は 1 スレッドのコメントを新しい順に取得します。
func (r *CommentRepository) ListByThreadID(
	ctx context.Context, threadID int64, page pagination.Page,
) ([]model.Comment, error) {
	rows, err := r.q.ListCommentsByThreadID(ctx, sqlcgen.ListCommentsByThreadIDParams{
		ThreadID: threadID,
		CursorID: page.CursorID(),
		PageSize: page.Size,
	})
	if err != nil {
		return nil, translateError("CommentRepository.ListByThreadID", err)
	}

	comments := make([]model.Comment, 0, len(rows))
	for _, row := range rows {
		comments = append(comments, *model.Reconstruct(
			row.ID, row.ThreadID, row.Seq, row.AuthorName,
			toCommentAuthor(row.AuthorPublicID, row.AuthorDisplayName,
				row.AuthorAvatarUrl, row.AuthorDeletedAt),
			row.Body, row.CreatedAt,
		))
	}
	return comments, nil
}

// Create はコメントを保存し、採番済みの値を返します。
//
// 実際の並行制御は config.CommentPostMode が決めます。
// どのモードでも外から見える契約は同じで、
// 親スレッドが無ければ apperr.ErrNotFound、
// リトライしても競合が解けなければ apperr.ErrConflict を返します。
//
// **naive モードだけはレス番号が重複します。** それを実測するための実装です。
func (r *CommentRepository) Create(ctx context.Context, comment *model.Comment) (*model.Comment, error) {
	var (
		created  *model.Comment
		attempts int
		err      error
	)

	if r.mode == config.CommentPostModeUnique {
		created, attempts, err = r.createAutoSeq(ctx, comment)
	} else {
		created, attempts, err = r.createInTx(ctx, comment)
	}
	if err != nil {
		return nil, err
	}

	// 成功したことの記録 (ADR 0010 の 4-3 / ADR 0019 の決定 4)。
	//
	// **本番で直列化失敗を集計する起点はこの行になります。**
	// 各試行の DEBUG は本番では出力されないため、
	// attempts > 1 の行がリトライして成功した投稿を表します。
	slog.LogAttrs(ctx, slog.LevelInfo, "comment_created",
		slog.Int64("thread_id", created.ThreadID),
		slog.Int64("comment_id", created.ID),
		slog.Int("seq", int(created.Seq)),
		slog.Int("attempts", attempts),
		slog.String("mode", string(r.mode)),
	)
	return created, nil
}

// createInTx は ssi / pessimistic / naive の 3 モードを実装します。
//
// 3 つの違いは**分離レベルと親行のロックだけ**です。
// 手順そのもの (存在確認 → 採番 → 挿入) は同じにしてあります。
// 手順まで変えると、Phase 4 の比較に別の交絡が入ります。
func (r *CommentRepository) createInTx(
	ctx context.Context, comment *model.Comment,
) (*model.Comment, int, error) {
	const op = "CommentRepository.Create"

	s := r.strategy()
	run := retrier{
		policy:    s.policy,
		retryable: IsRetryable,
		attrs: []slog.Attr{
			slog.Int64("thread_id", comment.ThreadID),
			slog.String("mode", string(r.mode)),
		},
	}

	var row sqlcgen.CreateCommentWithSeqRow
	attempts, err := run.do(ctx, func() error {
		return runInTx(ctx, r.pool, s.iso, func(q *sqlcgen.Queries) error {
			if err := r.ensureThreadAlive(ctx, q, comment.ThreadID, s.lockParent); err != nil {
				return err
			}

			// **ここが競合する 1 行。**
			// 読んだ値が有効であり続ける保証は、この文自身には無い。
			// ssi では SSI の述語ロックが、pessimistic では上の FOR UPDATE が、
			// naive では**何も**保証しない。
			seq, err := q.NextCommentSeq(ctx, comment.ThreadID)
			if err != nil {
				return translateError(op, err)
			}

			row, err = q.CreateCommentWithSeq(ctx, sqlcgen.CreateCommentWithSeqParams{
				ThreadID:   comment.ThreadID,
				Seq:        seq,
				AuthorName: comment.AuthorName,
				Body:       comment.Body,
				AuthorID:   comment.AuthorID,
			})
			if err != nil {
				return translateError(op, err)
			}
			return nil
		})
	})
	if err != nil {
		return nil, attempts, err
	}

	return r.toModel(row, comment.AuthorID), attempts, nil
}

// createAutoSeq は unique モードを実装します。
//
// **トランザクションで包んでいません。** 1 文で完結する処理を
// BEGIN / COMMIT で挟むと往復が 3 倍になり、
// 「READ COMMITTED のまま 1 往復で済む」というこのモードの利点そのものが
// 測定から消えます (ADR 0019 決定 2)。
//
// 単文は暗黙のトランザクションとして原子的に実行されるため、
// 正しさの上でも包む必要はありません。
func (r *CommentRepository) createAutoSeq(
	ctx context.Context, comment *model.Comment,
) (*model.Comment, int, error) {
	const op = "CommentRepository.Create"

	run := retrier{
		policy: r.retry,
		// **一意制約違反を無条件にリトライしません。**
		// レス番号の索引に対する違反だけがやり直す価値を持ちます。
		retryable: isCommentSeqConflict,
		attrs: []slog.Attr{
			slog.Int64("thread_id", comment.ThreadID),
			slog.String("mode", string(r.mode)),
		},
	}

	var row sqlcgen.CreateCommentAutoSeqRow
	attempts, err := run.do(ctx, func() error {
		var err error
		row, err = r.q.CreateCommentAutoSeq(ctx, sqlcgen.CreateCommentAutoSeqParams{
			ThreadID:   comment.ThreadID,
			AuthorName: comment.AuthorName,
			Body:       comment.Body,
			AuthorID:   comment.AuthorID,
		})
		return err
	})
	if err != nil {
		// リトライを使い切った採番の衝突は「競合」であって「サーバの不具合」ではない。
		// translateError は 23505 を知らないので、ここで 409 側へ寄せる。
		if isCommentSeqConflict(err) {
			return nil, attempts, fmt.Errorf(
				"%s: レス番号の採番が %d 回連続で衝突しました: %w", op, attempts, apperr.ErrConflict)
		}
		return nil, attempts, translateError(op, err)
	}

	return r.toModel(sqlcgen.CreateCommentWithSeqRow(row), comment.AuthorID), attempts, nil
}

// ensureThreadAlive は親スレッドが生存していることを確かめます。
//
// lock が真なら行ロックまで取ります (pessimistic モード)。
// **ロックの対象が threads であって comments でないことが要点**です。
// 採番はまだ存在しない行を巡る競合なので、コメント側の行ロックでは防げません。
func (r *CommentRepository) ensureThreadAlive(
	ctx context.Context, q *sqlcgen.Queries, threadID int64, lock bool,
) error {
	const op = "CommentRepository.Create"

	if lock {
		// 行が返らない = スレッドが無い or 論理削除済み。存在確認を兼ねる。
		if _, err := q.LockThreadForUpdate(ctx, threadID); err != nil {
			return translateError(op, err)
		}
		return nil
	}

	// SERIALIZABLE では、この読み取りが述語ロック (SIREAD) の対象になる。
	// 他のトランザクションを待たせないが、競合の検出には使われる。
	alive, err := q.ThreadExists(ctx, threadID)
	if err != nil {
		return translateError(op, err)
	}
	if !alive {
		return fmt.Errorf("%s: スレッド %d: %w", op, threadID, apperr.ErrNotFound)
	}
	return nil
}

// postStrategy は 1 つのモードが選ぶ「分離レベル・ロック・リトライ」です。
type postStrategy struct {
	iso        pgx.TxIsoLevel
	lockParent bool
	policy     RetryPolicy
}

// noRetryPolicy は 1 回だけ試す方針です。naive モード専用。
var noRetryPolicy = RetryPolicy{MaxAttempts: 1}

func (r *CommentRepository) strategy() postStrategy {
	switch r.mode {
	case config.CommentPostModePessimistic:
		// FOR UPDATE で待つので、直列化失敗は起きない。
		// それでもリトライを付けるのは、将来ロックの取得順が増えたときの
		// デッドロック (40P01) を拾えるようにするため。
		return postStrategy{iso: pgx.ReadCommitted, lockParent: true, policy: r.retry}
	case config.CommentPostModeNaive:
		// **リトライしない。** リトライを付けると、
		// 一意制約違反を偶然やり過ごして「壊れていない」ように見えてしまう。
		// このモードの役目は壊れることを見せることにある。
		return postStrategy{iso: pgx.ReadCommitted, lockParent: false, policy: noRetryPolicy}
	default:
		// ssi。unique は createAutoSeq が処理するのでここには来ない。
		return postStrategy{iso: pgx.Serializable, lockParent: false, policy: r.retry}
	}
}

// toModel は挿入結果をドメインモデルへ詰め替えます。
func (r *CommentRepository) toModel(row sqlcgen.CreateCommentWithSeqRow, authorID *int64) *model.Comment {
	created := model.Reconstruct(row.ID, row.ThreadID, row.Seq, row.AuthorName,
		toCommentAuthor(row.AuthorPublicID, row.AuthorDisplayName,
			row.AuthorAvatarUrl, row.AuthorDeletedAt),
		row.Body, row.CreatedAt)
	// 書いた内容を戻り値に反映させる (thread_repository.go と同じ理由)。
	created.AuthorID = authorID
	return created
}

// SoftDelete はコメントを論理削除します。
//
// **レス番号は消しません。** 行が残るので seq も残り、
// 次の投稿は削除された番号の次から続きます (ADR 0019 決定 5)。
func (r *CommentRepository) SoftDelete(ctx context.Context, threadID, id int64) error {
	affected, err := r.q.SoftDeleteComment(ctx, sqlcgen.SoftDeleteCommentParams{
		ThreadID: threadID,
		ID:       id,
	})
	if err != nil {
		return translateError("CommentRepository.SoftDelete", err)
	}
	if affected == 0 {
		return fmt.Errorf("CommentRepository.SoftDelete: %w", apperr.ErrNotFound)
	}
	return nil
}
