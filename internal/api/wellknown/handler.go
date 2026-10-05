// Package wellknown provides /.well-known/* discovery endpoints.
package wellknown

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/permissions"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// Handler handles webfinger / host-meta / nodeinfo discovery.
type Handler struct {
	urls        *activitypub.URLBuilder
	userService *user.Service
	host        string
	origin      string // scheme + host (e.g. "https://example.com")
	metaRepo    repository.MetaRepository
}

// NewHandler constructs a Handler.
// origin は config.URL 相当の完全URL (例: "https://example.com")。
func NewHandler(urls *activitypub.URLBuilder, userService *user.Service, host, origin string) *Handler {
	return &Handler{urls: urls, userService: userService, host: host, origin: origin}
}

// SetMetaRepo attaches a MetaRepository. When set, the discovery endpoints
// (host-meta / host-meta.json / .well-known/nodeinfo / webfinger) return 403
// while federation is disabled (meta.federation == "none"), mirroring upstream
// WellKnownServerService (#1924).
func (h *Handler) SetMetaRepo(r repository.MetaRepository) {
	h.metaRepo = r
}

// federationDisabled reports whether federation is turned off. Fails open: when
// the meta repo is unwired or the fetch errors, discovery proceeds (a transient
// DB error must not silently break federation discovery). Mirrors upstream's
// `meta.federation === 'none'` gate.
func (h *Handler) federationDisabled() bool {
	if h.metaRepo == nil {
		return false
	}
	m, err := h.metaRepo.Fetch()
	if err != nil {
		return false
	}
	return m.Federation == "none"
}

// setDiscoveryCORS sets CORS headers on discovery endpoints.
// TS本家は well-known ルートに preHandler hook で無条件に
// CORSヘッダーを設定しているため、Echo の CORS middleware (Originヘッダー
// がある場合のみ発動) とは別に明示的に付与する。
func setDiscoveryCORS(c echo.Context) {
	h := c.Response().Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Accept")
	h.Set("Access-Control-Expose-Headers", "Vary")
}

// Preflight handles OPTIONS /.well-known/* — upstream WellKnownServerService の
// `fastify.options(allPath)` 相当。204 + discovery 用 CORS ヘッダだけを返す。
//
// Echo のグローバル CORS middleware は Allow-Headers に Origin/Content-Type/
// Authorization まで載せてしまうので、discovery だけ Skipper で除外して
// upstream と同じ `Accept` のみを広告する。
func (h *Handler) Preflight(c echo.Context) error {
	setDiscoveryCORS(c)
	return c.NoContent(http.StatusNoContent)
}

