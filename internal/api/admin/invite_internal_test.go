package admin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestParseJSDateTimeString pins the forms of ECMAScript's Date Time String
// Format that admin/invite/create accepts as upstream's Date.parse does
// (#3854), and the ones it rejects.
func TestParseJSDateTimeString(t *testing.T) {
	loc := time.FixedZone("test", 9*3600)
	accepted := []struct {
		in   string
		want time.Time
	}{
		// 同梱の frontend の datetime-local の値。オフセットが無いのでサーバーの時刻帯。
		{"2026-10-27T12:34", time.Date(2026, 10, 27, 3, 34, 0, 0, time.UTC)},
		{"2026-10-27T12:34:56", time.Date(2026, 10, 27, 3, 34, 56, 0, time.UTC)},
		// 日付だけは UTC。
		{"2026-10-27", time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)},
		{"2026-10", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{"2026", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"2024-02-29", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)},
		{"2026-10-27T12:34:56Z", time.Date(2026, 10, 27, 12, 34, 56, 0, time.UTC)},
		{"2026-10-27T12:34Z", time.Date(2026, 10, 27, 12, 34, 0, 0, time.UTC)},
		{"2026-10-27T12:34:56.789+09:00", time.Date(2026, 10, 27, 3, 34, 56, 789_000_000, time.UTC)},
		{"2026-10-27T12:34:56-01:30", time.Date(2026, 10, 27, 14, 4, 56, 0, time.UTC)},
		// Date はミリ秒までなので、それより細かい桁は切り捨てる。
		{"2026-10-27T12:34:56.1234567Z", time.Date(2026, 10, 27, 12, 34, 56, 123_000_000, time.UTC)},
		{"2026-10-27T12:34:56.5Z", time.Date(2026, 10, 27, 12, 34, 56, 500_000_000, time.UTC)},
		// 小数部は桁数を問わず受け、ミリ秒までで切り捨てる (V8 と同じ。以前の RFC3339 も
		// 10 桁以上を受けていた)。
		{"2026-10-27T12:34:56.1234567890Z", time.Date(2026, 10, 27, 12, 34, 56, 123_000_000, time.UTC)},
		{"2026-10-27T12:34:56.12345678901234567890Z", time.Date(2026, 10, 27, 12, 34, 56, 123_000_000, time.UTC)},
		{"2026-10-27T12:34:56.9999Z", time.Date(2026, 10, 27, 12, 34, 56, 999_000_000, time.UTC)},
		{"2026-10-27T24:00:00.000000000000Z", time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)},
		{"2026-10-27T24:00", time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)},
		{"2026-10-27T24:00:00.000Z", time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)},
		{"+002026-10-27T00:00:00Z", time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)},
		{"-000001-01-01T00:00:00Z", time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"+275760-09-13T00:00:00Z", time.Date(275760, 9, 13, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range accepted {
		got, ok := parseJSDateTimeString(tc.in, loc)
		if assert.True(t, ok, tc.in) {
			assert.True(t, tc.want.Equal(got), "%s: got %s, want %s", tc.in, got.UTC(), tc.want)
		}
	}
	// 仕様の形に合わないもの。このうち 2026-10-27 12:34・2026/10/27・Oct 27 2026・
	// 2026-10-27t12:34・2026-10-27Z・2026-1-27・2026-02-29 は、V8 の
	// Date.parse が実装依存の形として受ける (残す差。docs/divergence/approximations.md)。
	for _, in := range []string{
		"", "hello", "x-y", "2026-10-27 12:34", "2026/10/27", "Oct 27 2026",
		"2026-10-27T12", "2026-10-27Z", "2026-1-27", "26-10-27", "2026-10-27t12:34",
		"-000000-01-01T00:00:00Z", "2026-00-01", "2026-13-01", "2026-02-29", "2026-10-00", "2026-10-32",
		"2026-10-27T24:01", "2026-10-27T24:00:01", "2026-10-27T24:00:00.001Z", "2026-10-27T25:00",
		"2026-10-27T12:60", "2026-10-27T12:34:60", "2026-10-27T12:34+24:00", "2026-10-27T12:34+09:60",
		"2026-10-27T12:34:56.Z", "2026-10-27T24:00:00.0001Z", "2026-10-27T24:00:00.00000000001Z",
		"+275760-09-13T00:00:00.001Z", "+275761-01-01T00:00:00Z", "-271822-01-01T00:00:00Z",
	} {
		_, ok := parseJSDateTimeString(in, loc)
		assert.False(t, ok, in)
	}
}
