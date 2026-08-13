// Package model は画像のドメインモデルです。
//
// 判断の記録は docs/adr/0007-image-storage.md。
package model

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
)

// Kind は画像の用途です。添付先のテーブルを兼ねて表します。
//
// DB の CHECK 制約 (images_kind_valid) と同じ値を持ちます。
// 意図的な重複です —— DB は最後の砦であり、こちらは入口の検証になります。
type Kind string

const (
	// KindCommentAttachment はコメントへの添付です。本文の一部なので大きめ。
	KindCommentAttachment Kind = "comment_attachment"
	// KindAvatar はプロフィール画像です。小さい正方形。
	KindAvatar Kind = "avatar"
	// KindThreadIcon はスレッドのアイコンです。小さい正方形。
	KindThreadIcon Kind = "thread_icon"
)

// Status は画像の状態です。DB の CHECK 制約 (images_status_valid) と揃えます。
type Status string

const (
	// StatusPending はストレージへの書き込みが確定していない状態です。
	//
	// **DB を先に書く** ため、この状態が必ず一度は存在します
	// (ADR 0007 決定 3)。一定時間以上ここに留まった行は孤児として回収します。
	StatusPending Status = "pending"
	// StatusCommitted はストレージへの書き込みが完了した状態です。
	StatusCommitted Status = "committed"
	// StatusDeleted はモデレーターが削除した状態です (ADR 0011 決定 5)。
	//
	// **DB 行は残します。** 投稿から参照されているため消すと外部キー違反になり、
	// かつ「画像は削除されました」と「元から画像なし」を区別できなくなります
	// (ADR 0016 問題 3)。消すのは S3 のオブジェクトだけです。
	StatusDeleted Status = "deleted"
)

// Format は保存する画像の形式です。
//
// **入力の形式ではありません。** 受け取ったバイト列は必ず再エンコードするため
// (ADR 0007 決定 2)、ここに現れるのは**こちらが生成した形式**だけになります。
type Format string

const (
	// FormatJPEG は非可逆圧縮。写真に向きます。
	FormatJPEG Format = "jpeg"
	// FormatWebP は可逆圧縮 (VP8L)。平坦な画像と透過に向きます。
	FormatWebP Format = "webp"
)

// ContentType は配信時の Content-Type です。
func (f Format) ContentType() string {
	switch f {
	case FormatJPEG:
		return "image/jpeg"
	case FormatWebP:
		return "image/webp"
	default:
		return ""
	}
}

// Extension はオブジェクトキーの拡張子です (先頭のドットを含む)。
//
// キーに拡張子を付けるのは、CDN やブラウザが URL から形式を推測する経路への
// 配慮であり、**判定には使いません**。判定は content_type 列が持ちます。
func (f Format) Extension() string {
	switch f {
	case FormatJPEG:
		return ".jpg"
	case FormatWebP:
		return ".webp"
	default:
		return ""
	}
}

// Format は用途から出力形式を決めます (ADR 0007 決定 6)。
//
// **用途によって形式が変わります。** 純 Go の WebP エンコーダは
// 可逆 (VP8L) しか出せず、写真では JPEG の 6.7 倍の容量になります。
// 逆に平坦な画像では WebP が圧勝します。実測値は ADR 0007 決定 6。
//
//	コメント添付   写真が主。大きめ            -> JPEG
//	アバター       小さい正方形。透過を保つ    -> WebP (可逆)
//	スレッドアイコン 同上                       -> WebP (可逆)
func (k Kind) Format() Format {
	if k == KindCommentAttachment {
		return FormatJPEG
	}
	return FormatWebP
}

// MaxDimension は長辺の上限です。これを超える画像は縮小してから保存します。
//
// アバターとアイコンを小さく抑えるのは、
// **可逆圧縮の容量が画素数にほぼ比例する**ためです (決定 6 の実測)。
func (k Kind) MaxDimension() int {
	if k == KindCommentAttachment {
		return 1600
	}
	return 512
}

// Valid は既知の用途かを返します。
func (k Kind) Valid() bool {
	switch k {
	case KindCommentAttachment, KindAvatar, KindThreadIcon:
		return true
	default:
		return false
	}
}

