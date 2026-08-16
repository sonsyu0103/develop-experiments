// Package usecase は画像のユースケースを担当します。
//
// 判断の記録は docs/adr/0007-image-storage.md。
package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/image/domain/model"
	"develop-experiments/apps/go-api/internal/image/domain/repository"
)

// Clock は現在時刻を返します。テストで固定するために注入します。
type Clock func() time.Time

// ImageDTO はハンドラへ返す画像です。
//
// **URL を組み立てて返します** (ADR 0007 決定 5)。
// オブジェクトキーは外に出しません —— 出すと、フロントが
// CDN のベース URL を知る必要が出ます。
type ImageDTO struct {
	ID     uuid.UUID
	URL    string
	Width  int
	Height int
}

// MaxUploadBytes は受け取るバイト数の上限です。
//
// **HTTP 層はドメインを見ません** (ADR 0004 のレイヤ方向)。
// 本文を読み切る前に打ち切るために HTTP 層でも同じ値が要るので、
// ユースケース層から再輸出します。値の出どころは 1 か所のままです。
const MaxUploadBytes = model.MaxUploadBytes

// ParseKind は用途の文字列を解釈します。
//
// **マルチパートの本文は仕様書の検証を通りません** (kin-openapi は
// JSON スキーマとしてしか検証しない)。ここが唯一の入口になります。
//
// **3 つの用途をすべて受け付けます。**
// PR 3 の時点ではコメント添付だけに絞っていました ——
// 添付する経路 (どこから参照させるか) が無いうちに受け付けると、
// アップロードできるのにどこからも参照されない画像が作れるためです。
// プロフィール画像とスレッドアイコンの経路が入ったので開けました。
func ParseKind(raw string) (model.Kind, error) {
	if raw == "" {
		return "", fmt.Errorf("kind フィールドがありません: %w", apperr.ErrInvalidArgument)
	}
	kind := model.Kind(raw)
	if !kind.Valid() {
		return "", fmt.Errorf("kind が不正です (got %q): %w", raw, apperr.ErrInvalidArgument)
	}
	return kind, nil
}

// defaultMaxConcurrentDecodes は同時にデコードする本数の上限です。
//
// **画素数の上限を下げるだけでは足りません** (ADR 0007 決定 2 の改訂)。
// 2500 万画素は RGBA で 1 枚 95 MB を確保します。上限が無いと、
// 同時アップロード数にそのまま比例してメモリを食い、OOM で落ちます。
//
// 4 本なら最悪でも約 380 MB。ADR 0007 の「引き受けるコスト」の
// 1 つ目 (同時アップロード数に上限を設けないとメモリで落ちる) が、
// 数字を入れると具体的な破綻シナリオになるため、ここで閉じます。
const defaultMaxConcurrentDecodes = 4

// ImageInteractor は画像のアップロードを担当します。
type ImageInteractor struct {
	images  repository.ImageRepository
	storage repository.ObjectStorage
	now     Clock
	// decodeSlots は同時デコード数を制限するセマフォです。
	decodeSlots chan struct{}
}

// NewImageInteractor は依存を注入してインタラクターを生成します。
// now が nil の場合は time.Now を使います。
func NewImageInteractor(
	images repository.ImageRepository,
	storage repository.ObjectStorage,
	now Clock,
) *ImageInteractor {
	if now == nil {
		now = time.Now
	}
	return &ImageInteractor{
		images:      images,
		storage:     storage,
		now:         now,
		decodeSlots: make(chan struct{}, defaultMaxConcurrentDecodes),
	}
}

// Upload は画像を検証・再エンコードし、保存します。
//
// **DB を先に書き、後から確定させます** (ADR 0007 決定 3)。
//
//  1. images に status = 'pending' で INSERT   <- ここで object_key が決まる
//  2. ストレージへ PUT
//  3. images を status = 'committed' に UPDATE
//
// 2 か 3 で落ちると 'pending' の行が残ります。これは**追跡できる孤児**であり、
// 回収バッチが拾えます。逆順にすると DB に記録の無いオブジェクトが残り、
// 全件リストと突き合わせないと見つけられません。
func (i *ImageInteractor) Upload(
	ctx context.Context, ownerID int64, kindRaw string, raw []byte,
) (*ImageDTO, error) {
	// **用途の検証はここで行う。** HTTP 層にドメインの型を出さないため
	// (ADR 0004 のレイヤ方向)。
	kind, err := ParseKind(kindRaw)
	if err != nil {
		return nil, err
	}

	// **枠を握るのは再エンコードの間だけにする。**
	//
	// 初版は defer で関数の出口まで持っていたため、DB への書き込みと
	// ストレージへの PUT (ネットワーク往復) の間も枠を占有していた。
	// S3 の応答が 5 秒に伸びると 4 本の枠が張り付き、
	// **以降のアップロードがストレージの遅さでまとめて失敗する**
	// (待ち側には期限が無く、クライアントの切断でしか抜けない)。
	// 枠が守りたいのはメモリであって、経路全体の同時実行数ではない。
	out, err := i.decode(ctx, raw, kind)
	if err != nil {
		return nil, err
	}

	img, err := model.NewPending(ownerID, kind, out.Width, out.Height, int64(len(out.Bytes)))
	if err != nil {
		return nil, err
	}

	// 1. DB を先に書く。
	saved, err := i.images.CreatePending(ctx, img)
	if err != nil {
		return nil, err
	}

	// 2. ストレージへ。
	if putErr := i.storage.Put(ctx, saved.ObjectKey, saved.ContentType, out.Bytes); putErr != nil {
		// **pending の行は消しません。** 消すと、PUT が実は成功していた場合に
		// 「DB に記録の無いオブジェクト」が残ります (決定 3 が避けたかった形)。
		// 残しておけば回収バッチが両方を消せます。
		slog.ErrorContext(ctx, "image_store_failed",
			slog.String("image_id", saved.ID.String()),
			slog.String("error", putErr.Error()),
		)
		return nil, putErr
	}

	// 3. 確定させる。
	committed, err := i.images.Commit(ctx, saved.ID)
	if err != nil {
		// ここで落ちても、オブジェクトと pending 行の両方が残っている。
		// 回収バッチが対にして消せる状態なので、握りつぶさずに返す。
		slog.ErrorContext(ctx, "image_commit_failed",
			slog.String("image_id", saved.ID.String()),
			slog.String("error", err.Error()),
		)
		return nil, err
	}

	return i.toDTO(committed), nil
}

