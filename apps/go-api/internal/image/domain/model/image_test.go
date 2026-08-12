package model

import (
	"errors"
	"strings"
	"testing"
	"time"

	"develop-experiments/apps/go-api/internal/apperr"
)

// **用途によって出力形式が変わる** (docs/adr/0007-image-storage.md 決定 6)。
//
// 純 Go の WebP エンコーダは可逆 (VP8L) しか出せない。
// 写真を可逆で保存すると JPEG の 6.7 倍の容量になる (決定 6 の実測)。
// ここが逆になると、コメント添付の帯域が一気に膨らむ。
func TestKind_Format(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind Kind
		want Format
	}{
		{KindCommentAttachment, FormatJPEG},
		{KindAvatar, FormatWebP},
		{KindThreadIcon, FormatWebP},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			t.Parallel()

			if got := tt.kind.Format(); got != tt.want {
				t.Errorf("Format() = %q, want %q", got, tt.want)
			}
		})
	}
}

// 形式と Content-Type / 拡張子の対応。
//
// **DB の CHECK 制約 (images_content_type_valid) と揃っている必要がある。**
// ずれると保存の瞬間に 500 になる。
func TestFormat_ContentTypeAndExtension(t *testing.T) {
	t.Parallel()

	if got := FormatJPEG.ContentType(); got != "image/jpeg" {
		t.Errorf("JPEG の ContentType = %q", got)
	}
	if got := FormatWebP.ContentType(); got != "image/webp" {
		t.Errorf("WebP の ContentType = %q", got)
	}
	if got := FormatJPEG.Extension(); got != ".jpg" {
		t.Errorf("JPEG の Extension = %q", got)
	}
	if got := FormatWebP.Extension(); got != ".webp" {
		t.Errorf("WebP の Extension = %q", got)
	}

	// 未知の形式は空を返す。**既定値に倒さない** ——
	// image/jpeg へ倒すと、別形式のバイト列に JPEG の Content-Type が付く。
	var unknown Format = "avif"
	if got := unknown.ContentType(); got != "" {
		t.Errorf("未知の形式の ContentType = %q, want 空", got)
	}
	if got := unknown.Extension(); got != "" {
		t.Errorf("未知の形式の Extension = %q, want 空", got)
	}
}

func TestKind_Valid(t *testing.T) {
	t.Parallel()

	for _, k := range []Kind{KindCommentAttachment, KindAvatar, KindThreadIcon} {
		if !k.Valid() {
			t.Errorf("%q が無効と判定された", k)
		}
	}
	for _, k := range []Kind{"", "comment", "COMMENT_ATTACHMENT", "banner"} {
		if k.Valid() {
			t.Errorf("%q が有効と判定された", k)
		}
	}
}

// アバターとアイコンの長辺を小さく抑えること。
// 可逆圧縮の容量は画素数にほぼ比例するため、ここが緩むと容量が効く。
func TestKind_MaxDimension(t *testing.T) {
	t.Parallel()

	if KindAvatar.MaxDimension() >= KindCommentAttachment.MaxDimension() {
		t.Errorf("アバター (%d) がコメント添付 (%d) より小さくない",
			KindAvatar.MaxDimension(), KindCommentAttachment.MaxDimension())
	}
	if KindThreadIcon.MaxDimension() != KindAvatar.MaxDimension() {
		t.Error("アイコンとアバターの上限が揃っていない")
	}
}

// ---------------------------------------------------------------------------
// NewPending
// ---------------------------------------------------------------------------

func TestNewPending(t *testing.T) {
	t.Parallel()

	img, err := NewPending(42, KindCommentAttachment, 1200, 800, 143_000)
	if err != nil {
		t.Fatalf("NewPending が失敗した: %v", err)
	}

	if img.Status != StatusPending {
		t.Errorf("Status = %q, want pending", img.Status)
	}
	// **確定していない画像に committed_at を入れない。**
	// DB の CHECK 制約 (images_committed_at_matches_status) が拒否する。
	if img.CommittedAt != nil {
		t.Error("pending なのに CommittedAt が入っている")
	}
	if img.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg (コメント添付は JPEG)", img.ContentType)
	}

	// キーは ID から作る。**利用者の入力を含めない** (ADR 0007 決定 3)。
	if want := "images/" + img.ID.String() + ".jpg"; img.ObjectKey != want {
		t.Errorf("ObjectKey = %q, want %q", img.ObjectKey, want)
	}

	// UUID v7 であること。先頭が時刻なので B-tree の挿入位置が末尾に寄る。
	if v := img.ID.Version(); v != 7 {
		t.Errorf("UUID version = %d, want 7", v)
	}
}

