package urlpreview

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// ParseHTML extracts OGP and Twitter card metadata from HTML content.
// 本家 Misskey の summaly と同じ優先順位: og:* > twitter:* > <title>/<link>。
//
// pageURL は base として使われ、href / og:image / icon / og:url 等の相対
// パスは絶対 URL に解決される (#639)。oEmbed 用 alternate link は meta
// map の `oembed:json` / `oembed:xml` キーに格納され、Fetcher 側で 2nd
// fetch のターゲットにする。
func ParseHTML(r io.Reader, pageURL string) *Result {
	doc, err := html.Parse(r)
	if err != nil {
		return &Result{URL: pageURL, Player: PlayerResult{Allow: []string{}}}
	}
	base, _ := url.Parse(pageURL) // base は err でも nil なら resolveURL no-op

	meta := extractMeta(doc)

	// **取得元が決める文字列は長さを切る。** ここに来る値は攻撃者のページが
	// 自由に決められ、結果は 24 時間 Redis に載る (fetcher.go)。上限が無いと
	// `urlPreviewMaximumContentLength` (既定 10MiB) いっぱいの title を
	// そのまま保存でき、クエリ文字列を変えれば URL は無限に作れる。
	// 値は upstream (summaly の `clip(title, 100)` / `clip(description, 300)`)
	// に合わせる。
	title := clipRunes(firstNonEmpty(meta["og:title"], meta["twitter:title"], meta["title"]), maxTitleRunes)
	desc := clipRunes(firstNonEmpty(meta["og:description"], meta["twitter:description"], meta["description"]), maxDescriptionRunes)
	thumb := resolveURL(base, firstNonEmpty(meta["og:image"], meta["twitter:image"], meta["twitter:image:src"]))
	icon := resolveURL(base, meta["icon"])
	sitename := clipRunes(firstNonEmpty(meta["og:site_name"]), maxSitenameRunes)
	apID := resolveURL(base, firstNonEmpty(meta["ap:id"]))

	canonical := resolveURL(base, meta["og:url"])
	// #2106 L45: upstream は summary.url が http(s) でなければ throw する。canonical (og:url) が
	// javascript:/data: 等の危険な scheme の場合、API レスポンスの url field に素通ししないよう
	// 安全な pageURL にフォールバックする (defense-in-depth)。
	if canonical == "" || !isHTTPOrHTTPSURL(canonical) {
		canonical = pageURL
	}
	result := &Result{
		URL:    canonical,
		Player: PlayerResult{Allow: []string{}},
	}
	if title != "" {
		result.Title = &title
	}
	if desc != "" {
		result.Description = &desc
	}
	if thumb != "" {
		result.Thumbnail = &thumb
	}
	if icon != "" {
		result.Icon = &icon
	}
	if sitename != "" {
		result.Sitename = &sitename
	}
	if apID != "" {
		result.ActivityPub = &apID
	}
	// oEmbed discovery URL は meta から取り出して Result の補助 field に
	// 渡し、Fetcher が 2nd request でフェッチする。preview.go 側で公開し
	// ない (内部用)。
	result.oEmbedURL = resolveURL(base, meta["oembed:json"])
	return result
}

// resolveURL turns rel into an absolute URL relative to base.
//   - Empty rel → "" (caller handles no-icon / no-thumbnail branch)
//   - nil base → return rel as-is (caller is responsible for already-absolute)
//   - parse failure → "" so frontend doesn't try to render a malformed URL
//     (review #3)
func resolveURL(base *url.URL, rel string) string {
	if rel == "" {
		return ""
	}
	if base == nil {
		return clipURL(rel)
	}
	r, err := url.Parse(rel)
	if err != nil {
		return ""
	}
	return clipURL(base.ResolveReference(r).String())
}

// clipURL drops a URL that exceeds maxURLBytes.
//
// **切らずに落とす。** 途中で切った URL は別の場所を指しうるので、短くして
// 保存するより出さないほうがよい (呼び出し側はどれも「空なら出さない」形)。
func clipURL(u string) string {
	if len(u) > maxURLBytes {
		return ""
	}
	return u
}

type metaMap map[string]string

func extractMeta(n *html.Node) metaMap {
	m := make(metaMap)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "meta":
				key, val := metaKeyVal(n)
				if key != "" && val != "" {
					m[key] = val
				}
			case "title":
				if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					m["title"] = strings.TrimSpace(n.FirstChild.Data)
				}
			case "link":
				rel, href := attrVal(n, "rel"), attrVal(n, "href")
				if (rel == "icon" || rel == "shortcut icon") && href != "" {
					m["icon"] = href
				}
				// alternate + application/activity+json → AP ID
				if rel == "alternate" && strings.Contains(attrVal(n, "type"), "activity+json") && href != "" {
					m["ap:id"] = href
				}
				// oEmbed discovery (#639)。JSON のみサポート: XML は
				// 仕様上有効だが現状 unmarshaler が無く fallback parser
				// 整備するまで無視 (review #1)。
				if rel == "alternate" && href != "" && attrVal(n, "type") == "application/json+oembed" {
					m["oembed:json"] = href
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return m
}

func metaKeyVal(n *html.Node) (string, string) {
	name := attrVal(n, "property")
	if name == "" {
		name = attrVal(n, "name")
	}
	content := attrVal(n, "content")
	return strings.ToLower(name), content
}

func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Upstream (summaly) の clip と同じ上限。
const (
	maxTitleRunes       = 100
	maxDescriptionRunes = 300
	// sitename は upstream が clip しないが、同じ経路で任意長を受け取れるので
	// 揃えて切る (意図的な乖離。`docs/divergence/operations.md` の URL preview の行)。
	maxSitenameRunes = 100
	// maxURLBytes bounds the URL-ish fields (`url` / `thumbnail` / `icon` /
	// `activityPub`)。
	//
	// **これらも取得元が決める文字列。** upstream (summaly) は clip しないが、
	// upstream は 1 時間 / 100 件のメモリキャッシュなのに対し mk-go は
	// **24 時間 Redis に載せ**、クエリ文字列を変えれば URL は無限に作れる。
	// その前提の差が title / description を切っている理由そのものなので、
	// 同じ entry に載る URL 側も揃える (実測: 上限が無いと 1 本あたり
	// `urlPreviewMaximumContentLength` (既定 10MiB) までリニアに伸びる)。
	//
	// 2048 は実運用の URL 長の上限として広く使われる値。正当な og:image が
	// ここに掛かることはまず無い。
	maxURLBytes = 2048
)

// clipRunes truncates s to at most n runes, appending an ellipsis when it did.
//
// **rune 単位で切る。** バイトで切ると UTF-8 の途中で分断して不正な文字列に
// なる。upstream の `clip` は JS の `substr` (UTF-16 コードユニット) なので
// 絵文字の扱いが厳密には違うが、どちらも「壊れた文字を作らない」点は同じ。
func clipRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
