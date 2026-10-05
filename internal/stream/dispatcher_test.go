package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBus implements PubSubBus and records subscribe/unsubscribe calls.
// stubBus records subscriptions per topic.
//
// **トピックごとに複数のハンドラを持つ。** 1 つしか持たない形は、本番から
// 取り除いた「後から来た購読が前のを上書きする」挙動をスタブ側に残すことに
// なり、複数の Dispatcher を同じトピックへ繋ぐテストで嘘をつく。
type stubBus struct {
	mu         sync.Mutex
	subs       map[string][]func([]byte)
	unsubs     []string
	subscribed []string
}

func newStubBus() *stubBus {
	return &stubBus{subs: map[string][]func([]byte){}}
}

func (b *stubBus) Subscribe(topic string, handler func([]byte)) func() {
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], handler)
	idx := len(b.subs[topic]) - 1
	b.subscribed = append(b.subscribed, topic)
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if hs := b.subs[topic]; idx < len(hs) {
				hs[idx] = nil
				// **全部外れたらキーごと消す。** 既存のテストが
				// `_, ok := bus.subs[topic]` で購読状態を見ているので、
				// 残すと「解除したのに購読中」に見える。
				live := false
				for _, h := range hs {
					if h != nil {
						live = true
						break
					}
				}
				if !live {
					delete(b.subs, topic)
				}
			}
			b.unsubs = append(b.unsubs, topic)
		})
	}
}

// deliver hands payload to every live handler for topic.
func (b *stubBus) deliver(topic string, payload []byte) {
	b.mu.Lock()
	hs := make([]func([]byte), len(b.subs[topic]))
	copy(hs, b.subs[topic])
	b.mu.Unlock()
	for _, h := range hs {
		if h != nil {
			h(payload)
		}
	}
}

// fakeChannel is a Channel that records every callback for assertion.
type fakeChannel struct {
	ctx          ChannelContext
	initParams   json.RawMessage
	initErr      error
	disposeCount int
	redisEvents  [][]byte
	clientMsgs   []string
	subscribed   []string
}

func (f *fakeChannel) Init(params json.RawMessage) error {
	f.initParams = params
	if f.initErr != nil {
		return f.initErr
	}
	f.subscribed = append(f.subscribed, "topic-a")
	f.ctx.Subscribe("topic-a")
	return nil
}
func (f *fakeChannel) Dispose() { f.disposeCount++ }
func (f *fakeChannel) OnRedisEvent(payload []byte) {
	f.redisEvents = append(f.redisEvents, append([]byte(nil), payload...))
}
func (f *fakeChannel) OnClientMessage(msgType string, _ json.RawMessage) {
	f.clientMsgs = append(f.clientMsgs, msgType)
}

// fakeChannelHolder lets tests grab a reference to the channel created by the
// factory. The factory closure stores into Holder.Value when invoked.
type fakeChannelHolder struct {
	Value *fakeChannel
}

func newDispatcherWithFake(t *testing.T) (*Dispatcher, *Connection, *Registry, *stubBus, *fakeChannelHolder) {
	t.Helper()
	bus := newStubBus()
	registry := NewRegistry()
	holder := &fakeChannelHolder{}
	registry.Register("test", func(ctx ChannelContext) Channel {
		ch := &fakeChannel{ctx: ctx}
		holder.Value = ch
		return ch
	})
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)
	return d, conn, registry, bus, holder
}

func TestRegistry_RegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	r.Register("foo", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
	assert.NotNil(t, r.Lookup("foo"))
	assert.Nil(t, r.Lookup("missing"))
}

// #1944: RegisterCredentialed marks a channel as requiring auth; Register does not.
func TestRegistry_Credentialed(t *testing.T) {
	r := NewRegistry()
	r.Register("anon", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
	r.RegisterCredentialed("authed", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
	assert.False(t, r.RequiresCredential("anon"))
	assert.True(t, r.RequiresCredential("authed"))
	assert.False(t, r.RequiresCredential("unknown"))
	assert.NotNil(t, r.Lookup("authed"), "RegisterCredentialed も factory を登録する")
}

// #1944: anon (user==nil) は requireCredential channel に接続できず、認証済みは接続できる。
// non-credentialed channel は anon も接続できる。
func TestDispatcher_RequireCredential(t *testing.T) {
	newReg := func() *Registry {
		r := NewRegistry()
		r.RegisterCredentialed("authed", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
		r.Register("open", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
		return r
	}
	hasChannel := func(d *Dispatcher, id string) bool {
		d.mu.RLock()
		defer d.mu.RUnlock()
		_, ok := d.channels[id]
		return ok
	}

	// anon → credentialed: 拒否。
	anon := NewDispatcher(NewConnection("c1", nil, newFakeConn()), newReg(), newStubBus())
	anon.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"authed"}`))
	assert.False(t, hasChannel(anon, "a"), "anon は requireCredential channel に接続できない")

	// anon → non-credentialed: 許可。
	anon.HandleClientMessage("connect", json.RawMessage(`{"id":"o","channel":"open"}`))
	assert.True(t, hasChannel(anon, "o"), "anon でも非 credentialed channel は接続できる")

	// authed → credentialed: 許可。
	authed := NewDispatcher(NewConnection("c2", &model.User{ID: "alice"}, newFakeConn()), newReg(), newStubBus())
	authed.HandleClientMessage("connect", json.RawMessage(`{"id":"b","channel":"authed"}`))
	assert.True(t, hasChannel(authed, "b"), "認証済みは requireCredential channel に接続できる")
}

func TestDispatcher_HandleConnect(t *testing.T) {
	d, _, _, bus, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test","params":{}}`))

	// fake channel が Init で subscribe するので bus に topic-a が登録される
	bus.mu.Lock()
	defer bus.mu.Unlock()
	assert.Contains(t, bus.subscribed, "topic-a")
}

func TestDispatcher_HandleConnectInvalidJSON(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{not json`))
}

func TestDispatcher_HandleConnectMissingFields(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{}`))
}

