package usecase

import (
	"context"
	"errors"
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
