package stream

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturingPubSub struct {
	calls []capturedPublish
	err   error
}

type capturedPublish struct {
	topic   string
	payload any
}

func (c *capturingPubSub) Publish(_ context.Context, topic string, payload any) error {
	if c.err != nil {
		return c.err
	}
	c.calls = append(c.calls, capturedPublish{topic, payload})
	return nil
}

func TestReversiGamePublisher_PublishEnvelope(t *testing.T) {
	bus := &capturingPubSub{}
	p := NewReversiGamePublisher(bus, nil)
	p.PublishGameEvent("g1", "log", map[string]any{"pos": 19})

	require.Len(t, bus.calls, 1)
	assert.Equal(t, "reversiGame:g1", bus.calls[0].topic)

	raw, ok := bus.calls[0].payload.(json.RawMessage)
	require.True(t, ok)
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, "log", env.Type)
}

func TestReversiGamePublisher_NilPub(t *testing.T) {
	p := NewReversiGamePublisher(nil, nil)
	p.PublishGameEvent("g1", "log", nil) // no panic
}

func TestReversiGamePublisher_EmptyGameID(t *testing.T) {
	bus := &capturingPubSub{}
	p := NewReversiGamePublisher(bus, nil)
	p.PublishGameEvent("", "log", nil)
	assert.Empty(t, bus.calls)
}

func TestReversiGamePublisher_PublishError(t *testing.T) {
	bus := &capturingPubSub{err: errors.New("pub boom")}
	p := NewReversiGamePublisher(bus, nil)
	p.PublishGameEvent("g1", "log", nil) // logged but not panicked
}
