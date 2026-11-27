package i

// This file keeps the rel=me check as it was before it stopped using
// x/net/html's tree (html.Parse), to write testdata/pages.golden.json (go test
// -run TestHTMLLinksBackTo_MatchGolden -update-golden). Tests only.

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

func legacyLinksBackTo(page []byte, myLink string) bool {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return false
	}
	return legacyHTMLLinksBackTo(doc, myLink)
}

func legacyHTMLLinksBackTo(doc *html.Node, myLink string) bool {
	found := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode && (n.Data == "a" || n.Data == "link") {
			var href, rel string
			for _, attr := range n.Attr {
				switch attr.Key {
				case "href":
					href = attr.Val
				case "rel":
					rel = attr.Val
				}
			}
			if href == myLink {
				// upstream includesMyLink: 任意の <a href=myLink> (rel 不問)。
				if n.Data == "a" {
					found = true
					return
				}
				// upstream includesRelMeLinks: <link> は rel に "me" を含むとき。
				for _, token := range strings.Fields(rel) {
					if token == "me" {
						found = true
						return
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}
