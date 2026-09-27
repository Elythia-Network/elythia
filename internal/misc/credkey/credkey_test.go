package credkey

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNative(t *testing.T) {
	assert.Equal(t, "", Native(""))
	k := Native("secret-token")
	assert.Equal(t, "native:930bbdc51b6aed5c2a5678fd6e28dee7a05e8a4b643cfc0b4427c3efb86c0d94", k)
	assert.NotContains(t, k, "secret-token")
	assert.NotEqual(t, k, Native("other-token"))
}

// char(16) の埋め草付きで読み出した値と、埋め草なしの値が同じ鍵になる。
func TestNative_IgnoresTrailingSpaces(t *testing.T) {
	assert.Equal(t, Native("short"), Native("short           "))
	assert.Equal(t, "", Native("   "))
	assert.NotEqual(t, Native("short"), Native(" short"), "先頭の空白は区別する")
}

func TestAccessToken(t *testing.T) {
	assert.Equal(t, "", AccessToken(""))
	assert.Equal(t, "app:abc", AccessToken("abc"))
	// native と app の鍵空間は prefix で分かれており、衝突しない。
	assert.NotEqual(t, Native("abc"), AccessToken("abc"))
}
