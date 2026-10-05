package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/core/mediaproxy"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

// TestParseOutputFormat covers the rules for picking WebP vs AVIF (#637 M3).
func TestParseOutputFormat(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		accept string
		want   mediaproxy.OutputFormat
	}{
		{"default", "", "", mediaproxy.FormatWebP},
		{"explicit query", "?avif=1", "", mediaproxy.FormatAVIF},
		{"accept image/avif", "", "image/avif", mediaproxy.FormatAVIF},
		{"accept multi", "", "image/webp,image/avif,*/*", mediaproxy.FormatAVIF},
		{"accept avif q=0", "", "image/avif;q=0", mediaproxy.FormatWebP},
		{"accept only webp", "", "image/webp,*/*;q=0.5", mediaproxy.FormatWebP},
		{"empty Accept", "", "", mediaproxy.FormatWebP},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/proxy/test"+tc.query, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := httptest.NewRecorder()
			ctx := echo.New().NewContext(req, rec)
			assert.Equal(t, tc.want, parseOutputFormat(ctx))
		})
	}
}

func TestAcceptsAVIF(t *testing.T) {
	cases := map[string]bool{
		"image/avif":                      true,
		"image/avif;q=0.8":                true,
		"image/avif; q=0":                 false,
		"image/webp":                      false,
		"image/webp,image/avif":           true,
		"text/html,application/xhtml+xml": false,
		"":                                false,
		"image/AVIF":                      true, // case-insensitive
	}
	for accept, want := range cases {
		assert.Equal(t, want, acceptsAVIF(accept), "Accept=%q", accept)
	}
}
