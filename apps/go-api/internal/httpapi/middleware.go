package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	imageusecase "develop-experiments/apps/go-api/internal/image/usecase"
	"develop-experiments/apps/go-api/internal/logging"
)

// requestIDHeader は相関 ID を運ぶヘッダです。
// 受信したものを尊重し、無ければ生成します (docs/adr/0010-log-pipeline.md 4-1)。
const requestIDHeader = "X-Request-Id"

// requestIDPattern は受信したリクエスト ID に許す書式です。
//
// **受け取った値をそのままログへ流さないこと。** ログは S3 に長期保管され、
// Athena から検索されます。任意の文字列を通すと、改行や制御文字を混ぜて
// 偽のログ行を作られる (ログインジェクション) 余地ができ、
// 長大な値を送られれば保管コストにも効きます。
//
// 書式は UUID や ULID、トレース ID が収まる範囲にしてあります。
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// requestID は相関 ID を用意し、コンテキストとレスポンスヘッダに載せます。
//
// **すべてのミドルウェアより先に置きます。** 後ろに置くと、
// それより前で出たログに request_id が付かず、相関から外れます。
//
// 受信ヘッダを尊重するのは、ALB やフロントが既に採番している場合に
// 前後をつなぐためです。**信頼できる境界の内側でのみ意味を持ちます** ——
// インターネットに直接晒す構成では、手前で剥がすか上書きしてください。
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if !requestIDPattern.MatchString(id) {
			id = uuid.NewString()
		}

		// 利用者からの問い合わせで引けるように、応答にも返す。
		c.Header(requestIDHeader, id)
		c.Request = c.Request.WithContext(
			logging.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

// cors は許可オリジンからのクロスオリジン要求を通すミドルウェアです。
//
// Next.js の Server Components はサーバ側から fetch するため CORS は不要ですが、
// コメント投稿のようにブラウザから直接叩く経路では必要になります。
//
// ワイルドカード (*) を使わず許可リスト方式にしているのは、
// Cookie 認証で必要な Access-Control-Allow-Credentials と
// 併用できるようにするためです (* との併用はブラウザが拒否します)。
func cors(allowedOrigins []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()

		// Vary は「許可したときだけ」ではなく、このミドルウェアが触る
		// 全レスポンスに付ける必要がある。
		// 許可ヘッダの無い応答に Vary が無いと、共有キャッシュ / CDN が
		// それをオリジン非依存として保存し、許可オリジンからの
		// リクエストにも使い回してしまう。
		// ブラウザ側では CORS ヘッダ欠落として弾かれ、断続的に失敗する。
		h.Add("Vary", "Origin")

		origin := c.GetHeader("Origin")
		if origin != "" && slices.Contains(allowedOrigins, origin) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			// X-Request-Id は送る側にも許す。ALB やフロントが既に採番している
			// 場合に前後をつなぐため (requestID のコメントを参照)。
			// ここに無いと preflight で弾かれ、リクエスト自体が失敗する。
			h.Set("Access-Control-Allow-Headers", "Content-Type, "+requestIDHeader)
			// **返すだけでは JavaScript から読めない。**
			// 既定で読めるのは限られたヘッダだけで、独自ヘッダは
			// Expose-Headers に挙げないと fetch の res.headers から消える。
			// 「問い合わせが来たときに引く」ためには、まず利用者側が
			// 値を知れる必要がある。
			h.Set("Access-Control-Expose-Headers", requestIDHeader)
			h.Set("Access-Control-Max-Age", "600")
			// セッションは Cookie で運ぶ (ADR 0005 決定 1)。
			// これが無いと、ブラウザは credentials 付きの要求に対する
			// 応答を JavaScript へ渡さない。Cookie は送られるので
			// サーバ側のログは正常に見え、フロントだけが失敗する。
			h.Set("Access-Control-Allow-Credentials", "true")
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// recovery はパニックを拾い、構造化ログに残してから 500 を返します。
//
// gin.Recovery は stderr へ平文で書くだけで、service / version / request_id が
// 付きません。**ADR 0010 の 4-3 はパニックを ERROR に分類し、
// CloudWatch のアラームはこのレベルを起点に組む**と決めているため、
// そのままだと最もアラートが要る事象で ERROR が立ちません。
//
// 本文も返します。gin.Recovery は 500 を空ボディで返すので、
// 「エラーは必ず JSON」という API の約束から外れます。
//
// **requestLogger より内側に置く必要があります。** 外側に置くと、
// パニックが requestLogger の c.Next() を巻き戻して抜けてしまい、
// http_request の行が 1 本も出ません (実測で確認済み)。
func recovery() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		// スタックは復帰処理の中で取る。パニックしたフレームがまだ
		// 積まれているため、発生箇所まで辿れる。
		slog.ErrorContext(c.Request.Context(), "panic",
			slog.Any("panic", recovered),
			slog.String("stack", string(debug.Stack())),
		)
		c.AbortWithStatusJSON(http.StatusInternalServerError,
			newErrorBody(oapigen.INTERNAL, "サーバ内部でエラーが発生しました"))
	})
}

