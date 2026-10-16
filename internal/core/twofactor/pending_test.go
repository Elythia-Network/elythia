package twofactor

import (
	"context"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/testutil"
)

func TestNewRedisPendingGuard_NilClient(t *testing.T) {
	assert.Nil(t, NewRedisPendingGuard(nil))
}

func TestRedisPendingGuard_AllowsUpToMaxAttempts(t *testing.T) {
	_, client := newMiniRedis(t)
	g := NewRedisPendingGuard(client)
	ctx := context.Background()

	require.NoError(t, g.Start(ctx, "u1", "secret"))
	for i := 0; i < PendingMaxAttempts; i++ {
		require.NoError(t, g.Attempt(ctx, "u1", "secret"), "attempt %d", i+1)
	}
	assert.ErrorIs(t, g.Attempt(ctx, "u1", "secret"), ErrPendingExpired, "the budget is used up")

	// 発行し直すと数え直す。
	require.NoError(t, g.Start(ctx, "u1", "secret2"))
	assert.NoError(t, g.Attempt(ctx, "u1", "secret2"))
}

func TestRedisPendingGuard_RejectsUnknownOrReplacedSecret(t *testing.T) {
	mr, client := newMiniRedis(t)
	g := NewRedisPendingGuard(client)
	ctx := context.Background()

	assert.ErrorIs(t, g.Attempt(ctx, "u1", "secret"), ErrPendingExpired, "never started")
	assert.False(t, mr.Exists(pendingKey("u1")), "a failed attempt must not create a key without expiry")

	require.NoError(t, g.Start(ctx, "u1", "old"))
	require.NoError(t, g.Start(ctx, "u1", "new"))
	assert.ErrorIs(t, g.Attempt(ctx, "u1", "old"), ErrPendingExpired, "a replaced secret")
	assert.ErrorIs(t, g.Attempt(ctx, "u2", "new"), ErrPendingExpired, "another user")
	assert.NoError(t, g.Attempt(ctx, "u1", "new"))

	require.NoError(t, g.Finish(ctx, "u1"))
	assert.ErrorIs(t, g.Attempt(ctx, "u1", "new"), ErrPendingExpired, "finished")
}

func TestRedisPendingGuard_Expires(t *testing.T) {
	mr, client := newMiniRedis(t)
	g := NewRedisPendingGuard(client)
	ctx := context.Background()

	require.NoError(t, g.Start(ctx, "u1", "secret"))
	assert.Equal(t, PendingTTL, mr.TTL(pendingKey("u1")))
	mr.FastForward(PendingTTL)
	assert.ErrorIs(t, g.Attempt(ctx, "u1", "secret"), ErrPendingExpired)
}

func TestRedisPendingGuard_StoresDigestOnly(t *testing.T) {
	mr, client := newMiniRedis(t)
	g := NewRedisPendingGuard(client)
	require.NoError(t, g.Start(context.Background(), "u1", "plainsecret"))
	assert.NotContains(t, mr.HGet(pendingKey("u1"), "s"), "plainsecret")
}

func TestRedisPendingGuard_StoreErrors(t *testing.T) {
	mr, _ := newMiniRedis(t)
	// 再試行の待ちで遅くならないよう、再試行しない client を使う。
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	g := NewRedisPendingGuard(client)
	ctx := context.Background()
	testutil.MiniredisDown(mr)

	assert.Error(t, g.Start(ctx, "u1", "secret"))
	err := g.Attempt(ctx, "u1", "secret")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrPendingExpired, "a store failure is not an expiry")
	assert.Error(t, g.Finish(ctx, "u1"))
}
