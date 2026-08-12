// Package idempotency はクライアント側のリトライを扱うための冪等キーを表します。
//
// 設計は docs/adr/0015-idempotency.md。
//
// **HTTP 層・ユースケース層・永続化層の 3 つを横断します。**
// ヘッダを読むのは HTTP 層、記録するのは永続化層、
// その間を運ぶのがユースケース層になるためです。
// internal/pagination と同じ位置づけで、どこからでも import してよい下地として扱います。
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"develop-experiments/apps/go-api/internal/apperr"
)

// MaxKeyLength はキーの最大長です。
// DB 側の CHECK 制約 idempotency_keys_key_length と、
// 仕様書 (api/openapi.yaml の IdempotencyKey) と同じ値にしてください。
const MaxKeyLength = 255

// Request は 1 回の冪等な要求です。
//
// **キーだけでは足りません。** 同じキーで別の内容が送られたことを
// 検出できないと、クライアントのバグが「前回の結果が黙って返る」形で隠れます
// (ADR 0015 の「request_hash を持つ理由」)。
type Request struct {
	// Key はクライアントが生成した値です。
	Key string
	// Endpoint はどの経路で使われたかです。診断用に保存します。
	Endpoint string
	// RequestHash は要求の内容から一意に定まる指紋です。
	RequestHash string
}

// New は検証済みの Request を組み立てます。
//
// fields には「同じキーなら同じでなければならない値」をすべて渡してください。
// ここから漏れた値は、変えても 422 になりません。
//
// **生のリクエストボディではなく、解析後の値を渡します。**
// バイト列で取ると、同じ内容でも JSON のキー順や空白が違うだけで
// 別物と判定され、正しいクライアントが 422 を受け取ります。
func New(key, endpoint string, fields ...string) (*Request, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("Idempotency-Key が空です: %w", apperr.ErrInvalidArgument)
	}
	// 仕様書側でも maxLength を宣言していますが、
	// ヘッダの検証が将来外れても DB の CHECK 制約に到達しないよう、ここでも見ます。
	if n := utf8.RuneCountInString(key); n > MaxKeyLength {
		return nil, fmt.Errorf(
			"Idempotency-Key が長すぎます (%d 文字, 上限 %d 文字): %w",
			n, MaxKeyLength, apperr.ErrInvalidArgument,
		)
	}

	return &Request{
		Key:         key,
		Endpoint:    endpoint,
		RequestHash: hashFields(endpoint, fields),
	}, nil
}

// hashFields は経路と内容から指紋を作ります。
//
// **各値を長さで前置きしてから連結します。** 単純に連結すると
// ("ab", "c") と ("a", "bc") が同じ指紋になり、
// **別の内容の要求が「同じ内容の再送」と判定されて、
// 前回の結果が返ってしまいます。** 投稿が 1 件失われる形になります。
func hashFields(endpoint string, fields []string) string {
	h := sha256.New()
	write := func(s string) {
		// 長さを 10 進で前置きし、区切りを入れる。
		_, _ = h.Write([]byte(strconv.Itoa(len(s))))
		_, _ = h.Write([]byte(":"))
		_, _ = h.Write([]byte(s))
	}

	write(endpoint)
	for _, f := range fields {
		write(f)
	}
	return hex.EncodeToString(h.Sum(nil))
}
