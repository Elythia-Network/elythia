package processors

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"absent", "", 0},
		{"seconds", "120", 120 * time.Second},
		{"padded", " 30 ", 30 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		// Duration へ直すときに溢れない (上限はブレーカー側で掛ける)。
		{"huge", "999999999999999999", 24 * time.Hour},
		{"beyond int64", "99999999999999999999999", 24 * time.Hour},
		{"beyond int64 negative", "-99999999999999999999999", 0},
		{"http date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"past date", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"garbage", "soon", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseRetryAfter(tt.in, now))
		})
	}
}
