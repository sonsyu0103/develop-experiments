// Package usecase はコメントに関するアプリケーションロジックを担います。
package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/comment/domain/repository"
	"develop-experiments/apps/go-api/internal/idempotency"
	"develop-experiments/apps/go-api/internal/pagination"
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

// CommentDTO はフロントエンドに返すコメントのデータ構造です。
type CommentDTO struct {
	ID       int64 `json:"id"`
	ThreadID int64 `json:"threadId"`
	// Seq はスレッド内のレス番号 (`>>1` の 1) です。
	// 削除しても詰めないため、欠番が出ます
	// (docs/adr/0019-comment-concurrency.md 決定 5)。
	Seq int32 `json:"seq"`
	// AuthorName は匿名投稿の表示名です。
	// Author があるときは、そちらを表示してください。
	AuthorName string `json:"authorName"`
	// Author は匿名投稿では nil になります。
	Author *AuthorDTO `json:"author"`
	// Image は添付画像です。画像がなければ nil になります。
	Image     *ImageDTO `json:"image"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// ImageDTO は添付画像です。
//
// **URL を組み立てて返します** (docs/adr/0007-image-storage.md 決定 5)。
// オブジェクトキーは外に出しません。
type ImageDTO struct {
	ID     uuid.UUID `json:"id"`
	URL    string    `json:"url"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
}

// imageKindCommentAttachment は EnsureOwned に渡す用途です。
//
// **image モジュールの定数を参照しません** (モジュールをまたがないため)。
// 値がずれると添付が常に 404 になるので、スモークが検出します。
const imageKindCommentAttachment = "comment_attachment"

// ImageResolver は画像の解決を担います。
//
// **image モジュールを import しません** (docs/adr/0004-modular-monolith.md)。
// 利用側が必要な操作だけのインターフェースを定義する形は、
// ThreadExistenceChecker と同じです。実装は image のユースケースが満たします。
type ImageResolver interface {
	// EnsureOwned は「その利用者が所有する確定済みの画像か」を確認します。
	//
	// 他人の画像・存在しない画像・未確定の画像は
	// いずれも apperr.ErrNotFound になります (存在を隠すため)。
	// kind は用途 ("comment_attachment" / "avatar" / "thread_icon")。
	// **文字列で渡します。** image モジュールの型を知らないためです。
	EnsureOwned(ctx context.Context, ownerID int64, imageID uuid.UUID, kind string) error

	// URL はオブジェクトキーから配信用の絶対 URL を組み立てます。
	URL(objectKey string) string
}

// CommentListResult はコメント一覧と、次ページ取得用のカーソルです。
// カーソルは不透明トークン (文字列) です。形式は ADR 0018 を参照してください。
type CommentListResult struct {
	Comments   []CommentDTO `json:"comments"`
	NextCursor *string      `json:"nextCursor"`
}

// CommentInteractor は「コメントを取得・投稿する」ユースケースを担当します。
type CommentInteractor struct {
	repo    repository.CommentRepository
	threads repository.ThreadExistenceChecker
	// images はストレージの設定が無い環境では nil になります。
	// そのとき画像を指定した投稿だけが 503 になります。
	images ImageResolver
}

// NewCommentInteractor はリポジトリを注入してインタラクターを生成します。
//
// images は nil を許します (ストレージの設定が無い環境)。
// **投稿系の主経路を画像に依存させないため**で、認証と同じ考え方です
// (docs/adr/0005-authentication.md 決定 4)。
func NewCommentInteractor(
	repo repository.CommentRepository,
	threads repository.ThreadExistenceChecker,
	images ImageResolver,
) *CommentInteractor {
	return &CommentInteractor{repo: repo, threads: threads, images: images}
}

