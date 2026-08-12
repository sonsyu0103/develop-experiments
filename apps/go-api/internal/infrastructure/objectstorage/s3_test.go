package objectstorage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"develop-experiments/apps/go-api/internal/config"
)

// ---------------------------------------------------------------------------
// URL の組み立て (実ストレージを要しない)
// ---------------------------------------------------------------------------

// **URL は絶対 URL でなければならない** (docs/adr/0007-image-storage.md 決定 5)。
// キーだけを返すとフロントが CDN のベース URL を知る必要が出る。
func TestURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		key     string
		want    string
	}{
		{
			name:    "素直な組み立て",
			baseURL: "https://cdn.example.com",
			key:     "images/018f2c00-0000-7000-8000-000000000001.webp",
			want:    "https://cdn.example.com/images/018f2c00-0000-7000-8000-000000000001.webp",
		},
		{
			// **末尾のスラッシュで二重にならないこと。**
			// 設定値は人が書くので、付いている場合と付いていない場合の両方が来る。
			name:    "ベース URL の末尾スラッシュを吸収する",
			baseURL: "http://localhost:9000/bbs-images/",
			key:     "images/a.jpg",
			want:    "http://localhost:9000/bbs-images/images/a.jpg",
		},
		{
			// スラッシュは区切りとして残す。エスケープすると階層が壊れる。
			name:    "キーのスラッシュは区切りのまま",
			baseURL: "https://cdn.example.com",
			key:     "images/nested/a.jpg",
			want:    "https://cdn.example.com/images/nested/a.jpg",
		},
		{
			// 現在のキーは UUID + 拡張子なので空白は現れない。
			// **キーの作り方が変わったときに URL が壊れないこと**を見る。
			name:    "エスケープが要る文字を通す",
			baseURL: "https://cdn.example.com",
			key:     "images/a b.jpg",
			want:    "https://cdn.example.com/images/a%20b.jpg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &S3Storage{publicBaseURL: trimBase(tt.baseURL)}
			if got := s.URL(tt.key); got != tt.want {
				t.Errorf("URL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// trimBase は New が行う正規化と同じ処理です。
//
// **New を通さずに構造体を組み立てるため、ここで再現しています。**
// config.StorageConfig を組み立てて New を呼ぶと AWS SDK の初期化が走り、
// 資格情報の探索でネットワークに出ることがあります (URL の検査には不要)。
// 正規化そのものは下の TestNew_TrimsTrailingSlash が New 経由で検査します。
func trimBase(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// New が設定を検証すること。
//
// **揃っていない設定で黙って動かさない。** バケットが空のまま起動すると、
// 最初のアップロードまで気づけない。
func TestNew_RejectsIncompleteConfig(t *testing.T) {
	t.Parallel()

	tests := map[string]config.StorageConfig{
		"すべて空":           {},
		"bucket が無い":     {PublicBaseURL: "https://cdn.example.com"},
		"public_url が無い": {Bucket: "b"},
	}

	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := New(context.Background(), cfg); err == nil {
				t.Error("不完全な設定なのに成功した")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 実 MinIO に対する検証
// ---------------------------------------------------------------------------
//
// **フェイクでは確かめられない部分がここにある。**
//   - path-style が要ること (virtual-hosted style では bucket.localhost を引けない)
//   - Content-Type が保存され、配信時に返ること
//   - 存在しないキーの DELETE が成功として扱われること
//
// S3_TEST_ENDPOINT が無ければスキップします。CI では migration-ci が
// MinIO を立てて設定します (make smoke と同じ「実物に当てる」層)。
//
// **CI ではスキップを失敗として扱います。**
// go test は全件 SKIP でも exit 0 になるため、S3_TEST_ENDPOINT を
// 消す / 打ち間違える / MinIO の起動ステップを削る、のいずれでも
// 検証が消滅したまま CI が緑のままになります。
// スモークの SMOKE_REQUIRE_FULL と同じ性質の穴なので、同じ形で塞ぎます
// (docs/adr/0005-authentication.md 決定 4 の経緯)。

// liveConfig は環境変数から実 MinIO の設定を組み立てます。
//
// 未設定なら通常はスキップしますが、CI (または S3_TEST_REQUIRE=1) では
// 失敗させます。fail-closed 側に倒すのは、
// **明示的な env を必須にするとその 1 行を消しただけで検出が止まる**ためです。
func liveConfig(t *testing.T) config.StorageConfig {
	t.Helper()

	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		if requireLive() {
			t.Fatal("S3_TEST_ENDPOINT が未設定です。" +
				"CI では実 MinIO に対する検証を省略できません " +
				"(意図的に飛ばすなら S3_TEST_REQUIRE=0)")
		}
		t.Skip("S3_TEST_ENDPOINT が未設定のためスキップ (実 MinIO が必要)")
	}

	return config.StorageConfig{
		Bucket:          envOr("S3_TEST_BUCKET", "bbs-images"),
		Region:          envOr("S3_TEST_REGION", "ap-northeast-1"),
		Endpoint:        endpoint,
		UsePathStyle:    true,
		AccessKeyID:     envOr("S3_TEST_ACCESS_KEY_ID", "minioadmin"),
		SecretAccessKey: envOr("S3_TEST_SECRET_ACCESS_KEY", "minioadmin"),
		PublicBaseURL:   envOr("S3_TEST_PUBLIC_BASE_URL", endpoint+"/"+envOr("S3_TEST_BUCKET", "bbs-images")),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// requireLive は実 MinIO への検証を必須とするかを返します。
//
// 既定は「CI なら必須」。スモークの SMOKE_REQUIRE_FULL と同じ判断で、
// 明示的な env を必須にすると、その 1 行を消しただけで検出が止まります。
func requireLive() bool {
	if raw, ok := os.LookupEnv("S3_TEST_REQUIRE"); ok {
		return strings.TrimSpace(raw) != "" &&
			strings.TrimSpace(raw) != "0" &&
			!strings.EqualFold(strings.TrimSpace(raw), "false")
	}
	return os.Getenv("CI") != ""
}

// PUT した内容が、返した URL でそのまま読めること。
//
// **ここが ADR 0007 決定 5 の実質的な検査になる。**
// URL の組み立てが正しくても、path-style の設定を落としていると
// 実際には読めない。組み立てだけを単体で見ていると気づけない。
func TestS3Storage_PutThenReadThroughURL(t *testing.T) {
	cfg := liveConfig(t)
	ctx := context.Background()

	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New が失敗した: %v", err)
	}

	const key = "images/probe-put-then-read.jpg"
	body := []byte("これは画像のバイト列ではないが、保存と取得の経路は同じ")

	if putErr := s.Put(ctx, key, "image/jpeg", body); putErr != nil {
		t.Fatalf("Put が失敗した: %v", putErr)
	}
	t.Cleanup(func() {
		if delErr := s.Delete(context.Background(), key); delErr != nil {
			t.Errorf("後片付けの Delete が失敗した: %v", delErr)
		}
	})

	res, err := http.Get(s.URL(key)) //nolint:gosec,noctx // 検証用。URL は自前で組んだ値
	if err != nil {
		t.Fatalf("配信 URL を引けなかった (%s): %v", s.URL(key), err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (url=%s)", res.StatusCode, s.URL(key))
	}
	// **Content-Type が保存されていること。**
	// 落とすと、ブラウザが中身から型を推測する余地が生まれる
	// (ADR 0013 決定 2 の nosniff は多層防御の一枚目でしかない)。
	if ct := res.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}

	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("本文を読めなかった: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("内容が一致しない (%d バイト読めた, want %d)", len(got), len(body))
	}
}

// **存在しないキーの DELETE は成功として扱う。**
//
// 回収バッチは「S3 は消したが DB 行の更新前に落ちた」状態から再実行される。
// ここで失敗にすると、その行は永久に回収されない (ADR 0007 決定 3)。
func TestS3Storage_DeleteMissingKeySucceeds(t *testing.T) {
	cfg := liveConfig(t)
	ctx := context.Background()

	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New が失敗した: %v", err)
	}

	if err := s.Delete(ctx, "images/この鍵は存在しない.jpg"); err != nil {
		t.Errorf("存在しないキーの Delete が失敗した: %v", err)
	}
}

// Delete したオブジェクトが本当に消えること。
// 成功を返すだけで消していない実装でも上の 2 つは通ってしまう。
func TestS3Storage_DeleteRemovesObject(t *testing.T) {
	cfg := liveConfig(t)
	ctx := context.Background()

	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New が失敗した: %v", err)
	}

	const key = "images/probe-delete.webp"
	if putErr := s.Put(ctx, key, "image/webp", []byte("消える予定")); putErr != nil {
		t.Fatalf("Put が失敗した: %v", putErr)
	}
	if delErr := s.Delete(ctx, key); delErr != nil {
		t.Fatalf("Delete が失敗した: %v", delErr)
	}

	res, err := http.Get(s.URL(key)) //nolint:gosec,noctx // 同上
	if err != nil {
		t.Fatalf("配信 URL を引けなかった: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusOK {
		t.Errorf("削除したはずのオブジェクトが読めた (status=%d)", res.StatusCode)
	}
}

// New が公開 URL の末尾スラッシュを落とすこと (実物経由での確認)。
func TestNew_TrimsTrailingSlash(t *testing.T) {
	cfg := liveConfig(t)
	cfg.PublicBaseURL += "/"

	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New が失敗した: %v", err)
	}
	if got := s.URL("images/a.jpg"); !bytes.HasSuffix([]byte(got), []byte("/images/a.jpg")) ||
		bytes.Contains([]byte(got), []byte("//images/a.jpg")) {
		t.Errorf("URL = %q, スラッシュが二重になっている", got)
	}
}
