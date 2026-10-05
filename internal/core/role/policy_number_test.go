package role_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/core/role"
)

func TestPolicyNumber(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
		ok   bool
	}{
		{"int", 5, 5, true},
		{"int64", int64(7), 7, true},
		// **これが本題。** role の policies は jsonb なので小数が届く。
		{"float64", 1.5, 1.5, true},
		{"zero", 0, 0, true},
		// 0 に潰れる値でも ok を返す。呼び出し側が float のまま比較する前提。
		{"sub-one float", 0.5, 0.5, true},
		{"negative", -1, -1, true},
		{"string", "5", 0, false},
		{"bool", true, 0, false},
		{"nil", nil, 0, false},
		{"missing key yields nil", any(nil), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := role.PolicyNumber(c.in)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

// **小数の分が切り捨てられないこと。** `time.Duration(f) * time.Minute` と
// 書くと 0.5 分が 0 になる。
func TestPolicyMinutes(t *testing.T) {
	got, ok := role.PolicyMinutes(0.5)
	assert.True(t, ok)
	assert.Equal(t, 30*time.Second, got)

	got, ok = role.PolicyMinutes(3)
	assert.True(t, ok)
	assert.Equal(t, 3*time.Minute, got)

	got, ok = role.PolicyMinutes(math.MaxFloat64)
	assert.True(t, ok)
	assert.Equal(t, time.Duration(math.MaxInt64), got)

	got, ok = role.PolicyMinutes(-math.MaxFloat64)
	assert.True(t, ok)
	assert.Equal(t, time.Duration(math.MinInt64), got)

	_, ok = role.PolicyMinutes("3")
	assert.False(t, ok)
}
