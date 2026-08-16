// Package pagination はキーセット (cursor) 方式のページ指定を表します。
//
// OFFSET 方式を採らない理由:
//   - OFFSET N は「読み飛ばす N 行を実際に読む」ため、深いページほど線形に遅くなる
//   - ページ送りの最中に新規行が挿入されると、行の重複や取りこぼしが起きる
//
// id は単調増加なので「id < cursor」で辿れば、索引だけで一定コストになります。
//
// カーソルは API では不透明トークン (base64url + JSON) として扱い、
// 内部表現である id を直接は晒しません。形式と検証の決定は ADR 0018、
// そもそも不透明にした理由は ADR 0006 を参照してください。
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
// フィールドを増やしても API 上の型 (文字列) は変わらないため、
// クライアントは影響を受けません。
type Cursor struct {
	// Version は形式のバージョン。復号時に検査します。
	Version int `json:"v"`
	// Sort は発行元の並び順です。**既定 (新着順) は空文字**にしてあり、
	// 並び順が 1 つだった頃に発行したトークンも読めます。
	//
	// **この値の一致を検査するのは、このパッケージではありません**
	// (ADR 0018「並び順を足すときの規則」)。どの並び順を要求されたかを
	// 知っているのは上の層だけで、ここに持ち込むと
	// ソート種別の一覧を下位層が知ることになります。
	//
	// 検査しないと何が起きるか:
	// **新着順が発行したトークン (ViewCount なし) を人気順に渡すと、
	// ViewCount がゼロ値に落ち、「閲覧数 0 の位置から」ページングが始まる。**
	// 400 も出ず、黙って誤ったページが返ります。
	Sort string `json:"s,omitempty"`
	// ID はこの値より小さい id を次に読む、という境界です。
	ID int64 `json:"id"`
	// ViewCount は人気順の第 1 キーです。新着順では nil になります。
	//
	// **ポインタなのは 0 と「無い」を区別するため。** 閲覧数 0 の
	// スレッドは普通に存在するので、ゼロ値では表せません。
	ViewCount *int64 `json:"vc,omitempty"`
}

// NewCursor は id からカーソルを組み立てます (新着順)。
func NewCursor(id int64) Cursor {
	return Cursor{Version: cursorVersion, ID: id}
}

// WithSort は並び順の識別子を付けたカーソルを返します。
//
// **識別子の文字列はこのパッケージが決めません。** 呼び出し側が
// 自分の並び順の語彙で渡します (上の Sort の説明を参照)。
func (c Cursor) WithSort(sort string) Cursor {
	c.Sort = sort
	return c
}

// WithViewCount は人気順の第 1 キーを付けたカーソルを返します。
func (c Cursor) WithViewCount(viewCount int64) Cursor {
	c.ViewCount = &viewCount
	return c
}

// Encode はカーソルを API に返す不透明トークンへ変換します。
//
// 署名は付けていません。改竄されても読めるのは「公開済みの一覧の別の位置」
// でしかなく、権限の昇格にはつながらないためです。
// トークンの目的は秘匿ではなく、内部表現を API の契約から切り離すことです。
//
// 現在の Cursor は json.Marshal が失敗しうる型 (chan, func, 循環参照) を
// 含まないため、エラーは発生しません。それでも返しているのは、
// このパッケージが「並び順を足すときにフィールドを増やす」前提で作られており、
// 握りつぶすと将来 Encode が空文字を返して
// "nextCursor": "" がクライアントへ流れるためです。
func (c Cursor) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("カーソルの符号化に失敗しました: %w", err)
	}
	return tokenEncoding.EncodeToString(b), nil
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
//
// 空文字を受け付けるのは、クライアントが「トークンが無ければ空文字」として
// ?cursor= を組み立てることがあるためです。
// 仕様書の pattern も空文字を許す形にしてあり、
// 「省略」と「空文字」はどちらも先頭ページになります。
// 片方だけを 400 にすると、初回ロードだけが失敗する形になります。
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

// CursorViewCount は人気順の境界値を返します。
// 先頭ページ、または新着順のカーソルでは nil です。
//
// **nil をそのまま SQL へ渡してよい形にしてあります。** 人気順のクエリは
// cursor_view_count が NULL なら先頭から読むので、
// 「新着順のトークンが混ざった」場合も先頭ページとして振る舞います。
// ただしそれは**握りつぶしではなく最後の砦**で、
// 混入は上の層が並び順の一致で弾きます (Cursor.Sort の説明)。
func (p Page) CursorViewCount() *int64 {
	if p.Cursor == nil || p.Cursor.ViewCount == nil {
		return nil
	}
	vc := *p.Cursor.ViewCount
	return &vc
}

// CursorSort は発行元の並び順を返します。先頭ページでは空文字です。
//
// **先頭ページと「新着順のトークン」は、どちらも空文字になります。**
// この 2 つを区別するには Cursor が nil かどうかを見てください。
//
// **一致の検査では区別が要ります。** 「どちらも新着順として扱ってよい」
// と考えると、人気順の 1 ページ目 (カーソル無し) が
// 「新着順のトークンが混ざった」と判定され、**必ず 400 になります**
// (実際にその形で作り、テストで捕まえました)。
func (p Page) CursorSort() string {
	if p.Cursor == nil {
		return ""
	}
	return p.Cursor.Sort
}

// NextToken は次ページ用のトークンを決めます。
//
// 「size 件ちょうど返ってきたら次ページがあるかもしれない」という判断なので、
// 最終ページがちょうど size 件だった場合、次ページが空になることがあります。
// 厳密にするには size+1 件取得して 1 件捨てる必要がありますが、
// 空ページを 1 回引く程度のコストなので、ここでは単純さを優先しています。
//
// 次ページがない場合は nil を返します。
func NextToken(lastID int64, returned int, size int32) (*string, error) {
	return NextTokenFor(NewCursor(lastID), returned, size)
}

// NextTokenFor は組み立て済みのカーソルから次ページ用のトークンを決めます。
//
// 並び順ごとにカーソルの中身が違うため、**「次ページがあるか」の判定だけを
// ここに残し、値の組み立ては呼び出し側に置いてあります。**
// ここで並び順ごとに分岐させると、このパッケージが
// ソート種別の一覧を知ることになります (Cursor.Sort の説明)。
func NextTokenFor(c Cursor, returned int, size int32) (*string, error) {
	if returned == 0 || returned != int(size) {
		return nil, nil
	}
	t, err := c.Encode()
	if err != nil {
		return nil, err
	}
	return &t, nil
}
