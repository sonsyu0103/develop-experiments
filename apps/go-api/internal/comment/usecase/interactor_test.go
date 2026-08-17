package usecase

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/comment/domain/repository"
	"develop-experiments/apps/go-api/internal/idempotency"
	"develop-experiments/apps/go-api/internal/pagination"
)

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

// fakeCommentRepo は id 降順で並んだコメントを保持し、
// ListByThreadID でカーソルと件数の絞り込みだけを再現します。
type fakeCommentRepo struct {
	// deleteCalls は SoftDeleteOwn に渡された (threadID, id, actorID) です。
	deleteCalls [][3]int64
	comments    []model.Comment
	listErr     error

	// 受け取ったページ指定。カーソルが永続化層まで届いたかの確認に使う。
	gotPage pagination.Page

	// 冪等キーの記録。キー -> 記録済みの応答。
	recorded map[string]recordedResponse
	// createCalls は実際に投稿された回数。**二重送信で増えないこと**を見る。
	createCalls int
	// gotRequest は永続化層まで届いた冪等キーの情報。
	gotRequest idempotency.Request

	// myComments は ListByAuthor が返す「自分のコメント」です。
	// **comments とは別に持ちます** —— model.MyComment は投稿者を持たず
	// スレッドの情報を持つ、別の形だからです。
	myComments []model.MyComment
	// gotAuthorID は ListByAuthor に渡された投稿者 ID です。
	// **他人の ID で呼ばれていないこと**を見るために記録します。
	gotAuthorID int64
}

var _ repository.CommentRepository = (*fakeCommentRepo)(nil)

func newFakeCommentRepo(n int) *fakeCommentRepo {
	// id が大きい順に並べる (実際のクエリが ORDER BY id DESC のため)。
	comments := make([]model.Comment, 0, n)
	for id := int64(n); id >= 1; id-- {
		comments = append(comments, model.Comment{
			ID:       id,
			ThreadID: 1,
			// レス番号。このフェイクでは 1 スレッドしか作らないので
			// id と一致するが、意味は別物 (スレッドごとに 1 から振られる)。
			Seq:        int32(id),
			AuthorName: "名無しさん",
			Body:       "本文",
			CreatedAt:  time.Unix(0, 0).UTC(),
		})
	}
	return &fakeCommentRepo{comments: comments}
}

func (f *fakeCommentRepo) ListByThreadID(
	_ context.Context, threadID int64, page pagination.Page,
) ([]model.Comment, error) {
	f.gotPage = page
	if f.listErr != nil {
		return nil, f.listErr
	}

	out := make([]model.Comment, 0, len(f.comments))
	for _, c := range f.comments {
		if c.ThreadID != threadID {
			continue
		}
		if cursorID := page.CursorID(); cursorID != nil && c.ID >= *cursorID {
			continue
		}
		out = append(out, c)
		if int32(len(out)) == page.Size {
			break
		}
	}
	return out, nil
}

// ListByAuthor は投稿者で絞ったコメントを返します (GET /me/comments)。
//
// **絞り込みそのものは再現しません。** model.MyComment は投稿者を持たない
// ので、フェイク側で「誰のものか」を持たせようがありません。
// 代わりに渡された投稿者 ID を記録し、用意した myComments を
// カーソルと件数だけ適用して返します ——
// 検証したいのはユースケース層のページ送りと詰め替えで、
// 投稿者による絞り込みが効くことは SQL 側の責務です。
func (f *fakeCommentRepo) ListByAuthor(
	_ context.Context, authorID int64, page pagination.Page,
) ([]model.MyComment, error) {
	f.gotAuthorID = authorID
	f.gotPage = page
	if f.listErr != nil {
		return nil, f.listErr
	}

	out := make([]model.MyComment, 0, len(f.myComments))
	for _, c := range f.myComments {
		if cursorID := page.CursorID(); cursorID != nil && c.ID >= *cursorID {
			continue
		}
		out = append(out, c)
		if int32(len(out)) == page.Size {
			break
		}
	}
	return out, nil
}

