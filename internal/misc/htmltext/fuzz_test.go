package htmltext

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/misc/jsstr"
)

// FuzzPlainText checks that reading the text of remote HTML never panics,
// ends, keeps valid UTF-8 valid, and gives text with its white space already
// collapsed: no ends to trim and no runs of spaces (#4116).
func FuzzPlainText(f *testing.F) {
	for _, s := range []string{
		`<p>hello <b>world</b></p>`, `a<br>b&lt;br&gt;c`, `<script>x</script>y<style>z</style>`,
		`<p>  a   b  </p>`, `&amp;&#x1F600;&#0;&nbsp;`, `<a href="x">l</a>ink`, `<p>unclosed <b`, ``,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 64<<10 {
			t.Skip()
		}
		var out string
		done := make(chan struct{})
		go func() {
			defer close(done)
			out = PlainText(in)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("did not return")
		}
		if utf8.ValidString(in) && !utf8.ValidString(out) {
			t.Fatalf("valid UTF-8 %q gave %q", in, out)
		}
		if strings.Contains(out, "  ") {
			t.Fatalf("%q gave a run of spaces: %q", in, out)
		}
		if out != jsstr.Trim(out) {
			t.Fatalf("%q gave untrimmed text: %q", in, out)
		}
	})
}
