package userlist

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubSysAcct struct {
	user *model.User
	err  error
}

func (s *stubSysAcct) Fetch(_ string) (*model.User, error) { return s.user, s.err }

type stubEnqueuer struct {
	got  []queue.FollowPayload
	err  error
	hits int
}

func (s *stubEnqueuer) EnqueueFollowBulk(p []queue.FollowPayload) error {
	s.hits++
	s.got = p
	return s.err
}

func TestProxyFollower_EnqueuesForEachRemote(t *testing.T) {
	sa := &stubSysAcct{user: &model.User{ID: "proxy1"}}
	enq := &stubEnqueuer{}
	pf := NewProxyFollower(sa, enq)
	pf.EnqueueProxyFollow([]string{"r1", "r2"})
	require.Equal(t, 1, enq.hits)
	require.Len(t, enq.got, 2)
	assert.Equal(t, queue.FollowPayload{FollowerID: "proxy1", FolloweeID: "r1"}, enq.got[0])
	assert.Equal(t, queue.FollowPayload{FollowerID: "proxy1", FolloweeID: "r2"}, enq.got[1])
}

func TestProxyFollower_EmptyIsNoop(t *testing.T) {
	sa := &stubSysAcct{user: &model.User{ID: "proxy1"}}
	enq := &stubEnqueuer{}
	NewProxyFollower(sa, enq).EnqueueProxyFollow(nil)
	assert.Equal(t, 0, enq.hits, "空 slice では proxy fetch も enqueue もしない")
}

// proxy account 取得失敗は best-effort で skip (enqueue しない、panic しない)。
func TestProxyFollower_FetchErrorSkips(t *testing.T) {
	sa := &stubSysAcct{err: errors.New("no proxy")}
	enq := &stubEnqueuer{}
	NewProxyFollower(sa, enq).EnqueueProxyFollow([]string{"r1"})
	assert.Equal(t, 0, enq.hits)
}

// enqueue 失敗も best-effort で握り潰す (panic しない)。
func TestProxyFollower_EnqueueErrorSwallowed(t *testing.T) {
	sa := &stubSysAcct{user: &model.User{ID: "proxy1"}}
	enq := &stubEnqueuer{err: errors.New("queue down")}
	NewProxyFollower(sa, enq).EnqueueProxyFollow([]string{"r1"})
	assert.Equal(t, 1, enq.hits)
}