// FetchComments は 1 スレッドのコメントを新しい順に取得します。
// スレッドが存在しない、または論理削除済みの場合は apperr.ErrNotFound を返します。
//
// 存在確認を挟まないと、存在しないスレッドに対して
// 「コメント 0 件」という誤った 200 を返してしまいます。
// また論理削除済みスレッドの中身が読めてしまい、
// 「削除したはずのものが API から見える」状態になります。
//
// 存在確認と取得の間にスレッドが削除される競合は残りますが、
// その場合に起きるのは「削除直後のコメントが 1 回だけ返る」ことだけで、
// 不正な書き込みは生じません。投稿側 (PostComment) は書き込みを伴うため
// SQL 側で 1 文に閉じており、こちらとは要求される厳密さが異なります。
// 1 クエリ化できない理由は db/query/comments.sql のコメントを参照してください。
func (i *CommentInteractor) FetchComments(
	ctx context.Context, threadID int64, page pagination.Page,
) (CommentListResult, error) {
	ok, err := i.threads.Exists(ctx, threadID)
	if err != nil {
		return CommentListResult{}, err
	}
	if !ok {
		return CommentListResult{}, fmt.Errorf("スレッド %d: %w", threadID, apperr.ErrNotFound)
	}

	comments, err := i.repo.ListByThreadID(ctx, threadID, page)
	if err != nil {
		return CommentListResult{}, err
	}

	dtos := make([]CommentDTO, 0, len(comments))
	for _, c := range comments {
		dtos = append(dtos, i.toDTO(c))
	}

	var lastID int64
	if len(dtos) > 0 {
		lastID = dtos[len(dtos)-1].ID
	}

	next, err := pagination.NextToken(lastID, len(dtos), page.Size)
	if err != nil {
		return CommentListResult{}, err
	}

	return CommentListResult{Comments: dtos, NextCursor: next}, nil
}

// PostComment はコメントを投稿します。
// 親スレッドが存在しない、または論理削除済みの場合は apperr.ErrNotFound を返します。
//
// ここで事前に存在確認をしないのは、確認と挿入の間にスレッドが
// 削除される競合を避けるためです。判定は INSERT ... WHERE EXISTS で
// SQL 側に寄せてあります。
func (i *CommentInteractor) PostComment(
	ctx context.Context, threadID int64, authorName, body string,
	authorID *int64, imageID *uuid.UUID,
) (CommentDTO, error) {
	comment, err := model.NewComment(threadID, authorName, body, authorID, imageID)
	if err != nil {
		return CommentDTO{}, err
	}

	if imgErr := i.ensureImageUsable(ctx, authorID, imageID); imgErr != nil {
		return CommentDTO{}, imgErr
	}

	created, err := i.repo.Create(ctx, comment)
	if err != nil {
		return CommentDTO{}, err
	}

	return i.toDTO(*created), nil
}

// PostCommentIdempotent は冪等キーつきで投稿します。
//
// 同じキーで再送された場合は**投稿せずに**前回の応答を返します
// (docs/adr/0015-idempotency.md)。
//
// **匿名では冪等キーを使えません。** キーの名前空間を分ける手段が無く、
// IP で分けると NAT の背後で他人のキーと衝突するためです (同 決定 4)。
// 呼び出し側 (HTTP 層) が匿名のヘッダを落とす前提ですが、
// 他人の結果を返す事故に直結するのでここでも弾きます。
func (i *CommentInteractor) PostCommentIdempotent(
	ctx context.Context, threadID int64, authorName, body string, authorID *int64,
	imageID *uuid.UUID, key, endpoint string,
) (CommentDTO, error) {
	if authorID == nil {
		return CommentDTO{}, fmt.Errorf(
			"匿名では Idempotency-Key を使えません: %w", apperr.ErrInvalidArgument)
	}

	comment, err := model.NewComment(threadID, authorName, body, authorID, imageID)
	if err != nil {
		return CommentDTO{}, err
	}

	if imgErr := i.ensureImageUsable(ctx, authorID, imageID); imgErr != nil {
		return CommentDTO{}, imgErr
	}

	// **指紋は正規化したあとの値から作ります。**
	//
	// 受け取ったままの値を使うと、結果に影響しない差で 422 になります。
	//   - authorName はログイン中には捨てられる (NewComment)。
	//     入っていても投稿結果は変わらないのに、指紋だけが変わる
	//   - body は TrimSpace される。末尾の空白が落ちただけで別物になる
	//
	// **422 は再試行では絶対に解けません** (キーを作り直すしかない)。
	// 「1 回目は名前欄あり、タイムアウト後の再送では名前欄が空」という、
	// 利用者から見て同じ操作が弾かれることになります。
	//
	// **添付画像も指紋に含めます。** 投稿結果を変える値だからです。
	// 含めないと、同じキーで別の画像を添えた再送が「同じ内容」と判定され、
	// **黙って前回の応答 (画像なし、または別の画像) が返ります。**
	// クライアントのバグが見えなくなるのは、ADR 0015 が
	// request_hash を持つ理由そのものです。
	//
	// 渡すのは投稿結果を決める値だけ。スレッドは endpoint に含まれています。
	req, err := idempotency.New(key, endpoint, comment.Body, imageFingerprint(comment.ImageID))
	if err != nil {
		return CommentDTO{}, err
	}

	// **記録するのは応答そのもの** (ADR 0015)。
	// 永続化層に DTO の形を知らせないため、詰め替えと符号化はここで行い、
	// バイト列だけを渡します。
	encode := func(c *model.Comment) ([]byte, error) {
		return json.Marshal(i.toDTO(*c))
	}

	created, replayed, err := i.repo.CreateIdempotent(ctx, comment, *req, encode)
	if err != nil {
		return CommentDTO{}, err
	}

	if replayed != nil {
		var dto CommentDTO
		if err := json.Unmarshal(replayed, &dto); err != nil {
			// 記録した本文が読めない = こちらが書式を変えた可能性が高い。
			// **前回と違う応答を返すくらいなら失敗させる。**
			return CommentDTO{}, fmt.Errorf(
				"記録済みの応答を復元できませんでした: %w", err)
		}
		return dto, nil
	}

	return i.toDTO(*created), nil
}

