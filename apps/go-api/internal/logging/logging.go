// Package logging は構造化ログの共通フィールドと、リクエスト相関を担います。
//
// 設計は docs/adr/0010-log-pipeline.md の決定 4。
//
// **Athena はスキーマオンリードなので、フィールドが揺れるとクエリが書けません。**
// 全ログに必ず含める最小集合をここで固定します。
//
//	time / level / msg / service / version / request_id / user_id
//
// `msg` は**イベント名として扱い、自由文にしません**。
// 自由文にすると、集計のたびに LIKE を書くことになります。
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
)

// ServiceName は全ログに載せるサービス名です。
const ServiceName = "go-api"

// ctxKey はコンテキストに載せる値の鍵です。
// 独自型にして、他のパッケージの値と衝突しないようにします。
type ctxKey int

const (
	requestIDKey ctxKey = iota
	userIDKey
)

// WithRequestID はリクエスト ID をコンテキストに載せます。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext はリクエスト ID を取り出します。
// 載っていない場合は空文字を返します。
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithUserID はログイン中の利用者の内部 ID をコンテキストに載せます。
//
// **内部 ID を使うのは意図的です。** ログに載せてよいのは user_id だけで、
// メールアドレスと google_sub は出しません (ADR 0010 の 4-5)。
// 一度 S3 に出したものは事実上消せないためです。
func WithUserID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// userIDFromContext は内部 ID を取り出します。
// 未ログインの場合は ok が false になります。
func userIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(userIDKey).(int64)
	return id, ok
}

// ContextHandler は、コンテキストに載った相関情報を
// **すべてのログレコードへ自動的に付ける** slog.Handler です。
//
// 各所で slog.String("request_id", ...) を書く形にすると、
// 必ずどこかで渡し忘れます。渡し忘れたレコードは Athena で
// 相関から外れ、しかも欠けていること自体に気づけません。
//
// 【グループを開いた場合】
// slog.Record に足した属性は、開いているグループの**中**に入ります。
// つまり素直に AddAttrs すると、WithGroup("db") のあとでは
// {"db": {"request_id": "..."}} になり、Athena の request_id = '...' が
// 一致しなくなります。**一部のログにだけ相関が効かない**という、
// 最も気づきにくい壊れ方をします。
//
// そのため、グループが開かれている場合だけ、
// グループを開く前のハンドラから組み直して相関情報を先に載せます。
// グループを使わない経路 (現在のすべて) は AddAttrs の速い方を通ります。
type ContextHandler struct {
	slog.Handler

	// root はグループも属性も適用していない元のハンドラです。
	root slog.Handler
	// steps は root に適用した操作を、順番どおりに再現するためのものです。
	steps []func(slog.Handler) slog.Handler
	// grouped は WithGroup が 1 度でも呼ばれたかどうかです。
	grouped bool
}

// NewContextHandler は相関情報を自動付与するハンドラを作ります。
func NewContextHandler(inner slog.Handler) ContextHandler {
	return ContextHandler{Handler: inner, root: inner}
}

// base は「グループを開く前のハンドラ」を返します。
//
// root が nil なのは、コンストラクタを通さず構造体リテラルで
// 組み立てられた場合です。そのときは今のハンドラを起点にします
// (まだ何も適用されていないため、それが正しい起点になります)。
// nil のまま使うと Handle が落ちます。**零値でも壊れない形にしておきます。**
func (h ContextHandler) base() slog.Handler {
	if h.root != nil {
		return h.root
	}
	return h.Handler
}

// contextAttrs はコンテキストから取り出す相関情報です。
func contextAttrs(ctx context.Context) []slog.Attr {
	attrs := make([]slog.Attr, 0, 2)
	if id := RequestIDFromContext(ctx); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	// 未ログインでは user_id を出しません。
	// 0 を出すと「利用者 0 番」と区別できなくなります。
	if id, ok := userIDFromContext(ctx); ok {
		attrs = append(attrs, slog.Int64("user_id", id))
	}
	return attrs
}

