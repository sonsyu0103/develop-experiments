package httpapi

import (
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

// UploadImage は POST /images を処理します。
//
// **ログインが必須です** (仕様書の security 宣言が強制します)。
// 匿名で任意のバイト列をストレージに置けると、容量の消費と
// 違法コンテンツの設置が追跡不能な形で可能になります
// (docs/adr/0007-image-storage.md の背景)。
func (s *Server) UploadImage(c *gin.Context) {
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

	// **本文の上限は bodyLimit ミドルウェアが張っています。**
	// ここで http.MaxBytesReader を張っても手遅れです ——
	// 仕様検証ミドルウェアが、この関数に入る前に本文を丸ごと読むため
	// (初版はここに置いていて、実測で何の効果も無かった)。
	//
	// **ここでエラーになることは無い** (実測)。仕様検証ミドルウェアが
	// 本文を読み切って multipart を解析済みで、しかも openapi3filter は
	// req.Body を bytes.Reader に差し替える。2 回目の呼び出しは
	// MultipartForm が埋まっているので即座に nil を返す。
	//
	//	壊れた multipart -> 400 "request body has an error: ... multipart: NextPart: EOF"
	//	chunked の超過   -> 413 "リクエストが大きすぎます"
	//
	// どちらも respondSpecError が返す。**MaxBytesError の枝は消した** ——
	// bodyLimit が張った MaxBytesReader はこの時点で差し替えられているので
	// 発火しないうえ、文言が上限を imageusecase.MaxUploadBytes (5 MiB) と
	// 取り違えていた (実際に効くのは maxImageUploadBytes = 5 MiB + 64 KiB)。
	// **到達しない枝が、間違った数字を持っていた。**
	if err := c.Request.ParseMultipartForm(multipartMemoryLimit); err != nil {
		respondBadRequest(c, "multipart/form-data として解釈できません: "+err.Error())
		return
	}

	raw, err := readUploadedFile(c)
	if err != nil {
		respondError(c, err)
		return
	}

	// **冪等キーは受け付けません** (仕様書からも外してあります)。
	//
	// ADR 0015 決定 2 は画像アップロードも対象に挙げていますが、
	// 冪等キーの記録は「応答を記録して再送に返す」仕組みであり、
	// 主トランザクションと同居させる必要があります (決定 3)。
	// 画像は DB とストレージの 2 システムにまたがるので、
	// **1 トランザクションに収まりません**。
	//
	// 初版は仕様書にヘッダを宣言したまま無視していました。
	// **仕様書が単一の情報源である以上、実装しないものは書かない**
	// —— 書いてあると、クライアントは「再送すれば前回の結果が返る」と
	// 信じて実装します (レビュー指摘)。
	//
	// 二重アップロードで起きるのは「使われない画像が 1 枚増える」ことだけで、
	// コメントの二重投稿のような不可逆な結果にはなりません
	// (添付されなかった画像は pending ではなく committed なので、
	//  現在の回収バッチの対象外という別の問題は残ります —— 未決事項)。

	// **用途の検証はユースケース層が行う。** HTTP 層はドメインの型を持たない。
	//
	// **PostFormValue であること。** FormValue は r.Form を見るが、
	// ParseMultipartForm は先に ParseForm を呼んでクエリ文字列を入れ、
	// そこへ multipart の値を**後ろに足す**。Get は先頭を返すので、
	// **URL に ?kind= を付けるだけで本文の kind を上書きできた** (実測):
	//
	//	POST /images?kind=avatar  (本文は kind=comment_attachment)
	//	  -> avatar として保存される
	//
	// 仕様検証は multipart 側の値を見て通すので、検証を抜けた値と
	// 実際に使う値が食い違う。PostFormValue は r.PostForm を見るため、
	// クエリ文字列は入らない。
	dto, err := s.images.Upload(ctx, *ownerID, c.Request.PostFormValue("kind"), raw)
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

// SetMyAvatar は PUT /me/avatar を処理します。
//
// **ログインが必須です** (仕様書の security 宣言が強制します)。
func (s *Server) SetMyAvatar(c *gin.Context) {
	ctx := c.Request.Context()
	userID := authorIDFromContext(ctx)
	if userID == nil {
		respondError(c, fmt.Errorf("ログインが必要です: %w", apperr.ErrUnauthenticated))
		return
	}

	var req oapigen.SetMyAvatarJSONRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "リクエストボディが不正です: "+err.Error())
		return
	}

	// **null は「解除」を意味します。** 省略と区別する必要はありません
	// (仕様書で imageId を required にしてあるため)。
	imageID, err := parseImageID(req.ImageId)
	if err != nil {
		respondError(c, err)
		return
	}

	me, err := s.sessions.SetAvatar(ctx, *userID, imageID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toWireMe(*me))
}