// middlewares はルータに積むミドルウェアを順番どおりに返します。
//
// **並びに意味があるので 1 か所にまとめます。** テストが同じ並びを
// 書き写す形にすると、順序を変えたときに検査だけが古くなります。
//
//	requestID     すべてのログに相関 ID を載せるため最初
//	requestLogger recovery より外。内側だとパニック時に行が出ない
//	recovery      パニックを ERROR として残す
//	cors          プリフライトをここで打ち切る
func middlewares(allowedOrigins []string) []gin.HandlerFunc {
	return []gin.HandlerFunc{
		requestID(),
		requestLogger(),
		recovery(),
		cors(allowedOrigins),
		// **仕様検証より前に置く。** 検証ミドルウェアは本文を
		// 丸ごと読むため、ここより後ろでは手遅れになる (bodyLimit を参照)。
		bodyLimit(),
	}
}

// requestLogger は 1 リクエストにつき 1 行の構造化ログを出します。
// gin 標準の Logger は平文なので、slog による JSON 出力に置き換えています。
//
// request_id と user_id はここで書きません。
// logging.ContextHandler がコンテキストから自動で付けます
// (各所で書く形にすると、必ずどこかで渡し忘れます)。
//
// **URL のクエリ文字列は出しません** (ADR 0010 の 4-5)。
// 丸ごと出すと、将来パラメータを足したときに自動的に漏れます。
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		// ERROR は「人が対応する必要がある」の意味を持ちます (ADR 0010 の 4-3)。
		// アラートの閾値と直結するため、4xx は INFO のままにします。
		level := slog.LevelInfo
		if c.Writer.Status() >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		slog.Log(c.Request.Context(), level, "http_request",
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()),
			// ミリ秒の数値で出す。slog.Duration は JSON ではナノ秒の整数になり、
			// Athena で毎回 / 1e6 を書くことになります (ADR 0010 の 4-4)。
			// 単位をフィールド名に含めて、桁の取り違えを防ぎます。
			slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000),
		)
	}
}

// 本文の大きさの上限。
//
// **仕様検証ミドルウェアより前に置く必要があります。**
// kin-openapi の openapi3filter は、ハンドラに入る前に本文を
// io.ReadAll で丸ごと読みます。しかも security の検証
// (validate_request.go の validateSecurityRequirement) が
// **AuthenticationFunc を呼ぶ前に**読むため、
// 未ログインの要求でも本文はすべてメモリに載ります。
//
// 初版はハンドラの中で http.MaxBytesReader を張っており、
// **到達した時点で既に読み終わっていたので何の効果もありませんでした。**
// 実測: 30 MiB の本文が、401 を返す経路でも全量読まれていた。
const (
	// maxJSONBodyBytes は JSON を受け取る経路の上限です。
	//
	// 本文は最大 2000 文字 + 投稿者名 50 文字なので、
	// UTF-8 で最悪 3 倍としても 64 KiB あれば足ります。
	// 余裕を持たせて 256 KiB。
	maxJSONBodyBytes int64 = 256 << 10

	// maxImageUploadBytes は POST /images の上限です。
	// 画像本体 5 MiB に、マルチパートの境界・ヘッダ・kind の余地を足した値。
	maxImageUploadBytes int64 = imageusecase.MaxUploadBytes + (1 << 16)
)

// bodyLimit は本文の大きさを経路ごとに制限します。
//
// 2 段構えにしています。
//
//  1. Content-Length が上限を超えていれば、**読む前に** 413 で返す
//  2. Content-Length が無い (chunked) 場合に備えて MaxBytesReader を張る
//
// 1 だけだと chunked で回避され、2 だけだと上限まで読んでから気づきます。
func bodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := maxJSONBodyBytes
		// **経路で分ける。** 画像だけを大きくし、他は絞ったままにする。
		// パスの雛形ではなく実際のパスで見る (この経路は 1 つしかない)。
		if c.Request.Method == http.MethodPost && c.Request.URL.Path == "/images" {
			limit = maxImageUploadBytes
		}

		// GET などに本文が付いていても、ここで縛って困ることはない。
		if c.Request.ContentLength > limit {
			c.Abort()
			respondError(c, fmt.Errorf(
				"リクエストが大きすぎます (%d バイト。上限は %d バイト): %w",
				c.Request.ContentLength, limit, apperr.ErrPayloadTooLarge))
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}
