// Package repository はモデレーションの永続化の契約を定義します。
//
// 【削除の口をここで定義する理由】
// [ADR 0011](../../../../../../docs/adr/0011-moderation.md) のモジュール構成が
// 「削除操作は、各モジュールが公開する最小のインターフェースを moderation 側で
// 定義して受ける」と決めています。**依存の向きを moderation → 各モジュールに
// しないため**です。逆にすると thread / comment / image のすべてが
// moderation を知ることになり、横断的な関心事を切り出した意味が無くなります。
//
// そのため、この中に他モジュールの型は現れません —— 対象は
// int64 と uuid.UUID としてだけ渡します。
package repository

import (
	"context"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/moderation/domain/model"
)

// ThreadSoftDeleter はスレッドを論理削除します。
type ThreadSoftDeleter interface {
	// SoftDeleteThread はスレッドを論理削除します。
	//
	// **既に削除済みの場合は apperr.ErrNotFound です。**
	// 「今回この操作で消えた」ことを呼び出し側が判断できる必要があります ——
	// 成功として扱うと、実際には何も起きていない操作が
	// モデレーション記録に残ります。
	SoftDeleteThread(ctx context.Context, id int64) error
}

// CommentSoftDeleter はコメントを論理削除します。
type CommentSoftDeleter interface {
	// SoftDeleteComment はコメントを論理削除します。
	//
	// **threadID が要ります。** comments は thread_id による HASH
	// パーティションで、主キーが (thread_id, id) です。
	// コメント ID だけでは先頭列を絞れず、8 パーティションすべてを走査します。
	//
	// 既に削除済みの場合は apperr.ErrNotFound です。
	SoftDeleteComment(ctx context.Context, threadID, id int64) error
}

// ImageDeleter は画像を削除します。
type ImageDeleter interface {
	// MarkImageDeleted は images.status を 'deleted' にします。
	//
	// **ストレージの実体はここでは消えません。** 回収バッチが
	// 後から消します (ADR 0011 決定 5)。HTTP のリクエスト内で
	// S3 への削除を待たせないためです。
	//
	// 既に削除済み、または回収済みの場合は apperr.ErrNotFound です。
	MarkImageDeleted(ctx context.Context, id uuid.UUID) error
}

// ActionRecorder はモデレーション操作を記録します。
type ActionRecorder interface {
	// RecordAction は 1 件の操作を moderation_actions に書きます。
	//
	// **対象が実際に変化したことを確認してから呼びます。**
	// ADR 0010 が「ログは欠落しうる」と決めているため、
	// この記録が監査の正になります。
	RecordAction(ctx context.Context, a *model.Action) (*model.Action, error)
}

// Repository はモデレーション 1 操作ぶんに必要なものをまとめたものです。
//
// 【なぜ 1 つに束ねるのか】
// 削除と記録は**同じトランザクションで行う必要があります**。別々にすると
// 片方だけ成功した状態が作れ、「消えていないのに記録がある」または
// 「誰が消したか分からない」のどちらかになります。
// トランザクションを 1 つにするには、その中で使う口が同じ実体から
// 出ている必要があるため、ここで合成します。
//
// 上の 4 つを分けて定義しているのは、**利用者に見せる契約を最小にする**ためです。
// 記録だけを行う経路 (ロール変更) は ActionRecorder だけを受け取れます。
type Repository interface {
	ActionRecorder
	ThreadSoftDeleter
	CommentSoftDeleter
	ImageDeleter

	// WithinTx は 1 つのトランザクションの中でリポジトリを使います。
	//
	// **入れ子にはできません。** 渡される Repository で再度 WithinTx を
	// 呼ぶとエラーになります (image モジュールと同じ約束)。
	WithinTx(ctx context.Context, fn func(Repository) error) error
}
