// Package pagination はキーセット (cursor) 方式のページ指定を表します。
//
// OFFSET 方式を採らない理由:
//   - OFFSET N は「読み飛ばす N 行を実際に読む」ため、深いページほど線形に遅くなる
//   - ページ送りの最中に新規行が挿入されると、行の重複や取りこぼしが起きる
//
// id は単調増加なので「id < cursor」で辿れば、索引だけで一定コストになります。
//
// カーソルは API では不透明トークン (base64url + JSON) として扱い、
// 内部表現である id を直接は晒しません。理由は ADR 0006 を参照してください。
// 人気順では並び順のキーが (view_count, id) の複合キーになるため、
// 「カーソル = id」を公開したままでは並び順を増やすたびに API が壊れます。
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"develop-experiments/apps/go-api/internal/apperr"
)

const (
	// DefaultSize は size が指定されなかった場合の件数です。
	DefaultSize int32 = 20
	// MaxSize は 1 リクエストで返す最大件数です。
	// 上限を設けないと、1 リクエストで全件走査させる攻撃を許してしまいます。
	MaxSize int32 = 100

	// maxTokenLen は復号を試みるトークンの最大長です。
	// 仕様書側でも maxLength を宣言していますが、検証を外した場合でも
	// 巨大な文字列の復号に付き合わされないよう二重に確認します。
	maxTokenLen = 256

	// cursorVersion は現在のトークン形式のバージョンです。
	// 互換性のない変更をしたらここを上げ、古いトークンは復号を拒否します。
	// (ページ送りの途中で失効するだけなので、実害は「先頭に戻る」程度)
	cursorVersion = 1
)

// tokenEncoding はクエリ文字列に載せるため、URL 安全かつパディングなしにします。
// パディングの '=' は %3D にエスケープされ、生の URL が読みにくくなるためです。
var tokenEncoding = base64.RawURLEncoding

// Cursor は「どの行の続きから読むか」を表します。
//
// 現在の並び順は id 降順だけなので中身は id 1 つですが、
// 人気順を追加すると view_count が加わります。フィールドを増やしても
// API 上の型 (文字列) は変わらないため、クライアントは影響を受けません。
type Cursor struct {
	// Version は形式のバージョン。復号時に検査します。
	Version int `json:"v"`
	// ID はこの値より小さい id を次に読む、という境界です。
	ID int64 `json:"id"`
}

// NewCursor は id からカーソルを組み立てます。
func NewCursor(id int64) Cursor {
	return Cursor{Version: cursorVersion, ID: id}
}

// Encode はカーソルを API に返す不透明トークンへ変換します。
//
// 署名は付けていません。改竄されても読めるのは「公開済みの一覧の別の位置」
// でしかなく、権限の昇格にはつながらないためです。
// トークンの目的は秘匿ではなく、内部表現を API の契約から切り離すことです。
func (c Cursor) Encode() string {
	// Cursor は json.Marshal が失敗しうる型 (chan, func, 循環参照) を
	// 含まないため、ここでエラーは発生しません。
	b, _ := json.Marshal(c)
	return tokenEncoding.EncodeToString(b)
}

// DecodeCursor は不透明トークンをカーソルへ戻します。
// 形式が壊れている場合は apperr.ErrInvalidArgument を返します。
//
// 復号できない理由 (base64 が壊れている / JSON が壊れている / 版が違う) を
// クライアントへ細かく伝えても、トークンは自力で作るものではないため
// 対処は「先頭ページから取り直す」の一手しかありません。
// メッセージはまとめて 1 種類にします。
func DecodeCursor(token string) (Cursor, error) {
	if len(token) > maxTokenLen {
		return Cursor{}, fmt.Errorf("cursor が長すぎます (%d 文字): %w", len(token), apperr.ErrInvalidArgument)
	}

	raw, err := tokenEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, invalidCursor()
	}

	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, invalidCursor()
	}
	if c.Version != cursorVersion {
		return Cursor{}, invalidCursor()
	}
	if c.ID <= 0 {
		return Cursor{}, invalidCursor()
	}
	return c, nil
}

func invalidCursor() error {
	return fmt.Errorf("cursor が不正です。先頭ページから取得し直してください: %w", apperr.ErrInvalidArgument)
}

// Page は「cursor より前の行を size 件」という取得範囲を表します。
// Cursor が nil の場合は先頭ページを意味します。
type Page struct {
	Cursor *Cursor
	Size   int32
}

// NewPage はページ指定を検証して組み立てます。
// token が nil または空文字の場合は先頭ページになります。
// size が 0 以下の場合は DefaultSize を使います。
func NewPage(token *string, size int32) (Page, error) {
	if size <= 0 {
		size = DefaultSize
	}
	if size > MaxSize {
		return Page{}, fmt.Errorf("size は %d 以下である必要があります (got %d): %w", MaxSize, size, apperr.ErrInvalidArgument)
	}

	if token == nil || *token == "" {
		return Page{Size: size}, nil
	}

	c, err := DecodeCursor(*token)
	if err != nil {
		return Page{}, err
	}
	return Page{Cursor: &c, Size: size}, nil
}

// CursorID は SQL に渡す境界値を返します。先頭ページでは nil です。
//
// 永続化層はトークンの形式を知る必要がないため、ここで剥がします。
func (p Page) CursorID() *int64 {
	if p.Cursor == nil {
		return nil
	}
	id := p.Cursor.ID
	return &id
}

// NextToken は次ページ用のトークンを決めます。
//
// 「size 件ちょうど返ってきたら次ページがあるかもしれない」という判断なので、
// 最終ページがちょうど size 件だった場合、次ページが空になることがあります。
// 厳密にするには size+1 件取得して 1 件捨てる必要がありますが、
// 空ページを 1 回引く程度のコストなので、ここでは単純さを優先しています。
//
// 次ページがない場合は nil を返します。
func NextToken(lastID int64, returned int, size int32) *string {
	if returned == 0 || returned != int(size) {
		return nil
	}
	t := NewCursor(lastID).Encode()
	return &t
}
