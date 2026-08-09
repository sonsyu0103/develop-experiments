package pagination

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

func ptr[T any](v T) *T { return &v }

// mustEncode は符号化に成功することを前提に、トークンを組み立てます。
func mustEncode(t *testing.T, c Cursor) string {
	t.Helper()
	s, err := c.Encode()
	if err != nil {
		t.Fatalf("Encode が失敗した (%+v): %v", c, err)
	}
	return s
}

// token は id を指すトークンを組み立てるテスト用の補助関数です。
func token(t *testing.T, id int64) string { return mustEncode(t, NewCursor(id)) }

func TestNewPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		token      *string
		size       int32
		wantSize   int32
		wantCursor *int64
		wantErr    bool
	}{
		{name: "既定値", token: nil, size: 0, wantSize: DefaultSize},
		{name: "負のサイズは既定値に丸める", token: nil, size: -1, wantSize: DefaultSize},
		{name: "明示指定", token: nil, size: 50, wantSize: 50},
		{name: "上限ちょうどは通る", token: nil, size: MaxSize, wantSize: MaxSize},
		{
			name: "カーソルつき", token: ptr(token(t, 100)), size: 10,
			wantSize: 10, wantCursor: ptr(int64(100)),
		},
		{name: "空文字は先頭ページ扱い", token: ptr(""), size: 10, wantSize: 10},

		{name: "上限超過は拒否", token: nil, size: MaxSize + 1, wantErr: true},
		{name: "base64 として壊れたトークンは拒否", token: ptr("!!!!"), size: 10, wantErr: true},
		{name: "JSON として壊れたトークンは拒否", token: ptr(encodeRaw("{")), size: 10, wantErr: true},
		{name: "id が 0 のトークンは拒否", token: ptr(token(t, 0)), size: 10, wantErr: true},
		{name: "id が負のトークンは拒否", token: ptr(token(t, -1)), size: 10, wantErr: true},
		{
			name: "版が違うトークンは拒否",
			// 将来 cursorVersion を上げたとき、古いトークンが素通りしないこと。
			token: ptr(mustEncode(t, Cursor{Version: cursorVersion + 1, ID: 100})), size: 10, wantErr: true,
		},
		{
			name:  "長すぎるトークンは復号する前に拒否",
			token: ptr(strings.Repeat("A", maxTokenLen+1)), size: 10, wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewPage(tt.token, tt.size)

			if tt.wantErr {
				if !errors.Is(err, apperr.ErrInvalidArgument) {
					t.Fatalf("err = %v, want apperr.ErrInvalidArgument", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("エラーは期待していないが発生した: %v", err)
			}
			if got.Size != tt.wantSize {
				t.Errorf("Size = %d, want %d", got.Size, tt.wantSize)
			}

			gotID := got.CursorID()
			switch {
			case tt.wantCursor == nil && gotID != nil:
				t.Errorf("CursorID() = %d, want nil", *gotID)
			case tt.wantCursor != nil && (gotID == nil || *gotID != *tt.wantCursor):
				t.Errorf("CursorID() = %v, want %d", gotID, *tt.wantCursor)
			}
		})
	}
}

// トークンは往復して元の値に戻る必要があります。
// ここが壊れると、2 ページ目以降が静かにずれます。
func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()

	for _, id := range []int64{1, 42, 1 << 62} {
		c, err := DecodeCursor(token(t, id))
		if err != nil {
			t.Fatalf("id=%d: 復号に失敗した: %v", id, err)
		}
		if c.ID != id {
			t.Errorf("ID = %d, want %d", c.ID, id)
		}
	}
}

// カーソルが id をそのまま晒していないことを確認します。
// 「不透明にする」がこの変更の目的なので、退行するとこのテストが落ちます。
func TestEncodeIsNotPlainID(t *testing.T) {
	t.Parallel()

	got := token(t, 42)
	if got == "42" {
		t.Fatalf("カーソルが id を素のまま公開している: %q", got)
	}
	if strings.ContainsAny(got, "=+/") {
		t.Errorf("URL 安全でない文字が含まれている: %q", got)
	}
}

func TestNextToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		lastID   int64
		returned int
		size     int32
		wantNil  bool
	}{
		{name: "size 件ちょうどなら次がある", lastID: 3, returned: 10, size: 10},
		{name: "size 未満なら最終ページ", lastID: 3, returned: 9, size: 10, wantNil: true},
		{name: "0 件なら最終ページ", lastID: 0, returned: 0, size: 10, wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NextToken(tt.lastID, tt.returned, tt.size)
			if err != nil {
				t.Fatalf("NextToken が失敗した: %v", err)
			}

			if tt.wantNil {
				if got != nil {
					t.Fatalf("NextToken = %q, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("NextToken = nil, want トークン")
			}

			c, err := DecodeCursor(*got)
			if err != nil {
				t.Fatalf("返したトークンが復号できない: %v", err)
			}
			if c.ID != tt.lastID {
				t.Errorf("ID = %d, want %d", c.ID, tt.lastID)
			}
		})
	}
}

func encodeRaw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
