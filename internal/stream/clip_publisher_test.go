package stream

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClipStreamPublisher_PublishEnvelope(t *testing.T) {
	bus := &capturingPubSub{}
	NewClipStreamPublisher(bus).PublishClipEvent("clip1", "updated")

	require.Len(t, bus.calls, 1)
	assert.Equal(t, "clipStream:clip1", bus.calls[0].topic)
	raw, ok := bus.calls[0].payload.(json.RawMessage)
	require.True(t, ok)
	assert.JSONEq(t, `{"type":"updated","body":null}`, string(raw))
}

func TestClipStreamPublisher_SkipsWithoutTarget(t *testing.T) {
	NewClipStreamPublisher(nil).PublishClipEvent("clip1", "updated") // no panic

	bus := &capturingPubSub{}
	p := NewClipStreamPublisher(bus)
	p.PublishClipEvent("", "updated")
	p.PublishClipEvent("clip1", "")
	assert.Empty(t, bus.calls)
}

func TestClipStreamPublisher_PublishError(t *testing.T) {
	bus := &capturingPubSub{err: errors.New("pub boom")}
	NewClipStreamPublisher(bus).PublishClipEvent("clip1", "deleted") // logged, not panicked
}
