package fetchrss

// フィードの解析 (RSS 0.9x / 1.0 / 2.0、Atom 0.3 / 1.0、JSON Feed)。
//
// 以前は github.com/mmcdole/gofeed で読んでいた。gofeed は HTML を木として解析する
// 箇所を持つ。外から来る HTML は木にせずに読むことにしたので (internal/misc/htmlstream)、
// HTML を解析しない形で読み直した。返す値は gofeed のときと同じにする
// (testdata/feeds の突き合わせ)。違うのは、gofeed が HTML を解析して作っていた値と、
// 名前空間の宣言の有効範囲の扱いだけ (docs/divergence/api.md)。
//
// gofeed の細かな振る舞い (xml:base の解決の範囲、CDATA の前の文字の扱い、
// 日付の形式の一覧など) は、返す値を変えないためにそのまま残している。読み取りの
// 組み立ては gofeed の rss / atom / json の parser と translator に倣っており、
// gofeed の許諾 (MIT) は feedxml.go の先頭にある。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// feed is the subset of a feed that /api/fetch-rss returns.
type feed struct {
	Title       string
	Description string
	Link        string
	FeedLink    string
	Image       *feedImage
	ITunes      *itunesFeed
	Items       []*feedItem
}

type feedImage struct {
	URL   string
	Title string
}

type itunesFeed struct {
	Author     string
	Summary    string
	Explicit   string
	Keywords   string
	Image      string
	Categories []*itunesCategory
	Owner      *itunesOwner
}

type itunesCategory struct {
	Text        string
	Subcategory *itunesCategory
}

type itunesOwner struct {
	Name  string
	Email string
}

type feedPerson struct {
	Name  string
	Email string
}

type feedItem struct {
	Title           string
	Description     string
	Content         string
	Link            string
	GUID            string
	Published       string
	PublishedParsed *time.Time
	UpdatedParsed   *time.Time
	Authors         []*feedPerson
	Author          *feedPerson
	DCCreator       []string
	Categories      []string
	Enclosures      []*feedEnclosure
}

type feedEnclosure struct {
	URL    string
	Type   string
	Length string
}

var errFeedTypeNotDetected = errors.New("failed to detect feed type")

const (
	// maxXMLDepth bounds how deep the elements of a feed may nest. 実際の
	// フィードは十数段で収まるが、エスケープせずに HTML を書いた本文の閉じていない
	// 要素 (<br> など) は、外側の終わりのタグで閉じるまで数える。そうした要素を
	// 数千個まで含む本文も読める値にする (gofeed は数に関わらず読めた)。
	maxXMLDepth = 4096
	// maxXMLAttrs bounds the attributes of one element.
	maxXMLAttrs = 256
	// maxXMLDeclDepth bounds how many elements that declare namespaces may be
	// open at once.
	maxXMLDeclDepth = 256
	// maxXMLBaseLen bounds one resolved xml:base, and maxXMLBaseTotal the
	// resolved ones open at once, in bytes after percent-encoding (ASCII でない
	// 文字は、UTF-8 の 1 バイトが 3 バイトに数えられる)。パーマリンクを xml:base にする
	// フィードもあるので、長い URL も入る値にする。超えたものは解決に使わない。
	// maxXMLBaseResolvedPerByte bounds the lengths of all the xml:base resolved
	// in a feed, per byte of the feed.
	maxXMLBaseLen             = 4096
	maxXMLBaseTotal           = 64 << 10
	maxXMLBaseResolvedPerByte = 8
	// maxXMLNamespaces bounds the distinct namespace URIs declared in a whole
	// feed. 実際のフィードは多くても 20 前後。同じ URI を要素ごとに宣言し直すのは
	// よくある (Atom の xhtml の本文の div) ので、宣言の数ではなく URI の数で数える。
	maxXMLNamespaces = 64
	// maxFeedParseTime bounds the time spent parsing one feed (and reading its
	// dates). 手元で作った 1MiB の実物に近いフィード (RSS 2.0 と Atom) の解析は
	// 数十 ms で終わる。
	maxFeedParseTime = 500 * time.Millisecond
)

// feedParseTime returns maxFeedParseTime, scaled up in -race builds.
//
// -race の下では解析が 10 倍前後遅くなるので、上限も同じだけ延ばす (本番のバイナリは
// -race で作らない)。延ばさないと、大きな入力を通すテストが上限に当たって揺れる。
func feedParseTime() time.Duration { return maxFeedParseTime * raceSlowdown }

// parseFeed detects the type of body and parses it within maxFeedParseTime.
func parseFeed(body []byte) (*feed, error) {
	return parseFeedUntil(body, time.Now().Add(feedParseTime()))
}

