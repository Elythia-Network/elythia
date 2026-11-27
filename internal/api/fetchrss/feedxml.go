package fetchrss

// The XML pull parser, the text helpers and the extension parsing in this file
// follow github.com/mmcdole/goxpp and github.com/mmcdole/gofeed v1.3.0, so that
// /api/fetch-rss returns the same values it returned while it used gofeed.
//
// gofeed をやめた理由と、同じ値を返すために残した細かな振る舞いは feedparse.go
// の先頭にある。
//
// ---
//
// The MIT License (MIT)
//
// Copyright (c) 2016 mmcdole
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

const xmlNSURI = "http://www.w3.org/XML/1998/namespace"

type xmlEvent int

const (
	evStartDocument xmlEvent = iota
	evEndDocument
	evStartTag
	evEndTag
	evText
	evComment
	evProcInst
	evDirective
)

// charsetReader converts the encoding named in the XML declaration to UTF-8.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	e, err := htmlindex.Get(label)
	if err != nil {
		return nil, fmt.Errorf("unsupported charset: %q", label)
	}
	return transform.NewReader(input, e.NewDecoder()), nil
}

// newXMLDecoder returns the non-strict decoder the feed parsers read with.
func newXMLDecoder(r io.Reader) *xml.Decoder {
	d := xml.NewDecoder(r)
	d.Strict = false
	d.CharsetReader = charsetReader
	return d
}

// pullParser is a pull parser over encoding/xml with the namespace and
// xml:base tracking of goxpp.
type pullParser struct {
	spaces      map[string]string
	spacesStack []map[string]string
	// spacesBefore is spaces before the current start tag was read.
	spacesBefore map[string]string
	baseStack    []*url.URL // 末尾が一番上
	// baseSizes is the length of each URL in baseStack (0 for one that repeats
	// the one below), and baseBytes their sum. baseLens is the length of each
	// URL, also for the repeated ones.
	baseSizes, baseLens []int
	baseBytes           int
	// maxBaseLen and maxBaseTotal bound one resolved xml:base and baseBytes
	// (maxXMLBaseLen and maxXMLBaseTotal), and resolveBudget the lengths of
	// all the xml:base resolved in the document (resolved).
	maxBaseLen, maxBaseTotal int
	resolveBudget, resolved  int

	event xmlEvent
	attrs []xml.Attr
	name  string
	space string
	text  string

	decoder *xml.Decoder
	token   xml.Token

	deadline time.Time   // 解析を打ち切る時刻 (ゼロなら打ち切らない)
	tokens   int         // 読んだトークンの数 (時刻を確かめる間隔に使う)
	dates    *dateReader // 日付を読む (同じ締め切りで止まる)
}

func newPullParser(r io.Reader) *pullParser {
	return &pullParser{
		decoder: newXMLDecoder(r), event: evStartDocument, spaces: map[string]string{}, dates: newDateReader(time.Time{}),
		maxBaseLen: maxXMLBaseLen, maxBaseTotal: maxXMLBaseTotal, resolveBudget: math.MaxInt,
	}
}

// errFeedParseTimeLimit is returned when parsing a feed took too long.
var errFeedParseTimeLimit = errors.New("parsing the feed took too long")

// tokenCheckInterval is how many tokens are read between two checks of the
// deadline.
const tokenCheckInterval = 1024

// overDeadline reports whether deadline has passed, checking the clock once
// every tokenCheckInterval calls (counted in *n).
func overDeadline(deadline time.Time, n *int) bool {
	*n++
	if deadline.IsZero() || *n%tokenCheckInterval != 0 {
		return false
	}
	return time.Now().After(deadline)
}

func (p *pullParser) baseTop() *url.URL {
	if len(p.baseStack) == 0 {
		return nil
	}
	return p.baseStack[len(p.baseStack)-1]
}

