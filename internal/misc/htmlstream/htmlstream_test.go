package htmlstream

import (
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

// parseTree writes the tree x/net/html's Parse builds, in the form events
// writes the events: the html, head and body elements are left out (their
// content stays), comments are left out.
func parseTree(t *testing.T, in string) string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(in))
	if err != nil {
		return "ERROR"
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				b.WriteString(c.Data)
			case html.ElementNode:
				if c.Namespace == "" && (c.Parent.Type == html.DocumentNode || c.Parent.DataAtom == 0x0 || c.Parent.Data == "html") &&
					(c.Data == "html" || c.Data == "head" || c.Data == "body") {
					walk(c)
					continue
				}
				writeStart(&b, c.Namespace, c.Data, c.Attr)
				walk(c)
				b.WriteString("</>")
			}
		}
	}
	walk(doc)
	return b.String()
}

func writeStart(b *strings.Builder, ns, name string, attrs []html.Attribute) {
	b.WriteString("<" + ns + ":" + name)
	for _, a := range attrs {
		b.WriteString(" " + a.Namespace + ":" + a.Key + "=" + a.Val)
	}
	b.WriteString(">")
}

// events writes the events of in, and returns the scanner's count of places
// where they are known to differ from x/net/html's tree.
func events(in string) (string, int) {
	s := NewScanner(strings.NewReader(in))
	defer func() { loopLimited += s.loopLimited }()
	var b strings.Builder
	for {
		switch s.Next() {
		case EOF:
			if s.Err() != nil {
				return "ERROR", s.approximated
			}
			return b.String(), s.approximated
		case Text:
			b.WriteString(s.Text())
		case StartElement:
			e := s.Element()
			writeStart(&b, e.Namespace, e.Name, e.Attr)
		case EndElement:
			b.WriteString("</>")
		}
	}
}

// loopLimited sums Scanner.loopLimited over the calls of events.
var loopLimited int

