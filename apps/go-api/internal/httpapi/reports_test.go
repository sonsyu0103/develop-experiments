package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	moderationmodel "develop-experiments/apps/go-api/internal/moderation/domain/model"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
)

// ---------------------------------------------------------------------------
// 通報 (ADR 0011 決定 4)
// ---------------------------------------------------------------------------

// do は任意のメソッドで叩きます。
func (e *moderationEnv) do(
	t *testing.T, method, path, body string, withSession bool,
) *httptest.ResponseRecorder {
	t.Helper()

	var r *strings.Reader
	if body == "" {
		r = strings.NewReader("")
	} else {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, r)
	req.Header.Set("Origin", testOrigin)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if withSession {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: string(e.token)})
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// **通報にはログインが要ること** (ADR 0011 決定 4)。
//
// 匿名で受け付けると、通報そのものが荒らしの手段になります。
func TestCreateReport_RequiresLogin(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleUser)
	env.reports.aliveThreads[7] = true

	rec := env.do(t, http.MethodPost, "/reports",
		`{"targetType":"thread","targetId":7,"reason":"spam"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(env.reports.reports) != 0 {
		t.Error("未ログインなのに通報が積まれている")
	}
}

// **一般利用者が通報できること。** モデレーターである必要はありません。
func TestCreateReport_PlainUserCanReport(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleUser)
	env.reports.aliveThreads[7] = true

	rec := env.do(t, http.MethodPost, "/reports",
		`{"targetType":"thread","targetId":7,"reason":"abuse","note":"連投"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.Report](t, rec)
	if got.Status != oapigen.ReportStatusOpen {
		t.Errorf("status = %q, want open", got.Status)
	}
	if got.Note == nil || *got.Note != "連投" {
		t.Errorf("note = %v, want 連投", got.Note)
	}
	// **通報者は返さない。** 見せると報復の材料になります。
	if strings.Contains(rec.Body.String(), "reporter") {
		t.Errorf("通報者が漏れている: %s", rec.Body.String())
	}
}

// **同じ対象への 2 回目は 200 で、エラーではないこと。**
//
// 一意制約は「1 人で通報数を積み上げてキューの優先度を操作させない」
// ためのもので、利用者に見せる失敗ではありません。
func TestCreateReport_DuplicateIsNotAnError(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleUser)
	env.reports.aliveThreads[7] = true

	body := `{"targetType":"thread","targetId":7,"reason":"spam"}`
	first := env.do(t, http.MethodPost, "/reports", body, true)
	if first.Code != http.StatusCreated {
		t.Fatalf("1 回目: status = %d, want 201", first.Code)
	}

	second := env.do(t, http.MethodPost, "/reports", body, true)
	if second.Code != http.StatusOK {
		t.Fatalf("2 回目: status = %d, want 200 (body=%s)", second.Code, second.Body.String())
	}

	// **最初の通報がそのまま返ること。** 新しい行が増えていない。
	if len(env.reports.reports) != 1 {
		t.Errorf("通報が %d 件。増えてはいけない", len(env.reports.reports))
	}
	if a, b := decodeJSON[oapigen.Report](t, first), decodeJSON[oapigen.Report](t, second); a.Id != b.Id {
		t.Errorf("2 回目の id = %d, want %d (最初の通報を返すべき)", b.Id, a.Id)
	}
}