func (p *pullParser) nextToken() (xmlEvent, error) {
	p.attrs, p.name, p.space, p.text = nil, "", "", ""
	if overDeadline(p.deadline, &p.tokens) {
		return p.event, errFeedParseTimeLimit
	}
	tok, err := p.decoder.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			p.token = nil
			p.event = evEndDocument
			return p.event, nil
		}
		return p.event, err
	}
	p.token = xml.CopyToken(tok)
	switch t := p.token.(type) {
	case xml.StartElement:
		p.event = evStartTag
		p.attrs = t.Attr
		p.name = t.Name.Local
		p.space = t.Name.Space
		p.trackNamespaces(t)
		p.pushBase()
	case xml.EndElement:
		p.event = evEndTag
		// goxpp と同じく、終わりのタグのたびに名前空間と xml:base を 1 段戻す。
		// xml:base は、付いていない要素の終わりでも戻す (gofeed の解決結果を変えない)。
		if len(p.spacesStack) > 0 {
			p.spacesStack = p.spacesStack[:len(p.spacesStack)-1]
		}
		if len(p.spacesStack) == 0 {
			p.spaces = map[string]string{}
		} else {
			p.spaces = p.spacesStack[len(p.spacesStack)-1]
		}
		p.name = t.Name.Local
		p.popBase()
	case xml.CharData:
		p.event = evText
		p.text = string(t)
	case xml.Comment:
		p.event = evComment
		p.text = string(t)
	case xml.ProcInst:
		p.event = evProcInst
	case xml.Directive:
		p.event = evDirective
	}
	return p.event, nil
}

// next returns the next start tag, end tag, text or end of document.
func (p *pullParser) next() (xmlEvent, error) {
	for {
		ev, err := p.nextToken()
		if err != nil {
			return ev, err
		}
		switch ev {
		case evStartTag, evEndTag, evEndDocument, evText:
			return ev, nil
		}
	}
}

// skip consumes the current element up to its end tag.
func (p *pullParser) skip() error {
	depth := 0
	for {
		ev, err := p.nextToken()
		if err != nil {
			return err
		}
		switch ev {
		case evStartTag:
			depth++
		case evEndTag:
			if depth == 0 {
				return nil
			}
			depth--
		case evEndDocument:
			return io.ErrUnexpectedEOF
		}
	}
}

