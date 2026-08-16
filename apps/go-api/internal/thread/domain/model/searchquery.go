package model

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"develop-experiments/apps/go-api/internal/apperr"
)

// SearchQueryMaxLength は検索語の最大文字数です。
// タイトルの上限 (TitleMaxLength) と同じ値にしてあります ——
// **これより長い検索語はどのタイトルにも一致しません。**
// 上限が無いと、長い文字列を投げるだけでトライグラムを大量に生成させられます。
const SearchQueryMaxLength = TitleMaxLength

// SearchQuery はスレッド検索の検索語です (docs/adr/0012-search.md)。
//
// **値オブジェクトにしているのは、正規化の規則を 1 か所に閉じるため**です。
// 「前後の空白を落とす」「空なら絞り込まない」を呼び出し側それぞれで
// 書くと、HTTP 層とテストで規則がずれます。
//
// **LIKE のエスケープはここでは行いません。** `%` や `_` がワイルドカードに
// なるのは PostgreSQL の LIKE の都合であって、検索語そのものの性質では
// ないためです。エスケープは永続化層が行います。
type SearchQuery struct {
	// keyword は正規化済みの検索語です。空にはなりません。
	keyword string
}

// ParseSearchQuery はクエリパラメータの値を検索語に変換します。
//
// **空白だけの検索語は「指定なし」です。** その場合は (nil, nil) を返し、
// 呼び出し側は絞り込みのない一覧を返します。400 にしないのは、
// 検索欄を空のまま送信したフォームが弾かれるのを避けるためで、
// カーソルに空文字を許しているのと同じ理由になります (ADR 0018)。
//
// raw が nil の場合も「指定なし」です。
func ParseSearchQuery(raw *string) (*SearchQuery, error) {
	if raw == nil {
		return nil, nil
	}

	// TrimSpace は unicode.IsSpace で判定するため、
	// 全角スペース (U+3000) もここで落ちます。
	keyword := strings.TrimSpace(*raw)
	if keyword == "" {
		return nil, nil
	}

	// 仕様書の maxLength が先に弾きますが、検証ミドルウェアを外した経路や
	// ハンドラを直接呼ぶテストのために、ここでも見ます (validateThreadID と同じ)。
	// バイト数ではなく文字数で数えるのは、仕様書の maxLength と揃えるためです。
	if n := utf8.RuneCountInString(keyword); n > SearchQueryMaxLength {
		return nil, fmt.Errorf(
			"検索語が長すぎます (%d 文字, 上限 %d 文字): %w",
			n, SearchQueryMaxLength, apperr.ErrInvalidArgument,
		)
	}

	// **制御文字を弾きます。とくに NUL (U+0000)。**
	//
	// PostgreSQL の text は NUL を格納できず、パラメータとして送ると
	// サーバが `invalid byte sequence for encoding "UTF8": 0x00`
	// (SQLSTATE 22021) を返します。**これは 500 になります** ——
	// つまり `?q=%00` の一撃で、未ログインの誰でもエラー応答を作れます
	// (レビュー指摘。実際に 500 を再現しました)。
	//
	// TrimSpace は改行やタブを「空白」として端からは落としますが、
	// **語の内側に入った制御文字は残ります** (`a\x00b` など)。
	// 検索語として意味を持たない文字なので、ここで 400 にします。
	// unicode.IsControl が見るのは C0 (U+0000〜U+001F)・DEL・C1 です。
	// 全角スペースなどの空白は制御文字ではないので、語の内側に入っていても
	// そのまま残ります (検索語として意味があるため)。
	if strings.ContainsFunc(keyword, unicode.IsControl) {
		return nil, fmt.Errorf(
			"検索語に使えない文字が含まれています: %w", apperr.ErrInvalidArgument)
	}

	return &SearchQuery{keyword: keyword}, nil
}

// Keyword は正規化済みの検索語を返します。空文字にはなりません。
func (q SearchQuery) Keyword() string {
	return q.keyword
}
