package processors_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

func ed25519Payload(t *testing.T) queue.DeliverPayload {
	t.Helper()
	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)
	return payload
}

func newMiniredisClient(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// TestDeliverProcessor_DegradeFlagReadWithBreakerCheck pins #4221: with a
// breaker wired, the Ed25519 degrade flag is read by the breaker's check (one
// round trip), not by a separate EXISTS on the processor's own client.
func TestDeliverProcessor_DegradeFlagReadWithBreakerCheck(t *testing.T) {
	ctx := context.Background()
	breakerRedis := newMiniredisClient(t)
	ownRedis := newMiniredisClient(t)
	// 印はブレーカーの Redis にだけ置く。processor が自分の client で読むと
	// 印が無いことになり、Ed25519 で送る。
	require.NoError(t, breakerRedis.Set(ctx, "ed25519:degrade:remote.example", "1", 0).Err())

	signer := &recordingSigner{}
	p := processors.NewDeliverProcessor(signer, nil, nil, nil, nil, nil)
	p.SetRedis(ownRedis)
	p.SetDeliveryBreaker(deliveryhealth.NewBreaker(breakerRedis))

	payload := ed25519Payload(t)
	require.NoError(t, p.Handle(ctx, makeTask(t, payload)))
	assert.Equal(t, []string{payload.KeyID}, signer.calls, "the flag on the breaker's Redis makes it sign with RSA")

	// 自分の client にだけ印があっても読まない (ブレーカーの判定とは別に聞かない)。
	require.NoError(t, breakerRedis.Del(ctx, "ed25519:degrade:remote.example").Err())
	require.NoError(t, ownRedis.Set(ctx, "ed25519:degrade:remote.example", "1", 0).Err())
	require.NoError(t, p.Handle(ctx, makeTask(t, payload)))
	assert.Equal(t, []string{payload.KeyID, payload.Ed25519KeyID}, signer.calls, "without the flag on the breaker's Redis it signs with Ed25519")
}

// TestDeliverProcessor_DegradeFlagKeyOnlyForEd25519 pins that the flag key is
// asked for only when the delivery would sign with Ed25519.
func TestDeliverProcessor_DegradeFlagKeyOnlyForEd25519(t *testing.T) {
	ctx := context.Background()
	b := &fakeBreaker{flagSet: true}
	signer := &recordingSigner{}
	p := processors.NewDeliverProcessor(signer, nil, nil, nil, nil, nil)
	p.SetDeliveryBreaker(b)

	rsaOnly := makePayload(t)
	require.NoError(t, p.Handle(ctx, makeTask(t, rsaOnly)))
	ed := ed25519Payload(t)
	require.NoError(t, p.Handle(ctx, makeTask(t, ed)))

	assert.Equal(t, []string{"", "ed25519:degrade:remote.example"}, b.flagKeys)
	assert.Equal(t, []string{rsaOnly.KeyID, ed.KeyID}, signer.calls, "a set flag makes the Ed25519 delivery sign with RSA")
}