// parseFeedUntil is parseFeed that stops with errFeedParseTimeLimit after
// deadline.
func parseFeedUntil(body []byte, deadline time.Time) (*feed, error) {
	switch detectFeedType(body) {
	case "rss":
		if err := checkXMLShape(body, deadline); err != nil {
			return nil, err
		}
		return parseRSS(body, deadline)
	case "atom":
		if err := checkXMLShape(body, deadline); err != nil {
			return nil, err
		}
		return parseAtom(body, deadline)
	case "json":
		return parseJSONFeed(body, deadline)
	}
	return nil, errFeedTypeNotDetected
}

// detectFeedType returns "rss", "atom", "json" or "" like gofeed's detector.
func detectFeedType(body []byte) string {
	i := 0
loop:
	for ; i < len(body); i++ {
		switch body[i] {
		case ' ', '\r', '\n', '\t':
		case 0xFE, 0xFF, 0x00, 0xEF, 0xBB, 0xBF: // byte order marks
		default:
			break loop
		}
	}
	if i == len(body) {
		return ""
	}
	switch body[i] {
	case '<':
		p := newPullParser(bytes.NewReader(body[i:]))
		if err := p.findRoot(); err != nil {
			return ""
		}
		switch strings.ToLower(p.name) {
		case "rdf", "rss":
			return "rss"
		case "feed":
			return "atom"
		}
	case '{':
		if json.Valid(body[i:]) {
			return "json"
		}
	}
	return ""
}

// checkXMLShape walks the whole document once and rejects one that nests too
// deep, has too many attributes on an element or declares too many distinct
// namespaces, before it is parsed.
func checkXMLShape(body []byte, deadline time.Time) error {
	return checkXMLShapeWith(body, deadline, maxXMLDeclDepth)
}

// checkXMLShapeWith is checkXMLShape with maxDeclDepth for maxXMLDeclDepth.
func checkXMLShapeWith(body []byte, deadline time.Time, maxDeclDepth int) error {
	d := newXMLDecoder(bytes.NewReader(body))
	depth, tokens := 0, 0
	// declares は開いている要素ごとに名前空間を宣言したか、declOpen はその数。
	var declares []bool
	declOpen := 0
	namespaces := map[string]struct{}{}
	for {
		if overDeadline(deadline, &tokens) {
			return errFeedParseTimeLimit
		}
		// 解析と同じ Token で読む (閉じ忘れの要素を閉じる数え方を揃える)。
		tok, err := d.Token()
		if err != nil {
			// 構文の誤りは、ここでは問わない。gofeed と同じく、解析の側が途中まで
			// 読んで決める (根の後ろの誤りなどは、解析では読まずに済むことがある)。
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxXMLDepth {
				return errors.New("feed nests too deep")
			}
			if len(t.Attr) > maxXMLAttrs {
				return errors.New("feed element has too many attributes")
			}
			declared := false
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" || a.Name.Local == "xmlns" {
					// 解析の側 (trackNamespaces) と同じく、前後の空白を落とした URI で数える。
					namespaces[strings.TrimSpace(a.Value)] = struct{}{}
					declared = true
				}
			}
			declares = append(declares, declared)
			if declared {
				if declOpen++; declOpen > maxDeclDepth {
					return errors.New("feed nests too many namespace declarations")
				}
			}
			if len(namespaces) > maxXMLNamespaces {
				return errors.New("feed declares too many namespaces")
			}
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
			if n := len(declares); n > 0 {
				if declares[n-1] {
					declOpen--
				}
				declares = declares[:n-1]
			}
		}
	}
}

// --- RSS ---

func parseRSS(body []byte, deadline time.Time) (*feed, error) {
	p := newPullParser(bytes.NewReader(body))
	p.deadline = deadline
	p.dates = newDateReader(deadline)
	p.resolveBudget = maxXMLBaseResolvedPerByte * len(body)
	if err := p.findRoot(); err != nil {
		return nil, err
	}
	if p.expect(evStartTag, "rss") != nil && p.expect(evStartTag, "rdf") != nil {
		return nil, fmt.Errorf("expected rss or rdf, got %q", p.name)
	}
	var channel *rssChannel
	var image *feedImage
	var rootItems []*rssItem
	for {
		ev, err := p.nextTag()
		if err != nil {
			return nil, err
		}
		if ev == evEndTag {
			break
		}
		if p.isExtension() {
			if err := p.skip(); err != nil {
				return nil, err
			}
			continue
		}
		switch strings.ToLower(p.name) {
		case "channel":
			if channel, err = parseRSSChannel(p); err != nil {
				return nil, err
			}
		case "item":
			it, err := parseRSSItem(p)
			if err != nil {
				return nil, err
			}
			rootItems = append(rootItems, it)
		case "textinput":
			if err := parseRSSChildren(p, "title", "description", "name", "link"); err != nil {
				return nil, err
			}
		case "image":
			if image, err = parseRSSImage(p); err != nil {
				return nil, err
			}
		default:
			if err := p.skip(); err != nil {
				return nil, err
			}
		}
	}
	if p.expect(evEndTag, "rss") != nil && p.expect(evEndTag, "rdf") != nil {
		return nil, fmt.Errorf("expected the end of rss or rdf, got %q", p.name)
	}
	if channel == nil {
		channel = &rssChannel{}
	}
	channel.items = append(channel.items, rootItems...)
	if image != nil {
		channel.image = image
	}
	return channel.translate(p.dates)
}

