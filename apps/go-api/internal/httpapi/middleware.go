package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
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

// idempotencyKeyHeader は二重送信を防ぐキーを運ぶヘッダです (ADR 0015)。
//
// **仕様書 (api/openapi.yaml の components/parameters/IdempotencyKey) と
// 同じ値であること。** cors() の Allow-Headers に挙げるために要ります ——
// 挙げないとブラウザが preflight の段階で送信を諦めます。
const idempotencyKeyHeader = "Idempotency-Key"

// corsAllowedMethods は preflight で許すメソッドです。
//
// **仕様書が使うメソッドをすべて含む必要があります。**
// 足りないメソッドはブラウザが preflight の段階で諦めるため、
// **フロントから一度も呼べないエンドポイント**になります ——
// サーバ側には要求が届かないので、ログにも痕跡が残りません。
//
// これは 2 度踏んでいます。
//
//   - `PUT` の欠落で `PUT /me/avatar` が呼べなかった
//     (ADR 0013 の「実装して分かったこと 6」)
//   - `PATCH` / `DELETE` の欠落で、通報の処理・ロール変更・本人削除が
//     まとめて呼べなかった。**エンドポイントを足した PR は
//     ここを見ていない** —— 管理画面を書き始めて初めて分かった
//
// 2 度とも「仕様書に足りているのに、ここに無い」形です。
// 揃っていることは `TestCORSAllowedMethods_AgreesWithSpec` が
// 仕様書そのものから検査します (目視で揃え続けるのは無理があるため)。
//
// `OPTIONS` は preflight 自身のメソッドで、仕様書には現れません。
const corsAllowedMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"

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
			h.Set("Access-Control-Allow-Methods", corsAllowedMethods)
			// X-Request-Id は送る側にも許す。ALB やフロントが既に採番している
			// 場合に前後をつなぐため (requestID のコメントを参照)。
			// ここに無いと preflight で弾かれ、リクエスト自体が失敗する。
			//
			// **Idempotency-Key も同じ。** 仕様書が受け付けると書いている
			// ヘッダ (api/openapi.yaml の components/parameters) は、
			// ここに挙げないとブラウザから送れない (ADR 0015)。
			h.Set("Access-Control-Allow-Headers",
				"Content-Type, "+requestIDHeader+", "+idempotencyKeyHeader)
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

// csrfGuard は状態変更メソッドの Origin / Referer を検証します
// (docs/adr/0013-http-defense.md 決定 1)。
//
// **CORS では CSRF を防げません。** CORS はブラウザにレスポンスを
// 読ませない仕組みであり、リクエスト自体は飛びます。しかも
// multipart/form-data はプリフライトを起こさない (simple request) ので、
// 画像アップロード (ADR 0007) の経路では CORS が一切効きません。
//
// **SameSite=Lax だけにも頼りません。** サブドメインは same-site 扱いなので、
// api.example.com と並ぶ別のサブドメインを取られると通ります。
//
// 検証の順序:
//
//  1. Origin がある      -> 許可リストと照合。一致しなければ 403
//  2. Origin が無い      -> Referer のオリジン部分で照合
//  3. どちらも無い       -> 403
//
// **3 番目が効くので、ブラウザ以外のクライアントも Origin を送る必要があります。**
// スモークテストと curl は明示的に付けています。ここを「無ければ通す」に
// すると、Origin を送らないだけで検証を迂回できるため、防御になりません。
//
// 許可リストは cors() と同じ設定値 (CORS_ALLOWED_ORIGINS) を共有しますが、
// 処理は独立させています —— CORS はヘッダを付ける処理、こちらは弾く処理です。
func csrfGuard(allowedOrigins []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isStateChanging(c.Request.Method) {
			c.Next()
			return
		}

		// **GET / HEAD に副作用を持たせないことが前提です** (ADR 0013 決定 1)。
		// この不変条件が崩れると SameSite=Lax の前提も同時に崩れ、
		// トークン方式の再検討が要ります。
		if origin := c.GetHeader("Origin"); origin != "" {
			if !isAllowedOrigin(c, allowedOrigins, origin) {
				rejectCrossOrigin(c, "origin", origin)
				return
			}
			c.Next()
			return
		}

		if referer := c.GetHeader("Referer"); referer != "" {
			origin, ok := originOf(referer)
			if !ok || !isAllowedOrigin(c, allowedOrigins, origin) {
				rejectCrossOrigin(c, "referer", referer)
				return
			}
			c.Next()
			return
		}

		rejectCrossOrigin(c, "missing", "")
	}
}

