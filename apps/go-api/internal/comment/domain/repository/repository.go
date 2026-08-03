// Package repository はコメントの永続化に対するインターフェースを定義します。
package repository

import (
	"context"

	"develop-experiments/apps/go-api/internal/comment/domain/model"
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

	// SoftDelete はコメントを論理削除します。
	// 対象が存在しない (すでに削除済みを含む) 場合は apperr.ErrNotFound を返します。
	//
	// 【現時点で HTTP エンドポイントからは呼ばれていません】
	// 削除 API を公開するかは未決です (docs/adr/0003-open-questions.md の項目 7)。
	// 認証が無い現状で公開すると誰でも他人のコメントを消せてしまうため、
	// 認証の方針 (同 項目 6) とセットで決める必要があります。
	// 決まるまでは永続化層のみを用意した状態で保留します。
	SoftDelete(ctx context.Context, threadID, id int64) error
}
