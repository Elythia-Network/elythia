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
// (ApiServerService.ts registers it with fastify.all): GET and POST answer 204
// with Clear-Site-Data, which makes the browser drop its caches; any other
// method answers 405. It needs no authentication.
//
// frontend の「キャッシュを消す」(utility/clear-cache.ts) はこれを GET で呼び、
// 失敗を握り潰す。無いと 404 になっても画面には出ず、キャッシュが消えないまま
// になる (#3926)。
//
// **別のサイトが起こした要求には Clear-Site-Data を付けない** (本家は付ける)。
// executionContexts は、このサーバーの開いているタブをすべて読み込み直させるので、
// 別のサイトの <img> や遷移で、書きかけの投稿などを失わせられる。frontend は自分の
// オリジンから呼ぶ (Sec-Fetch-Site が same-origin) ので、この判定に当たらない。
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
