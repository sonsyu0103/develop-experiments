// Package repository はコメントの永続化に対するインターフェースを定義します。
package repository

import (
	"context"

	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/idempotency"
	"develop-experiments/apps/go-api/internal/pagination"
)

// ThreadExistenceChecker は親スレッドが生存しているかだけを確認します。
//
// コメント側からスレッドのリポジトリ全体に依存すると結合が強くなるため、
// 必要な 1 メソッドだけを切り出したインターフェースを受け取ります
// (インターフェース分離の原則)。
// スレッド側の実装がそのまま満たします。
type ThreadExistenceChecker interface {
	// Exists はスレッドが存在し、かつ論理削除されていないかを返します。
	Exists(ctx context.Context, threadID int64) (bool, error)
}

// CommentRepository はコメントの読み書きを担います。
type CommentRepository interface {
	// ListByThreadID は 1 スレッドのコメントを新しい順に取得します。
	// thread_id を等値で指定するため、HASH パーティションの pruning が効きます。
	ListByThreadID(ctx context.Context, threadID int64, page pagination.Page) ([]model.Comment, error)

	// Create はコメントを保存し、採番済みの値を返します。
	// 親スレッドが存在しない、または論理削除済みの場合は
	// apperr.ErrNotFound を返します (判定は SQL 側で完結します)。
	Create(ctx context.Context, comment *model.Comment) (*model.Comment, error)

	// CreateIdempotent は冪等キーつきで保存します。
	//
	// **キーの確保・投稿・応答の記録を 1 トランザクションで行います**
	// (docs/adr/0015-idempotency.md 決定 3)。別トランザクションにすると、
	// 「キーを記録した直後に処理が失敗」したときにリトライしても
	// 「処理済み」と誤判定され、投稿が永久に失われます。
	//
	// 戻り値は排他的です。
	//   - 初回: created が埋まり、replayed は nil
	//   - 再送: created は nil で、replayed に記録済みの応答本文が入る
	//
	// encode は初回にだけ呼ばれ、その戻り値が記録されます。
	// 永続化層に API の表現を持ち込まないための受け口です。
	//
	// 同じキーで別の内容が送られた場合は apperr.ErrFailedPrecondition (422)、
	// 待ちが上限を超えた場合は apperr.ErrConflict (409) を返します。
	CreateIdempotent(
		ctx context.Context, comment *model.Comment, req idempotency.Request,
		encode func(*model.Comment) ([]byte, error),
	) (created *model.Comment, replayed []byte, err error)

	// SoftDelete はコメントを論理削除します。
	// 対象が存在しない (すでに削除済みを含む) 場合は apperr.ErrNotFound を返します。
	//
	// **投稿者を見ません。** モデレーターの削除 (ADR 0011 決定 2) が
	// この形を必要とします —— 匿名投稿も消せる必要があるためです。
	// 本人による削除には SoftDeleteOwn を使ってください。
	SoftDelete(ctx context.Context, threadID, id int64) error

	// SoftDeleteOwn は投稿者本人がコメントを論理削除します
	// (docs/adr/0005-authentication.md の権限モデル)。
	//
	// **ADR 0003 の未決 #7 (コメント削除 API を公開するか) は、
	// 認証が入ったこの時点で解けます。** 未決だったのは
	// 「認証が無い状態で公開すると誰でも他人のコメントを消せる」ためでした。
	//
	// threadID はパーティションキーです。省くと 8 パーティションすべてを
	// 走査します (主キーが (thread_id, id) のため)。
	//
	// 失敗の理由の撃ち分けは ThreadRepository.SoftDeleteOwn と同じです。
	SoftDeleteOwn(ctx context.Context, threadID, id, actorID int64) error
}
