package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	imageusecase "develop-experiments/apps/go-api/internal/image/usecase"
)

// multipartMemoryLimit はマルチパートの解析でメモリに置く上限です。
//
// これを超えた分は一時ファイルへ退避されます。**受け入れる上限とは別物**で、
// サイズの拒否は下の maxUploadRequestBytes と、ドメイン側の
// model.MaxUploadBytes が行います。
const multipartMemoryLimit = 1 << 20 // 1 MiB

// maxUploadRequestBytes はリクエスト全体として読む上限です。
//
// **本文を読み切る前に打ち切ります。** ドメイン側の検証 (model.MaxUploadBytes) は
// 「読み終えたバイト列」に対して働くので、それだけだと
// 巨大な本文を最後まで受け取ってから捨てることになります。
//
// 画像本体 5 MiB に、マルチパートの境界・ヘッダ・kind フィールドの
// 余地を足した値にしています。
const maxUploadRequestBytes = imageusecase.MaxUploadBytes + (1 << 16) // 5 MiB + 64 KiB

// UploadImage は POST /images を処理します。
//
// **ログインが必須です** (仕様書の security 宣言が強制します)。
// 匿名で任意のバイト列をストレージに置けると、容量の消費と
// 違法コンテンツの設置が追跡不能な形で可能になります
// (docs/adr/0007-image-storage.md の背景)。
func (s *Server) UploadImage(c *gin.Context, params oapigen.UploadImageParams) {
	if err := s.requireImagesEnabled(); err != nil {
		respondError(c, err)
		return
	}

	ctx := c.Request.Context()
	ownerID := authorIDFromContext(ctx)
	if ownerID == nil {
		// 到達しない想定。security 宣言と resolveSession の
		// どちらかが外れたときだけここに来る。
		respondError(c, fmt.Errorf("ログインが必要です: %w", apperr.ErrUnauthenticated))
		return
	}

	// **本文を読み切る前に上限で打ち切る。**
	// MaxBytesReader は超過時に読み出しをエラーにするので、
	// 5 MiB を超える本文をメモリにも一時ファイルにも溜めない。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadRequestBytes)

	if err := c.Request.ParseMultipartForm(multipartMemoryLimit); err != nil {
		// MaxBytesReader の超過はここに現れる。413 として返す
		// —— 400 だと「直せば通る」のか「大きすぎる」のかが伝わらない。
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respondError(c, fmt.Errorf(
				"画像が大きすぎます (上限は %d バイト): %w",
				imageusecase.MaxUploadBytes, apperr.ErrPayloadTooLarge))
			return
		}
		respondBadRequest(c, "multipart/form-data として解釈できません: "+err.Error())
		return
	}

	raw, err := readUploadedFile(c)
	if err != nil {
		respondError(c, err)
		return
	}

	// **冪等キーはここでは扱いません。**
	//
	// ADR 0015 決定 2 は画像アップロードも対象に挙げていますが、
	// 冪等キーの記録は「応答を記録して再送に返す」仕組みであり、
	// 主トランザクションと同居させる必要があります (決定 3)。
	// 画像は DB とストレージの 2 システムにまたがるので、
	// **1 トランザクションに収まりません**。
	//
	// 二重アップロードで起きるのは「使われない画像が 1 枚増える」ことだけで、
	// コメントの二重投稿のような不可逆な結果にはなりません
	// (添付されなかった画像は pending ではなく committed なので、
	//  現在の回収バッチの対象外という別の問題は残ります —— 未決事項)。
	//
	// params は仕様書がヘッダを宣言しているため受け取りますが、使いません。
	_ = params

	// **用途の検証はユースケース層が行う。** HTTP 層はドメインの型を持たない。
	dto, err := s.images.Upload(ctx, *ownerID, c.Request.FormValue("kind"), raw)
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusCreated, toWireImage(dto))
}

// readUploadedFile は file フィールドの中身を読み出します。
func readUploadedFile(c *gin.Context) ([]byte, error) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("file フィールドがありません: %w", apperr.ErrInvalidArgument)
	}
	defer func() { _ = file.Close() }()

	// **Content-Length に相当する値を先に見る。**
	// 読んでから判定してもよいが、明らかに超えているものを
	// 読み切る理由がない。中身の判定はドメイン側が別に行う。
	if header.Size > imageusecase.MaxUploadBytes {
		return nil, fmt.Errorf(
			"画像が大きすぎます (%d バイト。上限は %d バイト): %w",
			header.Size, imageusecase.MaxUploadBytes, apperr.ErrPayloadTooLarge)
	}

	// 申告された Size を信用せず、読み出し側でも上限で切る。
	// +1 して読むことで「上限ちょうど」と「超過」を区別する。
	raw, err := io.ReadAll(io.LimitReader(file, imageusecase.MaxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("画像を読み取れません: %w", apperr.ErrInvalidArgument)
	}
	if int64(len(raw)) > imageusecase.MaxUploadBytes {
		return nil, fmt.Errorf(
			"画像が大きすぎます (上限は %d バイト): %w",
			imageusecase.MaxUploadBytes, apperr.ErrPayloadTooLarge)
	}
	return raw, nil
}

// requireImagesEnabled はストレージの設定が入っているかを確認します。
//
// 認証と同じ考え方です (ADR 0005 決定 4)。設定が無くても API は起動し、
// **画像の経路だけが 503** になります。掲示板の閲覧・投稿・ログインは
// 画像に依存しません。
func (s *Server) requireImagesEnabled() error {
	if s.images == nil {
		return fmt.Errorf("画像ストレージが設定されていません: %w", apperr.ErrUnavailable)
	}
	return nil
}

// toWireImage は DTO を仕様書の型へ詰め替えます。
func toWireImage(d *imageusecase.ImageDTO) *oapigen.Image {
	if d == nil {
		return nil
	}
	return &oapigen.Image{
		Id:     d.ID,
		Url:    d.URL,
		Width:  d.Width,
		Height: d.Height,
	}
}

// parseImageID はコメントに添付する画像 ID を解釈します。
//
// **ゼロ値の UUID を弾きます。** JSON で "imageId": "0000...0" を
// 送られた場合、そのまま検索すると必ず 404 になりますが、
// 「指定していない」との区別が付かないほうが困ります。
func parseImageID(raw *openapi_types.UUID) (*uuid.UUID, error) {
	if raw == nil {
		return nil, nil
	}
	// openapi_types.UUID は google/uuid.UUID の別名なので変換は要らない。
	id := *raw
	if id == uuid.Nil {
		return nil, fmt.Errorf("imageId が不正です: %w", apperr.ErrInvalidArgument)
	}
	return &id, nil
}
