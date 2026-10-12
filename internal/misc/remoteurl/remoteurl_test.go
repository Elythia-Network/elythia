package remoteurl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMedia(t *testing.T) {
	for raw, want := range map[string]string{
		"https://remote.example/a.png":                      "https://remote.example/a.png",
		"http://remote.example/a.png":                       "http://remote.example/a.png",
		"https://remote.example:8443/media/a.png?x=1&y=2#f": "https://remote.example:8443/media/a.png?x=1&y=2#f",
		"https://xn--r8jz45g.example/a.png":                 "https://xn--r8jz45g.example/a.png",
		// 元のファイル名を残すサーバーの URL は、符号化して受け付ける。
		"https://remote.example/media/photo (1).jpg": "https://remote.example/media/photo%20%281%29.jpg",
		"https://remote.example/it's.png":            "https://remote.example/it%27s.png",
		`https://remote.example/"q".png`:             "https://remote.example/%22q%22.png",
	} {
		got, ok := Media(raw)
		assert.True(t, ok, raw)
		assert.Equal(t, want, got, raw)
	}
	for _, raw := range []string{
		"",
		"/a.png",
		"a.png",
		"//remote.example/a.png",
		"https:remote.example/a.png",
		"https:///remote.example/a.png",
		"https:\\\\remote.example/a.png",
		"https://remote.example/a\\b.png",
		"https://remote.example/a<b.png",
		"https://remote.example/a>b.png",
		"https://remote.example/a\nb.png",
		"https://remote.example/a\x7fb.png",
		"ftp://remote.example/a.png",
		"javascript://remote.example/a.png",
		"data:image/png;base64,AAAA",
		"https://",
		"x),url(https://remote.example/a.png",
	} {
		got, ok := Media(raw)
		assert.False(t, ok, "%q", raw)
		assert.Empty(t, got, "%q", raw)
	}
}
