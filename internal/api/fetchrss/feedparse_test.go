package fetchrss

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gofeedDifferences lists, per fixture, the JSON paths where the response is
// meant to differ from the *.golden.json files.
//
// testdata/feeds/*.golden.json は、gofeed v1.3.0 (golang.org/x/net v0.62.0) で読んだ
// ときの応答 (packFeed の結果) で、gofeed をやめる前のコミット (「Test fetchrss: 代表的な
// フィードの応答を、今の読み取り(gofeed)の応答と突き合わせる」) で gofeed で作った。
// fixture を足すときは、そのコミットで fixture を置いて -update-golden を付けて流すと、
// gofeed の応答で golden を書く (今の parseFeed は gofeed を使わないので、ここでは書かない)。
// ここに並べた違いは、gofeed が HTML を解析して作っていた値と、名前空間の宣言の
// 有効範囲の扱い (docs/divergence/api.md)。
var gofeedDifferences = map[string][]string{
	// 本文の HTML から探していたフィードの画像。本家も作らない。
	"rss2-html-image": {"image"},
	// xml:base で解決していた HTML の中の相対 URL。本家も解決しない。
	"atom-xmlbase": {"items[0].content"},
	// 値の無い属性。gofeed は空の値、今は名前と同じ値になる。
	"atom-xhtml-valueless": {"items[0].content"},
	// HTML の head へ移る要素の後ろの div。gofeed は外していた。
	"atom-html-head-before-div": {"items[0].content"},
	// xhtml の本文の中の文字。gofeed は HTML として書き直していた (' を &#39; に、
	// 文字参照を解く、タグ名を小文字に、など)。今は元の XML のまま。
	"atom-xhtml-text": {"items[0].content", "items[0].contentSnippet"},
	// 読み飛ばした要素の名前空間の宣言を、後ろの要素に持ち越さない (XML の
	// 宣言の有効範囲どおり)。gofeed は持ち越していたので、後ろの atom:link を
	// Atom の要素として読まなかった。
	"rss2-atom-prefix-on-skipped-element": {"feedUrl"},
	// 読み飛ばした要素を中に持つ要素の宣言も、その要素の後ろに持ち越さない。
	"rss2-atom-prefix-after-container": {"feedUrl"},
}

// The response for every fixture must match what gofeed returned, except for
// the paths listed in gofeedDifferences.
func TestParseFeed_MatchesGofeed(t *testing.T) {
	files, err := filepath.Glob("testdata/feeds/*")
	require.NoError(t, err)
	seen := 0
	used := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, ".golden.json") {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(f)
			require.NoError(t, err)
			want, err := os.ReadFile(strings.TrimSuffix(f, filepath.Ext(f)) + ".golden.json")
			require.NoError(t, err)
			got := []byte(`{"error":true}`)
			if parsed, err := parseFeed(body); err == nil {
				got, err = json.Marshal(packFeed(parsed))
				require.NoError(t, err)
			}
			var w, g any
			require.NoError(t, json.Unmarshal(want, &w))
			require.NoError(t, json.Unmarshal(got, &g))
			diffs := jsonDiffPaths("", w, g)
			sort.Strings(diffs)
			expected := append([]string(nil), gofeedDifferences[name]...)
			sort.Strings(expected)
			if len(expected) > 0 {
				used[name] = true
			}
			assert.Equal(t, expected, diffs, "got %s", got)
		})
		seen++
	}
	// 走査が空振りしても緑にならないよう、件数の下限を置く。
	assert.GreaterOrEqual(t, seen, 40)
	for name := range gofeedDifferences {
		assert.True(t, used[name], "gofeedDifferences has an entry for a missing fixture: %s", name)
	}
}

// jsonDiffPaths returns the paths where a and b differ.
func jsonDiffPaths(path string, a, b any) []string {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		var out []string
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			out = append(out, jsonDiffPaths(p, am[k], bm[k])...)
		}
		return out
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok && bok && len(as) == len(bs) {
		var out []string
		for i := range as {
			out = append(out, jsonDiffPaths(fmt.Sprintf("%s[%d]", path, i), as[i], bs[i])...)
		}
		return out
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		return []string{path}
	}
	return nil
}