func TestDispatcher_HandleConnectUnknownChannel(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"x","channel":"missing"}`))
}

func TestDispatcher_HandleConnectNoRegistry(t *testing.T) {
	conn := NewConnection("c1", nil, newFakeConn())
	d := NewDispatcher(conn, nil, newStubBus())
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"x","channel":"test"}`))
}

func TestDispatcher_HandleConnectDuplicateID(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
}

func TestDispatcher_HandleDisconnect(t *testing.T) {
	d, _, _, bus, holder := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, holder.Value)
	d.HandleClientMessage("disconnect", json.RawMessage(`{"id":"abc"}`))
	assert.Equal(t, 1, holder.Value.disposeCount)
	bus.mu.Lock()
	defer bus.mu.Unlock()
	assert.Contains(t, bus.unsubs, "topic-a")
}

func TestDispatcher_HandleDisconnectInvalidJSON(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("disconnect", json.RawMessage(`{bad`))
}

func TestDispatcher_HandleDisconnectUnknown(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("disconnect", json.RawMessage(`{"id":"missing"}`))
}

func TestDispatcher_HandleChannelMessage(t *testing.T) {
	d, _, _, _, holder := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, holder.Value)
	d.HandleClientMessage("ch", json.RawMessage(`{"id":"abc","type":"ping","body":{}}`))
	assert.Equal(t, []string{"ping"}, holder.Value.clientMsgs)
}

// #1780: top-level type 'channel' は 'ch' の alias として同じ
// onChannelMessageRequested に dispatch する (upstream Connection.ts)。
func TestDispatcher_HandleChannelMessage_ChannelAlias(t *testing.T) {
	d, _, _, _, holder := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, holder.Value)
	d.HandleClientMessage("channel", json.RawMessage(`{"id":"abc","type":"ping","body":{}}`))
	assert.Equal(t, []string{"ping"}, holder.Value.clientMsgs)
}

func TestDispatcher_HandleChannelMessageBadJSON(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("ch", json.RawMessage(`{bad`))
}

func TestDispatcher_HandleChannelMessageUnknown(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("ch", json.RawMessage(`{"id":"missing","type":"ping"}`))
}

func TestDispatcher_HandleUnknownEnvelope(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("noise", json.RawMessage(`{}`))
}

func TestDispatcher_FanoutDeliversRedisEvents(t *testing.T) {
	d, _, _, bus, holder := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, holder.Value)
	bus.deliver("topic-a", []byte(`{"event":"hi"}`))
	require.Len(t, holder.Value.redisEvents, 1)
	assert.Contains(t, string(holder.Value.redisEvents[0]), "hi")
}

func TestDispatcher_CloseAllDisposesEverything(t *testing.T) {
	d, _, _, bus, holder := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, holder.Value)
	d.CloseAll()
	assert.Equal(t, 1, holder.Value.disposeCount)
	bus.mu.Lock()
	defer bus.mu.Unlock()
	assert.Contains(t, bus.unsubs, "topic-a")
}

func TestDispatcher_DuplicateSubscribeIsNoOp(t *testing.T) {
	d, _, _, bus, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	// Init で 1 回 subscribe 済み。同じ topic を再度 subscribe しても増えない。
	d.subscribe("abc", "topic-a")
	bus.mu.Lock()
	defer bus.mu.Unlock()
	count := 0
	for _, t := range bus.subscribed {
		if t == "topic-a" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestDispatcher_SubscribeUnknownChannelIsNoOp(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.subscribe("missing", "topic-a")
}

func TestDispatcher_UnsubscribeMissingTopicIsNoOp(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	d.unsubscribe("abc", "topic-zz")
}

func TestDispatcher_UnsubscribeUnknownChannelIsNoOp(t *testing.T) {
	d, _, _, _, _ := newDispatcherWithFake(t)
	d.unsubscribe("missing", "topic-a")
}

func TestDispatcher_SharedTopicReferenceCount(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel {
		return &fakeChannel{ctx: ctx}
	})
	conn := NewConnection("c1", nil, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	// 2 つの channel が同じ topic を共有
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"test"}`))
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"b","channel":"test"}`))

	bus.mu.Lock()
	subscribedCount := 0
	for _, t := range bus.subscribed {
		if t == "topic-a" {
			subscribedCount++
		}
	}
	bus.mu.Unlock()
	// 1 度しか subscribe されない (reference count 共有)
	assert.Equal(t, 1, subscribedCount)

	// 1 つ disconnect しても unsubscribe されない
	d.HandleClientMessage("disconnect", json.RawMessage(`{"id":"a"}`))
	bus.mu.Lock()
	assert.Empty(t, bus.unsubs)
	bus.mu.Unlock()

	// 残り 1 つを disconnect すると unsubscribe される
	d.HandleClientMessage("disconnect", json.RawMessage(`{"id":"b"}`))
	bus.mu.Lock()
	assert.Contains(t, bus.unsubs, "topic-a")
	bus.mu.Unlock()
}

