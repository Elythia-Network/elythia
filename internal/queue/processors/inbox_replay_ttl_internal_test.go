package processors

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every path that writes the guard's key (Claim, Remember, and a Claim after
// Release) gives it the record lifetime derived from the longest retry delay.
func TestRedisInboxReplayGuard_WritesTheRecordTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	delay := 3 * time.Hour
	want := InboxReplayRecordTTL(delay)
	require.Greater(t, want, inboxReplayTTL+delay)
	g := NewRedisInboxReplayGuard(rdb, delay)
	ctx := context.Background()
	key := inboxReplayKey("k")

	held, err := g.Claim(ctx, "k", "job:1")
	require.NoError(t, err)
	require.True(t, held)
	assert.Equal(t, want, mr.TTL(key), "Claim")

	mr.FastForward(time.Hour)
	require.NoError(t, g.Release(ctx, "k", "job:1"))
	assert.False(t, mr.Exists(key), "Release drops the mark")
	held, err = g.Claim(ctx, "k", "job:2")
	require.NoError(t, err)
	require.True(t, held)
	assert.Equal(t, want, mr.TTL(key), "Claim after Release")

	mr.FastForward(time.Hour)
	require.NoError(t, g.Remember(ctx, "k"))
	assert.Equal(t, want, mr.TTL(key), "Remember")
}
