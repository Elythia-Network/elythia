package mfm

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/htmlstream"
	"golang.org/x/net/html/atom"
)

// urlRegex は MFM でそのままリンク化される URL パターン。TS本家の urlRegexFull と同じ
// 文字の範囲で、`!` も含む (#3314)。以前は `!` が抜けていて、`!` を含む URL のリンクを
// `<...>` で囲んでいた。
var urlRegex = regexp.MustCompile(`^https?://[\w/:%#@$&?!()\[\]~.,=+\-]+$`)

// FromHTML converts HTML (typically from an ActivityPub note `content` field)
// into MFM markup. TS MfmService.fromHtml() と同等の変換を行う。
func FromHTML(s string) string {
	return fromHTML(s, false)
}

// FromHTMLWithoutQuoteInline is FromHTML for a note whose quote is attached:
// it first drops the top-level `.quote-inline` element that holds a link (the
// "RE: <url>" that Mastodon and others put before the body of a quote).
//
// 本家 2026.11.0 (#18063) の fromHtml の hasQuote と同じく、直下の要素のうち `<a>` を
// 含む最初の `.quote-inline` を 1 つだけ除く (#4164)。引用はノートに付くので、
// 本文に URL が残ると同じものが二重に出る。
func FromHTMLWithoutQuoteInline(s string) string {
	return fromHTML(s, true)
}

func fromHTML(s string, dropQuoteInline bool) string {
	if s == "" {
		return ""
	}
	// Pixelfed等で <br>\n が同時に使われるケースを正規化する (TS本家と同じ)
	s = strings.ReplaceAll(s, "<br>\n", "\n")
	s = strings.ReplaceAll(s, "<br/>\n", "\n")
	s = strings.ReplaceAll(s, "<br />\n", "\n")

	doc, ok := readNodes(s)
	if !ok {
		return s
	}

	if dropQuoteInline {
		removeQuoteInline(doc)
	}

	var b strings.Builder
	walkChildren(doc, &b)
	return strings.TrimSpace(b.String())
}

// node is an element, a text or a comment of the converted HTML. Only what the
// conversion looks at is kept.
type node struct {
	el   *htmlstream.Element // nil for text and comments
	text string
	// comment は変換に出ないが、<pre> の中で <code> の後ろに何かあるかの判定に要る。
	comment bool

	parent, firstChild, lastChild, nextSibling *node
}

func (n *node) appendChild(c *node) {
	c.parent = n
	if n.lastChild == nil {
		n.firstChild = c
	} else {
		n.lastChild.nextSibling = c
	}
	n.lastChild = c
}

func (n *node) isElement(a atom.Atom) bool { return n.el != nil && n.el.Atom == a }

// readNodes reads s with htmlstream, the way x/net/html nests the elements.
// It reports false where x/net/html's Parse returned an error (the caller then
// keeps s as it is, as before).
//
// x/net/html の木の構築 (html.Parse) は使わない。htmlstream は入力の長さに比例する
// 時間とメモリで読む。
func readNodes(s string) (*node, bool) {
	sc := htmlstream.NewScanner(strings.NewReader(s))
	doc := &node{}
	cur := doc
	for {
		switch sc.Next() {
		case htmlstream.EOF:
			return doc, sc.Err() == nil
		case htmlstream.StartElement:
			n := &node{el: sc.Element()}
			cur.appendChild(n)
			cur = n
		case htmlstream.EndElement:
			if cur.parent != nil {
				cur = cur.parent
			}
		case htmlstream.Text:
			// x/net/html は隣り合う文字を 1 つのノードにまとめるが、変換は文字を
			// 順につなぐだけなので、まとめなくても結果は同じになる。
			cur.appendChild(&node{text: sc.Text()})
		case htmlstream.Comment:
			cur.appendChild(&node{comment: true})
		}
	}
}

// removeQuoteInline removes the first child of the body that has the class
// `quote-inline` and contains an <a>.
func removeQuoteInline(doc *node) {
	var prev *node
	for c := doc.firstChild; c != nil; prev, c = c, c.nextSibling {
		if c.el == nil || c.el.InHead || !hasClass(c, "quote-inline") || findElement(c, atom.A) == nil {
			continue
		}
		if prev == nil {
			doc.firstChild = c.nextSibling
		} else {
			prev.nextSibling = c.nextSibling
		}
		if doc.lastChild == c {
			doc.lastChild = prev
		}
		return
	}
}

// findElement returns the first element of type a in n or its descendants.
func findElement(n *node, a atom.Atom) *node {
	if n.isElement(a) {
		return n
	}
	for c := n.firstChild; c != nil; c = c.nextSibling {
		if found := findElement(c, a); found != nil {
			return found
		}
	}
	return nil
}

// hasClass reports whether the class attribute of n holds the token class.
func hasClass(n *node, class string) bool {
	for _, attr := range n.el.Attr {
		if attr.Namespace == "" && attr.Key == "class" && slices.Contains(strings.Fields(attr.Val), class) {
			return true
		}
	}
	return false
}

// walkChildren は子ノードを順に処理する。
func walkChildren(n *node, b *strings.Builder) {
	for c := n.firstChild; c != nil; c = c.nextSibling {
		analyze(c, b)
	}
}

// getText はノード配下のテキストを収集する。
// TS の getText と同等。<br> は改行に変換する。
func getText(n *node) string {
	var b strings.Builder
	writeText(n, &b)
	return b.String()
}

// writeText は getText の中身。入れ子の段ごとに文字列を作り直さないよう、
// 1 つの Builder に書く。
func writeText(n *node, b *strings.Builder) {
	if n.comment {
		return
	}
	if n.el == nil {
		b.WriteString(n.text)
		return
	}
	if n.el.Atom == atom.Br {
		b.WriteString("\n")
		return
	}
	for c := n.firstChild; c != nil; c = c.nextSibling {
		writeText(c, b)
	}
}

// analyze は1つのノードを MFM に変換する。TS の analyze と同等。
func analyze(n *node, b *strings.Builder) {
	if n.comment {
		return
	}
	if n.el == nil {
		b.WriteString(n.text)
		return
	}

	switch n.el.Atom {
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
func analyzeAnchor(n *node, b *strings.Builder) {
	txt := getText(n)
	href, _ := n.el.AttrVal("href")
	rel, _ := n.el.AttrVal("rel")

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
func analyzePre(n *node, b *strings.Builder) {
	// <pre> の直下に <code> が1つだけある場合はコードブロック
	child := firstElementChild(n)
	if child != nil && child.isElement(atom.Code) && child.nextSibling == nil {
		b.WriteString("\n```\n")
		b.WriteString(getText(child))
		b.WriteString("\n```\n")
		return
	}
	walkChildren(n, b)
}

// firstElementChild は最初の Element 子ノードを返す。
func firstElementChild(n *node) *node {
	for c := n.firstChild; c != nil; c = c.nextSibling {
		if c.el != nil {
			return c
		}
	}
	return nil
}
