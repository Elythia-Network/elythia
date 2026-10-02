package mfm

import (
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ToHTML converts MFM nodes to an HTML string.
// host はローカルホスト名 (例: "example.com")。
// メンションやハッシュタグのリンク先URLの生成に使う。
func ToHTML(nodes []*Node, host string) string {
	if len(nodes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range nodes {
		renderNode(&b, n, host)
	}
	return b.String()
}

// IsSimple reports whether the AST contains only "standard" node types
// that don't require _misskey_content / source fields.
// TS版の noMisskeyContent ロジックに対応: text, unicodeEmoji, emojiCode,
// mention, hashtag, url のみの場合に true を返す。
func IsSimple(nodes []*Node) bool {
	for _, n := range nodes {
		if !isSimpleNode(n) {
			return false
		}
	}
	return true
}

func isSimpleNode(n *Node) bool {
	switch n.Type {
	case NodeText, NodeUnicodeEmoji, NodeEmojiCode, NodeMention, NodeHashtag, NodeURL:
		return true
	default:
		return false
	}
}

func renderNode(b *strings.Builder, n *Node, host string) {
	switch n.Type {
	case NodeText:
		renderText(b, n.textValue())
	case NodeBold:
		b.WriteString("<b>")
		renderChildren(b, n.Children, host)
		b.WriteString("</b>")
	case NodeItalic:
		b.WriteString("<i>")
		renderChildren(b, n.Children, host)
		b.WriteString("</i>")
	case NodeStrike:
		b.WriteString("<del>")
		renderChildren(b, n.Children, host)
		b.WriteString("</del>")
	case NodeSmall:
		b.WriteString("<small>")
		renderChildren(b, n.Children, host)
		b.WriteString("</small>")
	case NodeCenter:
		b.WriteString(`<div style="text-align: center;">`)
		renderChildren(b, n.Children, host)
		b.WriteString("</div>")
	case NodePlain:
		renderChildren(b, n.Children, host)
	case NodeInlineCode:
		code, _ := n.Props["code"].(string)
		b.WriteString("<code>")
		b.WriteString(html.EscapeString(code))
		b.WriteString("</code>")
	case NodeBlockCode:
		code, _ := n.Props["code"].(string)
		b.WriteString("<pre><code>")
		b.WriteString(html.EscapeString(code))
		b.WriteString("</code></pre>")
	case NodeMathInline:
		formula, _ := n.Props["formula"].(string)
		b.WriteString("<code>")
		b.WriteString(html.EscapeString(formula))
		b.WriteString("</code>")
	case NodeMathBlock:
		formula, _ := n.Props["formula"].(string)
		b.WriteString("<code>")
		b.WriteString(html.EscapeString(formula))
		b.WriteString("</code>")
	case NodeQuote:
		b.WriteString("<blockquote>")
		renderChildren(b, n.Children, host)
		b.WriteString("</blockquote>")
	case NodeSearch:
		// 本家 MfmService.toHtml と同じく、URL は encodeURIComponent 相当でエスケープし、
		// リンクの文字には query ではなくボタンの語まで含む content を使う。
		query, _ := n.Props["query"].(string)
		content, _ := n.Props["content"].(string)
		b.WriteString(fmt.Sprintf(`<a href="%s">%s</a>`,
			html.EscapeString("https://www.google.com/search?q="+encodeURIComponent(query)),
			html.EscapeString(content)))
	case NodeURL:
		u, _ := n.Props["url"].(string)
		b.WriteString(fmt.Sprintf(`<a href="%s">%s</a>`,
			html.EscapeString(u), html.EscapeString(u)))
	case NodeLink:
		u, _ := n.Props["url"].(string)
		// XSS防止: http/https以外のスキーム (javascript: 等) はリンク化しない
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			renderChildren(b, n.Children, host)
			break
		}
		b.WriteString(fmt.Sprintf(`<a href="%s">`, html.EscapeString(u)))
		renderChildren(b, n.Children, host)
		b.WriteString("</a>")
	case NodeMention:
		username, _ := n.Props["username"].(string)
		mentionHost, _ := n.Props["host"].(string)
		acct, _ := n.Props["acct"].(string)

		var href string
		if mentionHost == "" {
			href = fmt.Sprintf("https://%s/@%s", host, username)
		} else {
			href = fmt.Sprintf("https://%s/@%s", mentionHost, username)
		}
		b.WriteString(fmt.Sprintf(`<a href="%s" class="u-url mention">%s</a>`,
			html.EscapeString(href), html.EscapeString(acct)))
	case NodeHashtag:
		tag, _ := n.Props["hashtag"].(string)
		b.WriteString(fmt.Sprintf(`<a href="https://%s/tags/%s" rel="tag">#%s</a>`,
			host, html.EscapeString(url.PathEscape(tag)), html.EscapeString(tag)))
	case NodeUnicodeEmoji:
		emoji, _ := n.Props["emoji"].(string)
		b.WriteString(emoji)
	case NodeEmojiCode:
		name, _ := n.Props["name"].(string)
		// ZWSP + :name: + ZWSP (TS版と同じ)
		b.WriteString("\u200b:")
		b.WriteString(html.EscapeString(name))
		b.WriteString(":\u200b")
	case NodeFn:
		renderFn(b, n, host)
	}
}

func renderFn(b *strings.Builder, n *Node, host string) {
	name, _ := n.Props["name"].(string)
	args, _ := n.Props["args"].(map[string]any)

	switch name {
	case "unixtime":
		// $[unixtime 1234567890] → <time>
		if len(n.Children) > 0 && n.Children[0].Type == NodeText {
			text := n.Children[0].textValue()
			if ts, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
				t := time.Unix(ts, 0).UTC()
				iso := t.Format(time.RFC3339)
				b.WriteString(fmt.Sprintf(`<time datetime="%s">%s</time>`, iso, iso))
				return
			}
		}
		// フォールバック
		b.WriteString("<i>")
		renderChildren(b, n.Children, host)
		b.WriteString("</i>")
	case "ruby":
		// $[ruby.rt=text base] → <ruby>
		rt := ""
		if args != nil {
			if v, ok := args["rt"].(string); ok {
				rt = v
			}
		}
		if rt != "" {
			b.WriteString("<ruby>")
			renderChildren(b, n.Children, host)
			b.WriteString("<rp>(</rp><rt>")
			b.WriteString(html.EscapeString(rt))
			b.WriteString("</rt><rp>)</rp></ruby>")
		} else {
			b.WriteString("<i>")
			renderChildren(b, n.Children, host)
			b.WriteString("</i>")
		}
	default:
		// 不明な fn は italic でフォールバック
		b.WriteString("<i>")
		renderChildren(b, n.Children, host)
		b.WriteString("</i>")
	}
}

func renderText(b *strings.Builder, text string) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		b.WriteString(html.EscapeString(line))
		if i < len(lines)-1 {
			b.WriteString("<br>")
		}
	}
}

func renderChildren(b *strings.Builder, children []*Node, host string) {
	for _, c := range children {
		renderNode(b, c, host)
	}
}

// encodeURIComponent percent-encodes s the same way as JavaScript's
// encodeURIComponent: every byte of the UTF-8 encoding is escaped as %XX
// (uppercase hex) except ASCII letters, digits and - _ . ! ~ * ' ( ).
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isURIComponentUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}

func isURIComponentUnreserved(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}