func TestDispatcher_NilBusOnSubscribe(t *testing.T) {
	conn := NewConnection("c1", nil, newFakeConn())
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel {
		return &fakeChannel{ctx: ctx}
	})
	d := NewDispatcher(conn, registry, nil)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	d.HandleClientMessage("disconnect", json.RawMessage(`{"id":"abc"}`))
}

// channelContextSendingTest exercises the channelContext Send / User helpers.
type sendingChannel struct {
	ctx     ChannelContext
	got     string
	gotUser any
}

func (s *sendingChannel) Init(json.RawMessage) error {
	s.gotUser = s.ctx.User()
	_ = s.ctx.Send("note", map[string]any{"text": "hi"})
	s.got = "ok"
	return nil
}
func (s *sendingChannel) Dispose()                                {}
func (s *sendingChannel) OnRedisEvent([]byte)                     {}
func (s *sendingChannel) OnClientMessage(string, json.RawMessage) {}

func TestChannelContext_NilConnUserAndSend(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
	d := NewDispatcher(nil, registry, bus)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	// channelContext.User() is nil, Send returns nil error.
	d.mu.RLock()
	entry := d.channels["abc"]
	d.mu.RUnlock()
	require.NotNil(t, entry)
	ctx := &channelContext{dispatcher: d, id: "abc"}
	assert.Nil(t, ctx.User())
	assert.NoError(t, ctx.Send("note", nil))
	assert.Equal(t, "abc", ctx.ID())
}

func TestChannelContext_UnsubscribeViaContext(t *testing.T) {
	d, _, _, bus, holder := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, holder.Value)
	holder.Value.ctx.Unsubscribe("topic-a")
	bus.mu.Lock()
	defer bus.mu.Unlock()
	assert.Contains(t, bus.unsubs, "topic-a")
}

func TestDispatcher_UnsubscribeRemovesEntryAndStopsBus(t *testing.T) {
	d, _, _, bus, holder := newDispatcherWithFake(t)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, holder.Value)
	d.unsubscribe("abc", "topic-a")
	bus.mu.Lock()
	defer bus.mu.Unlock()
	assert.Contains(t, bus.unsubs, "topic-a")
}

func TestDispatcher_UnsubscribeNilBusIsNoOp(t *testing.T) {
	registry := NewRegistry()
	holder := &fakeChannelHolder{}
	registry.Register("test", func(ctx ChannelContext) Channel {
		ch := &fakeChannel{ctx: ctx}
		holder.Value = ch
		return ch
	})
	conn := NewConnection("c1", nil, newFakeConn())
	d := NewDispatcher(conn, registry, nil)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, holder.Value)
	d.unsubscribe("abc", "topic-a")
}

func TestChannelContext_SendQueuesEnvelope(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	var sc *sendingChannel
	registry.Register("test", func(ctx ChannelContext) Channel {
		sc = &sendingChannel{ctx: ctx}
		return sc
	})
	fc := newFakeConn()
	conn := NewConnection("c1", &model.User{ID: "alice"}, fc)
	d := NewDispatcher(conn, registry, bus)
	go conn.Start()
	defer conn.Close()

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.NotNil(t, sc)
	require.NotNil(t, sc.gotUser)
	user, ok := sc.gotUser.(*model.User)
	require.True(t, ok)
	assert.Equal(t, "alice", user.ID)

	require.Eventually(t, func() bool { return fc.writeCount() >= 1 }, time.Second, 10*time.Millisecond)
}

// --- pong ack ---

func TestDispatcher_ConnectPongAck(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })

	fc := newFakeConn()
	conn := NewConnection("c1", &model.User{ID: "alice"}, fc)
	d := NewDispatcher(conn, registry, bus)

	go conn.Start()
	defer conn.Close()

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test","pong":true}`))

	// `connected` envelope should be queued on the connection
	require.Eventually(t, func() bool { return fc.writeCount() >= 1 }, time.Second, 10*time.Millisecond)
	fc.mu.Lock()
	wrote := append([]byte(nil), fc.writes[0]...)
	fc.mu.Unlock()
	var env map[string]any
	require.NoError(t, json.Unmarshal(wrote, &env))
	assert.Equal(t, "connected", env["type"])
	body := env["body"].(map[string]any)
	assert.Equal(t, "abc", body["id"])
}

func TestDispatcher_ConnectNoPong_NoAck(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })

	fc := newFakeConn()
	conn := NewConnection("c1", &model.User{ID: "alice"}, fc)
	d := NewDispatcher(conn, registry, bus)

	go conn.Start()
	defer conn.Close()

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	// No `connected` envelope when pong is absent
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, fc.writeCount())
}

// --- ShouldShare ---

type shareableFakeChannel struct {
	fakeChannel
}

func (s *shareableFakeChannel) ShouldShare() bool { return true }

func TestDispatcher_ShareableChannel_DuplicateIgnored(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("shared", func(ctx ChannelContext) Channel {
		return &shareableFakeChannel{fakeChannel: fakeChannel{ctx: ctx}}
	})

	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"shared"}`))
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"b","channel":"shared"}`))

	// 1回目の接続でtopic-aがsubscribeされ、2回目はスキップされるので
	// subscribe回数は1のはず
	bus.mu.Lock()
	defer bus.mu.Unlock()
	count := 0
	for _, t := range bus.subscribed {
		if t == "topic-a" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestDispatcher_NonShareableChannel_AllowsMultiple(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })

	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"test"}`))
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"b","channel":"test"}`))

	// non-shareable なら2つのエントリが作られる
	d.mu.RLock()
	defer d.mu.RUnlock()
	assert.Len(t, d.channels, 2)
}

