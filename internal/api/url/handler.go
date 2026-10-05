// Package url provides the /api/url endpoint for URL preview.
package url

import (
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/urlpreview"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/labstack/echo/v4"
)

// Handler handles the URL preview endpoint.
type Handler struct {
	fetcher *urlpreview.Fetcher
}

// NewHandler creates a new URL preview handler.
func NewHandler(fetcher *urlpreview.Fetcher) *Handler {
	return &Handler{fetcher: fetcher}
}

// Preview handles GET /url (matches Misskey's endpoint path).
// フロントエンドがリンクプレビューカード表示のために呼ぶ。
func (h *Handler) Preview(c echo.Context) error {
	// #2106 L44: upstream は lang query を summaly に渡し OGP の言語別 alternate を選ぶが、
	// mk-go は HTML を直接 parse する設計のため lang を honor できず無視する (preview の
	// 言語選択のみ乖離、実害は軽微)。
	rawURL := c.QueryParam("url")
	if rawURL == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "url is required.", apierr.UUIDInvalidParam))
	}

	result, err := h.fetcher.Fetch(c.Request().Context(), rawURL)
	if err != nil {
		// #2106 N18: upstream UrlPreviewService は disabled で 403 URL_PREVIEW_DISABLED、
		// 取得失敗で 422 URL_PREVIEW_FAILED を固有 UUID 付きで返す。以前は両方 200+null
		// preview を返しており frontend MkUrlPreview の !res.ok 分岐が機能しなかった。
		if errors.Is(err, urlpreview.ErrDisabled) {
			return c.JSON(http.StatusForbidden, apierr.Error("URL_PREVIEW_DISABLED",
				"URL preview is disabled", "58b36e13-d2f5-0323-b0c6-76aa9dabefb8"))
		}
		// upstream 同様、失敗も 1 日キャッシュして同一 URL の re-fetch を抑止する。
		c.Response().Header().Set("Cache-Control", "max-age=86400, immutable")
		return c.JSON(http.StatusUnprocessableEntity, apierr.Error("URL_PREVIEW_FAILED",
			"Failed to get preview", "09d01cb5-53b9-4856-82e5-38a50c290a3b"))
	}

	// thumbnail (OGP og:image) / icon (favicon) は外部サイトの画像。frontend
	// (MkUrlPreview) がこれらを直接描画するため、proxy 経由に書き換えて閲覧者の
	// IP が外部サイトへ漏洩するのを防ぐ (issue #1529)。player.url は iframe embed
	// なので対象外。
	//
	// **署名は期限付きにする (#3037)。** ここで包むのは**利用者が渡した
	// URL のページに書いてあった URL**、つまり攻撃者が自由に決められる値。
	// 無期限の署名を出すと、それを貼るだけで media proxy の allowlist を
	// 恒久的に迂回できる (allowlist は mk-go 独自の硬化で、upstream の
	// `/proxy` は元から open proxy なので「退化」の概念が無い)。
	result.Thumbnail = entity.ProxyUserSuppliedMediaURLPtr(result.Thumbnail)
	result.Icon = entity.ProxyUserSuppliedMediaURLPtr(result.Icon)
	// #2106 N18: upstream は成功 preview を 1 日キャッシュさせる。
	c.Response().Header().Set("Cache-Control", "max-age=86400, immutable")
	return c.JSON(http.StatusOK, result)
}