type rssChannel struct {
	title, description, link string
	image                    *feedImage
	ext                      extensions
	items                    []*rssItem
}

type rssItem struct {
	title, description, content, link, author, pubDate string
	pubDateParsed                                      *time.Time
	guid                                               string
	categories                                         []string
	enclosures                                         []*feedEnclosure
	ext                                                extensions
}

func parseRSSChannel(p *pullParser) (*rssChannel, error) {
	if err := p.expect(evStartTag, "channel"); err != nil {
		return nil, err
	}
	c := &rssChannel{ext: extensions{}}
	for {
		ev, err := p.nextTag()
		if err != nil {
			return nil, err
		}
		if ev == evEndTag {
			break
		}
		name := strings.ToLower(p.name)
		if p.isExtension() {
			if err := p.parseExtension(c.ext); err != nil {
				return nil, err
			}
			continue
		}
		switch name {
		case "title":
			c.title, err = p.parseText()
		case "description":
			c.description, err = p.parseText()
		case "link":
			c.link, err = parseRSSLink(p)
		case "language", "copyright", "managingeditor", "webmaster", "pubdate", "lastbuilddate",
			"generator", "docs", "ttl", "rating", "category":
			// 応答には使わないが、gofeed と同じく文字として読む (読めなければ失敗する)。
			_, err = p.parseText()
		case "skiphours":
			err = parseRSSChildren(p, "hour")
		case "skipdays":
			err = parseRSSChildren(p, "day")
		case "textinput":
			err = parseRSSChildren(p, "title", "description", "name", "link")
		case "item":
			var it *rssItem
			if it, err = parseRSSItem(p); err == nil {
				c.items = append(c.items, it)
			}
		case "cloud":
			_, err = p.nextTag()
			if err == nil {
				err = p.expect(evEndTag, "cloud")
			}
		case "image":
			c.image, err = parseRSSImage(p)
		default:
			err = p.skip()
		}
		if err != nil {
			return nil, err
		}
	}
	return c, p.expect(evEndTag, "channel")
}

func parseRSSItem(p *pullParser) (*rssItem, error) {
	if err := p.expect(evStartTag, "item"); err != nil {
		return nil, err
	}
	it := &rssItem{ext: extensions{}}
	for {
		ev, err := p.nextTag()
		if err != nil {
			return nil, err
		}
		if ev == evEndTag {
			break
		}
		name := strings.ToLower(p.name)
		if p.isExtension() {
			if err := p.parseExtension(it.ext); err != nil {
				return nil, err
			}
			continue
		}
		switch name {
		case "title":
			it.title, err = p.parseText()
		case "description":
			it.description, err = p.parseText()
		case "encoded":
			if p.prefixForNamespace(strings.TrimSpace(p.space)) == "content" {
				it.content, err = p.parseText()
			}
		case "link":
			it.link, err = parseRSSLink(p)
		case "author":
			it.author, err = p.parseText()
		case "comments":
			_, err = p.parseText()
		case "pubdate":
			if it.pubDate, err = p.parseText(); err == nil {
				if d, derr := p.dates.parse(it.pubDate); derr == nil {
					u := d.UTC()
					it.pubDateParsed = &u
				}
			}
		case "source":
			_, err = p.parseText()
			if err == nil {
				err = p.expect(evEndTag, "source")
			}
		case "enclosure":
			enc := &feedEnclosure{URL: p.attribute("url"), Length: p.attribute("length"), Type: p.attribute("type")}
			it.enclosures = append(it.enclosures, enc)
			err = skipToEndOf(p, "enclosure")
		case "guid":
			if it.guid, err = p.parseText(); err == nil {
				err = p.expect(evEndTag, "guid")
			}
		case "category":
			var v string
			if v, err = p.parseText(); err == nil {
				it.categories = append(it.categories, v)
				err = p.expect(evEndTag, "category")
			}
		default:
			// gofeed は知らない要素も文字として読み、失敗しても続ける。
			_, _ = p.parseText()
			err = nil
		}
		if err != nil {
			return nil, err
		}
	}
	return it, p.expect(evEndTag, "item")
}

// skipToEndOf consumes tokens up to the end tag named name (gofeed's
// enclosure handling).
func skipToEndOf(p *pullParser, name string) error {
	for {
		ev, err := p.next()
		if err != nil {
			return err
		}
		if ev == evEndDocument {
			return io.ErrUnexpectedEOF
		}
		if ev == evEndTag && p.name == name {
			return nil
		}
	}
}

