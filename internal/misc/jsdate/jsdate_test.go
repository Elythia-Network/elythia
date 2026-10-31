package jsdate

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidMillis(t *testing.T) {
	cases := []struct {
		name string
		ms   int64
		want bool
	}{
		{"zero", 0, true},
		{"max", MaxMillis, true},
		{"min", -MaxMillis, true},
		{"above max", MaxMillis + 1, false},
		{"below min", -MaxMillis - 1, false},
		{"int64 max", math.MaxInt64, false},
		{"int64 min", math.MinInt64, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ValidMillis(tc.ms))
		})
	}
}
