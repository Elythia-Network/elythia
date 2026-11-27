package meta

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/misc/secfetch"
	"github.com/labstack/echo/v5"
)

// clearSiteData is the Clear-Site-Data value upstream's /api/clear-browser-cache
// sends (ApiServerService.ts).
const clearSiteData = `"cache", "prefetchCache", "prerenderCache", "executionContexts"`

// ClearBrowserCache handles /api/clear-browser-cache like upstream
// (ApiServerService.ts, which registers it for GET and POST only since
// 2026.11.1): GET and POST answer 204 with Clear-Site-Data, which makes the
// browser drop its caches. Any other method answers 405. Upstream answers HEAD
// with 405 too (fastify's exposeHeadRoutes routes it to the same handler) and
// the rest with fastify's default 404 (docs/divergence/api.md). It needs no
// authentication.
//
// frontend の「キャッシュを消す」(utility/clear-cache.ts) はこれを GET で呼び、
// 失敗を握り潰す。無いと 404 になっても画面には出ず、キャッシュが消えないまま
// になる (#3926)。
//
// **別のサイトが起こした要求 (Sec-Fetch-Site が cross-site か same-site) には
// Clear-Site-Data を付けない。** frontend は自分のオリジンから呼ぶ (same-origin) ので、
// この判定に当たらない。
func ClearBrowserCache(c *echo.Context) error {
	switch c.Request().Method {
	case http.MethodGet, http.MethodPost:
		if !secfetch.FromOtherSite(c.Request()) {
			c.Response().Header().Set("Clear-Site-Data", clearSiteData)
		}
		return c.NoContent(http.StatusNoContent)
	}
	return c.NoContent(http.StatusMethodNotAllowed)
}