// 入れ子の深さ、1 つの要素の属性の数、名前空間を宣言する要素の入れ子に上限があること。
// 上限ちょうどは通る。
func TestParseFeed_ShapeLimits(t *testing.T) {
	nest := func(n int) []byte {
		// rss・channel・item の 3 段に、残りを item の中の要素で積む。
		inner := strings.Repeat("<x>", n-3) + strings.Repeat("</x>", n-3)
		return []byte(`<rss version="2.0"><channel><item>` + inner + `</item></channel></rss>`)
	}
	_, err := parseFeed(nest(maxXMLDepth))
	require.NoError(t, err, "maxXMLDepth levels must be accepted")
	_, err = parseFeed(nest(maxXMLDepth + 1))
	require.Error(t, err, "one more level must be rejected")

	attrs := func(n int) []byte {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, ` a%d="v"`, i)
		}
		return []byte(`<feed xmlns="http://www.w3.org/2005/Atom"><entry` + b.String() + `><title>t</title></entry></feed>`)
	}
	_, err = parseFeed(attrs(256))
	require.NoError(t, err, "256 attributes must be accepted")
	_, err = parseFeed(attrs(257))
	require.Error(t, err, "257 attributes must be rejected")

	// 名前空間を宣言する要素の入れ子も、上限ちょうどは通る。
	decls := func(n int) []byte {
		inner := strings.Repeat(`<x xmlns:a="urn:a">`, n-1) + strings.Repeat("</x>", n-1)
		return []byte(`<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel><item>` + inner + `</item></channel></rss>`)
	}
	// 上限は、実際のフィードの入れ子 (数段) より十分に大きいこと。
	_, err = parseFeed(decls(64))
	require.NoError(t, err, "64 declaring elements must be accepted")
	_, err = parseFeed(decls(maxXMLDeclDepth))
	require.NoError(t, err, "maxXMLDeclDepth declaring elements must be accepted")
	_, err = parseFeed(decls(maxXMLDeclDepth + 1))
	require.Error(t, err, "one more must be rejected")
}

// 壊れた入力 (どこで切れても) で panic しないこと。
func TestParseFeed_TruncatedInputs(t *testing.T) {
	files, err := filepath.Glob("testdata/feeds/*")
	require.NoError(t, err)
	for _, f := range files {
		if strings.HasSuffix(f, ".golden.json") {
			continue
		}
		body, err := os.ReadFile(f)
		require.NoError(t, err)
		// 大きな fixture は間を空けて切る (どの fixture も 2000 か所以上で切る)。
		step := max(1, len(body)/2000)
		for i := 0; i <= len(body); i += step {
			assert.NotPanics(t, func() { _, _ = parseFeed(body[:i]) }, "%s cut at %d", f, i)
		}
	}
}

func TestDecodeEntities(t *testing.T) {
	cases := map[string]string{
		"a &amp; b":        "a & b",
		"&nbsp;&hellip;":   " …",
		"tail &":           "tail &",
		"no end &amp b":    "no end &amp b",
		"space &a b; c":    "space &a b; c",
		"&#x3042;&#12354;": "ああ",
	}
	for in, want := range cases {
		assert.Equal(t, want, decodeEntities(in), in)
	}
}

func TestStripCDATA(t *testing.T) {
	assert.Equal(t, "x", stripCDATA("<![CDATA[x]]>"))
	// gofeed と同じく、CDATA の前の文字は落ち、後ろは文字参照を解く。
	assert.Equal(t, "xb&", stripCDATA("a<![CDATA[x]]>b&amp;"))
	assert.Equal(t, "<![CDATA[open", stripCDATA("<![CDATA[open"))
}

func TestParseNameAddress(t *testing.T) {
	cases := []struct{ in, name, addr string }{
		{"", "", ""},
		{"a@b.example (Name)", "Name", "a@b.example"},
		{"Name (a@b.example)", "Name", "a@b.example"},
		{"Just Name", "Just Name", ""},
		{"a@b.example", "", "a@b.example"},
		{"a@b@c", "", ""},
	}
	for _, c := range cases {
		n, a := parseNameAddress(c.in)
		assert.Equal(t, c.name, n, c.in)
		assert.Equal(t, c.addr, a, c.in)
	}
}

