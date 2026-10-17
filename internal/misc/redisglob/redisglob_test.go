package redisglob

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEscape(t *testing.T) {
	assert.Equal(t, `a\*b\?c\[d\]e\\f:`, Escape(`a*b?c[d]e\f:`))
	assert.Equal(t, "example.tld:3000:", Escape("example.tld:3000:"))
	assert.Equal(t, "", Escape(""))
	assert.Equal(t, "日本語", Escape("日本語"))
}
