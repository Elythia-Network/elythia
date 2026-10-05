package admin

import (
	"net/http"
	"net/url"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
)

// RelaysAdd handles POST /api/admin/relays/add. Routes through the
// relay Service so that a Follow activity is actually dispatched to
// the relay's inbox (#161). Falls back to raw DB insertion when the
// service is not wired (tests or early boot).
func (h *Handler) RelaysAdd(c echo.Context) error {
	var req struct {
		Inbox string `json:"inbox"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// upstream add.ts は `new URL(inbox).protocol !== 'https:'` または URL parse
	// 失敗で INVALID_URL を投げる。https 以外 (http:// / 非 URL / 空) を relay 行
	// 作成前に弾く。Go の url.Parse は相対 URL でも err にならないので、scheme と
	// host の存在も明示的に検証する。
	if u, err := url.Parse(req.Inbox); err != nil || u.Scheme != "https" || u.Host == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_URL", "Invalid URL", "fb8c92d3-d4e5-44e7-b3d4-800d5cef8b2c"))
	}
	if h.relayService != nil {
		rel, err := h.relayService.Add(c.Request().Context(), req.Inbox)
		if err != nil {
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusOK, rel)
	}
	if h.adminDB == nil {
		return c.NoContent(http.StatusNoContent)
	}
	relay := &model.Relay{
		ID: h.idGen.Generate(time.Now()), Inbox: req.Inbox, Status: "requesting",
	}
	h.adminDB.Create(relay)
	return c.JSON(http.StatusOK, relay)
}

// RelaysList handles POST /api/admin/relays/list.
func (h *Handler) RelaysList(c echo.Context) error {
	if h.relayService != nil {
		list, err := h.relayService.List(c.Request().Context())
		if err != nil {
			return apierr.JSONInternalError(c)
		}
		if list == nil {
			list = []*model.Relay{}
		}
		return c.JSON(http.StatusOK, list)
	}
	if h.adminDB == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	var relays []*model.Relay
	h.adminDB.Order(`"id" DESC`).Find(&relays)
	return c.JSON(http.StatusOK, relays)
}

// RelaysRemove handles POST /api/admin/relays/remove. When the relay
// service is configured, it sends an Undo(Follow) to the relay inbox
// before deleting the row.
func (h *Handler) RelaysRemove(c echo.Context) error {
	var req struct {
		ID    string `json:"id"`
		Inbox string `json:"inbox"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if h.relayService != nil {
		id := req.ID
		if id == "" && req.Inbox != "" && h.adminDB != nil {
			// 本家互換のため inbox 指定でも remove できるようにする
			var rel model.Relay
			if err := h.adminDB.Where(`"inbox" = ?`, req.Inbox).First(&rel).Error; err == nil {
				id = rel.ID
			}
		}
		if id == "" {
			return c.NoContent(http.StatusNoContent)
		}
		if err := h.relayService.Remove(c.Request().Context(), id); err != nil {
			return apierr.JSONInternalError(c)
		}
		return c.NoContent(http.StatusNoContent)
	}
	if h.adminDB == nil {
		return c.NoContent(http.StatusNoContent)
	}
	h.adminDB.Where(`"id" = ?`, req.ID).Delete(&model.Relay{})
	return c.NoContent(http.StatusNoContent)
}
