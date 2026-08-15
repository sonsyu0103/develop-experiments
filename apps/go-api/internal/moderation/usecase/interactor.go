// Package usecase はモデレーションのユースケースです。
package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/moderation/domain/model"
	"develop-experiments/apps/go-api/internal/moderation/domain/repository"
)

// maxReasonLength は理由の上限です。
//
// DB 側の CHECK 制約 (moderation_reason_length) と仕様書の maxLength と
// **同じ値**にしてください。3 か所が揃っていることは interactor_test.go が
// 実際にファイルを読んで検査します。
const maxReasonLength = 500

// DeleteCommand は削除操作 1 件ぶんの入力です。
type DeleteCommand struct {
	// Actor は操作する人です。**権限の判定はここだけを見ます。**
	Actor model.Actor

	// Action は delete_thread / delete_comment / delete_image のいずれかです。
	// change_role はこの経路では受け付けません。
	Action model.ActionType

	// TargetID は対象の ID の文字列表現です。
	// 解釈は Action によって変わります (整数 / UUID)。
	TargetID string

	// ThreadID は **delete_comment のときだけ必須**です。
	//
	// comments が thread_id による HASH パーティションで、
	// 主キーが (thread_id, id) であるためです。
	// 他の操作では無視します。
	ThreadID *int64

	// Reason は任意です。空白だけの文字列は「指定なし」に丸めます。
	Reason *string
}

// Interactor はモデレーション操作を実行し、記録します。
type Interactor struct {
	repo repository.Repository
}

// NewInteractor はインタラクタを生成します。
func NewInteractor(repo repository.Repository) *Interactor {
	return &Interactor{repo: repo}
}

// Delete は対象を論理削除し、その事実を記録します
// (docs/adr/0011-moderation.md 決定 2・3・5)。
//
// **検査の順序に意味があります。**
//
//  1. 権限        —— 対象を探す前に見る
//  2. 操作の種類  —— 削除以外をここで弾く
//  3. ID の形式   —— DB を引く前に弾く
//  4. 削除と記録  —— 同じトランザクション
//
// 1 を後ろに回すと、権限の無い利用者が 403 と 404 の差で
// **「その ID の対象が存在すること」を確かめられます**。
// 総当たりで存在する ID を列挙できるため、権限の検査は必ず先に置きます。
//
// 4 を分けると、片方だけ成功した状態が作れます ——
// 「消えていないのに記録がある」か「誰が消したか分からない」のどちらかで、
// どちらも監査記録としては破綻しています。
// ログでは代替できません (ADR 0010: ログは at-most-once であり欠落しうる)。
func (i *Interactor) Delete(ctx context.Context, cmd DeleteCommand) (*model.Action, error) {
	// 1. **権限。対象を探す前に見る。**
	if !cmd.Actor.CanModerate {
		return nil, fmt.Errorf("モデレーション権限がありません: %w", apperr.ErrPermissionDenied)
	}

	// 2. この経路が受け付けるのは削除だけ。
	if !cmd.Action.IsDelete() {
		return nil, fmt.Errorf("この操作は削除ではありません: %w", apperr.ErrInvalidArgument)
	}

	reason, err := normalizeReason(cmd.Reason)
	if err != nil {
		return nil, err
	}

	// 3. ID の形式。**DB を引く前に弾く。**
	//    ここを通してしまうと、形式の誤りが「対象なし」(404) に化けて、
	//    クライアントは ID を直す手がかりを得られません。
	target, err := parseTarget(cmd)
	if err != nil {
		return nil, err
	}

	action := &model.Action{
		ActorID:  cmd.Actor.UserID,
		Type:     cmd.Action,
		Target:   cmd.Action.TargetType(),
		TargetID: target.id,
		Reason:   reason,
	}

	// 4. **削除と記録を同じトランザクションで行う。**
	var recorded *model.Action
	if err := i.repo.WithinTx(ctx, func(tx repository.Repository) error {
		// 削除が 0 行なら apperr.ErrNotFound が返り、記録は書かれない。
		// 「実際には何も起きていない操作」を記録に残さないため。
		if delErr := target.delete(ctx, tx); delErr != nil {
			return delErr
		}
		var recErr error
		recorded, recErr = tx.RecordAction(ctx, action)
		return recErr
	}); err != nil {
		return nil, err
	}

	// **ログにも出すが、正はテーブル側** (ADR 0011 決定 3)。
	// 突き合わせができるよう、記録の ID を含める。
	slog.InfoContext(ctx, "moderation_action",
		slog.Int64("moderation_action_id", recorded.ID),
		slog.Int64("actor_id", recorded.ActorID),
		slog.String("action", string(recorded.Type)),
		slog.String("target_type", string(recorded.Target)),
		slog.String("target_id", recorded.TargetID),
	)
	return recorded, nil
}

// normalizeReason は理由を整えます。
func normalizeReason(raw *string) (*string, error) {
	return normalizeText(raw, maxReasonLength, "理由")
}

