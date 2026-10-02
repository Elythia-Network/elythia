package mfm

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// urlRegex は MFM でそのままリンク化される URL パターン。TS本家の urlRegexFull と同じ
// 文字の範囲で、`!` も含む (#3314)。以前は `!` が抜けていて、`!` を含む URL のリンクを
// `<...>` で囲んでいた。
var urlRegex = regexp.MustCompile(`^https?://[\w/:%#@$&?!()\[\]~.,=+\-]+$`)

// FromHTML converts HTML (typically from an ActivityPub note `content` field)
// into MFM markup. TS MfmService.fromHtml() と同等の変換を行う。
func FromHTML(s string) string {
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

	var b strings.Builder
	walkChildren(doc, &b)
	return strings.TrimSpace(b.String())
}

// walkChildren は子ノードを順に処理する。
func walkChildren(n *html.Node, b *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		analyze(c, b)
	}
}

// getText はノード配下のテキストを再帰的に収集する。
// TS の getText と同等。<br> は改行に変換する。
func getText(n *html.Node) string {
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
		b.WriteString(getText(c))
	}
	return b.String()
}

// analyze は1つのノードを MFM に変換する。TS の analyze と同等。
func analyze(n *html.Node, b *strings.Builder) {
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
		analyzeAnchor(n, b)

	case atom.H1:
		b.WriteString("【")
		walkChildren(n, b)
		b.WriteString("】\n")

	case atom.B, atom.Strong:
		b.WriteString("**")
		walkChildren(n, b)
		b.WriteString("**")

	case atom.Small:
		b.WriteString("<small>")
		walkChildren(n, b)
		b.WriteString("</small>")

	case atom.S, atom.Del:
		b.WriteString("~~")
		walkChildren(n, b)
		b.WriteString("~~")

	case atom.I, atom.Em:
		b.WriteString("<i>")
		walkChildren(n, b)
		b.WriteString("</i>")

	case atom.Pre:
		analyzePre(n, b)

	case atom.Code:
		b.WriteString("`")
		walkChildren(n, b)
		b.WriteString("`")

	case atom.Blockquote:
		t := getText(n)
		if t != "" {
			b.WriteString("\n> ")
			b.WriteString(strings.ReplaceAll(t, "\n", "\n> "))
		}

	// <p>, <h2>..<h6>: 段落区切り
	case atom.P, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		b.WriteString("\n\n")
		walkChildren(n, b)

	// ブロック要素: 改行区切り
	case atom.Div, atom.Header, atom.Footer, atom.Article, atom.Li, atom.Dt, atom.Dd:
		b.WriteString("\n")
		walkChildren(n, b)

	default:
		// span, ul, ol, その他インライン要素: 子ノードをそのまま処理
		walkChildren(n, b)
	}
}

// analyzeAnchor は <a> を MFM に変換する。
// メンション、ハッシュタグ、通常リンクを識別する。
func analyzeAnchor(n *html.Node, b *strings.Builder) {
	txt := getText(n)
	href := getAttr(n, "href")
	rel := getAttr(n, "rel")

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

// analyzePre は <pre> を処理する。<pre><code>...</code></pre> の場合は
// コードブロックに変換する。
func analyzePre(n *html.Node, b *strings.Builder) {
	// <pre> の直下に <code> が1つだけある場合はコードブロック
	child := firstElementChild(n)
	if child != nil && child.DataAtom == atom.Code && child.NextSibling == nil {
		b.WriteString("\n```\n")
		b.WriteString(getText(child))
		b.WriteString("\n```\n")
		return
	}
	walkChildren(n, b)
}

// getAttr はHTML要素から指定属性の値を返す。
func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// firstElementChild は最初の Element 子ノードを返す。
func firstElementChild(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}
