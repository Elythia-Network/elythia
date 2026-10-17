package cachebus

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/event"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var (
	testRedis *testutil.TestRedis
	testDB    *gorm.DB
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("failed to setup redis: %v", err)
	}
	testDB, err = testutil.OpenTestDB()
	if err != nil {
		testRedis.Teardown(ctx)
		log.Fatalf("failed to open test DB: %v", err)
	}
	testutil.ApplyMigrations(testDB)
	code := m.Run()
	testRedis.Teardown(ctx)
	os.Exit(code)
}

// twoBuses returns the internal pub/sub of two "processes" sharing one Redis.
// prefix keeps the tests of this package apart.
func twoBuses(t *testing.T, prefix string) (*event.PubSubService, *event.PubSubService) {
	t.Helper()
	a := event.NewPubSubService(testRedis.Client, prefix)
	b := event.NewPubSubService(testRedis.Client, prefix)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

// waitSubscribed blocks until every bus receives a probe, i.e. until their
// PSUBSCRIBE is in place. A publish sent before that is not delivered.
func waitSubscribed(t *testing.T, buses ...*event.PubSubService) {
	t.Helper()
	got := make([]atomic.Bool, len(buses))
	for i, b := range buses {
		stop := b.Subscribe(context.Background(), "probe", func([]byte) { got[i].Store(true) })
		t.Cleanup(stop)
	}
	require.Eventually(t, func() bool {
		_ = buses[0].Publish(context.Background(), "probe", struct{}{})
		for i := range got {
			if !got[i].Load() {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "pub/sub subscriptions were not established")
}

// loopBus is an in-memory Bus. Publish records the message and, unless
// publishErr is set, hands it to the handler of the topic synchronously.
type loopBus struct {
	mu         sync.Mutex
	handlers   map[string]func([]byte)
	published  map[string][][]byte
	publishErr error
}

func newLoopBus() *loopBus {
	return &loopBus{handlers: map[string]func([]byte){}, published: map[string][][]byte{}}
}

func (b *loopBus) Publish(_ context.Context, channel string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.publishErr != nil {
		b.mu.Unlock()
		return b.publishErr
	}
	b.published[channel] = append(b.published[channel], raw)
	h := b.handlers[channel]
	b.mu.Unlock()
	if h != nil {
		h(raw)
	}
	return nil
}

func (b *loopBus) Subscribe(_ context.Context, channel string, handler func([]byte)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[channel] = handler
	return func() {
		b.mu.Lock()
		delete(b.handlers, channel)
		b.mu.Unlock()
	}
}

func (b *loopBus) setPublishErr(err error) {
	b.mu.Lock()
	b.publishErr = err
	b.mu.Unlock()
}

// deliver hands raw to the handler of channel as if another process sent it.
func (b *loopBus) deliver(t *testing.T, channel string, raw []byte) {
	t.Helper()
	b.mu.Lock()
	h := b.handlers[channel]
	b.mu.Unlock()
	require.NotNil(t, h, "no handler for %s", channel)
	h(raw)
}

// deliverMessage encodes msg and delivers it on channel.
func deliverMessage[T any](t *testing.T, b *loopBus, channel string, msg Message[T]) {
	t.Helper()
	raw, err := json.Marshal(msg)
	require.NoError(t, err)
	b.deliver(t, channel, raw)
}

// messages decodes what was published on channel.
func messages[T any](t *testing.T, b *loopBus, channel string) []Message[T] {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Message[T], 0, len(b.published[channel]))
	for _, raw := range b.published[channel] {
		var m Message[T]
		require.NoError(t, json.Unmarshal(raw, &m))
		out = append(out, m)
	}
	return out
}

func TestPublishSync_FailureIsLoggedNotReturned(t *testing.T) {
	bus := newLoopBus()
	bus.setPublishErr(errors.New("redis down"))
	// 失敗しても panic せず、書き込みの経路を止めない。
	publishSync(bus, "x", "me", struct{}{})
	require.Empty(t, bus.published["x"])
}

// overflowLoopBus is a loopBus that also records the overflow functions, as
// event.PubSubService takes them.
type overflowLoopBus struct {
	*loopBus
	overflow map[string]func()
}

func (b *overflowLoopBus) SubscribeWithOverflow(ctx context.Context, channel string, handler func([]byte), onOverflow func()) func() {
	b.mu.Lock()
	b.overflow[channel] = onOverflow
	b.mu.Unlock()
	return b.Subscribe(ctx, channel, handler)
}

// TestSubscribe_ReceiveOverflowDropsEverything pins that when the receiving
// queue dropped messages, the cache is dropped as a whole, like on a message
// it cannot read (#3664).
func TestSubscribe_ReceiveOverflowDropsEverything(t *testing.T) {
	bus := &overflowLoopBus{loopBus: newLoopBus(), overflow: map[string]func(){}}
	var applied, dropped int
	stop := subscribe(bus, "t", "me", func(struct{}) { applied++ }, func() { dropped++ })
	defer stop()
	require.NotNil(t, bus.overflow["t"])
	bus.overflow["t"]()
	assert.Equal(t, 1, dropped)
	assert.Equal(t, 0, applied)
	deliverMessage(t, bus.loopBus, "t", Message[struct{}]{From: "other"})
	assert.Equal(t, 1, applied)
}

// TestSubscribe_RealPubSubReportsOverflow pins that event.PubSubService is
// recognised as a Bus that reports overflow.
func TestSubscribe_RealPubSubReportsOverflow(t *testing.T) {
	var bus Bus = event.NewPubSubService(testRedis.Client, "cboverflow3664:")
	_, ok := bus.(overflowBus)
	assert.True(t, ok)
}