func parseRSSLink(p *pullParser) (string, error) {
	href := p.attribute("href")
	u, err := p.parseText()
	if err != nil {
		return "", err
	}
	if u == "" && href != "" {
		u = href
	}
	return u, nil
}

func parseRSSImage(p *pullParser) (*feedImage, error) {
	if err := p.expect(evStartTag, "image"); err != nil {
		return nil, err
	}
	img := &feedImage{}
	for {
		ev, err := p.nextTag()
		if err != nil {
			return nil, err
		}
		if ev == evEndTag {
			break
		}
		switch strings.ToLower(p.name) {
		case "url":
			img.URL, err = p.parseText()
		case "title":
			img.Title, err = p.parseText()
		case "link", "width", "height", "description":
			_, err = p.parseText()
		default:
			err = p.skip()
		}
		if err != nil {
			return nil, err
		}
	}
	return img, p.expect(evEndTag, "image")
}

// parseRSSChildren reads the children of the current element named in names
// as text and skips the others, like gofeed (textInput, skipHours and skipDays
// are not used in the response, but how they are read changes xml:base).
func parseRSSChildren(p *pullParser, names ...string) error {
	name := p.name
	for {
		ev, err := p.nextTag()
		if err != nil {
			return err
		}
		if ev == evEndTag {
			break
		}
		if slices.Contains(names, strings.ToLower(p.name)) {
			_, err = p.parseText()
		} else {
			err = p.skip()
		}
		if err != nil {
			return err
		}
	}
	return p.expect(evEndTag, name)
}

// translate turns the channel into the response. It stops with
// errFeedParseTimeLimit at the deadline of dates (dc:date is read here).
func (c *rssChannel) translate(dates *dateReader) (*feed, error) {
	f := &feed{}
	itunes := c.ext["itunes"]
	dc := c.ext["dc"]

	f.Title = c.title
	if f.Title == "" && dc != nil && dc.all("title") != nil {
		f.Title = dc.first("title")
	}
	f.Description = c.description
	if f.Description == "" && itunes != nil {
		f.Description = itunes.first("summary")
	}
	f.Link = c.link
	if f.Link == "" && itunes != nil {
		f.Link = itunes.first("subtitle")
	}
	for _, key := range []string{"atom", "atom10", "atom03"} {
		for _, l := range c.ext[key]["link"] {
			if l.attrs["rel"] == "self" {
				f.FeedLink = l.attrs["href"]
			}
		}
	}
	f.Image = c.feedImage(itunes)
	if itunes != nil {
		f.ITunes = newITunesFeed(itunes)
	}
	f.Items = make([]*feedItem, 0, len(c.items))
	for _, it := range c.items {
		if dates.expired() {
			return nil, errFeedParseTimeLimit
		}
		f.Items = append(f.Items, it.translate(dates))
	}
	return f, nil
}

// feedImage picks the feed image like gofeed, except that it does not look
// for an <img> in the description HTML.
func (c *rssChannel) feedImage(itunes extGroup) *feedImage {
	if c.image != nil {
		return &feedImage{URL: c.image.URL, Title: c.image.Title}
	}
	if itunes != nil {
		if v := itunes["image"]; len(v) > 0 && v[0].attrs["href"] != "" {
			return &feedImage{URL: v[0].attrs["href"]}
		}
	}
	for _, m := range c.ext["media"]["content"] {
		if strings.HasPrefix(m.attrs["type"], "image/") || m.attrs["medium"] == "image" {
			return &feedImage{URL: m.attrs["url"]}
		}
	}
	return nil
}

func newITunesFeed(m extGroup) *itunesFeed {
	f := &itunesFeed{
		Author:   m.first("author"),
		Summary:  m.first("summary"),
		Explicit: m.first("explicit"),
		Keywords: m.first("keywords"),
	}
	if v := m["image"]; len(v) > 0 {
		f.Image = v[0].attrs["href"]
	}
	for _, cat := range m["category"] {
		c := &itunesCategory{Text: cat.attrs["text"]}
		if subs := cat.children["category"]; len(subs) > 0 {
			c.Subcategory = &itunesCategory{Text: subs[0].attrs["text"]}
		}
		f.Categories = append(f.Categories, c)
	}
	if v := m["owner"]; len(v) > 0 {
		f.Owner = &itunesOwner{}
		if n := v[0].children["name"]; len(n) > 0 {
			f.Owner.Name = n[0].value
		}
		if e := v[0].children["email"]; len(e) > 0 {
			f.Owner.Email = e[0].value
		}
	}
	return f
}

