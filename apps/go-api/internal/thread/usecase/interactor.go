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
	// ViewCount は閲覧数です。**正確な値ではありません**
	// (docs/adr/0006-view-count-and-popularity.md)。
	ViewCount int64 `json:"viewCount"`
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

// imageKindThreadIcon は EnsureOwned に渡す用途です。
//
// **image モジュールの定数を参照しません** (モジュールをまたがないため)。
// 値がずれると添付が常に 404 になるので、スモークが検出します。
const imageKindThreadIcon = "thread_icon"

// ImageResolver は画像の解決を担います。
//
// **image モジュールを import しません** (docs/adr/0004-modular-monolith.md)。
// 利用側が必要な操作だけのインターフェースを定義する形は、
// ThreadExistenceChecker と同じです。実装は image のユースケースが満たします。
type ImageResolver interface {
	// EnsureOwned は「その利用者が所有する確定済みの画像か」を確認します。
	// kind は用途 ("comment_attachment" / "avatar" / "thread_icon")。
	// **文字列で渡します。** image モジュールの型を知らないためです。
	EnsureOwned(ctx context.Context, ownerID int64, imageID uuid.UUID, kind string) error
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
//
// **rawQuery は正規化前の検索語です** (docs/adr/0012-search.md)。
// nil か、前後の空白を落とすと空になる文字列なら絞り込みません。
// 解釈をここで行うのは、タイトルを model.NewThread に渡しているのと同じ形で、
// HTTP 層にドメインの型を持ち込まないためです (ADR 0017 の層の境界)。
//
// 検索の有無で分かれるのは取得の 1 か所だけで、ページ送りの組み立て
// (buildListResult) から先は共通です —— 検索結果も新着順なので、
// カーソルの意味が変わらないためです (ADR 0012 決定 3)。
// **rawSort は正規化前の並び順です** (docs/adr/0006-view-count-and-popularity.md)。
// nil か空文字なら新着順になります。
//
// 検索と人気順は同時に指定できません。理由は仕様書の Sort パラメータに
// 書いてあるとおりで、索引をどちらか一方しか使えないためです。
// **黙って新着順に落とさず 400 にします** ——
// 「人気順で並べたつもりの新着順」は、利用者にもこちらにも見えません。
func (i *ThreadInteractor) FetchThreadList(
	ctx context.Context, page pagination.Page, rawQuery *string, rawSort *string,
) (ThreadListResult, error) {
	query, err := model.ParseSearchQuery(rawQuery)
	if err != nil {
		return ThreadListResult{}, err
	}
	order, err := model.ParseListOrder(rawSort)
	if err != nil {
		return ThreadListResult{}, err
	}
	if query != nil && order == model.ListOrderPopular {
		return ThreadListResult{}, fmt.Errorf(
			"検索と人気順は同時に指定できません: %w", apperr.ErrInvalidArgument)
	}

	// **カーソルの並び順が要求と一致しているかを検査する** (ADR 0018)。
	//
	// 検査しないと、新着順が発行したトークン (view_count なし) を
	// 人気順に渡したときに「閲覧数 0 の位置から」ページングが始まる。
	// 400 も出ず、黙って誤ったページが返る。
	//
	// **pagination には持ち込まない。** 並び順の語彙を知っているのは
	// この層で、下位層がソート種別の一覧を知る形にはしない。
	//
	// **先頭ページは検査しない。** カーソルが無いので発行元も無い。
	// ここを外すと、人気順の 1 ページ目が必ず 400 になる ——
	// 「先頭ページ」と「新着順のトークン」がどちらも空文字で、
	// 区別できないため (テストで実測した)。
	if page.Cursor != nil && page.CursorSort() != order.CursorSort() {
		return ThreadListResult{}, fmt.Errorf(
			"cursor は別の並び順で発行されたものです。先頭ページから取得し直してください: %w",
			apperr.ErrInvalidArgument)
	}

	var summaries []model.Summary
	switch {
	case query != nil:
		summaries, err = i.repo.SearchSummaries(ctx, *query, page)
	case order == model.ListOrderPopular:
		summaries, err = i.repo.ListPopularSummaries(ctx, page)
	default:
		summaries, err = i.repo.ListSummaries(ctx, page)
	}
	if err != nil {
		return ThreadListResult{}, err
	}
	return i.buildListResult(summaries, page.Size, order)
}

// FetchMyThreadList は 1 人が立てたスレッドの一覧を取得します
// (GET /me/threads)。
//
// 一覧の組み立て (buildListResult) は FetchThreadList と共用です ——
// 並び順が新着順で同じなので、カーソルの意味も変わりません。
//
// **検索も人気順も受け付けません。** 自分の投稿は件数が桁違いに少なく、
// 絞り込みや並べ替えの必要が薄いためです。必要になったら、
// そのときカーソルの並び順の検査ごと足します
// (FetchThreadList が既にその形を持っています)。
//
// **カーソルの並び順は検査しません。** 発行するのも受け取るのも
// 新着順のトークンだけで、取り違えようがないためです。
func (i *ThreadInteractor) FetchMyThreadList(
	ctx context.Context, authorID int64, page pagination.Page,
) (ThreadListResult, error) {
	summaries, err := i.repo.ListSummariesByAuthor(ctx, authorID, page)
	if err != nil {
		return ThreadListResult{}, err
	}
	return i.buildListResult(summaries, page.Size, model.ListOrderNew)
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
		if iconErr := i.images.EnsureOwned(ctx, *authorID, *iconImageID, imageKindThreadIcon); iconErr != nil {
			return ThreadDTO{}, iconErr
		}
	}

	created, err := i.repo.Create(ctx, thread)
	if err != nil {
		return ThreadDTO{}, err
	}

	return i.toDTO(model.Summary{Thread: *created, CommentCount: 0}), nil
}