// Webfinger handles GET /.well-known/webfinger.
//
// クエリパラメータ resource は acct:username@host / acct:username /
// ${url}/users/<id> (actor URI) / ${url}/@username の各形式を受理する。
// マッチするローカルユーザーが存在しない場合は 404、別ホスト指定は 422、
// 形式不正は 400 を返す。Accept ヘッダで application/xrd+xml が優先された
// ときは XRD(XML)、それ以外は JRD(JSON) を返す。
func (h *Handler) Webfinger(c echo.Context) error {
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	resource := c.QueryParam("resource")
	if resource == "" {
		return c.NoContent(http.StatusBadRequest)
	}

	resolve, status := h.parseResource(resource)
	if status != 0 {
		return c.NoContent(status)
	}

	var bundle *user.UserWithProfile
	var err error
	if resolve.byID {
		bundle, err = h.userService.ShowByID(resolve.value)
	} else {
		bundle, err = h.userService.ShowByUsername(resolve.value, nil)
	}
	if err != nil {
		return c.NoContent(http.StatusNotFound)
	}
	// 別ホストのユーザー (remote) は webfinger で返さない。actor-URI 解決などで
	// 取得した user が自ホスト以外なら 404 (upstream fromId/fromAcct は host: IsNull()
	// で local 限定)。suspended local user も upstream は isSuspended:false で弾くため
	// 404 にする (= suspend したアカウントは discovery 不可、#1924)。
	if bundle.User.Host != nil && *bundle.User.Host != "" {
		return c.NoContent(http.StatusNotFound)
	}
	if bundle.User.IsSuspended || bundle.User.IsDeleted {
		return c.NoContent(http.StatusNotFound)
	}

	subject := "acct:" + bundle.User.Username + "@" + h.host
	selfHref := h.urls.UserURI(bundle.User.ID)
	profileURL := h.origin + "/@" + bundle.User.Username
	subscribe := h.origin + "/authorize-follow?acct={uri}"

	setDiscoveryCORS(c)
	c.Response().Header().Set("Vary", "Accept")
	// upstream WellKnownServerService.ts:182: `Cache-Control: public, max-age=180`。
	c.Response().Header().Set("Cache-Control", "public, max-age=180")

	// content negotiation: Accept が xrd を jrd より優先するときだけ XRD(XML)。
	if wantsXRD(c.Request().Header.Get("Accept")) {
		doc := xrdDoc{
			Subject: subject,
			Links: []xrdLink{
				{Rel: "self", Type: "application/activity+json", Href: selfHref},
				{Rel: "http://webfinger.net/rel/profile-page", Type: "text/html", Href: profileURL},
				{Rel: "http://ostatus.org/schema/1.0/subscribe", Template: subscribe},
			},
		}
		body, err := xml.Marshal(doc)
		if err != nil {
			return c.NoContent(http.StatusInternalServerError)
		}
		return c.Blob(http.StatusOK, "application/xrd+xml", append([]byte(xml.Header), body...))
	}

	resp := map[string]any{
		"subject": subject,
		"links": []map[string]any{
			{"rel": "self", "type": "application/activity+json", "href": selfHref},
			{"rel": "http://webfinger.net/rel/profile-page", "type": "text/html", "href": profileURL},
			{"rel": "http://ostatus.org/schema/1.0/subscribe", "template": subscribe},
		},
	}
	// upstream は jrd = application/jrd+json を返す。Echo の c.JSON は
	// application/json 固定なので Blob で content-type を合わせる。
	body, err := json.Marshal(resp)
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.Blob(http.StatusOK, "application/jrd+json", body)
}

