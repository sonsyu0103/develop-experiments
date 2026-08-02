package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"develop-experiments/apps/go-api/internal/apperr"
)

// PostgreSQL の SQLSTATE。
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	// codeForeignKeyViolation は外部キー違反 (親行が存在しない) です。
	codeForeignKeyViolation = "23503"
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
			// 親スレッドが存在しない、または削除済み。
			return fmt.Errorf("%s: 参照先が存在しません (%s): %w", op, pgErr.ConstraintName, apperr.ErrNotFound)
		case codeCheckViolation:
			// アプリ側の検証をすり抜けた場合の最後の砦。
			return fmt.Errorf("%s: 制約違反 (%s): %w", op, pgErr.ConstraintName, apperr.ErrInvalidArgument)
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
