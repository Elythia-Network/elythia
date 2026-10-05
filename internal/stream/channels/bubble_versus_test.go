package channels

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/bubbleversus"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

type nopVersusPub struct{}

func (nopVersusPub) PublishInvited(string, *model.User, *bubbleversus.Match) {}
func (nopVersusPub) PublishUser(string, string, any)                         {}
func (nopVersusPub) PublishMatch(string, string, any)                        {}

// newVersusMatch returns a service and an accepted match between alice and bob.
func newVersusMatch(t *testing.T) (*bubbleversus.Service, *bubbleversus.Match, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := bubbleversus.NewService(rdb, nopVersusPub{}, nil, gen)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return now })
	ctx := context.Background()
	m, err := svc.Invite(ctx, &model.User{ID: "alice"}, &model.User{ID: "bob"}, "normal")
	require.NoError(t, err)
	m, err = svc.Accept(ctx, "bob", m.ID)
	require.NoError(t, err)
	return svc, m, &now
}

func TestBubbleVersusChannel(t *testing.T) {
	assert.ErrorIs(t, NewBubbleVersus(newCtx(nil)).Init(nil), stream.ErrInvalidParams)

	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewBubbleVersus(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"bubbleVersus:alice"}, ctx.subs)
	ch.OnRedisEvent([]byte(`{"type":"invited","body":{"matchId":"m1"}}`))
	ch.OnRedisEvent([]byte(`not json`))
	ch.OnClientMessage("anything", nil)
	assert.Equal(t, []string{"invited"}, ctx.sentType)
	assert.True(t, ch.(stream.ShareableChannel).ShouldShare())
	ch.Dispose()
	assert.Equal(t, []string{"bubbleVersus:alice"}, ctx.unsubs)

	// Init に失敗したものは何も解除しない。
	failed := newCtx(nil)
	ch = NewBubbleVersus(failed)
	_ = ch.Init(nil)
	ch.Dispose()
	assert.Empty(t, failed.unsubs)
}

// 参加者以外はつなげない (相手の盤面と攻撃が流れるため)。
func TestBubbleVersusMatchChannel_Init(t *testing.T) {
	svc, m, _ := newVersusMatch(t)
	f := NewBubbleVersusMatchFactory(svc)
	params := func(id string) json.RawMessage {
		raw, _ := json.Marshal(map[string]string{"matchId": id})
		return raw
	}
	for name, tc := range map[string]struct {
		user   any
		params json.RawMessage
	}{
		"anonymous":       {nil, params(m.ID)},
		"not a player":    {&model.User{ID: "carol"}, params(m.ID)},
		"missing match":   {&model.User{ID: "alice"}, params("nope")},
		"no matchId":      {&model.User{ID: "alice"}, json.RawMessage(`{}`)},
		"malformed param": {&model.User{ID: "alice"}, json.RawMessage(`[`)},
	} {
		ctx := newCtx(tc.user)
		ch := f.New(ctx)
		assert.ErrorIs(t, ch.Init(tc.params), stream.ErrInvalidParams, name)
		assert.Empty(t, ctx.subs, name)
		ch.OnClientMessage("ready", json.RawMessage(`true`)) // ignored
		ch.Dispose()
		assert.Empty(t, ctx.unsubs, name)
	}
	assert.ErrorIs(t, NewBubbleVersusMatchFactory(nil).New(newCtx(&model.User{ID: "alice"})).Init(params(m.ID)), stream.ErrInvalidParams)

	ctx := newCtx(&model.User{ID: "bob"})
	ch := f.New(ctx)
	require.NoError(t, ch.Init(params(m.ID)))
	assert.Equal(t, []string{"bubbleVersusMatch:" + m.ID}, ctx.subs)
	ch.OnRedisEvent([]byte(`{"type":"attack","body":{"count":2}}`))
	assert.Equal(t, []string{"attack"}, ctx.sentType)
	ch.Dispose()
	assert.Equal(t, []string{"bubbleVersusMatch:" + m.ID}, ctx.unsubs)
}

func TestBubbleVersusMatchChannel_Messages(t *testing.T) {
	svc, m, now := newVersusMatch(t)
	f := NewBubbleVersusMatchFactory(svc)
	params, _ := json.Marshal(map[string]string{"matchId": m.ID})
	actx, bctx := newCtx(&model.User{ID: "alice"}), newCtx(&model.User{ID: "bob"})
	a, b := f.New(actx), f.New(bctx)
	require.NoError(t, a.Init(params))
	require.NoError(t, b.Init(params))
	bg := context.Background()

	// 形の壊れた本文は黙って捨てる。
	a.OnClientMessage("ready", json.RawMessage(`"yes"`))
	a.OnClientMessage("attack", json.RawMessage(`"x"`))
	a.OnClientMessage("unknown", nil)
	assert.Empty(t, actx.sentType)

	a.OnClientMessage("ready", json.RawMessage(`true`))
	b.OnClientMessage("ready", json.RawMessage(`true`))
	got, err := svc.Get(bg, m.ID)
	require.NoError(t, err)
	assert.Equal(t, bubbleversus.StatusPlaying, got.Status)

	// カウントダウン中の攻撃は失敗として返す。
	a.OnClientMessage("attack", json.RawMessage(`{"count":1}`))
	require.Equal(t, []string{"error"}, actx.sentType)
	assert.Equal(t, map[string]any{"type": "attack", "code": "INVALID_STATE"}, actx.sentBody[0])

	*now = now.Add(bubbleversus.Countdown)
	a.OnClientMessage("attack", json.RawMessage(`{"count":4}`))
	a.OnClientMessage("attack", json.RawMessage(`{"count":0}`))
	assert.Equal(t, map[string]any{"type": "attack", "code": "INVALID_PARAM"}, actx.sentBody[len(actx.sentBody)-1])
	got, err = svc.Get(bg, m.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 4, got.Players[0].Sent)
	b.OnClientMessage("attack", json.RawMessage(`{"count":2}`))
	got, err = svc.Get(bg, m.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 4, got.Players[0].Sent)
	assert.EqualValues(t, 2, got.Players[1].Sent, "an attack counts for the sender")

	// alice が黙っていても、bob が盤面を送り続ければ切断扱いにならない。
	*now = now.Add(bubbleversus.DisconnectAfter)
	b.OnClientMessage("state", json.RawMessage(`{"score":1}`))
	a.OnClientMessage("claimDisconnected", nil)
	assert.Equal(t, map[string]any{"type": "claimDisconnected", "code": "NOT_YET"}, actx.sentBody[len(actx.sentBody)-1])

	// 大きすぎる / JSON でない盤面は中継しない (時刻も更新しない)。
	*now = now.Add(bubbleversus.DisconnectAfter)
	b.OnClientMessage("state", json.RawMessage(`{"s":"`+strings.Repeat("x", bubbleVersusMaxState)+`"}`))
	b.OnClientMessage("state", json.RawMessage(`{`))
	b.OnClientMessage("state", nil)
	a.OnClientMessage("claimDisconnected", nil)
	got, err = svc.Get(bg, m.ID)
	require.NoError(t, err)
	assert.Equal(t, bubbleversus.StatusEnded, got.Status)
	require.NotNil(t, got.WinnerID)
	assert.Equal(t, "alice", *got.WinnerID)
}

func TestBubbleVersusErrorCode(t *testing.T) {
	assert.Equal(t, "NO_SUCH_MATCH", bubbleVersusErrorCode(bubbleversus.ErrNoSuchMatch))
	assert.Equal(t, "INTERNAL_ERROR", bubbleVersusErrorCode(assertError))
}
