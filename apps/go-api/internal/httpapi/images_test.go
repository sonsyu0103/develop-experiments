package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/config"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	imagemodel "develop-experiments/apps/go-api/internal/image/domain/model"
	imagerepo "develop-experiments/apps/go-api/internal/image/domain/repository"
	imageusecase "develop-experiments/apps/go-api/internal/image/usecase"
	moderationusecase "develop-experiments/apps/go-api/internal/moderation/usecase"
	threadmodel "develop-experiments/apps/go-api/internal/thread/domain/model"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
	"develop-experiments/apps/go-api/internal/viewcount"
)

// ---------------------------------------------------------------------------
// フェイク
// ---------------------------------------------------------------------------

type fakeImageRepo struct {
	stored map[uuid.UUID]*imagemodel.Image
}

var _ imagerepo.ImageRepository = (*fakeImageRepo)(nil)

func (f *fakeImageRepo) CreatePending(
	_ context.Context, img *imagemodel.Image,
) (*imagemodel.Image, error) {
	cp := *img
	f.stored[cp.ID] = &cp
	return &cp, nil
}

func (f *fakeImageRepo) Commit(_ context.Context, id uuid.UUID) (*imagemodel.Image, error) {
	img := f.stored[id]
	img.Status = imagemodel.StatusCommitted
	now := time.Unix(0, 0).UTC()
	img.CommittedAt = &now
	return img, nil
}

func (f *fakeImageRepo) FindByID(_ context.Context, id uuid.UUID) (*imagemodel.Image, error) {
	if img, ok := f.stored[id]; ok {
		return img, nil
	}
	return nil, fmt.Errorf("fake: %w", apperr.ErrNotFound)
}

// 回収バッチ用。この環境では回収を回さないので、素直な実装で足りる。
func (f *fakeImageRepo) WithinTx(_ context.Context, fn func(imagerepo.ImageRepository) error) error {
	return fn(f)
}

func (f *fakeImageRepo) ListReclaimable(
	context.Context, time.Duration, int32,
) ([]imagemodel.Image, error) {
	return nil, nil
}

func (f *fakeImageRepo) MarkReclaimed(context.Context, uuid.UUID) error { return nil }

func (f *fakeImageRepo) Delete(_ context.Context, id uuid.UUID) error {
	delete(f.stored, id)
	return nil
}

type fakeObjectStorage struct{ objects map[string][]byte }

var _ imagerepo.ObjectStorage = (*fakeObjectStorage)(nil)

func (f *fakeObjectStorage) Put(_ context.Context, key, _ string, body []byte) error {
	f.objects[key] = body
	return nil
}
func (f *fakeObjectStorage) Delete(_ context.Context, key string) error {
	delete(f.objects, key)
	return nil
}
func (f *fakeObjectStorage) URL(key string) string { return "https://cdn.example.test/" + key }

// ---------------------------------------------------------------------------
// ヘルパ
// ---------------------------------------------------------------------------

type imageEnv struct {
	router   *gin.Engine
	images   *fakeImageRepo
	storage  *fakeObjectStorage
	comments *fakeCommentRepo
	token    usermodel.SessionToken
}

