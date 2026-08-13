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
// **S3 を先に消し、DB を後で更新します。** アップロード (決定 3) とは
// 逆順です —— あちらは「記録の無いオブジェクト」を避けたい。
// こちらは既に記録がある状態からの削除なので、
// **先に DB を消すと、消し損ねたオブジェクトを追う手段が無くなります。**
// 逆順なら、S3 だけ消えて DB が残った場合は次の周回で拾い直せます
// (Delete は存在しないキーを成功として扱う)。
//
// 呼び出し側は「Total() が 0 になるまで繰り返す」形で使ってください。
func (i *ImageInteractor) Reclaim(
	ctx context.Context, grace time.Duration, batchSize int32,
) (ReclaimResult, error) {
	if grace <= 0 {
		grace = DefaultReclaimGrace
	}
	if batchSize <= 0 {
		batchSize = DefaultReclaimBatchSize
	}

	var result ReclaimResult
	err := i.images.WithinTx(ctx, func(tx repository.ImageRepository) error {
		targets, err := tx.ListReclaimable(ctx, grace, batchSize)
		if err != nil {
			return err
		}

		for _, img := range targets {
			// **S3 を先に。** 失敗したら DB は触らず、次の周回に回す。
			if err := i.storage.Delete(ctx, img.ObjectKey); err != nil {
				result.Failed++
				slog.WarnContext(ctx, "画像の実体を削除できませんでした (次の周回で再試行します)",
					slog.String("image_id", img.ID.String()),
					slog.String("status", string(img.Status)),
					slog.String("error", err.Error()),
				)
				continue
			}

			if img.Status == model.StatusDeleted {
				// **DB 行を残す。** 消したことだけ記録する。
				if err := tx.MarkReclaimed(ctx, img.ID); err != nil {
					return err
				}
				result.Marked++
				continue
			}

			// pending の孤児と committed の孤立は行ごと消す。
			if err := tx.Delete(ctx, img.ID); err != nil {
				return err
			}
			result.Deleted++
		}
		return nil
	})
	if err != nil {
		return ReclaimResult{}, fmt.Errorf("画像の回収に失敗しました: %w", err)
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
