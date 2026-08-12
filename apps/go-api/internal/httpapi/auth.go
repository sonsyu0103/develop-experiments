package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/gin-gonic/gin"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/logging"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
)

// Cookie の名前。
const (
	// sessionCookieName は仕様書の securitySchemes の name と一致させます。
	// ずれると、仕様書上は認証必須なのに Cookie が読まれない状態になります。
	sessionCookieName = "session"

	// ログイン開始時に発行し、コールバックで照合する短命な Cookie。
	// サーバ側に保存しないのは、保存するとログイン開始のたびに行が増えるためです。
	stateCookieName    = "oauth_state"
	nonceCookieName    = "oauth_nonce"
	verifierCookieName = "oauth_verifier"

	// authFlowCookieMaxAge はログインフローの Cookie の寿命です。
	// 認可画面での操作時間を見込みつつ、放置されたものは短く捨てます。
	authFlowCookieMaxAge = 10 * 60
)

// principalKey はリクエストコンテキストに認証済み利用者を載せるための鍵です。
// 独自型にして、他のパッケージの値と衝突しないようにします。
type principalKey struct{}

// principalFromContext は認証済み利用者を取り出します。
// 未ログインの場合は nil を返します。
func principalFromContext(ctx context.Context) *userusecase.PrincipalDTO {
	p, _ := ctx.Value(principalKey{}).(*userusecase.PrincipalDTO)
	return p
}

// authorIDFromContext は投稿に紐付ける投稿者の内部 ID を返します。
//
// **未ログインでは nil**。それが匿名投稿を意味します
// (docs/adr/0005-authentication.md 決定 2)。
// 投稿系のエンドポイントは仕様書に security を宣言していないため、
// ここが nil でも 401 にはなりません。
func authorIDFromContext(ctx context.Context) *int64 {
	p := principalFromContext(ctx)
	if p == nil {
		return nil
	}
	// コンテキストに載せた値を共有しないよう、複製してから返す。
	id := p.UserID
	return &id
}

// resolveSession は Cookie からセッションを解決し、コンテキストに載せます。
//
// **未ログインでも通します。** 匿名投稿を残す設計 (ADR 0005 決定 2) のため、
// ここで弾くと投稿系まで認証必須になってしまいます。
// 「認証が必須かどうか」は仕様書の security 宣言が決め、
// 検証ミドルウェアが強制します。
//
// **OIDC の設定の有無で分岐しません** (ADR 0005 決定 4)。
// セッションの検証は sessions テーブルを引いて期限を見るだけで、
// Google を必要としないためです。ここで
// 「資格情報が無ければ何もしない」と分岐していた頃は、
// CI が Cookie を無視して全リクエストを匿名として扱っており、
// 認証を要する経路が 1 件も検証されていませんでした。
//
// 仕様検証ミドルウェアより前に置く必要があります。
// AuthenticationFunc がここで載せた値を見るためです。
func (s *Server) resolveSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := c.Cookie(sessionCookieName)
		if err != nil || raw == "" {
			c.Next()
			return
		}

		principal, err := s.sessions.Authenticate(c.Request.Context(), usermodel.SessionToken(raw))
		if err != nil {
			// 無効なセッションは「未ログイン」として扱い、ここでは弾かない。
			// security を宣言したエンドポイントだけが 401 になる。
			if errors.Is(err, apperr.ErrUnauthenticated) {
				c.Next()
				return
			}

			// **それ以外 (DB 障害など) を素通ししない。**
			// 素通しすると 401 に化け、利用者からは全員が突然ログアウトされた
			// ように見えるうえ、原因の手がかりがどこにも残らない。
			slog.ErrorContext(c.Request.Context(), "セッションの解決に失敗しました",
				slog.String("path", c.Request.URL.Path),
				slog.String("error", err.Error()),
			)
			c.Abort()
			respondError(c, err)
			return
		}

		ctx := context.WithValue(c.Request.Context(), principalKey{}, principal)
		// 以降のログに user_id が自動で乗る (ADR 0010 の 4-2)。
		// 載せるのは内部 ID だけ。メールアドレスと google_sub は出さない (4-5)。
		ctx = logging.WithUserID(ctx, principal.UserID)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// newAuthenticationFunc は仕様書の security 要件を検証する関数を返します。
