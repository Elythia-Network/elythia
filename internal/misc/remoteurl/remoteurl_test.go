package remoteurl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWellFormedMedia(t *testing.T) {
	for _, raw := range []string{
		"https://remote.example/a.png",
		"http://remote.example/a.png",
		"https://remote.example:8443/media/a.png?x=1&y=2#f",
		"https://xn--r8jz45g.example/a.png",
	} {
		assert.True(t, WellFormedMedia(raw), raw)
	}
	for _, raw := range []string{
		"",
		"/a.png",
		"a.png",
		"//remote.example/a.png",
		"https:remote.example/a.png",
		"https:\\\\remote.example/a.png",
		"https://remote.example/a(1).png",
		"https://remote.example/a).png",
		"https://remote.example/a'.png",
		`https://remote.example/a".png`,
		"https://remote.example/a<b.png",
		"https://remote.example/a>b.png",
		"https://remote.example/a b.png",
		"https://remote.example/a\nb.png",
		"https://remote.example/a\x7fb.png",
		"ftp://remote.example/a.png",
		"javascript://remote.example/a.png",
		"data:image/png;base64,AAAA",
		"https://",
	} {
		assert.False(t, WellFormedMedia(raw), "%q", raw)
	}
}
