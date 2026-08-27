package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	commentmodel "develop-experiments/apps/go-api/internal/comment/domain/model"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	threadmodel "develop-experiments/apps/go-api/internal/thread/domain/model"
)

// マイページの「自分の投稿」2 経路 (GET /me/threads / GET /me/comments) の検査。
//
// **この 2 つは security を宣言した読み取り**という、これまでに無い組み合わせです
// (/me と /me/avatar は自分自身の情報で、一覧ではありません)。
// 見るべき穴もそこにあります。
//
//  1. 未ログインで 401 になるか (宣言が効いているか)
//  2. **セッションの利用者 ID がそのまま永続化層まで届くか**
//     —— ここが固定値や別経路の値に化けると、他人の投稿が見えます
//  3. 一覧特有の詰め替え (空でも null にしない、カーソルが載る)

// TestListMyThreads_RequiresSession は未ログインが 401 になることを確かめます。
//
// **security 宣言が効いていることの検査です。** 宣言を落とすと、
// requireAuthorID が拾って 401 にはなりますが、
// 「仕様書に書いたから 401 になる」経路が消えていることに気づけません。
func TestListMyThreads_RequiresSession(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t, false)

	for _, path := range []string{"/me/threads", "/me/comments"} {
		rec := env.do(t, http.MethodGet, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, rec.Code)
		}
	}
}

// TestListMyThreads_PassesSessionUserToRepository は、
// **絞り込みに使う ID がセッション由来である**ことを確かめます。
//
// これが最も危険な穴です。投稿者をクエリや本文から受け取る実装に
// すり替わっても、応答の形は変わらないため気づけません。
func TestListMyThreads_PassesSessionUserToRepository(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t, false)
	env.threads.mySummaries = []threadmodel.Summary{
		{
			Thread:       *threadmodel.Reconstruct(7, "自分のスレッド", nil, nil, time.Unix(7, 0).UTC(), 3),
			CommentCount: 2,
		},
	}

	rec := env.do(t, http.MethodGet, "/me/threads", sessionCookie(env.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// newAuthEnv のセッションの持ち主は ID 1。
	if got := env.threads.myAuthorIDs; len(got) != 1 || got[0] != 1 {
		t.Fatalf("リポジトリに渡った投稿者 ID = %v, want [1] (セッションの利用者)", got)
	}

	var body oapigen.ThreadList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("応答が ThreadList として読めない: %v", err)
	}
	if len(body.Threads) != 1 || body.Threads[0].Id != 7 {
		t.Fatalf("Threads = %+v, want id 7 の 1 件", body.Threads)
	}
	if body.Threads[0].CommentCount != 2 {
		t.Errorf("CommentCount = %d, want 2", body.Threads[0].CommentCount)
	}
}

// TestListMyComments_PassesSessionUserToRepository は、
// コメント側も同じくセッション由来の ID で絞ることを確かめます。
func TestListMyComments_PassesSessionUserToRepository(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t, false)
	env.comments.myComments = []commentmodel.MyComment{
		{
			ID: 10, ThreadID: 1, ThreadTitle: strptr("スレッド"), ThreadDeleted: false,
			Seq: 3, Body: "ふぁ〜、眠いよ〜", CreatedAt: time.Unix(10, 0).UTC(),
		},
	}

	rec := env.do(t, http.MethodGet, "/me/comments", sessionCookie(env.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	if got := env.comments.myAuthorIDs; len(got) != 1 || got[0] != 1 {
		t.Fatalf("リポジトリに渡った投稿者 ID = %v, want [1] (セッションの利用者)", got)
	}

	var body oapigen.MyCommentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("応答が MyCommentList として読めない: %v", err)
	}
	if len(body.Comments) != 1 {
		t.Fatalf("件数 = %d, want 1", len(body.Comments))
	}

	got := body.Comments[0]
	if got.Id != 10 || got.ThreadId != 1 || got.Seq != 3 {
		t.Errorf("識別子が詰め替えられていない: %+v", got)
	}
	// **スレッドのタイトルが載ること。** これが無いと一覧として成立しません。
	if got.ThreadTitle == nil || *got.ThreadTitle != "スレッド" {
		t.Errorf("ThreadTitle = %v, want %q", got.ThreadTitle, "スレッド")
	}
	if got.ThreadDeleted {
		t.Error("ThreadDeleted = true, want false")
	}
}

