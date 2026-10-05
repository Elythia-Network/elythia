package meta

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/core/serverstats"
)

// ServerInfo returns the public subset of server machine statistics.
//
// GET|POST /api/server-info
//
// **公開エンドポイントは machine / cpu / mem / fs のみ** (upstream public
// server-info.ts)。os / node / psql / redis / net は admin 専用で、未認証には
// 出さない。
//
// `enableServerMachineStats` が無効なら空の shape を返す。**フィールドごと
// 落とさない** — frontend の server-metric widget は各キーを直接読む。
func (h *Handler) ServerInfo(c echo.Context) error {
	if h.metaRepo != nil {
		if m, err := h.metaRepo.Fetch(); err == nil && m != nil && m.EnableServerMachineStats {
			return c.JSON(http.StatusOK, serverstats.CollectPublic())
		}
	}
	return c.JSON(http.StatusOK, serverstats.EmptyPublic())
}