func (it *rssItem) translate(dates *dateReader) *feedItem {
	out := &feedItem{}
	itunes := it.ext["itunes"]
	dc := it.ext["dc"]

	out.Title = it.title
	if out.Title == "" && dc != nil && dc.all("title") != nil {
		out.Title = dc.first("title")
	}
	out.Description = it.description
	if out.Description == "" {
		if dc != nil && dc.all("description") != nil {
			out.Description = dc.first("description")
		} else if itunes != nil {
			out.Description = itunes.first("summary")
		}
	}
	out.Content = it.content
	out.Link = it.link
	out.GUID = it.guid

	out.Published = it.pubDate
	out.PublishedParsed = it.pubDateParsed
	if dc != nil && dc.all("date") != nil {
		date := dc.first("date")
		if out.Published == "" {
			out.Published = date
		}
		if out.PublishedParsed == nil {
			if d, err := dates.parse(date); err == nil {
				out.PublishedParsed = &d
			}
		}
	}

	var author string
	switch {
	case it.author != "":
		author = it.author
	case dc != nil && dc.all("author") != nil:
		author = dc.first("author")
	case dc != nil && dc.all("creator") != nil:
		author = dc.first("creator")
	case itunes != nil && itunes.first("author") != "":
		author = itunes.first("author")
	}
	if author != "" {
		name, email := parseNameAddress(author)
		out.Author = &feedPerson{Name: name, Email: email}
		out.Authors = []*feedPerson{out.Author}
	}
	if dc != nil {
		out.DCCreator = dc.all("creator")
	}

	var cats []string
	cats = append(cats, it.categories...)
	if itunes != nil && itunes.first("keywords") != "" {
		cats = append(cats, strings.Split(itunes.first("keywords"), ",")...)
	}
	if dc != nil {
		cats = append(cats, dc.all("subject")...)
	}
	if len(cats) > 0 {
		out.Categories = cats
	}
	if len(it.enclosures) > 0 {
		out.Enclosures = it.enclosures
	}
	return out
}

// --- Atom ---

// atomURIElements are the Atom elements whose text is resolved against
// xml:base.
var atomURIElements = map[string]bool{"icon": true, "id": true, "logo": true, "uri": true, "url": true}

type atomLink struct {
	href, rel, typ, length string
}

type atomEntry struct {
	title, summary, content, id string
	published, updated          string
	publishedParsed             *time.Time
	updatedParsed               *time.Time
	authors                     []*feedPerson
	categories                  []string
	links                       []*atomLink
}

func parseAtom(body []byte, deadline time.Time) (*feed, error) {
	p := newPullParser(bytes.NewReader(body))
	p.deadline = deadline
	p.dates = newDateReader(deadline)
	p.resolveBudget = maxXMLBaseResolvedPerByte * len(body)
	if err := p.findRoot(); err != nil {
		return nil, err
	}
	if err := p.expect(evStartTag, "feed"); err != nil {
		return nil, err
	}
	f := &feed{Items: []*feedItem{}}
	var links []*atomLink
	var logo, icon string
	ext := extensions{}
	for {
		ev, err := p.nextTag()
		if err != nil {
			return nil, err
		}
		if ev == evEndTag {
			break
		}
		name := strings.ToLower(p.name)
		if p.isExtension() {
			if err := p.parseExtension(ext); err != nil {
				return nil, err
			}
			continue
		}
		switch name {
		case "title":
			f.Title, err = parseAtomText(p)
		case "subtitle", "tagline":
			f.Description, err = parseAtomText(p)
		case "id", "updated", "modified", "rights", "copyright", "generator":
			_, err = parseAtomText(p)
			if err == nil && name == "generator" {
				err = p.expect(evEndTag, "generator")
			}
		case "icon":
			icon, err = parseAtomText(p)
		case "logo":
			logo, err = parseAtomText(p)
		case "link":
			var l *atomLink
			if l, err = parseAtomLink(p); err == nil {
				links = append(links, l)
			}
		case "author", "contributor":
			_, err = parseAtomPerson(p, name)
		case "category":
			_, err = parseAtomCategory(p)
		case "entry":
			var e *atomEntry
			if e, err = parseAtomEntry(p); err == nil {
				f.Items = append(f.Items, e.translate())
			}
		default:
			err = p.skip()
		}
		if err != nil {
			return nil, err
		}
	}
	if err := p.expect(evEndTag, "feed"); err != nil {
		return nil, err
	}
	if l := firstAtomLink(links, "alternate"); l != nil {
		f.Link = l.href
	}
	if l := firstAtomLink(links, "self"); l != nil {
		f.FeedLink = l.href
	}
	if logo != "" {
		f.Image = &feedImage{URL: logo}
	} else if icon != "" {
		f.Image = &feedImage{URL: icon}
	}
	return f, nil
}

