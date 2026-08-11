package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// newTestLogger は出力を捕まえられるロガーを組み立てます。
// Setup と同じ包み方 (ContextHandler + 共通フィールド) を再現します。
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewContextHandler(base).WithAttrs([]slog.Attr{
		slog.String("service", ServiceName),
		slog.String("version", "test"),
	})
	return slog.New(h)
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("ログを JSON として読めない: %v (raw=%s)", err, buf.String())
	}
	return got
}

// **相関 ID が自動で付くこと。**
//
// 各所で slog.String("request_id", ...) を書く形にすると必ず渡し忘れが出る。
// 渡し忘れたレコードは Athena で相関から外れ、
// しかも「欠けている」こと自体に気づけない。
func TestContextHandler_AddsRequestID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := WithRequestID(context.Background(), "req-123")

	newTestLogger(&buf).InfoContext(ctx, "http_request")

	got := decode(t, &buf)
	if got["request_id"] != "req-123" {
		t.Errorf("request_id = %v, want req-123", got["request_id"])
	}
}

// 共通フィールドが全レコードに乗ること (ADR 0010 の 4-2)。
// Athena はスキーマオンリードなので、揺れるとクエリが書けない。
//
// **NewHandler を通して検査する。** テスト用に組み直したロガーで見ると、
// 「テストの中でだけ共通フィールドが付いている」状態を検出できない。
func TestNewHandler_CommonFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slog.New(NewHandler(&buf, false)).InfoContext(context.Background(), "http_request")

	got := decode(t, &buf)
	for _, key := range []string{"time", "level", "msg", "service", "version"} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s が出ていない: %v", key, got)
		}
	}
	if got["service"] != ServiceName {
		t.Errorf("service = %v, want %s", got["service"], ServiceName)
	}
}

// **未ログインでは user_id を出さない。**
//
// 0 を出すと「利用者 0 番」と区別できなくなる。
func TestContextHandler_OmitsUserIDWhenAnonymous(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	newTestLogger(&buf).InfoContext(context.Background(), "http_request")

	if _, ok := decode(t, &buf)["user_id"]; ok {
		t.Error("未ログインなのに user_id が出ている")
	}
}

// ログイン中は内部 ID が乗る。
// 「常に省く」実装でも上のテストは通ってしまうため、両方を見る。
func TestContextHandler_AddsUserID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := WithUserID(context.Background(), 42)

	newTestLogger(&buf).InfoContext(ctx, "http_request")

	if got := decode(t, &buf)["user_id"]; got != float64(42) {
		t.Errorf("user_id = %v, want 42", got)
	}
}

// **slog.With を通しても相関が消えないこと。**
//
// WithAttrs / WithGroup で包み直さないと、
// 内側のハンドラが素で返り、そこから先のログに request_id が付かなくなる。
// 「一部のログにだけ相関 ID が無い」という一番気づきにくい壊れ方をする。
func TestContextHandler_SurvivesWith(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := WithRequestID(context.Background(), "req-456")

	logger := newTestLogger(&buf).With(slog.String("component", "worker"))
	logger.InfoContext(ctx, "view_count_flushed")

	got := decode(t, &buf)
	if got["request_id"] != "req-456" {
		t.Errorf("With のあとで request_id が消えた: %v", got)
	}
	if got["component"] != "worker" {
		t.Errorf("component = %v", got["component"])
	}
}

// グループを開いても同じ。
func TestContextHandler_SurvivesWithGroup(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := WithRequestID(context.Background(), "req-789")

	logger := slog.New(newTestLogger(&buf).Handler().WithGroup("db"))
	logger.InfoContext(ctx, "query")

	if got := decode(t, &buf)["request_id"]; got != "req-789" {
		t.Errorf("WithGroup のあとで request_id が消えた: %v", got)
	}
}

// 版数が分からない環境では嘘の値を作らない。
func TestVersion_FallsBackToEnv(t *testing.T) {
	t.Setenv("APP_VERSION", "abc1234")

	if got := Version(); got != "abc1234" {
		t.Errorf("Version() = %q, want abc1234", got)
	}
}

// **コンストラクタを通さず組み立てても落ちないこと。**
//
// 構造体リテラルで作ると root が nil になる。そのままグループを開くと
// Handle が nil 参照で落ちていた (テストを書いていて踏んだ)。
// ログの出力経路がパニックするのは、本番でいちばん困る壊れ方になる。
func TestContextHandler_ZeroValueIsSafe(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})

	logger := slog.New(ContextHandler{Handler: base}.WithGroup("db"))
	logger.InfoContext(WithRequestID(context.Background(), "req-zero"), "query")

	if got := decode(t, &buf)["request_id"]; got != "req-zero" {
		t.Errorf("request_id = %v, want req-zero", got)
	}
}

// **time は実行環境のタイムゾーンに関わらず UTC で出ること。**
//
// ADR 0010 の 4-2 は time を「RFC3339 (UTC)」と定めているが、
// slog は Record.Time をそのロケーションのまま書き出す。
// TZ=Asia/Tokyo が入ると +09:00 付きになり、
// Athena の範囲指定とパーティション整合が 9 時間ずれる。
func TestNewHandler_TimeIsUTC(t *testing.T) {
	original := time.Local
	t.Cleanup(func() { time.Local = original })

	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("タイムゾーンを読み込めない環境: %v", err)
	}
	time.Local = loc

	var buf bytes.Buffer
	slog.New(NewHandler(&buf, false)).InfoContext(context.Background(), "http_request")

	raw, ok := decode(t, &buf)["time"].(string)
	if !ok {
		t.Fatalf("time が文字列で出ていない: %s", buf.String())
	}

	got, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("time を RFC3339 として読めない (%q): %v", raw, err)
	}
	if _, offset := got.Zone(); offset != 0 {
		t.Errorf("time = %q, want UTC (オフセット %d 秒が付いている)", raw, offset)
	}
}
