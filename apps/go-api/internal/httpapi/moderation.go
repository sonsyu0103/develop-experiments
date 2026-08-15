package httpapi

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	moderationmodel "develop-experiments/apps/go-api/internal/moderation/domain/model"
	moderationusecase "develop-experiments/apps/go-api/internal/moderation/usecase"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
)

// CreateModerationAction は POST /moderation/actions を処理します。
//
// **ログインが必須です** (仕様書の security 宣言が強制します)。
// ロールの検査はユースケース側で行います —— ここで済ませてしまうと、
// 権限の判定がハンドラの数だけ散り、新しい経路を足した人が
// 書き忘れたときに黙って通ります。
func (s *Server) CreateModerationAction(c *gin.Context) {
	ctx := c.Request.Context()

	// **PrincipalDTO ごと要ります。** 投稿者の紐付けと違い、
	// 内部 ID だけでなくロールも見る必要があるためです。
	principal := principalFromContext(ctx)
	if principal == nil {
		respondError(c, fmt.Errorf("ログインが必要です: %w", apperr.ErrUnauthenticated))
		return
	}

	var req oapigen.CreateModerationActionJSONRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "リクエストボディが不正です: "+err.Error())
		return
	}

	// 仕様書の enum が既に弾いていますが、**ドメインの型へは必ず解釈を通します。**
	// 生成された文字列型をそのまま渡すと、仕様書と DB の CHECK 制約が
	// ずれたときに気づけません。
	action, err := moderationmodel.ParseActionType(string(req.Action))
	if err != nil {
		respondError(c, err)
		return
	}

	recorded, err := s.moderation.Delete(ctx, moderationusecase.DeleteCommand{
		// 判定は user モジュールが持ちます (toModerationActor を参照)。
		Actor:    toModerationActor(principal.Role, principal.UserID),
		Action:   action,
		TargetID: req.TargetId,
		ThreadID: req.ThreadId,
		Reason:   req.Reason,
	})
	if err != nil {
		respondError(c, err)
		return
	}

	c.JSON(http.StatusCreated, toWireModerationAction(*recorded))
}

// toWireModerationAction はドメインの記録を仕様書の表現に詰め替えます。
//
// **ActorID は返しません。** 呼び出した本人なので伝える情報が無く、
// 内部 ID (users.id) を外に出さない方針にも反します
// (docs/adr/0003-open-questions.md 未決 #11)。
func toWireModerationAction(a moderationmodel.Action) oapigen.ModerationAction {
	return oapigen.ModerationAction{
		Id:         a.ID,
		Action:     oapigen.ModerationActionType(a.Type),
		TargetType: oapigen.ModerationTargetType(a.Target),
		TargetId:   a.TargetID,
		Reason:     a.Reason,
		CreatedAt:  a.CreatedAt,
	}
}

// ChangeUserRole は PATCH /users/{publicId}/role を処理します。
//
// **admin だけが呼べます** (ADR 0011 決定 1)。
// モデレーターでは足りません —— 投稿を消せる権限と、
// 権限を配れる権限を分離するのがこの決定の趣旨です。
//
// 変更は moderation_actions に change_role として記録されます。
func (s *Server) ChangeUserRole(c *gin.Context, publicID openapi_types.UUID) {
	ctx := c.Request.Context()
	principal := principalFromContext(ctx)
	if principal == nil {
		respondError(c, fmt.Errorf("ログインが必要です: %w", apperr.ErrUnauthenticated))
		return
	}

	var req oapigen.ChangeUserRoleJSONRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "リクエストボディが不正です: "+err.Error())
		return
	}

	// **user モジュールに解釈させます。** 「ロールとは何か」は
	// あちらの関心事で、moderation は値を運ぶだけです。
	role, err := usermodel.ParseRole(string(req.Role))
	if err != nil {
		respondError(c, err)
		return
	}

	recorded, err := s.moderation.ChangeRole(ctx, moderationusecase.ChangeRoleCommand{
		Actor: toModerationActor(principal.Role, principal.UserID),
		// **自分自身かの判定は公開 ID で行います。**
		// 内部 ID は対象の指定に使えない (API が受け取らない) ため、
		// 突き合わせる側も公開 ID に揃えます。
		ActorPublicID:  principal.Me.PublicID,
		TargetPublicID: publicID,
		Role:           string(role),
		Reason:         req.Reason,
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toWireModerationAction(*recorded))
}
