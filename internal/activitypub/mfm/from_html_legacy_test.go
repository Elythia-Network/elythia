package mfm

// This file keeps the conversion as it was before FromHTML stopped using
// x/net/html's tree (html.Parse), to write testdata/fromhtml/*.golden.json
// (go test -run TestFromHTML_MatchesGolden -update-golden). Tests only.

import (
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// legacyFromHTMLPublic is FromHTML before the change.
func legacyFromHTMLPublic(s string) string {
	return legacyFromHTML(s, false)
}

// legacyFromHTMLWithoutQuoteInline is FromHTMLWithoutQuoteInline before the change.
func legacyFromHTMLWithoutQuoteInline(s string) string {
	return legacyFromHTML(s, true)
}

func legacyFromHTML(s string, dropQuoteInline bool) string {
	if s == "" {
		return ""
	}
	// Pixelfed等で <br>\n が同時に使われるケースを正規化する (TS本家と同じ)
	s = strings.ReplaceAll(s, "<br>\n", "\n")
	s = strings.ReplaceAll(s, "<br/>\n", "\n")
	s = strings.ReplaceAll(s, "<br />\n", "\n")

	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}

	if dropQuoteInline {
		legacyRemoveQuoteInline(doc)
	}

	var b strings.Builder
	legacyWalkChildren(doc, &b)
	return strings.TrimSpace(b.String())
}

// legacyRemoveQuoteInline removes the first child of <body> that has the class
// `quote-inline` and contains an <a>.
func legacyRemoveQuoteInline(doc *html.Node) {
	body := legacyFindElement(doc, atom.Body)
	if body == nil {
		return
	}
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && legacyHasClass(c, "quote-inline") && legacyFindElement(c, atom.A) != nil {
			body.RemoveChild(c)
			return
		}
	}
}

// legacyFindElement returns the first element of type a in n or its descendants.
func legacyFindElement(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := legacyFindElement(c, a); found != nil {
			return found
		}
	}
	return nil
}

// legacyHasClass reports whether the class attribute of n holds the token class.
func legacyHasClass(n *html.Node, class string) bool {
	for _, attr := range n.Attr {
		if attr.Namespace == "" && attr.Key == "class" && slices.Contains(strings.Fields(attr.Val), class) {
			return true
		}
	}
	return false
}

// legacyWalkChildren は子ノードを順に処理する。
func legacyWalkChildren(n *html.Node, b *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		legacyAnalyze(c, b)
	}
}

// legacyGetText はノード配下のテキストを再帰的に収集する。
// TS の getText と同等。<br> は改行に変換する。
func legacyGetText(n *html.Node) string {
	if n == nil {
		return ""
	}
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type == html.ElementNode && (n.DataAtom == atom.Br) {
		return "\n"
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(legacyGetText(c))
	}
	return b.String()
}

// legacyAnalyze は1つのノードを MFM に変換する。TS の analyze と同等。
func legacyAnalyze(n *html.Node, b *strings.Builder) {
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
		return
	}
	if n.Type != html.ElementNode {
		return
	}

	switch n.DataAtom {
	case atom.Br:
		b.WriteString("\n")

	case atom.A:
		legacyAnalyzeAnchor(n, b)

	case atom.H1:
		b.WriteString("【")
		legacyWalkChildren(n, b)
		b.WriteString("】\n")

	case atom.B, atom.Strong:
		b.WriteString("**")
		legacyWalkChildren(n, b)
		b.WriteString("**")

	case atom.Small:
		b.WriteString("<small>")
		legacyWalkChildren(n, b)
		b.WriteString("</small>")

	case atom.S, atom.Del:
		b.WriteString("~~")
		legacyWalkChildren(n, b)
		b.WriteString("~~")

	case atom.I, atom.Em:
		b.WriteString("<i>")
		legacyWalkChildren(n, b)
		b.WriteString("</i>")

	case atom.Pre:
		legacyAnalyzePre(n, b)

	case atom.Code:
		b.WriteString("`")
		legacyWalkChildren(n, b)
		b.WriteString("`")

	case atom.Blockquote:
		t := legacyGetText(n)
		if t != "" {
			b.WriteString("\n> ")
			b.WriteString(strings.ReplaceAll(t, "\n", "\n> "))
		}

	// <p>, <h2>..<h6>: 段落区切り
	case atom.P, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		b.WriteString("\n\n")
		legacyWalkChildren(n, b)

	// ブロック要素: 改行区切り
	case atom.Div, atom.Header, atom.Footer, atom.Article, atom.Li, atom.Dt, atom.Dd:
		b.WriteString("\n")
		legacyWalkChildren(n, b)

	default:
		// span, ul, ol, その他インライン要素: 子ノードをそのまま処理
		legacyWalkChildren(n, b)
	}
}