func TestParseDate(t *testing.T) {
	_, err := newDateReader(time.Time{}).parse("  ")
	require.Error(t, err)
	d, err := newDateReader(time.Time{}).parse("Mon, 02 Jan 2006 15:04:05 +0900")
	require.NoError(t, err)
	assert.Equal(t, "2006-01-02T06:04:05Z", d.UTC().Format(time.RFC3339))
	d, err = newDateReader(time.Time{}).parse("Monday, 02 January 2006 15:04:05 EST")
	require.NoError(t, err)
	assert.Equal(t, "2006-01-02T20:04:05Z", d.UTC().Format(time.RFC3339), "a named zone is loaded")
	_, err = newDateReader(time.Time{}).parse("Mon, 02 Jan 2006 15:04:05 XYZ")
	require.NoError(t, err, "an unknown zone name is still parsed")
	_, err = newDateReader(time.Time{}).parse("garbage")
	require.Error(t, err)
}

func TestXMLBaseResolveURL(t *testing.T) {
	_, err := xmlBaseResolveURL(nil, "http://[::1")
	require.Error(t, err)
	u, err := xmlBaseResolveURL(nil, "a b")
	require.NoError(t, err)
	assert.Equal(t, "a%20b", u.String())
}

func TestCharsetReader(t *testing.T) {
	_, err := charsetReader("no-such-charset", strings.NewReader(""))
	require.Error(t, err)
}

func TestStripWrappingDiv(t *testing.T) {
	cases := map[string]string{
		`<div>a<b>c</b></div>`:                        `a<b>c</b>`,
		`<DIV><br/>x</DIV>`:                           `<br/>x`,
		`text <div>in</div> more`:                     `in`,
		`<div>a</div><div>b</div>`:                    `<div>a</div><div>b</div>`,
		`<p>not a div</p>`:                            `<p>not a div</p>`,
		`no element`:                                  `no element`,
		`<div>unclosed`:                               `<div>unclosed`,
		`</div>`:                                      `</div>`,
		`<div><img src='a.png' alt="q'"/></div>`:      `<img src="a.png" alt="q&#39;"/>`,
		`<div><x:y xmlns:x="u" x:a="1">t</x:y></div>`: `<x:y xmlns:x="u" x:a="1">t</x:y>`,
	}
	for in, want := range cases {
		assert.Equal(t, want, stripWrappingDiv(in), in)
	}
}

func TestDetectFeedType(t *testing.T) {
	assert.Equal(t, "", detectFeedType(nil))
	assert.Equal(t, "", detectFeedType([]byte("  \n ")))
	assert.Equal(t, "rss", detectFeedType([]byte("\xef\xbb\xbf <rss/>")))
	assert.Equal(t, "atom", detectFeedType([]byte("<feed/>")))
	assert.Equal(t, "rss", detectFeedType([]byte("<RDF/>")))
	assert.Equal(t, "", detectFeedType([]byte("<html/>")))
	assert.Equal(t, "", detectFeedType([]byte("<")))
	assert.Equal(t, "json", detectFeedType([]byte(`{"items":[]}`)))
	assert.Equal(t, "", detectFeedType([]byte(`{"items":`)))
}

