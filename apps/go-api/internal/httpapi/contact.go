package httpapi

import (
	"net/http"
	"net/netip"

	"github.com/gin-gonic/gin"

	contactusecase "develop-experiments/apps/go-api/internal/contact/usecase"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
)

// CreateContact は POST /contact を処理します。
//
// **202 を返します** (docs/adr/0008-contact-and-mail.md 決定 1)。
// この時点で完了しているのは受理までで、メールはまだ送っていません。
// 200 は「送信完了」を含意するため使いません。
//
// **ログインは不要です** (決定 4)。「ログインできない」という
// 問い合わせが来る以上、必須にすると詰みます。
// ログイン済みなら投稿者を記録しますが、動作は変わりません。
func (s *Server) CreateContact(c *gin.Context) {
	ctx := c.Request.Context()

	var req oapigen.CreateContactJSONRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "リクエストボディが不正です: "+err.Error())
		return
	}

	receivedAt, err := s.contact.Submit(ctx, contactusecase.SubmitCommand{
		// 未ログインなら nil。それが匿名の問い合わせを意味します。
		UserID:  authorIDFromContext(ctx),
		Name:    req.Name,
		Email:   string(req.Email),
		Subject: req.Subject,
		Body:    req.Body,
		// **honeypot は省略可能です。** 埋まっているときだけ破棄するので、
		// 未指定 (nil) は空文字と同じ扱いになります。
		Honeypot: deref(req.Website),
		ClientIP: clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusAccepted, oapigen.ContactAccepted{
		Status:     oapigen.Accepted,
		ReceivedAt: receivedAt,
	})
}

// clientIP はレート制限に使う送信元を取り出します。
//
// **解釈できなければゼロ値を返します。** そのときレート制限は
// 効きませんが、問い合わせ自体は受け付けます ——
// 送信元が取れないという実装側の都合で、利用者の問い合わせを
// 落とすほうが害が大きいためです (ユースケース側が警告を残します)。
//
// gin の ClientIP は信頼できるプロキシの設定に従って X-Forwarded-For を
// 見ます。**偽装されうる値です。** 偽装されればレート制限を回避できますが、
// それは「IP でレート制限する」ことに元々ある限界で、
// 突破されたら WAF 側のルールへ移します (ADR 0008 決定 4)。
func clientIP(c *gin.Context) netip.Addr {
	// **ParseAddr です。** ClientIP はポートを含まない文字列を返すので、
	// ここで SplitHostPort は要りません。
	addr, err := netip.ParseAddr(c.ClientIP())
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

// deref は省略可能な文字列を取り出します。nil なら空文字です。
func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