// #1943: 1 接続あたり maxChannelsPerConnection(32) を超えた connect は silent に無視する。
func TestDispatcher_MaxChannelsPerConnection(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	for i := 0; i < maxChannelsPerConnection; i++ {
		d.HandleClientMessage("connect", json.RawMessage(fmt.Sprintf(`{"id":"ch%d","channel":"test"}`, i)))
	}
	d.mu.RLock()
	n := len(d.channels)
	d.mu.RUnlock()
	require.Equal(t, maxChannelsPerConnection, n, "32 個までは登録できる")

	// 33 個目は無視される (silent、登録されない)。
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"overflow","channel":"test"}`))
	d.mu.RLock()
	_, exists := d.channels["overflow"]
	n = len(d.channels)
	d.mu.RUnlock()
	assert.Equal(t, maxChannelsPerConnection, n, "上限超過の connect は無視")
	assert.False(t, exists, "overflow channel は登録されない")
}

// --- OAuth2 scope check (PermittedChannel) ---

type permittedFakeChannel struct {
	fakeChannel
	kind string
}

func (p *permittedFakeChannel) RequiredPermission() string { return p.kind }

func TestConnection_Permissions_HasPermission(t *testing.T) {
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	conn.SetPermissions([]string{"read:account", "write:notes"})

	assert.Equal(t, []string{"read:account", "write:notes"}, conn.Permissions())
	assert.True(t, conn.HasPermission("read:account"))
	assert.False(t, conn.HasPermission("read:drive"))
	// 空文字列は常に true
	assert.True(t, conn.HasPermission(""))
}

func TestConnection_HasPermission_NilMeansUnrestricted(t *testing.T) {
	// session/cookie 認証では SetPermissions が呼ばれず nil のまま。
	// この場合はトークンスコープによる制限がないため、全ての kind で true。
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	assert.True(t, conn.HasPermission("read:account"))
	assert.True(t, conn.HasPermission("write:admin"))
}

func TestConnection_HasPermission_EmptySliceDenies(t *testing.T) {
	// 明示的に空スライスをセット → スコープなしトークン → 全 kind で false
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	conn.SetPermissions([]string{})
	assert.False(t, conn.HasPermission("read:account"))
	// 空 kind だけは常に true
	assert.True(t, conn.HasPermission(""))
}

func TestDispatcher_PermittedChannel_Granted(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("permitted", func(ctx ChannelContext) Channel {
		return &permittedFakeChannel{fakeChannel: fakeChannel{ctx: ctx}, kind: "read:account"}
	})

	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	conn.SetPermissions([]string{"read:account"})
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"permitted"}`))

	d.mu.RLock()
	defer d.mu.RUnlock()
	assert.Len(t, d.channels, 1)
}

func TestDispatcher_PermittedChannel_Denied(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("permitted", func(ctx ChannelContext) Channel {
		return &permittedFakeChannel{fakeChannel: fakeChannel{ctx: ctx}, kind: "read:account"}
	})

	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	conn.SetPermissions([]string{"write:notes"})
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"permitted"}`))

	d.mu.RLock()
	defer d.mu.RUnlock()
	assert.Len(t, d.channels, 0)
}

func TestDispatcher_PermittedChannel_SessionAuthAllowed(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("permitted", func(ctx ChannelContext) Channel {
		return &permittedFakeChannel{fakeChannel: fakeChannel{ctx: ctx}, kind: "read:account"}
	})

	// permissions を set しない (session/cookie 認証相当) → フルアクセス扱い
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"permitted"}`))

	d.mu.RLock()
	defer d.mu.RUnlock()
	assert.Len(t, d.channels, 1)
}

func TestDispatcher_PermittedChannel_EmptyPermissionsDenied(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("permitted", func(ctx ChannelContext) Channel {
		return &permittedFakeChannel{fakeChannel: fakeChannel{ctx: ctx}, kind: "read:account"}
	})

	// 明示的に空スライスをセット (スコープなしトークン) → 拒否
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	conn.SetPermissions([]string{})
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"permitted"}`))

	d.mu.RLock()
	defer d.mu.RUnlock()
	assert.Len(t, d.channels, 0)
}

func TestDispatcher_PermittedChannel_EmptyKindAllowed(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("optional", func(ctx ChannelContext) Channel {
		return &permittedFakeChannel{fakeChannel: fakeChannel{ctx: ctx}, kind: ""}
	})

	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"optional"}`))

	d.mu.RLock()
	defer d.mu.RUnlock()
	assert.Len(t, d.channels, 1)
}

