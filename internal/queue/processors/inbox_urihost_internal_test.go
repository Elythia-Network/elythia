package processors

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// activity.id の host gate も保存側と同じ正規化で比べること。接続先が同じ
// authority の別綴り (全角英字・U+3002・soft hyphen) は同じ host として扱い、
// 別 authority は別のまま。
func TestURIHost_FoldsLikeStoredHost(t *testing.T) {
	assert.Equal(t, "evil.example", uriHost("https://ｅｖｉｌ.example/notes/1"))
	assert.Equal(t, "evil.example", uriHost("https://evil。example/notes/1"))
	assert.Equal(t, "evil.example", uriHost("https://EVIL.example:8443/notes/1"), "port は見ない")
	assert.Equal(t, "xn--eckve.example", uriHost("https://パイ.example/x"))
	assert.NotEqual(t, uriHost("https://evil.example/x"), uriHost("https://other.example/x"))
	assert.Equal(t, "", uriHost("/relative"))
	assert.Equal(t, "", uriHost("https://%zz/"))
}