// legacyAnalyzeAnchor は <a> を MFM に変換する。
// メンション、ハッシュタグ、通常リンクを識別する。
func legacyAnalyzeAnchor(n *html.Node, b *strings.Builder) {
	txt := legacyGetText(n)
	href := legacyGetAttr(n, "href")
	rel := legacyGetAttr(n, "rel")

	// メンション: @で始まるテキスト (ただし rel="me ..." は除外)
	if strings.HasPrefix(txt, "@") && !(rel != "" && strings.HasPrefix(rel, "me ")) {
		parts := strings.Split(txt, "@")
		if len(parts) == 2 && href != "" {
			// @user → @user@hostname (ホスト名部分が省略されているので復元)
			if u, err := url.Parse(href); err == nil {
				b.WriteString(txt + "@" + u.Hostname())
			} else {
				b.WriteString(txt)
			}
		} else if len(parts) == 3 {
			// @user@host の形式: そのまま
			b.WriteString(txt)
		}
		return
	}

	// ハッシュタグ: #で始まるテキスト
	if strings.HasPrefix(txt, "#") {
		b.WriteString(txt)
		return
	}

	// 通常リンク
	if href == "" && txt == "" {
		return
	}
	if href == "" {
		b.WriteString(txt)
		return
	}
	if txt == "" || txt == href {
		if urlRegex.MatchString(href) {
			b.WriteString(href)
		} else {
			b.WriteString("<" + href + ">")
		}
		return
	}
	// テキストとURLが異なる場合。TS本家は urlRegex (部分一致) にマッチし
	// かつ urlRegexFull (完全一致) にマッチしない場合に角括弧を付ける。
	// **本家とは意図的に違えている。** 本家の部分一致は scheme の直後に URL の文字が
	// 1 文字以上あることを求めるので、`https://日本` は角括弧が付かず
	// `[c](https://日本)` になり、mfm-js で読むとリンクにならず文字のまま残る。
	// こちらは http(s):// で始まれば完全一致しないものを全部角括弧で囲み、
	// `[c](<https://日本>)` としてリンクを保つ。
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		if urlRegex.MatchString(href) {
			b.WriteString("[" + txt + "](" + href + ")")
		} else {
			b.WriteString("[" + txt + "](<" + href + ">)")
		}
	} else {
		b.WriteString("[" + txt + "](" + href + ")")
	}
}

// legacyAnalyzePre は <pre> を処理する。<pre><code>...</code></pre> の場合は
// コードブロックに変換する。
func legacyAnalyzePre(n *html.Node, b *strings.Builder) {
	// <pre> の直下に <code> が1つだけある場合はコードブロック
	child := legacyFirstElementChild(n)
	if child != nil && child.DataAtom == atom.Code && child.NextSibling == nil {
		b.WriteString("\n```\n")
		b.WriteString(legacyGetText(child))
		b.WriteString("\n```\n")
		return
	}
	legacyWalkChildren(n, b)
}

// legacyGetAttr はHTML要素から指定属性の値を返す。
func legacyGetAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// legacyFirstElementChild は最初の Element 子ノードを返す。
func legacyFirstElementChild(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}
