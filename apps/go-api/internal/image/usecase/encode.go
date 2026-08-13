package usecase

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"

	// **デコーダを副作用 import で登録する。**
	// image.Decode / image.DecodeConfig は登録済みの形式しか扱えません。
	// 入れ忘れると「PNG だけ通らない」のような形で静かに壊れます。
	//
	// GIF は入れません。許可する入力は JPEG / PNG / WebP だけです
	// (docs/adr/0007-image-storage.md 決定 2)。**登録しただけで
	// 受け付ける形式が増える**ので、ここに書くものは許可リストそのものです。
	// (image/jpeg は上で通常 import しているため、登録もそこで済んでいます。)
	_ "image/png"

	"github.com/HugoSmits86/nativewebp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // WebP のデコード (エンコードは nativewebp)

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/image/domain/model"
)

// colorWhite は JPEG へ落とすときに透過を埋める色です。
var colorWhite = color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}

// jpegQuality は再エンコード時の品質です。
//
// 80 は実測で「JPEG q80 が可逆 WebP の 1/6.7」という比較に使った値
// (ADR 0007 決定 6)。上げると容量が増え、下げると輪郭が崩れます。
const jpegQuality = 80

// decoded は再エンコードの結果です。
type decoded struct {
	Bytes  []byte
	Width  int
	Height int
}

// reencode は受け取ったバイト列を検証し、保存する形式へ変換します。
//
// **検証の順序が防御そのものになります** (ADR 0007 決定 2)。
//
//  1. バイト数の上限
//  2. マジックナンバーで形式を判定 (拡張子と Content-Type は信用しない)
//  3. ヘッダだけをデコードして width x height を取得
//  4. 画素数の上限                        <- 5 より前に置くことが要点
//  5. ここで初めて全体をデコード
//  6. リサイズ + 再エンコード
//
// 4 を 5 より前に置かないと、「圧縮後は数十 KB だが展開すると数十 GB」
// という画像 (decompression bomb) を止められません。
func reencode(raw []byte, kind model.Kind) (*decoded, error) {
	// 1. バイト数。呼び出し側 (HTTP 層) でも制限しますが、
	//    ここでも見ます —— ドメインの不変条件を HTTP 層に預けないため。
	if int64(len(raw)) > model.MaxUploadBytes {
		return nil, fmt.Errorf(
			"画像が大きすぎます (%d バイト。上限は %d バイト): %w",
			len(raw), model.MaxUploadBytes, apperr.ErrPayloadTooLarge)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("画像が空です: %w", apperr.ErrInvalidArgument)
	}

	// 2. マジックナンバー。**拡張子と Content-Type は自己申告**なので使いません。
	if !isAllowedFormat(raw) {
		return nil, fmt.Errorf(
			"対応していない画像形式です (JPEG / PNG / WebP のみ): %w", apperr.ErrInvalidArgument)
	}

	// 3. ヘッダだけを読む。**全体を展開しません。**
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("画像を解釈できません: %w", apperr.ErrInvalidArgument)
	}

	// 4. 画素数。ここが decompression bomb への防御になります。
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if pixels > model.MaxPixels {
		return nil, fmt.Errorf(
			"画像の画素数が多すぎます (%dx%d = %d 画素。上限は %d 画素): %w",
			cfg.Width, cfg.Height, pixels, model.MaxPixels, apperr.ErrPayloadTooLarge)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("画像の寸法が不正です: %w", apperr.ErrInvalidArgument)
	}

	// 5. ここで初めて全体をデコードします。
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("画像をデコードできません: %w", apperr.ErrInvalidArgument)
	}

	// **EXIF の向きを適用してから縮小します。**
	//
	// image.Decode は Orientation タグを適用しません。再エンコードで
	// EXIF は落ちるので (それは位置情報を消すための意図した挙動)、
	// ここで向きを画素に焼き込まないと、**縦に構えて撮った写真が
	// 横倒しのまま保存され、回転のヒントも失われます。**
	//
	// 縮小より前に置くのは、回転で幅と高さが入れ替わるためです。
	// 後に置くと、長辺の上限が入れ替わる前の辺に適用されます。
	src = applyOrientation(src, readOrientation(raw))

	// 6. リサイズしてから、こちらの形式でエンコードし直します。
	resized := resizeToFit(src, kind.MaxDimension())
	bounds := resized.Bounds()

	var buf bytes.Buffer
	switch kind.Format() {
	case model.FormatJPEG:
		// **JPEG は透過を持てません。** アルファを白で潰してから渡します。
		// そのまま渡すと、透過部分が黒く出る実装差に振り回されます。
		if err := jpeg.Encode(&buf, flattenAlpha(resized), &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, fmt.Errorf("JPEG のエンコードに失敗しました: %w", err)
		}
	case model.FormatWebP:
		if err := nativewebp.Encode(&buf, resized, &nativewebp.Options{
			CompressionLevel: nativewebp.DefaultCompression,
		}); err != nil {
			return nil, fmt.Errorf("WebP のエンコードに失敗しました: %w", err)
		}
	default:
		return nil, fmt.Errorf("出力形式が不明です (kind=%q): %w", kind, apperr.ErrInvalidArgument)
	}

	return &decoded{
		Bytes:  buf.Bytes(),
		Width:  bounds.Dx(),
		Height: bounds.Dy(),
	}, nil
}

