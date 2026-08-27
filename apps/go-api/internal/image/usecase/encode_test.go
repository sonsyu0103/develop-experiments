package usecase

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/image/domain/model"
)

// ---------------------------------------------------------------------------
// テスト用の画像を作る
// ---------------------------------------------------------------------------

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("テスト用 PNG の生成に失敗した: %v", err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("テスト用 JPEG の生成に失敗した: %v", err)
	}
	return buf.Bytes()
}

// makeFlatPNG は「圧縮すると極小だが展開すると巨大」な画像を作ります。
//
// **decompression bomb の再現です。** 単色の PNG は極めてよく圧縮されるため、
// バイト数の上限 (5 MiB) の内側に収まったまま、展開すると数億画素になります。
func makeFlatPNG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("テスト用 PNG の生成に失敗した: %v", err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// 検証の順序 (ADR 0007 決定 2)
// ---------------------------------------------------------------------------

// **これがこのファイルで最も重要なテストになる。**
//
// 「圧縮後は小さいが展開すると巨大」な画像を、**展開せずに**弾けること。
// 画素数の検査を全体デコードより後ろに置くと、ここで数百 MB を確保して
// から気づくことになる。
func TestReencode_RejectsDecompressionBomb(t *testing.T) {
	t.Parallel()

	// 8000x6000 = 4800 万画素。上限 (2500 万) の約 2 倍。
	// 単色なので PNG としては極小になる。
	bomb := makeFlatPNG(t, 8000, 6000)

	// **前提の確認。** バイト数の上限に引っかかって弾かれたのでは、
	// 画素数の検査が働いたことにならない。
	if int64(len(bomb)) > model.MaxUploadBytes {
		t.Fatalf("テストの前提が崩れている: %d バイトで、バイト数の上限に達している", len(bomb))
	}
	t.Logf("圧縮後 %d バイト (%.1f KB) / 展開すると 4800 万画素", len(bomb), float64(len(bomb))/1024)

	_, err := reencode(bomb, model.KindCommentAttachment)
	if !errors.Is(err, apperr.ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want apperr.ErrPayloadTooLarge", err)
	}
}

// バイト数の上限。
func TestReencode_RejectsTooManyBytes(t *testing.T) {
	t.Parallel()

	huge := make([]byte, model.MaxUploadBytes+1)
	copy(huge, magicPNG)

	_, err := reencode(huge, model.KindCommentAttachment)
	if !errors.Is(err, apperr.ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want apperr.ErrPayloadTooLarge", err)
	}
}

// **許可していない形式はデコーダに渡さない。**
//
// SVG が代表例。ベクタ形式はスクリプトと外部参照を含められるため、
// 「画像」として扱うと XSS と SSRF の経路になる (ADR 0007 決定 2)。
func TestReencode_RejectsDisallowedFormats(t *testing.T) {
	t.Parallel()

	tests := map[string][]byte{
		"SVG":          []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"GIF":          []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"実行ファイル (ELF)": {0x7F, 'E', 'L', 'F', 2, 1, 1, 0},
		"ただのテキスト":      []byte("これは画像ではありません"),
		// **拡張子や Content-Type ではなく中身で判定していること。**
		// 呼び出し側が何を名乗っても、ここには生のバイト列しか来ない。
		"PNG を名乗る HTML": []byte("<!DOCTYPE html><html><body>png</body></html>"),
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := reencode(raw, model.KindCommentAttachment)
			if !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
			}
		})
	}
}

// **マジックナンバーだけ正しくて中身が壊れているものも弾く。**
// ここを通すと、デコーダが不正なデータを読むことになる。
func TestReencode_RejectsTruncatedImage(t *testing.T) {
	t.Parallel()

	valid := makePNG(t, 32, 32)
	truncated := valid[:len(valid)/2]

	if _, err := reencode(truncated, model.KindCommentAttachment); err == nil {
		t.Fatal("壊れた画像が通った")
	}
}

func TestReencode_RejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := reencode(nil, model.KindCommentAttachment)
	if !errors.Is(err, apperr.ErrInvalidArgument) {
		t.Fatalf("err = %v, want apperr.ErrInvalidArgument", err)
	}
}

// ---------------------------------------------------------------------------
// 再エンコード
// ---------------------------------------------------------------------------

