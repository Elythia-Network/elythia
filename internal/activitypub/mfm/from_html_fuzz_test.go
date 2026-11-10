package mfm

import (
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzFromHTML checks that turning a remote note's HTML into MFM never panics,
// ends, keeps valid UTF-8 valid, and gives the same text each time (#4116).
func FuzzFromHTML(f *testing.F) {
	for _, s := range []string{
		`<p>hello <a href="https://remote.example/@a" class="u-url mention">@<span>a</span></a></p>`,
		`<p><span class="h-card"><a href="https://e.example/users/b">@b</a></span> <a href="https://e.example/tags/x" rel="tag">#x</a></p>`,
		`<p>a<br>b<br/>c<br />d</p><p>e</p>`, `<blockquote><p>q</p></blockquote><pre><code>x &lt; y</code></pre>`,
		`<ruby>漢<rp>(</rp><rt>かん</rt><rp>)</rp></ruby>`, `<a href="javascript:alert(1)">x</a>`,
		`<b><i><s><small><del>nest</del></small></s></i></b>`, `<ol><li>a<ul><li>b</li></ul></li></ol>`,
		`<p>unclosed <a href=`, `&amp;&#x1F600;&#0;&nbsp;`, `<script>x</script><style>y</style>`, ``,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 64<<10 {
			t.Skip()
		}
		var a, b string
		done := make(chan struct{})
		go func() {
			defer close(done)
			a, b = FromHTML(in), FromHTML(in)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("did not return")
		}
		if a != b {
			t.Fatalf("not deterministic: %q / %q", a, b)
		}
		if utf8.ValidString(in) && !utf8.ValidString(a) {
			t.Fatalf("valid UTF-8 %q gave %q", in, a)
		}
	})
}