var cases = []string{
	``, `text`, `  lead`, `<p>a<p>b`, `<ul><li>a<li>b</ul>`, `<dl><dt>a<dd>b<dt>c</dl>`,
	`</p>x`, `a</p>b`, `</br>`, `<br/>x<img src=a>y`, `<div/>x`, `<a href=x/>y`,
	`<p>a<div>b</div>c</p>`, `<h1>a<h2>b</h2>`, "<pre>\nx</pre>", "<pre>\r\nx</pre>", "<listing>\nx</listing>",
	"<textarea>\ny</textarea>", "<pre><!---->\nx</pre>", `<script>a<b>c</script>d`, `<title>t&amp;</title>x`,
	`<style>s</style>`, `<noscript><p>n</p></noscript>`, `<xmp><b></xmp>`, `<iframe><p></iframe>`, `<plaintext><b>x`,
	"a\x00b<pre>\x00\nc</pre>", `<svg>a` + "\x00" + `b</svg>`, `<svg><title><b>t</b></title></svg>`, `<svg><foreignObject><p>x</p></foreignObject></svg>`,
	`<svg><a xlink:href=x>l</a></svg>`, `<svg><path/>x</svg>`, `<math><mi>x</mi><mtext><b>y</b></mtext></math>`,
	`<math><annotation-xml encoding="text/html"><p>z</p></annotation-xml></math>`, `<svg><![CDATA[c<d]]></svg>`, `<p><![CDATA[e]]></p>`,
	`<svg><p>break</p></svg>`, `<svg><font color=red>f</font></svg>`, `<svg><g></SVG>x`,
	`<b>1<p>2</b>3`, `<b><i>x</b>y</i>z`, `<p><b>x</p>y`, `<a href=1>x<a href=2>y</a>`, `<b>x</b></b>y`,
	`<b><b><b><b>x</b></b></b></b>`, `<p><b><b><b><b>x</p>y`, `<p><b c=1><b c=2>x</p>y`, `<nobr>a<nobr>b`,
	`<table><tr><td>a</td></tr></table>`, `<table><td>a<td>b</table>`, `<table><caption>c</caption><colgroup><col></colgroup><tbody><tr><th>h</table>`,
	`<table><tr><td><table><tr><td>x</table></table>`, `<table><tr><td>a</tr>b`, `<table>x</table>`, `<table><tr>x</tr></table>`,
	`<td>x</td>`, `<p><table>t</table>`, `<!DOCTYPE html><p><table>t</table>`, `<!DOCTYPE HTML><p><table><td>t</table>`, `<!DOCTYPE html SYSTEM "about:legacy-compat"><p><table><td>t</table>`, `<p><table><td>t</table>`, `<!DOCTYPE html><p><table><td>t</table>`, `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "x"><p><table><td>t</table>`, `<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN"><p><table><td>t</table>`, `<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN" "x"><p><table><td>t</table>`, `<!DOCTYPE html PUBLIC "-//IETF//DTD HTML//EN" "x"><p><table><td>t</table>`, `<!DOCTYPE html SYSTEM "http://www.ibm.com/data/dtd/v11/ibmxhtml1-transitional.dtd"><p><table><td>t</table>`, `<!DOCTYPE html PUBLIC><p><table><td>t</table>`, `<!DOCTYPE html junk><p><table><td>t</table>`, `<!DOCTYPE html PUBLIC "x><p><table><td>t</table>`, `<!DOCTYPE html PUBLIC "html"><p><table><td>t</table>`, `<!DOCTYPE html PUBLIC x><p><table><td>t</table>`, `<!doctype html><title>x</title>`,
	`<html><head><title>t</title></head><body><p>b</p></body></html>`, `<head></head> <title>x</title> y`, `</head> <meta> x`,
	`<body><p>x</body>y`, `<p>x</html>y`, `x<!--c-->y`, `<frameset><p>x`, `<template><p>t</p></template>x`, `<p><template>t</template>`,
	`<select><option>a<option>b</select>`, `<select><select>x`, `<option>a<optgroup>b`, `<ruby>a<rt>b<rp>c</ruby>`,
	`<form><form>x</form>y`, `<form><b>x</form>y`, `<button>a<button>b`, `<hr>`, `<p>a<hr>b`, `<image src=x>`,
	`<object>o<b>x</object>y`, `<marquee>m</marquee>`, `<input type=hidden>`, `<table><input type=hidden></table>`,
	`<x-y>a</x-y>`, `<x-y>a</x-z>b`, `<span>a</div>b`, `<A HREF=X>Y</A>`,
	`<!--a--><html><!--b--><head><!--c--><base></head><!--d--><body>x`, `<html><html><head><head><title>t</title></head>`,
	`</div><!doctype html>x`, `<html></p>x`, `<head></br>x`, `<head></x>y</head>z`, `<head><template>t</template>x`,
	`</head></head>x`, `</body>x`, `</html>x`, `<p>a</body><!--c--> <p>b</html><!--d--> <html>c`, `<p>x</html></html>y`,
	`<table><colgroup> <col><!--c--></colgroup><col><template>t</template></table>`, `<table><colgroup><p>x</table>`,
	`<table><caption>c<td>d</table>`, `<table><caption>c</table>e`, `<table><tbody><caption>c</tbody></table>`,
	`<table><tr><td>a<caption>b</table>`, `<table><tr><td>a</tbody>b</table>`, `<table><thead><tr><td>a</thead><tfoot>b</table>`,
	`<table><tbody></tr></td>a</table>`, `<table><tr></td></caption>a</table>`, `<table><td></body></caption>a</table>`,
}

var pieces = strings.Fields(`<p> </p> <b> </b> <i> </i> <a_href=x> </a> <br> </br> <div> </div> <span> </span> <ul> <li> </li>
		</ul> <blockquote> </blockquote> <pre> </pre> <code> </code> <h1> </h1> <h2> <s> </s> <small> <strong> </strong>
		<em> text &amp; <img> <hr> <x-y> </x-y> <!--c--> <title>t</title> <meta> <dl> <dt> <dd> <ruby> <rt> <u> <font>
		<nobr> </nobr> <button> </button> <form> </form> <svg> </svg> <math> </math> <foreignObject> <mi> <desc>
		<script>x</script> <textarea>z</textarea> <body> </body> </html> <head> </head> <select> <option> </select>
		<table> </table> <tr> </tr> <td> </td> <th> <tbody> </tbody> <caption> </caption> <colgroup> <col> <template> </template>`)

