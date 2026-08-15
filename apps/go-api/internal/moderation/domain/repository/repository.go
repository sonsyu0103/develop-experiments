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
	"develop-experiments/apps/go-api/internal/pagination"
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

// TargetExistenceChecker は通報の対象が生きているかを確かめます。
//
// **通報を積む前に確かめます** (ADR 0011 決定 4)。確かめないと、
// 存在しない ID の通報でキューを埋められます。
//
// thread / comment のどちらのモジュールにも属さないので、
// 削除の口と同じく moderation 側で定義します。
type TargetExistenceChecker interface {
	// ThreadExists はスレッドが生きているかを返します。
	ThreadExists(ctx context.Context, id int64) (bool, error)
	// CommentExists はコメントが生きているかを返します。
	//
	// **threadID が要ります。** 主キーが (thread_id, id) なので、
	// 無いと 8 パーティションすべてを走査します。
	CommentExists(ctx context.Context, threadID, id int64) (bool, error)
}

// ReportRepository は通報の読み書きです。
type ReportRepository interface {
	// Create は通報を 1 件積みます。
	//
	// **重複はエラーにしません** (ADR 0011 の引き受けるコスト)。
	// 同じ人が同じ対象を既に通報している場合は
	// created = false で、既存の通報を返します。
	// 呼び出し側は「既に通報済み」として正常に扱ってください。
	Create(ctx context.Context, r *model.Report) (report *model.Report, created bool, err error)

	// List は通報キューを古い順に返します。
	//
	// **id 昇順です。** created_at は一意ではないため、
	// キーセットの境界に使うと取りこぼしと重複が起きます。
	List(ctx context.Context, status model.ReportStatus, page pagination.Page) ([]model.Report, error)

	// Resolve は通報を処理済みにします。
	//
	// 既に処理済み、または存在しない場合は apperr.ErrNotFound です。
	// **投稿には触れません** —— 削除は別の操作になります。
	Resolve(ctx context.Context, id int64, status model.ReportStatus, actorID int64) (*model.Report, error)
}

// RoleChanger は利用者のロールを変更します。
//
// **user モジュールの型は現れません。** ロールは文字列として渡します ——
// 「ロールとは何か」は user モジュールの関心事で、
// こちらは記録と権限判定のためにその値を運ぶだけです。
type RoleChanger interface {
	// ChangeRole はロールを変更し、変更後の内部 ID を返します。
	//
	// 対象が存在しない、または退会済みの場合は apperr.ErrNotFound です。
	ChangeRole(ctx context.Context, publicID uuid.UUID, role string) (userID int64, err error)
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
// **ロール変更も同じ理由でここに入ります** (ADR 0011 決定 1
// 「変更は監査記録に残す」)。変更と記録が分かれると、
// 「権限が変わったのに誰がやったか分からない」状態が作れます。
//
// 分けて定義しているのは、**利用者に見せる契約を最小にする**ためです。
// 通報キューのように記録を伴わない経路は ReportRepository だけを受け取れます。
type Repository interface {
	ActionRecorder
	ThreadSoftDeleter
	CommentSoftDeleter
	ImageDeleter
	RoleChanger

	// WithinTx は 1 つのトランザクションの中でリポジトリを使います。
	//
	// **入れ子にはできません。** 渡される Repository で再度 WithinTx を
	// 呼ぶとエラーになります (image モジュールと同じ約束)。
	WithinTx(ctx context.Context, fn func(Repository) error) error
}