func TestDispatcher_NonPermittedChannel_AlwaysAllowed(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("plain", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })

	// PermittedChannel を実装していないチャンネルは permission なくても接続可能
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"plain"}`))

	d.mu.RLock()
	defer d.mu.RUnlock()
	assert.Len(t, d.channels, 1)
}

// #2106 L58: 既接続 shareable channel への重複 connect は pong=true でも connected ack を
// 返さない (upstream Connection.ts は silent return)。
func TestDispatcher_ShareableNoPongAck(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("shared", func(ctx ChannelContext) Channel {
		return &shareableFakeChannel{fakeChannel: fakeChannel{ctx: ctx}}
	})

	fc := newFakeConn()
	conn := NewConnection("c1", &model.User{ID: "alice"}, fc)
	d := NewDispatcher(conn, registry, bus)

	go conn.Start()
	defer conn.Close()

	// 1回目: 作成され connected ack が返る。
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"a","channel":"shared","pong":true}`))
	require.Eventually(t, func() bool { return fc.writeCount() >= 1 }, time.Second, 10*time.Millisecond)

	// 2回目: 既存共有なので ack しない (write が増えない)。
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"b","channel":"shared","pong":true}`))
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, fc.writeCount(), "既接続 shareable への重複 connect は ack しない")
}

// --- Init error path ---

func TestDispatcher_InitError_RollsBackChannel(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel {
		return &fakeChannel{ctx: ctx, initErr: ErrInvalidParams}
	})

	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))

	// Init が error を返したら channel は登録されない
	d.mu.RLock()
	defer d.mu.RUnlock()
	assert.Len(t, d.channels, 0)
}

func TestDispatcher_InitError_NoPongAck(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel {
		return &fakeChannel{ctx: ctx, initErr: ErrInvalidParams}
	})

	fc := newFakeConn()
	conn := NewConnection("c1", &model.User{ID: "alice"}, fc)
	d := NewDispatcher(conn, registry, bus)

	go conn.Start()
	defer conn.Close()

	// pong=true でも Init error 時は ack を送らない (TS 互換)
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test","pong":true}`))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, fc.writeCount())
}

// initSubscribingChannel is a test channel that Subscribe時に失敗する Init を
// 持つ。Dispatcher が removeChannel で topic refcount を適切に戻すことを
// 確認するための helper。
type initSubscribingChannel struct {
	ctx          ChannelContext
	disposeCount int
}

func (c *initSubscribingChannel) Init(json.RawMessage) error {
	c.ctx.Subscribe("dangling-topic")
	return ErrInvalidParams
}
func (c *initSubscribingChannel) OnRedisEvent([]byte)                     {}
func (c *initSubscribingChannel) OnClientMessage(string, json.RawMessage) {}
func (c *initSubscribingChannel) Dispose()                                { c.disposeCount++ }

func TestDispatcher_InitError_CleansUpSubscriptions(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	var created *initSubscribingChannel
	registry.Register("test", func(ctx ChannelContext) Channel {
		created = &initSubscribingChannel{ctx: ctx}
		return created
	})

	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))

	// Init 中に Subscribe した topic が Dispatcher の map から除去され、
	// bus からも Unsubscribe されている必要がある。
	d.mu.RLock()
	_, topicRemains := d.topics["dangling-topic"]
	d.mu.RUnlock()
	assert.False(t, topicRemains)

	bus.mu.Lock()
	defer bus.mu.Unlock()
	assert.Contains(t, bus.unsubs, "dangling-topic")
	// Dispose も呼ばれる
	assert.Equal(t, 1, created.disposeCount)
}

// --- readNotification / subNote / unsubNote tests ---

type stubNotifReader struct {
	called int
	lastID string
}

func (s *stubNotifReader) ReadAll(userID string) error {
	s.called++
	s.lastID = userID
	return nil
}

// stubNoteVisibility implements NoteVisibilityChecker for subNote tests
// (#1460)。allowed[noteID] = true なら visible、defaultAllow=true なら
// allowed 未登録 noteID も visible 扱い (permissive)。lastViewer / calls
// で checker が dispatch から正しい引数で呼ばれたか sanity check できる。
type stubNoteVisibility struct {
	allowed      map[string]bool
	defaultAllow bool
	lastViewer   *model.User
	calls        int
}

func (s *stubNoteVisibility) RequireVisible(viewer *model.User, noteID string) (*model.Note, error) {
	s.lastViewer = viewer
	s.calls++
	if v, ok := s.allowed[noteID]; ok {
		if v {
			return &model.Note{ID: noteID}, nil
		}
		return nil, errors.New("not visible")
	}
	if s.defaultAllow {
		return &model.Note{ID: noteID}, nil
	}
	return nil, errors.New("not visible")
}

func TestDispatcher_ReadNotification(t *testing.T) {
	conn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	nr := &stubNotifReader{}
	d.SetNotificationReader(nr)

	d.HandleClientMessage("readNotification", nil)
	assert.Equal(t, 1, nr.called)
	assert.Equal(t, "alice", nr.lastID)
}

func TestDispatcher_ReadNotification_NoReader(t *testing.T) {
	conn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, nil, nil)
	// notifReader未設定でもpanic しない
	d.HandleClientMessage("readNotification", nil)
}

func TestDispatcher_SubNote_UnsubNote(t *testing.T) {
	conn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	// #1460: subNote は noteVisibility 必須 (未配線時は fail-closed で
	// subscribe しない)。本 test の主旨は refcount semantics なので permissive
	// stub を wire して旧挙動を維持する。
	d.SetNoteVisibilityChecker(&stubNoteVisibility{defaultAllow: true})

	// subscribe
	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))
	bus.mu.Lock()
	_, subbed := bus.subs["noteStream:n1"]
	bus.mu.Unlock()
	assert.True(t, subbed)

	// duplicate subscribe: refcount increments, no duplicate bus.Subscribe
	d.HandleClientMessage("subNote", json.RawMessage(`{"id":"n1"}`))
	bus.mu.Lock()
	assert.Equal(t, 1, countOccurrences(bus.subscribed, "noteStream:n1"))
	bus.mu.Unlock()

	// unsubscribe once: refcount decrements but still > 0
	d.HandleClientMessage("un", json.RawMessage(`{"id":"n1"}`))
	bus.mu.Lock()
	_, stillSubbed := bus.subs["noteStream:n1"]
	bus.mu.Unlock()
	assert.True(t, stillSubbed)

	// unsubscribe again: refcount reaches 0, bus.Unsubscribe called
	d.HandleClientMessage("unsubNote", json.RawMessage(`{"id":"n1"}`))
	bus.mu.Lock()
	_, stillSubbed2 := bus.subs["noteStream:n1"]
	bus.mu.Unlock()
	assert.False(t, stillSubbed2)
}