func (f *fakeCommentRepo) Create(_ context.Context, comment *model.Comment) (*model.Comment, error) {
	created := *comment
	created.ID = int64(len(f.comments) + 1)
	// **採番は永続化層の責務** (docs/adr/0019-comment-concurrency.md)。
	// フェイクなので競合しないが、「値が入って返る」ことだけは本物と揃える。
	created.Seq = int32(len(f.comments) + 1)
	created.CreatedAt = time.Unix(0, 0).UTC()
	return &created, nil
}

// CreateIdempotent は「キーごとに 1 回だけ処理する」ところだけを再現します。
// トランザクションと待ちは本物 (実 DB) の担当なので、ここでは扱いません。
func (f *fakeCommentRepo) CreateIdempotent(
	ctx context.Context, comment *model.Comment, req idempotency.Request,
	encode func(*model.Comment) ([]byte, error),
) (*model.Comment, []byte, error) {
	if f.recorded == nil {
		f.recorded = map[string]recordedResponse{}
	}
	f.gotRequest = req

	if rec, ok := f.recorded[req.Key]; ok {
		if rec.hash != req.RequestHash {
			return nil, nil, fmt.Errorf(
				"同じキーで別の内容: %w", apperr.ErrFailedPrecondition)
		}
		return nil, rec.body, nil
	}

	created, err := f.Create(ctx, comment)
	if err != nil {
		return nil, nil, err
	}
	body, err := encode(created)
	if err != nil {
		return nil, nil, err
	}
	f.recorded[req.Key] = recordedResponse{hash: req.RequestHash, body: body}
	f.createCalls++
	return created, nil, nil
}

// recordedResponse は記録済みの応答です。
type recordedResponse struct {
	hash string
	body []byte
}

func (f *fakeCommentRepo) SoftDelete(context.Context, int64, int64) error { return nil }

// SoftDeleteOwn は呼ばれた引数を記録します。
//
// **ここで見たいのは「ログインしていないと呼ばれないこと」と
// 「パーティションキーがそのまま届くこと」**なので、
// 生存の再現はしません (それは httpapi と実 DB の検査が持ちます)。
func (f *fakeCommentRepo) SoftDeleteOwn(_ context.Context, threadID, id, actorID int64) error {
	f.deleteCalls = append(f.deleteCalls, [3]int64{threadID, id, actorID})
	return nil
}

type fakeThreadChecker struct {
	exists bool
	err    error
	// calls は Exists の呼び出し回数です。
	// **「呼ばれないこと」を見る検査**があるので数えます (FetchMyComments)。
	calls int
}

var _ repository.ThreadExistenceChecker = (*fakeThreadChecker)(nil)

func (f *fakeThreadChecker) Exists(context.Context, int64) (bool, error) {
	f.calls++
	return f.exists, f.err
}

func newInteractor(repo *fakeCommentRepo) *CommentInteractor {
	return NewCommentInteractor(repo, &fakeThreadChecker{exists: true}, nil)
}

// fakeImageResolver は「どの画像も自分のもの」として通します。
// 所有者の判定そのものは image のユースケース側で検査しています。
type fakeImageResolver struct{ err error }

func (f *fakeImageResolver) EnsureOwned(context.Context, int64, uuid.UUID, string) error {
	return f.err
}
func (f *fakeImageResolver) URL(key string) string { return "https://cdn.test/" + key }

func newInteractorWithImages(repo *fakeCommentRepo) *CommentInteractor {
	return NewCommentInteractor(repo, &fakeThreadChecker{exists: true}, &fakeImageResolver{})
}

