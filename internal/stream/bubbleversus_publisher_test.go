package stream

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/bubbleversus"
	"github.com/shiroha-a/mk/internal/model"
)

func decodeEnvelope(t *testing.T, payload any) (string, map[string]any) {
	t.Helper()
	raw, ok := payload.(json.RawMessage)
	require.True(t, ok)
	var env struct {
		Type string         `json:"type"`
		Body map[string]any `json:"body"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	return env.Type, env.Body
}

func TestBubbleVersusPublisher(t *testing.T) {
	bus := &capturingPubSub{}
	p := NewBubbleVersusPublisher(bus)

	p.PublishInvited("bob", &model.User{ID: "alice", Username: "alice"}, &bubbleversus.Match{ID: "m1", GameMode: "square"})
	p.PublishUser("alice", "accepted", map[string]any{"matchId": "m1"})
	p.PublishMatch("m1", "attack", map[string]any{"from": "alice", "count": 3})
	// 送り先の無い呼び出しは何も出さない。
	p.PublishInvited("bob", nil, &bubbleversus.Match{ID: "m1"})
	p.PublishInvited("bob", &model.User{ID: "alice"}, nil)

	require.Len(t, bus.calls, 3)
	assert.Equal(t, "bubbleVersus:bob", bus.calls[0].topic)
	typ, body := decodeEnvelope(t, bus.calls[0].payload)
	assert.Equal(t, "invited", typ)
	assert.Equal(t, "m1", body["matchId"])
	assert.Equal(t, "square", body["gameMode"])
	user, ok := body["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", user["id"])

	assert.Equal(t, "bubbleVersus:alice", bus.calls[1].topic)
	typ, _ = decodeEnvelope(t, bus.calls[1].payload)
	assert.Equal(t, "accepted", typ)

	assert.Equal(t, "bubbleVersusMatch:m1", bus.calls[2].topic)
	typ, body = decodeEnvelope(t, bus.calls[2].payload)
	assert.Equal(t, "attack", typ)
	assert.EqualValues(t, 3, body["count"])
}

func TestBubbleVersusPublisher_Failures(t *testing.T) {
	NewBubbleVersusPublisher(nil).PublishMatch("m1", "x", nil) // no panic
	var nilPub *BubbleVersusPublisher
	nilPub.PublishMatch("m1", "x", nil)
	NewBubbleVersusPublisher(&capturingPubSub{err: errors.New("boom")}).PublishMatch("m1", "x", nil)
	bus := &capturingPubSub{}
	NewBubbleVersusPublisher(bus).PublishMatch("m1", "x", func() {}) // marshal failure
	assert.Empty(t, bus.calls)
}
