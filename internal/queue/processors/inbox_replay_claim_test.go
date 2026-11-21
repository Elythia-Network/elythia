package processors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
)

// The Redis guard's processing mark: one owner holds it, the same owner may
// take it again (a retry of the same job), Remember turns it into a processed
// record nobody holds, and Release drops only the owner's own mark.
func TestRedisInboxReplayGuard_Claim(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	tr := withRedis(t)
	g := NewRedisInboxReplayGuard(tr.Client)
	ctx := context.Background()

	held, err := g.Claim(ctx, "k1", "job:1")
	require.NoError(t, err)
	assert.True(t, held, "the first claim places the mark")
	held, err = g.Claim(ctx, "k1", "job:1")
	require.NoError(t, err)
	assert.True(t, held, "a retry of the same job holds its own mark")
	held, err = g.Claim(ctx, "k1", "job:2")
	require.NoError(t, err)
	assert.False(t, held, "another job does not")

	require.NoError(t, g.Release(ctx, "k1", "job:2"))
	held, err = g.Claim(ctx, "k1", "job:2")
	require.NoError(t, err)
	assert.False(t, held, "another job cannot release the mark")
	require.NoError(t, g.Release(ctx, "k1", "job:1"))
	held, err = g.Claim(ctx, "k1", "job:2")
	require.NoError(t, err)
	assert.True(t, held, "the owner's release frees the key")

	require.NoError(t, g.Remember(ctx, "k1"))
	require.NoError(t, g.Release(ctx, "k1", "job:2"))
	for _, owner := range []string{"job:2", "job:3"} {
		held, err = g.Claim(ctx, "k1", owner)
		require.NoError(t, err)
		assert.False(t, held, "a processed key is held by nobody (%s)", owner)
	}
	// ジョブの ID が処理済みの値 ("1") と同じでも、自分の印とは取り違えない。
	require.NoError(t, g.Remember(ctx, "k2"))
	held, err = g.Claim(ctx, "k2", "1")
	require.NoError(t, err)
	assert.False(t, held)

	ttl, err := tr.Client.TTL(ctx, inboxReplayKey("k3")).Result()
	require.NoError(t, err)
	require.Negative(t, ttl, "k3 does not exist yet")
	_, err = g.Claim(ctx, "k3", "job:1")
	require.NoError(t, err)
	ttl, err = tr.Client.TTL(ctx, inboxReplayKey("k3")).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl, "the mark expires")
}

func TestRedisInboxReplayGuard_NilAndEmptyKey(t *testing.T) {
	var g *RedisInboxReplayGuard
	held, err := g.Claim(context.Background(), "k", "o")
	require.NoError(t, err)
	assert.True(t, held)
	require.NoError(t, g.Release(context.Background(), "k", "o"))
	require.NoError(t, g.Remember(context.Background(), "k"))
}

func TestReplayOwner(t *testing.T) {
	assert.Equal(t, "job:42", replayOwner(driver.WithJobID(context.Background(), "42")))
	a, b := replayOwner(context.Background()), replayOwner(context.Background())
	assert.NotEqual(t, a, b, "without a job ID every call is its own owner")
	assert.Contains(t, a, "call:")
}
