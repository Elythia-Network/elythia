package following

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// RequestsSent handles POST /api/following/requests/sent.
//
// 呼び出しユーザーが送った未承認の follow request を返す。承認済の関係は
// Following テーブルへ移るのでここには含まれない。
func (h *Handler) RequestsSent(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		Limit     *jsnum.Int `json:"limit"`
		SinceID   string     `json:"sinceId"`
		UntilID   string     `json:"untilId"`
		SinceDate *jsnum.Int `json:"sinceDate"`
		UntilDate *jsnum.Int `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursorIDFirst(req.SinceID, req.UntilID, req.SinceDate.Int64Ptr(), req.UntilDate.Int64Ptr())
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.followingService.ListSentRequests(me.ID, limit, sinceID, untilID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.packRequests(rows))
}
