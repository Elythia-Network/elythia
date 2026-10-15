package oauth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// maxClientDocBytes caps how much of the client_id document we read.
const maxClientDocBytes = 1 << 20 // 1 MiB

// Caps on the discovered name and logo URL. They are kept in the authorize
// transaction (Redis, cacheTTL) and rendered on the consent page.
//
// 承認の待ちに置く値の大きさを、文書の大きさによらず一定に収める。名前は表示に
// 足りる長さで切り、切ると意味を失うロゴの URL は長すぎれば出さない。
const (
	maxClientNameRunes = 256
	maxClientLogoBytes = 2048
)

// errInvalidClient indicates the client_id or its discovered metadata is
// invalid. Surfaced to the caller as an invalid_request authorization error.
var errInvalidClient = errors.New("oauth: invalid client")

// clientInfo is the discovered client metadata used to render the consent
// prompt and validate the redirect_uri.
type clientInfo struct {
	ID           string
	RedirectURIs []string
	Name         string
	Logo         string
}

var domainHostRe = regexp.MustCompile(`\.\w+$`)

// validateClientID enforces the IndieAuth client-identifier rules
// (https://indieauth.spec.indieweb.org/#client-identifier), mirroring upstream
// validateClientId. allowHTTP permits http scheme (test only); production
// requires https.
func validateClientID(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errInvalidClient
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return nil, errInvalidClient
	}
	// path component is required (url.Parse leaves "" for no path); normalize to "/".
	if u.Path == "" {
		u.Path = "/"
	}
	// no single-dot / double-dot path segments.
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, errInvalidClient
		}
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errInvalidClient
	}
	if u.User != nil {
		return nil, errInvalidClient
	}
	// host names MUST be domain names or a loopback interface, NOT raw IPs
	// (except loopback). A domain has a trailing `.tld`; loopback hosts are
	// allowed explicitly.
	host := u.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if !loopback && !domainHostRe.MatchString(host) {
		return nil, errInvalidClient
	}
	return u, nil
}

// discoverClientInformation fetches the client_id document and extracts the
// client metadata (redirect_uris, name, logo) via IndieAuth client discovery:
// JSON client metadata (RFC7591-ish) when the document is application/json,
// otherwise HTML h-app microformats. Link headers contribute redirect_uris in
// both cases. The supplied client must be SSRF-guarded.
func discoverClientInformation(httpClient *http.Client, id string) (*clientInfo, error) {
	req, err := http.NewRequest(http.MethodGet, id, nil)
	if err != nil {
		return nil, errInvalidClient
	}
	req.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errInvalidClient
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errInvalidClient
	}
	// 最終 URL (redirect 後) を base にして相対 URL を解決する。
	baseURL := id
	if resp.Request != nil && resp.Request.URL != nil {
		baseURL = resp.Request.URL.String()
	}

	info := &clientInfo{ID: id, Name: id}

	// Link header rel=redirect_uri (both JSON and HTML paths)。
	for _, lh := range resp.Header.Values("Link") {
		info.RedirectURIs = append(info.RedirectURIs, parseLinkRedirectURIs(lh)...)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxClientDocBytes))
	if err != nil {
		return nil, errInvalidClient
	}

	mediaType := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))

	if mediaType == "application/json" {
		if err := applyJSONMetadata(info, id, baseURL, body); err != nil {
			return nil, err
		}
	} else {
		applyHTMLMicroformats(info, baseURL, id, body)
	}

	// redirect_uris を base に対して解決して absolute 化する。
	resolved := make([]string, 0, len(info.RedirectURIs))
	for _, ru := range info.RedirectURIs {
		if abs := resolveURL(baseURL, ru); abs != "" {
			resolved = append(resolved, abs)
		}
	}
	info.RedirectURIs = resolved
	if info.Name == "" {
		info.Name = id
	}
	info.Name = truncateRunes(info.Name, maxClientNameRunes)
	if len(info.Logo) > maxClientLogoBytes {
		info.Logo = ""
	}
	return info, nil
}

