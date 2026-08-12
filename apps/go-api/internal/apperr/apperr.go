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

	// ErrUnauthenticated は未ログイン、またはセッションが無効なことを表します
	// (HTTP 401)。
	//
	// 「誰か分からない」状態であり、ログインすれば解決します。
	// 「誰かは分かるが権限がない」(403) とは分けてください
	// —— フロントは 401 でログイン画面へ遷移します
	// (docs/adr/0013-http-defense.md)。
	ErrUnauthenticated = errors.New("認証が必要です")

	// ErrConflict は同時更新などで競合が起きたことを表します (HTTP 409)。
	// SERIALIZABLE 分離レベルで直列化失敗が起きた場合などに使います。
	ErrConflict = errors.New("競合が発生しました")

	// ErrFailedPrecondition は要求そのものは正しいが、
	// 現在の状態と噛み合わないことを表します (HTTP 422)。
	//
	// **同じ冪等キーで別の内容が送られた場合**に使います
	// (docs/adr/0015-idempotency.md / docs/adr/0013-http-defense.md 決定 3)。
	//
	// ErrInvalidArgument (400) と分けるのは、入力の形は正しいためです。
	// ErrConflict (409) と分けるのは、**再試行しても解決しない**ためです
	// —— クライアントはキーを作り直す必要があります。
	ErrFailedPrecondition = errors.New("要求が現在の状態と噛み合いません")

	// ErrUnavailable は依存する外部要素の設定や疎通が無く、
	// その経路だけが使えないことを表します (HTTP 503)。
	//
	// 認証プロバイダの設定が入っていない場合に使います。
	// 設定が無くても API 自体は起動します —— 掲示板の閲覧と匿名投稿は
	// 認証に依存しないため、全体を落とすほうが害が大きいからです。
	ErrUnavailable = errors.New("この機能は現在利用できません")
)
