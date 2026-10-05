package transfer_test

import (
	"testing"

	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFollowingServiceAdapter_ConstructsWithRealType(t *testing.T) {
	// Adapter wraps *corefollowing.Service. We can't easily construct a real
	// one without pulling in DB dependencies, so nil service + nil adapter
	// exercises the construction and nil-safety paths.
	var svc *corefollowing.Service
	a := transfer.NewFollowingServiceAdapter(svc)
	assert.NotNil(t, a)
	// Follow on a nil inner returns (nil, nil) via the guard.
	out, err := a.Follow("f", "g", transfer.FollowOptions{})
	assert.NoError(t, err)
	assert.Nil(t, out)
}

type adapterMainEvents struct{ events []string }

func (a *adapterMainEvents) PublishMainEvent(userID, eventType string, _ any) {
	a.events = append(a.events, userID+":"+eventType)
}

// adapter は Silent を core/following へ渡す。silent ならフォローした側の follow を
// 流さず、followed は流す (本家 follow の silent と同じ)。
func TestFollowingServiceAdapter_PassesSilent(t *testing.T) {
	for _, tc := range []struct {
		silent bool
		want   []string
	}{
		{false, []string{"alice:follow", "bob:followed"}},
		{true, []string{"bob:followed"}},
	} {
		userRepo := testutil.NewMockUserRepository()
		userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
		userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
		idGen, _ := id.NewGenerator("aidx")
		svc := corefollowing.NewService(userRepo, testutil.NewMockFollowingRepository(), testutil.NewMockFollowRequestRepository(), idGen)
		events := &adapterMainEvents{}
		svc.SetMainStreamPublisher(events)
		_, err := transfer.NewFollowingServiceAdapter(svc).Follow("alice", "bob", transfer.FollowOptions{Silent: tc.silent})
		require.NoError(t, err)
		assert.Equal(t, tc.want, events.events)
	}
}