// DeleteOwnThread は投稿者本人がスレッドを論理削除します
// (docs/adr/0005-authentication.md の権限モデル / ADR 0003 未決 #7)。
//
// **ログインが必須です。** 匿名で立てたスレッドを匿名のまま消す手段は
// ありません —— 投稿者を特定する情報が無く、本人であることを示せないためです
// (ADR 0005 決定 2)。荒らしへの対処はモデレーターが行います
// (ADR 0011 決定 2)。
//
// 判定はすべて永続化層の 1 文に寄せてあります。ここで「読んでから消す」と、
// 確認と削除の間にモデレーターの削除が入る窓ができます。
func (i *ThreadInteractor) DeleteOwnThread(ctx context.Context, id int64, actorID *int64) error {
	if actorID == nil {
		return fmt.Errorf("削除にはログインが必要です: %w", apperr.ErrUnauthenticated)
	}
	return i.repo.SoftDeleteOwn(ctx, id, *actorID)
}

func (i *ThreadInteractor) toDTO(s model.Summary) ThreadDTO {
	return ThreadDTO{
		ID:           s.ID,
		Title:        s.Title,
		CommentCount: s.CommentCount,
		ViewCount:    s.ViewCount,
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
// 次ページの有無の判定は pagination.NextTokenFor にまとめてあります。
//
// **カーソルの中身は並び順ごとに違います。** 人気順は
// (view_count, id) の複合キーになるため、閲覧数も埋めます ——
// id だけだと、同じ閲覧数の塊の途中で境界を作れません。
func (i *ThreadInteractor) buildListResult(
	summaries []model.Summary, size int32, order model.ListOrder,
) (ThreadListResult, error) {
	dtos := make([]ThreadDTO, 0, len(summaries))
	for _, s := range summaries {
		dtos = append(dtos, i.toDTO(s))
	}

	var last ThreadDTO
	if len(dtos) > 0 {
		last = dtos[len(dtos)-1]
	}

	cursor := pagination.NewCursor(last.ID).WithSort(order.CursorSort())
	if order == model.ListOrderPopular {
		cursor = cursor.WithViewCount(last.ViewCount)
	}

	next, err := pagination.NextTokenFor(cursor, len(dtos), size)
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