func parseAtomEntry(p *pullParser) (*atomEntry, error) {
	if err := p.expect(evStartTag, "entry"); err != nil {
		return nil, err
	}
	e := &atomEntry{}
	ext := extensions{}
	for {
		ev, err := p.nextTag()
		if err != nil {
			return nil, err
		}
		if ev == evEndTag {
			break
		}
		name := strings.ToLower(p.name)
		if p.isExtension() {
			if err := p.parseExtension(ext); err != nil {
				return nil, err
			}
			continue
		}
		switch name {
		case "title":
			e.title, err = parseAtomText(p)
		case "id":
			e.id, err = parseAtomText(p)
		case "rights", "copyright":
			_, err = parseAtomText(p)
		case "summary":
			e.summary, err = parseAtomText(p)
		case "source":
			err = parseAtomSource(p)
		case "updated", "modified":
			if e.updated, err = parseAtomText(p); err == nil {
				if d, derr := p.dates.parse(e.updated); derr == nil {
					u := d.UTC()
					e.updatedParsed = &u
				}
			}
		case "published", "issued":
			if e.published, err = parseAtomText(p); err == nil {
				if d, derr := p.dates.parse(e.published); derr == nil {
					u := d.UTC()
					e.publishedParsed = &u
				}
			}
		case "author":
			var a *feedPerson
			if a, err = parseAtomPerson(p, name); err == nil {
				e.authors = append(e.authors, a)
			}
		case "contributor":
			_, err = parseAtomPerson(p, name)
		case "category":
			var c string
			if c, err = parseAtomCategory(p); err == nil {
				e.categories = append(e.categories, c)
			}
		case "link":
			var l *atomLink
			if l, err = parseAtomLink(p); err == nil {
				e.links = append(e.links, l)
			}
		case "content":
			e.content, err = parseAtomText(p)
		default:
			err = p.skip()
		}
		if err != nil {
			return nil, err
		}
	}
	return e, p.expect(evEndTag, "entry")
}

func (e *atomEntry) translate() *feedItem {
	it := &feedItem{
		Title:           e.title,
		Description:     e.summary,
		Content:         e.content,
		GUID:            e.id,
		Published:       e.published,
		PublishedParsed: e.publishedParsed,
		UpdatedParsed:   e.updatedParsed,
	}
	if it.Published == "" {
		it.Published = e.updated
	}
	if it.PublishedParsed == nil {
		it.PublishedParsed = e.updatedParsed
	}
	if l := firstAtomLink(e.links, "alternate"); l != nil {
		it.Link = l.href
	}
	if len(e.authors) > 0 {
		it.Authors = e.authors
		it.Author = &feedPerson{Name: e.authors[0].Name, Email: e.authors[0].Email}
	}
	if e.categories != nil {
		it.Categories = e.categories
	}
	for _, l := range e.links {
		if l.rel == "enclosure" {
			it.Enclosures = append(it.Enclosures, &feedEnclosure{URL: l.href, Length: l.length, Type: l.typ})
		}
	}
	return it
}

func parseAtomSource(p *pullParser) error {
	if err := p.expect(evStartTag, "source"); err != nil {
		return err
	}
	ext := extensions{}
	for {
		ev, err := p.nextTag()
		if err != nil {
			return err
		}
		if ev == evEndTag {
			break
		}
		name := strings.ToLower(p.name)
		if p.isExtension() {
			if err := p.parseExtension(ext); err != nil {
				return err
			}
			continue
		}
		switch name {
		case "title", "id", "updated", "modified", "subtitle", "tagline", "icon", "logo", "rights", "copyright":
			_, err = parseAtomText(p)
		case "generator":
			if _, err = parseAtomText(p); err == nil {
				err = p.expect(evEndTag, "generator")
			}
		case "link":
			_, err = parseAtomLink(p)
		case "author", "contributor":
			_, err = parseAtomPerson(p, name)
		case "category":
			_, err = parseAtomCategory(p)
		default:
			err = p.skip()
		}
		if err != nil {
			return err
		}
	}
	return p.expect(evEndTag, "source")
}

func parseAtomPerson(p *pullParser, name string) (*feedPerson, error) {
	if err := p.expect(evStartTag, name); err != nil {
		return nil, err
	}
	person := &feedPerson{}
	for {
		ev, err := p.nextTag()
		if err != nil {
			return nil, err
		}
		if ev == evEndTag {
			break
		}
		switch strings.ToLower(p.name) {
		case "name":
			person.Name, err = parseAtomText(p)
		case "email":
			person.Email, err = parseAtomText(p)
		case "uri", "url", "homepage":
			_, err = parseAtomText(p)
		default:
			err = p.skip()
		}
		if err != nil {
			return nil, err
		}
	}
	return person, p.expect(evEndTag, name)
}

func parseAtomLink(p *pullParser) (*atomLink, error) {
	if err := p.expect(evStartTag, "link"); err != nil {
		return nil, err
	}
	l := &atomLink{href: p.attribute("href"), typ: p.attribute("type"), length: p.attribute("length"), rel: p.attribute("rel")}
	if l.rel == "" {
		l.rel = "alternate"
	}
	if err := p.skip(); err != nil {
		return nil, err
	}
	return l, p.expect(evEndTag, "link")
}

