// Package htmltext converts HTML descriptions into plain text the way
// upstream Misskey's misc/html-to-plain-text.ts does (2026.10.1).
package htmltext

import (
	"regexp"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/jsstr"
	"golang.org/x/net/html"
)

// separatorTags become a space, so words on both sides do not run together.
var separatorTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "blockquote": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"pre": true, "hr": true, "td": true, "th": true, "section": true,
}

// nonTextTags are dropped together with what they contain.
var nonTextTags = map[string]bool{
	"script": true, "style": true, "template": true, "textarea": true, "option": true, "xmp": true,
}

// 本家は sanitize-html が文字参照を解いた後に < > & を書き戻し、最後に
// `&lt;br&gt;` の形だけを空白に替える。文字参照で書いた `<br>` も区切りになる。
// 本家の `\s` は u フラグ付きの JavaScript の空白なので、Go の `\s` (ASCII だけ)
// でなく jsstr の集合で書く。
var escapedBreak = regexp.MustCompile(`(?i)<br[` + jsstr.SpaceClass + `]*/?>`)

// PlainText returns the text of html with the separator tags turned into
// spaces, the non-text tags dropped with their contents, other tags removed
// (their text kept), runs of white space collapsed and the ends trimmed.
func PlainText(src string) string {
	var b strings.Builder
	skip := 0
	z := html.NewTokenizer(strings.NewReader(src))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return collapse(b.String())
		case html.TextToken:
			if skip == 0 {
				b.WriteString(escapedBreak.ReplaceAllString(string(z.Text()), " "))
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			tok := z.Token()
			switch {
			case nonTextTags[tok.Data] && tok.Type == html.StartTagToken:
				skip++
			case nonTextTags[tok.Data] && tok.Type == html.EndTagToken:
				if skip > 0 {
					skip--
				}
			case separatorTags[tok.Data] && skip == 0:
				b.WriteByte(' ')
			}
		}
	}
}

// collapse replaces runs of white space (JavaScript's \s) with one space and
// trims both ends.
//
// 本家の`\s`は U+0085 (NEL) を含まないので、Go の unicode.IsSpace でなく
// jsstr.IsSpace で見る (#3369。以前は U+0085 も空白として潰していた)。
func collapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, jsstr.IsSpace), " ")
}