func (i *CommentInteractor) toDTO(c model.Comment) CommentDTO {
	return CommentDTO{
		ID:         c.ID,
		ThreadID:   c.ThreadID,
		Seq:        c.Seq,
		AuthorName: c.AuthorName,
		Author:     toAuthorDTO(c.Author),
		Image:      i.toImageDTO(c.Image),
		Body:       c.Body,
		CreatedAt:  c.CreatedAt,
	}
}

// toAuthorDTO は投稿者を詰め替えます。匿名投稿では nil のまま返します。
//
// 退会済みの表示の差し替えはドメイン側 (model.NewAuthor) で済んでいます。
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

// ensureImageUsable は添付画像が使えるかを確認します。
//
// **投稿する前に確認します。** 投稿したあとで気づくと、
// 「コメントは作られたが画像だけ付かない」状態になります。
//
// resolver が未設定 (ストレージの設定が無い環境) で画像を指定された場合は
// 503 にします —— 黙って画像なしで投稿すると、利用者は
// 「添付したのに消えた」としか分かりません。
func (i *CommentInteractor) ensureImageUsable(
	ctx context.Context, authorID *int64, imageID *uuid.UUID,
) error {
	if imageID == nil {
		return nil
	}
	// NewComment が先に弾いているので到達しない想定。
	// 呼び出し順が変わったときの保険として残す。
	if authorID == nil {
		return fmt.Errorf("画像を添付するにはログインが必要です: %w", apperr.ErrUnauthenticated)
	}
	if i.images == nil {
		return fmt.Errorf("画像は現在利用できません: %w", apperr.ErrUnavailable)
	}
	return i.images.EnsureOwned(ctx, *authorID, *imageID, imageKindCommentAttachment)
}

// imageFingerprint は指紋に含める添付画像の表現です。
//
// **nil と「画像あり」を区別できる形にします。** 空文字を返すと、
// 画像なしの投稿と「UUID が空の画像」が同じ指紋になります
// (現実には後者は作れませんが、区別が値の形に現れているほうが安全です)。
func imageFingerprint(id *uuid.UUID) string {
	if id == nil {
		return "no-image"
	}
	return "image:" + id.String()
}

// toImageDTO はドメインの添付画像を DTO へ詰め替えます。
// URL の組み立てはここで行います (ドメインは配信基盤を知りません)。
func (i *CommentInteractor) toImageDTO(img *model.Image) *ImageDTO {
	if img == nil {
		return nil
	}
	// **resolver が無い環境でも表示は壊さない。**
	// 設定を外した環境では URL を組み立てられないが、
	// 一覧が 500 になるよりは URL 空文字のほうが被害が小さい。
	var url string
	if i.images != nil {
		url = i.images.URL(img.ObjectKey)
	}
	return &ImageDTO{ID: img.ID, URL: url, Width: img.Width, Height: img.Height}
}
