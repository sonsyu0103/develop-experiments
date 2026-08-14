package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/image/domain/model"
	"develop-experiments/apps/go-api/internal/image/domain/repository"
	"develop-experiments/apps/go-api/internal/infrastructure/postgres/sqlcgen"
)

// ImageRepository は repository.ImageRepository の PostgreSQL 実装です。
type ImageRepository struct {
	q *sqlcgen.Queries
	// pool はトランザクションを開始するために持ちます。
	// **トランザクションの中で作った実体では nil になります**
	// (入れ子のトランザクションを作らせないため)。
	pool *pgxpool.Pool
}

var _ repository.ImageRepository = (*ImageRepository)(nil)

// NewImageRepository は接続プールからリポジトリを生成します。
func NewImageRepository(pool *pgxpool.Pool) *ImageRepository {
	return &ImageRepository{q: sqlcgen.New(pool), pool: pool}
}

// WithinTx は 1 つのトランザクションの中でリポジトリを使います。
//
// **回収バッチが要求します。** ListReclaimable が取る行ロック
// (FOR UPDATE SKIP LOCKED) はトランザクションの終わりまでしか持たないため、
// 取得と削除が別トランザクションだとロックの意味が無くなります。
func (r *ImageRepository) WithinTx(
	ctx context.Context, fn func(repository.ImageRepository) error,
) error {
	if r.pool == nil {
		return fmt.Errorf("ImageRepository.WithinTx: トランザクションの入れ子は作れません")
	}
	// READ COMMITTED でよい。守りたいのは「同じ行を 2 つのプロセスが
	// 処理しない」ことで、それは行ロックが担う。
	return runInTx(ctx, "ImageRepository.WithinTx", r.pool, pgx.ReadCommitted,
		func(_ pgx.Tx, q *sqlcgen.Queries) error {
			// pool を渡さない = この中でさらに WithinTx は呼べない。
			return fn(&ImageRepository{q: q})
		})
}

// ListReclaimable は回収対象の画像を取得します。
//
// **行ロックを取ります。** WithinTx の中で呼び、同じトランザクションで
// 後続の削除まで済ませてください。
func (r *ImageRepository) ListReclaimable(
	ctx context.Context, grace time.Duration, maxRows int32,
) ([]model.Image, error) {
	rows, err := r.q.ListReclaimableImages(ctx, sqlcgen.ListReclaimableImagesParams{
		Grace:   toInterval(grace),
		MaxRows: clampMaxRows(maxRows),
	})
	if err != nil {
		return nil, translateError("ImageRepository.ListReclaimable", err)
	}

	images := make([]model.Image, 0, len(rows))
	for _, row := range rows {
		images = append(images, *toImage(row))
	}
	return images, nil
}

// MarkReclaimed は S3 のオブジェクトを消したことを記録します。
func (r *ImageRepository) MarkReclaimed(ctx context.Context, id uuid.UUID) error {
	if _, err := r.q.MarkImageReclaimed(ctx, id); err != nil {
		return translateError("ImageRepository.MarkReclaimed", err)
	}
	// **0 行でもエラーにしません。** 別のプロセスが先に記録した場合が
	// これに当たります (行ロックで防いでいますが、周回をまたぐと起こりえます)。
	return nil
}

// Delete は DB 行ごと消します。
func (r *ImageRepository) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := r.q.DeleteImage(ctx, id); err != nil {
		return translateError("ImageRepository.Delete", err)
	}
	return nil
}

// CreatePending は status = 'pending' の行を作ります (ADR 0007 決定 3 の手順 1)。
func (r *ImageRepository) CreatePending(
	ctx context.Context, img *model.Image,
) (*model.Image, error) {
	width, err := toInt32("width", img.Width)
	if err != nil {
		return nil, err
	}
	height, err := toInt32("height", img.Height)
	if err != nil {
		return nil, err
	}

	row, err := r.q.CreatePendingImage(ctx, sqlcgen.CreatePendingImageParams{
		ID:          img.ID,
		OwnerID:     img.OwnerID,
		Kind:        string(img.Kind),
		ObjectKey:   img.ObjectKey,
		ContentType: img.ContentType,
		Width:       width,
		Height:      height,
		ByteSize:    img.ByteSize,
	})
	if err != nil {
		return nil, translateError("ImageRepository.CreatePending", err)
	}
	return toImage(row), nil
}

