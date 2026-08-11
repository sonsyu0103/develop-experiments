// Package usecase はスレッドに関するアプリケーションロジックを担います。
package usecase

import (
	"context"
	"time"

	"github.com/google/uuid"

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
	Author    *AuthorDTO `json:"author"`
	CreatedAt time.Time  `json:"createdAt"`
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
}

// NewThreadInteractor はリポジトリを注入してインタラクターを生成します。
func NewThreadInteractor(repo repository.ThreadRepository) *ThreadInteractor {
	return &ThreadInteractor{repo: repo}
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
	return buildListResult(summaries, page.Size)
}

// FetchThread は 1 件のスレッドをコメント数つきで取得します。
// 存在しない場合は apperr.ErrNotFound を返します。
func (i *ThreadInteractor) FetchThread(ctx context.Context, id int64) (ThreadDTO, error) {
	summary, err := i.repo.FindSummaryByID(ctx, id)
	if err != nil {
		return ThreadDTO{}, err
	}
	return toDTO(*summary), nil
}

// CreateThread は新しいスレッドを作成します。
func (i *ThreadInteractor) CreateThread(
	ctx context.Context, title string, authorID *int64,
) (ThreadDTO, error) {
	thread, err := model.NewThread(title, authorID)
	if err != nil {
		return ThreadDTO{}, err
	}

	created, err := i.repo.Create(ctx, thread)
	if err != nil {
		return ThreadDTO{}, err
	}

	return toDTO(model.Summary{Thread: *created, CommentCount: 0}), nil
}

func toDTO(s model.Summary) ThreadDTO {
	return ThreadDTO{
		ID:           s.ID,
		Title:        s.Title,
		CommentCount: s.CommentCount,
		Author:       toAuthorDTO(s.Author),
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
func buildListResult(summaries []model.Summary, size int32) (ThreadListResult, error) {
	dtos := make([]ThreadDTO, 0, len(summaries))
	for _, s := range summaries {
		dtos = append(dtos, toDTO(s))
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