// randomDocs returns n documents of up to maxPieces pieces each (an "_" in a
// piece is a space).
func randomDocs(seed int64, n, maxPieces int, pieces []string) []string {
	r := rand.New(rand.NewSource(seed))
	docs := make([]string, n)
	for i := range docs {
		var b strings.Builder
		for range 1 + r.Intn(maxPieces) {
			b.WriteString(strings.ReplaceAll(pieces[r.Intn(len(pieces))], "_", " "))
		}
		docs[i] = b.String()
	}
	return docs
}

// The events nest the way x/net/html builds the tree, except where the scanner
// counts a known difference.
func TestScanner_NestsLikeParse(t *testing.T) {
	compared := 0
	check := func(in string) {
		got, approx := events(in)
		if approx > 0 {
			return
		}
		compared++
		if want := parseTree(t, in); got != want {
			t.Errorf("%q:\n got  %q\n want %q", in, got, want)
		}
	}
	for _, in := range cases {
		check(in)
	}
	for _, in := range randomDocs(1, 20000, 16, pieces) {
		check(in)
	}
	// 比べずに飛ばしたものばかりでは何も確かめていないので、下限を置く。
	assert.Greater(t, compared, 15000)
	// 処理が終わらずに捨てたトークンは無い (近似として飛ばした入力も含めて)。
	assert.Zero(t, loopLimited)
}

// Elements nested deeper than x/net/html accepts end the events with
// ErrTooDeep, at the same depth where Parse fails.
func TestScanner_TooDeep(t *testing.T) {
	for _, tc := range []struct {
		in  string
		err bool
	}{
		{strings.Repeat("<span>", 510) + "x", false},
		// 中身の無い要素も、いったん開くので 1 段に数える。
		{strings.Repeat("<span>", 510) + "<br>", true},
		{strings.Repeat("<span>", 511) + "x", true},
		{"<title>t</title>" + strings.Repeat("<span>", 510) + "x", false},
		{"<title>t</title>" + strings.Repeat("<span>", 511) + "x", true},
	} {
		got, _ := events(tc.in)
		assert.Equal(t, tc.err, got == "ERROR", "%.40q", tc.in[len(tc.in)-20:])
		assert.Equal(t, parseTree(t, tc.in) == "ERROR", got == "ERROR", "x/net/html disagrees")
	}
	s := NewScanner(strings.NewReader(strings.Repeat("<div>", 600) + "<a href=x>"))
	for s.Next() != EOF {
	}
	assert.ErrorIs(t, s.Err(), ErrTooDeep)
}

type failingReader struct{ n int }

var errRead = errors.New("read failed")

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, errRead
	}
	r.n--
	return copy(p, "<p>a</p>"), nil
}

// An error from the reader ends the events and is returned by Err (Parse
// fails on it too), and Next keeps returning EOF after that.
func TestScanner_ReadError(t *testing.T) {
	s := NewScanner(&failingReader{n: 3})
	for s.Next() != EOF {
	}
	assert.ErrorIs(t, s.Err(), errRead)
	assert.Equal(t, EOF, s.Next())

	s = NewScanner(strings.NewReader("<p>a"))
	for s.Next() != EOF {
	}
	assert.NoError(t, s.Err())
	_, err := html.Parse(&failingReader{n: 3})
	assert.ErrorIs(t, err, errRead)
}

func count(in string, k Kind, name string) int {
	s := NewScanner(strings.NewReader(in))
	n := 0
	for {
		got := s.Next()
		if got == EOF {
			return n
		}
		if got == k && s.Element().Name == name {
			n++
		}
	}
}