func TestDispatcher_SubNote_InvalidBody(t *testing.T) {
	conn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, nil, newStubBus())
	// #1460: 空 body は parse 直後に return するので visibility check 自体
	// 到達しないが、interface gate 配線後でも panic しないことを確認するため
	// permissive stub を wire しておく。
	d.SetNoteVisibilityChecker(&stubNoteVisibility{defaultAllow: true})
	// empty body → no panic
	d.HandleClientMessage("s", json.RawMessage(`{}`))
	d.HandleClientMessage("un", json.RawMessage(`{}`))
}

// TS互換性検証: forwardNoteEvent は noteStream:{id} に届いた
// {type, body} envelope を、クライアントに `{type: "noteUpdated",
// body: {id, type, body}}` という外殻で送信する必要がある (TS
// NoteUpdatedEvent discriminated union)。各 subtype (reacted /
// unreacted / deleted / pollVoted) で形状が維持されることを確認する。
func TestDispatcher_ForwardNoteEvent_TSEnvelope(t *testing.T) {
	cases := []struct {
		name   string
		inner  string
		evType string
	}{
		{"reacted", `{"type":"reacted","body":{"reaction":":smile:","userId":"u1"}}`, "reacted"},
		{"unreacted", `{"type":"unreacted","body":{"reaction":":smile:","userId":"u1"}}`, "unreacted"},
		{"deleted", `{"type":"deleted","body":{"deletedAt":"2026-04-19T00:00:00.000Z"}}`, "deleted"},
		{"pollVoted", `{"type":"pollVoted","body":{"choice":1,"userId":"u1"}}`, "pollVoted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeConn()
			conn := NewConnection("test", &model.User{ID: "alice"}, fc)
			go conn.Start()
			defer conn.Close()
			bus := newStubBus()
			d := NewDispatcher(conn, nil, bus)
			// #1460: subNote の visibility gate を permissive stub で素通り。
			d.SetNoteVisibilityChecker(&stubNoteVisibility{defaultAllow: true})
			d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

			bus.deliver("noteStream:n1", []byte(tc.inner))

			require.Eventually(t, func() bool { return fc.writeCount() >= 1 }, time.Second, 5*time.Millisecond)
			fc.mu.Lock()
			writes := append([][]byte(nil), fc.writes...)
			fc.mu.Unlock()
			require.Len(t, writes, 1)

			var outer struct {
				Type string `json:"type"`
				Body struct {
					ID   string          `json:"id"`
					Type string          `json:"type"`
					Body json.RawMessage `json:"body"`
				} `json:"body"`
			}
			require.NoError(t, json.Unmarshal(writes[0], &outer))
			assert.Equal(t, "noteUpdated", outer.Type)
			assert.Equal(t, "n1", outer.Body.ID)
			assert.Equal(t, tc.evType, outer.Body.Type)
			// inner body は json.RawMessage として原形保存される (TS schema を
			// そのまま frontend に渡す)。
			assert.NotEmpty(t, outer.Body.Body)
		})
	}
}

func TestDispatcher_ForwardNoteEvent_InvalidPayload(t *testing.T) {
	fc := newFakeConn()
	conn := NewConnection("test", &model.User{ID: "alice"}, fc)
	go conn.Start()
	defer conn.Close()
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	// #1460: subNote の visibility gate を permissive stub で素通り。
	d.SetNoteVisibilityChecker(&stubNoteVisibility{defaultAllow: true})
	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

	// invalid JSON / 空 type のどちらも Send しない (早期 return)。
	bus.deliver("noteStream:n1", []byte(`not-json`))
	bus.deliver("noteStream:n1", []byte(`{"type":""}`))

	// すぐには届かない可能性もあるため少し待ってから確認。
	time.Sleep(50 * time.Millisecond)
	fc.mu.Lock()
	n := len(fc.writes)
	fc.mu.Unlock()
	assert.Zero(t, n)
}

// --- #1460 subNote visibility gate tests ---

// TestDispatcher_SubNote_HiddenNote_NoSubscribe は非可視 note への subNote
// を checker が拒否した場合、bus.Subscribe / refcount どちらにも痕跡が
// 残らないことを確認する。refcount 加算前で gate しないと、後から
// unsubNote で d.noteSubs map state を leak する経路が残るため。
func TestDispatcher_SubNote_HiddenNote_NoSubscribe(t *testing.T) {
	conn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	stub := &stubNoteVisibility{allowed: map[string]bool{"n1": false}}
	d.SetNoteVisibilityChecker(stub)

	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

	// checker が呼ばれ、viewer が conn.User() で渡っている。
	assert.Equal(t, 1, stub.calls)
	require.NotNil(t, stub.lastViewer)
	assert.Equal(t, "alice", stub.lastViewer.ID)

	bus.mu.Lock()
	_, subbed := bus.subs["noteStream:n1"]
	subbedSlice := append([]string(nil), bus.subscribed...)
	bus.mu.Unlock()
	assert.False(t, subbed, "should not subscribe to hidden note")
	assert.NotContains(t, subbedSlice, "noteStream:n1")

	// refcount も加算されていない。後続の unsubNote が noise として
	// d.noteSubs map state を leak しないため必須。
	d.noteSubMu.Lock()
	cnt := d.noteSubs["n1"]
	d.noteSubMu.Unlock()
	assert.Equal(t, 0, cnt)
}

