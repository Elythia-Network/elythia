package admin

import (
	"encoding/json"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
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
	// descriptionは、欠け(変えない)とnull(消す)を見分ける。本家のparamDefは
	// nullableで、本家はnullをそのまま書いて説明を消す。以前は*stringで受けて
	// いたので、nullを欠けと同じに扱い、説明を消せなかった (#3861)。
	var req struct {
		Description json.RawMessage `json:"description"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	var description *string
	present := len(req.Description) > 0
	if present && string(req.Description) != "null" {
		var s string
		if err := json.Unmarshal(req.Description, &s); err != nil {
			return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
		}
		description = &s
	}
	if h.systemAccountFetcher == nil || h.userRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	proxy, err := h.systemAccountFetcher.Fetch("proxy")
	if err != nil || proxy == nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if present {
		// **変更前の値を残す。** upstream update-proxy-account.ts は before を null で
		// 記録する (TODO のまま) が、それでは何から何へ変えたかが追えない。形
		// ({before, after}) は同じなので、表示はそのまま使える。
		var before any
		if prev, perr := h.userRepo.FindProfileByUserID(proxy.ID); perr == nil && prev != nil {
			before = prev.Description
		}
		if err := h.userRepo.UpdateProfile(proxy.ID, map[string]any{"description": description}); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		h.logModeration(c, moderationlog.LogUpdateProxyAccountDescription, map[string]any{
			"before": before,
			"after":  description,
		})
	}
	// 更新後 profile を再取得して MeDetailed を返す (upstream schema 'MeDetailed')。
	//
	// 本家は pack(proxy.id, proxy, {schema: 'MeDetailed'}) で、proxy 自身を閲覧者に
	// する。ピン留め・移行先も proxy から見た形で埋め、createdAt と policies など
	// MeDetailed の項目は他の self 経路と同じく meself.Packer.PackMe で揃える (#3330)。
	profile, _ := h.userRepo.FindProfileByUserID(proxy.ID)
	ctx := c.Request().Context()
	d := h.entityPacker.PackUserDetailed(proxy, profile, h.idGen)
	if h.detailExtras != nil {
		h.detailExtras.FillDetailedExtras(ctx, proxy, proxy, profile, &d)
	}
	return c.JSON(http.StatusOK, h.selfPacker.PackMe(ctx, d, proxy, profile))
}