// decode は同時実行を絞りながら再エンコードします。
//
// メモリを確保するのはこの中だけなので、枠もここに閉じます。
//
// **ctx を尊重します。** 待っている間にクライアントが切断したら、
// そこで諦めるほうが行儀がよいためです。
func (i *ImageInteractor) decode(
	ctx context.Context, raw []byte, kind model.Kind,
) (*decoded, error) {
	select {
	case i.decodeSlots <- struct{}{}:
		defer func() { <-i.decodeSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("画像の処理を待っている間に中断されました: %w", ctx.Err())
	}
	return reencode(raw, kind)
}

// FindOwned は自分が所有する画像を取得します。
//
// **他人の画像は「見つからない」として扱います** (404)。
// 403 を返すと「その ID の画像が存在すること」自体が漏れます
// (docs/adr/0013-http-defense.md の「存在を隠すべき場合は 404 を返す」)。
//
// **確定していない画像も添付させません。** ストレージにバイト列が
// 無い可能性があるためです。
func (i *ImageInteractor) FindOwned(
	ctx context.Context, ownerID int64, id uuid.UUID,
) (*model.Image, error) {
	img, err := i.images.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if img.OwnerID != ownerID {
		return nil, fmt.Errorf("他人の画像です (id=%s): %w", id, apperr.ErrNotFound)
	}
	if img.Status != model.StatusCommitted {
		return nil, fmt.Errorf(
			"確定していない画像です (id=%s status=%s): %w", id, img.Status, apperr.ErrNotFound)
	}
	// **回収が始まった画像は添付させません。**
	//
	// 回収は「確保 (object_reclaimed_at を書く) -> S3 削除 -> 行削除」の順で進み、
	// 確保のあとは実体が消えている可能性があります。
	// ここで弾かないと、**実体の無い画像を投稿に添付できてしまいます**
	// (添付されると NOT EXISTS が偽になり、二度と回収されません)。
	if img.ObjectReclaimedAt != nil {
		return nil, fmt.Errorf(
			"回収済みの画像です (id=%s): %w", id, apperr.ErrNotFound)
	}
	return img, nil
}

// EnsureOwned は「その利用者が所有する、その用途の確定済み画像か」を確認します。
//
// **添付する側 (コメント / アバター / アイコン) がここを呼びます。**
// 各モジュールはこの型を知らず、自分で定義したインターフェース越しに
// 呼びます (ADR 0004 の ThreadExistenceChecker と同じ形)。
//
// **kind も確認します。** 用途によって保存する形式と寸法が変わるため
// (ADR 0007 決定 6)、コメント添付として上げた JPEG (長辺 1600) を
// アバターに使えると、仕様書の説明と実装が食い違います。
// 逆向き (アバター用の WebP をコメントに添付) も同じです。
//
// **404 として扱います。** 「その ID は存在するが用途が違う」と返すと、
// 他人の画像の存在を確かめる手段になります (ADR 0013)。
func (i *ImageInteractor) EnsureOwned(
	ctx context.Context, ownerID int64, imageID uuid.UUID, kind string,
) error {
	img, err := i.FindOwned(ctx, ownerID, imageID)
	if err != nil {
		return err
	}
	if string(img.Kind) != kind {
		return fmt.Errorf(
			"用途が違う画像です (id=%s kind=%s want=%s): %w",
			imageID, img.Kind, kind, apperr.ErrNotFound)
	}
	return nil
}

// URL はオブジェクトキーから配信用の絶対 URL を組み立てます。
//
// 添付先の詰め替えから使います。環境ごとの差はここで吸収されます
// (ADR 0007 決定 5)。
func (i *ImageInteractor) URL(objectKey string) string {
	return i.storage.URL(objectKey)
}

func (i *ImageInteractor) toDTO(img *model.Image) *ImageDTO {
	if img == nil {
		return nil
	}
	return &ImageDTO{
		ID:     img.ID,
		URL:    i.storage.URL(img.ObjectKey),
		Width:  img.Width,
		Height: img.Height,
	}
}
