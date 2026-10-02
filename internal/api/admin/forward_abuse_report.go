package admin

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/repository"
)

// ForwardAbuseUserReport handles POST /api/admin/forward-abuse-user-report.
//
// 対象ユーザーがリモートの場合、system actor 署名で origin インスタンス
// の inbox へ ActivityPub Flag を配送し、DB 側 forwarded=true も立てる。
// ローカル通報の場合は配送スキップで DB フラグのみ更新する。
func (h *Handler) ForwardAbuseUserReport(c echo.Context) error {
	var req struct {
		ReportID string `json:"reportId"`
	}
	_ = c.Bind(&req)
	if req.ReportID == "" {
		return c.NoContent(http.StatusNoContent)
	}
	// report が存在しなければ NO_SUCH_ABUSE_REPORT
	// (upstream forward-abuse-user-report.ts:47-50)。abuseRepo が未配線だと
	// 存在を確かめられないので、確かめられないまま 204 を返さず、
	// admin/update-abuse-user-report などと同じく見つからない扱いにする (#3330)。
	if h.abuseRepo == nil {
		return c.JSON(http.StatusNotFound, noSuchForwardAbuseReport())
	}
	// snapshot for moderation log info (forwarded フラグが立つ前の状態)。
	snapshot, err := h.abuseRepo.FindByID(req.ReportID)
	// **DB 障害を not-found に丸めない** (#2792)。
	if err != nil && !repository.IsNotFound(err) {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil || snapshot == nil {
		return c.JSON(http.StatusNotFound, noSuchForwardAbuseReport())
	}
	// ログには、更新前の通報の列だけを載せる (#3267)。
	logRow := abuseReportLogRow(snapshot)
	// upstream AbuseReportService.forward の事前 guard: 対象がローカル
	// (targetUserHost == null) か、既に forwarded の場合は forward 不可。順序は
	// upstream に合わせ host==null を先に評価する。旧 mk-go はこれらを無視し
	// ローカル通報でも forwarded=true を立てていた。
	if snapshot.TargetUserHost == nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("The target user host is null."))
	}
	if snapshot.Forwarded {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("The report has already been forwarded."))
	}
	if h.abuseForwarder != nil {
		if err := h.abuseForwarder.ForwardReport(req.ReportID); err != nil {
			return apierr.JSONInternalError(c)
		}
		h.logModeration(c, moderationlog.LogForwardAbuseReport, map[string]any{
			"reportId": req.ReportID,
			"report":   logRow,
		})
		return c.NoContent(http.StatusNoContent)
	}
	// forwarder 未配線時のフォールバック: DB フラグだけ更新する (テストや
	// federation stack 未初期化パスで有効)。
	// 更新の失敗を握りつぶして 204 を返さない。upstream は
	// abuseUserReportsRepository.update の例外がそのまま INTERNAL_ERROR (500) に
	// なり、moderation log も書かない (AbuseReportService.ts:135-150) (#3330)。
	if err := h.abuseRepo.UpdateFields(req.ReportID, map[string]any{"forwarded": true}); err != nil {
		return apierr.JSONInternalError(c)
	}
	h.logModeration(c, moderationlog.LogForwardAbuseReport, map[string]any{
		"reportId": req.ReportID,
		"report":   logRow,
	})
	return c.NoContent(http.StatusNoContent)
}

// noSuchForwardAbuseReport is the NO_SUCH_ABUSE_REPORT error of
// admin/forward-abuse-user-report.
func noSuchForwardAbuseReport() map[string]any {
	return apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "8763e21b-d9bc-40be-acf6-54c1a6986493", apierr.KindServer)
}