// 受け入れる入力の上限 (ADR 0007 決定 2 の「検証の順序」)。
const (
	// MaxUploadBytes は受け取るバイト数の上限です。
	MaxUploadBytes int64 = 5 << 20 // 5 MiB

	// MaxPixels はデコード後の画素数の上限です。
	//
	// **バイト数の上限だけでは decompression bomb を防げません。**
	// 「圧縮後は数十 KB だが展開すると数十 GB」という画像が作れるため、
	// 全体をデコードする前に image.DecodeConfig で寸法だけを読んで弾きます。
	//
	// 【ADR 0007 決定 2 の「既定 5000 万画素」から下げています】
	// 5000 万画素は RGBA で 1 枚あたり約 191 MB になります。
	// 平坦な PNG なら 5 MiB の内側で 8000x6000 (4800 万画素) が作れるので、
	// **バイト数の上限を守ったまま到達できます**。同時 10 件で約 1.9 GB。
	//
	//	5000 万画素   191 MB / 枚   同時 10 件で 1.9 GB
	//	2500 万画素    95 MB / 枚   同時 10 件で 0.9 GB
	//
	// 出力の長辺は 1600 (コメント添付) / 512 (アバター) までなので、
	// 入力を 5000 万まで受ける必要がありません。
	// 一般的なスマートフォンの写真は 1200 万画素程度で、2500 万なら収まります。
	//
	// **これだけでは足りません。** 縮小はデコード後に効くため、
	// 同時にデコードする本数の上限が別に要ります (アップロード API 側の責務)。
	MaxPixels int64 = 25_000_000
)

// Image は保存された画像です。
type Image struct {
	ID      uuid.UUID
	OwnerID int64
	Kind    Kind
	// ObjectKey はストレージ上のキーです。**利用者の入力を含みません**
	// (ADR 0007 決定 3)。ファイル名をそのまま使うと、パストラバーサル・
	// 他人のオブジェクトの上書き・キーの衝突がすべて可能になります。
	ObjectKey   string
	ContentType string
	Width       int
	Height      int
	ByteSize    int64
	Status      Status
	CreatedAt   time.Time
	CommittedAt *time.Time
	// ObjectReclaimedAt は回収バッチが S3 のオブジェクトを消した時刻です。
	//
	// **'deleted' の行を回収対象から外すために要ります** ——
	// DB 行を残す種類なので、消したことを記録しないと
	// 毎周回で同じ行へ DELETE を投げ直すことになります
	// (000006 の列コメントを参照)。
	ObjectReclaimedAt *time.Time
}

// NewPending は再エンコード済みの画像から pending の行を組み立てます。
//
// **ストレージへ書く前に呼びます** (ADR 0007 決定 3)。
// ここで決めた ObjectKey で PUT し、成功したら Commit します。
//
// ownerID を必須にしているのは、画像を投稿できるのがログイン済み利用者だけ
// だからです (ADR 0007 の背景)。匿名で任意のバイト列を置けると、
// 容量の消費と違法コンテンツの設置が追跡不能な形で可能になります。
func NewPending(
	ownerID int64, kind Kind, width, height int, byteSize int64,
) (*Image, error) {
	if ownerID <= 0 {
		return nil, fmt.Errorf("画像の所有者が必要です: %w", apperr.ErrUnauthenticated)
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("画像の用途が不正です (got %q): %w", kind, apperr.ErrInvalidArgument)
	}
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("画像の寸法が不正です (%dx%d): %w", width, height, apperr.ErrInvalidArgument)
	}
	if byteSize <= 0 {
		return nil, fmt.Errorf("画像が空です: %w", apperr.ErrInvalidArgument)
	}

	// UUID v7 を Go 側で採番する。PostgreSQL 17 には uuidv7() が無い
	// (ADR 0003 未決 #11)。先頭が時刻なので B-tree の挿入位置が末尾に寄る。
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("画像 ID の生成に失敗しました: %w", err)
	}

	format := kind.Format()
	return &Image{
		ID:      id,
		OwnerID: ownerID,
		Kind:    kind,
		// キーは ID から作る。用途や利用者の入力を混ぜない
		// —— 混ぜると「キーから何かを読み取る」コードが生まれ、
		// キーの書式がスキーマの一部になってしまう。
		ObjectKey:   "images/" + id.String() + format.Extension(),
		ContentType: format.ContentType(),
		Width:       width,
		Height:      height,
		ByteSize:    byteSize,
		Status:      StatusPending,
	}, nil
}

// Commit はストレージへの書き込みが完了したことを記録します。
//
// **status と committed_at は必ず同時に動きます。**
// DB 側にも CHECK 制約 (images_committed_at_matches_status) があり、
// 片方だけ書こうとすると拒否されます。
func (i *Image) Commit(at time.Time) {
	i.Status = StatusCommitted
	i.CommittedAt = &at
}

// Reconstruct は永続化層が読み出した行からモデルを組み立てます。
//
// 検証を通しません。**DB にある値は既に検証済み**であり、
// ここで弾くと「保存できたのに読めない」状態が作れてしまいます。
func Reconstruct(
	id uuid.UUID, ownerID int64, kind Kind, objectKey, contentType string,
	width, height int, byteSize int64, status Status,
	createdAt time.Time, committedAt, objectReclaimedAt *time.Time,
) *Image {
	return &Image{
		ID:                id,
		OwnerID:           ownerID,
		Kind:              kind,
		ObjectKey:         objectKey,
		ContentType:       contentType,
		Width:             width,
		Height:            height,
		ByteSize:          byteSize,
		Status:            status,
		CreatedAt:         createdAt,
		CommittedAt:       committedAt,
		ObjectReclaimedAt: objectReclaimedAt,
	}
}
