package admin

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/model"
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
	// snapshot for moderation log info (forwarded フラグが立つ前の状態)。
	// abuseRepo が wired で report が存在しなければ NO_SUCH_ABUSE_REPORT
	// (upstream forward-abuse-user-report.ts:47-50)。未配線時は従来どおり通す。
	var snapshot *model.AbuseUserReport
	if h.abuseRepo != nil {
		s, err := h.abuseRepo.FindByID(req.ReportID)
		// **DB 障害を not-found に丸めない** (#2792)。
		if err != nil && !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		if err != nil || s == nil {
			return c.JSON(http.StatusNotFound, apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "8763e21b-d9bc-40be-acf6-54c1a6986493", apierr.KindServer))
		}
		snapshot = s
	}
	// ログには、更新前の通報の列だけを載せる (#3267)。
	var logRow *model.AbuseUserReport
	if snapshot != nil {
		logRow = abuseReportLogRow(snapshot)
	}
	// upstream AbuseReportService.forward の事前 guard: 対象がローカル
	// (targetUserHost == null) か、既に forwarded の場合は forward 不可。順序は
	// upstream に合わせ host==null を先に評価する。旧 mk-go はこれらを無視し
	// ローカル通報でも forwarded=true を立てていた。
	if snapshot != nil {
		if snapshot.TargetUserHost == nil {
			return c.JSON(http.StatusBadRequest, apierr.InvalidParam("The target user host is null."))
		}
		if snapshot.Forwarded {
			return c.JSON(http.StatusBadRequest, apierr.InvalidParam("The report has already been forwarded."))
		}
	}
	if h.abuseForwarder != nil {
		if err := h.abuseForwarder.ForwardReport(req.ReportID); err != nil {
			return apierr.JSONInternalError(c)
		}
		if snapshot != nil {
			h.logModeration(c, moderationlog.LogForwardAbuseReport, map[string]any{
				"reportId": req.ReportID,
				"report":   logRow,
			})
		}
		return c.NoContent(http.StatusNoContent)
	}
	// forwarder 未配線時のフォールバック: DB フラグだけ更新する (テストや
	// federation stack 未初期化パスで有効)。
	if h.abuseRepo != nil {
		if err := h.abuseRepo.UpdateFields(req.ReportID, map[string]any{"forwarded": true}); err == nil && snapshot != nil {
			h.logModeration(c, moderationlog.LogForwardAbuseReport, map[string]any{
				"reportId": req.ReportID,
				"report":   logRow,
			})
		}
	}
	return c.NoContent(http.StatusNoContent)
}