func mustPage(t *testing.T, cursorID *int64, size int32) pagination.Page {
	t.Helper()

	var token *string
	if cursorID != nil {
		s, err := pagination.NewCursor(*cursorID).Encode()
		if err != nil {
			t.Fatalf("カーソルの符号化が失敗した: %v", err)
		}
		token = &s
	}

	p, err := pagination.NewPage(token, size)
	if err != nil {
		t.Fatalf("pagination.NewPage が失敗した: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// ページ送り
// ---------------------------------------------------------------------------

// 次ページのカーソルは「最後に返した行」を指す必要があります。
//
// コメントは id 降順で返るため、先頭 (最大 id) を指してしまうと
// 2 ページ目が 1 ページ目と同じ内容になり、ページ送りが止まらなくなります。
// 型が同じ int64 なので、取り違えてもコンパイルは通ります。
func TestFetchComments_NextCursorPointsAtLastRow(t *testing.T) {
	t.Parallel()

	uc := newInteractor(newFakeCommentRepo(10))

	got, err := uc.FetchComments(context.Background(), 1, mustPage(t, nil, 3))
	if err != nil {
		t.Fatalf("FetchComments が失敗した: %v", err)
	}

	if len(got.Comments) != 3 {
		t.Fatalf("件数 = %d, want 3", len(got.Comments))
	}
	if got.Comments[0].ID != 10 || got.Comments[2].ID != 8 {
		t.Fatalf("並び順が想定と違う: %+v", got.Comments)
	}

	if got.NextCursor == nil {
		t.Fatal("NextCursor = nil, want トークン")
	}
	c, err := pagination.DecodeCursor(*got.NextCursor)
	if err != nil {
		t.Fatalf("NextCursor が復号できない (%q): %v", *got.NextCursor, err)
	}
	if c.ID != 8 {
		t.Errorf("NextCursor が指す ID = %d, want 8 (最後に返した行)", c.ID)
	}
}

// 実際に 2 ページ目を引いて、1 ページ目と重ならないことを確認します。
// カーソルの向き (id < cursor) が逆になっていると、ここで気づけます。
func TestFetchComments_SecondPageDoesNotOverlap(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(10)
	uc := newInteractor(repo)
	ctx := context.Background()

	page1, err := uc.FetchComments(ctx, 1, mustPage(t, nil, 3))
	if err != nil {
		t.Fatalf("1 ページ目の取得に失敗した: %v", err)
	}

	next, err := pagination.DecodeCursor(*page1.NextCursor)
	if err != nil {
		t.Fatalf("NextCursor が復号できない: %v", err)
	}

	page2, err := uc.FetchComments(ctx, 1, mustPage(t, &next.ID, 3))
	if err != nil {
		t.Fatalf("2 ページ目の取得に失敗した: %v", err)
	}

	ids1 := idsOf(page1.Comments)
	ids2 := idsOf(page2.Comments)
	if ids2[0] != 7 {
		t.Errorf("2 ページ目の先頭 = %d, want 7", ids2[0])
	}
	for _, a := range ids1 {
		for _, b := range ids2 {
			if a == b {
				t.Fatalf("ページ間で ID %d が重複した (1: %v, 2: %v)", a, ids1, ids2)
			}
		}
	}
}

func TestFetchComments_NoCursorOnLastPage(t *testing.T) {
	t.Parallel()

	uc := newInteractor(newFakeCommentRepo(2))

	got, err := uc.FetchComments(context.Background(), 1, mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchComments が失敗した: %v", err)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("件数 = %d, want 2", len(got.Comments))
	}
	if got.NextCursor != nil {
		t.Errorf("NextCursor = %q, want nil", *got.NextCursor)
	}
}

// カーソルは永続化層まで id として届く必要があります。
// ここが nil のままだと、常に先頭ページが返り続けます。
func TestFetchComments_CursorReachesRepository(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(10)
	uc := newInteractor(repo)

	cursorID := int64(8)
	if _, err := uc.FetchComments(context.Background(), 1, mustPage(t, &cursorID, 3)); err != nil {
		t.Fatalf("FetchComments が失敗した: %v", err)
	}

	gotID := repo.gotPage.CursorID()
	if gotID == nil || *gotID != 8 {
		t.Errorf("リポジトリが受け取ったカーソル = %v, want 8", gotID)
	}
}

func TestFetchComments_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	uc := newInteractor(newFakeCommentRepo(0))

	got, err := uc.FetchComments(context.Background(), 1, mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchComments が失敗した: %v", err)
	}
	// nil スライスだと JSON が null になり、フロントで .map() が落ちる。
	if got.Comments == nil {
		t.Error("Comments = nil, want 空スライス (JSON で null にしないため)")
	}
}

// ---------------------------------------------------------------------------
// 存在確認とエラーの伝播
// ---------------------------------------------------------------------------

// 存在しないスレッドは「コメント 0 件」ではなく 404 相当になります。
func TestFetchComments_ThreadNotFound(t *testing.T) {
	t.Parallel()

	uc := NewCommentInteractor(newFakeCommentRepo(3), &fakeThreadChecker{exists: false}, nil)

	_, err := uc.FetchComments(context.Background(), 999, mustPage(t, nil, 10))
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("err = %v, want apperr.ErrNotFound", err)
	}
}

func TestFetchComments_PropagatesRepositoryError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("DB がダウンしています")
	repo := newFakeCommentRepo(3)
	repo.listErr = sentinel

	_, err := newInteractor(repo).FetchComments(context.Background(), 1, mustPage(t, nil, 10))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func idsOf(dtos []CommentDTO) []int64 {
	ids := make([]int64, 0, len(dtos))
	for _, d := range dtos {
		ids = append(ids, d.ID)
	}
	return ids
}

// レス番号がユースケース層の DTO まで運ばれること。
//
// **これが無いと詰め替えを消してもテストが通る** (変異プローブで実測)。
// ドメインからワイヤ型までの経路は、途中の 1 か所を落とすだけで
// API から静かにフィールドが消える。
func TestComments_SeqReachesDTO(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(3)
	interactor := NewCommentInteractor(repo, &fakeThreadChecker{exists: true}, nil)

	t.Run("一覧", func(t *testing.T) {
		t.Parallel()

		got, err := interactor.FetchComments(t.Context(), 1, pagination.Page{Size: 10})
		if err != nil {
			t.Fatalf("FetchComments が失敗した: %v", err)
		}
		if len(got.Comments) == 0 {
			t.Fatal("コメントが 0 件")
		}
		for _, c := range got.Comments {
			// ID と一致することではなく、0 でないことを見る。
			// 「ID をそのまま入れている」実装も通してしまわないよう、
			// 対応関係はフェイク側 (Seq = id) で決めてある。
			if c.Seq == 0 {
				t.Errorf("id=%d のコメントに seq が入っていない", c.ID)
			}
			if c.Seq != int32(c.ID) {
				t.Errorf("id=%d の seq = %d, want %d", c.ID, c.Seq, c.ID)
			}
		}
	})

	t.Run("投稿", func(t *testing.T) {
		t.Parallel()

		got, err := interactor.PostComment(t.Context(), 1, "ホシノ", "ふぁ〜", nil, nil)
		if err != nil {
			t.Fatalf("PostComment が失敗した: %v", err)
		}
		if got.Seq == 0 {
			t.Error("投稿の応答に seq が入っていない (採番結果が返らない)")
		}
	})
}

// ---------------------------------------------------------------------------
// 冪等キー (docs/adr/0015-idempotency.md)
// ---------------------------------------------------------------------------

// 同じキーで 2 回送っても、投稿は 1 回しか行われないこと。
//
// **これが Phase 2 後半の主題。** タイムアウト後の再送は
// 「サーバでは成功していた」場合があり、ボタン制御では原理的に防げない。
func TestPostCommentIdempotent_SecondCallDoesNotCreate(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	uc := newInteractor(repo)
	authorID := int64(42)

	first, err := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, nil, "key-1", "POST /threads/1/comments")
	if err != nil {
		t.Fatalf("1 回目が失敗した: %v", err)
	}

	second, err := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, nil, "key-1", "POST /threads/1/comments")
	if err != nil {
		t.Fatalf("2 回目が失敗した: %v", err)
	}

	if repo.createCalls != 1 {
		t.Errorf("投稿が %d 回行われた, want 1 (二重投稿)", repo.createCalls)
	}
	// 記録した応答をそのまま返す。ID が変わっていたら別の投稿になっている。
	if first.ID != second.ID || first.Seq != second.Seq {
		t.Errorf("再送で別の応答が返った: %+v vs %+v", first, second)
	}
}

