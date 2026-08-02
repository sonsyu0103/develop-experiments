// Package pagination はキーセット (cursor) 方式のページ指定を表します。
//
// OFFSET 方式を採らない理由:
//   - OFFSET N は「読み飛ばす N 行を実際に読む」ため、深いページほど線形に遅くなる
//   - ページ送りの最中に新規行が挿入されると、行の重複や取りこぼしが起きる
//
// id は単調増加なので「id < cursor」で辿れば、索引だけで一定コストになります。
package pagination

import (
	"fmt"

	"develop-experiments/apps/go-api/internal/apperr"
)

const (
	// DefaultSize は size が指定されなかった場合の件数です。
	DefaultSize int32 = 20
	// MaxSize は 1 リクエストで返す最大件数です。
	// 上限を設けないと、1 リクエストで全件走査させる攻撃を許してしまいます。
	MaxSize int32 = 100
)

// Page は「cursor より前の行を size 件」という取得範囲を表します。
// Cursor が nil の場合は先頭ページを意味します。
type Page struct {
	Cursor *int64
	Size   int32
}

// NewPage はページ指定を検証して組み立てます。
// size が 0 以下の場合は DefaultSize を使います。
func NewPage(cursor *int64, size int32) (Page, error) {
	if cursor != nil && *cursor <= 0 {
		return Page{}, fmt.Errorf("cursor は正の整数である必要があります (got %d): %w", *cursor, apperr.ErrInvalidArgument)
	}
	if size <= 0 {
		size = DefaultSize
	}
	if size > MaxSize {
		return Page{}, fmt.Errorf("size は %d 以下である必要があります (got %d): %w", MaxSize, size, apperr.ErrInvalidArgument)
	}
	return Page{Cursor: cursor, Size: size}, nil
}