func (p *pullParser) attribute(name string) string {
	for _, a := range p.attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func (p *pullParser) expect(ev xmlEvent, name string) error {
	if p.event == ev && (name == "*" || strings.EqualFold(p.name, name)) {
		return nil
	}
	return fmt.Errorf("expected %q (event %d) but got %q (event %d)", name, ev, p.name, p.event)
}

// decodeElement decodes the current element into v and leaves the parser on
// its end tag, like goxpp's DecodeElement.
func (p *pullParser) decodeElement(v any) error {
	start, ok := p.token.(xml.StartElement)
	if p.event != evStartTag || !ok {
		return errors.New("decodeElement must be called on a start tag")
	}
	if err := p.decoder.DecodeElement(v, &start); err != nil {
		return err
	}
	name := p.name
	p.attrs, p.name, p.space, p.text = nil, name, "", ""
	p.event = evEndTag
	p.token = nil
	// 終わりのタグは encoding/xml が読むので、ここで名前空間の段を 1 段戻し、
	// その要素を読む前の表に戻す。読み飛ばした要素が宣言した接頭辞は、後ろの
	// 要素に持ち越さない。
	if n := len(p.spacesStack); n > 0 && p.spacesBefore != nil {
		p.spacesStack[n-1] = nil
		p.spacesStack = p.spacesStack[:n-1]
		p.spaces = p.spacesBefore
	}
	// 要素自身に xml:base があったときだけ戻す (gofeed と同じ)。
	for _, a := range start.Attr {
		if a.Name.Space == xmlNSURI && a.Name.Local == "base" {
			p.popBase()
			break
		}
	}
	return nil
}

func (p *pullParser) trackNamespaces(t xml.StartElement) {
	p.spacesBefore = p.spaces
	// 表を変えない要素 (宣言を持たないか、今の表と同じ宣言だけを持つもの) は、
	// 親の表をそのまま積む (表は書き換えないので共有できる)。表を変えるときは
	// 写して作る。URI の数は checkXMLShape が文書全体で抑えるので、1 回に写す
	// 件数にも上限がある。
	changes := false
	for _, a := range t.Attr {
		uri, prefix, ok := namespaceDecl(a)
		if !ok {
			continue
		}
		if cur, has := p.spaces[uri]; !has || cur != prefix {
			changes = true
			break
		}
	}
	if !changes {
		p.spacesStack = append(p.spacesStack, p.spaces)
		return
	}
	ns := make(map[string]string, len(p.spaces)+1)
	for k, v := range p.spaces {
		ns[k] = v
	}
	for _, a := range t.Attr {
		if uri, prefix, ok := namespaceDecl(a); ok {
			ns[uri] = prefix
		}
	}
	p.spaces = ns
	p.spacesStack = append(p.spacesStack, ns)
}

// namespaceDecl returns the URI and prefix that a declares, if it is a
// namespace declaration ("" is the prefix of the default namespace).
func namespaceDecl(a xml.Attr) (uri, prefix string, ok bool) {
	switch {
	case a.Name.Space == "xmlns":
		return strings.TrimSpace(a.Value), strings.TrimSpace(strings.ToLower(a.Name.Local)), true
	case a.Name.Local == "xmlns":
		return strings.TrimSpace(a.Value), "", true
	}
	return "", "", false
}

func (p *pullParser) popBase() {
	if n := len(p.baseStack); n > 0 {
		p.baseStack = p.baseStack[:n-1]
		p.baseBytes -= p.baseSizes[n-1]
		p.baseSizes = p.baseSizes[:n-1]
		p.baseLens = p.baseLens[:n-1]
	}
}

func (p *pullParser) pushBase() {
	var base string
	for _, a := range p.attrs {
		if a.Name.Local == "base" && a.Name.Space == xmlNSURI {
			base = a.Value
			break
		}
	}
	if base == "" {
		return
	}
	// 解決した xml:base の長さ (percent-encode した後のバイト数) と、段に積んだ URL
	// の長さの合計、文書全体で解決した長さの合計に上限を置く。超えたときは今の
	// 一番上の写しを積む (終わりのタグで 1 段戻す数え方は変えない)。
	top := p.baseTop()
	topLen := 0
	if n := len(p.baseLens); n > 0 {
		topLen = p.baseLens[n-1]
	}
	// 解決した長さは、基底の長さと、percent-encode した値の長さ (多くて 3 倍) の和を
	// 超えない。
	if p.resolved+3*len(base)+topLen > p.resolveBudget {
		p.repeatBase(top, topLen)
		return
	}
	u, err := url.Parse(base)
	if err != nil {
		return
	}
	if top != nil {
		u = top.ResolveReference(u)
	}
	n := len(u.String())
	p.resolved += n
	if n > p.maxBaseLen || p.baseBytes+n > p.maxBaseTotal {
		p.repeatBase(top, topLen)
		return
	}
	p.appendBase(u, n, n)
}

// repeatBase pushes a copy of top (the URL below), not counted in baseBytes.
// 写しにするのは、解決するときに一番上の Path を書き換えることがあるため。
func (p *pullParser) repeatBase(top *url.URL, topLen int) {
	if top != nil {
		c := *top
		top = &c
	}
	p.appendBase(top, 0, topLen)
}

func (p *pullParser) appendBase(u *url.URL, size, length int) {
	p.baseStack = append(p.baseStack, u)
	p.baseSizes = append(p.baseSizes, size)
	p.baseLens = append(p.baseLens, length)
	p.baseBytes += size
}

// findRoot moves to the first start tag.
func (p *pullParser) findRoot() error {
	for {
		ev, err := p.next()
		if err != nil {
			return err
		}
		if ev == evStartTag {
			return nil
		}
		if ev == evEndDocument {
			return errors.New("failed to find root node before document end")
		}
	}
}

// uriAttrs are the attributes resolved against xml:base (gofeed's list).
var uriAttrs = map[string]bool{"href": true, "scheme": true, "src": true, "uri": true}

// nextTag moves to the next start or end tag, resolving the URI attributes of
// a start tag against xml:base (gofeed's shared.NextTag).
func (p *pullParser) nextTag() (xmlEvent, error) {
	for {
		ev, err := p.next()
		if err != nil {
			return ev, err
		}
		switch ev {
		case evEndTag:
			return ev, nil
		case evStartTag:
			for i, a := range p.attrs {
				if !uriAttrs[strings.ToLower(a.Name.Local)] {
					continue
				}
				abs, err := xmlBaseResolveURL(p.baseTop(), a.Value)
				if err != nil {
					return ev, err
				}
				if abs != nil {
					p.attrs[i].Value = abs.String()
				}
			}
			return ev, nil
		case evEndDocument:
			return ev, errors.New("failed to find next tag before the end of the document")
		}
	}
}

// xmlBaseResolveURL resolves u against b. Like gofeed it treats the path of b
// as a directory, and returns u re-serialized when there is no base.
func xmlBaseResolveURL(b *url.URL, u string) (*url.URL, error) {
	rel, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return rel, nil
	}
	if b.Path != "" && u != "" && b.Path[len(b.Path)-1] != '/' {
		b.Path += "/"
	}
	return b.ResolveReference(rel), nil
}

