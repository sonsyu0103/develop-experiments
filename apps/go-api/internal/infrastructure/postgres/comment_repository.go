package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/comment/domain/repository"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/idempotency"
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
		return runInTx(ctx, op, r.pool, s.iso, func(_ pgx.Tx, q *sqlcgen.Queries) error {
			var err error
			row, err = r.insertWithSeq(ctx, q, comment, s.lockParent)
			return err
		})
	})
	if err != nil {
		return nil, attempts, err
	}

	return r.toModel(row, comment.AuthorID), attempts, nil
}

// insertWithSeq は「存在確認 → 採番 → 挿入」の 3 手順です。
// ssi / pessimistic / naive が共有し、冪等な経路からも呼ばれます。
func (r *CommentRepository) insertWithSeq(
	ctx context.Context, q *sqlcgen.Queries, comment *model.Comment, lockParent bool,
) (sqlcgen.CreateCommentWithSeqRow, error) {
	const op = "CommentRepository.Create"

	var zero sqlcgen.CreateCommentWithSeqRow
	if err := r.ensureThreadAlive(ctx, q, comment.ThreadID, lockParent); err != nil {
		return zero, err
	}

	// **ここが競合する 1 行。**
	// 読んだ値が有効であり続ける保証は、この文自身には無い。
	// ssi では SSI の述語ロックが、pessimistic では上の FOR UPDATE が、
	// naive では**何も**保証しない。
	seq, err := q.NextCommentSeq(ctx, comment.ThreadID)
	if err != nil {
		return zero, translateError(op, err)
	}

	row, err := q.CreateCommentWithSeq(ctx, sqlcgen.CreateCommentWithSeqParams{
		ThreadID:   comment.ThreadID,
		Seq:        seq,
		AuthorName: comment.AuthorName,
		Body:       comment.Body,
		AuthorID:   comment.AuthorID,
	})
	if err != nil {
		return zero, translateError(op, err)
	}
	return row, nil
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

// idempotencyWaitTimeout は冪等キーの確保が待てる上限です。
//
// 同じキーの処理が未コミットのとき、ON CONFLICT DO NOTHING は
// 相手のトランザクションが終わるまで待ちます。
// **待ちはコネクションを占有する**ため、上限を設けて 409 に落とします
// (docs/adr/0015-idempotency.md)。
const idempotencyWaitTimeout = 3 * time.Second

// CreateIdempotent は冪等キーの確保・投稿・応答の記録を **1 トランザクション**で行います。
//
// **別トランザクションにしてはいけません** (ADR 0015 決定 3)。
// 「キーを記録した直後に処理が失敗」したとき、リトライしても
// 「処理済み」と誤判定されて投稿が永久に失われます。
//
// 戻り値は排他的です。
//   - 初回:   created が埋まり、replayed は nil
//   - 再送:   created は nil で、replayed に記録済みの応答本文が入る
//
// encode は初回にだけ呼ばれ、その戻り値が記録されます。
// **応答の組み立てをユースケース層に残すため**の受け口です
// (永続化層が API の表現を知る必要はありません)。
func (r *CommentRepository) CreateIdempotent(
	ctx context.Context, comment *model.Comment, req idempotency.Request,
	encode func(*model.Comment) ([]byte, error),
) (created *model.Comment, replayed []byte, err error) {
	const op = "CommentRepository.CreateIdempotent"

	// 匿名にはキーの名前空間を分ける手段がありません (ADR 0015 決定 4)。
	// ユースケース層が弾いているはずですが、他人の結果を返す事故に直結するため
	// ここでも閉じておきます。
	if comment.AuthorID == nil {
		return nil, nil, fmt.Errorf("%s: 匿名では冪等キーを使えません: %w", op, apperr.ErrInvalidArgument)
	}
	userID := *comment.AuthorID

	s := r.strategy()
	run := retrier{
		policy: s.policy,
		// ssi の直列化失敗と、unique モードの採番衝突の両方をやり直します。
		// **どちらもトランザクションごと巻き戻る**ので、
		// 冪等キーの記録も一緒に消えて、やり直しで再度 INSERT されます。
		retryable: func(err error) bool {
			return IsRetryable(err) || isCommentSeqConflict(err)
		},
		attrs: []slog.Attr{
			slog.Int64("thread_id", comment.ThreadID),
			slog.String("mode", string(r.mode)),
			slog.Bool("idempotent", true),
		},
	}

	var row sqlcgen.CreateCommentWithSeqRow
	attempts, err := run.do(ctx, func() error {
		row, replayed = sqlcgen.CreateCommentWithSeqRow{}, nil
		return runInTx(ctx, op, r.pool, s.iso, func(tx pgx.Tx, q *sqlcgen.Queries) error {
			return r.claimAndInsert(ctx, tx, q, comment, req, userID, encode, &row, &replayed)
		})
	})
	if err != nil {
		// 待ちを打ち切られたのは競合であって、サーバの不具合ではありません。
		//
		// **PgError を捨てずに包みます。** ここで apperr.ErrConflict だけにすると、
		// IsRetryable が「競合だからやり直す」と判断し、
		// **3 秒の待ちを最大 12 回繰り返す**ことになります。
		if isStatementTimeout(err) {
			return nil, nil, fmt.Errorf("%s: 冪等キーの確保が %s 以内に終わりませんでした: %w",
				op, idempotencyWaitTimeout, errors.Join(err, apperr.ErrConflict))
		}
		// リトライを使い切った採番の衝突は「競合」であって「サーバの不具合」ではありません。
		//
		// **createAutoSeq と同じ扱いに揃えます。** ここを素通しすると
		// 生の 23505 が respondError の default に落ち、
		// **Idempotency-Key を付けた途端に 409 が 500 に変わります。**
		if isCommentSeqConflict(err) {
			return nil, nil, fmt.Errorf(
				"%s: レス番号の採番が %d 回連続で衝突しました: %w", op, attempts, apperr.ErrConflict)
		}
		return nil, nil, err
	}

	if replayed != nil {
		slog.LogAttrs(ctx, slog.LevelInfo, "idempotent_replay",
			slog.Int64("thread_id", comment.ThreadID),
			slog.Int64("user_id", userID),
			slog.String("endpoint", req.Endpoint),
		)
		return nil, replayed, nil
	}

	created = r.toModel(row, comment.AuthorID)
	slog.LogAttrs(ctx, slog.LevelInfo, "comment_created",
		slog.Int64("thread_id", created.ThreadID),
		slog.Int64("comment_id", created.ID),
		slog.Int("seq", int(created.Seq)),
		slog.Int("attempts", attempts),
		slog.String("mode", string(r.mode)),
		slog.Bool("idempotent", true),
	)
	return created, nil, nil
}

// claimAndInsert は ADR 0015 の「処理の流れ」をそのまま実装したものです。
//
//  1. INSERT ... ON CONFLICT DO NOTHING で先着を決める
//     ├─ 挿入できた   → 処理を実行 → response を記録
//     └─ 挿入できない → 既存行を読む
//     ├─ request_hash が違う → 422
//     └─ 一致する           → 記録した response を返す
func (r *CommentRepository) claimAndInsert(
	ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, comment *model.Comment,
	req idempotency.Request, userID int64,
	encode func(*model.Comment) ([]byte, error),
	row *sqlcgen.CreateCommentWithSeqRow, replayed *[]byte,
) error {
	const op = "CommentRepository.CreateIdempotent"

	// **上限を張るのはこの 1 文だけ。**
	// トランザクション全体に効かせたままにすると、
	// pessimistic モードの FOR UPDATE 待ちや重い INSERT も同じ 3 秒で切られ、
	// **すべてが「冪等キーの待ちが長すぎた」という同じ文言の 409 になる。**
	// 障害の切り分けで誤った結論へ誘導することになる。
	if err := setStatementTimeout(ctx, tx, idempotencyWaitTimeout); err != nil {
		return translateError(op, err)
	}

	_, err := q.ClaimIdempotencyKey(ctx, sqlcgen.ClaimIdempotencyKeyParams{
		UserID:      userID,
		Key:         req.Key,
		Endpoint:    req.Endpoint,
		RequestHash: req.RequestHash,
	})

	// 確保の可否にかかわらず、ここで上限を戻す。
	// 戻し忘れると、下の投稿処理まで 3 秒で切られる。
	if resetErr := resetStatementTimeout(ctx, tx); resetErr != nil && err == nil {
		return translateError(op, resetErr)
	}

	switch {
	case err == nil:
		// 先着。ここから下は初回の処理。
	case errors.Is(err, pgx.ErrNoRows):
		// 既に取られている。**待った結果ここに来ているので、相手はコミット済み。**
		return r.replayRecorded(ctx, q, req, userID, replayed)
	default:
		return translateError(op, err)
	}

	inserted, err := r.insertComment(ctx, q, comment)
	if err != nil {
		return err
	}

	body, err := encode(r.toModel(inserted, comment.AuthorID))
	if err != nil {
		return fmt.Errorf("%s: 応答を記録できませんでした: %w", op, err)
	}

	status := int32(http.StatusCreated)
	affected, err := q.CompleteIdempotencyKey(ctx, sqlcgen.CompleteIdempotencyKeyParams{
		UserID:         userID,
		Key:            req.Key,
		ResponseStatus: &status,
		ResponseBody:   body,
	})
	if err != nil {
		return translateError(op, err)
	}
	if affected != 1 {
		// 自分が確保した行なので、必ず 1 行のはず。
		// 0 行なら「キーは確保したが応答を記録していない」状態でコミットされ、
		// 次の再送が永久に待つ側に倒れる。**コミットさせない。**
		return fmt.Errorf("%s: 冪等キーの記録が %d 行に当たりました: %w",
			op, affected, apperr.ErrConflict)
	}

	*row = inserted
	return nil
}

// replayRecorded は記録済みの応答を取り出します。
func (r *CommentRepository) replayRecorded(
	ctx context.Context, q *sqlcgen.Queries, req idempotency.Request,
	userID int64, replayed *[]byte,
) error {
	const op = "CommentRepository.CreateIdempotent"

	existing, err := q.GetIdempotencyKey(ctx, sqlcgen.GetIdempotencyKeyParams{
		UserID: userID,
		Key:    req.Key,
	})
	if err != nil {
		return translateError(op, err)
	}

	// **同じキーで別の内容。** 黙って前回の結果を返すと、
	// クライアントのバグが見えなくなる (ADR 0015)。
	if existing.RequestHash != req.RequestHash {
		// **メッセージに op を含めない。** 422 の本文はそのまま
		// クライアントへ返るため (respondError)、操作名は内部構造の露出になる。
		// translateError の CHECK 違反が同じ理由で op を外しているのに、
		// ここだけ揃っていなかった。切り分けに要る情報はログへ。
		slog.WarnContext(ctx, "idempotency_request_mismatch",
			slog.String("op", op),
			slog.Int64("user_id", userID),
			slog.String("endpoint", req.Endpoint),
		)
		return fmt.Errorf(
			"同じ Idempotency-Key で別の内容が送られました。キーを作り直してください: %w",
			apperr.ErrFailedPrecondition)
	}

	// 記録が無いのは「確保したが応答を書かずにコミットされた」場合だけ。
	// 決定 3 (同一トランザクション) が守られている限り起きない。
	//
	// **apperr.ErrConflict を返してはいけない。**
	// IsRetryable が ErrConflict をリトライ可能と判定するため、
	// **原理的に解消しない状態を 12 回やり直す**ことになる
	// (行は 24 時間残るので、何度読んでも同じ結果になる)。
	// BEGIN → claim → SELECT → ROLLBACK をバックオフつきで空回りさせるだけ。
	//
	// 利用者から見た正しい対処は「キーを作り直す」なので 422 に寄せる。
	// 不変条件が壊れている印なので、記録は ERROR で残す。
	if existing.CompletedAt == nil || len(existing.ResponseBody) == 0 {
		slog.LogAttrs(ctx, slog.LevelError, "idempotency_record_incomplete",
			slog.Int64("user_id", userID),
			slog.String("endpoint", req.Endpoint),
		)
		return fmt.Errorf(
			"冪等キーの記録が未完了です。キーを作り直してください: %w",
			apperr.ErrFailedPrecondition)
	}

	*replayed = existing.ResponseBody
	return nil
}

// insertComment はモードに応じてコメントを 1 件挿入します。
func (r *CommentRepository) insertComment(
	ctx context.Context, q *sqlcgen.Queries, comment *model.Comment,
) (sqlcgen.CreateCommentWithSeqRow, error) {
	const op = "CommentRepository.Create"

	if r.mode == config.CommentPostModeUnique {
		// 1 文で採番する。トランザクションの中でもそのまま使える。
		row, err := q.CreateCommentAutoSeq(ctx, sqlcgen.CreateCommentAutoSeqParams{
			ThreadID:   comment.ThreadID,
			AuthorName: comment.AuthorName,
			Body:       comment.Body,
			AuthorID:   comment.AuthorID,
		})
		if err != nil {
			return sqlcgen.CreateCommentWithSeqRow{}, translateError(op, err)
		}
		return sqlcgen.CreateCommentWithSeqRow(row), nil
	}

	return r.insertWithSeq(ctx, q, comment, r.strategy().lockParent)
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
	case config.CommentPostModeUnique:
		// 通常の投稿はトランザクションを持たない (createAutoSeq が 1 文で完結する)。
		// **冪等キーがあるときだけ**この方針でトランザクションに入る ——
		// キーの記録を主トランザクションに同居させる必要があるため
		// (docs/adr/0015-idempotency.md 決定 3)。
		return postStrategy{iso: pgx.ReadCommitted, lockParent: false, policy: r.retry}
	default:
		// ssi。
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
