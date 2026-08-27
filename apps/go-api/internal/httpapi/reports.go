package httpapi

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"develop-experiments/apps/go-api/internal/apperr"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	moderationmodel "develop-experiments/apps/go-api/internal/moderation/domain/model"
	moderationusecase "develop-experiments/apps/go-api/internal/moderation/usecase"
	usermodel "develop-experiments/apps/go-api/internal/user/domain/model"
)

// CreateReport は POST /reports を処理します。
//
// **ログインが必須です** (ADR 0011 決定 4)。匿名で受け付けると、
// 通報そのものが荒らしの手段になります。
//
// **重複通報は 200 で、エラーではありません。** 一意制約は
// 「1 人で通報数を積み上げてキューの優先度を操作させない」ためのもので、
// 利用者に見せる失敗ではありません。
func (s *Server) CreateReport(c *gin.Context) {
	ctx := c.Request.Context()
	reporterID := authorIDFromContext(ctx)
	if reporterID == nil {
		respondError(c, fmt.Errorf("通報にはログインが必要です: %w", apperr.ErrUnauthenticated))
		return
	}

	var req oapigen.CreateReportJSONRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "リクエストボディが不正です: "+err.Error())
		return
	}

	// 仕様書の enum が既に弾いていますが、**ドメインの型へは必ず解釈を通します。**
	// 仕様書と DB の CHECK 制約がずれたときに気づけるようにするためです。
	target, err := moderationmodel.ParseReportTargetType(string(req.TargetType))
	if err != nil {
		respondError(c, err)
		return
	}
	reason, err := moderationmodel.ParseReportReason(string(req.Reason))
	if err != nil {
		respondError(c, err)
		return
	}

	report, created, err := s.reports.Report(ctx, moderationusecase.ReportCommand{
		ReporterID: *reporterID,
		Target:     target,
		TargetID:   req.TargetId,
		ThreadID:   req.ThreadId,
		Reason:     reason,
		Note:       req.Note,
	})
	if err != nil {
		respondError(c, err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, toWireReport(*report))
}

// ListReports は GET /moderation/reports を処理します。
//
// **moderator 以上だけが読めます。** 判定はユースケース側にあります。
func (s *Server) ListReports(c *gin.Context, params oapigen.ListReportsParams) {
	ctx := c.Request.Context()
	principal := principalFromContext(ctx)
	if principal == nil {
		respondError(c, fmt.Errorf("ログインが必要です: %w", apperr.ErrUnauthenticated))
		return
	}

	page, err := toPage(params.Cursor, params.Size)
	if err != nil {
		respondError(c, err)
		return
	}

	// **既定は open。** キューは未処理を見るためのものです。
	status := moderationmodel.ReportOpen
	if params.Status != nil {
		status, err = moderationmodel.ParseReportStatus(string(*params.Status))
		if err != nil {
			respondError(c, err)
			return
		}
	}

	result, err := s.reports.ListQueue(ctx, toModerationActor(principal.Role, principal.UserID), status, page)
	if err != nil {
		respondError(c, err)
		return
	}

	reports := make([]oapigen.Report, 0, len(result.Reports))
	for _, r := range result.Reports {
		reports = append(reports, toWireReport(r))
	}
	c.JSON(http.StatusOK, oapigen.ReportList{
		Reports:    reports,
		NextCursor: result.NextCursor,
	})
}

// ResolveReport は PATCH /moderation/reports/{reportId} を処理します。
//
// **投稿には触れません。** 削除は POST /moderation/actions が別に行います。
func (s *Server) ResolveReport(c *gin.Context, reportID int64) {
	ctx := c.Request.Context()
	principal := principalFromContext(ctx)
	if principal == nil {
		respondError(c, fmt.Errorf("ログインが必要です: %w", apperr.ErrUnauthenticated))
		return
	}

	var req oapigen.ResolveReportJSONRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "リクエストボディが不正です: "+err.Error())
		return
	}

	status, err := moderationmodel.ParseReportStatus(string(req.Status))
	if err != nil {
		respondError(c, err)
		return
	}

	resolved, err := s.reports.Resolve(
		ctx, toModerationActor(principal.Role, principal.UserID), reportID, status)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toWireReport(*resolved))
}

// toWireReport はドメインの通報を仕様書の表現に詰め替えます。
//
// **ReporterID と ResolvedBy は返しません。** 誰が通報したかを
// 見せる必要が無く、見せると報復の材料になります。
// 内部 ID を外に出さない方針とも揃います (ADR 0003 未決 #11)。
func toWireReport(r moderationmodel.Report) oapigen.Report {
	return oapigen.Report{
		Id:         r.ID,
		TargetType: oapigen.ReportTargetType(r.Target),
		TargetId:   r.TargetID,
		// **スレッドの通報では省略されます** (null ではありません)。
		// required に入れていないので、生成される型は省略可能になります。
		ThreadId:   r.TargetThreadID,
		Reason:     oapigen.ReportReason(r.Reason),
		Note:       r.Note,
		Status:     oapigen.ReportStatus(r.Status),
		CreatedAt:  r.CreatedAt,
		ResolvedAt: r.ResolvedAt,
	}
}

// toModerationActor は認証済み利用者をモデレーションの実行者に詰め替えます。
//
// **判定は user モジュールが持ちます。** ここで
// `Role == "moderator" || Role == "admin"` と書くと、
// ロールが増えたときに直す場所が 2 か所になります。
func toModerationActor(role usermodel.Role, userID int64) moderationmodel.Actor {
	return moderationmodel.Actor{
		UserID:         userID,
		CanModerate:    role.CanModerate(),
		CanChangeRoles: role.CanChangeRoles(),
	}
}
