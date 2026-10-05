package users

import (
	"encoding/json"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/labstack/echo/v4"
)

// Achievements handles POST /api/users/achievements.
func (h *Handler) Achievements(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	bundle, err := h.userService.ShowByID(req.UserID)
	if err != nil {
		return apierr.JSONNoSuchUser(c)
	}
	if bundle.Profile == nil || bundle.Profile.Achievements == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	var achievements []any
	_ = json.Unmarshal(bundle.Profile.Achievements, &achievements)
	return c.JSON(http.StatusOK, achievements)
}