// isAllowedOrigin は許可リスト、または**自分自身のオリジン**かを返します。
//
// **同一オリジン構成を落とさないために自分自身を足しています** (レビュー指摘)。
// フロントと API を 1 つのホストに置く構成 (https://example.com と
// https://example.com/api) では CORS が本当に不要なので、運用者が
// CORS_ALLOWED_ORIGINS を設定する理由がありません。既定値は開発用の
// http://localhost:3000 なので、**そのまま本番へ出すと書き込みが全滅します。**
// この PR より前は「レスポンスヘッダが付かないだけ」で済んでいた設定漏れが、
// 初回デプロイでの全面停止に変わってしまいます。
//
// **Host を信用してよいのか** —— この検査が守る相手はブラウザだけです。
// ブラウザが送る Host は「利用者が開いた URL」であって攻撃者は変えられません
// (evil.test のページから example.com へ投げても Host は example.com、
// Origin は evil.test になる)。ブラウザ以外は Origin を自由に詐称できるので、
// そもそもこの検査の射程外です。
//
// scheme は X-Forwarded-Proto (ALB がある構成) を見て、無ければ
// TLS の有無で判断します。**逆プロキシの内側でしか正しくありません** ——
// インターネットに直接晒す構成では、手前でこのヘッダを剥がしてください
// (requestID が受信ヘッダを尊重するのと同じ前提になります)。
func isAllowedOrigin(c *gin.Context, allowedOrigins []string, origin string) bool {
	if slices.Contains(allowedOrigins, origin) {
		return true
	}
	return origin == selfOrigin(c)
}

// selfOrigin はこの要求が向けられたオリジンを組み立てます。
func selfOrigin(c *gin.Context) string {
	host := c.Request.Host
	if host == "" {
		return ""
	}

	scheme := "http"
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		// 複数段のプロキシではカンマ区切りで積まれる。手前のものを使う。
		scheme, _, _ = strings.Cut(proto, ",")
		scheme = strings.TrimSpace(scheme)
	} else if c.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host
}

// isStateChanging は副作用を持ちうるメソッドかを返します。
func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// originOf は Referer からオリジン部分 (scheme://host[:port]) を取り出します。
//
// **文字列の前方一致で判定しません。** "https://example.com.evil.test/" は
// "https://example.com" で始まるので、前方一致だと通ってしまいます。
func originOf(referer string) (string, bool) {
	u, err := url.Parse(referer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// maxLoggedHeaderBytes はログに残すヘッダ値の上限です。
//
// **利用者の入力を無制限にログへ流さないこと** (requestIDPattern と同じ理由)。
// ログは S3 に長期保管され Athena から検索されます。Referer は完全に
// 相手が決める値で、長さは Go の MaxHeaderBytes (既定 1 MiB) までしか
// 縛られていません。未認証の POST 1 本ごとに WARN が 1 行出るので、
// 大きな Referer を撒かれると保管コストがそのまま膨らみます (レビュー指摘)。
const maxLoggedHeaderBytes = 256

// truncateForLog はログに載せる値を丸めます。
func truncateForLog(v string) string {
	if len(v) <= maxLoggedHeaderBytes {
		return v
	}
	// 丸めたことが分かる形にする。切れているのか元から短いのかを
	// 区別できないと、調査で「値が変」と「ログが変」を取り違える。
	return v[:maxLoggedHeaderBytes] + "...(truncated)"
}

// rejectCrossOrigin は 403 で打ち切り、判断の材料をログに残します。
//
// **理由を本文に書き分けません。** どの条件で落ちたかを返すと、
// 許可リストの内容を外から探れます。ログには残します。
func rejectCrossOrigin(c *gin.Context, reason, value string) {
	slog.WarnContext(c.Request.Context(), "csrf_rejected",
		slog.String("reason", reason),
		slog.String("value", truncateForLog(value)),
		slog.String("method", c.Request.Method),
		slog.String("path", c.Request.URL.Path),
	)
	c.AbortWithStatusJSON(http.StatusForbidden,
		newErrorBody(oapigen.PERMISSIONDENIED, "この要求は受け付けられません"))
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
			// **Any にしない。** recover した値は任意の型で、
			// JSON に出る形が呼び出しごとに変わる。Athena は
			// 1 つの型しか宣言できないので、ここで文字列に固定する。
			slog.String("panic", fmt.Sprint(recovered)),
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
//	csrfGuard     cors の直後。OPTIONS は既に抜けているので素通しを考えなくてよい
func middlewares(allowedOrigins []string) []gin.HandlerFunc {
	return []gin.HandlerFunc{
		requestID(),
		requestLogger(),
		recovery(),
		cors(allowedOrigins),
		// **bodyLimit より前に置く。** 弾くと決まっている要求の本文を
		// 読み始める理由がありません (ADR 0013 決定 1)。
		csrfGuard(allowedOrigins),
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