// truncateRunes returns s cut to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// applyJSONMetadata parses IndieAuth JSON client metadata into info, enforcing
// the client_id match and client_uri prefix rules.
func applyJSONMetadata(info *clientInfo, id, baseURL string, body []byte) error {
	var doc struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		ClientURI    string   `json:"client_uri"`
		LogoURI      string   `json:"logo_uri"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return errInvalidClient
	}
	// "client_id in the document MUST match the client_id URL."
	if doc.ClientID != id {
		return errInvalidClient
	}
	// "client_uri MUST be a prefix of client_id."
	if doc.ClientURI == "" || !strings.HasPrefix(id, doc.ClientURI) {
		return errInvalidClient
	}
	if doc.ClientName != "" {
		info.Name = doc.ClientName
	}
	if doc.LogoURI != "" {
		// logo_uri は相対の場合があるので document URL に対して解決する。
		info.Logo = resolveURL(baseURL, doc.LogoURI)
	}
	info.RedirectURIs = append(info.RedirectURIs, doc.RedirectURIs...)
	return nil
}

// applyHTMLMicroformats extracts redirect_uris (link[rel=redirect_uri]) and the
// h-app name/logo microformats from the client document. Best-effort: parse
// failures simply leave the defaults.
func applyHTMLMicroformats(info *clientInfo, baseURL, id string, body []byte) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return
	}
	// link[rel=redirect_uri][href]
	forEachNode(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "link" {
			if relMatches(attr(n, "rel"), "redirect_uri") {
				if href := attr(n, "href"); href != "" {
					info.RedirectURIs = append(info.RedirectURIs, href)
				}
			}
		}
	})
	// h-app microformat (first occurrence)。
	hApp := findClass(doc, "h-app")
	if hApp == nil {
		return
	}
	// .p-name: upstream は href/src が client_id に一致するときだけ name を採用。
	if nameEl := findClass(hApp, "p-name"); nameEl != nil {
		href := attr(nameEl, "href")
		if href == "" {
			href = attr(nameEl, "src")
		}
		if href != "" && resolveURL(baseURL, href) == normalizeURL(id) {
			if name := strings.TrimSpace(textContent(nameEl)); name != "" {
				info.Name = name
			}
		}
	}
	// .u-logo: href/src を base 解決。
	if logoEl := findClass(hApp, "u-logo"); logoEl != nil {
		href := attr(logoEl, "href")
		if href == "" {
			href = attr(logoEl, "src")
		}
		if href != "" {
			info.Logo = resolveURL(baseURL, href)
		}
	}
}

// --- HTML traversal helpers ---

func forEachNode(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		forEachNode(c, fn)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// relMatches reports whether a (space-separated) rel attribute contains token.
func relMatches(rel, token string) bool {
	for _, f := range strings.Fields(rel) {
		if strings.EqualFold(f, token) {
			return true
		}
	}
	return false
}

// classMatches reports whether a (space-separated) class attribute contains cls.
func classMatches(class, cls string) bool {
	for _, f := range strings.Fields(class) {
		if f == cls {
			return true
		}
	}
	return false
}

// findClass returns the first descendant (or self) element carrying the given
// class token.
func findClass(n *html.Node, cls string) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if found != nil {
			return
		}
		if node.Type == html.ElementNode && classMatches(attr(node, "class"), cls) {
			found = node
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}

func textContent(n *html.Node) string {
	var sb strings.Builder
	forEachNode(n, func(node *html.Node) {
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
		}
	})
	return sb.String()
}

// --- URL helpers ---

func resolveURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return b.ResolveReference(r).String()
}

func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.String()
}

// parseLinkRedirectURIs extracts the URIs of rel="redirect_uri" entries from a
// Link header value (RFC8288). Minimal parser handling `<uri>; rel="..."`.
func parseLinkRedirectURIs(header string) []string {
	var uris []string
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		lt := strings.IndexByte(part, '<')
		gt := strings.IndexByte(part, '>')
		if lt != 0 || gt < 0 {
			continue
		}
		uri := part[1:gt]
		params := part[gt+1:]
		hasRel := false
		for _, p := range strings.Split(params, ";") {
			p = strings.TrimSpace(p)
			if !strings.HasPrefix(strings.ToLower(p), "rel") {
				continue
			}
			val := strings.Trim(strings.TrimSpace(strings.TrimPrefix(p, "rel")), "=")
			val = strings.Trim(strings.TrimSpace(val), `"`)
			for _, t := range strings.Fields(val) {
				if t == "redirect_uri" {
					hasRel = true
				}
			}
		}
		if hasRel {
			uris = append(uris, uri)
		}
	}
	return uris
}