// TestDispatcher_SubNote_HiddenNote_NoEventForwarded は subNote 拒否後に
// note の publisher が event を流しても、subscribe 自体が存在しないため
// client 側 write が 1 件も発生しないことを end-to-end で確認する
// (issue #1460 完了条件の「event を受け取らない」を担保)。
func TestDispatcher_SubNote_HiddenNote_NoEventForwarded(t *testing.T) {
	fc := newFakeConn()
	conn := NewConnection("test", &model.User{ID: "alice"}, fc)
	go conn.Start()
	defer conn.Close()
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	d.SetNoteVisibilityChecker(&stubNoteVisibility{allowed: map[string]bool{"n1": false}})

	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))
	// publisher が event を流しても deliver 先 handler が登録されていない
	// ので no-op になる。
	bus.deliver("noteStream:n1", []byte(`{"type":"reacted","body":{"reaction":":smile:","userId":"u1"}}`))

	time.Sleep(50 * time.Millisecond)
	fc.mu.Lock()
	n := len(fc.writes)
	fc.mu.Unlock()
	assert.Zero(t, n, "no event should reach client for denied subscribe")
}

// TestDispatcher_SubNote_PublicNote_Subscribes は permissive checker (=
// public/home/visible note) の場合、既存の subscribe + event forward が
// regress していないことを確認する。
func TestDispatcher_SubNote_PublicNote_Subscribes(t *testing.T) {
	conn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	d.SetNoteVisibilityChecker(&stubNoteVisibility{defaultAllow: true})

	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

	bus.mu.Lock()
	_, subbed := bus.subs["noteStream:n1"]
	bus.mu.Unlock()
	assert.True(t, subbed)
	d.noteSubMu.Lock()
	cnt := d.noteSubs["n1"]
	d.noteSubMu.Unlock()
	assert.Equal(t, 1, cnt)
}

// TestDispatcher_SubNote_Author_Subscribes は author 自身が自分の
// followers note を sub する経路。stubNoteVisibility は viewer.ID を見て
// allow するわけではなく allowed[noteID] map を見るので、本 test では
// 「allow されている noteID なら viewer が誰であっても通る」ことを確認する
// (実本番では QueryService.RequireVisible が CanSeeNote 経由で viewer.ID
// == note.UserID を見て author を素通しする)。
func TestDispatcher_SubNote_Author_Subscribes(t *testing.T) {
	conn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	stub := &stubNoteVisibility{allowed: map[string]bool{"my-note": true}}
	d.SetNoteVisibilityChecker(stub)

	d.HandleClientMessage("s", json.RawMessage(`{"id":"my-note"}`))

	require.NotNil(t, stub.lastViewer)
	assert.Equal(t, "alice", stub.lastViewer.ID)
	bus.mu.Lock()
	_, subbed := bus.subs["noteStream:my-note"]
	bus.mu.Unlock()
	assert.True(t, subbed)
}

// TestDispatcher_SubNote_Anonymous_HiddenNote_NoSubscribe は匿名 conn
// (User()==nil) からの非可視 sub が、checker に nil viewer を渡した上で
// 拒否されることを確認する。CanSeeNote は nil viewer + followers/specified
// を必ず false にするので、本経路は production でも自動的に塞がる。
func TestDispatcher_SubNote_Anonymous_HiddenNote_NoSubscribe(t *testing.T) {
	conn := NewConnection("test", nil, newFakeConn())
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	stub := &stubNoteVisibility{allowed: map[string]bool{"n1": false}}
	d.SetNoteVisibilityChecker(stub)

	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

	assert.Equal(t, 1, stub.calls)
	assert.Nil(t, stub.lastViewer, "anonymous viewer should be passed as nil to checker")
	bus.mu.Lock()
	_, subbed := bus.subs["noteStream:n1"]
	bus.mu.Unlock()
	assert.False(t, subbed)
}

// TestDispatcher_SubNote_QueryServiceNil_FailClosed は checker が wire
// されていないとき (production wiring drift / test の partial setup)
// subscribe を一切させない fail-closed 挙動を確認する。notifications
// #1444 と同 doctrine。
func TestDispatcher_SubNote_QueryServiceNil_FailClosed(t *testing.T) {
	conn := NewConnection("test", &model.User{ID: "alice"}, newFakeConn())
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	// 意図的に SetNoteVisibilityChecker を呼ばない。

	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

	bus.mu.Lock()
	_, subbed := bus.subs["noteStream:n1"]
	bus.mu.Unlock()
	assert.False(t, subbed, "fail-closed: no checker should block all subscribes")
}

func countOccurrences(slice []string, target string) int {
	n := 0
	for _, s := range slice {
		if s == target {
			n++
		}
	}
	return n
}