const (
	cdataStart = "<![CDATA["
	cdataEnd   = "]]>"
)

// parseText reads the current element as text: the raw inner XML, trimmed,
// with CDATA sections unwrapped or entities decoded (gofeed's ParseText).
func (p *pullParser) parseText() (string, error) {
	var text struct {
		InnerXML string `xml:",innerxml"`
	}
	if err := p.decodeElement(&text); err != nil {
		return "", err
	}
	result := strings.TrimSpace(text.InnerXML)
	if strings.Contains(result, cdataStart) {
		return stripCDATA(result), nil
	}
	return decodeEntities(result), nil
}

// stripCDATA unwraps CDATA sections and decodes entities outside them, with
// gofeed's exact behaviour (text before a CDATA section is dropped).
func stripCDATA(str string) string {
	var buf strings.Builder
	curr := 0
	for curr < len(str) {
		start := indexAt(str, cdataStart, curr)
		if start == -1 {
			buf.WriteString(decodeEntities(str[curr:]))
			return buf.String()
		}
		end := indexAt(str, cdataEnd, start)
		if end == -1 {
			buf.WriteString(decodeEntities(str[curr:]))
			return buf.String()
		}
		buf.WriteString(str[start+len(cdataStart) : end])
		// gofeed と同じく、次の位置は end の分だけ先へ進める (curr を足し込む)。
		curr = curr + end + len(cdataEnd)
	}
	return buf.String()
}

func indexAt(str, substr string, start int) int {
	idx := strings.Index(str[start:], substr)
	if idx > -1 {
		idx += start
	}
	return idx
}

// decodeEntities decodes character references with the HTML entity table, as
// gofeed's DecodeEntities does (a '&' without a ';', or with a space before
// it, ends the decoding).
func decodeEntities(str string) string {
	data := []byte(str)
	var buf bytes.Buffer
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '&')
		if idx == -1 {
			buf.Write(data)
			break
		}
		buf.Write(data[:idx])
		data = data[idx:]
		if len(data) == 1 {
			buf.Write(data)
			return buf.String()
		}
		end := bytes.IndexByte(data, ';')
		if end == -1 || bytes.Contains(data[1:end], []byte(" ")) {
			buf.Write(data)
			return buf.String()
		}
		buf.WriteString(html.UnescapeString(string(data[:end+1])))
		data = data[end+1:]
	}
	return buf.String()
}

