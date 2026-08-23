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
	//    **これだけでは足りません** (レビュー指摘)。admin が 2 人いるとき、
	//    互いを同時に降格させると両方が「対象は自分ではない」を通り、
	//    更新する行も別なのでロックも衝突せず、**両方コミットして
	//    admin が 0 人になります。** 残りは 4 で塞ぎます。
	//
	//    **ゼロ値も弾きます** (レビュー指摘)。ActorPublicID は Actor と
	//    別のフィールドなので、呼び出し側が埋め忘れても型では気づけません。
	//    忘れると比較が**常に偽**になり、admin が 2 人以上いれば
	//    ensureAdminRemains も通って、**自分を降格できてしまいます。**
	//    静かに素通りするより、ここで落とすほうが安全側になります。
	if cmd.ActorPublicID == uuid.Nil {
		return nil, fmt.Errorf("操作者の ID が不正です: %w", apperr.ErrInvalidArgument)
	}
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
		// 4. **admin を 0 人にしない。**
		//
		//    admin の行をロックしてから数えるので、同時に走った降格は
		//    直列化されます —— 後から来たほうは、先のコミット後に
		//    数え直して 1 人だと分かります。
		//
		//    ロックを取るのは降格のときだけ。昇格 (admin にする) では
		//    admin が減らないので、admin 全体を待たせる理由がありません。
		if err := ensureAdminRemains(ctx, tx, cmd); err != nil {
			return err
		}

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
		// **target_id にしない。** 通報側が int64 で使っており、
		// こちらは型が混在するため文字列になる (model.Action の説明)。
		// 同名で型が違う列は Athena に宣言できない (ADR 0010 の 4-2)。
		slog.String("target_ref", recorded.TargetID),
		slog.String("new_role", cmd.Role),
	)
	return recorded, nil
}

// roleAdmin は「権限を配れる」ロールの値です。
//
// **user モジュールの定数は使いません** (ADR 0011 のモジュール構成)。
// moderation はロールを文字列として運ぶだけで、
// 「ロールとは何か」を知る立場にありません。
//
// 突き合わせは role_test.go が users.role の CHECK 制約を読んで行います。
const roleAdmin = "admin"

// ensureAdminRemains は、この変更のあとも admin が 1 人以上残ることを確かめます。
//
// **降格のときだけ効きます。** admin にする変更では admin が減りません。
//
// 順序に意味があります。
//
//  1. admin の行をロックして数える —— 同時降格を直列化する
//  2. 対象の現在のロールを読む      —— ロック後の値でないと意味がない
//  3. 対象が admin で、他に居なければ弾く
//
// 1 と 2 を入れ替えると、読んだ直後に他の admin が降格する窓が開きます。
func ensureAdminRemains(
	ctx context.Context, tx repository.Repository, cmd ChangeRoleCommand,
) error {
	if cmd.Role == roleAdmin {
		return nil
	}

	admins, err := tx.LockAdminsAndCount(ctx)
	if err != nil {
		return err
	}
	_, current, err := tx.FindRole(ctx, cmd.TargetPublicID)
	if err != nil {
		return err
	}
	if current != roleAdmin {
		return nil
	}

	// 対象を含めて 1 人 = 降格すると 0 人になる。
	if admins <= 1 {
		// **422 にします。** 入力の形は正しく、現在の状態と噛み合わないだけで、
		// 再試行しても解決しません (先に別の admin を作る必要がある)。
		return fmt.Errorf(
			"最後の管理者は降格できません。先に別の管理者を作ってください: %w",
			apperr.ErrFailedPrecondition)
	}
	return nil
}