// Commit は status を 'committed' にします (ADR 0007 決定 3 の手順 3)。
//
// **0 行だった場合を「見つからない」で終わらせません。**
// クエリは status = 'pending' を条件に含めているため、0 行の意味は
// 「そんな画像は無い」と「既に pending ではない」の 2 通りあります。
// 後者はモデレーターが削除した ('deleted') 画像を再確定させようとした場合で、
// **これを成功させると消したはずのオブジェクトが S3 に残り続けます。**
func (r *ImageRepository) Commit(ctx context.Context, id uuid.UUID) (*model.Image, error) {
	row, err := r.q.CommitImage(ctx, id)
	if err == nil {
		return toImage(row), nil
	}

	translated := translateError("ImageRepository.Commit", err)
	if !errors.Is(translated, apperr.ErrNotFound) {
		return nil, translated
	}

	// 行そのものが無いのか、状態が違うのかを引き直して切り分ける。
	// **1 往復増えるのは失敗経路だけ**なので、通常の流れは変わらない。
	current, findErr := r.FindByID(ctx, id)
	if findErr != nil {
		// **引けなかった理由を潰さない。**
		// ここで一律に translated (= ErrNotFound) を返すと、接続断や
		// タイムアウトが 404「対象のリソースが見つかりません」に化ける。
		// 実際には画像は存在し、pending のまま孤児として残っている ——
		// 利用者にもログにも「無かった」としか残らないのが一番困る。
		if !errors.Is(findErr, apperr.ErrNotFound) {
			return nil, findErr
		}
		// 本当に無い。元のエラーの意味をそのまま返す。
		return nil, translated
	}
	return nil, fmt.Errorf(
		"確定できない状態の画像です (id=%s status=%s): %w",
		id, current.Status, apperr.ErrFailedPrecondition)
}

// FindByID は 1 件取得します。
func (r *ImageRepository) FindByID(ctx context.Context, id uuid.UUID) (*model.Image, error) {
	row, err := r.q.GetImageByID(ctx, id)
	if err != nil {
		return nil, translateError("ImageRepository.FindByID", err)
	}
	return toImage(row), nil
}

// toImage は生成された行をドメインモデルへ詰め替えます。
//
// kind と status を検証しません。**DB の CHECK 制約が既に絞っている**ため、
// ここで弾くと「保存できたのに読めない」状態が作れてしまいます。
// (ロールのように「読めない値を安全側へ倒す」必要がある列とは事情が違います
// —— あちらは権限が広がる方向の危険があります。)
func toImage(row sqlcgen.Image) *model.Image {
	return model.Reconstruct(
		row.ID,
		row.OwnerID,
		model.Kind(row.Kind),
		row.ObjectKey,
		row.ContentType,
		int(row.Width),
		int(row.Height),
		row.ByteSize,
		model.Status(row.Status),
		row.CreatedAt,
		row.CommittedAt,
		row.ObjectReclaimedAt,
		row.AttachedAt,
	)
}

// toInt32 は寸法を DB の INT へ収めます。
//
// **範囲外を黙って切り詰めません。** int32 を超える寸法は
// 画素数の上限 (MaxPixels) で先に弾かれるはずなので、ここに来るのは
// 検証の順序が壊れたときだけになります。切り詰めると
// 「保存はできたが寸法が別の値」という追いにくい形で残ります。
func toInt32(name string, v int) (int32, error) {
	if v < 0 || v > math.MaxInt32 {
		return 0, fmt.Errorf("画像の %s が範囲外です (got %d): %w", name, v, apperr.ErrInvalidArgument)
	}
	return int32(v), nil
}
