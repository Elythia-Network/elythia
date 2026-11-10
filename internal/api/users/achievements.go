package users

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
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
		// **DB 障害を「居ない」に丸めない** (#2792、#4122)。ShowByID は不在を
		// 必ず ErrUserNotFound に包み直し、それ以外の失敗はそのまま返す。
		if !errors.Is(err, coreuser.ErrUserNotFound) {
			return apierr.JSONInternalError(c)
		}
		return apierr.JSONNoSuchUser(c)
	}
	if bundle.Profile == nil || bundle.Profile.Achievements == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	var achievements []any
	_ = json.Unmarshal(bundle.Profile.Achievements, &achievements)
	return c.JSON(http.StatusOK, achievements)
}
