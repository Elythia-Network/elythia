package meta

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// clearSiteData is the Clear-Site-Data value upstream's /api/clear-browser-cache
// sends (ApiServerService.ts).
const clearSiteData = `"cache", "prefetchCache", "prerenderCache", "executionContexts"`

// ClearBrowserCache handles /api/clear-browser-cache like upstream
// (ApiServerService.ts registers it with fastify.all): GET and POST answer 204
// with Clear-Site-Data, which makes the browser drop its caches; any other
// method answers 405. It needs no authentication.
//
// frontend の「キャッシュを消す」(utility/clear-cache.ts) はこれを GET で呼び、
// 失敗を握り潰す。無いと 404 になっても画面には出ず、キャッシュが消えないまま
// になる (#3926)。
func ClearBrowserCache(c echo.Context) error {
	switch c.Request().Method {
	case http.MethodGet, http.MethodPost:
		c.Response().Header().Set("Clear-Site-Data", clearSiteData)
		return c.NoContent(http.StatusNoContent)
	}
	return c.NoContent(http.StatusMethodNotAllowed)
}
