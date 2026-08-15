package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
	"develop-experiments/apps/go-api/internal/moderation/domain/repository"
)

// ChangeRoleCommand はロール変更 1 件ぶんの入力です。
type ChangeRoleCommand struct {
	// Actor は操作する人です。**admin だけが実行できます。**
	Actor model.Actor
	// ActorPublicID は操作する人の公開 ID です。
	// **自分自身を対象にしていないか**の判定に使います。
	ActorPublicID uuid.UUID

	// TargetPublicID は対象の公開 ID です。
	// **内部 ID は受け取りません** (ADR 0003 未決 #11)。
	TargetPublicID uuid.UUID
	// Role は新しいロールです。値の妥当性は呼び出し側 (user モジュール) が
	// 解釈済みのものを文字列として渡します。
	Role string

	Reason *string
}

// ChangeRole は利用者のロールを変更し、その事実を記録します
// (docs/adr/0011-moderation.md 決定 1 / 決定 3)。
//
// **検査の順序は削除と同じです。**
//
//  1. 権限        —— 対象を探す前に見る
//  2. 自分自身か  —— 同上
//  3. 変更と記録  —— 同じトランザクション
//
// **`Interactor` に置いています。** ロール変更も
// `moderation_actions` に書く操作であり、記録の経路を分けると
// 「監査記録を書く場所が 2 か所ある」状態になります。
func (i *Interactor) ChangeRole(
	ctx context.Context, cmd ChangeRoleCommand,
) (*model.Action, error) {
	// 1. **admin だけ。** モデレーターでは足りません ——
	//    投稿を消せる権限と、権限を配れる権限を分離するのが決定 1 の趣旨です。
	if !cmd.Actor.CanChangeRoles {
		return nil, fmt.Errorf("ロールを変更する権限がありません: %w", apperr.ErrPermissionDenied)
	}

	// 2. **自分自身は対象にできない。**
	//
	//    最後の admin が自分を降格させると、誰もロールを配れなくなります。
	//    復旧には DB を直接触るか BOOTSTRAP_ADMIN_GOOGLE_SUB を
	//    設定し直して再ログインするしかありません。
	//
	//    「admin が何人いるか」を数えて最後の 1 人だけ止める形にはしません ——
	//    数えた直後に他の admin が降格する競合があり、
	//    **自分を触らせない**ほうが単純で確実です。
	if cmd.TargetPublicID == cmd.ActorPublicID {
		return nil, fmt.Errorf("自分のロールは変更できません: %w", apperr.ErrPermissionDenied)
	}
	if cmd.TargetPublicID == uuid.Nil {
		return nil, fmt.Errorf("対象の ID が不正です: %w", apperr.ErrInvalidArgument)
	}

	reason, err := normalizeReason(cmd.Reason)
	if err != nil {
		return nil, err
	}

	// 3. **変更と記録を同じトランザクションで行う。**
	//    分けると「権限が変わったのに誰がやったか分からない」状態が作れます。
	var recorded *model.Action
	if err := i.repo.WithinTx(ctx, func(tx repository.Repository) error {
		userID, changeErr := tx.ChangeRole(ctx, cmd.TargetPublicID, cmd.Role)
		if changeErr != nil {
			return changeErr
		}

		var recErr error
		recorded, recErr = tx.RecordAction(ctx, &model.Action{
			ActorID: cmd.Actor.UserID,
			Type:    model.ActionChangeRole,
			Target:  model.ActionChangeRole.TargetType(),
			// **内部 ID を書きます。** 監査記録から users を辿るためで、
			// 公開 ID だと結合にもう 1 段要ります。
			// 削除の対象と同じく、target_id は外に出す値ではありません。
			TargetID: strconv.FormatInt(userID, 10),
			Reason:   reason,
		})
		return recErr
	}); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "moderation_role_changed",
		slog.Int64("moderation_action_id", recorded.ID),
		slog.Int64("actor_id", recorded.ActorID),
		slog.String("target_id", recorded.TargetID),
		slog.String("new_role", cmd.Role),
	)
	return recorded, nil
}
