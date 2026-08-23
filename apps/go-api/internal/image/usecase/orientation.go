package usecase

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
)

// orientation は EXIF の Orientation タグ (0x0112) の値です。
//
// 1 が「そのまま」で、2〜8 が回転と反転の組み合わせを表します。
// 0 は「情報なし」を表すためにこちらで使う値です。
type orientation uint16

const (
	orientationUnknown orientation = 0
	orientationNormal  orientation = 1
)

// applyOrientation は EXIF の向きに従って画像を回します。
//
// **再エンコードで EXIF は落ちます** (docs/adr/0007-image-storage.md 決定 2)。
// それは位置情報を消すための意図した挙動ですが、**向きの情報も一緒に消えます。**
//
// スマートフォンで縦に構えて撮った写真は、センサーの向き (横) のまま
// 記録され、`Orientation=6` で「表示時に 90 度回せ」と伝えます。
// 向きを適用せずに再エンコードすると、**横倒しの写真が保存され、
// 回転のヒントも失われた状態**で配信されます。
// コメント添付の主な用途が「スマホで撮った写真」なので、ここは効きます。
//
// 向きが無い / 1 の場合は元の画像をそのまま返します (複製しません)。
func applyOrientation(src image.Image, o orientation) image.Image {
	if o == orientationUnknown || o == orientationNormal {
		return src
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	// 5〜8 は 90 度単位の回転を含むので、幅と高さが入れ替わります。
	swap := o >= 5 && o <= 8
	dstW, dstH := w, h
	if swap {
		dstW, dstH = h, w
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	// **1 画素ずつ At/Set で運ばない。**
	//
	// At が返すのは color.Color (インターフェース) なので、
	// image.NRGBA64 のような 8 バイトの値は**1 画素ごとにヒープへ箱詰め**される。
	// 2500 万画素なら 2500 万回の確保になり、実測でこの関数のピークが
	// 568 MB まで膨らんでいた —— **縮小そのもの (456 MB) より大きい。**
	// スマートフォンの写真は EXIF 付きなので、ここが既定の経路になる。
	//
	// draw.Draw で一度だけ RGBA に落とし、あとは Pix を直接動かす。
	// 座標の対応 (下の switch) は変えていない。
	srcRGBA, ok := src.(*image.RGBA)
	if !ok {
		srcRGBA = image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(srcRGBA, srcRGBA.Bounds(), src, b.Min, draw.Src)
	}
	sMinX, sMinY := srcRGBA.Rect.Min.X, srcRGBA.Rect.Min.Y

	for y := range h {
		for x := range w {
			// EXIF の 8 通り (TIFF 6.0 の Orientation)。
			//   1 そのまま        2 左右反転
			//   3 180 度回転      4 上下反転
			//   5 転置            6 時計回り 90 度
			//   7 反転して転置    8 反時計回り 90 度
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			default:
				nx, ny = x, y
			}
			si := srcRGBA.PixOffset(sMinX+x, sMinY+y)
			di := dst.PixOffset(nx, ny)
			copy(dst.Pix[di:di+4], srcRGBA.Pix[si:si+4])
		}
	}
	return dst
}

// JPEG のマーカー。
const (
	jpegMarkerPrefix = 0xFF
	jpegMarkerSOI    = 0xD8
	jpegMarkerAPP1   = 0xE1
	jpegMarkerSOS    = 0xDA
)

// readOrientation は JPEG の EXIF から Orientation を読み取ります。
//
// **外部ライブラリを足しません。** 必要なのはタグ 1 つで、
// JPEG の APP1 セグメントと TIFF の IFD0 を辿るだけで届きます。
// EXIF を汎用に解釈するライブラリを入れると、
// 「向きのために EXIF 全体の解析を持ち込む」ことになります
// (依存を固定して増やす方針との釣り合い)。
//
// 読めない場合は orientationUnknown を返します。**エラーにしません** ——
// 向きが分からないことは異常ではなく、大多数の画像がそうです。
//
// PNG と WebP は扱いません。PNG に向きの概念はなく、
// WebP の EXIF チャンクは任意かつ稀なためです。
func readOrientation(raw []byte) orientation {
	if len(raw) < 4 || raw[0] != jpegMarkerPrefix || raw[1] != jpegMarkerSOI {
		return orientationUnknown
	}

	// セグメントを順に辿って APP1 を探す。
	for i := 2; i+4 <= len(raw); {
		if raw[i] != jpegMarkerPrefix {
			return orientationUnknown
		}
		marker := raw[i+1]
		// SOS 以降は圧縮データなので、そこまでに見つからなければ終わり。
		if marker == jpegMarkerSOS {
			return orientationUnknown
		}
		// セグメント長は自身の 2 バイトを含む。
		size := int(binary.BigEndian.Uint16(raw[i+2 : i+4]))
		if size < 2 || i+2+size > len(raw) {
			return orientationUnknown
		}
		if marker == jpegMarkerAPP1 {
			payload := raw[i+4 : i+2+size]
			if o := orientationFromExif(payload); o != orientationUnknown {
				return o
			}
		}
		i += 2 + size
	}
	return orientationUnknown
}

// exifHeader は APP1 の先頭に置かれる識別子です。
var exifHeader = []byte{'E', 'x', 'i', 'f', 0x00, 0x00}

// orientationFromExif は APP1 の中身から Orientation を取り出します。
func orientationFromExif(payload []byte) orientation {
	if !bytes.HasPrefix(payload, exifHeader) {
		return orientationUnknown
	}
	tiff := payload[len(exifHeader):]
	if len(tiff) < 8 {
		return orientationUnknown
	}

	// TIFF ヘッダ。先頭 2 バイトがバイト順を表す ("II" = little / "MM" = big)。
	var order binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		order = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		order = binary.BigEndian
	default:
		return orientationUnknown
	}
	if order.Uint16(tiff[2:4]) != 42 { // TIFF のマジックナンバー
		return orientationUnknown
	}

	ifdOffset := int(order.Uint32(tiff[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(tiff) {
		return orientationUnknown
	}

	count := int(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	for n := range count {
		// 各エントリは 12 バイト固定。
		entry := ifdOffset + 2 + n*12
		if entry+12 > len(tiff) {
			return orientationUnknown
		}
		tag := order.Uint16(tiff[entry : entry+2])
		if tag != 0x0112 { // Orientation
			continue
		}
		// 値は SHORT (型 3) で、12 バイトのエントリ内に収まる。
		value := order.Uint16(tiff[entry+8 : entry+10])
		if value >= 1 && value <= 8 {
			return orientation(value)
		}
		return orientationUnknown
	}
	return orientationUnknown
}