// 同じキーで別の内容を送ったら 422 相当になること。
// 黙って前回の結果を返すと、クライアントのバグが見えなくなる。
func TestPostCommentIdempotent_DifferentBodyIsRejected(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	uc := newInteractor(repo)
	authorID := int64(42)

	if _, postErr := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, nil,
		"key-1", "POST /threads/1/comments"); postErr != nil {
		t.Fatalf("1 回目が失敗した: %v", postErr)
	}

	// 同じキー、違う本文。
	_, err := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "おはよう", &authorID, nil, "key-1", "POST /threads/1/comments")

	if !errors.Is(err, apperr.ErrFailedPrecondition) {
		t.Fatalf("err = %v, want apperr.ErrFailedPrecondition (422)", err)
	}
	if repo.createCalls != 1 {
		t.Errorf("投稿が %d 回行われた, want 1", repo.createCalls)
	}
}

// **同じキーで別の画像も 422 になること。**
//
// 添付は投稿結果を変えるので、指紋に含める必要があります
// (docs/adr/0015-idempotency.md の request_hash が存在する理由)。
// 含めないと、画像を差し替えた再送が「同じ内容」と判定され、
// **黙って前回の応答 (別の画像) が返ります。**
//
// 本文の違いを見るテストだけでは、この抜けを検出できませんでした
// (変異プローブで実測)。
func TestPostCommentIdempotent_DifferentImageIsRejected(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	uc := newInteractorWithImages(repo)
	authorID := int64(42)
	firstImage := uuid.New()
	secondImage := uuid.New()

	if _, postErr := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, &firstImage,
		"key-img", "POST /threads/1/comments"); postErr != nil {
		t.Fatalf("1 回目が失敗した: %v", postErr)
	}

	// 同じキー・同じ本文で、画像だけ違う。
	_, err := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, &secondImage,
		"key-img", "POST /threads/1/comments")

	if !errors.Is(err, apperr.ErrFailedPrecondition) {
		t.Fatalf("err = %v, want apperr.ErrFailedPrecondition (422)", err)
	}
	if repo.createCalls != 1 {
		t.Errorf("投稿が %d 回行われた, want 1", repo.createCalls)
	}
}

