package deliveryhealth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openUntilMaxInterval opens the breaker for host and fails probes until the
// interval reaches BreakerMaxInterval.
func openUntilMaxInterval(t *testing.T, b *Breaker, clk *testClock, host string) {
	t.Helper()
	ctx := context.Background()
	failN(b, host, BreakerThreshold)
	interval := BreakerInitialInterval
	for interval < BreakerMaxInterval {
		clk.Advance(interval)
		r := b.Check(ctx, host, "")
		require.Equal(t, BreakerProbe, r.Decision)
		_, interval = b.RecordFailure(ctx, host, r.ProbeToken)
	}
}

// TestBreaker_LongOpenHoldsUntilNextProbe pins #4222: once the interval is
// long, a held job waits for the next probe instead of waking every
// breakerHoldCap.
func TestBreaker_LongOpenHoldsUntilNextProbe(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	ctx := context.Background()
	openUntilMaxInterval(t, b, clk, "down.example")

	clk.Advance(10 * time.Minute)
	r := b.Check(ctx, "down.example", "")
	require.Equal(t, BreakerDelay, r.Decision)
	assert.Equal(t, BreakerMaxInterval-10*time.Minute, r.Delay, "held until the next probe")

	var gotMax time.Duration
	b.jitter = func(max time.Duration) time.Duration { gotMax = max; return max }
	r = b.Check(ctx, "down.example", "")
	assert.Equal(t, (BreakerMaxInterval-10*time.Minute)/10, gotMax, "the jitter scales with the wait")
	assert.Equal(t, (BreakerMaxInterval-10*time.Minute)*11/10, r.Delay)
}

// TestBreaker_LongOpenWakesLess counts how often one held job wakes while a
// host stays down for a day at the longest interval.
//
// 5 分で区切ると 1 時間に 12 回起きる。次に試す時刻まで待たせれば、試行の 1 回と、
// 試行が失敗した後に HoldFor の区切りで起きる 1 回の、1 時間に 2 回で済む。
func TestBreaker_LongOpenWakesLess(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	ctx := context.Background()
	openUntilMaxInterval(t, b, clk, "down.example")

	end := clk.Now().Add(24 * time.Hour)
	wakes, probes := 0, 0
	for clk.Now().Before(end) {
		wakes++
		r := b.Check(ctx, "down.example", "")
		var hold time.Duration
		switch r.Decision {
		case BreakerProbe:
			probes++
			open, untilProbe := b.RecordFailure(ctx, "down.example", r.ProbeToken)
			require.True(t, open)
			require.Equal(t, BreakerMaxInterval, untilProbe)
			hold = b.HoldFor(untilProbe)
		case BreakerDelay:
			hold = r.Delay
		default:
			t.Fatalf("decision %v", r.Decision)
		}
		require.Positive(t, hold)
		clk.Advance(hold)
	}
	// 最初の 1 回 (次に試す時刻まで待つ) と、1〜23 時間目の試行ごとに 2 回。
	assert.Equal(t, 23, probes, "the host is still tried once an hour")
	assert.Equal(t, 1+23*2, wakes)
}

// TestBreaker_CloseFreesLongHeldJobs pins that a long-held job is held until
// the next probe, and goes when it wakes after a hand close.
//
// 手で閉じても、delayed に入ったジョブを早く起こす手段は無い (起きる時刻は
// mkq が持ち、ブレーカーの外にある)。ここで確かめるのは、待ちが次に試す時刻まで
// であることと、起きたときに流れることだけ。
func TestBreaker_CloseFreesLongHeldJobs(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	ctx := context.Background()
	openUntilMaxInterval(t, b, clk, "down.example")
	r := b.Check(ctx, "down.example", "")
	require.Equal(t, BreakerDelay, r.Decision)
	require.Equal(t, BreakerMaxInterval, r.Delay)

	require.NoError(t, b.Close(ctx, "down.example"))
	clk.Advance(r.Delay)
	assert.Equal(t, BreakerAllow, b.Check(ctx, "down.example", "").Decision)
}
