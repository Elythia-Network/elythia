package stream

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturePubSub は Publish 呼び出しをメモリに記録するテスト用 publisher。
type capturePubSub struct {
	mu    sync.Mutex
	calls []struct {
		channel string
		payload []byte
	}
}

func (c *capturePubSub) Publish(_ context.Context, channel string, payload any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// payload is json.RawMessage which is []byte
	var raw []byte
	if b, ok := payload.(json.RawMessage); ok {
		raw = []byte(b)
	}
	c.calls = append(c.calls, struct {
		channel string
		payload []byte
	}{channel, raw})
	return nil
}

func (c *capturePubSub) snapshot() []struct {
	channel string
	payload []byte
} {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]struct {
		channel string
		payload []byte
	}, len(c.calls))
	copy(out, c.calls)
	return out
}

// newEnabledServerStatsPublisher は enableServerMachineStats が有効な状態の
// publisher を作る。
func newEnabledServerStatsPublisher(pub PubSubPublisher, interval time.Duration) *ServerStatsPublisher {
	return NewServerStatsPublisher(pub, interval, func() bool { return true })
}

func TestServerStatsPublisher_PublishesSnapshot(t *testing.T) {
	pub := &capturePubSub{}
	// 短い interval で Start → Stop し、最低 1 回 tick されたことを確認する。
	p := newEnabledServerStatsPublisher(pub, 20*time.Millisecond)
	p.Start()
	time.Sleep(60 * time.Millisecond)
	p.Stop()

	calls := pub.snapshot()
	require.NotEmpty(t, calls, "publisher should tick at least once")
	for _, c := range calls {
		assert.Equal(t, "serverStats", c.channel)
		// payload should parse as serverStatsSnapshot JSON shape.
		var body struct {
			CPU float64 `json:"cpu"`
			Mem struct {
				Used   uint64 `json:"used"`
				Active uint64 `json:"active"`
			} `json:"mem"`
			Net struct {
				Rx uint64 `json:"rx"`
				Tx uint64 `json:"tx"`
			} `json:"net"`
			Fs struct {
				R uint64 `json:"r"`
				W uint64 `json:"w"`
			} `json:"fs"`
		}
		require.NoError(t, json.Unmarshal(c.payload, &body))
	}
}

func TestServerStatsPublisher_DoubleStartNoOp(t *testing.T) {
	pub := &capturePubSub{}
	p := newEnabledServerStatsPublisher(pub, 50*time.Millisecond)
	p.Start()
	p.Start() // second call must be a no-op
	time.Sleep(10 * time.Millisecond)
	p.Stop()
	// Stop を 2 回呼んでも panic しないこと。
	p.Stop()
}

func TestServerStatsPublisher_NilPubNoOp(t *testing.T) {
	// pub が nil でも panic せず、Start はno-opとなる。
	p := NewServerStatsPublisher(nil, 50*time.Millisecond, nil)
	p.Start()
	p.Stop()
}

func TestDiffPerSec(t *testing.T) {
	assert.Equal(t, uint64(100), diffPerSec(200, 100, 1.0))
	assert.Equal(t, uint64(50), diffPerSec(200, 100, 2.0))
	// underflow (counter reset) clamps to 0
	assert.Equal(t, uint64(0), diffPerSec(50, 100, 1.0))
	// zero elapsed clamps to 0
	assert.Equal(t, uint64(0), diffPerSec(200, 100, 0))
}

// publisher が ring buffer に snapshot を貯めて Log() で返せること (#571 item 2)
func TestServerStatsPublisher_LogReturnsRecentSnapshots(t *testing.T) {
	pub := &capturePubSub{}
	p := newEnabledServerStatsPublisher(pub, 10*time.Millisecond)
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()

	logs := p.Log(0) // default
	require.NotEmpty(t, logs, "Log should accumulate snapshots from each tick")
	// 各 entry は serverStatsSnapshot 形式の JSON
	for _, raw := range logs {
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		assert.Contains(t, body, "cpu")
		assert.Contains(t, body, "mem")
	}
	// publisher snapshot 数 == capture pub の call 数 (各 tick で 1:1 対応)
	calls := pub.snapshot()
	assert.LessOrEqual(t, len(logs), len(calls), "log entry count should match tick count")
}

