// Package usecase はスレッドに関するアプリケーションロジックを担います。
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/pagination"
	"develop-experiments/apps/go-api/internal/thread/domain/model"
	"develop-experiments/apps/go-api/internal/thread/domain/repository"
)

// AuthorDTO はフロントエンドに返す投稿者のデータ構造です。
// 内部 ID (users.id) は含めません。
type AuthorDTO struct {
	// PublicID は退会済みでは nil になります。
	PublicID    *uuid.UUID `json:"publicId"`
	DisplayName string     `json:"displayName"`
	AvatarURL   *string    `json:"avatarUrl"`
	Withdrawn   bool       `json:"withdrawn"`
}

// ThreadDTO はフロントエンドに返すスレッドのデータ構造です。
type ThreadDTO struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	CommentCount int64  `json:"commentCount"`
	// Author は匿名投稿では nil になります。
	Author *AuthorDTO `json:"author"`
	// Icon はスレッドアイコンです。設定されていなければ nil になります。
	Icon      *ImageDTO `json:"icon"`
	CreatedAt time.Time `json:"createdAt"`
}

// ImageDTO はスレッドアイコンです。URL を組み立てて返します
// (docs/adr/0007-image-storage.md 決定 5)。
type ImageDTO struct {
	ID     uuid.UUID `json:"id"`
	URL    string    `json:"url"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
}

// ThreadListResult はスレッド一覧と、次ページ取得用のカーソルです。
// NextCursor が nil の場合は、それ以上ページがないことを意味します。
//
// カーソルは不透明トークン (文字列) です。id をそのまま返さないのは、
// 並び順を追加したときにカーソルの中身が変わっても
// API の契約を壊さないためです (形式は ADR 0018)。
type ThreadListResult struct {
	Threads    []ThreadDTO `json:"threads"`
	NextCursor *string     `json:"nextCursor"`
}

// ThreadInteractor は「スレッドを取得・作成する」ユースケースを担当します。
type ThreadInteractor struct {
	repo repository.ThreadRepository
	// images はストレージの設定が無い環境では nil になります。
	images ImageResolver
}

// ImageResolver はアイコンの解決を担います。
//
// **image モジュールを import しません** (ADR 0004)。
// 利用側が必要な操作だけのインターフェースを定義する形は、
// comment モジュールと同じです。
type ImageResolver interface {
	// EnsureOwned は「その利用者が所有する確定済みの画像か」を確認します。
	EnsureOwned(ctx context.Context, ownerID int64, imageID uuid.UUID) error
	// URL はオブジェクトキーから配信用の絶対 URL を組み立てます。
	URL(objectKey string) string
}

// NewThreadInteractor はリポジトリを注入してインタラクターを生成します。
// images は nil を許します (ストレージの設定が無い環境)。
func NewThreadInteractor(
	repo repository.ThreadRepository, images ImageResolver,
) *ThreadInteractor {
	return &ThreadInteractor{repo: repo, images: images}
}

// FetchThreadList はスレッド一覧をコメント数つきで取得します。
//
// コメント数の集計は goroutine では並列化せず、
// LEFT JOIN + COUNT(*) FILTER の単一クエリで行います。
// スレッドごとに COUNT を投げる実装 (N+1) を goroutine で並列化しても、
// DB へのラウンドトリップ回数そのものは減らないため、
// 単一クエリのほうがほぼ常に速いからです。
//
// 「goroutine で並列集計」した版は FetchThreadListNPlusOne に残してあり、
// Phase 4 のベンチマークで両者を比較します。
func (i *ThreadInteractor) FetchThreadList(ctx context.Context, page pagination.Page) (ThreadListResult, error) {
	summaries, err := i.repo.ListSummaries(ctx, page)
	if err != nil {
		return ThreadListResult{}, err
	}
	return i.buildListResult(summaries, page.Size)
}

// FetchThread は 1 件のスレッドをコメント数つきで取得します。
// 存在しない場合は apperr.ErrNotFound を返します。
func (i *ThreadInteractor) FetchThread(ctx context.Context, id int64) (ThreadDTO, error) {
	summary, err := i.repo.FindSummaryByID(ctx, id)
	if err != nil {
		return ThreadDTO{}, err
	}
	return i.toDTO(*summary), nil
}

// CreateThread は新しいスレッドを作成します。
func (i *ThreadInteractor) CreateThread(
	ctx context.Context, title string, authorID *int64, iconImageID *uuid.UUID,
) (ThreadDTO, error) {
	thread, err := model.NewThread(title, authorID, iconImageID)
	if err != nil {
		return ThreadDTO{}, err
	}

	// **アイコンは投稿する前に確かめる。** 作ってから気づくと、
	// 「スレッドはできたがアイコンだけ付かない」状態になる。
	if iconImageID != nil {
		if i.images == nil {
			return ThreadDTO{}, fmt.Errorf("画像は現在利用できません: %w", apperr.ErrUnavailable)
		}
		if iconErr := i.images.EnsureOwned(ctx, *authorID, *iconImageID); iconErr != nil {
			return ThreadDTO{}, iconErr
		}
	}

	created, err := i.repo.Create(ctx, thread)
	if err != nil {
		return ThreadDTO{}, err
	}

	return i.toDTO(model.Summary{Thread: *created, CommentCount: 0}), nil
}

func (i *ThreadInteractor) toDTO(s model.Summary) ThreadDTO {
	return ThreadDTO{
		ID:           s.ID,
		Title:        s.Title,
		CommentCount: s.CommentCount,
		Author:       toAuthorDTO(s.Author),
		Icon:         i.toImageDTO(s.Icon),
		CreatedAt:    s.CreatedAt,
	}
}

// toAuthorDTO は投稿者を詰め替えます。匿名投稿では nil のまま返します。
//
// 退会済みの表示の差し替えはドメイン側 (model.NewAuthor) で済んでいます。
// ここで判定を足すと、同じ規則が 2 か所に散ります。
func toAuthorDTO(a *model.Author) *AuthorDTO {
	if a == nil {
		return nil
	}
	return &AuthorDTO{
		PublicID:    a.PublicID,
		DisplayName: a.DisplayName,
		AvatarURL:   a.AvatarURL,
		Withdrawn:   a.Withdrawn,
	}
}

// buildListResult は取得結果を DTO に詰め替え、次ページ用のカーソルを決めます。
// 次ページの有無の判定は pagination.NextToken にまとめてあります。
func (i *ThreadInteractor) buildListResult(summaries []model.Summary, size int32) (ThreadListResult, error) {
	dtos := make([]ThreadDTO, 0, len(summaries))
	for _, s := range summaries {
		dtos = append(dtos, i.toDTO(s))
	}

	var lastID int64
	if len(dtos) > 0 {
		lastID = dtos[len(dtos)-1].ID
	}

	next, err := pagination.NextToken(lastID, len(dtos), size)
	if err != nil {
		return ThreadListResult{}, err
	}

	return ThreadListResult{Threads: dtos, NextCursor: next}, nil
}

// toImageDTO はアイコンを詰め替えます。URL の組み立てはここで行います。
func (i *ThreadInteractor) toImageDTO(img *model.Image) *ImageDTO {
	if img == nil {
		return nil
	}
	// **resolver が無い環境でも一覧を壊さない。**
	// 設定を外した環境では URL を組み立てられないが、
	// 500 になるよりは URL が空のほうが被害が小さい。
	var url string
	if i.images != nil {
		url = i.images.URL(img.ObjectKey)
	}
	return &ImageDTO{ID: img.ID, URL: url, Width: img.Width, Height: img.Height}
}
