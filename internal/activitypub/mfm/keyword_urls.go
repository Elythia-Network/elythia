package mfm

import (
	"strings"
	"unicode/utf8"

	mfmparser "github.com/elythia-network/mfm"
	"golang.org/x/net/idna"
)

// keywordCheckMaxUTF16 is upstream DB_MAX_NOTE_TEXT_LENGTH (const.ts): only the
// first this many UTF-16 code units of a text are searched for URLs.
const keywordCheckMaxUTF16 = 8192

// KeywordCheckTarget returns text followed by the URLs that
// KeywordCheckURLs finds in it, one per line, for matching against
// meta.prohibitedWords / meta.sensitiveWords. It returns text unchanged when
// there are no URLs.
//
// 本家 NoteCreateService は、判定する文字列に extractUrlsForKeyWordCheck の結果を
// 改行でつないで足してから isKeyWordIncluded に渡す。Elythia は cw と text を別々に
// 判定する (#1106) ので、それぞれの欄に、その欄から取り出した URL を足す。
func KeywordCheckTarget(text string) string {
	urls := KeywordCheckURLs(text)
	if len(urls) == 0 {
		return text
	}
	return text + "\n" + strings.Join(urls, "\n")
}

// KeywordCheckURLs returns, for every MFM url and link node in text, the URL's
// href and the URL in the form a client displays it: the host converted to
// Unicode and the path, query and fragment percent-decoded. It mirrors upstream
// extractUrlsForKeyWordCheck (misc/extract-urls-for-keyword-check.ts).
//
// 本家と同じく、パーセントエンコードや全角文字で表記したドメインを、表示される形に
// 直してから禁止ワードなどと比べるために使う。保存時に切り詰められる範囲より後ろは
// 表示されないので、本家と同じく先頭の DB_MAX_NOTE_TEXT_LENGTH (UTF-16 で 8192)
// までだけを読む。読む長さを抑えるのは解析の費用を抑えるためでもある。
func KeywordCheckURLs(text string) []string {
	if !strings.Contains(text, "://") {
		return nil
	}
	var urls []string
	var walk func(nodes []*mfmparser.Node)
	walk = func(nodes []*mfmparser.Node) {
		for _, n := range nodes {
			if n.Type == mfmparser.NodeURL || n.Type == mfmparser.NodeLink {
				if raw, ok := n.Props["url"].(string); ok {
					if u, ok := parseWhatwgURL(raw); ok {
						urls = append(urls, u.href(), u.displayForm())
					}
				}
			}
			walk(n.Children)
		}
	}
	walk(mfmparser.Parse(clipUTF16(text, keywordCheckMaxUTF16)))
	return urls
}

// displayForm returns the URL as upstream extractUrlsForKeyWordCheck
// normalizes it: `${protocol}//${hostname}${port}` followed by the
// percent-decoded pathname, search and hash. Userinfo is dropped.
func (u whatwgURL) displayForm() string {
	var b strings.Builder
	b.WriteString(u.scheme)
	b.WriteString("://")
	b.WriteString(hostToUnicode(u.host))
	if u.port != "" {
		b.WriteByte(':')
		b.WriteString(u.port)
	}
	b.WriteString(safeDecodeURIComponent(u.path))
	b.WriteString(safeDecodeURIComponent(u.search()))
	b.WriteString(safeDecodeURIComponent(u.hash()))
	return b.String()
}

// hostToUnicode converts the punycode labels of a serialized host to Unicode,
// like Node's url.domainToUnicode. A label that does not decode is kept as is.
func hostToUnicode(host string) string {
	if !strings.Contains(host, "xn--") || strings.HasPrefix(host, "[") {
		return host
	}
	labels := strings.Split(host, ".")
	for i, label := range labels {
		if !strings.HasPrefix(label, "xn--") {
			continue
		}
		// 拡張部分が空のラベル (`xn--badword-`) は基本文字だけに decode される
		// (Node の domainToUnicode と同じ)
		if basic, ok := emptyExtensionPunycodeBasic(label); ok && basic != "" {
			labels[i] = basic
			continue
		}
		// `xn---abc` は区切りの `-` が先頭にしかない綴りで、x/net/idna は decode に
		// 失敗する。同じ文字列に decode される `xn--abc` で読む (domainToASCII と同じ)
		decodeAs := label
		if len(label) > 5 && label[:5] == "xn---" && !strings.Contains(label[5:], "-") {
			decodeAs = label[:4] + label[5:]
		}
		if u, err := idna.Punycode.ToUnicode(decodeAs); err == nil {
			labels[i] = u
		}
	}
	return strings.Join(labels, ".")
}

// safeDecodeURIComponent percent-decodes s like JavaScript's
// decodeURIComponent, except that an escape which is not part of a valid UTF-8
// sequence is left in its original form instead of failing the whole string.
// It mirrors upstream safeDecodeURIComponent (misc/safe-decode-uri-component.ts).
func safeDecodeURIComponent(s string) string {
	if strings.IndexByte(s, '%') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if !isPercentEscape(s, i) {
			b.WriteByte(s[i])
			i++
			continue
		}
		// 連続するエスケープをまとめてバイト列に直し、UTF-8 として正しい並びだけを
		// 文字にする。正しくないバイトは元の `%XX` の綴りのまま残す
		start := i
		var bytes []byte
		for isPercentEscape(s, i) {
			bytes = append(bytes, hexVal(s[i+1])<<4|hexVal(s[i+2]))
			i += 3
		}
		for k := 0; k < len(bytes); {
			r, size := utf8.DecodeRune(bytes[k:])
			if r == utf8.RuneError && size <= 1 {
				pos := start + k*3
				b.WriteString(s[pos : pos+3])
				k++
				continue
			}
			b.Write(bytes[k : k+size])
			k += size
		}
	}
	return b.String()
}

// isPercentEscape reports whether s has a `%XX` escape at i.
func isPercentEscape(s string, i int) bool {
	return i+2 < len(s) && s[i] == '%' && isASCIIHex(s[i+1]) && isASCIIHex(s[i+2])
}

// clipUTF16 cuts s to at most max UTF-16 code units, never splitting a
// surrogate pair.
//
// 本家の `text.slice(0, n)` は UTF-16 の単位で切る。サロゲートペアの途中に当たると
// 本家は対になっていない surrogate を残すが、URL の判定には関わらないので手前で切る。
func clipUTF16(s string, max int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > max {
			return s[:i]
		}
		units += w
	}
	return s
}
