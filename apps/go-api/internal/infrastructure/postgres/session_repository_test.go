package postgres

import "testing"

// LIMIT に 0 が渡ると 1 件も消えず、しかも戻り値が 0 になる。
// 掃除の呼び出し規約は「0 件になるまで繰り返す」なので、
// バグの症状と正常な終了条件が区別できなくなる。
//
// 実測: LIMIT 0 で DELETE すると、期限切れの行が残ったまま 0 件が返ることを
// 実 DB で確認済み。
func TestClampMaxRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		maxRows int32
		want    int32
	}{
		{name: "正の値はそのまま", maxRows: 500, want: 500},
		{name: "1 もそのまま", maxRows: 1, want: 1},
		{name: "0 は既定値に丸める", maxRows: 0, want: defaultDeleteExpiredMaxRows},
		{name: "負の値も既定値に丸める", maxRows: -1, want: defaultDeleteExpiredMaxRows},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := clampMaxRows(tt.maxRows); got != tt.want {
				t.Errorf("clampMaxRows(%d) = %d, want %d", tt.maxRows, got, tt.want)
			}
		})
	}
}
