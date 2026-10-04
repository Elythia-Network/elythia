package server

import (
	"net/http"
	urlpkg "net/url"
	"path/filepath"

	"github.com/labstack/echo/v4"
)

// staticAssetCacheControl is the Cache-Control header value used for
// long-lived browser-cached static asset routes (/twemoji, /fluent-emoji).
// 30 days matches upstream Misskey TS `ClientServerService.ts` `ms('30 days')`.
// `immutable` enables aggressive cache: the browser SKIPS revalidation
// entirely until the entry expires, eliminating repeat HTTP requests for
// already-fetched assets. Safe because the assets are content-addressed by
// hex codepoint and never mutate in place (a new emoji set ships with new
// filenames).
const staticAssetCacheControl = "public, max-age=2592000, immutable"

// assetCSP is the Content-Security-Policy upstream Misskey TS attaches to its
// asset routes (`ClientServerService.ts` / `ServerService.ts`)。
//
// `default-src 'none'` で script / frame / connect 等を全面禁止し、SVG や PNG が
// 単体で開かれたときに何も実行できないようにする。`style-src 'unsafe-inline'` は
// SVG が持つ inline style を壊さないための upstream 由来の緩和。
//
// 値は upstream の文字列そのまま。**独自に強めない。** mk-go でだけ厳しくすると、
// TS から切り替えたときに「mk-go に載せると表示が壊れる」形の差になる。
const assetCSP = "default-src 'none'; style-src 'unsafe-inline'"

// serveStaticAssetDir mirrors echo.Echo.Static(prefix, root) but attaches
// the long-lived `Cache-Control` header. Used for /twemoji and
// /fluent-emoji to align with upstream Misskey TS browser cache policy.
//
// Echo's built-in `Static` does not accept middleware or response-header
// hooks, so this helper duplicates Echo's tiny `prefix+"/*"` + `c.File`
// pattern (path traversal is rejected via `filepath.Clean` per Echo's
// own implementation) and adds the header before serving.
func serveStaticAssetDir(e *echo.Echo, prefix, root string) {
	e.GET(prefix+"/*", func(c echo.Context) error {
		p, err := urlpkg.PathUnescape(c.Param("*"))
		if err != nil {
			// malformed URL escape は client 起因なので 400 にする。echo.Static
			// は raw error を返して 500 になるが、本 helper では proper HTTP
			// semantics を優先する。
			return echo.NewHTTPError(http.StatusBadRequest, "invalid path escape")
		}
		c.Response().Header().Set("Cache-Control", staticAssetCacheControl)
		c.Response().Header().Set("Content-Security-Policy", assetCSP)
		return c.File(filepath.Join(root, filepath.Clean("/"+p)))
	})
}
