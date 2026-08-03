package httpapi

import (
	"fmt"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/pagination"
)

// toPage は仕様書から生成されたクエリパラメータを、
// ドメイン層のページ指定へ変換します。
//
// 型の解析 (文字列 → int64 / int32) と、仕様書に書いた範囲制約
// (minimum / maximum) の検証は、それぞれ生成コードと
// 仕様検証ミドルウェアが済ませています。
// ここでは「省略時の既定値」の適用と、最終的な整合性の確認だけを行います。
func toPage(cursor *int64, size *int32) (pagination.Page, error) {
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
