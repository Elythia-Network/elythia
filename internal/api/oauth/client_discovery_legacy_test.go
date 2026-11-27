package oauth

// This file keeps applyHTMLMicroformats as it was before it stopped using
// x/net/html's tree (html.Parse), to write testdata/pages.golden.json (go test
// -run TestApplyHTMLMicroformats_MatchGolden -update-golden). Tests only.

import (
	"strings"

	"golang.org/x/net/html"
)

func legacyApplyHTMLMicroformats(info *clientInfo, baseURL, id string, body []byte) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return
	}
	// link[rel=redirect_uri][href]
	legacyForEachNode(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "link" {
			if relMatches(legacyAttr(n, "rel"), "redirect_uri") {
				if href := legacyAttr(n, "href"); href != "" {
					info.RedirectURIs = append(info.RedirectURIs, href)
				}
			}
		}
	})
	// h-app microformat (first occurrence)。
	hApp := legacyFindClass(doc, "h-app")
	if hApp == nil {
		return
	}
	// .p-name: upstream は href/src が client_id に一致するときだけ name を採用。
	if nameEl := legacyFindClass(hApp, "p-name"); nameEl != nil {
		href := legacyAttr(nameEl, "href")
		if href == "" {
			href = legacyAttr(nameEl, "src")
		}
		if href != "" && resolveURL(baseURL, href) == normalizeURL(id) {
			if name := strings.TrimSpace(legacyTextContent(nameEl)); name != "" {
				info.Name = name
			}
		}
	}
	// .u-logo: href/src を base 解決。
	if logoEl := legacyFindClass(hApp, "u-logo"); logoEl != nil {
		href := legacyAttr(logoEl, "href")
		if href == "" {
			href = legacyAttr(logoEl, "src")
		}
		if href != "" {
			info.Logo = resolveURL(baseURL, href)
		}
	}
}

func legacyForEachNode(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		legacyForEachNode(c, fn)
	}
}

func legacyAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// legacyFindClass returns the first descendant (or self) element carrying the given
// class token.
func legacyFindClass(n *html.Node, cls string) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if found != nil {
			return
		}
		if node.Type == html.ElementNode && classMatches(legacyAttr(node, "class"), cls) {
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

func legacyTextContent(n *html.Node) string {
	var sb strings.Builder
	legacyForEachNode(n, func(node *html.Node) {
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
		}
	})
	return sb.String()
}
