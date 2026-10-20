package relay_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/core/relay"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInvalidationHook_CalledByEveryWrite pins that Add, Remove and the
// status changes drop this process's list and then call the hook that tells
// the other processes (#3664), while InvalidateCacheLocally never calls it.
func TestInvalidationHook_CalledByEveryWrite(t *testing.T) {
	svc, repo, _, _ := newService(t)
	calls := 0
	svc.SetInvalidationHook(func() { calls++ })
	ctx := context.Background()

	rel, err := svc.Add(ctx, "https://r.example/inbox")
	require.NoError(t, err)
	assert.Equal(t, 1, calls)

	require.NoError(t, svc.MarkAccepted(ctx, rel.ID))
	assert.Equal(t, 2, calls)
	// requesting でない行への変更は何も書かないので、伝えない。
	require.NoError(t, svc.MarkRejected(ctx, rel.ID))
	assert.Equal(t, 2, calls)

	inbox := rel.Inbox
	actor := &model.User{ID: "a", Inbox: &inbox}
	assert.True(t, svc.IsRelayActor(actor))
	// 受けた側の捨て方は hook を呼ばない (呼ぶと通知を投げ合う)。
	repo.Relays[rel.ID].Status = relay.StatusRejected
	svc.InvalidateCacheLocally()
	assert.False(t, svc.IsRelayActor(actor))
	assert.Equal(t, 2, calls)

	require.NoError(t, svc.Remove(ctx, rel.ID))
	assert.Equal(t, 3, calls)
}

// TestDropLocal pins that DropLocal drops this process's list without calling
// the hook (#3729).
func TestDropLocal(t *testing.T) {
	svc, repo, _, _ := newService(t)
	ctx := context.Background()
	rel, err := svc.Add(ctx, "https://r.example/inbox")
	require.NoError(t, err)
	require.NoError(t, svc.MarkAccepted(ctx, rel.ID))
	svc.SetInvalidationHook(func() { t.Error("DropLocal must not call the hook") })

	inbox := rel.Inbox
	actor := &model.User{ID: "a", Inbox: &inbox}
	require.True(t, svc.IsRelayActor(actor))
	// 他のプロセスが書いた (このプロセスの invalidate は通っていない)。
	repo.Relays[rel.ID].Status = relay.StatusRejected
	require.True(t, svc.IsRelayActor(actor), "前提: 捨てるまでは cache が返る")

	svc.DropLocal()
	assert.False(t, svc.IsRelayActor(actor), "DropLocal did not drop the list")
}
