package pagination

import (
	"errors"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
)

func ptr(v int64) *int64 { return &v }

func TestNewPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cursor   *int64
		size     int32
		wantSize int32
		wantErr  bool
	}{
		{name: "既定値", cursor: nil, size: 0, wantSize: DefaultSize},
		{name: "負のサイズは既定値に丸める", cursor: nil, size: -1, wantSize: DefaultSize},
		{name: "明示指定", cursor: nil, size: 50, wantSize: 50},
		{name: "上限ちょうどは通る", cursor: nil, size: MaxSize, wantSize: MaxSize},
		{name: "カーソルつき", cursor: ptr(100), size: 10, wantSize: 10},

		{name: "上限超過は拒否", cursor: nil, size: MaxSize + 1, wantErr: true},
		{name: "カーソル 0 は拒否", cursor: ptr(0), size: 10, wantErr: true},
		{name: "負のカーソルは拒否", cursor: ptr(-1), size: 10, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewPage(tt.cursor, tt.size)

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
			if tt.cursor == nil && got.Cursor != nil {
				t.Errorf("Cursor = %v, want nil", *got.Cursor)
			}
			if tt.cursor != nil && (got.Cursor == nil || *got.Cursor != *tt.cursor) {
				t.Errorf("Cursor = %v, want %d", got.Cursor, *tt.cursor)
			}
		})
	}
}