var (
	emailNameRgx = regexp.MustCompile(`^([^@]+@[^\s]+)\s+\(([^@]+)\)$`)
	nameEmailRgx = regexp.MustCompile(`^([^@]+)\s+\(([^@]+@[^)]+)\)$`)
	nameOnlyRgx  = regexp.MustCompile(`^([^@()]+)$`)
	emailOnlyRgx = regexp.MustCompile(`^([^@()]+@[^@()]+)$`)
)

// parseNameAddress splits "Name (mail)" / "mail (Name)" style strings.
func parseNameAddress(s string) (name, address string) {
	if s == "" {
		return
	}
	if m := emailNameRgx.FindStringSubmatch(s); m != nil {
		return m[2], m[1]
	}
	if m := nameEmailRgx.FindStringSubmatch(s); m != nil {
		return m[1], m[2]
	}
	if m := nameOnlyRgx.FindStringSubmatch(s); m != nil {
		return m[1], ""
	}
	if m := emailOnlyRgx.FindStringSubmatch(s); m != nil {
		return "", m[1]
	}
	return
}

// extension is an element in a non-default namespace (gofeed's ext.Extension).
type extension struct {
	name     string
	value    string
	attrs    map[string]string
	children map[string][]extension
}

// extGroup maps an element name to the elements of one namespace.
type extGroup map[string][]extension

// extensions maps a namespace prefix to its elements.
type extensions map[string]extGroup

func (p *pullParser) prefixForNamespace(space string) string {
	if prefix, ok := canonicalNamespaces[space]; ok {
		return prefix
	}
	if prefix, ok := p.spaces[space]; ok {
		return prefix
	}
	return space
}

func (p *pullParser) isExtension() bool {
	prefix := p.prefixForNamespace(strings.TrimSpace(p.space))
	return !(prefix == "" || prefix == "rss" || prefix == "rdf" || prefix == "content")
}

func (p *pullParser) parseExtension(fe extensions) error {
	prefix := p.prefixForNamespace(p.space)
	e, err := p.parseExtensionElement()
	if err != nil {
		return err
	}
	if fe[prefix] == nil {
		fe[prefix] = extGroup{}
	}
	fe[prefix][p.name] = append(fe[prefix][p.name], e)
	return nil
}

func (p *pullParser) parseExtensionElement() (extension, error) {
	// 属性と子の表は、持つときだけ作る (読む側は nil の表でも空として読める)。
	e := extension{name: p.name}
	var value strings.Builder
	if err := p.expect(evStartTag, "*"); err != nil {
		return e, err
	}
	for _, a := range p.attrs {
		if e.attrs == nil {
			e.attrs = make(map[string]string, len(p.attrs))
		}
		e.attrs[a.Name.Local] = a.Value
	}
	for {
		ev, err := p.next()
		if err != nil {
			return e, err
		}
		if ev == evEndTag {
			break
		}
		if ev == evEndDocument {
			return e, io.ErrUnexpectedEOF
		}
		if ev == evStartTag {
			child, err := p.parseExtensionElement()
			if err != nil {
				return e, err
			}
			if e.children == nil {
				e.children = map[string][]extension{}
			}
			e.children[child.name] = append(e.children[child.name], child)
		} else if ev == evText {
			value.WriteString(p.text)
		}
	}
	e.value = strings.TrimSpace(value.String())
	return e, p.expect(evEndTag, e.name)
}

// first returns the value of the first element named name.
func (m extGroup) first(name string) string {
	if v := m[name]; len(v) > 0 {
		return v[0].value
	}
	return ""
}

