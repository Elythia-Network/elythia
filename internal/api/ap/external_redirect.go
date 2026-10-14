package ap

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
)

// maybeAPLookup matches an Accept header of an ActivityPub object lookup
// (upstream ServerService の maybeApLookupRegex)。
var maybeAPLookup = regexp.MustCompile(`(?i)application/activity\+json|application/ld\+json.+activitystreams`)

// SetExternalRedirectPolicy wires meta.allowExternalApRedirect. allow is read
// on every redirect so the setting takes effect without a restart. selfURL is
// this server's URL (config.url); a redirect is local only when its scheme and
// host (with the port) match it.
func (h *Handler) SetExternalRedirectPolicy(allow func() bool, selfURL string) {
	h.allowExternalRedirect = allow
	h.selfOrigin = nil
	if u, err := url.Parse(selfURL); err == nil && u.Host != "" {
		h.selfOrigin = u
	}
}

// redirectAP answers a lookup of a remote object with a redirect to its
// original URI, unless meta.allowExternalApRedirect is off and the request
// looks like an ActivityPub lookup, in which case it refuses with 406 like
// upstream.
//
// **この設定は「自分が返す外部への 3xx」を止めるもの** (upstream ServerService の
// onSend hook)。自サーバーの URL を踏み台にして、取得した URL を確かめない相手に
// 第三者のオブジェクトを読ませない。以前は外向きの AP の取得でリダイレクトを
// 追わない設定として使っており、管理者が false にしても受け側は外部へ 301 / 302 を
// 返し続け、代わりに `.well-known` や actor を別ホストへ委譲している相手との
// 連合が黙って壊れていた。
func (h *Handler) redirectAP(c echo.Context, code int, location string) error {
	if h.allowExternalRedirect != nil && !h.allowExternalRedirect() &&
		maybeAPLookup.MatchString(c.Request().Header.Get("Accept")) && !h.isSelfURL(location) {
		if u, err := url.Parse(location); err == nil {
			c.Response().Header().Set("Link", "<"+u.String()+">; rel=\"canonical\"")
		}
		return c.String(http.StatusNotAcceptable, strings.Join([]string{
			"Refusing to relay remote ActivityPub object lookup.",
			"",
			"Please remove 'application/activity+json' and 'application/ld+json' from the Accept header or fetch using the authoritative URL at " + location + ".",
		}, "\n"))
	}
	return c.Redirect(code, location)
}

// isSelfURL reports whether location points at this server.
//
// upstream は `https://${config.host}/` の前方一致で見る。hostname だけで比べると、
// 同じ hostname の別のポートや別の scheme のサーバーを自分と取り違える。
func (h *Handler) isSelfURL(location string) bool {
	u, err := url.Parse(location)
	if err != nil {
		return false
	}
	if u.Scheme == "" && u.Host == "" {
		// 相対の転送は自分の中に留まる。
		return true
	}
	if h.selfOrigin != nil {
		return strings.EqualFold(u.Scheme, h.selfOrigin.Scheme) && strings.EqualFold(u.Host, h.selfOrigin.Host)
	}
	return h.localHost != "" && strings.EqualFold(u.Host, h.localHost)
}
