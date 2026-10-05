package i

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// SigninHistory handles POST /api/i/signin-history.
func (h *Handler) SigninHistory(c echo.Context) error {
	if h.signinRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	u := middleware.GetUser(c)
	// この handler だけ SinceID / UntilID が `*string` で、他の cursor
	// endpoint (= 多くが string) と shape が異なる。歴史的経緯で「未指定」と
	// 「空文字明示」を区別する必要があった経路。`*string` を unwrap して
	// 空文字 string に揃えてから id.NormalizeCursor に渡す 2-step。
	var req struct {
		Limit     *int    `json:"limit"`
		SinceID   *string `json:"sinceId"`
		UntilID   *string `json:"untilId"`
		SinceDate *int64  `json:"sinceDate"`
		UntilDate *int64  `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid param.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	limit := 10
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 {
			limit = 1
		}
		if limit > 100 {
			limit = 100
		}
	}
	sinceID := ""
	if req.SinceID != nil {
		sinceID = *req.SinceID
	}
	untilID := ""
	if req.UntilID != nil {
		untilID = *req.UntilID
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。unwrap 後の
	// string 値を渡す。
	cursorSince, cursorUntil, cursorOK := id.NormalizeCursor(sinceID, untilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	sinceID, untilID = cursorSince, cursorUntil

	rows, err := h.signinRepo.ListByUserID(u.ID, limit, untilID, sinceID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	result := make([]map[string]any, len(rows))
	for i, s := range rows {
		entry := map[string]any{
			"id":      s.ID,
			"ip":      s.IP,
			"headers": s.Headers,
			"success": s.Success,
		}
		if t, err := h.idGen.ParseTime(s.ID); err == nil {
			entry["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		result[i] = entry
	}
	return c.JSON(http.StatusOK, result)
}
