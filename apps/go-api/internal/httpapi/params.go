package httpapi

import (
	"fmt"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/pagination"
)

// toPage は仕様書から生成されたクエリパラメータを、
// ドメイン層のページ指定へ変換します。
//
// 型の解析 (文字列 → int32) と、仕様書に書いた制約
// (minimum / maximum / pattern / maxLength) の検証は、それぞれ生成コードと
// 仕様検証ミドルウェアが済ませています。
// ここでは「省略時の既定値」の適用と、最終的な整合性の確認だけを行います。
//
// cursor は不透明トークンなので、中身の復号と検証は pagination 側で行います。
// HTTP 層はトークンの形式を知りません。
func toPage(cursor *string, size *int32) (pagination.Page, error) {
	var s int32
	if size != nil {
		s = *size
	}
	return pagination.NewPage(cursor, s)
}

// validateThreadID はパス上のスレッド ID を検証します。
// 仕様書で minimum: 1 を宣言しているため通常はミドルウェアが弾きますが、
// 検証を外した場合でもドメインが壊れないよう二重に確認します。
func validateThreadID(id int64) error {
	if id <= 0 {
		return fmt.Errorf("threadId は正の整数である必要があります (got %d): %w",
			id, apperr.ErrInvalidArgument)
	}
	return nil
}

// validateCommentID はコメント ID を検査します。
//
// **仕様書の minimum: 1 が既に弾きますが、ここでも見ます。**
// 検証ミドルウェアを外した経路や、ハンドラを直接呼ぶテストから
// 0 や負数が入ると、DB を引いたうえで 404 になるだけになります
// (threadId と同じ扱い)。
func validateCommentID(id int64) error {
	if id <= 0 {
		return fmt.Errorf("commentId は正の整数である必要があります (got %d): %w",
			id, apperr.ErrInvalidArgument)
	}
	return nil
}
