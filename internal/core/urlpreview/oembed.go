package urlpreview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/htmlstream"
)

// oEmbed JSON shape (subset). The spec defines several types (rich, video,
// photo, link); we only care about ones that embed an iframe (rich, video).
//
// 仕様: https://oembed.com/
type oembedResponse struct {
	Type   string `json:"type"`   // "rich" | "video" | "photo" | "link"
	HTML   string `json:"html"`   // typically `<iframe src="..."></iframe>`
	Width  any    `json:"width"`  // number or string in the wild
	Height any    `json:"height"` // number or string in the wild
}

// playerAllowlist は oEmbed iframe の `allow` 属性で許可する feature policy
// directive。任意属性を通すと XSS / privacy リスクが拡がるので Misskey TS
// と同じく主要 directive のみ allowlist する。
var playerAllowlist = map[string]struct{}{
	"autoplay":           {},
	"clipboard-write":    {},
	"encrypted-media":    {},
	"fullscreen":         {},
	"picture-in-picture": {},
	"web-share":          {},
	"accelerometer":      {},
	"gyroscope":          {},
}

// fetchOEmbed retrieves the oEmbed JSON from oembedURL and extracts iframe
// information into a PlayerResult. Errors are returned to the caller which
// MAY drop them — preview fetching must not fail just because oEmbed is
// unreachable.
//
// Redirect handling: this method shares the Fetcher's primary client, so
// redirects follow the same `cfg.AllowRedirect` policy as the HTML body
// fetch. Provider redirects (e.g. youtube.com/oembed → youtu.be variants)
// are intentional in real-world traffic; if AllowRedirect is false the
// 2nd hop is rejected and we silently fall back to "no player" preview.
// SSRF guard (#638-B1) is applied on every dial including redirect hops,
// so attacker-controlled oEmbed URLs cannot pivot to private IPs.
func (f *Fetcher) fetchOEmbed(ctx context.Context, oembedURL string) (PlayerResult, error) {
	zero := PlayerResult{Allow: []string{}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oembedURL, nil)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	ua := f.cfg.UserAgent
	if ua == "" {
		ua = "Misskey URL Preview"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, fmt.Errorf("%w: status %d", ErrFetchFailed, resp.StatusCode)
	}
	// oEmbed JSON はサイズ的に小さい (< 4 KiB が多い) が念のため body 上限を
	// 適用 (preview fetch と同じ MaxContentLength)。
	limit := f.cfg.MaxContentLength
	if limit <= 0 {
		limit = 1 << 20 // 1 MiB safety default
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return zero, fmt.Errorf("%w: read: %v", ErrFetchFailed, err)
	}
	var doc oembedResponse
	if err := json.Unmarshal(data, &doc); err != nil {
		// Provider が Accept: application/json を無視して HTML/XML を返す
		// ケースが現実にある (text/xml+oembed 経路は #639 review #1 で
		// drop)。observability のため Content-Type を debug ログに残す。
		slog.Debug("urlpreview: oembed json parse failed",
			"url", oembedURL,
			"contentType", resp.Header.Get("Content-Type"),
			"err", err)
		return zero, fmt.Errorf("%w: parse: %v", ErrFetchFailed, err)
	}
	return parseOEmbedPlayer(doc), nil
}

// parseOEmbedPlayer extracts the iframe src + size + sandbox-safe `allow`
// attributes from an oEmbed JSON. Returns the zero (Allow=[]) PlayerResult
// when the document is not iframe-shaped.
func parseOEmbedPlayer(doc oembedResponse) PlayerResult {
	zero := PlayerResult{Allow: []string{}}
	if doc.Type != "rich" && doc.Type != "video" {
		return zero
	}
	if doc.HTML == "" {
		return zero
	}
	src, allow := extractIframe(doc.HTML)
	if src == "" {
		return zero
	}
	if !isHTTPSURL(src) {
		// Embedded player must be HTTPS to avoid mixed-content / downgrade
		// attacks. http:// embeds get dropped silently.
		return zero
	}
	w := numAttr(doc.Width)
	h := numAttr(doc.Height)
	pr := PlayerResult{URL: &src, Allow: allow}
	if w > 0 {
		pr.Width = &w
	}
	if h > 0 {
		pr.Height = &h
	}
	return pr
}

// extractIframe walks the HTML snippet returned by oEmbed and returns the
// first <iframe>'s src and a sandbox-safe slice of `allow` directives.
//
// allow attribute は ";" 区切り (Permission Policy 形式) で来る。許可
// directive は playerAllowlist で絞り込む。allowfullscreen 単体属性も
// "fullscreen" directive 等価として扱う。
func extractIframe(snippet string) (string, []string) {
	var src string
	var allow []string
	sc := htmlstream.NewScanner(strings.NewReader(snippet))
	for {
		k := sc.Next()
		if k == htmlstream.EOF {
			break
		}
		if src != "" || k != htmlstream.StartElement || sc.Element().Name != "iframe" {
			// src のある最初の iframe を使う。解析に失敗したら何も返さないので
			// (木を作っていたときと同じ)、見つけた後も最後まで読む。
			continue
		}
		n := sc.Element()
		src = attrVal(n, "src")
		allow = filterAllow(attrVal(n, "allow"))
		// allowfullscreen 単体属性も許可フラグとして扱う
		if n.HasAttr("allowfullscreen") {
			allow = appendUnique(allow, "fullscreen")
		}
	}
	if sc.Err() != nil {
		return "", []string{}
	}
	if allow == nil {
		allow = []string{}
	}
	return src, allow
}

// filterAllow splits the iframe's `allow` attribute (Permissions Policy
// format) and keeps only allowlisted directives.
func filterAllow(raw string) []string {
	if raw == "" {
		return []string{}
	}
	out := []string{}
	for _, part := range strings.Split(raw, ";") {
		// 各 entry は "directive" or "directive value-list" の形。
		// Misskey TS は directive 名のみを通す。
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		name := strings.ToLower(fields[0])
		if _, ok := playerAllowlist[name]; ok {
			out = appendUnique(out, name)
		}
	}
	return out
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// isHTTPSURL は scheme://host... 形式を string match で判定 (url.Parse の
// 全 path validation は不要、scheme のみ確認できれば十分)。
func isHTTPSURL(u string) bool {
	return strings.HasPrefix(strings.ToLower(u), "https://")
}

// isHTTPOrHTTPSURL は http:// または https:// scheme を string match で判定する
// (#2106 L45: canonical url の scheme 検証用)。
func isHTTPOrHTTPSURL(u string) bool {
	lu := strings.ToLower(u)
	return strings.HasPrefix(lu, "http://") || strings.HasPrefix(lu, "https://")
}

// numAttr coerces an oEmbed width/height field (which can be number or
// string in the wild — providers are inconsistent) to int. 0 on failure.
func numAttr(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		// best-effort parse; "640" → 640, "640px" → 640
		n := 0
		for _, r := range x {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		return n
	}
	return 0
}
