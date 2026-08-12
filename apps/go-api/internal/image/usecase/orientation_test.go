package usecase

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"develop-experiments/apps/go-api/internal/image/domain/model"
)

// makeJPEGWithOrientation は Orientation タグを埋めた JPEG を作ります。
//
// **ライブラリを使わずに組み立てます。** EXIF を書けるライブラリを
// テストのためだけに足すと、本体の「依存を増やさない」判断と釣り合いません。
// APP1 は SOI の直後に差し込めばよいので、手で組めます。
func makeJPEGWithOrientation(t *testing.T, w, h int, o uint16) []byte {
	t.Helper()

	// まず素の JPEG を作る。左上だけ色を変えて、回転を判定できるようにする。
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 20, G: 20, B: 20, A: 255})
		}
	}
	// 左上の 1/4 を白く塗る。回転すると白い領域の位置が変わる。
	for y := range h / 2 {
		for x := range w / 2 {
			img.Set(x, y, color.RGBA{R: 250, G: 250, B: 250, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("テスト用 JPEG の生成に失敗した: %v", err)
	}
	base := buf.Bytes()

	// TIFF: ヘッダ 8 バイト + IFD (件数 2 + エントリ 12 + 次 IFD 4)。
	tiff := make([]byte, 0, 26)
	tiff = append(tiff, 'M', 'M', 0, 42) // ビッグエンディアン + マジック
	tiff = binary.BigEndian.AppendUint32(tiff, 8)
	tiff = binary.BigEndian.AppendUint16(tiff, 1)      // エントリ 1 件
	tiff = binary.BigEndian.AppendUint16(tiff, 0x0112) // Orientation
	tiff = binary.BigEndian.AppendUint16(tiff, 3)      // 型 SHORT
	tiff = binary.BigEndian.AppendUint32(tiff, 1)      // 件数
	tiff = binary.BigEndian.AppendUint16(tiff, o)      // 値
	tiff = binary.BigEndian.AppendUint16(tiff, 0)      // 詰め物
	tiff = binary.BigEndian.AppendUint32(tiff, 0)      // 次の IFD 無し

	payload := append(append([]byte{}, exifHeader...), tiff...)
	app1 := []byte{0xFF, 0xE1}
	app1 = binary.BigEndian.AppendUint16(app1, uint16(len(payload)+2))
	app1 = append(app1, payload...)

	// SOI (2 バイト) の直後に APP1 を差し込む。
	out := make([]byte, 0, len(base)+len(app1))
	out = append(out, base[:2]...)
	out = append(out, app1...)
	out = append(out, base[2:]...)
	return out
}

func TestReadOrientation(t *testing.T) {
	t.Parallel()

	for _, want := range []uint16{1, 2, 3, 4, 5, 6, 7, 8} {
		raw := makeJPEGWithOrientation(t, 16, 8, want)
		if got := readOrientation(raw); got != orientation(want) {
			t.Errorf("Orientation=%d: readOrientation() = %d", want, got)
		}
	}
}

// EXIF が無い画像でも落ちないこと。**大多数の画像がこちら。**
func TestReadOrientation_MissingExif(t *testing.T) {
	t.Parallel()

	if got := readOrientation(makeJPEG(t, 16, 8)); got != orientationUnknown {
		t.Errorf("EXIF が無いのに %d が返った", got)
	}
	if got := readOrientation(makePNG(t, 16, 8)); got != orientationUnknown {
		t.Errorf("PNG で %d が返った", got)
	}
	// 壊れた入力でも落ちないこと。
	for _, raw := range [][]byte{nil, {0xFF}, {0xFF, 0xD8}, {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x02}} {
		if got := readOrientation(raw); got != orientationUnknown {
			t.Errorf("壊れた入力で %d が返った", got)
		}
	}
}

// **縦横が入れ替わること。**
//
// スマホの縦写真は Orientation=6 で「表示時に 90 度回せ」と伝えてくる。
// 適用しないと、16x8 の横長のまま保存される。
func TestReencode_AppliesExifOrientation(t *testing.T) {
	t.Parallel()

	raw := makeJPEGWithOrientation(t, 16, 8, 6)

	out, err := reencode(raw, model.KindCommentAttachment)
	if err != nil {
		t.Fatalf("reencode が失敗した: %v", err)
	}

	// 90 度回るので 8x16 になる。
	if out.Width != 8 || out.Height != 16 {
		t.Errorf("寸法 = %dx%d, want 8x16 (向きが適用されていない)", out.Width, out.Height)
	}
}

// Orientation=1 では寸法が変わらないこと。
// 「常に回す」実装でも上のテストは通ってしまう。
func TestReencode_KeepsDimensionsForNormalOrientation(t *testing.T) {
	t.Parallel()

	raw := makeJPEGWithOrientation(t, 16, 8, 1)

	out, err := reencode(raw, model.KindCommentAttachment)
	if err != nil {
		t.Fatalf("reencode が失敗した: %v", err)
	}
	if out.Width != 16 || out.Height != 8 {
		t.Errorf("寸法 = %dx%d, want 16x8", out.Width, out.Height)
	}
}

// **画素の位置まで動いていること。**
//
// 寸法だけを見ると、幅と高さを入れ替えただけの実装でも通る。
// 左上を白く塗ってあるので、時計回り 90 度なら白は右上へ移る。
func TestApplyOrientation_MovesPixels(t *testing.T) {
	t.Parallel()

	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := range 2 {
		for x := range 4 {
			src.Set(x, y, color.RGBA{A: 255})
		}
	}
	// 左上の 1 画素を白に。
	src.Set(0, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})

	// 6 = 時計回り 90 度。左上 (0,0) は右上 (h-1, 0) = (1, 0) へ。
	got := applyOrientation(src, 6)
	if got.Bounds().Dx() != 2 || got.Bounds().Dy() != 4 {
		t.Fatalf("寸法 = %v, want 2x4", got.Bounds())
	}
	r, _, _, _ := got.At(1, 0).RGBA()
	if r < 0x8000 {
		t.Errorf("白い画素が右上に来ていない (At(1,0) の R = %d)", r>>8)
	}
	// 元の位置は黒になっているはず。
	if r0, _, _, _ := got.At(0, 0).RGBA(); r0 > 0x8000 {
		t.Errorf("左上がまだ白い (回転していない)")
	}
}

// 向きが 1 / 未知なら、元の画像をそのまま返すこと (複製しない)。
func TestApplyOrientation_NoOpReturnsSameImage(t *testing.T) {
	t.Parallel()

	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if got := applyOrientation(src, orientationNormal); got != image.Image(src) {
		t.Error("Orientation=1 で画像が作り直されている")
	}
	if got := applyOrientation(src, orientationUnknown); got != image.Image(src) {
		t.Error("向きが不明なのに画像が作り直されている")
	}
}