// newImageEnv は画像を有効にしたルータを組み立てます。
func newImageEnv(t *testing.T) *imageEnv {
	t.Helper()

	const token = usermodel.SessionToken("image-test-token")
	publicID := uuid.MustParse("01920000-0000-7000-8000-000000000001")

	sessions := &fakeSessionRepo{
		liveToken: token,
		owner: usermodel.SessionOwner{
			ID: 1, PublicID: publicID, Email: "h@example.com",
			DisplayName: "ホシノ", Role: usermodel.RoleUser,
		},
	}

	imageRepo := &fakeImageRepo{stored: map[uuid.UUID]*imagemodel.Image{}}
	storage := &fakeObjectStorage{objects: map[string][]byte{}}
	imageInteractor := imageusecase.NewImageInteractor(imageRepo, storage, nil)

	threads := &fakeThreadRepo{
		summaries: []threadmodel.Summary{
			{Thread: *threadmodel.Reconstruct(1, "スレッド", nil, nil, time.Unix(1, 0).UTC(), 0)},
		},
	}
	comments := &fakeCommentRepo{}

	router, err := NewRouter(Deps{
		Server: NewServer(
			threadusecase.NewThreadInteractor(threads, nil),
			commentusecase.NewCommentInteractor(comments, threads, imageInteractor),
			&fakePinger{},
			// **本番と同じく画像の解決を渡す** (cmd/api/main.go の
			// sessionImageResolver)。nil にすると SetAvatar が
			// ErrUnavailable で短絡し、PUT /me/avatar の検査が
			// 503 しか見られなくなる (レビュー指摘)。
			userusecase.NewSessionInteractor(sessions, imageInteractor),
			nil,
			imageInteractor,
			moderationusecase.NewInteractor(newFakeModerationRepo()),
			moderationusecase.NewReportInteractor(newFakeReportRepo(), newFakeReportRepo()),
			// 問い合わせの受付は設定に依存せず常に結線する (ADR 0008 決定 1)。
			newTestContactInteractor(),
			viewcount.New(),
			config.AuthConfig{FrontendURL: "http://localhost:3000"}),
		AllowedOrigins: []string{"http://localhost:3000"},
	})
	if err != nil {
		t.Fatalf("NewRouter が失敗した: %v", err)
	}

	return &imageEnv{
		router: router, images: imageRepo, storage: storage,
		comments: comments, token: token,
	}
}

// testPNG はテスト用の PNG を作ります。
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("テスト用 PNG の生成に失敗した: %v", err)
	}
	return buf.Bytes()
}