// An end tag whose element is further than maxScopeSearch open elements down
// is ignored (x/net/html closes it).
func TestScanner_SearchLimit(t *testing.T) {
	near := "<x-a>" + strings.Repeat("<span>", maxScopeSearch-2) + "</x-a>x"
	got, _ := events(near)
	assert.Equal(t, parseTree(t, near), got)
	assert.True(t, strings.HasSuffix(got, "</>x"), got)

	far := "<x-a>" + strings.Repeat("<span>", maxScopeSearch) + "</x-a>x"
	got, _ = events(far)
	assert.True(t, strings.HasSuffix(got, "x"+strings.Repeat("</>", maxScopeSearch+1)), "the end tag closed the element: %s", got)
	assert.NotEqual(t, parseTree(t, far), got)
}

// Resetting the insertion mode after a table closes also looks at most at
// maxScopeSearch open elements: a cell further down is not found, and the mode
// becomes the body's. (The events do not change: everything a cell's mode does
// would search below that depth too.)
func TestScanner_ResetSearchLimit(t *testing.T) {
	mode := func(n int) mode {
		s := NewScanner(strings.NewReader("<table><tr><td>" + strings.Repeat("<span>", n) + "<table></table>"))
		for s.Next() != EOF {
		}
		require.NoError(t, s.Err())
		return s.im
	}
	assert.Equal(t, inCellIM, mode(maxScopeSearch-4))
	assert.Equal(t, inBodyIM, mode(maxScopeSearch))
}

// Elements taken off the stack while elements opened in them are still open
// (a closed form) count toward maxScopeSearch, so a search never looks at more
// than that many entries.
func TestScanner_SearchLimitCountsRemovedElements(t *testing.T) {
	n := maxScopeSearch/2 + 4
	in := "<x-a>" + strings.Repeat("<form><span></form>", n) + "</x-a>x"
	got, _ := events(in)
	assert.True(t, strings.HasSuffix(got, "x"+strings.Repeat("</>", 2*n+1)), "the end tag closed the element: %s", got)
	// 外した form を数えない x/net/html は閉じる。数えなければ同じになる。
	small := "<x-a>" + strings.Repeat("<form><span></form>", 4) + "</x-a>x"
	got, _ = events(small)
	assert.Equal(t, parseTree(t, small), got)
}

// What the scanner does stays within what it read: the attributes of the
// reopened formatting elements add up to at most the bytes read and are all
// counted, and the scanner reads each start tag's attributes at most once.
func TestScanner_WorkWithinInput(t *testing.T) {
	more := append(append([]string{}, pieces...),
		`<a_href="https://e.example/`+strings.Repeat("x", 40)+`">`, `<b_class=c_id=d>`,
		`<math><annotation-xml_encoding=x_k=v>`, `<math><annotation-xml_encoding=text/html>`)
	reopenedSome := false
	// 閉じ忘れたリンクを段落ごとに開き直す短い文書を、ランダムな文書の前に置く。
	docs := append([]string{`<p><a href="https://e.example/a/b">x<p>y<p>z<p>w`}, randomDocs(2, 5000, 60, more)...)
	for _, in := range docs {
		s := NewScanner(strings.NewReader(in))
		carried := 0
		for s.Next() != EOF {
			if s.cur.kind == StartElement {
				for _, at := range s.Element().Attr {
					carried += len(at.Namespace) + len(at.Key) + len(at.Val)
				}
			}
		}
		if s.reconstructed > 0 {
			reopenedSome = true
		}
		require.LessOrEqual(t, s.reopened, s.read, "%q", in)
		require.LessOrEqual(t, carried, s.attrBytesInTokens+s.reopened, "%q", in)
		require.LessOrEqual(t, s.attrsRead, s.attrsInTokens, "%q", in)
	}
	assert.True(t, reopenedSome, "the documents must reopen formatting elements")
}

