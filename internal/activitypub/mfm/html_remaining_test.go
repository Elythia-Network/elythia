package mfm

import (
	"testing"

	mfmparser "github.com/elythia-network/mfm"
	"github.com/stretchr/testify/assert"
)

// TestToHTML_RemainingDivergences checks the HTML and IsSimple results that
// the tree differences in #3329 changed.
func TestToHTML_RemainingDivergences(t *testing.T) {
	// 空のラベルのリンクは空の <a> にしない
	assert.Equal(t, `[](<a href="https://e.x/">https://e.x</a>)`, ToHTML(mfmparser.Parse("[](https://e.x)"), testHost))
	assert.True(t, IsSimple(mfmparser.Parse("`a\rb`")))
	assert.True(t, IsSimple(mfmparser.Parse("<plain></plain>")))
	assert.True(t, IsSimple(mfmparser.Parse("```\n\n```")))
}

// TestToHTML_EscapeMatchesUpstream fixes the HTML escaping to upstream
// MfmService's escapeHtml (& < > " ' to &amp; &lt; &gt; &quot; &#039;) in
// text, attributes and code (#3329).
func TestToHTML_EscapeMatchesUpstream(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a'b"c&d<e>f`, "a&#039;b&quot;c&amp;d&lt;e&gt;f"},
		{"`'\"&<>`", "<code>&#039;&quot;&amp;&lt;&gt;</code>"},
		{"```\n'\"\n```", "<pre><code>&#039;&quot;</code></pre>"},
		{`\('"\)`, "<code>&#039;&quot;</code>"},
		// href は new URL().href で作るので `"` は %22 になる
		{"<https://e.x/'a\"&>", `<a href="https://e.x/&#039;a%22&amp;">https://e.x/&#039;a&quot;&amp;</a>`},
		{"[x'](<https://e.x/'a'>)", `<a href="https://e.x/&#039;a&#039;">x&#039;</a>`},
		{"https://e.x/?a=1&b=2", `<a href="https://e.x/?a=1&amp;b=2">https://e.x/?a=1&amp;b=2</a>`},
		{"@a'", `<a href="https://` + testHost + `/@a" class="u-url mention">@a</a>&#039;`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, ToHTML(mfmparser.Parse(tc.in), testHost))
		})
	}
}