// 画像の有無が変わった場合も 422 になること。
// 「画像あり -> なし」を握りつぶすと、添付が黙って消えたように見えます。
func TestPostCommentIdempotent_DroppingImageIsRejected(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	uc := newInteractorWithImages(repo)
	authorID := int64(42)
	imageID := uuid.New()

	if _, postErr := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, &imageID,
		"key-drop", "POST /threads/1/comments"); postErr != nil {
		t.Fatalf("1 回目が失敗した: %v", postErr)
	}

	_, err := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, nil,
		"key-drop", "POST /threads/1/comments")

	if !errors.Is(err, apperr.ErrFailedPrecondition) {
		t.Fatalf("err = %v, want apperr.ErrFailedPrecondition (422)", err)
	}
}

// **結果に影響しない差で 422 にしないこと。**
//
// 指紋を「受け取ったままの値」から作ると、ここが 422 になる。
// 422 は再試行では絶対に解けない (キーを作り直すしかない) ので、
// 利用者から見て同じ操作が永久に通らなくなる。
func TestPostCommentIdempotent_NormalizedFieldsDoNotChangeFingerprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		firstAuthorName string
		firstBody       string
		retryAuthorName string
		retryBody       string
	}{
		{
			// ログイン中は authorName が捨てられる (model.NewComment)。
			// 投稿結果は変わらないのに、指紋だけが変わってはいけない。
			name:            "再送で名前欄が空になっても同じ",
			firstAuthorName: "ホシノ", firstBody: "ふぁ〜",
			retryAuthorName: "", retryBody: "ふぁ〜",
		},
		{
			name:            "再送で名前欄が変わっても同じ",
			firstAuthorName: "ホシノ", firstBody: "ふぁ〜",
			retryAuthorName: "先生", retryBody: "ふぁ〜",
		},
		{
			// 本文は TrimSpace される。末尾の空白の有無で別物にしない。
			name:            "本文の前後の空白は無視される",
			firstAuthorName: "ホシノ", firstBody: "ふぁ〜",
			retryAuthorName: "ホシノ", retryBody: "  ふぁ〜  ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := newFakeCommentRepo(0)
			uc := newInteractor(repo)
			authorID := int64(42)

			first, err := uc.PostCommentIdempotent(t.Context(), 1,
				tt.firstAuthorName, tt.firstBody, &authorID, nil, "key-1", "POST /threads/1/comments")
			if err != nil {
				t.Fatalf("1 回目が失敗した: %v", err)
			}

			second, err := uc.PostCommentIdempotent(t.Context(), 1,
				tt.retryAuthorName, tt.retryBody, &authorID, nil, "key-1", "POST /threads/1/comments")
			if err != nil {
				t.Fatalf("再送が失敗した (結果に影響しない差で 422 になっている): %v", err)
			}

			if repo.createCalls != 1 {
				t.Errorf("投稿が %d 回行われた, want 1", repo.createCalls)
			}
			if first.ID != second.ID {
				t.Errorf("再送で別の投稿になった: %+v vs %+v", first, second)
			}
		})
	}
}

