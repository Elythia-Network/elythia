package mfm

import (
	"fmt"
	"strings"
)

// ToHTML converts MFM nodes to an HTML string without any mentioned remote
// users, i.e. every mention links to this server's `/@<acct>` page.
// host はローカルホスト名 (例: "example.com")。
// メンションやハッシュタグのリンク先URLの生成に使う。
//
// 出力は本家 MfmService.toHtml に揃えてある (文字列を連結するだけで、DOM を
// 通した直列化はしない)。本家で mentionedRemoteUsers を渡さない呼び出し
// (ApRendererService.renderPerson の summary) はこちらに当たる。
func ToHTML(nodes []*Node, host string) string {
	return ToHTMLWithMentions(nodes, host, nil)
}

// ToHTMLWithMentions converts MFM nodes to an HTML string like upstream
// MfmService.toHtml(nodes, mentionedRemoteUsers): a mention of a user in
// mentioned links to that user's url (or uri when url is empty).
//
// 本家はノートの本文 (ApMfmService.getNoteHtml) とフィード (FeedService) で、
// ノートの mentionedRemoteUsers 列を渡す。
func ToHTMLWithMentions(nodes []*Node, host string, mentioned []MentionedRemoteUser) string {
	if len(nodes) == 0 {
		return ""
	}
	hc := &htmlContext{host: host, mentioned: mentioned}
	var b strings.Builder
	for _, n := range nodes {
		renderNode(&b, n, hc)
	}
	return b.String()
}

