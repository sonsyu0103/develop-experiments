// Package apperr は層をまたいで使う番兵エラーを定義します。
//
// ハンドラ層は errors.Is でこれらを判定して HTTP ステータスに対応づけるため、
// ユースケース層やドメイン層は「HTTP を知らないまま」意味のあるエラーを返せます。
package apperr

import "errors"

var (
	// ErrNotFound は対象のリソースが存在しないことを表します (HTTP 404)。
	ErrNotFound = errors.New("リソースが見つかりません")

	// ErrInvalidArgument は入力値が不正であることを表します (HTTP 400)。
	ErrInvalidArgument = errors.New("入力が不正です")

	// ErrConflict は同時更新などで競合が起きたことを表します (HTTP 409)。
	// SERIALIZABLE 分離レベルで直列化失敗が起きた場合などに使います。
	ErrConflict = errors.New("競合が発生しました")
)
