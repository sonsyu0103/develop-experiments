package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	commentmodel "develop-experiments/apps/go-api/internal/comment/domain/model"
	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	moderationusecase "develop-experiments/apps/go-api/internal/moderation/usecase"
	threadmodel "develop-experiments/apps/go-api/internal/thread/domain/model"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
	"develop-experiments/apps/go-api/internal/viewcount"
)

// ---------------------------------------------------------------------------
// 本人による削除 (ADR 0005 の権限モデル / ADR 0003 未決 #7)
// ---------------------------------------------------------------------------

// deleteEnv は「自分の投稿」と「他人の投稿」と「匿名の投稿」を並べた環境です。
type deleteEnv struct {
	router   http.Handler
	threads  *fakeThreadRepo
	comments *fakeCommentRepo
	token    usermodel.SessionToken
}

// ログイン中の利用者の内部 ID。
const deleteTestUserID = int64(77)

func newDeleteEnv(t *testing.T) *deleteEnv {
	t.Helper()

	const token = usermodel.SessionToken("delete-test-token")
	sessions := &fakeSessionRepo{
		liveToken: token,
		owner: usermodel.SessionOwner{
			ID: deleteTestUserID, PublicID: uuid.MustParse("01920000-0000-7000-8000-000000000077"),
			Email: "owner@example.com", DisplayName: "ホシノ", Role: usermodel.RoleUser,
		},
	}

	me := deleteTestUserID
	other := int64(99)

	// **AuthorID は Reconstruct が埋めない。**
	// 読み出し経路のクエリは author_id を選ばず、代わりに JOIN 済みの
	// Author を返すため (thread.go の型コメント)。所有の判定に使うのは
	// 内部 ID のほうなので、ここでは書き込み時と同じ形に手で揃える。
	ownedThread := func(id int64, title string, authorID *int64, at int64) threadmodel.Summary {
		th := threadmodel.Reconstruct(id, title, nil, nil, time.Unix(at, 0).UTC(), 0)
		th.AuthorID = authorID
		return threadmodel.Summary{Thread: *th}
	}
	ownedComment := func(id int64, seq int32, name string, authorID *int64, body string, at int64) commentmodel.Comment {
		c := commentmodel.Reconstruct(id, 1, seq, name, nil, nil, body, time.Unix(at, 0).UTC())
		c.AuthorID = authorID
		return *c
	}

	// 1 = 自分のもの / 2 = 他人のもの / 3 = 匿名
	threads := &fakeThreadRepo{
		summaries: []threadmodel.Summary{
			ownedThread(1, "自分のスレッド", &me, 1),
			ownedThread(2, "他人のスレッド", &other, 2),
			ownedThread(3, "匿名のスレッド", nil, 3),
		},
	}
	comments := &fakeCommentRepo{
		comments: []commentmodel.Comment{
			ownedComment(10, 1, "ホシノ", &me, "自分のコメント", 4),
			ownedComment(11, 2, "誰か", &other, "他人のコメント", 5),
			ownedComment(12, 3, "名無しさん", nil, "匿名のコメント", 6),
		},
	}

	router, err := NewRouter(Deps{
		Server: NewServer(
			threadusecase.NewThreadInteractor(threads, nil),
			commentusecase.NewCommentInteractor(comments, threads, nil),
			&fakePinger{},
			userusecase.NewSessionInteractor(sessions, nil),
			nil,
			nil,
			moderationusecase.NewInteractor(newFakeModerationRepo()),
			moderationusecase.NewReportInteractor(newFakeReportRepo(), newFakeReportRepo()),
			// 問い合わせの受付は設定に依存せず常に結線する (ADR 0008 決定 1)。
			newTestContactInteractor(),
			viewcount.New(),
			config.AuthConfig{FrontendURL: "http://localhost:3000"}),
		AllowedOrigins: []string{testOrigin},
	})
	if err != nil {
		t.Fatalf("NewRouter が失敗した: %v", err)
	}
	return &deleteEnv{router: router, threads: threads, comments: comments, token: token}
}

func (e *deleteEnv) del(t *testing.T, path string, withSession bool) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, path, nil)
	// **Origin が要る** (ADR 0013 決定 1)。
	req.Header.Set("Origin", testOrigin)
	if withSession {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: string(e.token)})
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// **自分のスレッドは消せること。** 応答は 204 で本文なし。
func TestDeleteThread_DeletesOwn(t *testing.T) {
	env := newDeleteEnv(t)

	rec := env.del(t, "/threads/1", true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 なのに本文がある: %s", rec.Body.String())
	}
	if !env.threads.deletedThreads[1] {
		t.Error("スレッドが論理削除されていない")
	}
}