// all returns the values of the elements named name, or nil if there is none.
func (m extGroup) all(name string) []string {
	v, ok := m[name]
	if !ok || len(v) == 0 {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, e := range v {
		out = append(out, e.value)
	}
	return out
}

// canonicalNamespaces are the prefixes gofeed uses for well-known namespaces,
// whatever prefix the feed declares.
var canonicalNamespaces = map[string]string{
	"http://webns.net/mvcb/":                                         "admin",
	"http://purl.org/rss/1.0/modules/aggregation/":                   "ag",
	"http://purl.org/rss/1.0/modules/annotate/":                      "annotate",
	"http://media.tangent.org/rss/1.0/":                              "audio",
	"http://backend.userland.com/blogChannelModule":                  "blogChannel",
	"http://creativecommons.org/ns#license":                          "cc",
	"http://web.resource.org/cc/":                                    "cc",
	"http://cyber.law.harvard.edu/rss/creativeCommonsRssModule.html": "creativeCommons",
	"http://backend.userland.com/creativeCommonsRssModule":           "creativeCommons",
	"http://purl.org/rss/1.0/modules/company":                        "co",
	"http://purl.org/rss/1.0/modules/content/":                       "content",
	"http://my.theinfo.org/changed/1.0/rss/":                         "cp",
	"http://purl.org/dc/elements/1.1/":                               "dc",
	"http://purl.org/dc/terms/":                                      "dcterms",
	"http://purl.org/rss/1.0/modules/email/":                         "email",
	"http://purl.org/rss/1.0/modules/event/":                         "ev",
	"http://rssnamespace.org/feedburner/ext/1.0":                     "feedburner",
	"http://freshmeat.net/rss/fm/":                                   "fm",
	"http://xmlns.com/foaf/0.1/":                                     "foaf",
	"http://www.w3.org/2003/01/geo/wgs84_pos#":                       "geo",
	"http://www.georss.org/georss":                                   "georss",
	"http://www.opengis.net/gml":                                     "gml",
	"http://postneo.com/icbm/":                                       "icbm",
	"http://purl.org/rss/1.0/modules/image/":                         "image",
	"http://www.itunes.com/DTDs/PodCast-1.0.dtd":                     "itunes",
	"http://example.com/DTDs/PodCast-1.0.dtd":                        "itunes",
	"http://purl.org/rss/1.0/modules/link/":                          "l",
	"http://search.yahoo.com/mrss":                                   "media",
	"http://search.yahoo.com/mrss/":                                  "media",
	"http://madskills.com/public/xml/rss/module/pingback/":           "pingback",
	"http://prismstandard.org/namespaces/1.2/basic/":                 "prism",
	"http://www.w3.org/1999/02/22-rdf-syntax-ns#":                    "rdf",
	"http://www.w3.org/2000/01/rdf-schema#":                          "rdfs",
	"http://purl.org/rss/1.0/modules/reference/":                     "ref",
	"http://purl.org/rss/1.0/modules/richequiv/":                     "reqv",
	"http://purl.org/rss/1.0/modules/search/":                        "search",
	"http://purl.org/rss/1.0/modules/slash/":                         "slash",
	"http://schemas.xmlsoap.org/soap/envelope/":                      "soap",
	"http://purl.org/rss/1.0/modules/servicestatus/":                 "ss",
	"http://hacks.benhammersley.com/rss/streaming/":                  "str",
	"http://purl.org/rss/1.0/modules/subscription/":                  "sub",
	"http://purl.org/rss/1.0/modules/syndication/":                   "sy",
	"http://schemas.pocketsoap.com/rss/myDescModule/":                "szf",
	"http://purl.org/rss/1.0/modules/taxonomy/":                      "taxo",
	"http://purl.org/rss/1.0/modules/threading/":                     "thr",
	"http://purl.org/rss/1.0/modules/textinput/":                     "ti",
	"http://madskills.com/public/xml/rss/module/trackback/":          "trackback",
	"http://wellformedweb.org/commentAPI/":                           "wfw",
	"http://purl.org/rss/1.0/modules/wiki/":                          "wiki",
	"http://www.w3.org/1999/xhtml":                                   "xhtml",
	"http://www.w3.org/1999/xlink":                                   "xlink",
	"http://www.w3.org/XML/1998/namespace":                           "xml",
	"http://podlove.org/simple-chapters":                             "psc",
}
