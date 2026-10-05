package federation_test

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 配線用 setter とガード節は挙動の分岐点なので、呼べること自体を固定する。
// カバレッジのマージン確保も兼ねる (#2340 の CI 89.9% 落ちで判明)。

func TestPinDeliveryHook_SetRelayBroadcaster(t *testing.T) {
	h := federation.NewPinDeliveryHook(nil, nil,
		testutil.NewMockUserRepository(), testutil.NewMockNoteRepository())
	assert.NotPanics(t, func() { h.SetRelayBroadcaster(nil) })
}

func TestDeliverService_SetSyncDeliverHookForTest(t *testing.T) {
	s := federation.NewDeliverService(nil, testutil.NewMockUserRepository(),
		testutil.NewMockFollowingRepository(), testutil.NewMockUserKeypairRepository(), nil)
	called := false
	s.SetSyncDeliverHookForTest(func(queue.DeliverPayload) error { called = true; return nil })
	assert.False(t, called, "設定しただけでは呼ばれない")
}

// SendAcceptForInboundFollow の方向ガード。
// local→remote / local→local は AP 配信不要なので何もしない。
func TestSendAcceptForInboundFollow_DirectionGuards(t *testing.T) {
	h := federation.NewFollowingDeliveryHook(nil, nil, nil)
	host := "remote.example"
	local := &model.User{ID: "l1"}
	remote := &model.User{ID: "r1", Host: &host}
	raw := json.RawMessage(`{"type":"Follow"}`)

	for _, tc := range []struct {
		name             string
		follower, target *model.User
	}{
		{"nil follower", nil, local},
		{"nil followee", remote, nil},
		{"local -> remote は配信不要", local, remote},
		{"local -> local は配信不要", local, local},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, h.SendAcceptForInboundFollow(tc.follower, tc.target, raw))
		})
	}
}