// アバターは WebP になり、拡張子も揃うこと。
func TestNewPending_AvatarIsWebP(t *testing.T) {
	t.Parallel()

	img, err := NewPending(1, KindAvatar, 256, 256, 3_000)
	if err != nil {
		t.Fatalf("NewPending が失敗した: %v", err)
	}
	if img.ContentType != "image/webp" {
		t.Errorf("ContentType = %q, want image/webp", img.ContentType)
	}
	if !strings.HasSuffix(img.ObjectKey, ".webp") {
		t.Errorf("ObjectKey = %q, want .webp で終わる", img.ObjectKey)
	}
}

// **キーが毎回違うこと。** 同じなら他人のオブジェクトを上書きできる。
func TestNewPending_KeysAreUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for range 50 {
		img, err := NewPending(1, KindAvatar, 64, 64, 100)
		if err != nil {
			t.Fatalf("NewPending が失敗した: %v", err)
		}
		if seen[img.ObjectKey] {
			t.Fatalf("キーが重複した: %s", img.ObjectKey)
		}
		seen[img.ObjectKey] = true
	}
}

// **匿名は画像を投稿できない** (ADR 0007 の背景)。
//
// 匿名で任意のバイト列をストレージに置けると、容量の消費と
// 違法コンテンツの設置が追跡不能な形で可能になる。
// ここは 400 ではなく 401 で返す —— ログインすれば解決するため。
func TestNewPending_RequiresOwner(t *testing.T) {
	t.Parallel()

	for _, ownerID := range []int64{0, -1} {
		_, err := NewPending(ownerID, KindAvatar, 64, 64, 100)
		if !errors.Is(err, apperr.ErrUnauthenticated) {
			t.Errorf("ownerID=%d: err = %v, want apperr.ErrUnauthenticated", ownerID, err)
		}
	}
}

func TestNewPending_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		kind     Kind
		width    int
		height   int
		byteSize int64
	}{
		{name: "未知の用途", kind: "banner", width: 10, height: 10, byteSize: 1},
		{name: "幅が 0", kind: KindAvatar, width: 0, height: 10, byteSize: 1},
		{name: "高さが 0", kind: KindAvatar, width: 10, height: 0, byteSize: 1},
		{name: "幅が負", kind: KindAvatar, width: -1, height: 10, byteSize: 1},
		// 0 バイトは「PUT したつもりで何も書けていない」状態。
		{name: "空のバイト列", kind: KindAvatar, width: 10, height: 10, byteSize: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewPending(1, tt.kind, tt.width, tt.height, tt.byteSize)
			if !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Errorf("err = %v, want apperr.ErrInvalidArgument", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Commit
// ---------------------------------------------------------------------------

// **status と committed_at は必ず同時に動く。**
// 片方だけ書こうとすると DB の CHECK 制約が拒否する。
func TestImage_Commit(t *testing.T) {
	t.Parallel()

	img, err := NewPending(1, KindAvatar, 64, 64, 100)
	if err != nil {
		t.Fatalf("NewPending が失敗した: %v", err)
	}

	at := time.Unix(1_700_000_000, 0).UTC()
	img.Commit(at)

	if img.Status != StatusCommitted {
		t.Errorf("Status = %q, want committed", img.Status)
	}
	if img.CommittedAt == nil {
		t.Fatal("CommittedAt が入っていない")
	}
	if !img.CommittedAt.Equal(at) {
		t.Errorf("CommittedAt = %v, want %v", *img.CommittedAt, at)
	}
}

// 上限の値そのものを固定する。
//
// **画素数の上限を別に持つことが要点** (ADR 0007 決定 2)。
// バイト数だけでは decompression bomb を防げない。
func TestUploadLimits(t *testing.T) {
	t.Parallel()

	if MaxUploadBytes != 5<<20 {
		t.Errorf("MaxUploadBytes = %d, want 5 MiB", MaxUploadBytes)
	}
	if MaxPixels != 50_000_000 {
		t.Errorf("MaxPixels = %d, want 5000 万", MaxPixels)
	}
}