// Handle は共通フィールドを足してから内側のハンドラに渡します。
func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := contextAttrs(ctx)
	if len(attrs) == 0 {
		return h.Handler.Handle(ctx, r)
	}

	if !h.grouped {
		// 速い方。グループが無ければ AddAttrs で最上位に載る。
		r.AddAttrs(attrs...)
		return h.Handler.Handle(ctx, r)
	}

	// グループが開いている。相関情報を最上位に置くため、
	// root に先に載せてから、これまでの操作を順に再現する。
	built := h.base().WithAttrs(attrs)
	for _, step := range h.steps {
		built = step(built)
	}
	return built.Handle(ctx, r)
}

// WithAttrs は内側のハンドラに委譲しつつ、ContextHandler で包み直します。
// 包み直さないと、slog.With(...) を使った時点で相関情報が消えます。
func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{
		Handler: h.Handler.WithAttrs(attrs),
		root:    h.base(),
		steps:   appendStep(h.steps, func(in slog.Handler) slog.Handler { return in.WithAttrs(attrs) }),
		grouped: h.grouped,
	}
}

// WithGroup も同様に包み直します。あわせて grouped を立て、
// Handle が「組み直す側」の経路を通るようにします。
func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{
		Handler: h.Handler.WithGroup(name),
		root:    h.base(),
		steps:   appendStep(h.steps, func(in slog.Handler) slog.Handler { return in.WithGroup(name) }),
		grouped: true,
	}
}

// appendStep は既存のスライスを共有しないように複製してから足します。
// append をそのまま使うと、同じロガーから 2 つ派生させたときに
// 片方の操作がもう片方へ混ざります。
func appendStep(
	steps []func(slog.Handler) slog.Handler, step func(slog.Handler) slog.Handler,
) []func(slog.Handler) slog.Handler {
	out := make([]func(slog.Handler) slog.Handler, len(steps), len(steps)+1)
	copy(out, steps)
	return append(out, step)
}

// Setup は slog の既定ロガーを構成します。
// 本番は JSON、開発は人間が読みやすいテキスト形式にします。
func Setup(debug bool) {
	slog.SetDefault(slog.New(NewHandler(os.Stdout, debug)))
}

// NewHandler は共通フィールドを載せたハンドラを組み立てます。
//
// Setup から出力先を切り離してあるのは、**共通フィールドが実際に
// 載っているかをテストできるようにする**ためです。
// os.Stdout に直接書く形だと、そこを検査する手立てが無くなります。
func NewHandler(w io.Writer, debug bool) slog.Handler {
	level := slog.LevelInfo
	var base slog.Handler

	if debug {
		// DEBUG を本番で出さない理由は ADR 0010 の 4-3。
		// 保管コストと機密情報の露出リスクが同時に上がります。
		level = slog.LevelDebug
		base = slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	} else {
		base = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	}

	return NewContextHandler(base).WithAttrs([]slog.Attr{
		slog.String("service", ServiceName),
		slog.String("version", Version()),
	})
}

// Version はデプロイ間の比較に使う版数です (ADR 0010 の 4-2)。
//
// 決め方は 3 段階で、上から順に採用します。
//
//  1. APP_VERSION 環境変数。コンテナイメージのビルド時に埋める用
//  2. ビルド情報の vcs.revision。go build がリポジトリ内で行われた場合に入る
//  3. "unknown"
//
// **開発コンテナでは 3 になります。** compose は apps/go-api だけを
// マウントしており .git が無いため、go run はリビジョンを埋め込めません。
// ここで嘘の値を作らず unknown のままにするのは、
// 「版数が分からない環境である」ことがログから読めるようにするためです。
func Version() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return shortRevision(s.Value)
			}
		}
	}
	return "unknown"
}

// shortRevision は git の短縮ハッシュに揃えます。
func shortRevision(rev string) string {
	const shortLen = 7
	if len(rev) > shortLen {
		return rev[:shortLen]
	}
	return rev
}