// **匿名では冪等キーを使えない** (ADR 0015 決定 4)。
//
// キーの名前空間を分ける手段が無いため、通してしまうと
// 他人のキーと衝突して「他人の投稿結果が返る」ことになる。
// HTTP 層が落とす前提だが、事故の重さから見てここでも閉じる。
func TestPostCommentIdempotent_AnonymousIsRejected(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	uc := newInteractor(repo)

	_, err := uc.PostCommentIdempotent(
		t.Context(), 1, "", "ふぁ〜", nil, nil, "key-1", "POST /threads/1/comments")
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want apperr.ErrInvalidArgument", err)
	}
	if repo.createCalls != 0 {
		t.Errorf("匿名なのに投稿された (%d 回)", repo.createCalls)
	}
}

// キーの情報が永続化層まで届くこと。
// ここで落ちると、冪等性が「実装したが効いていない」状態になる。
func TestPostCommentIdempotent_RequestReachesRepository(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	uc := newInteractor(repo)
	authorID := int64(42)

	if _, err := uc.PostCommentIdempotent(t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, nil,
		"key-xyz", "POST /threads/1/comments"); err != nil {
		t.Fatalf("PostCommentIdempotent が失敗した: %v", err)
	}

	if repo.gotRequest.Key != "key-xyz" {
		t.Errorf("リポジトリが受け取ったキー = %q, want key-xyz", repo.gotRequest.Key)
	}
	if repo.gotRequest.RequestHash == "" {
		t.Error("指紋がリポジトリまで届いていない (別内容の検出が効かない)")
	}
	if repo.gotRequest.Endpoint != "POST /threads/1/comments" {
		t.Errorf("経路 = %q, want POST /threads/1/comments", repo.gotRequest.Endpoint)
	}
}

// 不正なキーは投稿より先に弾くこと。
func TestPostCommentIdempotent_InvalidKeyIsRejected(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	uc := newInteractor(repo)
	authorID := int64(42)

	_, err := uc.PostCommentIdempotent(
		t.Context(), 1, "ホシノ", "ふぁ〜", &authorID, nil, "   ", "POST /threads/1/comments")
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want apperr.ErrInvalidArgument", err)
	}
	if repo.createCalls != 0 {
		t.Errorf("キーが不正なのに投稿された (%d 回)", repo.createCalls)
	}
}

