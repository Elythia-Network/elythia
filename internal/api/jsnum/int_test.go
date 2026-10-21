package jsnum

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
		ok   bool
	}{
		{name: "integer", in: `10`, want: 10, ok: true},
		{name: "negative integer", in: `-3`, want: -3, ok: true},
		{name: "integer-valued fraction", in: `10.0`, want: 10, ok: true},
		{name: "exponent", in: `1e1`, want: 10, ok: true},
		{name: "negative zero", in: `-0.0`, want: 0, ok: true},
		{name: "large integer-valued float", in: `1e18`, want: 1e18, ok: true},
		{name: "int64 min as a float", in: `-9.223372036854775808e18`, want: -1 << 63, ok: true},
		{name: "fraction", in: `10.5`},
		{name: "string", in: `"10"`},
		{name: "boolean", in: `true`},
		{name: "beyond int64 as an integer literal", in: `9223372036854775808`},
		{name: "beyond int64 as a float", in: `1e19`},
		{name: "beyond float64", in: `1e400`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n Int
			err := n.UnmarshalJSON([]byte(tc.in))
			if !tc.ok {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, Int(tc.want), n)
		})
	}
}

func TestIntUnmarshalNullKeepsTheValue(t *testing.T) {
	n := Int(7)
	require.NoError(t, n.UnmarshalJSON([]byte(`null`)))
	assert.Equal(t, Int(7), n)
}

func TestIntInAStruct(t *testing.T) {
	var req struct {
		Limit *Int `json:"limit"`
		Since *Int `json:"since"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"limit":10.0,"since":null}`), &req))
	require.NotNil(t, req.Limit)
	assert.Equal(t, Int(10), *req.Limit)
	assert.Nil(t, req.Since)
	assert.Error(t, json.Unmarshal([]byte(`{"limit":1.5}`), &req))
}

func TestIntPointers(t *testing.T) {
	var nilInt *Int
	assert.Nil(t, nilInt.IntPtr())
	assert.Nil(t, nilInt.Int64Ptr())

	n := Int(42)
	require.NotNil(t, n.IntPtr())
	assert.Equal(t, 42, *n.IntPtr())
	require.NotNil(t, n.Int64Ptr())
	assert.Equal(t, int64(42), *n.Int64Ptr())
}