// parseAtomCategory returns the label of a category, or its term.
func parseAtomCategory(p *pullParser) (string, error) {
	if err := p.expect(evStartTag, "category"); err != nil {
		return "", err
	}
	v := p.attribute("label")
	if v == "" {
		v = p.attribute("term")
	}
	if err := p.skip(); err != nil {
		return "", err
	}
	return v, p.expect(evEndTag, "category")
}

func firstAtomLink(links []*atomLink, rel string) *atomLink {
	for _, l := range links {
		if l.rel == rel {
			return l
		}
	}
	return nil
}

// parseAtomText reads an Atom text construct as gofeed does, but without
// parsing HTML: the wrapping <div> of html / xhtml content is removed at the
// XML level, and relative URLs inside the HTML are left as they are.
func parseAtomText(p *pullParser) (string, error) {
	var text struct {
		Type     string `xml:"type,attr"`
		Mode     string `xml:"mode,attr"`
		InnerXML string `xml:",innerxml"`
	}
	base := p.baseTop()
	if err := p.decodeElement(&text); err != nil {
		return "", err
	}
	result := strings.TrimSpace(text.InnerXML)
	lowerType := strings.ToLower(text.Type)
	lowerMode := strings.ToLower(text.Mode)

	switch {
	case strings.Contains(result, cdataStart):
		result = stripCDATA(result)
	case lowerType == "text" || strings.HasPrefix(lowerType, "text/") || (lowerType == "" && lowerMode == ""):
		result = decodeEntities(result)
	case strings.Contains(lowerType, "xhtml"):
		result = stripWrappingDiv(result)
	case lowerType == "html":
		result = decodeEntities(stripWrappingDiv(result))
	default:
		if decoded, err := base64.StdEncoding.DecodeString(result); err == nil {
			result = string(decoded)
		}
	}

	if atomURIElements[strings.ToLower(p.name)] {
		if resolved, err := xmlBaseResolveURL(base, result); err == nil && resolved != nil {
			result = resolved.String()
		}
	}
	return result, nil
}

// stripWrappingDiv returns the inner XML of s when s is a single <div> element
// (text around it may exist, as in gofeed), and s otherwise.
func stripWrappingDiv(s string) string {
	d := xml.NewDecoder(strings.NewReader(s))
	d.Strict = false
	var divStart, divEnd int64 = -1, -1
	depth := 0
	elements := 0
	isDiv := false
	for {
		before := d.InputOffset()
		tok, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				elements++
				// 接頭辞の付いた div (`xhtml:div`) は外さない (gofeed も HTML として
				// 読んで別の名前の要素にしていた)。
				isDiv = t.Name.Space == "" && strings.EqualFold(t.Name.Local, "div")
				divStart = d.InputOffset()
			}
			depth++
		case xml.EndElement:
			depth--
			if depth == 0 {
				divEnd = before
			}
			if depth < 0 {
				return s
			}
		}
	}
	if elements != 1 || !isDiv || depth != 0 || divStart < 0 || divEnd < divStart {
		return s
	}
	return requoteStartTags(s[divStart:divEnd])
}

// requoteStartTags rewrites the attributes of every start tag in s with double
// quotes, leaving everything else as it is.
//
// gofeed は外側の div を外した後に HTML として書き直していたので、属性はいつも
// 二重引用符になっていた。単一引用符の属性がそのまま残ると値は同じでも文字列が
// 変わるので、属性だけ同じ形に揃える (本家の rss-parser も XML として書き直し、
// 二重引用符にする)。
func requoteStartTags(s string) string {
	d := xml.NewDecoder(strings.NewReader(s))
	d.Strict = false
	// HTML の実体参照 (`&nbsp;` など) も解く。gofeed は HTML として読み直していたので、
	// 属性の値の中の実体参照は 1 回解かれていた。
	d.Entity = xml.HTMLEntity
	var b strings.Builder
	last := int64(0)
	skipEnd := false
	for {
		before := d.InputOffset()
		tok, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s
		}
		after := d.InputOffset()
		switch t := tok.(type) {
		case xml.StartElement:
			b.WriteString(s[last:before])
			b.WriteByte('<')
			b.WriteString(rawName(t.Name))
			for _, a := range t.Attr {
				b.WriteByte(' ')
				b.WriteString(rawName(a.Name))
				b.WriteString(`="`)
				b.WriteString(escapeAttr(a.Value))
				b.WriteByte('"')
			}
			if strings.HasSuffix(s[before:after], "/>") {
				b.WriteString("/>")
				skipEnd = true
			} else {
				b.WriteByte('>')
			}
			last = after
		case xml.EndElement:
			if skipEnd {
				// 空要素 (<img/>) の終わりは RawToken が補ったもので、入力には無い。
				skipEnd = false
				last = after
			}
		}
	}
	b.WriteString(s[last:])
	return b.String()
}

