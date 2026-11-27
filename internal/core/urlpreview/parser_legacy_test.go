package urlpreview

// This file keeps extractMeta and extractIframe as they were before they
// stopped using x/net/html's tree (html.Parse), to write
// testdata/pages.golden.json (go test -run TestReadPages_MatchGolden
// -update-golden). Tests only.

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

func legacyParseMeta(page []byte) (metaMap, bool) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, false
	}
	return legacyExtractMeta(doc), true
}

func legacyExtractMeta(n *html.Node) metaMap {
	m := make(metaMap)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "meta":
				key, val := legacyMetaKeyVal(n)
				if key != "" && val != "" {
					m[key] = val
				}
			case "title":
				if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					m["title"] = strings.TrimSpace(n.FirstChild.Data)
				}
			case "link":
				rel, href := legacyAttrVal(n, "rel"), legacyAttrVal(n, "href")
				if (rel == "icon" || rel == "shortcut icon") && href != "" {
					m["icon"] = href
				}
				if rel == "alternate" && strings.Contains(legacyAttrVal(n, "type"), "activity+json") && href != "" {
					m["ap:id"] = href
				}
				if rel == "alternate" && href != "" && legacyAttrVal(n, "type") == "application/json+oembed" {
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

func legacyMetaKeyVal(n *html.Node) (string, string) {
	name := legacyAttrVal(n, "property")
	if name == "" {
		name = legacyAttrVal(n, "name")
	}
	content := legacyAttrVal(n, "content")
	return strings.ToLower(name), content
}

func legacyAttrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func legacyExtractIframe(snippet string) (string, []string) {
	doc, err := html.Parse(strings.NewReader(snippet))
	if err != nil {
		return "", []string{}
	}
	var src string
	var allow []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if src != "" {
			return // first iframe only
		}
		if n.Type == html.ElementNode && n.Data == "iframe" {
			src = legacyAttrVal(n, "src")
			allow = filterAllow(legacyAttrVal(n, "allow"))
			if legacyHasAttr(n, "allowfullscreen") {
				allow = appendUnique(allow, "fullscreen")
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if allow == nil {
		allow = []string{}
	}
	return src, allow
}

func legacyHasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}