// Log(maxLen) で件数が制限されること
func TestServerStatsPublisher_LogRespectsMaxLen(t *testing.T) {
	pub := &capturePubSub{}
	p := newEnabledServerStatsPublisher(pub, 5*time.Millisecond)
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()

	logs := p.Log(2)
	assert.LessOrEqual(t, len(logs), 2, "Log(2) should return at most 2 entries")
}

// ring buffer は最大 ServerStatsLogMax 件で頭打ち。
// publisher の Append は private なので statsRingBuffer を直接 verify する。
func TestServerStatsPublisher_LogCapAtMax(t *testing.T) {
	pub := &capturePubSub{}
	p := newEnabledServerStatsPublisher(pub, 0) // default 2s — manually feed
	// publisher の logBuf を直接叩いて 250 件詰める (max=200 で頭打ち)。
	for i := 0; i < 250; i++ {
		p.logBuf.Append(json.RawMessage(`{}`))
	}
	logs := p.Log(0)
	assert.Equal(t, ServerStatsLogMax, len(logs), "ring buffer caps at ServerStatsLogMax")
}

// Log の戻り値は publisher 内部 buffer を mutate しても影響しない (defensive copy)
func TestServerStatsPublisher_LogDefensiveCopy(t *testing.T) {
	pub := &capturePubSub{}
	p := newEnabledServerStatsPublisher(pub, 0)
	original := json.RawMessage(`{"k":1}`)
	p.logBuf.Append(original)

	logs := p.Log(0)
	require.Len(t, logs, 1)

	// caller が後から同 slot を書き換えても publisher 内部に影響しない
	logs[0] = json.RawMessage(`{"k":2}`)
	logs2 := p.Log(0)
	require.Len(t, logs2, 1)
	assert.JSONEq(t, `{"k":1}`, string(logs2[0]), "internal buffer must not be aliased")
}

func TestServerStatsPublisher_DisabledPublishesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled func() bool
	}{
		{name: "gate not wired", enabled: nil},
		{name: "gate returns false", enabled: func() bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &capturePubSub{}
			p := NewServerStatsPublisher(pub, 0, tc.enabled)
			p.tick()
			p.tick()
			assert.Empty(t, pub.snapshot(), "nothing may be published while disabled")
			assert.Empty(t, p.Log(0), "requestLog must not return snapshots while disabled")
		})
	}
}

func TestServerStatsPublisher_DisablingDropsHistory(t *testing.T) {
	pub := &capturePubSub{}
	var enabled atomic.Bool
	enabled.Store(true)
	p := NewServerStatsPublisher(pub, 0, enabled.Load)

	p.tick()
	p.tick()
	require.Len(t, pub.snapshot(), 2)
	require.Len(t, p.Log(0), 2)

	enabled.Store(false)
	assert.Empty(t, p.Log(0), "history collected while enabled must not be served after disabling")
	p.tick()
	assert.Len(t, pub.snapshot(), 2, "no publish after disabling")

	// 再び有効にしても、無効にする前の履歴は戻らない。差分の起点も捨てているので、
	// 最初の tick は初回として扱われ、無効だった期間の平均を出さない。
	assert.True(t, p.prevTick.IsZero(), "the rate baseline must be dropped while disabled")
	enabled.Store(true)
	assert.Empty(t, p.Log(0))
	p.tick()
	assert.Len(t, p.Log(0), 1)
	assert.Len(t, pub.snapshot(), 3)
}

func TestStatsRingBuffer_Reset(t *testing.T) {
	rb := newStatsRingBuffer(3)
	rb.Append(json.RawMessage(`1`))
	rb.Append(json.RawMessage(`2`))
	rb.Reset()
	assert.Empty(t, rb.Log(0))
	rb.Append(json.RawMessage(`3`))
	logs := rb.Log(0)
	require.Len(t, logs, 1)
	assert.JSONEq(t, `3`, string(logs[0]))
}
