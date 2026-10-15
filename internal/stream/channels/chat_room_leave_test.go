package channels

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const chatRoomMessageEvent = `{"type":"message","body":{"id":"m1"}}`

// stepClock is a settable clock for the chatRoom re-check interval.
type stepClock struct{ t time.Time }

func (c *stepClock) Now() time.Time          { return c.t }
func (c *stepClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// newClockedChatRoom builds a chatRoom channel whose clock the test drives.
func newClockedChatRoom(t *testing.T, factory *ChatRoomFactory, ctx *stubContext) (*ChatRoomChannel, *stepClock) {
	t.Helper()
	ch, ok := factory.New(ctx).(*ChatRoomChannel)
	require.True(t, ok)
	clk := &stepClock{t: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)}
	ch.now = clk.Now
	return ch, clk
}

func TestChatRoomChannel_StopsAfterMembershipEnds(t *testing.T) {
	cases := []struct {
		name   string
		remove func(repo *testutil.MockChatRepository)
	}{
		// 自分で抜ける / オーナーに外される / 連合の Remove は、どれも membership の行を消す。
		{name: "membership removed", remove: func(r *testutil.MockChatRepository) {
			_ = r.DeleteMembership("bob", "r1")
		}},
		{name: "room deleted", remove: func(r *testutil.MockChatRepository) {
			_ = r.DeleteRoom("r1")
			_ = r.DeleteMembership("bob", "r1")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newChatSvc(t)
			repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
			repo.Memberships["bob:r1"] = &model.ChatRoomMembership{UserID: "bob", RoomID: "r1"}
			ctx := newCtx(&model.User{ID: "bob"})
			ch, clk := newClockedChatRoom(t, NewChatRoomFactory(svc), ctx)
			require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))

			ch.OnRedisEvent([]byte(chatRoomMessageEvent))
			require.Len(t, ctx.sentType, 1, "a member receives room events")

			tc.remove(repo)
			clk.Advance(chatRoomRecheckInterval)
			ch.OnRedisEvent([]byte(chatRoomMessageEvent))
			assert.Len(t, ctx.sentType, 1, "no events after leaving the room")
			assert.Equal(t, []string{"chatRoomStream:r1"}, ctx.unsubs, "the room topic is released")

			// 一度外れたら、メンバーに戻っても同じチャンネルは再開しない (開き直す)。
			repo.Memberships["bob:r1"] = &model.ChatRoomMembership{UserID: "bob", RoomID: "r1"}
			ch.OnRedisEvent([]byte(chatRoomMessageEvent))
			assert.Len(t, ctx.sentType, 1)
			assert.Len(t, ctx.unsubs, 1, "unsubscribe runs once")
		})
	}
}

func TestChatRoomChannel_RecheckErrorKeepsSubscription(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	repo.Memberships["bob:r1"] = &model.ChatRoomMembership{UserID: "bob", RoomID: "r1"}
	ctx := newCtx(&model.User{ID: "bob"})
	ch, clk := newClockedChatRoom(t, NewChatRoomFactory(svc), ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))

	repo.FindMembershipErr = errors.New("db down")
	clk.Advance(chatRoomRecheckInterval)
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	assert.Empty(t, ctx.sentType, "an event is withheld while membership cannot be checked")
	assert.Empty(t, ctx.unsubs, "a transient error must not close the channel")

	// 障害の結果は覚えないので、次のイベントで引き直す。
	repo.FindMembershipErr = nil
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	assert.Len(t, ctx.sentType, 1)
}

// A positive check is reused for chatRoomRecheckInterval: events in that
// window do not look the membership up again, and the first event after it
// does.
func TestChatRoomChannel_ReusesPositiveCheck(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	repo.Memberships["bob:r1"] = &model.ChatRoomMembership{UserID: "bob", RoomID: "r1"}
	ctx := newCtx(&model.User{ID: "bob"})
	ch, clk := newClockedChatRoom(t, NewChatRoomFactory(svc), ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))

	// 引けば失敗する状態にしておき、期限内は引かないことを見る。
	repo.FindMembershipErr = errors.New("must not be looked up")
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	clk.Advance(chatRoomRecheckInterval - time.Nanosecond)
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	assert.Len(t, ctx.sentType, 2, "events within the interval reuse the check from Init")

	// 期限を過ぎたら引き直す。
	clk.Advance(time.Nanosecond)
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	assert.Len(t, ctx.sentType, 2, "the first event after the interval looks the membership up")

	// 引き直して見られれば、また期限まで使い回す。
	repo.FindMembershipErr = nil
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	require.Len(t, ctx.sentType, 3)
	repo.FindMembershipErr = errors.New("must not be looked up")
	clk.Advance(chatRoomRecheckInterval - time.Nanosecond)
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	assert.Len(t, ctx.sentType, 4, "a re-check renews the interval")
}

func TestChatRoomChannel_DefaultClock(t *testing.T) {
	ch := &ChatRoomChannel{}
	assert.WithinDuration(t, time.Now(), ch.clock(), time.Minute)
}

func TestChatRoomChannel_RecheckWithoutUser(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(nil)
	ch := &ChatRoomChannel{ctx: ctx, svc: svc, roomID: "r1", topic: "chatRoomStream:r1"}
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	assert.Empty(t, ctx.sentType)
}

func TestChatRoomChannel_NilServiceForwards(t *testing.T) {
	ctx := newCtx(&model.User{ID: "bob"})
	ch := &ChatRoomChannel{ctx: ctx, roomID: "r1", topic: "chatRoomStream:r1"}
	ch.OnRedisEvent([]byte(chatRoomMessageEvent))
	assert.Len(t, ctx.sentType, 1)
}