// White space read after the head is kept in a Builder and put out as one text
// when the body starts.
func TestScanner_HoldsWhiteSpaceUntilBody(t *testing.T) {
	s := NewScanner(strings.NewReader("<title>t</title></head> \n<!--c-->\n<p>x"))
	var texts []string
	for s.Next() != EOF {
		if s.cur.kind == Text {
			texts = append(texts, s.Text())
		}
		if s.cur.kind == StartElement && s.Element().Name == "p" {
			assert.Zero(t, s.betweenHeadAndBody.Len(), "the white space must have been put out")
		}
	}
	assert.Equal(t, []string{"t", " \n\n", "x"}, texts)
}

// A token whose handling does not end within reprocessLimit is dropped and
// counted.
func TestScanner_CountsDroppedTokens(t *testing.T) {
	s := NewScanner(strings.NewReader("<p>x"))
	// 最初のトークンは、挿入モードを進めながら何度か処理し直す。
	s.reprocessLimit = 0
	for s.Next() != EOF {
	}
	assert.Positive(t, s.loopLimited)
}

// After an error, the events the same token would have added are not passed on.
func TestScanner_NoEventsAfterFailure(t *testing.T) {
	// 閉じた b を、510 段の中で開き直そうとして深さの上限に当たる。
	in := "<p><b></p>" + strings.Repeat("<div>", maxContentDepth) + "z"
	s := NewScanner(strings.NewReader(in))
	for s.Next() != EOF {
		assert.NotEqual(t, "z", s.Text(), "text after the failure")
	}
	assert.ErrorIs(t, s.Err(), ErrTooDeep)
}

// After maxReconstructed reopened formatting elements, a document reopens no
// more of them.
func TestScanner_ReconstructLimit(t *testing.T) {
	in := "<p><b>" + strings.Repeat("<p>x", maxReconstructed+100)
	assert.Equal(t, 1+maxReconstructed, count(in, StartElement, "b"))

	in = "<p><b>" + strings.Repeat("<p>x", 100)
	assert.Equal(t, 101, count(in, StartElement, "b"))
}

// The list of active formatting elements keeps at most maxActiveFormatting
// entries after the last marker, so at most that many are reopened at once.
func TestScanner_ActiveFormattingLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("<p>")
	for i := range maxActiveFormatting + 8 {
		b.WriteString("<b id=" + strings.Repeat("i", i+1) + ">")
	}
	b.WriteString("</p>x")
	in := b.String()
	assert.Equal(t, 2*maxActiveFormatting+8, count(in, StartElement, "b"))

	// 上限より少なければ、Parse の木と同じになる。
	small := "<p><b id=1><b id=2><b id=3></p>x"
	got, _ := events(small)
	assert.Equal(t, parseTree(t, small), got)
}

// The scanner counts the places where it does not follow x/net/html's tree: a
// formatting element closed while a block opened in it is still open, text or
// elements put directly in a table, template and frameset.
func TestScanner_CountsApproximations(t *testing.T) {
	for in, want := range map[string]int{
		`<b>1<p>2</b>3`:                            1,
		`<table><tr><td>x</td></tr>y</table>`:      1,
		`<b>1</b><p>2</p><table><tr><td>3</table>`: 0,
	} {
		_, got := events(in)
		assert.Equal(t, want, got, in)
	}
}

func TestElement_AttrVal(t *testing.T) {
	s := NewScanner(strings.NewReader(`<a HREF="x" rel=me data-x>`))
	require.Equal(t, StartElement, s.Next())
	e := s.Element()
	v, ok := e.AttrVal("href")
	assert.True(t, ok)
	assert.Equal(t, "x", v)
	assert.True(t, e.HasAttr("data-x"))
	_, ok = e.AttrVal("class")
	assert.False(t, ok)
	assert.False(t, e.InHead)

	s = NewScanner(strings.NewReader(`<meta name=a><p>b</p>`))
	require.Equal(t, StartElement, s.Next())
	assert.True(t, s.Element().InHead)
	require.Equal(t, EndElement, s.Next())
	require.Equal(t, StartElement, s.Next())
	assert.False(t, s.Element().InHead)
	require.Equal(t, Text, s.Next())
	assert.Equal(t, "b", s.Text())
	_ = io.EOF
}
