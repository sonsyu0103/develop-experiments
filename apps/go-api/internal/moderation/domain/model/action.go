// Package model はモデレーションのドメインモデルです。
//
// **thread / comment / image のいずれにも属しません**
// (docs/adr/0011-moderation.md のモジュール構成)。
// 通報とモデレーション記録はどれかに寄せると他の 2 つへの参照が必要になり、
// モジュール境界が壊れます。そのため、この中に他モジュールの型は現れません
// —— 対象は ID (整数 / UUID) としてだけ扱います。
package model

import (
	"fmt"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
)

// ActionType はモデレーション操作の種類です。
//
// DB 側の CHECK 制約 (moderation_action_valid) と**必ず同じ 4 値**にしてください。
// 片方だけ増やすと、保存できない値をアプリが作るか、
// アプリが知らない値が DB に入ります (user の Role と同じ約束)。
type ActionType string

const (
	// ActionDeleteThread はスレッドの論理削除です。
	ActionDeleteThread ActionType = "delete_thread"
	// ActionDeleteComment はコメントの論理削除です。
	ActionDeleteComment ActionType = "delete_comment"
	// ActionDeleteImage は画像の削除です。
	//
	// 論理削除ではありますが、**ストレージの実体は消えます**
	// (ADR 0011 決定 5)。DB 行を残すのは、投稿から参照されているためと、
	// 「画像は削除されました」と「元から画像なし」を区別するためです。
	ActionDeleteImage ActionType = "delete_image"
	// ActionChangeRole はロールの変更です。
	//
	// **削除の API からは受け付けません。** 入力の形が違う (新しいロールが要る)
	// ため専用のエンドポイントが書きますが、記録先は同じテーブルです
	// (ADR 0011 決定 1「変更は監査記録に残す」)。
	ActionChangeRole ActionType = "change_role"
)

// AllActionTypes は定義済みの操作をすべて並べたものです。
//
// **定数を足したらここにも足してください。** ParseActionType はこれを見ます。
// この定数群 / DB の CHECK 制約 / 仕様書の enum が揃っていることは
// action_test.go が実際にファイルを読んで検査します。
var AllActionTypes = []ActionType{
	ActionDeleteThread, ActionDeleteComment, ActionDeleteImage, ActionChangeRole,
}

// ParseActionType は外部から来た文字列を ActionType にします。
//
// **知らない値はエラーにします。** 既定値へ丸めると、
// 記録されるべき操作が別の名前で残ります。
func ParseActionType(s string) (ActionType, error) {
	for _, a := range AllActionTypes {
		if ActionType(s) == a {
			return a, nil
		}
	}
	return "", fmt.Errorf("不正なモデレーション操作です: %w", apperr.ErrInvalidArgument)
}

// IsDelete は削除操作かを返します。
//
// 削除 API が受け付けてよいのはこちらだけです。
// **列挙で判定します。** 「change_role でなければ削除」と書くと、
// 将来 change_role 以外の非削除操作を足したときに黙って通ります。
func (a ActionType) IsDelete() bool {
	switch a {
	case ActionDeleteThread, ActionDeleteComment, ActionDeleteImage:
		return true
	case ActionChangeRole:
		return false
	default:
		return false
	}
}

// TargetType は操作の対象種別です。
//
// DB 側の CHECK 制約 (moderation_target_type_valid) と同じ 4 値です。
type TargetType string

const (
	// TargetThread はスレッドです。
	TargetThread TargetType = "thread"
	// TargetComment はコメントです。
	TargetComment TargetType = "comment"
	// TargetImage は画像です。
	TargetImage TargetType = "image"
	// TargetUser は利用者です (ロール変更の対象)。
	TargetUser TargetType = "user"
)

// TargetType は操作から対象種別を導きます。
//
// **リクエストから受け取りません。** 受け取ると `delete_thread` と `image` の
// ように食い違う組み合わせを表現でき、そのまま記録に残ります。
// 操作が決まれば対象種別は一意なので、導出する側に倒します。
func (a ActionType) TargetType() TargetType {
	switch a {
	case ActionDeleteThread:
		return TargetThread
	case ActionDeleteComment:
		return TargetComment
	case ActionDeleteImage:
		return TargetImage
	case ActionChangeRole:
		return TargetUser
	default:
		// ParseActionType を通っていれば到達しません。
		// 空文字を返すと DB の CHECK 制約が拒否するため、
		// 未知の操作が記録に残ることはありません。
		return ""
	}
}

// Actor はモデレーション操作の実行者です。
//
// **user モジュールの型を持ち込みません** (ADR 0011 のモジュール構成)。
// 「ロールとは何か」は user モジュールの関心事であり、
// こちらが知る必要があるのは「この人は他人の投稿を消してよいか」だけです。
// 判定そのものは usermodel.Role.CanModerate() が持ちます。
type Actor struct {
	// UserID は内部 ID (users.id) です。moderation_actions.actor_id に入ります。
	//
	// **公開 ID (public_id) ではありません。** 監査記録は外部キーで
	// users を指すため、内部 ID が要ります。
	UserID int64

	// CanModerate は他人・匿名の投稿を削除してよいかです。
	//
	// **呼び出し側が Role から解決して渡します。** ここを false のまま
	// 呼ぶと、インタラクタが対象を探す前に弾きます。
	CanModerate bool
}

// Action は記録された 1 件のモデレーション操作です。
type Action struct {
	ID      int64
	ActorID int64
	Type    ActionType
	Target  TargetType
	// TargetID は対象の ID です。**型が混在するため文字列です**
	// (threads / comments は BIGINT、images は UUID)。
	// ADR 0003 の未決 #11 が「用途ごとに使い分ける」決定になったため、
	// この混在は解消されません。
	TargetID string
	// Reason は削除の理由です。**任意**なので nil になりえます。
	Reason    *string
	CreatedAt time.Time
}
