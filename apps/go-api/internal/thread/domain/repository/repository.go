// Package repository はスレッドの永続化に対するインターフェースを定義します。
//
// インターフェースをドメイン側に置き、実装を infrastructure 側に置くことで、
// ユースケース層が PostgreSQL にも sqlc にも依存しなくなります
// (依存性逆転)。テストではフェイク実装を差し込むだけで済みます。
package repository

import (
	"context"

	"develop-experiments/apps/go-api/internal/pagination"
	"develop-experiments/apps/go-api/internal/thread/domain/model"
)

// ThreadRepository はスレッドの読み書きを担います。
type ThreadRepository interface {
	// ListSummaries はスレッドとコメント数の組を、新しい順に取得します。
	// コメント数の集計は 1 クエリで完結します。
	ListSummaries(ctx context.Context, page pagination.Page) ([]model.Summary, error)

	// SearchSummaries はタイトルの中間一致で絞り込んだうえで、
	// ListSummaries と同じ組を新しい順に取得します
	// (docs/adr/0012-search.md 決定 3: 関連度順にはしません)。
	//
	// **並び順が同じなので、カーソルの意味も同じ**です。
	// 絞り込みの有無でページ送りの扱いを変える必要はありません。
	SearchSummaries(
		ctx context.Context, query model.SearchQuery, page pagination.Page,
	) ([]model.Summary, error)

	// FindSummaryByID は 1 件のスレッドをコメント数つきで取得します。
	// 存在しない場合は apperr.ErrNotFound を返します。
	FindSummaryByID(ctx context.Context, id int64) (*model.Summary, error)

	// Create はスレッドを保存し、採番済みの値を返します。
	Create(ctx context.Context, thread *model.Thread) (*model.Thread, error)

	// Exists はスレッドが存在する (かつ削除されていない) かを返します。
	Exists(ctx context.Context, id int64) (bool, error)

	// SoftDeleteOwn は投稿者本人がスレッドを論理削除します
	// (docs/adr/0005-authentication.md の権限モデル / ADR 0003 未決 #7)。
	//
	// **消せるのは自分のスレッドだけです。** 匿名で立てられたスレッド
	// (author_id が NULL) は本人であることを示せないため消せません ——
	// 消せるのはモデレーターだけです (ADR 0011 決定 2)。
	//
	// 失敗の理由を撃ち分けます。
	//
	//	apperr.ErrNotFound         無い、または既に削除済み
	//	apperr.ErrPermissionDenied 他人のもの、または匿名投稿
	//
	// **403 に隠しません。** スレッドは誰でも読めるので存在は公開情報であり、
	// 404 にすると自分の投稿が消せないときに
	// 「消えたのか、権限が無いのか」を利用者が区別できません。
	SoftDeleteOwn(ctx context.Context, id, actorID int64) error
}

// BenchmarkRepository は Phase 4 のベンチマークで、
// 「N+1 クエリ + goroutine 並列集計」を再現するための実装用インターフェースです。
// 本番経路では使いません。
type BenchmarkRepository interface {
	// ListThreadsOnly はコメント数を含めずにスレッドだけを取得します。
	ListThreadsOnly(ctx context.Context, page pagination.Page) ([]model.Thread, error)

	// CountComments は 1 スレッド分のコメント数を数えます。
	// スレッド件数ぶん呼ばれることを前提とした、意図的な N+1 用メソッドです。
	CountComments(ctx context.Context, threadID int64) (int64, error)
}