// 文書全体で宣言する名前空間の URI の数に上限があること。同じ URI の宣言し直し
// (Atom の xhtml の本文でよくある) は数えない。
func TestParseFeed_NamespaceLimit(t *testing.T) {
	decls := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `<e xmlns:p%d="urn:%d"/>`, i, i)
		}
		return b.String()
	}
	_, err := parseFeed([]byte(`<rss version="2.0"><channel><title>t</title>` + decls(64) + `</channel></rss>`))
	require.NoError(t, err, "64 namespaces must be accepted")
	_, err = parseFeed([]byte(`<rss version="2.0"><channel><title>t</title>` + decls(65) + `</channel></rss>`))
	require.Error(t, err, "65 namespaces must be rejected")

	redeclare := strings.Repeat(`<entry><id>x</id><content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml">a</div></content></entry>`, 500)
	f, err := parseFeed([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"><title>t</title>` + redeclare + `</feed>`))
	require.NoError(t, err, "declaring the same namespace again must not count")
	assert.Len(t, f.Items, 500)
}

// 宣言を持たない要素は、親の名前空間の表を共有する (複写しない)。
func TestPullParser_SharesNamespaceTables(t *testing.T) {
	p := newPullParser(strings.NewReader(`<a xmlns:x="urn:x"><b><c xmlns:y="urn:y"><d/></c></b></a>`))
	var tables []map[string]string
	for {
		ev, err := p.nextToken()
		require.NoError(t, err)
		if ev == evEndDocument {
			break
		}
		if ev == evStartTag {
			tables = append(tables, p.spaces)
		}
	}
	require.Len(t, tables, 4)
	same := func(a, b map[string]string) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	assert.True(t, same(tables[0], tables[1]), "<b> declares nothing and must share <a>'s table")
	assert.False(t, same(tables[1], tables[2]), "<c> declares a namespace and needs its own table")
	assert.True(t, same(tables[2], tables[3]))
	assert.Equal(t, map[string]string{"urn:x": "x", "urn:y": "y"}, tables[3])
}

// 今の表と同じ宣言だけを持つ要素は、表を写さずに親の表を使う。
func TestPullParser_SameDeclarationSharesTable(t *testing.T) {
	p := newPullParser(strings.NewReader(`<a xmlns:x="urn:x" xmlns="urn:d"><b xmlns:x="urn:x" xmlns="urn:d"><c xmlns:x="urn:y"/><e xmlns:z="urn:x"/></b></a>`))
	var tables []map[string]string
	for {
		ev, err := p.nextToken()
		require.NoError(t, err)
		if ev == evEndDocument {
			break
		}
		if ev == evStartTag {
			tables = append(tables, p.spaces)
		}
	}
	require.Len(t, tables, 4)
	same := func(a, b map[string]string) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	assert.True(t, same(tables[0], tables[1]), "<b> repeats <a>'s declarations")
	assert.False(t, same(tables[1], tables[2]), "<c> declares a new URI")
	assert.Equal(t, map[string]string{"urn:x": "x", "urn:d": "", "urn:y": "x"}, tables[2])
	assert.False(t, same(tables[1], tables[3]), "<e> gives a URI another prefix")
	assert.Equal(t, map[string]string{"urn:x": "z", "urn:d": ""}, tables[3])
}

var feedPieces = strings.Fields(`<item> </item> <title>t</title> <title_xmlns:q="urn:q">t</title> <e_xmlns:a="urn:1">
	<e_xmlns:b="urn:1"> <e_xmlns:a="urn:1"_xmlns:c="urn:2"> <e> </e> <y_xml:base="b/"> <y_xml:base="https://ns.example/a/b/c/">
	<y_xml:base="../"> </y> text <!----> <dc:creator>c</dc:creator> <link>https://ns.example/</link>`)

// randomFeeds returns n RSS documents with up to maxPieces pieces each inside
// the channel (an "_" in a piece is a space).
func randomFeeds(seed int64, n, maxPieces int) []string {
	r := rand.New(rand.NewSource(seed))
	feeds := make([]string, n)
	for i := range feeds {
		var b strings.Builder
		b.WriteString(`<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel>`)
		for range 1 + r.Intn(maxPieces) {
			b.WriteString(strings.ReplaceAll(feedPieces[r.Intn(len(feedPieces))], "_", " "))
		}
		b.WriteString(`</channel></rss>`)
		feeds[i] = b.String()
	}
	return feeds
}

// While reading, the namespace tables on the stack are only the ones of the
// open elements that changed the table (and the first one), and the xml:base
// URLs stay within maxBaseLen each and maxBaseTotal together.
func TestPullParser_StacksWithinBounds(t *testing.T) {
	for _, limits := range [][3]int{{28, 1000, math.MaxInt}, {1000, 60, math.MaxInt}, {1000, 1000, 100}} {
		checkStacksWithinBounds(t, limits[0], limits[1], limits[2])
	}
}

func checkStacksWithinBounds(t *testing.T, maxBaseLen, maxBaseTotal, resolveBudget int) {
	t.Helper()
	capped := false
	str := func(u *url.URL) string {
		if u == nil {
			return ""
		}
		return strings.TrimSuffix(u.String(), "/")
	}
	for _, in := range randomFeeds(1, 3000, 40) {
		p := newPullParser(strings.NewReader(in))
		p.maxBaseLen, p.maxBaseTotal, p.resolveBudget = maxBaseLen, maxBaseTotal, resolveBudget
		// changed は開いている要素ごとに、その要素が表を替えたか。
		var changed []bool
		r := rand.New(rand.NewSource(int64(len(in))))
		for {
			ev, err := p.nextToken()
			if err != nil || ev == evEndDocument {
				break
			}
			switch ev {
			case evStartTag:
				changed = append(changed, p.spaces != nil && reflect.ValueOf(p.spaces).Pointer() != reflect.ValueOf(p.spacesBefore).Pointer())
				if r.Intn(3) == 0 {
					// 要素ごと読み飛ばす (終わりのタグは encoding/xml が読む)。
					if _, err := p.parseText(); err != nil {
						break
					}
					changed = changed[:len(changed)-1]
				}
			case evEndTag:
				if len(changed) > 0 {
					changed = changed[:len(changed)-1]
				}
			}
			open := 1
			for _, c := range changed {
				if c {
					open++
				}
			}
			tables := map[uintptr]bool{}
			for _, m := range p.spacesStack {
				tables[reflect.ValueOf(m).Pointer()] = true
			}
			require.LessOrEqual(t, len(tables), open, "%q", in)
			sum := 0
			for i, u := range p.baseStack {
				if u != nil {
					// 解決するときに Path の末尾へ "/" を足すことがあるので、1 バイトは許す。
					require.LessOrEqual(t, len(u.String()), p.maxBaseLen+1, "%q", in)
				}
				if p.baseSizes[i] == 0 {
					// 上限で、解決せずに下の段の写しを積んだ段。
					capped = true
					if i == 0 {
						require.Nil(t, u, "%q", in)
					} else {
						require.Equal(t, str(p.baseStack[i-1]), str(u), "%q", in)
						if u != nil {
							require.NotSame(t, p.baseStack[i-1], u, "%q", in)
						}
					}
				}
				require.LessOrEqual(t, p.baseSizes[i], p.maxBaseLen, "%q", in)
				sum += p.baseSizes[i]
			}
			require.Equal(t, sum, p.baseBytes)
			require.LessOrEqual(t, p.baseBytes, p.maxBaseTotal, "%q", in)
			require.LessOrEqual(t, p.resolved, p.resolveBudget, "%q", in)
		}
	}
	assert.True(t, capped, "the feeds must reach the xml:base limit (%d, %d, %d)", maxBaseLen, maxBaseTotal, resolveBudget)
}

// checkXMLShape rejects a feed with more elements that declare namespaces open
// at once than its limit, and accepts the others.
func TestCheckXMLShape_DeclarationDepth(t *testing.T) {
	const limit = 3
	rejected := false
	for _, in := range randomFeeds(2, 3000, 40) {
		d := newXMLDecoder(strings.NewReader(in))
		var declares []bool
		open, most := 0, 0
		for {
			tok, err := d.Token()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.StartElement:
				decl := false
				for _, a := range t.Attr {
					if a.Name.Space == "xmlns" || a.Name.Local == "xmlns" {
						decl = true
					}
				}
				declares = append(declares, decl)
				if decl {
					open++
					most = max(most, open)
				}
			case xml.EndElement:
				if n := len(declares); n > 0 {
					if declares[n-1] {
						open--
					}
					declares = declares[:n-1]
				}
			}
		}
		err := checkXMLShapeWith([]byte(in), time.Time{}, limit)
		if most > limit {
			rejected = true
			require.Error(t, err, "%q", in)
		} else {
			require.NoError(t, err, "%q", in)
		}
	}
	assert.True(t, rejected, "the feeds must reach the limit")
}

