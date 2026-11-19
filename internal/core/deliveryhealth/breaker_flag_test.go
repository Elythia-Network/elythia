package deliveryhealth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBreaker_CheckWithFlag pins #4221: the flag rides along every decision
// of the check script.
func TestBreaker_CheckWithFlag(t *testing.T) {
	ctx := context.Background()
	b, clk, mr := newTestBreaker(t)
	require.NoError(t, mr.Set("flag", "1"))

	r := b.CheckWithFlag(ctx, "a.example", "", "flag")
	assert.Equal(t, BreakerAllow, r.Decision)
	assert.True(t, r.FlagSet, "allow")
	assert.False(t, b.CheckWithFlag(ctx, "a.example", "", "missing").FlagSet)
	require.NoError(t, mr.Set("", "1"))
	assert.False(t, b.CheckWithFlag(ctx, "a.example", "", "").FlagSet, "no key reads nothing")
	assert.False(t, b.Check(ctx, "a.example", "").FlagSet)

	for range BreakerThreshold {
		b.RecordFailure(ctx, "down.example", "")
	}
	r = b.CheckWithFlag(ctx, "down.example", "", "flag")
	require.Equal(t, BreakerDelay, r.Decision)
	assert.True(t, r.FlagSet, "open")

	clk.Advance(BreakerInitialInterval)
	r = b.CheckWithFlag(ctx, "down.example", "", "flag")
	require.Equal(t, BreakerProbe, r.Decision)
	assert.True(t, r.FlagSet, "probe")
	r = b.CheckWithFlag(ctx, "down.example", "", "flag")
	require.Equal(t, BreakerDelay, r.Decision)
	assert.True(t, r.FlagSet, "probing")

	b.Throttle(ctx, "busy.example", 0)
	r = b.CheckWithFlag(ctx, "busy.example", "j", "flag")
	require.Equal(t, BreakerDelay, r.Decision)
	assert.True(t, r.FlagSet, "throttled")
	r = b.CheckWithFlag(ctx, "busy.example", "j", "flag")
	require.Equal(t, BreakerDelay, r.Decision)
	assert.True(t, r.FlagSet, "reserved")
}