// normalizeText は任意入力の自由記述を整えます。
//
// 空白だけの文字列を nil に丸めるのは、UI の入力欄が
// 「未入力」を空文字として送ってくるためです。そのまま保存すると、
// 記録上は「値あり」なのに中身が無い行になります。
//
// **文字数で数えます。** バイト数だと日本語が 1/3 の長さで弾かれます。
// DB の CHECK 制約は char_length なので、そちらと同じ数え方に揃えます。
func normalizeText(raw *string, maxLength int, label string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil, nil
	}
	if len([]rune(trimmed)) > maxLength {
		return nil, fmt.Errorf("%sが長すぎます: %w", label, apperr.ErrInvalidArgument)
	}
	return &trimmed, nil
}

// target は解釈済みの操作対象です。
//
// **ID の解釈と削除の呼び分けを 1 か所に閉じます。** 分けると
// 「整数として解釈したのに画像の削除を呼ぶ」ような取り違えが書けてしまいます。
type target struct {
	// id は記録に残す文字列表現です。**入力をそのまま使いません** ——
	// 解釈した値を書き戻すので、"007" や大文字の UUID が
	// 正規化された形で記録されます。
	id string
	// delete は対象を削除します。0 行なら apperr.ErrNotFound です。
	delete func(context.Context, repository.Repository) error
}

// parseTarget は操作に応じて対象 ID を解釈します。
func parseTarget(cmd DeleteCommand) (target, error) {
	switch cmd.Action {
	case model.ActionDeleteThread:
		id, err := parseID(cmd.TargetID, "スレッド")
		if err != nil {
			return target{}, err
		}
		return target{
			id: strconv.FormatInt(id, 10),
			delete: func(ctx context.Context, tx repository.Repository) error {
				return tx.SoftDeleteThread(ctx, id)
			},
		}, nil

	case model.ActionDeleteComment:
		id, err := parseID(cmd.TargetID, "コメント")
		if err != nil {
			return target{}, err
		}
		// **スレッド ID が無ければここで弾く。**
		// 無いまま進めると、8 パーティションすべてを走査する
		// UPDATE になります (主キーの先頭列が thread_id のため)。
		if cmd.ThreadID == nil {
			return target{}, fmt.Errorf(
				"コメントの削除にはスレッド ID が必要です: %w", apperr.ErrInvalidArgument)
		}
		if *cmd.ThreadID < 1 {
			return target{}, fmt.Errorf("スレッド ID が不正です: %w", apperr.ErrInvalidArgument)
		}
		threadID := *cmd.ThreadID
		return target{
			// **記録にはスレッド ID も残す** (レビュー指摘)。
			//
			// コメント ID だけを書くと、moderation_actions_target_idx から
			// 辿った先で**この経路が避けたはずの 8 パーティション全走査**が
			// 必要になる。「削除するときは thread_id が要る」と言いながら、
			// 「何を削除したかの記録」からは落ちている状態だった。
			//
			// 復活や調査は記録を起点に始まるので、そこで毎回起きる。
			id: formatCommentTarget(threadID, id),
			delete: func(ctx context.Context, tx repository.Repository) error {
				return tx.SoftDeleteComment(ctx, threadID, id)
			},
		}, nil

	case model.ActionDeleteImage:
		// **uuid.Parse は緩い。** ハイフン無しや波括弧付きも受け付けます。
		// 記録には正規化した表現 (id.String()) を書くので、
		// 同じ画像への操作が 2 つの表記で残ることはありません。
		id, err := uuid.Parse(cmd.TargetID)
		if err != nil {
			return target{}, fmt.Errorf("画像 ID が不正です: %w", apperr.ErrInvalidArgument)
		}
		return target{
			id: id.String(),
			delete: func(ctx context.Context, tx repository.Repository) error {
				return tx.MarkImageDeleted(ctx, id)
			},
		}, nil

	default:
		// IsDelete() を通っていれば到達しません。
		return target{}, fmt.Errorf("この操作は削除ではありません: %w", apperr.ErrInvalidArgument)
	}
}

// CommentTargetSeparator は、コメントの記録で
// スレッド ID とコメント ID を区切る文字です。
//
// **`:` を選んでいます。** 10 進の整数にも UUID にも現れないため、
// 他の対象種別の target_id と取り違えようがありません。
const CommentTargetSeparator = ":"

// formatCommentTarget はコメントの target_id を組み立てます。
//
//	"{thread_id}:{comment_id}"
//
// **パーティションキーを記録に残すためです。** コメント ID だけでは
// 記録から対象を引くときに 8 パーティションすべてを走査することになり、
// この経路が API の形まで曲げて避けたものが、そのまま戻ってきます。
//
// BIGINT は最大 19 桁なので、区切りを含めても 39 文字。
// DB の CHECK 制約 (moderation_target_id_length、上限 64) に収まります。
func formatCommentTarget(threadID, commentID int64) string {
	return strconv.FormatInt(threadID, 10) + CommentTargetSeparator +
		strconv.FormatInt(commentID, 10)
}

// parseID は 10 進の整数 ID を解釈します。
//
// **0 と負数を弾きます。** IDENTITY 列は 1 から始まるので、
// そのまま渡しても「対象なし」になるだけですが、
// 400 で返したほうがクライアントは原因に辿り着けます。
func parseID(raw, label string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%s ID が不正です: %w", label, apperr.ErrInvalidArgument)
	}
	return id, nil
}
