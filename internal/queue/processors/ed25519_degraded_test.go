package processors_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/queue/processors"
)

// 疎通の診断 (#3055) が読む格下げフラグは、配送側が立てるものと同じキーを見る。
// Redis の障害は「落としていない」と断定せずにエラーで返す。
func TestDeliverProcessor_Ed25519Degraded(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	p := processors.NewDeliverProcessor(&stubSigner{})
	ctx := context.Background()

	d, err := p.Ed25519Degraded(ctx, "remote.example")
	require.NoError(t, err)
	assert.False(t, d, "no Redis wired means not degraded")

	p.SetRedis(rdb)
	d, err = p.Ed25519Degraded(ctx, "remote.example")
	require.NoError(t, err)
	assert.False(t, d)

	require.NoError(t, mr.Set("ed25519:degrade:remote.example", "1"))
	d, err = p.Ed25519Degraded(ctx, "remote.example")
	require.NoError(t, err)
	assert.True(t, d)
	d, err = p.Ed25519Degraded(ctx, "other.example")
	require.NoError(t, err)
	assert.False(t, d)

	mr.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err = p.Ed25519Degraded(ctx, "remote.example")
	assert.Error(t, err, "a Redis failure is reported, not read as not degraded")
}
