package mfm

import (
	"strings"
	"testing"
)

// FuzzWhatwgHref checks that whatwgHref never panics, only returns http(s)
// URLs, never returns characters that could end an HTML attribute or start a
// tag before escaping, and is idempotent.
//
// href は EscapeHTML を通してから書くが、正規化の結果そのものにも空白・制御文字・
// `<` `>` が残らないことを確かめておく。
func FuzzWhatwgHref(f *testing.F) {
	for _, s := range []string{
		"https://e.x/", "https://E.X:443/a/../b?c'd#e`f", "https://[::1]:8080/", "https://0x7f.1/",
		"https://例え.テスト/パス", "https://u@v:w@e.x/", "https://aא.x/", "https://xn--r8jz45g.x/",
		"javascript:alert(1)", "https://e.x\\a", "https://%45.x/", "https://[::ffff:1.2.3.4]/",
		"https://e.x/<a b>?<c d>#<e f>", "https://<u v>@e.x/", "https://e.x/\x7f\u0080",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, ok := whatwgHref(strings.ToValidUTF8(in, "\ufffd"))
		if !ok {
			if out != "" {
				t.Fatalf("failure with output %q", out)
			}
			return
		}
		if !strings.HasPrefix(out, "http://") && !strings.HasPrefix(out, "https://") {
			t.Fatalf("non-http output %q", out)
		}
		for i := 0; i < len(out); i++ {
			if c := out[i]; c <= 0x20 || c >= 0x7F || c == '<' || c == '>' {
				t.Fatalf("unsafe byte %q in %q", c, out)
			}
		}
		again, ok := whatwgHref(out)
		if !ok || again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q (%v)", in, out, again, ok)
		}
	})
}
