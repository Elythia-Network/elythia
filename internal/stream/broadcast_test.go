package stream

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2046: SubscribeBroadcast は broadcast topic の event を全 connection
// (認証/匿名問わず) へ top-level {type, body} で forward する。
func TestManager_SubscribeBroadcast_ForwardsToAllConnections(t *testing.T) {
	bus := newStubBus()
	m := NewManager(NewRegistry(), bus)

	fc1, fc2 := newFakeConn(), newFakeConn()
	c1 := NewConnection("c1", &model.User{ID: "alice"}, fc1)
	c2 := NewConnection("c2", nil, fc2) // anonymous connection も受信する
	go c1.Start()
	go c2.Start()
	m.register(c1)
	m.register(c2)
	t.Cleanup(func() { c1.Close(); c2.Close() })

	m.SubscribeBroadcast()
	require.Contains(t, bus.subs, BroadcastTopic)

	env, _ := json.Marshal(BroadcastEnvelope{Type: "emojiAdded", Body: json.RawMessage(`{"emoji":{"id":"e1"}}`)})
	bus.deliver(BroadcastTopic, env)

	for _, fc := range []*fakeConn{fc1, fc2} {
		require.Eventually(t, func() bool { return fc.writeCount() >= 1 }, time.Second, 5*time.Millisecond)
		var msg struct {
			Type string          `json:"type"`
			Body json.RawMessage `json:"body"`
		}
		require.NoError(t, json.Unmarshal(fc.writeAt(0), &msg))
		assert.Equal(t, "emojiAdded", msg.Type)
		assert.JSONEq(t, `{"emoji":{"id":"e1"}}`, string(msg.Body))
	}
}

// #2046: 不正な broadcast payload は drop され forward されない。
func TestManager_SubscribeBroadcast_DropsInvalidPayload(t *testing.T) {
	bus := newStubBus()
	m := NewManager(NewRegistry(), bus)
	fc := newFakeConn()
	c := NewConnection("c1", &model.User{ID: "alice"}, fc)
	go c.Start()
	m.register(c)
	t.Cleanup(func() { c.Close() })

	m.SubscribeBroadcast()
	bus.deliver(BroadcastTopic, []byte(`not-json`))
	bus.deliver(BroadcastTopic, []byte(`{"type":"","body":null}`)) // type 空も drop

	// forward されないことを確認 (短時間待っても write 0)。
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, 0, fc.writeCount())
}

// #2046: BroadcastPublisher は {type, body} envelope を broadcast topic へ publish する。
func TestBroadcastPublisher_PublishBroadcast(t *testing.T) {
	bus := &capturingPubSub{}
	bp := NewBroadcastPublisher(bus)
	bp.PublishBroadcast("emojiDeleted", map[string]any{"emojis": []any{map[string]any{"id": "e1"}}})

	require.Len(t, bus.calls, 1)
	assert.Equal(t, BroadcastTopic, bus.calls[0].topic)
	raw, ok := bus.calls[0].payload.(json.RawMessage)
	require.True(t, ok)
	var env BroadcastEnvelope
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, "emojiDeleted", env.Type)
	assert.JSONEq(t, `{"emojis":[{"id":"e1"}]}`, string(env.Body))
}

// #2046: nil pub / 空 type は no-op (panic しない)。
func TestBroadcastPublisher_NilAndEmpty(t *testing.T) {
	NewBroadcastPublisher(nil).PublishBroadcast("emojiAdded", nil) // no panic
	bus := &capturingPubSub{}
	NewBroadcastPublisher(bus).PublishBroadcast("", map[string]any{"x": 1})
	assert.Empty(t, bus.calls)
}
