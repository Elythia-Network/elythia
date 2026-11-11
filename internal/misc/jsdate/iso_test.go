package jsdate

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestISOString(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	assert.Equal(t, "2026-06-21T03:34:56.789Z", ISOString(time.Date(2026, 6, 21, 12, 34, 56, 789_123_456, jst)))
	// 秒ちょうどでも .000 を残す (RFC3339Nano は落とす)。
	assert.Equal(t, "2026-01-02T03:04:05.000Z", ISOString(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
}

func TestTimeMarshalJSON(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	type resp struct {
		At    Time  `json:"at"`
		Ptr   *Time `json:"ptr"`
		Unset *Time `json:"unset"`
		Omit  Time  `json:"omit,omitzero"`
	}
	exact := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	b, err := json.Marshal(resp{
		At:  NewTime(time.Date(2026, 6, 21, 12, 34, 56, 789_123_456, jst)),
		Ptr: NewTimePtr(&exact),
	})
	require.NoError(t, err)
	// Go の既定 (RFC3339Nano) なら "2026-06-21T12:34:56.789123456+09:00" と "2026-01-02T03:04:05Z" になる。
	assert.JSONEq(t, `{"at":"2026-06-21T03:34:56.789Z","ptr":"2026-01-02T03:04:05.000Z","unset":null}`, string(b))
}

func TestTimeUnmarshalJSON(t *testing.T) {
	// 中継する別のサービスの応答や、以前の版が保存した値は、どちらの書式でも読める。
	for _, in := range []string{`"2026-06-21T12:34:56.789123456+09:00"`, `"2026-06-21T03:34:56.789Z"`} {
		var got Time
		require.NoError(t, json.Unmarshal([]byte(in), &got), in)
		assert.Equal(t, int64(1782012896789), got.UnixMilli(), in)
	}
	var p *Time
	require.NoError(t, json.Unmarshal([]byte(`null`), &p))
	assert.Nil(t, p)
	assert.Nil(t, NewTimePtr(nil))
}
