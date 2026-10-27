package admin

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
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

// RelaysRemove handles POST /api/admin/relays/remove. It looks the relay up
// by inbox, like upstream. When the relay service is configured, it sends an
// Undo(Follow) to the relay inbox before deleting the row.
//
// 以前は Elythia 独自の id でも引いた (inbox より優先) が、本家の paramDef は
// inbox を必須にしていて (#3861 から拒否に使う)、id だけの入力は既に 400 だった。
// 同梱の frontend も inbox だけを送るので、#3867 で消した。
func (h *Handler) RelaysRemove(c echo.Context) error {
	var req struct {
		Inbox string `json:"inbox"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if req.Inbox == "" || h.adminDB == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if h.relayService != nil {
		var rel model.Relay
		if err := h.adminDB.Where(`"inbox" = ?`, req.Inbox).First(&rel).Error; err != nil {
			// **DB 障害を not-found に丸めない** (#2792)。
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return apierr.JSONInternalError(c)
			}
			return c.NoContent(http.StatusNoContent)
		}
		if err := h.relayService.Remove(c.Request().Context(), rel.ID); err != nil {
			return apierr.JSONInternalError(c)
		}
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.adminDB.Where(`"inbox" = ?`, req.Inbox).Delete(&model.Relay{}).Error; err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}
