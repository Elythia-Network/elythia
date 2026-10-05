package federation

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// UpdateRemoteUser handles POST /api/federation/update-remote-user.
//
// 対象のリモートユーザーを ActivityPub actor fetch で強制更新する。管理者用。
func (h *Handler) UpdateRemoteUser(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.userRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		// #2106 L29: upstream は GetterService の IdentifiableError(15348ddd) を
		// ApiCallService が INTERNAL_ERROR(500) に包む。mk-go は意図的に明快な
		// 404 NO_SUCH_USER を返す (worse な 500 拡散を避ける)。
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_USER", "No such user.", "ae1bd95a-1a3b-4d4e-9c47-7e76a2020f8e"))
	}
	if user.URI == nil || *user.URI == "" {
		// ローカルユーザーは対象外。
		// #2106 L29: upstream は local user 指定を Error('user is not a remote user') で
		// 500 にするが、mk-go は remote 更新エンドポイントで local 指定が実質 no-op のため
		// 204 を返す。
		return c.NoContent(http.StatusNoContent)
	}
	if h.resolver == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if _, err := h.resolver.ForceResolveActor(*user.URI); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}