// **存在しない対象は通報できないこと。**
//
// 確かめずに積むと、存在しない ID の通報でキューを埋められます。
func TestCreateReport_RejectsMissingTarget(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleUser)

	rec := env.do(t, http.MethodPost, "/reports",
		`{"targetType":"thread","targetId":9999,"reason":"spam"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(env.reports.reports) != 0 {
		t.Error("存在しない対象の通報が積まれている")
	}
}

// **コメントの通報にはスレッド ID が要ること。**
//
// 主キーが (thread_id, id) なので、無いと存在確認の時点で
// 8 パーティションを走査します。
func TestCreateReport_CommentRequiresThreadID(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleUser)
	env.reports.aliveComments[[2]int64{1, 10}] = true

	rec := env.do(t, http.MethodPost, "/reports",
		`{"targetType":"comment","targetId":10,"reason":"spam"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	rec = env.do(t, http.MethodPost, "/reports",
		`{"targetType":"comment","targetId":10,"threadId":1,"reason":"spam"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	// **記録にもスレッド ID が残ること。** 無いとキューから対象を引けません。
	if got := decodeJSON[oapigen.Report](t, rec); got.ThreadId == nil || *got.ThreadId != 1 {
		t.Errorf("threadId = %v, want 1", got.ThreadId)
	}
}

// **スレッドの通報にスレッド ID は指定できないこと。**
//
// 受け取ると targetId と threadId が食い違う組み合わせが表現でき、
// DB の CHECK 制約に当たって 500 になります。手前で 400 にします。
func TestCreateReport_ThreadRejectsThreadID(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleUser)
	env.reports.aliveThreads[7] = true

	rec := env.do(t, http.MethodPost, "/reports",
		`{"targetType":"thread","targetId":7,"threadId":7,"reason":"spam"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 通報キュー
// ---------------------------------------------------------------------------

// **キューは moderator 以上だけが読めること。**
func TestListReports_RequiresModerator(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleUser)

	rec := env.do(t, http.MethodGet, "/moderation/reports", "", true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.PERMISSIONDENIED {
		t.Errorf("code = %q, want PERMISSION_DENIED", code)
	}
}

// **既定は未処理だけを、古い順に返すこと。**
//
// 古い順なのは、通報が滞留したときに最初に届いたものから
// 処理されるようにするためです。
func TestListReports_ReturnsOpenOldestFirst(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	seedReports(t, env, 3)

	// 1 件だけ処理済みにする。
	if rec := env.do(t, http.MethodPatch, "/moderation/reports/2",
		`{"status":"resolved"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("解決に失敗した: status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	rec := env.do(t, http.MethodGet, "/moderation/reports", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.ReportList](t, rec)
	var ids []int64
	for _, r := range got.Reports {
		ids = append(ids, r.Id)
	}
	// 2 は処理済みなので出ない。1 -> 3 の昇順。
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Errorf("ids = %v, want [1 3]", ids)
	}
}

// **カーソルが前へ進むこと。**
//
// キューは古い順なので、他の一覧と**カーソルの向きが逆**になります。
// ここを取り違えると「次ページが常に空」になります。
func TestListReports_CursorMovesForward(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	seedReports(t, env, 3)

	first := decodeJSON[oapigen.ReportList](t,
		env.do(t, http.MethodGet, "/moderation/reports?size=2", "", true))
	if len(first.Reports) != 2 || first.NextCursor == nil {
		t.Fatalf("1 ページ目 = %d 件 / next=%v", len(first.Reports), first.NextCursor)
	}

	second := decodeJSON[oapigen.ReportList](t,
		env.do(t, http.MethodGet, "/moderation/reports?size=2&cursor="+*first.NextCursor, "", true))
	if len(second.Reports) != 1 {
		t.Fatalf("2 ページ目 = %d 件, want 1", len(second.Reports))
	}
	if second.Reports[0].Id != 3 {
		t.Errorf("2 ページ目の id = %d, want 3", second.Reports[0].Id)
	}
	if second.NextCursor != nil {
		t.Errorf("最終ページなのに nextCursor がある: %v", second.NextCursor)
	}
}

// **処理済みにできること。投稿には触れないこと。**
func TestResolveReport_DoesNotTouchThePost(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	env.repo.aliveThreads[7] = true
	seedReports(t, env, 1)

	rec := env.do(t, http.MethodPatch, "/moderation/reports/1", `{"status":"rejected"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[oapigen.Report](t, rec); got.Status != oapigen.ReportStatusRejected {
		t.Errorf("status = %q, want rejected", got.Status)
	}

	// **通報を閉じただけで投稿は消えない。**
	// 「通報を却下する」と「投稿を消す」は別の判断です。
	if !env.repo.aliveThreads[7] {
		t.Error("通報の処理でスレッドが消えている")
	}
	if len(env.repo.actions) != 0 {
		t.Error("通報の処理が moderation_actions に記録されている")
	}
}

// **2 回目の処理は 404 であること。**
//
// 含めないと、既に処理済みの通報の resolved_by が上書きされます。
func TestResolveReport_SecondTimeIsNotFound(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	seedReports(t, env, 1)

	if rec := env.do(t, http.MethodPatch, "/moderation/reports/1",
		`{"status":"resolved"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("1 回目: status = %d, want 200", rec.Code)
	}
	rec := env.do(t, http.MethodPatch, "/moderation/reports/1", `{"status":"resolved"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("2 回目: status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// **未処理へ戻す経路は無いこと。**
func TestResolveReport_RejectsOpen(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleModerator)
	seedReports(t, env, 1)

	rec := env.do(t, http.MethodPatch, "/moderation/reports/1", `{"status":"open"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// seedReports は通報を n 件積みます (id は 1 から)。
func seedReports(t *testing.T, env *moderationEnv, n int) {
	t.Helper()

	for i := 1; i <= n; i++ {
		env.reports.aliveThreads[int64(i)] = true
		env.reports.reports = append(env.reports.reports, moderationmodel.Report{
			ID:         int64(i),
			ReporterID: int64(100 + i),
			Target:     moderationmodel.ReportTargetThread,
			TargetID:   int64(i),
			Reason:     moderationmodel.ReasonSpam,
			Status:     moderationmodel.ReportOpen,
		})
		env.reports.nextID = int64(i)
	}
}

// ---------------------------------------------------------------------------
// ロール変更 (ADR 0011 決定 1)
// ---------------------------------------------------------------------------

const roleTargetPublicID = "01920000-0000-7000-8000-000000000123"

// **admin だけが変更できること。** モデレーターでは足りません。
//
// 投稿を消せる権限と、権限を配れる権限を分離するのが決定 1 の趣旨です。
func TestChangeUserRole_RequiresAdmin(t *testing.T) {
	for _, role := range []usermodel.Role{usermodel.RoleUser, usermodel.RoleModerator} {
		t.Run(string(role), func(t *testing.T) {
			env := newModerationEnv(t, role)
			env.repo.users[uuid.MustParse(roleTargetPublicID)] = 123

			rec := env.do(t, http.MethodPatch, "/users/"+roleTargetPublicID+"/role",
				`{"role":"moderator"}`, true)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			if len(env.repo.roles) != 0 {
				t.Error("権限が無いのにロールが変わっている")
			}
		})
	}
}

// **admin は他人のロールを変更でき、記録が残ること** (決定 1 / 決定 3)。
func TestChangeUserRole_AdminChangesAndRecords(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleAdmin)
	target := uuid.MustParse(roleTargetPublicID)
	env.repo.users[target] = 123

	rec := env.do(t, http.MethodPatch, "/users/"+roleTargetPublicID+"/role",
		`{"role":"moderator","reason":"運営に協力してもらう"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if env.repo.roles[target] != "moderator" {
		t.Errorf("ロール = %q, want moderator", env.repo.roles[target])
	}

	if len(env.repo.actions) != 1 {
		t.Fatalf("記録が %d 件。1 件であるべき", len(env.repo.actions))
	}
	got := env.repo.actions[0]
	if got.Type != moderationmodel.ActionChangeRole {
		t.Errorf("action = %q, want change_role", got.Type)
	}
	if got.Target != moderationmodel.TargetUser {
		t.Errorf("target_type = %q, want user", got.Target)
	}
	// **内部 ID を書く。** 監査記録から users を辿るためです。
	if got.TargetID != "123" {
		t.Errorf("target_id = %q, want 123", got.TargetID)
	}

	var body oapigen.ModerationAction
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("レスポンスの解析に失敗した: %v", err)
	}
	if body.Action != oapigen.ModerationActionTypeChangeRole {
		t.Errorf("レスポンスの action = %q, want change_role", body.Action)
	}
	// **応答は公開 ID を返す** (レビュー指摘)。記録は内部 ID (123) だが、
	// API は内部 ID を出さない (ADR 0003 未決 #11)。
	// ここが内部 ID だと、管理画面は受け取った値を他の API へ渡せない。
	if body.TargetId != roleTargetPublicID {
		t.Errorf("レスポンスの targetId = %q, want %q (公開 ID)",
			body.TargetId, roleTargetPublicID)
	}
	if body.TargetId == "123" {
		t.Error("内部 ID が API に漏れている")
	}
}

// **最後の admin は降格させられないこと** (レビュー指摘)。
//
// 自分自身を弾くだけでは、admin 2 人が互いを同時に降格させたときに
// 両方が通って admin が 0 人になります。
func TestChangeUserRole_KeepsAtLeastOneAdmin(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleAdmin)
	target := uuid.MustParse(roleTargetPublicID)
	env.repo.users[target] = 123
	// **対象が唯一の admin。** 降格させると 0 人になる。
	env.repo.roleOf[target] = "admin"

	rec := env.do(t, http.MethodPatch, "/users/"+roleTargetPublicID+"/role",
		`{"role":"user"}`, true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.FAILEDPRECONDITION {
		t.Errorf("code = %q, want FAILED_PRECONDITION", code)
	}
	if len(env.repo.roles) != 0 {
		t.Error("弾いたのにロールが変わっている")
	}

	// もう 1 人 admin が居れば通る。
	other := uuid.MustParse("01920000-0000-7000-8000-000000000456")
	env.repo.users[other] = 456
	env.repo.roleOf[other] = "admin"

	rec = env.do(t, http.MethodPatch, "/users/"+roleTargetPublicID+"/role",
		`{"role":"user"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// **自分のロールは変更できないこと。**
//
// 最後の admin が自分を降格させると、誰もロールを配れなくなります。
func TestChangeUserRole_RejectsSelf(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleAdmin)
	// newModerationEnv のセッション所有者と同じ公開 ID。
	self := "01920000-0000-7000-8000-000000000042"
	env.repo.users[uuid.MustParse(self)] = 42

	rec := env.do(t, http.MethodPatch, "/users/"+self+"/role", `{"role":"user"}`, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(env.repo.roles) != 0 {
		t.Error("自分のロールが変わっている")
	}
	if len(env.repo.actions) != 0 {
		t.Error("弾いたのに記録が残っている")
	}
}

// **居ない利用者は 404。**
func TestChangeUserRole_NotFound(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleAdmin)

	rec := env.do(t, http.MethodPatch,
		"/users/01920000-0000-7000-8000-000000000999/role", `{"role":"user"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(env.repo.actions) != 0 {
		t.Error("変更できていないのに記録が残っている")
	}
}

// **Origin が無ければ csrfGuard が先に 403** (ADR 0013 決定 1)。
func TestReportsAndRole_RequireOrigin(t *testing.T) {
	env := newModerationEnv(t, usermodel.RoleAdmin)

	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/reports", `{"targetType":"thread","targetId":7,"reason":"spam"}`},
		{http.MethodPatch, "/moderation/reports/1", `{"status":"resolved"}`},
		{http.MethodPatch, "/users/" + roleTargetPublicID + "/role", `{"role":"user"}`},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path,
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: string(env.token)})

			rec := httptest.NewRecorder()
			env.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
	if len(env.reports.reports) != 0 || len(env.repo.roles) != 0 {
		t.Error("Origin が無いのに状態が変わっている")
	}
}
