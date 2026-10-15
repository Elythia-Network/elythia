package middleware

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAcceptsEncoding(t *testing.T) {
	tests := []struct {
		header, coding string
		want           bool
	}{
		{"gzip, deflate, br, zstd", "br", true},
		{"gzip, deflate, br, zstd", "gzip", true},
		{"gzip, deflate", "br", false},
		{"", "gzip", false},
		{"GZIP", "gzip", true},
		{"br;q=0, gzip", "br", false},
		{"br; q=0.5", "br", true},
		{"br;q=0.0", "br", false},
		{"*", "br", true},
		{"*;q=0", "br", false},
		{"*, br;q=0", "br", false},
		{"br;q=0, *", "br", false},
		{"gzip;q=1;foo=bar", "gzip", true},
		{"br;q=x", "br", false},
		{"br;q=2", "br", false},
		{"xgzip", "gzip", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, AcceptsEncoding(tt.header, tt.coding), "%q / %s", tt.header, tt.coding)
	}
}

func TestEncodingQ(t *testing.T) {
	assert.InDelta(t, 0.5, EncodingQ("gzip, br;q=0.5", "br"), 1e-9)
	assert.InDelta(t, 1, EncodingQ("gzip, br;q=0.5", "gzip"), 1e-9)
	assert.InDelta(t, 0.2, EncodingQ("*;q=0.2", "br"), 1e-9)
	assert.InDelta(t, 0, EncodingQ("deflate", "br"), 1e-9)
}
