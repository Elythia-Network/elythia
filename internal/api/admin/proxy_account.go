package admin

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/labstack/echo/v4"
)

// UpdateProxyAccount handles POST /api/admin/update-proxy-account.
//
// 本家 update-proxy-account.ts と同じく proxy system user の profile.description
// を更新する handler (旧 stub は誤って meta.proxyAccountId を書き換えていた、#348)。
// frontend admin/settings 画面の proxyAccountForm が `{ description }` 形式で
// 呼び出すのでこれに合わせる。Response は upstream update-proxy-account.ts と同じく
// MeDetailed schema で返す (#1539)。
func (h *Handler) UpdateProxyAccount(c echo.Context) error {
	var req struct {
		Description *string `json:"description"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	if h.systemAccountFetcher == nil || h.userRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	proxy, err := h.systemAccountFetcher.Fetch("proxy")
	if err != nil || proxy == nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if req.Description != nil {
		if err := h.userRepo.UpdateProfile(proxy.ID, map[string]any{"description": req.Description}); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		// Misskey TS update-proxy-account.ts は before を null で記録する
		// (TODO コメントで before 取得が未実装の状態)。互換性のため同じ
		// schema で出力する。
		h.logModeration(c, moderationlog.LogUpdateProxyAccountDescription, map[string]any{
			"before": nil,
			"after":  req.Description,
		})
	}
	// 更新後 profile を再取得して MeDetailed を返す (upstream schema 'MeDetailed')。
	//
	// 本家は pack(proxy.id, proxy, {schema: 'MeDetailed'}) で、proxy 自身を閲覧者に
	// する。ピン留め・移行先も proxy から見た形で埋め、createdAt と policies など
	// MeDetailed の項目は他の self 経路と同じく meself.PackMe で揃える (#3330)。
	profile, _ := h.userRepo.FindProfileByUserID(proxy.ID)
	ctx := c.Request().Context()
	d := entity.PackUserDetailed(proxy, profile, h.idGen)
	if h.detailExtras != nil {
		h.detailExtras.FillDetailedExtras(ctx, proxy, proxy, profile, &d)
	}
	return c.JSON(http.StatusOK, meself.PackMe(ctx, d, proxy, profile))
}
