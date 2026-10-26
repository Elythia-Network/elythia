package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestJSTruthy pins jsTruthy to JavaScript's truthiness of the JSON values.
func TestJSTruthy(t *testing.T) {
	for raw, want := range map[string]bool{
		``: false, `null`: false, `false`: false, `""`: false,
		`0`: false, `-0`: false, `0.0`: false, `-0.0`: false, `0e5`: false, `1e-400`: false,
		`true`: true, `"a"`: true, `"0"`: true, `1`: true, `-1`: true, `0.5`: true, `1e400`: true,
		`[]`: true, `{}`: true, `[0]`: true,
	} {
		assert.Equal(t, want, jsTruthy(json.RawMessage(raw)), "%q", raw)
	}
}