// uploadRequest はマルチパートのアップロード要求を組み立てます。
func uploadRequest(t *testing.T, kind string, body []byte, cookies ...*http.Cookie) *http.Request {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if kind != "" {
		if err := w.WriteField("kind", kind); err != nil {
			t.Fatalf("kind の書き込みに失敗した: %v", err)
		}
	}
	if body != nil {
		part, err := w.CreateFormFile("file", "upload.png")
		if err != nil {
			t.Fatalf("file パートの作成に失敗した: %v", err)
		}
		if _, err := part.Write(body); err != nil {
			t.Fatalf("file の書き込みに失敗した: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("multipart の終端に失敗した: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images", &buf)
	// **Origin が要る** (ADR 0013 決定 1 の CSRF 対策)。
	// 付けないと csrfGuard が 403 で打ち切り、ここから先を検査できない。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", w.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return req
}

func (e *imageEnv) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// アップロード
// ---------------------------------------------------------------------------

func TestUploadImage_Succeeds(t *testing.T) {
	env := newImageEnv(t)

	rec := env.do(uploadRequest(t, "comment_attachment", testPNG(t, 64, 64), sessionCookie(env.token)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	got := decodeJSON[oapigen.Image](t, rec)
	if got.Id == uuid.Nil {
		t.Error("id が空")
	}
	// **絶対 URL を返す** (ADR 0007 決定 5)。
	if !strings.HasPrefix(got.Url, "https://") {
		t.Errorf("url = %q, want 絶対 URL", got.Url)
	}
	if got.Width != 64 || got.Height != 64 {
		t.Errorf("寸法 = %dx%d, want 64x64", got.Width, got.Height)
	}

	// **オブジェクトキーを応答に出さない。**
	// 出すとフロントが URL を組み立てられてしまい、決定 5 の意味が薄れる。
	if strings.Contains(rec.Body.String(), "objectKey") {
		t.Errorf("応答にオブジェクトキーが含まれている: %s", rec.Body.String())
	}
	if len(env.storage.objects) != 1 {
		t.Errorf("ストレージの件数 = %d, want 1", len(env.storage.objects))
	}
}

// **未ログインでは 401。** 匿名で任意のバイト列を置けると、
// 容量の消費と違法コンテンツの設置が追跡不能になる (ADR 0007 の背景)。
func TestUploadImage_RequiresLogin(t *testing.T) {
	env := newImageEnv(t)

	rec := env.do(uploadRequest(t, "comment_attachment", testPNG(t, 32, 32)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(env.storage.objects) != 0 {
		t.Error("未ログインなのにストレージへ書かれた")
	}
}

// 許可していない形式は 400。SVG が代表例。
func TestUploadImage_RejectsDisallowedFormat(t *testing.T) {
	env := newImageEnv(t)

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	rec := env.do(uploadRequest(t, "comment_attachment", svg, sessionCookie(env.token)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(env.storage.objects) != 0 {
		t.Error("弾いたはずなのにストレージへ書かれた")
	}
}

// 上限を超えるバイト数は 413。
//
// **400 と分ける。** 「直せば通る」のか「大きすぎる」のかで、
// クライアントの対処が変わる。
func TestUploadImage_RejectsTooLarge(t *testing.T) {
	env := newImageEnv(t)

	huge := make([]byte, imageusecase.MaxUploadBytes+1024)
	copy(huge, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})

	rec := env.do(uploadRequest(t, "comment_attachment", huge, sessionCookie(env.token)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.PAYLOADTOOLARGE {
		t.Errorf("code = %q, want PAYLOAD_TOO_LARGE", code)
	}
}

// 未知の用途は受け付けない。
func TestUploadImage_RejectsUnsupportedKind(t *testing.T) {
	env := newImageEnv(t)

	for _, kind := range []string{"banner", "", "COMMENT_ATTACHMENT"} {
		t.Run("kind="+kind, func(t *testing.T) {
			rec := env.do(uploadRequest(t, kind, testPNG(t, 32, 32), sessionCookie(env.token)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUploadImage_RejectsMissingFile(t *testing.T) {
	env := newImageEnv(t)

	rec := env.do(uploadRequest(t, "comment_attachment", nil, sessionCookie(env.token)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// **ストレージの設定が無ければ 503。**
// 認証と同じ形 (ADR 0005 決定 4) で、画像の経路だけが落ちる。
func TestUploadImage_DisabledReturns503(t *testing.T) {
	env := newAuthEnv(t, true) // 画像を結線していない環境

	req := uploadRequest(t, "comment_attachment", testPNG(t, 32, 32), sessionCookie(env.token))
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.UNAVAILABLE {
		t.Errorf("code = %q, want UNAVAILABLE", code)
	}
}

// 設定が無くても掲示板は読める。画像だけを落とす設計の要点。
func TestUploadImage_DisabledStillServesThreads(t *testing.T) {
	env := newAuthEnv(t, true)

	rec := env.do(t, http.MethodGet, "/threads")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// コメントへの添付
// ---------------------------------------------------------------------------

// アップロード -> 添付が通ること。
func TestCreateComment_AttachesImage(t *testing.T) {
	env := newImageEnv(t)

	rec := env.do(uploadRequest(t, "comment_attachment", testPNG(t, 64, 64), sessionCookie(env.token)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("アップロードが失敗した: %s", rec.Body.String())
	}
	uploaded := decodeJSON[oapigen.Image](t, rec)

	body := `{"body":"画像つきの投稿","imageId":"` + uploaded.Id.String() + `"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/threads/1/comments",
		strings.NewReader(body))
	// **Origin が要る** (ADR 0013 決定 1 の CSRF 対策)。
	// 付けないと csrfGuard が 403 で打ち切り、ここから先を検査できない。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookie(env.token))

	rec = env.do(req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	// **応答にも画像が載ること。**
	//
	// 初版はリポジトリに渡った値しか見ておらず、
	// **仕様書の型への詰め替え (toWireComment) の書き忘れを見逃していた。**
	// 実 DB を通すスモークで初めて落ちた。
	// 「保存したか」と「返したか」は別なので、両方を見る。
	posted := decodeJSON[oapigen.Comment](t, rec)
	if posted.Image == nil {
		t.Fatalf("応答に画像が載っていない (body=%s)", rec.Body.String())
	}
	if posted.Image.Id != uploaded.Id {
		t.Errorf("応答の画像 = %v, want %v", posted.Image.Id, uploaded.Id)
	}
	if posted.Image.Url == "" {
		t.Error("応答の画像に URL が入っていない")
	}

	// **リポジトリに image_id が渡ったこと。**
	// 応答だけを見ると「詰め替えは正しいが保存していない」を見逃す。
	if env.comments.created == nil {
		t.Fatal("コメントが保存されていない")
	}
	if env.comments.created.ImageID == nil {
		t.Fatal("image_id が渡っていない")
	}
	if *env.comments.created.ImageID != uploaded.Id {
		t.Errorf("image_id = %v, want %v", *env.comments.created.ImageID, uploaded.Id)
	}
}

// **他人の画像は添付できない。** 404 にするのは存在を隠すため
// (docs/adr/0013-http-defense.md)。
func TestCreateComment_RejectsOthersImage(t *testing.T) {
	env := newImageEnv(t)

	// 別人 (owner_id = 99) の画像を直接置く。
	other, err := imagemodel.NewPending(99, imagemodel.KindCommentAttachment, 10, 10, 100)
	if err != nil {
		t.Fatalf("NewPending が失敗した: %v", err)
	}
	other.Status = imagemodel.StatusCommitted
	env.images.stored[other.ID] = other

	body := `{"body":"他人の画像を添付","imageId":"` + other.ID.String() + `"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/threads/1/comments",
		strings.NewReader(body))
	// **Origin が要る** (ADR 0013 決定 1 の CSRF 対策)。
	// 付けないと csrfGuard が 403 で打ち切り、ここから先を検査できない。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookie(env.token))

	rec := env.do(req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	// **投稿されていないこと。** 添付だけ落として投稿が通ると、
	// 利用者からは「画像が消えた」ようにしか見えない。
	if env.comments.created != nil {
		t.Error("他人の画像を指定したのに投稿された")
	}
}

// **匿名は画像を添付できない** (ADR 0007 の背景)。
func TestCreateComment_AnonymousCannotAttachImage(t *testing.T) {
	env := newImageEnv(t)

	rec := env.do(uploadRequest(t, "comment_attachment", testPNG(t, 32, 32), sessionCookie(env.token)))
	uploaded := decodeJSON[oapigen.Image](t, rec)

	body := `{"body":"匿名で画像を添付","imageId":"` + uploaded.Id.String() + `"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/threads/1/comments",
		strings.NewReader(body))
	// **Origin が要る** (ADR 0013 決定 1 の CSRF 対策)。
	// 付けないと csrfGuard が 403 で打ち切り、ここから先を検査できない。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	// Cookie を付けない = 匿名

	rec = env.do(req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if env.comments.created != nil {
		t.Error("匿名なのに画像つきで投稿された")
	}
}

// 画像を指定しない投稿はこれまでどおり通ること。
func TestCreateComment_WithoutImageStillWorks(t *testing.T) {
	env := newImageEnv(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/threads/1/comments",
		strings.NewReader(`{"body":"画像なし"}`))
	// **Origin が要る** (ADR 0013 決定 1 の CSRF 対策)。
	// 付けないと csrfGuard が 403 で打ち切り、ここから先を検査できない。
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")

	rec := env.do(req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if env.comments.created.ImageID != nil {
		t.Error("画像を指定していないのに image_id が入っている")
	}
}

// **URL のクエリ文字列で、本文の kind を上書きできないこと。**
//
// FormValue は r.Form を見る。ParseMultipartForm は先に ParseForm を呼んで
// クエリ文字列を入れ、そこへ multipart の値を**後ろに足す**ので、
// Get は先頭 = クエリの値を返していた。仕様検証は multipart 側を見て通すため、
// **検証を抜けた値と、実際に使う値が食い違う**:
//
//	POST /images?kind=avatar  (本文は kind=comment_attachment) -> avatar で保存
//	POST /images?kind=not_a_real_kind                          -> 400
func TestUploadImage_QueryCannotOverrideKind(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "別の用途を指す", query: "?kind=avatar"},
		{name: "存在しない用途を指す", query: "?kind=not_a_real_kind"},
		{name: "空の指定", query: "?kind="},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newImageEnv(t)

			base := uploadRequest(t, "comment_attachment", testPNG(t, 64, 64), sessionCookie(env.token))
			body, err := io.ReadAll(base.Body)
			if err != nil {
				t.Fatalf("本文を読めない: %v", err)
			}
			// **URL を組み立て直す。** httptest.NewRequest のあとで
			// RawQuery を書き換えても RequestURI 側に載らず、届かない。
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"/images"+tt.query, bytes.NewReader(body))
			req.Header = base.Header.Clone()
			for _, c := range base.Cookies() {
				req.AddCookie(c)
			}

			rec := env.do(req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (クエリで弾かれた) body=%s", rec.Code, rec.Body.String())
			}
			got := decodeJSON[oapigen.Image](t, rec)
			stored := env.images.stored[got.Id]
			if stored == nil {
				t.Fatal("保存されていない")
			}
			if string(stored.Kind) != "comment_attachment" {
				t.Errorf("保存された kind = %q, want comment_attachment (クエリが本文を上書きした)",
					stored.Kind)
			}
		})
	}
}

// **壊れた multipart は仕様検証が 400 で返すこと。**
//
// ハンドラの ParseMultipartForm はここまで来ない (検証ミドルウェアが
// 本文を読み切って解析済みで、req.Body も差し替わっている)。
// 到達しない枝に MaxBytesError の処理と**間違った上限値**が残っていたので、
// 実際に返っている経路を検査で固定する。
func TestUploadImage_MalformedMultipartIs400(t *testing.T) {
	env := newImageEnv(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/images",
		bytes.NewReader([]byte("これは multipart ではない")))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.AddCookie(sessionCookie(env.token))

	rec := env.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != oapigen.INVALIDARGUMENT {
		t.Errorf("code = %q, want INVALID_ARGUMENT", code)
	}
}

// ---------------------------------------------------------------------------
// アバターの設定 (PUT /me/avatar)
// ---------------------------------------------------------------------------

// **この経路には HTTP 層の検査が 1 本も無かった** (レビュー指摘)。
//
// フロントの e2e は `page.route('**/me/avatar', …)` で差し替えているので、
// 要求は Go まで届かない。しかも newImageEnv は本番と違って画像の解決を
// 渡していなかったため、仮に検査を足しても SetAvatar が
// ErrUnavailable で短絡し、**503 しか見られなかった** (そちらも直した)。
func TestSetMyAvatar(t *testing.T) {
	setAvatar := func(t *testing.T, env *imageEnv, body string) *httptest.ResponseRecorder {
		t.Helper()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/me/avatar",
			strings.NewReader(body))
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sessionCookie(env.token))
		return env.do(req)
	}

	// 自分の avatar 画像を上げて、それを設定できること。
	t.Run("設定できる", func(t *testing.T) {
		env := newImageEnv(t)

		rec := env.do(uploadRequest(t, "avatar", testPNG(t, 64, 64), sessionCookie(env.token)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("アップロード status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		uploaded := decodeJSON[oapigen.Image](t, rec)

		rec = setAvatar(t, env, `{"imageId":"`+uploaded.Id.String()+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		me := decodeJSON[oapigen.Me](t, rec)
		if me.AvatarUrl == nil || *me.AvatarUrl == "" {
			t.Fatal("avatarUrl が空 (設定が反映されていない)")
		}
		// **絶対 URL を返す** (ADR 0007 決定 5)。
		if !strings.HasPrefix(*me.AvatarUrl, "https://") {
			t.Errorf("avatarUrl = %q, want 絶対 URL", *me.AvatarUrl)
		}
	})

	// **null は「解除」。** 省略と区別する必要はない (imageId は required)。
	t.Run("null で解除できる", func(t *testing.T) {
		env := newImageEnv(t)

		rec := setAvatar(t, env, `{"imageId":null}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// ゼロ値の UUID は「指定していない」と区別が付かないので弾く。
	t.Run("ゼロ値の UUID は 400", func(t *testing.T) {
		env := newImageEnv(t)

		rec := setAvatar(t, env, `{"imageId":"00000000-0000-0000-0000-000000000000"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// **用途が違う画像は 404。** 添付用に上げた画像をアバターにはできない
	// (EnsureOwned が kind まで見る)。
	t.Run("用途が違う画像は 404", func(t *testing.T) {
		env := newImageEnv(t)

		rec := env.do(uploadRequest(t, "comment_attachment", testPNG(t, 64, 64), sessionCookie(env.token)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("アップロード status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		uploaded := decodeJSON[oapigen.Image](t, rec)

		rec = setAvatar(t, env, `{"imageId":"`+uploaded.Id.String()+`"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// 存在しない画像も 404。
	t.Run("知らない画像は 404", func(t *testing.T) {
		env := newImageEnv(t)

		rec := setAvatar(t, env, `{"imageId":"01920000-0000-7000-8000-0000000000ff"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}
