package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEncodeURIComponent compares the helper with the output of JavaScript's
// encodeURIComponent (as printed by Node.js) for the same inputs. The last
// unescaped group !'()* is what separates it from url.QueryEscape.
func TestEncodeURIComponent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abcXYZ019", "abcXYZ019"},
		{" ", "%20"},
		{"&", "%26"},
		{"+", "%2B"},
		{"/", "%2F"},
		{"?", "%3F"},
		{"#", "%23"},
		{"%", "%25"},
		{"=", "%3D"},
		{":", "%3A"},
		{"@", "%40"},
		{"$", "%24"},
		{",", "%2C"},
		{";", "%3B"},
		{"[", "%5B"},
		{"]", "%5D"},
		{"!'()*", "!'()*"},
		{"-_.~", "-_.~"},
		{"\"<>\\^`{|}", "%22%3C%3E%5C%5E%60%7B%7C%7D"},
		{"\t\n", "%09%0A"},
		{"日本語", "%E6%97%A5%E6%9C%AC%E8%AA%9E"},
		{"é", "%C3%A9"},
		{"😀", "%F0%9F%98%80"},
		{"👨‍👩‍👧", "%F0%9F%91%A8%E2%80%8D%F0%9F%91%A9%E2%80%8D%F0%9F%91%A7"},
		{"a b&c=d+e/f?g#h%i", "a%20b%26c%3Dd%2Be%2Ff%3Fg%23h%25i"},
		{"　", "%E3%80%80"},
		{"\u007f", "%7F"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, encodeURIComponent(c.in), "input %q", c.in)
	}
}

// TestSearch_MatchesUpstream checks the search node props against mfm-js 0.26.0
// parse() and the HTML against upstream MfmService.toHtml (search branch:
// href = escapeHtml("https://www.google.com/search?q=" + encodeURIComponent(query)),
// text = escapeHtml(content)).
func TestSearch_MatchesUpstream(t *testing.T) {
	cases := []struct {
		in, query, content, html string
	}{
		{"foo bar 検索", "foo bar", "foo bar 検索",
			`<a href="https://www.google.com/search?q=foo%20bar">foo bar 検索</a>`},
		{"a&b [search]", "a&b", "a&b [search]",
			`<a href="https://www.google.com/search?q=a%26b">a&amp;b [search]</a>`},
		{"hello search", "hello", "hello search",
			`<a href="https://www.google.com/search?q=hello">hello search</a>`},
		{"MFM 検索", "MFM", "MFM 検索",
			`<a href="https://www.google.com/search?q=MFM">MFM 検索</a>`},
		{"x [検索]", "x", "x [検索]",
			`<a href="https://www.google.com/search?q=x">x [検索]</a>`},
		{"Foo Search", "Foo", "Foo Search",
			`<a href="https://www.google.com/search?q=Foo">Foo Search</a>`},
		{"Foo SEARCH", "Foo", "Foo SEARCH",
			`<a href="https://www.google.com/search?q=Foo">Foo SEARCH</a>`},
		{"a/b?c#d%e+f 検索", "a/b?c#d%e+f", "a/b?c#d%e+f 検索",
			`<a href="https://www.google.com/search?q=a%2Fb%3Fc%23d%25e%2Bf">a/b?c#d%e+f 検索</a>`},
		{"日本語 😀 search", "日本語 😀", "日本語 😀 search",
			`<a href="https://www.google.com/search?q=%E6%97%A5%E6%9C%AC%E8%AA%9E%20%F0%9F%98%80">日本語 😀 search</a>`},
		// mfm-js は query の前後の空白を削らないので、URL にも空白が残る (#3325)
		{" q  search", " q ", " q  search",
			`<a href="https://www.google.com/search?q=%20q%20"> q  search</a>`},
		{"x\t[SEARCH]", "x", "x\t[SEARCH]",
			"<a href=\"https://www.google.com/search?q=x\">x\t[SEARCH]</a>"},
		// `'` は encodeURIComponent が残すので、href 側の HTML エスケープで、本家の
		// escapeHtml と同じ `&#039;` になる (#3329)。
		{"'a' search", "'a'", "'a' search",
			`<a href="https://www.google.com/search?q=&#039;a&#039;">&#039;a&#039; search</a>`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			nodes := Parse(c.in)
			if assert.Len(t, nodes, 1) && assert.Equal(t, NodeSearch, nodes[0].Type) {
				assert.Equal(t, c.query, nodes[0].Props["query"])
				assert.Equal(t, c.content, nodes[0].Props["content"])
			}
			assert.Equal(t, c.html, ToHTML(nodes, testHost))
		})
	}
}
