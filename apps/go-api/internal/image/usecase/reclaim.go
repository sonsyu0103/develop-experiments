package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"develop-experiments/apps/go-api/internal/image/domain/model"
	"develop-experiments/apps/go-api/internal/image/domain/repository"
)

// 回収バッチの既定値。
const (
	// DefaultReclaimGrace は「作られてからこれだけ経ったもの」を対象にする閾値です。
	//
	// **アップロード直後の画像を消してはいけません。**
	// アップロードと添付は別の操作なので (ADR 0007 の実装して分かったこと 1)、
	// 添付するまでの間、参照されていない committed は正常な状態です。
	// 利用者が投稿フォームを開いたまま席を立つ余地も見ておきます。
	DefaultReclaimGrace = 1 * time.Hour

	// DefaultReclaimBatchSize は 1 周回で扱う件数です。
	//
	// **行ロックを保持したままの時間を短く保つため**に絞ります。
	// S3 への DELETE を含むので、1 件あたりの時間が読めません。
	DefaultReclaimBatchSize int32 = 100
)

// ReclaimResult は 1 周回の結果です。
type ReclaimResult struct {
	// Deleted は DB 行ごと消した件数です ('pending' の孤児と committed の孤立)。
	Deleted int
	// Marked は S3 だけ消して DB 行を残した件数です ('deleted')。
	Marked int
	// Failed は S3 の削除に失敗した件数です。次の周回で再試行されます。
	Failed int
}

// Total は扱った件数です。0 なら回収するものが無かったことを意味します。
func (r ReclaimResult) Total() int { return r.Deleted + r.Marked + r.Failed }

// Reclaim は孤児になった画像を 1 周回ぶん回収します。
//
// **status によって扱いが分かれます** (docs/adr/0016-schema-and-indexes.md 問題 3)。
//
//	pending の孤児    DB 行も S3 オブジェクトも消す
//	committed の孤立  同上 (アップロードしたが添付されなかった)
//	deleted           S3 だけ消し、**DB 行は残す**
//
// `deleted` の行を残すのは、投稿から参照されているためです。
// 消すと外部キー違反になり、かつ「画像は削除されました」と
// 「元から画像なし」を区別できなくなります。
//
// **3 段階に分けます。**
//
//  1. 確保        object_reclaimed_at を書く (トランザクション内)
//  2. S3 削除      トランザクションの外
//  3. DB 行の削除  1 件ずつ独立したトランザクション
//
// 1 を先に置くのは、**確保のあと実体が消えている画像を添付させない**ため。
// FindOwned は確保済みの画像を返しません。
//
// S3 の削除をトランザクションの外へ出すのは、100 件ぶんの往復のあいだ
// 行ロックと接続を占有しないためです。
//
// 呼び出し側は「Deleted + Marked が 0 になるまで繰り返す」形で使ってください
// (Failed は繰り返しても減らないので、終了条件に含めてはいけません)。
func (i *ImageInteractor) Reclaim(
	ctx context.Context, grace time.Duration, batchSize int32,
) (ReclaimResult, error) {
	if grace <= 0 {
		grace = DefaultReclaimGrace
	}
	if batchSize <= 0 {
		batchSize = DefaultReclaimBatchSize
	}

	// 1. **回収する行を「確保」する。** ここだけがトランザクション。
	//
	// object_reclaimed_at を先に書くことに 2 つの意味がある。
	//
	//   a. **添付できなくする。** FindOwned は確保済みの画像を返さないので、
	//      「実体を消したのに、まだ添付できる」窓が閉じる。
	//      初版は S3 の削除をトランザクションの中で行っており、
	//      途中で DB がエラーになると**消えたオブジェクトの行が復活**し、
	//      次の周回まで利用者が添付できてしまった (レビュー指摘)。
	//   b. **S3 の往復でロックを持たない。** 100 件ぶんの DELETE を
	//      トランザクション内で直列に投げると、行ロックと接続を
	//      S3 のレイテンシぶん占有する。
	var targets []model.Image
	if err := i.images.WithinTx(ctx, func(tx repository.ImageRepository) error {
		found, listErr := tx.ListReclaimable(ctx, grace, batchSize)
		if listErr != nil {
			return listErr
		}
		for _, img := range found {
			if markErr := tx.MarkReclaimed(ctx, img.ID); markErr != nil {
				return markErr
			}
		}
		targets = found
		return nil
	}); err != nil {
		return ReclaimResult{}, fmt.Errorf("画像の回収対象を確保できませんでした: %w", err)
	}

	var result ReclaimResult

	// 2. **トランザクションの外で S3 を消す。**
	for _, img := range targets {
		if err := i.storage.Delete(ctx, img.ObjectKey); err != nil {
			result.Failed++
			// **確保したまま残る。** 実体が残り、行も残る。
			// 次の周回では拾われないので、放置すると容量を食う ——
			// ログに ERROR で出し、運用で気づけるようにする
			// (ここで確保を戻すと、恒久的に消せないオブジェクトが
			//  先頭に詰まって後続を止める)。
			slog.ErrorContext(ctx, "image_reclaim_storage_failed",
				slog.String("image_id", img.ID.String()),
				slog.String("object_key", img.ObjectKey),
				slog.String("status", string(img.Status)),
				slog.String("error", err.Error()),
			)
			continue
		}

		if img.Status == model.StatusDeleted {
			// **DB 行を残す** (ADR 0016 問題 3)。確保の記録がそのまま結果になる。
			result.Marked++
			continue
		}

		// 3. pending の孤児と committed の孤立は行ごと消す。
		//
		// **1 件ずつ独立したトランザクションで消す。** まとめると、
		// 途中の失敗で「S3 は消えたのに行が残る」件数が増える
		// (残った行は status が committed のままなので、
		//  再確保もされず実体だけ無い状態になる)。
		if err := i.images.Delete(ctx, img.ID); err != nil {
			result.Failed++
			slog.ErrorContext(ctx, "image_reclaim_row_delete_failed",
				slog.String("image_id", img.ID.String()),
				slog.String("error", err.Error()),
			)
			continue
		}
		result.Deleted++
	}

	if result.Total() > 0 {
		slog.InfoContext(ctx, "image_reclaim",
			slog.Int("deleted", result.Deleted),
			slog.Int("marked", result.Marked),
			slog.Int("failed", result.Failed),
		)
	}
	return result, nil
}