// htmlContext carries the per-call inputs of ToHTMLWithMentions.
type htmlContext struct {
	host      string
	mentioned []MentionedRemoteUser
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

func renderNode(b *strings.Builder, n *Node, hc *htmlContext) {
	switch n.Type {
	case NodeText:
		renderText(b, n.textValue())
	case NodeBold:
		b.WriteString("<b>")
		renderChildren(b, n.Children, hc)
		b.WriteString("</b>")
	case NodeItalic:
		b.WriteString("<i>")
		renderChildren(b, n.Children, hc)
		b.WriteString("</i>")
	case NodeStrike:
		b.WriteString("<del>")
		renderChildren(b, n.Children, hc)
		b.WriteString("</del>")
	case NodeSmall:
		b.WriteString("<small>")
		renderChildren(b, n.Children, hc)
		b.WriteString("</small>")
	case NodeCenter:
		b.WriteString(`<div style="text-align: center;">`)
		renderChildren(b, n.Children, hc)
		b.WriteString("</div>")
	case NodePlain:
		b.WriteString("<span>")
		renderChildren(b, n.Children, hc)
		b.WriteString("</span>")
	case NodeInlineCode:
		code, _ := n.Props["code"].(string)
		b.WriteString("<code>")
		b.WriteString(EscapeHTML(code))
		b.WriteString("</code>")
	case NodeBlockCode:
		code, _ := n.Props["code"].(string)
		b.WriteString("<pre><code>")
		b.WriteString(EscapeHTML(code))
		b.WriteString("</code></pre>")
	case NodeMathInline:
		formula, _ := n.Props["formula"].(string)
		b.WriteString("<code>")
		b.WriteString(EscapeHTML(formula))
		b.WriteString("</code>")
	case NodeMathBlock:
		formula, _ := n.Props["formula"].(string)
		b.WriteString("<pre><code>")
		b.WriteString(EscapeHTML(formula))
		b.WriteString("</code></pre>")
	case NodeQuote:
		b.WriteString("<blockquote>")
		renderChildren(b, n.Children, hc)
		b.WriteString("</blockquote>")
	case NodeSearch:
		// 本家 MfmService.toHtml と同じく、URL は encodeURIComponent 相当でエスケープし、
		// リンクの文字には query ではなくボタンの語まで含む content を使う。
		query, _ := n.Props["query"].(string)
		content, _ := n.Props["content"].(string)
		b.WriteString(fmt.Sprintf(`<a href="%s">%s</a>`,
			EscapeHTML("https://www.google.com/search?q="+encodeURIComponent(query)),
			EscapeHTML(content)))
	case NodeURL:
		// 本家と同じく href は `new URL(url).href` で正規化し、文字は元の url の
		// まま出す。URL として読めなければリンクにしない
		u, _ := n.Props["url"].(string)
		href, ok := whatwgHref(u)
		if !ok {
			b.WriteString(EscapeHTML(u))
			break
		}
		b.WriteString(fmt.Sprintf(`<a href="%s">%s</a>`,
			EscapeHTML(href), EscapeHTML(u)))
	case NodeLink:
		// whatwgHref は http / https 以外を失敗にするので、javascript: などの
		// scheme はリンクにならない (XSS 防止)。失敗は本家と同じく `[文字](url)` の
		// 文字にする
		u, _ := n.Props["url"].(string)
		href, ok := whatwgHref(u)
		if !ok {
			b.WriteByte('[')
			renderChildren(b, n.Children, hc)
			b.WriteString("](")
			b.WriteString(EscapeHTML(u))
			b.WriteByte(')')
			break
		}
		b.WriteString(fmt.Sprintf(`<a href="%s">`, EscapeHTML(href)))
		renderChildren(b, n.Children, hc)
		b.WriteString("</a>")
	case NodeMention:
		username, _ := n.Props["username"].(string)
		mentionHost, _ := n.Props["host"].(string)
		acct, _ := n.Props["acct"].(string)
		href, ok := whatwgHref(hc.mentionHref(username, mentionHost, acct))
		if !ok {
			b.WriteString(EscapeHTML(acct))
			break
		}
		b.WriteString(fmt.Sprintf(`<a href="%s" class="u-url mention">%s</a>`,
			EscapeHTML(href), EscapeHTML(acct)))
	case NodeHashtag:
		// 本家は encodeURIComponent でエスケープする。url.PathEscape は `&` `+` `=`
		// などを残すので、href が本家と違っていた (#3329)
		tag, _ := n.Props["hashtag"].(string)
		b.WriteString(fmt.Sprintf(`<a href="https://%s/tags/%s" rel="tag">#%s</a>`,
			hc.host, EscapeHTML(encodeURIComponent(tag)), EscapeHTML(tag)))
	case NodeUnicodeEmoji:
		emoji, _ := n.Props["emoji"].(string)
		b.WriteString(emoji)
	case NodeEmojiCode:
		name, _ := n.Props["name"].(string)
		// ZWSP + :name: + ZWSP (TS版と同じ)
		b.WriteString("\u200b:")
		b.WriteString(EscapeHTML(name))
		b.WriteString(":\u200b")
	case NodeFn:
		renderFn(b, n, hc)
	}
}

func renderFn(b *strings.Builder, n *Node, hc *htmlContext) {
	name, _ := n.Props["name"].(string)

	switch name {
	case "unixtime":
		// 本家: new Date(parseInt(最初の子の文字, 10) * 1000).toISOString()。
		// 最初の子が文字でなければ空文字を読んで失敗し、斜体に戻す
		if len(n.Children) > 0 && n.Children[0].Type == NodeText {
			if iso, ok := jsUnixtimeISO(n.Children[0].textValue()); ok {
				b.WriteString(fmt.Sprintf(`<time datetime="%s">%s</time>`, iso, iso))
				return
			}
		}
	case "ruby":
		if renderRuby(b, n, hc) {
			return
		}
	}
	// 不明な fn と、読めなかった unixtime / ruby は斜体にする (本家の fnDefault)
	b.WriteString("<i>")
	renderChildren(b, n.Children, hc)
	b.WriteString("</i>")
}

// renderRuby writes upstream's ruby rendering and reports whether it did.
//
// 本家は ruby の引数 (`$[ruby.rt=x ...]`) を見ない。子が 1 つなら、その文字を
// 半角空白で切って 1 つ目を本文、2 つ目をルビにする。子が 2 つ以上なら、最後の子
// (文字でなければ空) を trim してルビにし、残りの子を本文にする。
//
// 子が 1 つで半角空白が無いとき (`$[ruby abc]`) や、1 つの子が文字でないときは、
// 本家は escapeHtml(undefined) で TypeError を投げ、ノートの HTML を作れない。
// 例外で配送や描画を止めるわけにいかないので、mk-go は斜体に戻す
// (docs/divergence.md)。
func renderRuby(b *strings.Builder, n *Node, hc *htmlContext) bool {
	switch len(n.Children) {
	case 0:
		return false
	case 1:
		child := n.Children[0]
		if child.Type != NodeText {
			return false
		}
		parts := strings.Split(child.textValue(), " ")
		if len(parts) < 2 {
			return false
		}
		b.WriteString("<ruby>")
		b.WriteString(EscapeHTML(parts[0]))
		b.WriteString("<rp>(</rp><rt>")
		b.WriteString(EscapeHTML(parts[1]))
		b.WriteString("</rt><rp>)</rp></ruby>")
		return true
	}
	last := n.Children[len(n.Children)-1]
	rt := ""
	if last.Type == NodeText {
		rt = last.textValue()
	}
	b.WriteString("<ruby>")
	renderChildren(b, n.Children[:len(n.Children)-1], hc)
	b.WriteString("<rp>(</rp><rt>")
	b.WriteString(EscapeHTML(jsTrim(rt)))
	b.WriteString("</rt><rp>)</rp></ruby>")
	return true
}

// renderText writes a text node like upstream: lines split on CRLF, CR or LF
// and joined with `<br />`.
func renderText(b *strings.Builder, text string) {
	for {
		i := strings.IndexAny(text, "\r\n")
		if i < 0 {
			b.WriteString(EscapeHTML(text))
			return
		}
		b.WriteString(EscapeHTML(text[:i]))
		b.WriteString("<br />")
		if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
			i++
		}
		text = text[i+1:]
	}
}

// htmlEscaper mirrors upstream's escapeHtml (packages/backend/src/misc/escape-html.ts).
var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#039;",
)

// EscapeHTML escapes s exactly like upstream's escapeHtml, which MfmService
// and ApRendererService use for the HTML they federate.
//
// html.EscapeString は `'` / `"` を `&#39;` / `&#34;` にする。意味は同じだが、
// エスケープの表記を本家に揃えるため、本家と同じ置き換えにする。
func EscapeHTML(s string) string { return htmlEscaper.Replace(s) }

func renderChildren(b *strings.Builder, children []*Node, hc *htmlContext) {
	for _, c := range children {
		renderNode(b, c, hc)
	}
}

// encodeURIComponent percent-encodes s the same way as JavaScript's
// encodeURIComponent: every byte of the UTF-8 encoding is escaped as %XX
// (uppercase hex) except ASCII letters, digits and - _ . ! ~ * ' ( ).
//
// JavaScript の encodeURIComponent は孤立したサロゲートで URIError を投げるが、
// ここへ来る文字列は検索構文の query とハッシュタグだけで、Parse が入口で
// ToValidUTF8 を通した入力の部分文字列なので、常に正しい UTF-8 (サロゲートの
// 符号も含まない) になる。不正なバイトの扱いを JavaScript に合わせる経路が無いので、
// バイトごとにエスケープするままにしてある (#3329)。
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
