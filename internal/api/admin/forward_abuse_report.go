package admin

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/repository"
)

// ForwardAbuseUserReport handles POST /api/admin/forward-abuse-user-report.
//
// It delivers an ActivityPub Flag signed by the system actor to the inbox of
// the target user's origin instance and sets forwarded=true. Reports against
// a local user, or reports that have already been forwarded, are rejected
// with 400 INVALID_PARAM (upstream answers 500; see docs/divergence.md).
func (h *Handler) ForwardAbuseUserReport(c echo.Context) error {
	var body struct {
		ReportID json.RawMessage `json:"reportId"`
	}
	if err := c.Bind(&body); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// 本家の paramDef は reportId を required かつ format misskey:id にしており、
	// 欠落・型違い・空文字は ajv が INVALID_PARAM (400) で弾く。旧 mk-go は
	// 欠落と空文字を黙って 204 にしていた (#3330)。info は ajv の最初の違反
	// (required → type → format の順) に合わせる。
	if body.ReportID == nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient("#/required", "must have required property 'reportId'"))
	}
	var reportID string
	if string(body.ReportID) == "null" || json.Unmarshal(body.ReportID, &reportID) != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient("#/properties/reportId/type", "must be string"))
	}
	if !misskeyIDFormat.MatchString(reportID) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient("#/properties/reportId/format", `must match format "misskey:id"`))
	}
	// report が存在しなければ NO_SUCH_ABUSE_REPORT
	// (upstream forward-abuse-user-report.ts:47-50)。abuseRepo が未配線だと
	// 存在を確かめられないので、確かめられないまま 204 を返さず、
	// admin/update-abuse-user-report などと同じく見つからない扱いにする (#3330)。
	if h.abuseRepo == nil {
		return c.JSON(http.StatusNotFound, noSuchForwardAbuseReport())
	}
	// snapshot for moderation log info (forwarded フラグが立つ前の状態)。
	snapshot, err := h.abuseRepo.FindByID(reportID)
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
	// 本家は素の Error を投げるので INTERNAL_ERROR (500) になるが、利用者の
	// 操作の誤りなので mk-go は 400 を返す。意図的な差として
	// docs/divergence.md に記録している (#3330)。
	if snapshot.TargetUserHost == nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("The target user host is null."))
	}
	if snapshot.Forwarded {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("The report has already been forwarded."))
	}
	if h.abuseForwarder != nil {
		if err := h.abuseForwarder.ForwardReport(reportID); err != nil {
			return apierr.JSONInternalError(c)
		}
		h.logModeration(c, moderationlog.LogForwardAbuseReport, map[string]any{
			"reportId": reportID,
			"report":   logRow,
		})
		return c.NoContent(http.StatusNoContent)
	}
	// forwarder 未配線時のフォールバック: DB フラグだけ更新する (テストや
	// federation stack 未初期化パスで有効)。
	// 更新の失敗を握りつぶして 204 を返さない。upstream は
	// abuseUserReportsRepository.update の例外がそのまま INTERNAL_ERROR (500) に
	// なり、moderation log も書かない (AbuseReportService.ts:135-150) (#3330)。
	if err := h.abuseRepo.UpdateFields(reportID, map[string]any{"forwarded": true}); err != nil {
		return apierr.JSONInternalError(c)
	}
	h.logModeration(c, moderationlog.LogForwardAbuseReport, map[string]any{
		"reportId": reportID,
		"report":   logRow,
	})
	return c.NoContent(http.StatusNoContent)
}

// misskeyIDFormat is the upstream ajv `misskey:id` format
// (endpoint-base.ts: `/^[a-zA-Z0-9]+$/`).
var misskeyIDFormat = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// noSuchForwardAbuseReport is the NO_SUCH_ABUSE_REPORT error of
// admin/forward-abuse-user-report.
func noSuchForwardAbuseReport() map[string]any {
	return apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "8763e21b-d9bc-40be-acf6-54c1a6986493", apierr.KindServer)
}