//
// kin-openapi は security を宣言したオペレーションでこれが未設定だと
// ErrAuthenticationServiceMissing を返してリクエストを弾きます
// (validate_request.go: "users MUST set AuthenticationFunc explicitly")。
// つまり**仕様書に security を書いた時点で、この結線は必須**になります。
//
// ここでは DB を引きません。resolveSession が済ませた結果を見るだけです。
// ここで引くと、認証つきリクエストのたびにセッションを 2 回引くことになります。
//
// 【罠】渡ってくる ctx はリクエストのコンテキストではありません。
// gin-middleware は context.Background() から新しい文脈を作り、
// そこに gin.Context だけを載せて渡してきます (oapi_validate.go:140)。
// resolveSession が c.Request のコンテキストに入れた値は**そのままでは届きません**。
// gin.Context を取り出してから読む必要があります。
func newAuthenticationFunc() openapi3filter.AuthenticationFunc {
	return func(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
		c := ginmiddleware.GetGinContext(ctx)
		if c == nil {
			return input.NewError(fmt.Errorf("リクエストの文脈を取得できません"))
		}
		if principalFromContext(c.Request.Context()) == nil {
			return input.NewError(fmt.Errorf("セッションがありません"))
		}
		return nil
	}
}

// setSessionCookie はセッション Cookie を発行します。
//
// 属性の理由 (docs/adr/0005-authentication.md):
//   - HttpOnly: JavaScript から読めなくする (XSS でのセッション奪取を防ぐ)
//   - Secure:   本番のみ。localhost は HTTP なので開発時は付けられない
//   - SameSite=Lax: フロントと API が same-site に収まるため None は不要。
//     same-site の判定にポートは含まれないので localhost:3000 → :8080 も同一
func (s *Server) setSessionCookie(c *gin.Context, token usermodel.SessionToken, maxAge int) {
	// gosec G124 は Secure が定数 true であることを求めるが、
	// localhost は HTTP なので開発時は付けられない (ADR 0005 の Cookie 属性)。
	// 既定は本番扱い (SecureCookie=true) で、ENV=development のときだけ外れる。
	//nolint:gosec // Secure は設定で切り替える。既定は付ける側
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    string(token),
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.authCfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

// setFlowCookie はログインフロー用の短命な Cookie を発行します。
func (s *Server) setFlowCookie(c *gin.Context, name, value string) {
	//nolint:gosec // 同上 (setSessionCookie のコメントを参照)
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   authFlowCookieMaxAge,
		HttpOnly: true,
		Secure:   s.authCfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearFlowCookies はログインフロー用の Cookie をまとめて破棄します。
//
// **レスポンスを書き出す前に呼ぶ必要があります。** ヘッダを送出したあとに
// Set-Cookie を足しても、レスポンスには載りません。
func (s *Server) clearFlowCookies(c *gin.Context) {
	s.clearCookie(c, stateCookieName)
	s.clearCookie(c, nonceCookieName)
	s.clearCookie(c, verifierCookieName)
}

// frontendURLWithError はログイン失敗をフロントへ伝えるリダイレクト先を組み立てます。
//
// IdP が返した文字列をそのまま載せません。フロント側でそのまま描画されると
// 反射型 XSS の入口になるため、既知の書式に収まるものだけ通します。
func (s *Server) frontendURLWithError(reason string) string {
	if !loginErrorPattern.MatchString(reason) {
		reason = "login_failed"
	}

	u, err := url.Parse(s.authCfg.FrontendURL)
	if err != nil {
		return s.authCfg.FrontendURL
	}
	q := u.Query()
	q.Set("login_error", reason)
	u.RawQuery = q.Encode()
	return u.String()
}

// loginErrorPattern は OAuth 2.0 の error に許す書式です
// (RFC 6749 4.1.2.1 は access_denied のような小文字の識別子を定めています)。
var loginErrorPattern = regexp.MustCompile(`^[a-z_]{1,64}$`)

// clearCookie は Cookie を破棄します。MaxAge を負にすると即時削除になります。
func (s *Server) clearCookie(c *gin.Context, name string) {
	//nolint:gosec // 同上 (setSessionCookie のコメントを参照)
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.authCfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

// requireLoginEnabled は**ログインに必要な**設定が入っているかを確認します。
//
// 設定が無くても API は起動します。掲示板の閲覧と匿名投稿は認証に依存せず、
// 全体を落とすほうが害が大きいためです。
//
// **503 になるのはログインの 2 経路だけ**です (ADR 0005 決定 4)。
// セッションの検証・/me・ログアウトは発行済みのセッションを見るだけなので、
// 設定の有無にかかわらず動きます。ここを「認証まわり全部」に広げると、
// 資格情報の無い環境 (CI) で認証済みの経路が検証できなくなります。
func (s *Server) requireLoginEnabled() error {
	if s.login == nil {
		return fmt.Errorf("認証プロバイダが設定されていません: %w", apperr.ErrUnavailable)
	}
	return nil
}
