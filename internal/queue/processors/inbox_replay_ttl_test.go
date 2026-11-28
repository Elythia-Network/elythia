package processors_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

// defaultInboxMaxRetryDelay is the longest retry delay of the default
// inboxJobMaxAttempts (8, internal/server defaultInboxJobMaxAttempts).
var defaultInboxMaxRetryDelay = mkqdriver.FederationMaxRetryDelay(8)

// replayTTLHarness runs the real Redis guard on miniredis, whose clock the
// tests move with FastForward.
type replayTTLHarness struct {
	mr   *miniredis.Miniredis
	stub *stubFedProcessor
	p    *processors.InboxProcessor
	task driver.RawTask
	ttl  time.Duration
}

func newReplayTTLHarness(t *testing.T, id string, fail func(call int) bool) *replayTTLHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	body := []byte(`{"id":"https://remote.example/undo/` + id + `","type":"Undo","actor":"https://remote.example/users/alice","object":"https://remote.example/follows/` + id + `"}`)
	verifier, payload := signedReplayFixture(t, body)
	h := &replayTTLHarness{mr: mr, ttl: processors.InboxReplayRecordTTL(defaultInboxMaxRetryDelay)}
	h.stub = &stubFedProcessor{returnFn: func([]byte) error {
		if fail != nil && fail(len(h.stub.calls)) {
			return errors.New("transient")
		}
		return nil
	}}
	h.p = processors.NewInboxProcessor(h.stub, verifier, nil, nil, nil,
		processors.NewRedisInboxReplayGuard(rdb, defaultInboxMaxRetryDelay))
	h.task = driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload)}
	return h
}

func (h *replayTTLHarness) handle(job string) error {
	return h.p.Handle(driver.WithJobID(context.Background(), job), h.task)
}

// The record covers the Date window, the longest retry delay and a margin.
func TestInboxReplayRecordTTL(t *testing.T) {
	window := 2*activitypub.InboxDateSkew + 5*time.Minute
	assert.Equal(t, window+time.Hour, processors.InboxReplayRecordTTL(0))
	assert.Equal(t, window+time.Hour, processors.InboxReplayRecordTTL(-time.Minute))
	assert.Equal(t, window+time.Hour+defaultInboxMaxRetryDelay, processors.InboxReplayRecordTTL(defaultInboxMaxRetryDelay))
	// 既定の回数では 3 時間 47 分 24 秒。
	assert.Equal(t, 3*time.Hour+47*time.Minute+24*time.Second, processors.InboxReplayRecordTTL(defaultInboxMaxRetryDelay))
	assert.Equal(t, time.Duration(1<<63-1), processors.InboxReplayRecordTTL(time.Duration(1<<63-1)))
}

// The same request is dropped long after the first one was processed, as long
// as the record lives.
func TestInboxProcessor_ReplayDroppedUntilTheRecordExpires(t *testing.T) {
	h := newReplayTTLHarness(t, "late", nil)
	require.NoError(t, h.handle("1"))
	require.Len(t, h.stub.calls, 1)

	h.mr.FastForward(16 * time.Minute)
	require.NoError(t, h.handle("2"))
	assert.Equal(t, 1, len(h.stub.calls), "the request was processed again 16 minutes later")

	h.mr.FastForward(h.ttl - 16*time.Minute - time.Second)
	require.NoError(t, h.handle("3"))
	assert.Equal(t, 1, len(h.stub.calls), "the request was processed again just before the record expires")
}

// Two jobs of the same request are retried on their own: after one of them
// succeeds, the other is dropped.
func TestInboxProcessor_RetriedJobsOfTheSameRequest(t *testing.T) {
	h := newReplayTTLHarness(t, "retry", func(call int) bool { return call <= 2 })
	require.Error(t, h.handle("A"))
	require.Error(t, h.handle("B"))
	require.Len(t, h.stub.calls, 2)

	h.mr.FastForward(time.Minute)
	require.NoError(t, h.handle("A"))
	require.Len(t, h.stub.calls, 3)

	h.mr.FastForward(defaultInboxMaxRetryDelay + 30*time.Minute)
	require.NoError(t, h.handle("B"))
	assert.Equal(t, 3, len(h.stub.calls), "the other job was processed")
}

// The record is not kept forever.
func TestInboxProcessor_ReplayRecordExpires(t *testing.T) {
	h := newReplayTTLHarness(t, "expire", nil)
	require.NoError(t, h.handle("1"))
	require.Len(t, h.stub.calls, 1)

	h.mr.FastForward(h.ttl + time.Second)
	require.NoError(t, h.handle("2"))
	assert.Equal(t, 2, len(h.stub.calls), "the record outlived its lifetime")
}
