package postgres

import (
	"time"

	"github.com/google/uuid"

	commentmodel "develop-experiments/apps/go-api/internal/comment/domain/model"
	threadmodel "develop-experiments/apps/go-api/internal/thread/domain/model"
)

// LEFT JOIN users で得た投稿者の列を、各モジュールの Author へ詰め替えます。
//
// thread と comment は互いの型を知らないため
// (docs/adr/0014-author-resolution.md)、Author 型はモジュールごとに存在します。
// **両方を知ってよいのはこのアダプタだけ**です
// (依存方向としては外側が内側を知る形で正しい)。
//
// 【匿名の判定に public_id を使う理由】
// users.public_id は NOT NULL なので、結合が成立していれば必ず値が入ります。
// display_name で判定すると、将来その列が NULL 許容になったときに
// 「投稿者が居るのに匿名として表示される」形で静かに壊れます。
//
// 退会済みの扱い (表示名の差し替え、public_id を返さない) は
// 各モジュールの NewAuthor が行います。ここでは判定しません。
//
// 【型が pgtype.UUID から *uuid.UUID に変わった】
// sqlc.yaml に NULL 許容 uuid の上書きを足したためです (000006 の副作用)。
// 判定は publicID.Valid から publicID == nil に変わりましたが、
// **意味は同じ**で「LEFT JOIN が成立したか」を見ています。

func toThreadAuthor(
	publicID *uuid.UUID, displayName, avatarURL *string, deletedAt *time.Time,
) *threadmodel.Author {
	if publicID == nil {
		return nil
	}
	return threadmodel.NewAuthor(*publicID, derefString(displayName), avatarURL, deletedAt)
}

func toCommentAuthor(
	publicID *uuid.UUID, displayName, avatarURL *string, deletedAt *time.Time,
) *commentmodel.Author {
	if publicID == nil {
		return nil
	}
	return commentmodel.NewAuthor(*publicID, derefString(displayName), avatarURL, deletedAt)
}

// derefString は LEFT JOIN で NULL 許容になった列を読み出します。
// public_id が入っている時点で display_name も必ず入っているため、
// ここに来る nil は理屈上ありません。落とさずに空文字にします。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
