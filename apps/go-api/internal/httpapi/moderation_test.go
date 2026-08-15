package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	moderationmodel "develop-experiments/apps/go-api/internal/moderation/domain/model"
	moderationrepo "develop-experiments/apps/go-api/internal/moderation/domain/repository"
	moderationusecase "develop-experiments/apps/go-api/internal/moderation/usecase"
	"develop-experiments/apps/go-api/internal/pagination"
	threadmodel "develop-experiments/apps/go-api/internal/thread/domain/model"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
)

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

// fakeModerationRepo は削除対象の生存状態と、書かれた記録を持ちます。
//
// **削除は「生きているものだけ」を消します。** 実装側の
// 「UPDATE ... WHERE deleted_at IS NULL が 0 行なら ErrNotFound」を
// フェイク側でも再現しないと、二重削除の検査が通ってしまいます。
type fakeModerationRepo struct {
	aliveThreads  map[int64]bool
	aliveComments map[[2]int64]bool
	aliveImages   map[uuid.UUID]bool

	actions []moderationmodel.Action
	nextID  int64

	// txDepth は WithinTx の入れ子を検出するために数えます。
	txDepth int
	// recordErr が非 nil なら RecordAction がそれを返します。
	// **記録の失敗で削除まで巻き戻ることを検査する**ために使います。
	recordErr error

	// users は公開 ID -> 内部 ID。ロール変更の対象になります。
	users map[uuid.UUID]int64
	// roles は変更後のロール。**書かれたことを検査する**ために持ちます。
	roles map[uuid.UUID]string
}

var _ moderationrepo.Repository = (*fakeModerationRepo)(nil)

func newFakeModerationRepo() *fakeModerationRepo {
	return &fakeModerationRepo{
		aliveThreads:  map[int64]bool{},
		aliveComments: map[[2]int64]bool{},
		aliveImages:   map[uuid.UUID]bool{},
		users:         map[uuid.UUID]int64{},
		roles:         map[uuid.UUID]string{},
	}
}

// WithinTx は「エラーなら巻き戻す」だけを再現します。
//
// 実 DB のトランザクションはここでは再現できないので、
// **fn が失敗したら控えを書き戻す**形にしています。
// 再現しないと「削除は成功したが記録に失敗した」ときに
// 削除だけが残る実装を通してしまいます。
func (f *fakeModerationRepo) WithinTx(
	ctx context.Context, fn func(moderationrepo.Repository) error,
) error {
	if f.txDepth > 0 {
		return fmt.Errorf("トランザクションの入れ子は作れません")
	}
	f.txDepth++
	defer func() { f.txDepth-- }()

	// 控え (シャローコピーで足りる。中身の型はすべて値)。
	threads := maps(f.aliveThreads)
	comments := mapsPair(f.aliveComments)
	images := mapsUUID(f.aliveImages)
	actions := append([]moderationmodel.Action(nil), f.actions...)
	nextID := f.nextID

	if err := fn(f); err != nil {
		f.aliveThreads, f.aliveComments, f.aliveImages = threads, comments, images
		f.actions, f.nextID = actions, nextID
		return err
	}
	return nil
}