// #787: channelContext.HardMuteRules forwards from Connection.HardMuteRules.
func TestChannelContext_HardMuteRules(t *testing.T) {
	conn := NewConnection("c1", nil, newFakeConn())
	conn.SetHardMuteRules([]byte(`["foo"]`))
	d := NewDispatcher(conn, nil, newStubBus())
	ctx := &channelContext{dispatcher: d, id: "ch1"}
	if got := ctx.HardMuteRules(); string(got) != `["foo"]` {
		t.Fatalf("HardMuteRules = %q", got)
	}
}

// nil connection (= disconnected) でも panic せず nil を返す。
func TestChannelContext_HardMuteRules_NilConn(t *testing.T) {
	d := &Dispatcher{}
	ctx := &channelContext{dispatcher: d, id: "ch1"}
	if got := ctx.HardMuteRules(); got != nil {
		t.Fatalf("expected nil, got %q", got)
	}
}

// #1711: channelContext.MuteBlockSnapshot forwards from Connection.
func TestChannelContext_MuteBlockSnapshot(t *testing.T) {
	conn := NewConnection("c1", nil, newFakeConn())
	conn.SetMuteBlockSnapshot(&MuteBlockSnapshot{Muting: map[string]struct{}{"u1": {}}})
	d := NewDispatcher(conn, nil, newStubBus())
	ctx := &channelContext{dispatcher: d, id: "ch1"}
	got := ctx.MuteBlockSnapshot()
	if got == nil || len(got.Muting) != 1 {
		t.Fatalf("MuteBlockSnapshot = %v", got)
	}
}

// nil connection (= disconnected) でも panic せず nil を返す。
func TestChannelContext_MuteBlockSnapshot_NilConn(t *testing.T) {
	d := &Dispatcher{}
	ctx := &channelContext{dispatcher: d, id: "ch1"}
	if got := ctx.MuteBlockSnapshot(); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// #1942: channelContext.UserPolicies forwards from Connection.
func TestChannelContext_UserPolicies(t *testing.T) {
	conn := NewConnection("c1", nil, newFakeConn())
	conn.SetPolicies(map[string]any{"ltlAvailable": false, "gtlAvailable": true})
	d := NewDispatcher(conn, nil, newStubBus())
	ctx := &channelContext{dispatcher: d, id: "ch1"}
	got := ctx.UserPolicies()
	if got == nil || got["ltlAvailable"] != false || got["gtlAvailable"] != true {
		t.Fatalf("UserPolicies = %v", got)
	}
}

func TestChannelContext_UserPolicies_NilConn(t *testing.T) {
	d := &Dispatcher{}
	ctx := &channelContext{dispatcher: d, id: "ch1"}
	if got := ctx.UserPolicies(); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// #2106 H6: stream の 'sr' message は note 購読のみ行い、全通知既読 (readAllNotification)
// を起こしてはならない。frontend は note capture の度に 'sr' を送るため。
type fakeNotifReader struct{ called int }

func (f *fakeNotifReader) ReadAll(string) error { f.called++; return nil }

func TestDispatcher_SrDoesNotReadAllNotifications(t *testing.T) {
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, NewRegistry(), newStubBus())
	nr := &fakeNotifReader{}
	d.SetNotificationReader(nr)

	d.HandleClientMessage("sr", json.RawMessage(`{"id":"note1"}`))
	assert.Equal(t, 0, nr.called, "'sr' (subNote alias) must NOT trigger readAllNotifications")

	// 正規の readNotification は引き続き全通知既読を行う。
	d.HandleClientMessage("readNotification", json.RawMessage(`{}`))
	assert.Equal(t, 1, nr.called, "'readNotification' must mark all notifications read")
}

// **subNote は 1 接続あたり maxNoteSubsPerConnection 件まで。** 超えたら最も
// 古く購読したものを外す (LRU)。購読 1 件ごとに Redis の接続を張るので、上限が
// 無いと 1 本の接続から Redis の接続を無制限に増やせる。
func TestDispatcher_SubNote_CapEvictsOldest(t *testing.T) {
	conn := NewConnection("test", nil, newFakeConn())
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	d.SetNoteVisibilityChecker(&stubNoteVisibility{defaultAllow: true})

	sub := func(id string) {
		d.HandleClientMessage("subNote", json.RawMessage(`{"id":"`+id+`"}`))
	}
	subbed := func(id string) bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		_, ok := bus.subs["noteStream:"+id]
		return ok
	}

	for i := range maxNoteSubsPerConnection {
		sub(fmt.Sprintf("n%d", i))
	}
	// n0 を購読し直すと最も新しい扱いになり、次に外れるのは n1。
	sub("n0")

	sub("over")
	d.noteSubMu.Lock()
	held := len(d.noteSubs)
	d.noteSubMu.Unlock()
	assert.Equal(t, maxNoteSubsPerConnection, held, "上限を超えて保持しない")
	assert.True(t, subbed("over"))
	assert.True(t, subbed("n0"), "購読し直したものは残る")
	assert.False(t, subbed("n1"), "最も古いものが bus から外れる")

	// 外れたものへの unsubNote は何も起こさない。
	d.HandleClientMessage("unsubNote", json.RawMessage(`{"id":"n1"}`))
	assert.True(t, subbed("n2"))

	// unsubNote で空いた枠は次の購読で使え、誰も押し出さない。
	d.HandleClientMessage("unsubNote", json.RawMessage(`{"id":"n2"}`))
	d.HandleClientMessage("unsubNote", json.RawMessage(`{"id":"n0"}`))
	d.HandleClientMessage("unsubNote", json.RawMessage(`{"id":"n0"}`))
	sub("new1")
	assert.True(t, subbed("n3"), "空いた枠があれば押し出さない")
}
