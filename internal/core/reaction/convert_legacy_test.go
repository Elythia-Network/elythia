package reaction_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/stretchr/testify/assert"
)

// TestConvertLegacy covers the read-time reaction conversion used by
// users/reactions (#1547): legacy aliases map to unicode and a local custom
// emoji ":name@.:" is decoded back to ":name:". Remote custom emoji and plain
// unicode pass through unchanged.
func TestConvertLegacy(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"like", "👍"},
		{"love", "❤"},
		{"star", "⭐"},
		{":smile@.:", ":smile:"},             // 局所 custom emoji の @. を除去
		{":smile:", ":smile:"},               // host 無しはそのまま
		{":smile@ex.com:", ":smile@ex.com:"}, // remote はそのまま
		{"👍", "👍"},                           // unicode はそのまま
		{"unknown", "unknown"},               // 未知 legacy はそのまま
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, reaction.ConvertLegacy(tc.in), "ConvertLegacy(%q)", tc.in)
	}
}