func maps(src map[int64]bool) map[int64]bool {
	dst := make(map[int64]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func mapsPair(src map[[2]int64]bool) map[[2]int64]bool {
	dst := make(map[[2]int64]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func mapsUUID(src map[uuid.UUID]bool) map[uuid.UUID]bool {
	dst := make(map[uuid.UUID]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (f *fakeModerationRepo) RecordAction(
	_ context.Context, a *moderationmodel.Action,
) (*moderationmodel.Action, error) {
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	f.nextID++
	saved := *a
	saved.ID = f.nextID
	saved.CreatedAt = time.Unix(1_700_000_000, 0).UTC()
	f.actions = append(f.actions, saved)
	return &saved, nil
}

func (f *fakeModerationRepo) SoftDeleteThread(_ context.Context, id int64) error {
	if !f.aliveThreads[id] {
		return apperr.ErrNotFound
	}
	f.aliveThreads[id] = false
	return nil
}

func (f *fakeModerationRepo) SoftDeleteComment(_ context.Context, threadID, id int64) error {
	key := [2]int64{threadID, id}
	if !f.aliveComments[key] {
		return apperr.ErrNotFound
	}
	f.aliveComments[key] = false
	return nil
}

// ChangeRole はロール変更を記録します。
//
// **対象の生存を再現します。** 常に成功するフェイクにすると、
// 404 の経路が「実装したつもり」で通ってしまいます。
func (f *fakeModerationRepo) ChangeRole(
	_ context.Context, publicID uuid.UUID, role string,
) (int64, error) {
	id, ok := f.users[publicID]
	if !ok {
		return 0, apperr.ErrNotFound
	}
	if f.roles == nil {
		f.roles = map[uuid.UUID]string{}
	}
	f.roles[publicID] = role
	return id, nil
}

func (f *fakeModerationRepo) MarkImageDeleted(_ context.Context, id uuid.UUID) error {
	if !f.aliveImages[id] {
		return apperr.ErrNotFound
	}
	f.aliveImages[id] = false
	return nil
}

// ---------------------------------------------------------------------------
// 通報のフェイク
// ---------------------------------------------------------------------------

// fakeReportRepo は積まれた通報を覚えます。
//
// **一意制約を再現します。** 同じ (reporter, target) の 2 回目は
// created = false になる —— ここを常に true にすると、
// 「重複通報が 200 になる」検査が何も見ていないことになります。
type fakeReportRepo struct {
	reports []moderationmodel.Report
	nextID  int64

	// aliveThreads / aliveComments は通報対象の生存です。
	aliveThreads  map[int64]bool
	aliveComments map[[2]int64]bool
}

var (
	_ moderationrepo.ReportRepository       = (*fakeReportRepo)(nil)
	_ moderationrepo.TargetExistenceChecker = (*fakeReportRepo)(nil)
)

func newFakeReportRepo() *fakeReportRepo {
	return &fakeReportRepo{
		aliveThreads:  map[int64]bool{},
		aliveComments: map[[2]int64]bool{},
	}
}

func (f *fakeReportRepo) Create(
	_ context.Context, r *moderationmodel.Report,
) (*moderationmodel.Report, bool, error) {
	for i := range f.reports {
		e := &f.reports[i]
		if e.ReporterID == r.ReporterID && e.Target == r.Target && e.TargetID == r.TargetID {
			cp := *e
			return &cp, false, nil
		}
	}
	f.nextID++
	saved := *r
	saved.ID = f.nextID
	saved.Status = moderationmodel.ReportOpen
	saved.CreatedAt = time.Unix(1_700_000_000, 0).UTC()
	f.reports = append(f.reports, saved)
	cp := saved
	return &cp, true, nil
}

func (f *fakeReportRepo) List(
	_ context.Context, status moderationmodel.ReportStatus, page pagination.Page,
) ([]moderationmodel.Report, error) {
	out := make([]moderationmodel.Report, 0, len(f.reports))
	for _, r := range f.reports {
		if r.Status != status {
			continue
		}
		// **古い順 (id 昇順) で、カーソルより大きい id。**
		// 他の一覧と向きが逆なので、ここを取り違えると
		// 「次ページが常に空」になる。
		if c := page.CursorID(); c != nil && r.ID <= *c {
			continue
		}
		out = append(out, r)
		if int32(len(out)) == page.Size {
			break
		}
	}
	return out, nil
}

func (f *fakeReportRepo) Resolve(
	_ context.Context, id int64, status moderationmodel.ReportStatus, actorID int64,
) (*moderationmodel.Report, error) {
	for i := range f.reports {
		r := &f.reports[i]
		if r.ID != id || r.Status != moderationmodel.ReportOpen {
			continue
		}
		at := time.Unix(1_700_000_100, 0).UTC()
		r.Status, r.ResolvedAt, r.ResolvedBy = status, &at, &actorID
		cp := *r
		return &cp, nil
	}
	return nil, apperr.ErrNotFound
}

func (f *fakeReportRepo) ThreadExists(_ context.Context, id int64) (bool, error) {
	return f.aliveThreads[id], nil
}

func (f *fakeReportRepo) CommentExists(_ context.Context, threadID, id int64) (bool, error) {
	return f.aliveComments[[2]int64{threadID, id}], nil
}

// ---------------------------------------------------------------------------
// テスト環境
// ---------------------------------------------------------------------------

type moderationEnv struct {
	router  http.Handler
	repo    *fakeModerationRepo
	reports *fakeReportRepo
	token   usermodel.SessionToken
}

// newModerationEnv は指定したロールでログイン済みのルータを組み立てます。
func newModerationEnv(t *testing.T, role usermodel.Role) *moderationEnv {
	t.Helper()

	const token = usermodel.SessionToken("moderation-test-token")
	sessions := &fakeSessionRepo{
		liveToken: token,
		owner: usermodel.SessionOwner{
			ID: 42, PublicID: uuid.MustParse("01920000-0000-7000-8000-000000000042"),
			Email: "mod@example.com", DisplayName: "モデレーター", Role: role,
		},
	}

	repo := newFakeModerationRepo()
	reports := newFakeReportRepo()
	threads := &fakeThreadRepo{
		summaries: []threadmodel.Summary{
			{Thread: *threadmodel.Reconstruct(1, "スレッド", nil, nil, time.Unix(1, 0).UTC())},
		},
	}
	comments := &fakeCommentRepo{}

	router, err := NewRouter(Deps{
		Server: NewServer(
			threadusecase.NewThreadInteractor(threads, nil),
			commentusecase.NewCommentInteractor(comments, threads, nil),
			&fakePinger{},
			userusecase.NewSessionInteractor(sessions, nil),
			nil,
			nil,
			moderationusecase.NewInteractor(repo),
			moderationusecase.NewReportInteractor(reports, reports),
			config.AuthConfig{FrontendURL: "http://localhost:3000"},
		),
		AllowedOrigins: []string{testOrigin},
	})
	if err != nil {
		t.Fatalf("NewRouter が失敗した: %v", err)
	}
	return &moderationEnv{router: router, repo: repo, reports: reports, token: token}
}

// post は POST /moderation/actions を叩きます。
// withSession が false のときは Cookie を送りません。
func (e *moderationEnv) post(t *testing.T, body string, withSession bool) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodPost, "/moderation/actions", strings.NewReader(body))
	// **Origin が要る** (ADR 0013 決定 1)。付けないと csrfGuard が先に 403 にする。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	if withSession {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: string(e.token)})
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// 検査
// ---------------------------------------------------------------------------

// **モデレーターは匿名投稿も消せること** (ADR 0011 決定 2)。
//
// 消えたことと、記録が 1 件残ることの両方を見ます。
// 片方だけだと「消したが記録が無い」実装が通ります。
func TestCreateModerationAction_DeletesThreadAndRecords(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	env.repo.aliveThreads[7] = true

	rec := env.post(t, `{"action":"delete_thread","targetId":"7","reason":"荒らし"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	if env.repo.aliveThreads[7] {
		t.Error("スレッドが論理削除されていない")
	}
	if len(env.repo.actions) != 1 {
		t.Fatalf("記録が %d 件。1 件であるべき", len(env.repo.actions))
	}

	got := env.repo.actions[0]
	if got.ActorID != 42 {
		t.Errorf("actor_id = %d, want 42", got.ActorID)
	}
	if got.Type != moderationmodel.ActionDeleteThread {
		t.Errorf("action = %q, want delete_thread", got.Type)
	}
	// **対象種別はリクエストではなく操作から導く。**
	if got.Target != moderationmodel.TargetThread {
		t.Errorf("target_type = %q, want thread", got.Target)
	}
	if got.Reason == nil || *got.Reason != "荒らし" {
		t.Errorf("reason = %v, want 荒らし", got.Reason)
	}

	var body oapigen.ModerationAction
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("レスポンスの解析に失敗した: %v", err)
	}
	if body.TargetType != oapigen.ModerationTargetTypeThread {
		t.Errorf("レスポンスの targetType = %q, want thread", body.TargetType)
	}
}

// **画像の削除が status を書き替えること** (ADR 0011 決定 5)。
//
// ここが「モデレーターの削除 → 回収バッチ → S3」の入口になります。
// 回収バッチまで通す検査はスモークと実 DB 検査にあります。
func TestCreateModerationAction_DeletesImage(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	id := uuid.MustParse("018f2c00-0000-7000-8000-000000000001")
	env.repo.aliveImages[id] = true

	rec := env.post(t,
		`{"action":"delete_image","targetId":"018f2c00-0000-7000-8000-000000000001"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if env.repo.aliveImages[id] {
		t.Error("画像が削除扱いになっていない")
	}
	// 理由は任意。**省略したら null で記録される。**
	if got := env.repo.actions[0]; got.Reason != nil {
		t.Errorf("reason = %v, want nil", got.Reason)
	}
}

// **コメントの削除にはスレッド ID が要ること。**
//
// 無いまま通すと、8 パーティションすべてを走査する UPDATE になります
// (主キーが (thread_id, id) のため)。400 で弾きます。
func TestCreateModerationAction_CommentRequiresThreadID(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	env.repo.aliveComments[[2]int64{1, 10}] = true

	rec := env.post(t, `{"action":"delete_comment","targetId":"10"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !env.repo.aliveComments[[2]int64{1, 10}] {
		t.Error("弾いたのにコメントが消えている")
	}
	if len(env.repo.actions) != 0 {
		t.Error("弾いたのに記録が残っている")
	}

	// スレッド ID を付ければ通る。
	rec = env.post(t, `{"action":"delete_comment","targetId":"10","threadId":1}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if env.repo.aliveComments[[2]int64{1, 10}] {
		t.Error("コメントが論理削除されていない")
	}

	// **記録にもスレッド ID が残ること** (レビュー指摘)。
	//
	// コメント ID だけだと、moderation_actions_target_idx から辿った先で
	// 8 パーティション全走査になる —— この経路が API の形まで曲げて
	// 避けたものが、記録の側から戻ってくる。
	if got := env.repo.actions[0].TargetID; got != "1:10" {
		t.Errorf("target_id = %q, want \"1:10\" (パーティションキーが落ちている)", got)
	}
	var body oapigen.ModerationAction
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("レスポンスの解析に失敗した: %v", err)
	}
	if body.TargetId != "1:10" {
		t.Errorf("レスポンスの targetId = %q, want \"1:10\"", body.TargetId)
	}
}

// **一般利用者は 403** (ADR 0011 決定 1)。
//
// **対象が存在しない ID を送っても 403 であること**を同時に見ます。
// ここが 404 になると、権限の無い利用者が 403 と 404 の差で
// 「その ID の対象が存在すること」を確かめられます。
func TestCreateModerationAction_NonModeratorIsForbidden(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleUser)
	env.repo.aliveThreads[7] = true

	for _, targetID := range []string{"7", "999999"} {
		rec := env.post(t,
			fmt.Sprintf(`{"action":"delete_thread","targetId":%q}`, targetID), true)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("targetId=%s: status = %d, want 403 (body=%s)",
				targetID, rec.Code, rec.Body.String())
		}
		if code := decodeError(t, rec).Error.Code; code != oapigen.PERMISSIONDENIED {
			t.Errorf("targetId=%s: code = %q, want PERMISSION_DENIED", targetID, code)
		}
	}

	if !env.repo.aliveThreads[7] {
		t.Error("権限が無いのにスレッドが消えている")
	}
	if len(env.repo.actions) != 0 {
		t.Error("権限が無いのに記録が残っている")
	}
}

// **admin もモデレーションできること** (ADR 0011 決定 1 の権限表)。
func TestCreateModerationAction_AdminCanModerate(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleAdmin)
	env.repo.aliveThreads[7] = true

	if rec := env.post(t, `{"action":"delete_thread","targetId":"7"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
}

// **未ログインは 401** (仕様書の security 宣言)。
func TestCreateModerationAction_RequiresLogin(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)

	rec := env.post(t, `{"action":"delete_thread","targetId":"7"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

// **二重削除は 404 で、記録も増えないこと。**
//
// 記録が 2 件になると「1 回しか起きていない削除」が
// 2 回行われたように監査記録に残ります。
func TestCreateModerationAction_SecondDeleteIsNotFound(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	env.repo.aliveThreads[7] = true

	if rec := env.post(t, `{"action":"delete_thread","targetId":"7"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("1 回目: status = %d, want 201", rec.Code)
	}
	rec := env.post(t, `{"action":"delete_thread","targetId":"7"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("2 回目: status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(env.repo.actions) != 1 {
		t.Errorf("記録が %d 件。二重削除で増えてはいけない", len(env.repo.actions))
	}
}

// **記録に失敗したら削除も残らないこと。**
//
// 削除と記録が同じトランザクションであることの検査です。
// 分かれていると「誰が消したか分からない投稿」ができます
// —— ADR 0010 の理由でログでは代替できません。
func TestCreateModerationAction_RecordFailureRollsBackDelete(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	env.repo.aliveThreads[7] = true
	env.repo.recordErr = fmt.Errorf("記録に失敗しました")

	rec := env.post(t, `{"action":"delete_thread","targetId":"7"}`, true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if !env.repo.aliveThreads[7] {
		t.Error("記録に失敗したのにスレッドが消えたままになっている")
	}
}

// **仕様書の enum に無い操作は 400 であること。**
//
// change_role は DB の CHECK 制約には含まれますが、
// この経路では受け付けません (入力の形が違う)。
func TestCreateModerationAction_RejectsNonDeleteAction(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)

	rec := env.post(t, `{"action":"change_role","targetId":"1"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// **ID の形式が合わなければ 400 であること。**
//
// 404 に化けると、クライアントは ID を直す手がかりを得られません。
func TestCreateModerationAction_RejectsMalformedTargetID(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)

	cases := []struct{ name, body string }{
		{"スレッドに UUID", `{"action":"delete_thread","targetId":"018f2c00-0000-7000-8000-000000000001"}`},
		{"画像に整数", `{"action":"delete_image","targetId":"7"}`},
		{"0 は無効", `{"action":"delete_thread","targetId":"0"}`},
		{"負数は無効", `{"action":"delete_thread","targetId":"-1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.post(t, tc.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}