// HostMeta handles GET /.well-known/host-meta.
func (h *Handler) HostMeta(c echo.Context) error {
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	setDiscoveryCORS(c)
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<XRD xmlns="http://docs.oasis-open.org/ns/xri/xrd-1.0">
  <Link rel="lrdd" type="application/xrd+xml" template="` + h.origin + `/.well-known/webfinger?resource={uri}"/>
</XRD>`
	return c.Blob(http.StatusOK, "application/xrd+xml", []byte(xml))
}

// NodeInfoDiscovery handles GET /.well-known/nodeinfo.
//
// **Cache-Control を付ける。** この文書は origin から組み立てるだけで内容が
// 変わらないのに、付けていないと crawler の 1 hit ごとに origin まで届いていた
// (同ファイルの WebFinger は `public, max-age=180` を返している)。max-age は
// ここが advertise する `/nodeinfo/2.x` と同じ 600 に揃える — 参照先より短く
// しても、先に discovery だけが失効して得るものが無い。
func (h *Handler) NodeInfoDiscovery(c echo.Context) error {
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	setDiscoveryCORS(c)
	c.Response().Header().Set("Cache-Control", "public, max-age=600")
	resp := map[string]any{
		"links": []map[string]any{
			{
				"rel":  "http://nodeinfo.diaspora.software/ns/schema/2.1",
				"href": h.origin + "/nodeinfo/2.1",
			},
			{
				"rel":  "http://nodeinfo.diaspora.software/ns/schema/2.0",
				"href": h.origin + "/nodeinfo/2.0",
			},
		},
	}
	return c.JSON(http.StatusOK, resp)
}

// HostMetaJSON handles GET /.well-known/host-meta.json.
// host-metaのJSON版。TS本家と同じレスポンスを返す。
func (h *Handler) HostMetaJSON(c echo.Context) error {
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	setDiscoveryCORS(c)
	resp := map[string]any{
		"links": []map[string]any{
			{
				"rel":      "lrdd",
				"type":     "application/jrd+json",
				"template": h.origin + "/.well-known/webfinger?resource={uri}",
			},
		},
	}
	return c.JSON(http.StatusOK, resp)
}

// OAuthAuthorizationServer handles GET /.well-known/oauth-authorization-server.
// RFC 8414 OAuth 2.0 Authorization Server Metadata。
func (h *Handler) OAuthAuthorizationServer(c echo.Context) error {
	setDiscoveryCORS(c)
	resp := map[string]any{
		"issuer":                                         h.origin,
		"authorization_endpoint":                         h.origin + "/oauth/authorize",
		"token_endpoint":                                 h.origin + "/oauth/token",
		"scopes_supported":                               permissions.All,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code"},
		"service_documentation":                          "https://misskey-hub.net",
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	}
	return c.JSON(http.StatusOK, resp)
}

// ChangePassword redirects to the password change page. Implements the W3C
// "A Well-Known URL for Changing Passwords" spec so that password managers
// (iCloud Keychain / 1Password / Chrome) can find the page programmatically.
// Mirrors upstream WellKnownServerService (upstream PR 17901).
//
// federationDisabled() は通さない。upstream が federation off で 403 を返すのは
// host-meta / host-meta.json / nodeinfo / webfinger の 4 本で、この endpoint は
// 連合ではなくローカル利用者のパスワード変更導線なので対象外。
func (h *Handler) ChangePassword(c echo.Context) error {
	// upstream の `fastify.addHook('onRequest')` は plugin scope 全体に張られるので
	// この endpoint にも discovery 用 CORS が付く。**global CORS middleware は
	// `/.well-known/` を Skipper で丸ごと除外している** ので、ここで付けないと
	// ヘッダがゼロになり、cross-origin の fetch が preflight だけ通って本命の GET で
	// 結果を捨てられる (一番切り分けにくい壊れ方)。
	setDiscoveryCORS(c)
	return c.Redirect(http.StatusFound, h.origin+"/settings/security")
}

// webfingerResolve describes how to resolve a parsed webfinger resource:
// either by user id (actor URI form) or by local username (acct / @username).
type webfingerResolve struct {
	byID  bool
	value string
}

// parseResource parses a webfinger `resource` into a resolve directive, or
// returns a non-zero HTTP status to reply with: 422 for a cross-host acct,
// 400 for a malformed resource. Mirrors upstream generateQuery / fromAcct
// (WellKnownServerService.ts:119-156): the resource is matched case-insensitively
// (upstream lowercases it), the `${url}/users/<id>` form resolves by id, the
// `${url}/@username` / `acct:` / bare forms resolve by username, and an acct
// pointing at another host yields 422.
func (h *Handler) parseResource(resource string) (webfingerResolve, int) {
	res := strings.ToLower(strings.TrimSpace(resource))
	originLower := strings.ToLower(h.origin)

	// actor URI: ${url}/users/<id>
	if prefix := originLower + "/users/"; strings.HasPrefix(res, prefix) {
		id := strings.TrimPrefix(res, prefix)
		if id == "" || strings.Contains(id, "/") {
			return webfingerResolve{}, http.StatusBadRequest
		}
		return webfingerResolve{byID: true, value: id}, 0
	}

	// acct string: ${url}/@username -> last path segment, acct:... -> after prefix,
	// otherwise the resource as-is (upstream feeds the raw value to Acct.parse).
	var acctStr string
	switch {
	case strings.HasPrefix(res, originLower+"/@"):
		acctStr = res[strings.LastIndex(res, "/")+1:]
	case strings.HasPrefix(res, "acct:"):
		acctStr = strings.TrimPrefix(res, "acct:")
	default:
		acctStr = res
	}

	username, host := parseAcct(acctStr)
	// 別ホスト指定は upstream fromAcct と同じく 422。
	if host != "" && host != strings.ToLower(h.host) {
		return webfingerResolve{}, http.StatusUnprocessableEntity
	}
	if username == "" {
		return webfingerResolve{}, http.StatusBadRequest
	}
	return webfingerResolve{byID: false, value: username}, 0
}

// parseAcct splits an acct string into username and host, stripping a leading
// "@" (mirrors misskey-js Acct.parse). host is "" when absent.
func parseAcct(acct string) (username, host string) {
	acct = strings.TrimPrefix(acct, "@")
	parts := strings.SplitN(acct, "@", 2)
	username = parts[0]
	if len(parts) == 2 {
		host = parts[1]
	}
	return username, host
}

// xrdLink / xrdDoc render the XRD (XML) webfinger response. encoding/xml escapes
// attribute values, so user-derived fields (subject/href) are safe.
type xrdLink struct {
	Rel      string `xml:"rel,attr"`
	Type     string `xml:"type,attr,omitempty"`
	Href     string `xml:"href,attr,omitempty"`
	Template string `xml:"template,attr,omitempty"`
}

type xrdDoc struct {
	XMLName xml.Name  `xml:"http://docs.oasis-open.org/ns/xri/xrd-1.0 XRD"`
	Subject string    `xml:"Subject"`
	Links   []xrdLink `xml:"Link"`
}

const (
	mimeJRD = "application/jrd+json"
	mimeXRD = "application/xrd+xml"
)

// wantsXRD reports whether the client prefers XRD (XML) over JRD (JSON) for the
// webfinger response, mirroring upstream `request.accepts().type([jrd, xrd]) === xrd`.
// jrd is listed first, so ties (and */* / no Accept) resolve to JRD; XRD is only
// chosen when its effective q-value strictly exceeds jrd's.
func wantsXRD(accept string) bool {
	if accept == "" {
		return false
	}
	return acceptQuality(accept, mimeXRD) > acceptQuality(accept, mimeJRD)
}

// acceptQuality returns the highest q-value matching the given media type (or
// */*) in the Accept header, or 0 when none match. Only the exact type matches:
// upstream negotiates against [jrd, xrd] specifically, so generic application/*
// or sibling subtypes (application/json/xml) are not treated as a match.
func acceptQuality(accept, mediaType string) float64 {
	best := 0.0
	for _, part := range strings.Split(accept, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		segs := strings.Split(token, ";")
		media := strings.ToLower(strings.TrimSpace(segs[0]))
		q := 1.0
		for _, p := range segs[1:] {
			p = strings.TrimSpace(p)
			if v, ok := strings.CutPrefix(p, "q="); ok {
				q = parseQ(v)
			}
		}
		if (media == "*/*" || media == mediaType) && q > best {
			best = q
		}
	}
	return best
}

// parseQ parses an Accept q-value (0.0-1.0), defaulting to 0 on parse failure.
func parseQ(s string) float64 {
	switch {
	case s == "" || s == "1" || s == "1.0" || s == "1.00" || s == "1.000":
		return 1.0
	case s == "0" || s == "0.0":
		return 0.0
	}
	// 簡易 parse: 0.x 形式のみ想定 (Accept の q は [0,1])。
	var whole, frac float64
	var fracDigits int
	dot := false
	for _, r := range s {
		switch {
		case r == '.':
			dot = true
		case r >= '0' && r <= '9':
			if dot {
				frac = frac*10 + float64(r-'0')
				fracDigits++
			} else {
				whole = whole*10 + float64(r-'0')
			}
		default:
			return 0
		}
	}
	for i := 0; i < fracDigits; i++ {
		frac /= 10
	}
	q := whole + frac
	// RFC 7231: q は [0,1]。範囲外は 1.0 にクランプする。
	if q > 1.0 {
		return 1.0
	}
	return q
}
