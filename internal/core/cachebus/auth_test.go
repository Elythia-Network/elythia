package cachebus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuthCache records what ConnectAuth applies.
type fakeAuthCache struct {
	hook    func(string)
	dropped []string
	all     int
}

func (f *fakeAuthCache) SetInvalidationHook(fn func(string))      { f.hook = fn }
func (f *fakeAuthCache) InvalidateTokensForUserLocally(id string) { f.dropped = append(f.dropped, id) }
func (f *fakeAuthCache) InvalidateAllLocally()                    { f.all++ }

// TestConnectAuth_Wire pins the wire format (user ids only, never a token),
// that the process ignores its own messages, applies others' per user, and
// drops everything on a message it cannot read or on a receive overflow.
func TestConnectAuth_Wire(t *testing.T) {
	auth := &fakeAuthCache{}
	bus := &overflowLoopBus{loopBus: newLoopBus(), overflow: map[string]func(){}}
	stop := ConnectAuth(auth, bus, "me")
	defer stop()

	require.NotNil(t, auth.hook)
	auth.hook("u1")
	assert.Equal(t, []Message[AuthInvalidation]{{From: "me", Inv: AuthInvalidation{UserIDs: []string{"u1"}}}},
		messages[AuthInvalidation](t, bus.loopBus, TopicAuth))
	bus.mu.Lock()
	raw := string(bus.published[TopicAuth][0])
	bus.mu.Unlock()
	assert.JSONEq(t, `{"from":"me","inv":{"userIds":["u1"]}}`, raw, "nothing but the sender and the user ids")

	// 自分の通知は受け流す (loopBus は publish を自分にも配る)。
	assert.Empty(t, auth.dropped)
	// 他のプロセスの通知は利用者ごとに捨てる。
	deliverMessage(t, bus.loopBus, TopicAuth, Message[AuthInvalidation]{From: "other", Inv: AuthInvalidation{UserIDs: []string{"u2", "u3"}}})
	assert.Equal(t, []string{"u2", "u3"}, auth.dropped)
	deliverMessage(t, bus.loopBus, TopicAuth, Message[AuthInvalidation]{From: "other", Inv: AuthInvalidation{All: true}})
	assert.Equal(t, 1, auth.all)
	// 読めない通知と受け側の溢れは全体を捨てる。
	bus.deliver(t, TopicAuth, []byte("{not json"))
	assert.Equal(t, 2, auth.all)
	bus.overflow[TopicAuth]()
	assert.Equal(t, 3, auth.all)
	assert.Equal(t, []string{"u2", "u3"}, auth.dropped)
}

func TestConnectAuth_NilIsNoop(t *testing.T) {
	ConnectAuth(nil, newLoopBus(), "x")()
	ConnectAuth(&fakeAuthCache{}, nil, "x")()
}