// **入力の形式にかかわらず、出力は用途で決まる** (ADR 0007 決定 6)。
//
// PNG を入れても、コメント添付なら JPEG が出る。
// これが「アップロードされたバイト列をそのまま保存しない」という
// 決定 2 の実装そのものになる。
func TestReencode_OutputFormatFollowsKind(t *testing.T) {
	t.Parallel()

	src := makePNG(t, 64, 64)

	t.Run("コメント添付は JPEG になる", func(t *testing.T) {
		t.Parallel()

		out, err := reencode(src, model.KindCommentAttachment)
		if err != nil {
			t.Fatalf("reencode が失敗した: %v", err)
		}
		if !bytes.HasPrefix(out.Bytes, magicJPEG) {
			t.Errorf("出力が JPEG ではない (先頭 %x)", out.Bytes[:4])
		}
	})

	t.Run("アバターは WebP になる", func(t *testing.T) {
		t.Parallel()

		out, err := reencode(src, model.KindAvatar)
		if err != nil {
			t.Fatalf("reencode が失敗した: %v", err)
		}
		if !isAllowedFormat(out.Bytes) || !bytes.Equal(out.Bytes[8:12], magicWEBP) {
			t.Errorf("出力が WebP ではない (先頭 %x)", out.Bytes[:12])
		}
	})
}

// **出力は入力と別のバイト列になる** (再エンコードされている)。
//
// 「そのまま保存していない」ことの直接の確認になる。
// ここが通らない実装は、決定 2 の防御が丸ごと効いていない。
func TestReencode_DoesNotPassThroughOriginalBytes(t *testing.T) {
	t.Parallel()

	// 入力も JPEG にする。**同じ形式でも作り直すこと**を見たいので、
	// 形式が違うから別物、という理由で通らないようにする。
	src := makeJPEG(t, 64, 64)

	out, err := reencode(src, model.KindCommentAttachment)
	if err != nil {
		t.Fatalf("reencode が失敗した: %v", err)
	}
	if bytes.Equal(out.Bytes, src) {
		t.Error("入力のバイト列がそのまま返っている (再エンコードされていない)")
	}
}

// 長辺が上限を超える画像は縮小されること。
func TestReencode_ResizesLargeImages(t *testing.T) {
	t.Parallel()

	src := makePNG(t, 3000, 1500)

	out, err := reencode(src, model.KindCommentAttachment)
	if err != nil {
		t.Fatalf("reencode が失敗した: %v", err)
	}

	want := model.KindCommentAttachment.MaxDimension()
	if out.Width != want {
		t.Errorf("Width = %d, want %d", out.Width, want)
	}
	// 縦横比が保たれること。3000x1500 -> 1600x800。
	if out.Height != want/2 {
		t.Errorf("Height = %d, want %d (縦横比が保たれていない)", out.Height, want/2)
	}
}

// **小さい画像を拡大しない。** 情報は増えず容量だけ増える。
func TestReencode_DoesNotUpscale(t *testing.T) {
	t.Parallel()

	src := makePNG(t, 100, 50)

	out, err := reencode(src, model.KindCommentAttachment)
	if err != nil {
		t.Fatalf("reencode が失敗した: %v", err)
	}
	if out.Width != 100 || out.Height != 50 {
		t.Errorf("寸法 = %dx%d, want 100x50 (拡大された)", out.Width, out.Height)
	}
}

// 極端に細長い画像でも、寸法が 0 にならないこと。
// 0 になるとエンコードにも DB の CHECK 制約にも通らない。
func TestReencode_KeepsDimensionsPositive(t *testing.T) {
	t.Parallel()

	// 4000x1。長辺を 1600 に縮めると、短辺は切り捨てで 0 になりうる。
	src := makePNG(t, 4000, 1)

	out, err := reencode(src, model.KindCommentAttachment)
	if err != nil {
		t.Fatalf("reencode が失敗した: %v", err)
	}
	if out.Height < 1 || out.Width < 1 {
		t.Errorf("寸法 = %dx%d, want どちらも 1 以上", out.Width, out.Height)
	}
}

// マジックナンバーの判定そのもの。
func TestIsAllowedFormat(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw  []byte
		want bool
	}{
		"JPEG":              {append([]byte{}, magicJPEG...), true},
		"PNG":               {append([]byte{}, magicPNG...), true},
		"WebP":              {[]byte("RIFF\x00\x00\x00\x00WEBPVP8L"), true},
		"RIFF だが WebP ではない": {[]byte("RIFF\x00\x00\x00\x00WAVEfmt "), false},
		"短すぎる RIFF":         {[]byte("RIFF"), false},
		"空":                 {nil, false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := isAllowedFormat(tt.raw); got != tt.want {
				t.Errorf("isAllowedFormat() = %v, want %v", got, tt.want)
			}
		})
	}
}
