package usecase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
)

var (
	actorPublic  = uuid.MustParse("01920000-0000-7000-8000-000000000042")
	targetPublic = uuid.MustParse("01920000-0000-7000-8000-000000000123")
)

func changeCmd(actor model.Actor) ChangeRoleCommand {
	return ChangeRoleCommand{
		Actor:          actor,
		ActorPublicID:  actorPublic,
		TargetPublicID: targetPublic,
		Role:           "moderator",
	}
}

// **admin だけがロールを変更できること** (ADR 0011 決定 1)。
//
// **モデレーターでは足りません。** 決定 1 の趣旨が
// 「投稿を消せる権限と、権限を配れる権限を分離する」ことなので、
// CanModerate で通してしまうと分離が消えます ——
// モデレーターを増やすと権限を配れる人間も増える形になります。
func TestChangeRole_RequiresAdmin(t *testing.T) {
	t.Parallel()

	// **CanModerate は true だが CanChangeRoles は false。**
	// 1 つの真偽値にまとめていたら、この区別を検査できません。
	moderatorOnly := model.Actor{UserID: 42, CanModerate: true}

	repo := &fakeRepo{}
	_, err := NewInteractor(repo).ChangeRole(context.Background(), changeCmd(moderatorOnly))
	if !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if repo.changedTo != nil {
		t.Error("権限が無いのにロールが変わっている")
	}
	if repo.recorded != nil {
		t.Error("権限が無いのに記録が書かれている")
	}
}

// **自分自身は対象にできないこと。**
//
// 最後の admin が自分を降格させると、誰もロールを配れなくなります。
// 復旧には DB を直接触るしかありません。
func TestChangeRole_RejectsSelf(t *testing.T) {
	t.Parallel()

	cmd := changeCmd(admin())
	cmd.TargetPublicID = actorPublic // 自分自身

	repo := &fakeRepo{}
	_, err := NewInteractor(repo).ChangeRole(context.Background(), cmd)
	if !errors.Is(err, apperr.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if repo.changedTo != nil {
		t.Error("自分のロールが変わっている")
	}
}

// **変更と記録が揃うこと** (決定 1 / 決定 3)。
func TestChangeRole_ChangesAndRecords(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	got, err := NewInteractor(repo).ChangeRole(context.Background(), changeCmd(admin()))
	if err != nil {
		t.Fatalf("ChangeRole が失敗した: %v", err)
	}

	if repo.changedTo == nil || *repo.changedTo != "moderator" {
		t.Errorf("渡されたロール = %v, want moderator", repo.changedTo)
	}
	if repo.changedPublicID == nil || *repo.changedPublicID != targetPublic {
		t.Errorf("渡された公開 ID = %v, want %v", repo.changedPublicID, targetPublic)
	}
	if got.Type != model.ActionChangeRole {
		t.Errorf("action = %q, want change_role", got.Type)
	}
	// **対象種別は操作から導く。** 削除と同じ扱いです。
	if got.Target != model.TargetUser {
		t.Errorf("target_type = %q, want user", got.Target)
	}
	// **内部 ID を書く。** 監査記録から users を辿るためです。
	if got.TargetID != "4242" {
		t.Errorf("target_id = %q, want 4242 (内部 ID)", got.TargetID)
	}
}

// **変更に失敗したら記録も残らないこと。**
//
// 分けると「権限が変わったのに誰がやったか分からない」状態が作れます。
func TestChangeRole_NoRecordWhenChangeFails(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{changeErr: apperr.ErrNotFound}
	_, err := NewInteractor(repo).ChangeRole(context.Background(), changeCmd(admin()))
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if repo.recorded != nil {
		t.Error("変更に失敗したのに記録が書かれている")
	}
}

// **最後の admin は降格させられないこと** (レビュー指摘)。
//
// 「自分自身を弾く」だけでは、admin 2 人が互いを同時に降格させたときに
// **両方が通って admin が 0 人になります。**
// admin の行をロックして数え、他に残ることを確かめます。
func TestChangeRole_KeepsAtLeastOneAdmin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		admins     int64
		targetRole string
		newRole    string
		wantErr    bool
	}{
		// 対象が最後の admin。降格させると 0 人になる。
		{"最後の admin の降格は弾く", 1, "admin", "user", true},
		// 他にも admin が居るので通る。
		{"他に admin が居れば降格できる", 2, "admin", "user", false},
		// 対象が admin でなければ、admin は減らない。
		{"admin でない相手の変更は通る", 1, "user", "moderator", false},
		// **昇格は数えない。** admin が減らないので待たせる理由がない。
		{"admin にする変更は数えない", 1, "user", "admin", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := &fakeRepo{admins: tc.admins, targetRole: tc.targetRole}
			cmd := changeCmd(admin())
			cmd.Role = tc.newRole

			_, err := NewInteractor(repo).ChangeRole(context.Background(), cmd)
			if tc.wantErr {
				// **422。** 入力の形は正しく、現在の状態と噛み合わないだけで、
				// 再試行しても解決しない (先に別の admin を作る必要がある)。
				if !errors.Is(err, apperr.ErrFailedPrecondition) {
					t.Fatalf("err = %v, want ErrFailedPrecondition", err)
				}
				if repo.changedTo != nil {
					t.Error("弾いたのにロールが変わっている")
				}
				if repo.recorded != nil {
					t.Error("弾いたのに記録が残っている")
				}
				return
			}
			if err != nil {
				t.Fatalf("ChangeRole が失敗した: %v", err)
			}
		})
	}
}

// **roleAdmin が users.role の CHECK 制約と揃っていること。**
//
// moderation は「ロールとは何か」を知らない立場なので文字列で持ちますが、
// **綴りがずれると「最後の admin を守る」判定が黙って効かなくなります**
// (対象が admin かどうかを判定できなくなる)。
func TestRoleAdmin_AgreesWithMigration(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, "apps/go-api/db/migrations/000003_add_user_role.up.sql"))
	if err != nil {
		t.Fatalf("000003 を読めない: %v", err)
	}
	m := regexp.MustCompile(`CHECK \(role IN \(([^)]+)\)\)`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("000003 から role の CHECK 制約を読み取れなかった")
	}
	if !strings.Contains(m[1], "'"+roleAdmin+"'") {
		t.Errorf("roleAdmin = %q が DB の CHECK 制約 (%s) に無い", roleAdmin, m[1])
	}
}