// 拡張要素の文字は、コメントなどで分かれていても続けてつなぐ。
func TestParseExtension_FragmentedText(t *testing.T) {
	body := func(n int) []byte {
		return []byte(`<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel><title>t</title><item><dc:creator>` +
			strings.Repeat("abcdefg<!---->", n) + `</dc:creator></item></channel></rss>`)
	}
	f, err := parseFeed(body(3))
	require.NoError(t, err)
	assert.Equal(t, []string{"abcdefgabcdefgabcdefg"}, f.Items[0].DCCreator)

}

// 拡張要素の属性と子の表は、持つときだけ作る。
func TestParseExtensionElement_MapsOnlyWhenUsed(t *testing.T) {
	read := func(src string) extension {
		p := newPullParser(strings.NewReader(src))
		require.NoError(t, p.findRoot())
		e, err := p.parseExtensionElement()
		require.NoError(t, err)
		return e
	}
	empty := read(`<a:x xmlns:a="urn:a"/>`)
	assert.Equal(t, map[string]string{"a": "urn:a"}, empty.attrs)
	assert.Nil(t, empty.children)
	bare := read(`<x>v</x>`)
	assert.Nil(t, bare.attrs)
	assert.Nil(t, bare.children)
	assert.Equal(t, "v", bare.value)
	full := read(`<x k="1"><y>c</y></x>`)
	assert.Equal(t, "1", full.attrs["k"])
	assert.Equal(t, "c", full.children["y"][0].value)
}

