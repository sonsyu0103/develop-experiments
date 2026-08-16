package model

import (
	"fmt"
	"strings"

	"develop-experiments/apps/go-api/internal/apperr"
)

// ListOrder は一覧の並び順です (docs/adr/0006-view-count-and-popularity.md)。
//
// **文字列をここで解釈するのは ParseSearchQuery と同じ形です。**
// HTTP 層にドメインの語彙を持ち込まないためで、
// 仕様書の enum とこの型が対になります。
type ListOrder string

const (
	// ListOrderNew は新着順 (id の降順) です。**既定。**
	ListOrderNew ListOrder = "new"
	// ListOrderPopular は閲覧数の多い順です。
	//
	// 並びのキーは (view_count, id) の複合キーになります。
	// id を第 2 キーに入れるのは、閲覧数が同値のスレッドが必ず存在し、
	// 同値の並びが不定だとページ境界で行が重複・欠落するためです。
	ListOrderPopular ListOrder = "popular"
)

// listOrders は選べる並び順です。
var listOrders = map[ListOrder]bool{
	ListOrderNew:     true,
	ListOrderPopular: true,
}

// ParseListOrder は sort パラメータを解釈します。
// nil または空文字は既定 (新着順) になります。
//
// **未知の値は既定に落とさずエラーにします。** 綴りを間違えたときに
// 黙って新着順で返すと、「人気順で並べたつもりの新着順」が
// 利用者にもこちらにも見えないまま流れます。
//
// 空文字を許すのは cursor と同じ理由です —— クライアントが
// `?sort=${sort ?? ”}` と組み立てたときに初回ロードだけ 400 になるのを
// 避けるためで、仕様書側も省略と空文字を同じ扱いにしています。
func ParseListOrder(raw *string) (ListOrder, error) {
	if raw == nil || *raw == "" {
		return ListOrderNew, nil
	}

	order := ListOrder(strings.TrimSpace(strings.ToLower(*raw)))
	if !listOrders[order] {
		return "", fmt.Errorf(
			"sort が不正です (got %q, 選べるのは new / popular): %w", *raw, apperr.ErrInvalidArgument)
	}
	return order, nil
}

// CursorSort はカーソルに埋める並び順の識別子を返します。
//
// **既定 (新着順) は空文字にします。** 並び順が 1 つだった頃に発行した
// トークンと同じ形になり、古いトークンをそのまま読めます
// (ADR 0018「並び順を足すときの規則」)。
func (o ListOrder) CursorSort() string {
	if o == ListOrderNew {
		return ""
	}
	return string(o)
}
