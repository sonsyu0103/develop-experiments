// Package repository は画像の永続化とストレージの契約を定義します。
//
// **S3 SDK の型は一切現れません** (docs/adr/0007-image-storage.md のモジュール構成)。
// 実装は internal/infrastructure/objectstorage と
// internal/infrastructure/postgres に置き、こちら側は
// 「オブジェクトを置く / 消す」「URL を組む」だけを知ります。
package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/image/domain/model"
)

// ImageRepository は images テーブルへの操作です。
type ImageRepository interface {
	// CreatePending は status = 'pending' の行を作ります。
	//
	// **ストレージへ書く前に呼びます** (ADR 0007 決定 3)。
	// 逆順にすると、DB に記録の無いオブジェクトが残り、
	// 全件リストと突き合わせないと見つけられなくなります。
	CreatePending(ctx context.Context, img *model.Image) (*model.Image, error)

	// Commit は status を 'committed' にします。
	//
	// ストレージへの PUT が成功したあとに呼びます。
	// ここで落ちると 'pending' の行が残りますが、それは**追跡できる孤児**であり、
	// 回収バッチが拾えます。
	Commit(ctx context.Context, id uuid.UUID) (*model.Image, error)

	// ListReclaimable は回収対象の画像を取得します。
	//
	// **行ロックを取ります** (FOR UPDATE SKIP LOCKED)。定期処理を
	// API プロセス内で動かすため、レプリカの数だけ同時に走ります
	// (docs/adr/0003-open-questions.md 未決 #9)。
	// そのため、**トランザクションの中で呼び、同じトランザクションで
	// 後続の削除まで済ませる**必要があります。
	//
	// grace より新しい画像は返しません。アップロードしてから添付するまでの
	// 数十秒のあいだ、参照されていない committed は正常な状態だからです。
	ListReclaimable(ctx context.Context, grace time.Duration, maxRows int32) ([]model.Image, error)

	// MarkReclaimed は回収対象を「確保」します。
	//
	// **status によらず、拾ったすべての行に対して呼びます。**
	// 名前は「S3 のオブジェクトを消した記録」ですが、
	// 実際には**消す前に**書きます。理由が 2 つあります。
	//
	//  1. **添付できなくする。** FindOwned は確保済みの画像を返しません。
	//     S3 を消したあとに書くと、「実体は消えたのにまだ添付できる」
	//     窓が空きます (レビュー指摘で実際に踏んだ形)
	//  2. **対象から外す。** 'deleted' は DB 行を残すので (ADR 0016 問題 3)、
	//     ここに記録が無いと次の周回でも同じ行が拾われ続けます
	//
	// **'deleted' 専用だと読まないでください。** 'pending' と 'committed' で
	// 呼ばなくすると、1 の窓が開き、同じ行を毎周回で拾い直します
	// (変異 ② が検出します)。
	MarkReclaimed(ctx context.Context, id uuid.UUID) error

	// Delete は DB 行ごと消します。
	// **参照されていない画像にだけ使います** ('pending' の孤児と committed の孤立)。
	Delete(ctx context.Context, id uuid.UUID) error

	// WithinTx は 1 つのトランザクションの中でリポジトリを使います。
	//
	// 回収バッチが要求します —— ListReclaimable が取った行ロックは
	// トランザクションの終わりまでしか持たないため、
	// 取得と削除が別トランザクションだと**ロックの意味が無くなります**。
	WithinTx(ctx context.Context, fn func(ImageRepository) error) error

	// FindByID は 1 件取得します。見つからない場合は apperr.ErrNotFound です。
	//
	// **'pending' の行も返します。** 「まだ確定していない」ことを
	// 呼び出し側が判断できる必要があるためです ——
	// ここで隠すと、確定に失敗した画像が「存在しない」と区別できなくなります。
	FindByID(ctx context.Context, id uuid.UUID) (*model.Image, error)
}

// ObjectStorage はバイト列を置く先です。S3 と MinIO の差はこの背後に閉じます。
//
// **署名付き URL を発行するメソッドを置いていません。**
// 現在は公開読み取りで URL が不変なため不要であり (ADR 0007 決定 4)、
// 先に口を作ると「使われないまま実装だけが残る」ことになります。
// 移行条件は ADR 0007 決定 1 に書いてあります。
type ObjectStorage interface {
	// Put はオブジェクトを書き込みます。
	//
	// contentType は**こちらが再エンコードして決めた値**を渡します
	// (利用者の申告ではありません)。
	Put(ctx context.Context, key, contentType string, body []byte) error

	// Delete はオブジェクトを消します。
	//
	// **存在しないキーでも成功として扱います。** 回収バッチが
	// 「消したはずだが DB 行だけ残った」状態から再実行されるため、
	// ここで失敗にすると回収が永久に進まなくなります。
	Delete(ctx context.Context, key string) error

	// URL は配信用の絶対 URL を返します。
	//
	// **API は絶対 URL を返します** (ADR 0007 決定 5)。
	// キーだけを返すとフロントが CDN のベース URL を知る必要があり、
	// 組み立てロジックが Server Component と Client Component に散ります。
	// 環境ごとの差 (MinIO と CloudFront) はここで吸収します。
	URL(key string) string
}