// 閉じていない <br> などを含むフィードを、深さの上限で拒否しないこと (解析と同じく
// 閉じ忘れの要素を閉じて数える)。
func TestParseFeed_UnclosedElementsAreNotTooDeep(t *testing.T) {
	items := strings.Repeat(`<item><title>t</title><description>a<br>b<p>c</description></item>`, 300)
	f, err := parseFeed([]byte(`<rss version="2.0"><channel><title>t</title>` + items + `</channel></rss>`))
	require.NoError(t, err)
	assert.Len(t, f.Items, 300)
}

// 解析に時間の上限があること。上限を過ぎていたら、解析の前の検査と解析のどちらでも
// 止まる。
func TestParseFeed_Deadline(t *testing.T) {
	items := strings.Repeat(`<item><title>t</title></item>`, 2000)
	body := []byte(`<rss version="2.0"><channel><title>t</title>` + items + `</channel></rss>`)
	past := time.Now().Add(-time.Second)
	_, err := parseFeedUntil(body, past)
	require.ErrorIs(t, err, errFeedParseTimeLimit)
	require.ErrorIs(t, checkXMLShape(body, past), errFeedParseTimeLimit)
	_, err = parseRSS(body, past)
	require.ErrorIs(t, err, errFeedParseTimeLimit)
	atom := []byte(`<feed xmlns="http://www.w3.org/2005/Atom"><title>t</title>` + strings.Repeat(`<entry><id>x</id></entry>`, 2000) + `</feed>`)
	_, err = parseAtom(atom, past)
	require.ErrorIs(t, err, errFeedParseTimeLimit)

	f, err := parseFeedUntil(body, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Len(t, f.Items, 2000)
}

// 時間帯の名前ごとに読み込んだ結果は、1 つのフィードを読む間 (dateReader ごと) だけ
// 覚え、別のフィードには持ち越さない。
func TestDateReader_LocationsPerFeed(t *testing.T) {
	r := newDateReader(time.Time{})
	loc, err := r.loadLocation("Asia/Tokyo")
	require.NoError(t, err)
	again, err := r.loadLocation("Asia/Tokyo")
	require.NoError(t, err)
	assert.Same(t, loc, again, "the second lookup must come from the reader's table")
	_, err = r.loadLocation("No/Such_Zone")
	require.Error(t, err)
	assert.Len(t, r.locations, 2)
	assert.Empty(t, newDateReader(time.Time{}).locations, "another feed must not see the table")
}

// 解析の後に日付を読む区間 (RSS の dc:date) と、JSON Feed の日付も、締め切りで止まる。
func TestParseFeed_DeadlineCoversDates(t *testing.T) {
	past := time.Now().Add(-time.Second)
	rss := []byte(`<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel>` +
		strings.Repeat(`<item><dc:date>not a date</dc:date></item>`, 3) + `</channel></rss>`)
	// 解析の中の締め切りの確認の間隔には届かない小ささにする。
	_, err := parseFeedUntil(rss, past)
	require.ErrorIs(t, err, errFeedParseTimeLimit)
	json := []byte(`{"version":"https://jsonfeed.org/version/1.1","items":[{"date_published":"x","date_modified":"y"}]}`)
	_, err = parseFeedUntil(json, past)
	require.ErrorIs(t, err, errFeedParseTimeLimit)

	f, err := parseFeedUntil(rss, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Len(t, f.Items, 3)
	f, err = parseFeedUntil(json, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Len(t, f.Items, 1)
}