// **他人と匿名のスレッドは 403** (ADR 0005 の権限モデル)。
//
// 匿名を含めるのが要点。**投稿者を特定する情報が無い**ので、
// 本人であることを示せません (ADR 0005 決定 2)。
// 消せるのはモデレーターだけです (ADR 0011 決定 2)。
func TestDeleteThread_RejectsOthersAndAnonymous(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"他人のスレッド", "/threads/2"},
		{"匿名のスレッド", "/threads/3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newDeleteEnv(t)

			rec := env.del(t, tc.path, true)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			if code := decodeError(t, rec).Error.Code; code != oapigen.PERMISSIONDENIED {
				t.Errorf("code = %q, want PERMISSION_DENIED", code)
			}
			if len(env.threads.deletedThreads) != 0 {
				t.Error("権限が無いのにスレッドが消えている")
			}
		})
	}
}

// **無いスレッドは 404、二重削除も 404。**
func TestDeleteThread_NotFound(t *testing.T) {
	env := newDeleteEnv(t)

	if rec := env.del(t, "/threads/9999", true); rec.Code != http.StatusNotFound {
		t.Fatalf("存在しない: status = %d, want 404", rec.Code)
	}

	if rec := env.del(t, "/threads/1", true); rec.Code != http.StatusNoContent {
		t.Fatalf("1 回目: status = %d, want 204", rec.Code)
	}
	if rec := env.del(t, "/threads/1", true); rec.Code != http.StatusNotFound {
		t.Fatalf("2 回目: status = %d, want 404", rec.Code)
	}
}

// **未ログインは 401** (仕様書の security 宣言)。
//
// 匿名で立てたスレッドを匿名のまま消す手段はありません。
func TestDeleteThread_RequiresLogin(t *testing.T) {
	env := newDeleteEnv(t)

	for _, path := range []string{"/threads/1", "/threads/3"} {
		if rec := env.del(t, path, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, rec.Code)
		}
	}
	if len(env.threads.deletedThreads) != 0 {
		t.Error("未ログインなのにスレッドが消えている")
	}
}

// **自分のコメントは消せること。**
func TestDeleteComment_DeletesOwn(t *testing.T) {
	env := newDeleteEnv(t)

	rec := env.del(t, "/threads/1/comments/10", true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if !env.comments.deletedComments[10] {
		t.Error("コメントが論理削除されていない")
	}
}

// **他人と匿名のコメントは 403。**
func TestDeleteComment_RejectsOthersAndAnonymous(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"他人のコメント", "/threads/1/comments/11"},
		{"匿名のコメント", "/threads/1/comments/12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newDeleteEnv(t)

			rec := env.del(t, tc.path, true)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			if len(env.comments.deletedComments) != 0 {
				t.Error("権限が無いのにコメントが消えている")
			}
		})
	}
}

// **スレッド ID が違えば当たらないこと。**
//
// 主キーが (thread_id, id) なので、パーティションキーが違うと
// そもそも同じ行に到達しません。ここが 204 になるなら、
// クエリから thread_id が落ちています。
func TestDeleteComment_WrongThreadIsNotFound(t *testing.T) {
	env := newDeleteEnv(t)

	rec := env.del(t, "/threads/2/comments/10", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(env.comments.deletedComments) != 0 {
		t.Error("別スレッドの ID でコメントが消えている")
	}
}

// **未ログインは 401。**
func TestDeleteComment_RequiresLogin(t *testing.T) {
	env := newDeleteEnv(t)

	if rec := env.del(t, "/threads/1/comments/10", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// **Origin が無ければ csrfGuard が先に 403** (ADR 0013 決定 1)。
//
// DELETE も状態変更メソッドなので検証の対象です。
// **セッションを付けても通ってはいけません。**
func TestDelete_RequiresOrigin(t *testing.T) {
	env := newDeleteEnv(t)

	for _, path := range []string{"/threads/1", "/threads/1/comments/10"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: string(env.token)})

		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, rec.Code)
		}
	}
	if len(env.threads.deletedThreads) != 0 || len(env.comments.deletedComments) != 0 {
		t.Error("Origin が無いのに削除が通っている")
	}
}

// **ID の形式が合わなければ 400。**
//
// 仕様書の minimum: 1 が弾くので、ここは検証ミドルウェアの検査でもあります。
func TestDelete_RejectsInvalidIDs(t *testing.T) {
	env := newDeleteEnv(t)

	for _, path := range []string{"/threads/0", "/threads/1/comments/0", "/threads/-1"} {
		if rec := env.del(t, path, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// **モデレーターの削除とは別の経路であること。**
//
// 本人の削除は moderation_actions に記録しません ——
// 載せると記録の大半が通常の操作で埋まり、
// モデレーションの調査に使えなくなります (ADR 0011 決定 3 の趣旨)。
func TestDeleteThread_DoesNotRecordModerationAction(t *testing.T) {
	env := newDeleteEnv(t)

	if rec := env.del(t, "/threads/1", true); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	// moderation のフェイクはこの環境から触られない。
	// 記録されていれば、本人の削除が監査経路へ流れ込んでいることになる。
	if !env.threads.deletedThreads[1] {
		t.Fatal("前提が壊れている (削除されていない)")
	}
}