// **未ログインを usecase 側でも弾くこと** (thread 側と同じ理由)。
func TestDeleteOwnComment_RequiresActor(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	err := NewCommentInteractor(repo, &fakeThreadChecker{exists: true}, nil).
		DeleteOwnComment(t.Context(), 1, 10, nil)
	if !errors.Is(err, apperr.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	if len(repo.deleteCalls) != 0 {
		t.Error("未ログインなのに永続化層まで届いている")
	}
}

// **パーティションキーと内部 ID がそのまま渡ること。**
func TestDeleteOwnComment_PassesKeys(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	actor := int64(42)
	if err := NewCommentInteractor(repo, &fakeThreadChecker{exists: true}, nil).
		DeleteOwnComment(t.Context(), 1, 10, &actor); err != nil {
		t.Fatalf("DeleteOwnComment が失敗した: %v", err)
	}
	if len(repo.deleteCalls) != 1 || repo.deleteCalls[0] != [3]int64{1, 10, 42} {
		t.Errorf("渡された値 = %v, want [{1 10 42}]", repo.deleteCalls)
	}
}

// マイページの「自分のコメント」(GET /me/comments) の検査。
//
// **スレッドの存在確認をしないこと**がスレッド内一覧との一番の違いです。
// 絞り込みの軸が利用者なので、0 件は「まだ書いていない」という正しい答えになります。

// strptr は文字列へのポインタを返します。
// ThreadTitle は削除済みで nil になるため、生きている側を書くのに要ります。
func strptr(s string) *string { return &s }

// newMyComments は id が大きい順の MyComment を n 件作ります。
// **すべて生きているスレッド**への投稿です。
func newMyComments(n int) []model.MyComment {
	out := make([]model.MyComment, 0, n)
	for id := int64(n); id >= 1; id-- {
		out = append(out, model.MyComment{
			ID: id, ThreadID: id, ThreadTitle: strptr(fmt.Sprintf("スレッド %d", id)),
			Seq: int32(id), Body: fmt.Sprintf("本文 %d", id),
			CreatedAt: time.Unix(id, 0).UTC(),
		})
	}
	return out
}

// TestFetchMyComments_PassesAuthorAndMapsThread は、投稿者 ID が永続化層まで届き、
// スレッドの情報が DTO に載ることを確かめます。
func TestFetchMyComments_PassesAuthorAndMapsThread(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	repo.myComments = []model.MyComment{{
		ID: 10, ThreadID: 3, ThreadTitle: strptr("眠いスレ"), ThreadDeleted: false,
		Seq: 5, Body: "ふぁ〜", CreatedAt: time.Unix(10, 0).UTC(),
	}}
	uc := NewCommentInteractor(repo, &fakeThreadChecker{}, nil)

	got, err := uc.FetchMyComments(context.Background(), 42, mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchMyComments が失敗した: %v", err)
	}

	if repo.gotAuthorID != 42 {
		t.Errorf("永続化層に届いた投稿者 ID = %d, want 42", repo.gotAuthorID)
	}
	if len(got.Comments) != 1 {
		t.Fatalf("件数 = %d, want 1", len(got.Comments))
	}

	c := got.Comments[0]
	if c.ID != 10 || c.ThreadID != 3 || c.Seq != 5 {
		t.Errorf("識別子が詰め替えられていない: %+v", c)
	}
	// **一覧として成立させるための 2 項目。**
	if c.ThreadTitle == nil || *c.ThreadTitle != "眠いスレ" {
		t.Errorf("ThreadTitle = %v, want 眠いスレ", c.ThreadTitle)
	}
	if c.ThreadDeleted {
		t.Error("ThreadDeleted = true, want false")
	}
}

// TestFetchMyComments_DeletedThreadKeepsCommentButDropsTitle は、
// **削除済みスレッドのタイトルを運ばない**ことを確かめます。
//
// タイトル自体が誹謗中傷や個人情報だったために消された場合、ここで運ぶと
// 書き込んだ全員のマイページに残り続けます。GET /threads/{id} は 404 なので、
// この API が削除後にタイトルを読める唯一の経路になってしまいます。
//
// **コメント自体は落としません。** 落とすと「消えた」のか
// 「元から無い」のかを本人が区別できなくなります。
func TestFetchMyComments_DeletedThreadKeepsCommentButDropsTitle(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	// 永続化層が返す形。SQL 側が LEFT JOIN の空振りで nil を返します。
	repo.myComments = []model.MyComment{{
		ID: 11, ThreadID: 4, ThreadTitle: nil, ThreadDeleted: true,
		Seq: 1, Body: "自分が書いた本文は残る", CreatedAt: time.Unix(11, 0).UTC(),
	}}
	uc := NewCommentInteractor(repo, &fakeThreadChecker{}, nil)

	got, err := uc.FetchMyComments(context.Background(), 1, mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchMyComments が失敗した: %v", err)
	}

	if len(got.Comments) != 1 {
		t.Fatalf("件数 = %d, want 1 (削除済みでもコメントは落とさない)", len(got.Comments))
	}
	c := got.Comments[0]
	if c.ThreadTitle != nil {
		t.Errorf("ThreadTitle = %q, want nil (削除済みのタイトルは運ばない)", *c.ThreadTitle)
	}
	if !c.ThreadDeleted {
		t.Error("ThreadDeleted = false, want true")
	}
	// 本人が書いた本文は残る。
	if c.Body != "自分が書いた本文は残る" {
		t.Errorf("Body = %q, want 本文がそのまま", c.Body)
	}
}

// TestFetchMyComments_DoesNotCheckThreadExistence は、
// **親スレッドの存在確認を行わない**ことを確かめます。
//
// FetchComments と取り違えて Exists を挟むと、
// スレッド ID を持たないこの経路では確認しようがなく、
// 実装が壊れるか無意味な問い合わせが増えます。
func TestFetchMyComments_DoesNotCheckThreadExistence(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	repo.myComments = newMyComments(2)
	checker := &fakeThreadChecker{}
	uc := NewCommentInteractor(repo, checker, nil)

	if _, err := uc.FetchMyComments(context.Background(), 1, mustPage(t, nil, 10)); err != nil {
		t.Fatalf("FetchMyComments が失敗した: %v", err)
	}

	if checker.calls != 0 {
		t.Errorf("スレッドの存在確認が %d 回呼ばれた, want 0", checker.calls)
	}
}

// TestFetchMyComments_Paginates はカーソルが効くことを確かめます。
func TestFetchMyComments_Paginates(t *testing.T) {
	t.Parallel()

	repo := newFakeCommentRepo(0)
	repo.myComments = newMyComments(5)
	uc := NewCommentInteractor(repo, &fakeThreadChecker{}, nil)
	ctx := context.Background()

	first, err := uc.FetchMyComments(ctx, 1, mustPage(t, nil, 3))
	if err != nil {
		t.Fatalf("1 ページ目が失敗した: %v", err)
	}
	if len(first.Comments) != 3 || first.Comments[0].ID != 5 {
		t.Fatalf("1 ページ目 = %+v, want id 5,4,3", first.Comments)
	}
	if first.NextCursor == nil {
		t.Fatal("NextCursor = nil, want トークン (まだ続きがある)")
	}

	next, err := pagination.DecodeCursor(*first.NextCursor)
	if err != nil {
		t.Fatalf("NextCursor が復号できない: %v", err)
	}
	second, err := uc.FetchMyComments(ctx, 1, mustPage(t, &next.ID, 3))
	if err != nil {
		t.Fatalf("2 ページ目が失敗した: %v", err)
	}
	if len(second.Comments) != 2 || second.Comments[0].ID != 2 {
		t.Fatalf("2 ページ目 = %+v, want id 2,1", second.Comments)
	}
	// 返しきったので次は無い。
	if second.NextCursor != nil {
		t.Errorf("NextCursor = %v, want nil", *second.NextCursor)
	}
}

// TestFetchMyComments_EmptyIsNotNil は 0 件が nil スライスにならないことを確かめます。
func TestFetchMyComments_EmptyIsNotNil(t *testing.T) {
	t.Parallel()

	uc := NewCommentInteractor(newFakeCommentRepo(0), &fakeThreadChecker{}, nil)

	got, err := uc.FetchMyComments(context.Background(), 1, mustPage(t, nil, 10))
	if err != nil {
		t.Fatalf("FetchMyComments が失敗した: %v", err)
	}
	if got.Comments == nil {
		t.Error("Comments = nil, want 空スライス (JSON で null にしないため)")
	}
}

// TestFetchMyComments_PropagatesRepositoryError はエラーがそのまま伝わることを確かめます。
func TestFetchMyComments_PropagatesRepositoryError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("DB がダウンしています")
	repo := newFakeCommentRepo(0)
	repo.listErr = sentinel
	uc := NewCommentInteractor(repo, &fakeThreadChecker{}, nil)

	_, err := uc.FetchMyComments(context.Background(), 1, mustPage(t, nil, 10))
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}
