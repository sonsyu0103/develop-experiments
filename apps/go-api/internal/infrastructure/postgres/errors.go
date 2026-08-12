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
)

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
			slog.Warn("DB の CHECK 制約に違反した (アプリ側の検証漏れの可能性)",
				slog.String("op", op),
				slog.String("constraint", pgErr.ConstraintName),
			)
			return fmt.Errorf("入力値が制約を満たしていません: %w", apperr.ErrInvalidArgument)
		case codeSerializationFailure, codeDeadlockDetected:
			return fmt.Errorf("%s: 直列化に失敗しました (%s): %w", op, pgErr.Code, apperr.ErrConflict)
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
