package secfetch

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFromOtherSite(t *testing.T) {
	for value, want := range map[string]bool{
		"":            false,
		"none":        false,
		"same-origin": false,
		"same-site":   true,
		"cross-site":  true,
		"bogus":       false,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if value != "" {
			r.Header.Set("Sec-Fetch-Site", value)
		}
		assert.Equal(t, want, FromOtherSite(r), "Sec-Fetch-Site %q", value)
	}
}