// マジックナンバー。**先頭バイトだけで判定します。**
var (
	magicJPEG = []byte{0xFF, 0xD8, 0xFF}
	magicPNG  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	// WebP は RIFF コンテナ。4-7 バイト目がサイズなので飛ばして照合します。
	magicRIFF = []byte{'R', 'I', 'F', 'F'}
	magicWEBP = []byte{'W', 'E', 'B', 'P'}
)

// isAllowedFormat は許可した形式かをマジックナンバーで判定します。
//
// **拡張子と Content-Type を信用しません** (ADR 0007 決定 2)。
// どちらも送信側の自己申告であり、中身の保証になりません。
//
// なお、ここを通っても安全にはなりません。**安全にするのは再エンコード**で、
// この判定は「デコーダに渡す前に明らかな異物を落とす」ためのものです。
func isAllowedFormat(raw []byte) bool {
	switch {
	case bytes.HasPrefix(raw, magicJPEG):
		return true
	case bytes.HasPrefix(raw, magicPNG):
		return true
	case len(raw) >= 12 &&
		bytes.Equal(raw[0:4], magicRIFF) &&
		bytes.Equal(raw[8:12], magicWEBP):
		return true
	default:
		return false
	}
}

// resizeToFit は長辺が max に収まるよう縮小します。
//
// **拡大はしません。** 小さい画像を引き伸ばしても情報は増えず、
// 容量だけが増えます。
func resizeToFit(src image.Image, max int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return src
	}

	// 長辺を max に合わせ、短辺は比率を保つ。
	// **整数の切り捨てで 0 にしない** —— 極端に細長い画像で
	// 高さ 0 の画像を作ると、エンコードも DB の CHECK も通らない。
	var nw, nh int
	if w >= h {
		nw = max
		nh = h * max / w
	} else {
		nh = max
		nw = w * max / h
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	// CatmullRom は縮小の品質が高い代わりに遅い。
	// 画像 1 枚あたりの処理なので、品質を優先する。
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}

// flattenAlpha は透過を白で埋めた画像を返します。
//
// JPEG は透過を表現できません。アルファ付きの画像をそのまま
// jpeg.Encode に渡すと、実装によって透過部分の色が変わります。
// **白で潰す**ことを明示的な決定として置きます。
func flattenAlpha(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, image.NewUniform(colorWhite), image.Point{}, draw.Src)
	draw.Draw(dst, b, src, b.Min, draw.Over)
	return dst
}