// TestListMyComments_DeletedThreadIsMarkedAndTitleWithheld は、
// **削除済みスレッドへのコメントが「消えず、印がつき、タイトルは出ない」**
// ことを確かめます。
//
// 落とす実装にすると、自分の投稿が「消えた」のか「元から無い」のかを
// 本人が区別できなくなります。件数だけ見ていると気づけない差です。
//
// 一方でタイトルを運ぶと、**削除後もタイトルを読める唯一の経路**になります
// (GET /threads/{id} は 404)。タイトル自体が理由で消された場合に、
// 書き込んだ全員のマイページへ残り続けることになります。
func TestListMyComments_DeletedThreadIsMarkedAndTitleWithheld(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t, false)
	env.comments.myComments = []commentmodel.MyComment{
		{
			// **削除済みではタイトルを運ばない** (SQL 側が LEFT JOIN の空振りで nil にする)。
			ID: 11, ThreadID: 2, ThreadTitle: nil, ThreadDeleted: true,
			Seq: 1, Body: "本文", CreatedAt: time.Unix(11, 0).UTC(),
		},
	}

	rec := env.do(t, http.MethodGet, "/me/comments", sessionCookie(env.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body oapigen.MyCommentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("応答が読めない: %v", err)
	}
	if len(body.Comments) != 1 {
		t.Fatalf("件数 = %d, want 1 (削除済みスレッドでも落とさない)", len(body.Comments))
	}
	if !body.Comments[0].ThreadDeleted {
		t.Error("ThreadDeleted = false, want true")
	}
	// **タイトルは運ばない。** ここが漏れると、消したタイトルが
	// 書き込んだ全員のマイページに残り続ける。
	if got := body.Comments[0].ThreadTitle; got != nil {
		t.Errorf("ThreadTitle = %q, want null (削除済みのタイトルは返さない)", *got)
	}
	// 自分が書いた本文は残る。
	if body.Comments[0].Body != "本文" {
		t.Errorf("Body = %q, want 本文がそのまま", body.Comments[0].Body)
	}
}

// TestListMyComments_ImageIsCarried は添付画像が応答まで届くことを確かめます。
//
// **詰め替えの取りこぼしは実際に起きた事故です** (toWireComment の初版)。
// DTO に載っていても写し忘れれば応答に出ず、リポジトリ側だけ見る
// テストでは通ってしまいます。
func TestListMyComments_ImageIsCarried(t *testing.T) {
	t.Parallel()

	imageID := uuid.MustParse("01920000-0000-7000-8000-0000000000ff")
	env := newAuthEnv(t, false)
	env.comments.myComments = []commentmodel.MyComment{
		{
			ID: 12, ThreadID: 1, ThreadTitle: strptr("スレッド"), Seq: 1,
			Body: "画像つき", CreatedAt: time.Unix(12, 0).UTC(),
			Image: commentmodel.NewImage(imageID, "objects/ff.webp", 640, 480),
		},
	}

	rec := env.do(t, http.MethodGet, "/me/comments", sessionCookie(env.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body oapigen.MyCommentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("応答が読めない: %v", err)
	}
	if len(body.Comments) != 1 || body.Comments[0].Image == nil {
		t.Fatalf("Image が応答に載っていない: %+v", body.Comments)
	}
	if got := body.Comments[0].Image; got.Id != imageID || got.Width != 640 || got.Height != 480 {
		t.Errorf("Image = %+v, want id/寸法がそのまま", got)
	}
}

// TestListMyPosts_EmptyIsNotNull は 0 件が JSON の null にならないことを確かめます。
// null になるとフロントの .map() が落ちます。
func TestListMyPosts_EmptyIsNotNull(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t, false)

	rec := env.do(t, http.MethodGet, "/me/threads", sessionCookie(env.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("/me/threads: status = %d, want 200", rec.Code)
	}
	var threads oapigen.ThreadList
	if err := json.Unmarshal(rec.Body.Bytes(), &threads); err != nil {
		t.Fatalf("応答が読めない: %v", err)
	}
	if threads.Threads == nil {
		t.Error("threads = null, want []")
	}
	if threads.NextCursor != nil {
		t.Errorf("NextCursor = %v, want null (次ページは無い)", *threads.NextCursor)
	}

	rec = env.do(t, http.MethodGet, "/me/comments", sessionCookie(env.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("/me/comments: status = %d, want 200", rec.Code)
	}
	var comments oapigen.MyCommentList
	if err := json.Unmarshal(rec.Body.Bytes(), &comments); err != nil {
		t.Fatalf("応答が読めない: %v", err)
	}
	if comments.Comments == nil {
		t.Error("comments = null, want []")
	}
}

// TestListMyPosts_RejectsBrokenCursor は壊れたカーソルが 400 になることを確かめます。
// 一覧の共通処理 (toPage) を通っていることの確認になります。
func TestListMyPosts_RejectsBrokenCursor(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t, false)

	for _, path := range []string{"/me/threads", "/me/comments"} {
		rec := env.do(t, http.MethodGet, path+"?cursor=!!!!", sessionCookie(env.token))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body: %s)", path, rec.Code, rec.Body.String())
		}
	}
}

// strptr は文字列へのポインタを返します。
// **MyComment.ThreadTitle は削除済みで nil になる**ため、
// 生きているスレッドを書くのに要ります。
func strptr(s string) *string { return &s }