func rawName(n xml.Name) string {
	if n.Space == "" {
		return n.Local
	}
	return n.Space + ":" + n.Local
}

// escapeAttr escapes an attribute value the way x/net/html renders one, as
// gofeed's output did.
var attrEscaper = strings.NewReplacer(`&`, "&amp;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;", `"`, "&#34;", "\r", "&#13;")

func escapeAttr(v string) string { return attrEscaper.Replace(v) }

// --- JSON Feed ---

// The JSON Feed types declare every field gofeed declared, including the ones
// the response does not use, so that a value of the wrong type fails the
// parse as it did with gofeed.
type jsonFeed struct {
	Version     string        `json:"version"`
	Title       string        `json:"title"`
	HomePageURL string        `json:"home_page_url"`
	FeedURL     string        `json:"feed_url"`
	Description string        `json:"description"`
	UserComment string        `json:"user_comment"`
	NextURL     string        `json:"next_url"`
	Icon        string        `json:"icon"`
	Favicon     string        `json:"favicon"`
	Author      *jsonAuthor   `json:"author"`
	Expired     bool          `json:"expired"`
	Items       []*jsonItem   `json:"items"`
	Authors     []*jsonAuthor `json:"authors"`
	Language    string        `json:"language"`
}

type jsonItem struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	ExternalURL   string            `json:"external_url"`
	Title         string            `json:"title"`
	ContentHTML   string            `json:"content_html"`
	ContentText   string            `json:"content_text"`
	Summary       string            `json:"summary"`
	Image         string            `json:"image"`
	BannerImage   string            `json:"banner_image"`
	DatePublished string            `json:"date_published"`
	DateModified  string            `json:"date_modified"`
	Author        *jsonAuthor       `json:"author"`
	Tags          []string          `json:"tags"`
	Attachments   *[]jsonAttachment `json:"attachments"`
	Authors       []*jsonAuthor     `json:"authors"`
	Language      string            `json:"language"`
}

type jsonAuthor struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Avatar string `json:"avatar"`
}

type jsonAttachment struct {
	URL               string `json:"url"`
	MimeType          string `json:"mime_type"`
	Title             string `json:"title"`
	SizeInBytes       int64  `json:"size_in_bytes"`
	DurationInSeconds int64  `json:"duration_in_seconds"`
}

// parseJSONFeed reads a JSON Feed. It stops with errFeedParseTimeLimit after
// deadline (the dates are what takes time).
func parseJSONFeed(body []byte, deadline time.Time) (*feed, error) {
	dates := newDateReader(deadline)
	var jf jsonFeed
	if err := json.Unmarshal(body, &jf); err != nil {
		return nil, err
	}
	f := &feed{Title: jf.Title, Description: jf.Description, Link: jf.HomePageURL, FeedLink: jf.FeedURL, Items: []*feedItem{}}
	if jf.Icon != "" {
		f.Image = &feedImage{URL: jf.Icon}
	}
	for _, ji := range jf.Items {
		if dates.expired() {
			return nil, errFeedParseTimeLimit
		}
		if ji == nil {
			ji = &jsonItem{}
		}
		it := &feedItem{
			GUID:        ji.ID,
			Link:        ji.URL,
			Title:       ji.Title,
			Description: ji.Summary,
			Published:   ji.DatePublished,
		}
		it.Content = ji.ContentHTML
		if it.Content == "" {
			it.Content = ji.ContentText
		}
		if ji.DatePublished != "" {
			if d, err := dates.parse(ji.DatePublished); err == nil {
				it.PublishedParsed = &d
			}
		}
		if ji.DateModified != "" {
			if d, err := dates.parse(ji.DateModified); err == nil {
				it.UpdatedParsed = &d
			}
		}
		if ji.Author != nil {
			name, email := parseNameAddress(ji.Author.Name)
			it.Author = &feedPerson{Name: name, Email: email}
		}
		if ji.Authors != nil {
			it.Authors = []*feedPerson{}
			for _, a := range ji.Authors {
				var n string
				if a != nil {
					n = a.Name
				}
				name, email := parseNameAddress(n)
				it.Authors = append(it.Authors, &feedPerson{Name: name, Email: email})
			}
		} else if it.Author != nil {
			it.Authors = []*feedPerson{it.Author}
		}
		if len(ji.Tags) > 0 {
			it.Categories = ji.Tags
		}
		if ji.Attachments != nil {
			for _, a := range *ji.Attachments {
				it.Enclosures = append(it.Enclosures, &feedEnclosure{URL: a.URL, Type: a.MimeType, Length: fmt.Sprintf("%d", a.DurationInSeconds)})
			}
		}
		f.Items = append(f.Items, it)
	}
	return f, nil
}
