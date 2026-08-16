package postgres

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"develop-experiments/apps/go-api/internal/apperr"
)

// PostgreSQL の SQLSTATE。
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	// codeForeignKeyViolation は外部キー違反 (親行が存在しない) です。
	codeForeignKeyViolation = "23503"
	// codeUniqueViolation は一意制約違反です。
	//
	// **これを無条件にリトライ可能として扱ってはいけません。**
	// ほとんどの一意制約違反は「同じものを二重に作ろうとした」であり、
	// 何度やり直しても失敗します。リトライしてよいのは、
	// やり直せば別の値を計算し直す経路 (レス番号の採番) だけです。
	codeUniqueViolation = "23505"
	// codeCheckViolation は CHECK 制約違反です。
	codeCheckViolation = "23514"
	// codeSerializationFailure は SERIALIZABLE / REPEATABLE READ における
	// 直列化失敗です。呼び出し側でリトライすれば成功しうる、一時的なエラーです。
	codeSerializationFailure = "40001"
	// codeDeadlockDetected はデッドロック検出です。こちらもリトライ可能です。
	codeDeadlockDetected = "40P01"
	// codeCharacterNotInRepertoire は「その符号化で表せない文字」です。
	//
	// **実際に踏むのは NUL (U+0000) です。** PostgreSQL の text は
	// NUL を格納できず、パラメータとして送ると
	// `invalid byte sequence for encoding "UTF8": 0x00` を返します。
	//
	// **これを拾わないと 500 になります。** 利用者が送った 1 文字で
	// サーバ内部エラーが出る形なので、入力の誤りとして扱います。
	// 各経路の検証をすり抜けた場合の最後の砦で、
	// 本来は入口 (model 側) で弾きます。
	codeCharacterNotInRepertoire = "22021"
)

// classifyDeleteFailure は「本人による削除」が 0 行だった理由を撃ち分けます。
//
// 削除の 1 文が 0 行を返したあと、所有を問い合わせた結果を渡してください。
// 引数の owned / err は、その問い合わせの戻り値をそのまま渡します。
//
//	行が無い        apperr.ErrNotFound         無い、または既に削除済み
//	owned = false   apperr.ErrPermissionDenied 他人のもの、または匿名投稿
//	owned = true    apperr.ErrNotFound         削除との競合 (直前に消えた)
//
// **最後の 1 つが 404 になるのは意図どおりです。** 自分のものだと
// 分かっているのに消せないのは「既に消えている」場合しかありません
// (モデレーターの削除と競合したか、自分で 2 回押したか)。
//
// 403 に隠さないのは、スレッドもコメントも誰でも読めるためです。
// 存在は公開情報なので、404 にしても何も守れないうえ、
// 利用者が「消えたのか、権限が無いのか」を区別できなくなります。
func classifyDeleteFailure(op string, owned bool, err error) error {
	if err != nil {
		// pgx.ErrNoRows は translateError が ErrNotFound へ翻訳します。
		return translateError(op, err)
	}
	if owned {
		return fmt.Errorf("%s: %w", op, apperr.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", op, apperr.ErrPermissionDenied)
}

// translateError は pgx / PostgreSQL のエラーを、層をまたげる apperr に翻訳します。
// これにより上位層が pgx や SQLSTATE を知らずに済みます。
func translateError(op string, err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, apperr.ErrNotFound)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeForeignKeyViolation:
			// 参照先が存在しない。
			// ErrNotFound は固定文言で応答されるため、ここは詳細を残してよい。
			return fmt.Errorf("%s: 参照先が存在しません (%s): %w", op, pgErr.ConstraintName, apperr.ErrNotFound)
		case codeCheckViolation:
			// アプリ側の検証をすり抜けた場合の最後の砦。
			//
			// ハンドラは ErrInvalidArgument のメッセージをそのまま
			// クライアントへ返すため、ここに操作名や制約名を含めてはいけない。
			// 詳細はログにだけ残す。
			slog.Warn("check_constraint_violated",
				slog.String("op", op),
				slog.String("constraint", pgErr.ConstraintName),
			)
			return fmt.Errorf("入力値が制約を満たしていません: %w", apperr.ErrInvalidArgument)
		case codeSerializationFailure, codeDeadlockDetected:
			return fmt.Errorf("%s: 直列化に失敗しました (%s): %w", op, pgErr.Code, apperr.ErrConflict)
		case codeCharacterNotInRepertoire:
			// **文言に op を含めないこと** (codeCheckViolation と同じ理由)。
			// ErrInvalidArgument のメッセージはそのままクライアントへ返ります。
			slog.Warn("invalid_text_encoding",
				slog.String("op", op),
			)
			return fmt.Errorf("入力に使えない文字が含まれています: %w", apperr.ErrInvalidArgument)
		}
	}

	return fmt.Errorf("%s: %w", op, err)
}

// IsRetryable は、リトライすれば成功しうる一時的なエラーかを返します。
// SERIALIZABLE 分離レベルで動かす場合、直列化失敗は「異常」ではなく
// 想定内の事象なので、呼び出し側で再実行する必要があります。
func IsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == codeSerializationFailure || pgErr.Code == codeDeadlockDetected
	}
	return errors.Is(err, apperr.ErrConflict)
}

// codeQueryCanceled は文が打ち切られたことを表します。
// `statement_timeout` を超えた場合がこれになります。
const codeQueryCanceled = "57014"

// isStatementTimeout は、文が実行時間の上限で打ち切られたかを返します。
//
// 冪等キーの確保は、同じキーの処理が未コミットのとき**相手の終了を待ちます**。
// 待ちはコネクションを占有するため上限を設けており、
// 超えたことをここで見分けます (docs/adr/0015-idempotency.md)。
func isStatementTimeout(err error) bool {
	return sqlState(err) == codeQueryCanceled
}

// sqlState は PostgreSQL の SQLSTATE を取り出します。
// PostgreSQL 由来でないエラーでは空文字を返します。
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// commentSeqIndexSuffix は、レス番号の一意索引が名乗る名前の末尾です。
//
// **親の索引名 (comments_thread_id_seq_idx) では一致しません。**
// comments はパーティションテーブルなので、一意性は子の索引で検出され、
// SQLSTATE 23505 の ConstraintName には
// comments_p5_thread_id_seq_idx のような子の名前が入ります (実測)。
// 親と子の両方が同じ接尾辞で終わるように、
// マイグレーションで索引名を _idx に揃えてあります。
//
// この前提は session_repository_test.go と同じく実 DB のテストで検査します。
// PostgreSQL が子索引の命名規則を変えたら、そこで落ちます。
const commentSeqIndexSuffix = "_thread_id_seq_idx"

// isCommentSeqConflict は、レス番号の採番が衝突したかを返します。
//
// unique モード (ADR 0019 決定 2) は READ COMMITTED のまま 1 文で採番するため、
// 同時実行では必ずこの違反が返ります。**やり直せば MAX(seq) を読み直す**ので、
// 一意制約違反でありながらリトライして意味がある数少ない経路になります。
//
// 判定を索引名まで絞るのは、将来 comments に別の一意制約が増えたときに、
// 永久に成功しない違反をリトライし続けないためです。
func isCommentSeqConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == codeUniqueViolation &&
		strings.HasSuffix(pgErr.ConstraintName, commentSeqIndexSuffix)
}
