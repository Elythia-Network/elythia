package instance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capabilityCall struct {
	signal string
	host   string
	alg    string
	at     time.Time
}

type fakeCapabilityTarget struct {
	mu    sync.Mutex
	calls []capabilityCall
	err   error
}

func (f *fakeCapabilityTarget) record(c capabilityCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.err
}

func (f *fakeCapabilityTarget) RecordInboundAlg(host, alg string, at time.Time) error {
	return f.record(capabilityCall{signal: "inboundAlg", host: host, alg: alg, at: at})
}

func (f *fakeCapabilityTarget) RecordLDSignature(host string, at time.Time) error {
	return f.record(capabilityCall{signal: "ldSignature", host: host, at: at})
}

func (f *fakeCapabilityTarget) RecordEd25519Accepted(host string, at time.Time) error {
	return f.record(capabilityCall{signal: "ed25519Accepted", host: host, at: at})
}

func (f *fakeCapabilityTarget) snapshot() []capabilityCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capabilityCall(nil), f.calls...)
}

func (f *fakeCapabilityTarget) signals() []string {
	out := []string{}
	for _, c := range f.snapshot() {
		out = append(out, c.signal)
	}
	return out
}

// KeyTypeName と model.SignatureAlg* は別パッケージにある同じ語彙。model は最下層
// なので activitypub を import できず定数を共有できないため、一致をここで担保する。
// ずれると「Ed25519 で受信したのにラベルが出ない」形で静かに壊れる。
func TestKeyTypeNameMatchesModelConstants(t *testing.T) {
	assert.Equal(t, model.SignatureAlgRSA, activitypub.KeyTypeName(activitypub.KeyTypeRSA))
	assert.Equal(t, model.SignatureAlgEd25519, activitypub.KeyTypeName(activitypub.KeyTypeEd25519))
}

// 同一 host への連続観測が 1 flush 1 write に縮退する。ここが効かないと inbound
// hot path が per-request UPDATE に戻る。
func TestCapabilityBuffer_CoalescesPerHost(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)

	for i := 0; i < 10; i++ {
		b.ObserveInboundAlg("remote.example", model.SignatureAlgEd25519)
	}
	b.FlushNow()

	calls := target.snapshot()
	require.Len(t, calls, 1, "10 観測が 1 write に縮退する")
	assert.Equal(t, "inboundAlg", calls[0].signal)
	assert.Equal(t, "remote.example", calls[0].host)
	assert.Equal(t, model.SignatureAlgEd25519, calls[0].alg)
}

// 窓内で観測されなかった系統は呼ばない。repository 側が部分 upsert なので、
// 呼んでしまうと他系統の記録をゼロ値で潰す。
func TestCapabilityBuffer_OnlyFlushesObservedSignals(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)

	b.ObserveLDSignature("ld-only.example")
	b.FlushNow()

	assert.Equal(t, []string{"ldSignature"}, target.signals())
}

func TestCapabilityBuffer_FlushesAllObservedSignals(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)

	b.ObserveInboundAlg("all.example", model.SignatureAlgRSA)
	b.ObserveLDSignature("all.example")
	b.ObserveEd25519Accepted("all.example")
	b.FlushNow()

	assert.ElementsMatch(t, []string{"inboundAlg", "ldSignature", "ed25519Accepted"}, target.signals())
	for _, c := range target.snapshot() {
		assert.Equal(t, "all.example", c.host)
		assert.False(t, c.at.IsZero(), "観測時刻が入る: %s", c.signal)
	}
}

func TestCapabilityBuffer_SeparatesHosts(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)

	b.ObserveInboundAlg("a.example", model.SignatureAlgEd25519)
	b.ObserveInboundAlg("b.example", model.SignatureAlgRSA)
	b.FlushNow()

	byHost := map[string]string{}
	for _, c := range target.snapshot() {
		byHost[c.host] = c.alg
	}
	assert.Equal(t, map[string]string{
		"a.example": model.SignatureAlgEd25519,
		"b.example": model.SignatureAlgRSA,
	}, byHost)
}

// 直近の観測が勝つ。RSA→Ed25519 に切り替えた相手のラベルが古いまま固まらない。
func TestCapabilityBuffer_LastObservationWins(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)

	b.ObserveInboundAlg("switch.example", model.SignatureAlgRSA)
	b.ObserveInboundAlg("switch.example", model.SignatureAlgEd25519)
	b.FlushNow()

	calls := target.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, model.SignatureAlgEd25519, calls[0].alg)
}

func TestCapabilityBuffer_FlushClearsPending(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)

	b.ObserveInboundAlg("once.example", model.SignatureAlgRSA)
	b.FlushNow()
	b.FlushNow()

	assert.Len(t, target.snapshot(), 1, "2 回目の flush は何も書かない")
}

func TestCapabilityBuffer_IgnoresEmptyInput(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)

	b.ObserveInboundAlg("", model.SignatureAlgRSA)
	b.ObserveInboundAlg("no-alg.example", "")
	b.ObserveLDSignature("")
	b.ObserveEd25519Accepted("")
	b.FlushNow()

	assert.Empty(t, target.snapshot())
}

// nil receiver でも panic しない。観測フックは capability 未配線の構成
// (テスト / 最小構成) でも呼ばれるため。
func TestCapabilityBuffer_NilReceiverIsNoOp(t *testing.T) {
	var b *CapabilityBuffer
	assert.NotPanics(t, func() {
		b.ObserveInboundAlg("x.example", model.SignatureAlgRSA)
		b.ObserveLDSignature("x.example")
		b.ObserveEd25519Accepted("x.example")
	})
}

// target 未配線でも pending を捨てるだけで panic しない。
func TestCapabilityBuffer_NilTargetIsNoOp(t *testing.T) {
	b := NewCapabilityBuffer(nil, time.Hour)
	b.ObserveInboundAlg("x.example", model.SignatureAlgRSA)
	assert.NotPanics(t, b.FlushNow)
}

// target が error を返しても他 host の flush を止めない (best-effort)。
func TestCapabilityBuffer_ContinuesOnTargetError(t *testing.T) {
	target := &fakeCapabilityTarget{err: errors.New("db down")}
	b := NewCapabilityBuffer(target, time.Hour)

	b.ObserveInboundAlg("a.example", model.SignatureAlgRSA)
	b.ObserveLDSignature("a.example")
	b.ObserveEd25519Accepted("a.example")
	assert.NotPanics(t, b.FlushNow)
	assert.Len(t, target.snapshot(), 3, "1 系統の失敗で残りを諦めない")
}

func TestCapabilityBuffer_StartFlushesPeriodically(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	t.Cleanup(b.Close)

	b.ObserveInboundAlg("ticker.example", model.SignatureAlgEd25519)

	require.Eventually(t, func() bool {
		return len(target.snapshot()) > 0
	}, 2*time.Second, 5*time.Millisecond, "ticker が pending を flush する")
}

// Start は idempotent。二重起動で goroutine が増えない (Close が 1 度で終わる)。
func TestCapabilityBuffer_StartIsIdempotent(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	b.Start(ctx)

	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close が返らない (二重 Start で goroutine が増えている)")
	}
}

// Start を呼んでいない状態の Close は doneCh を待たずに pending を吐く。
// TouchBuffer が #580 で踏んだ deadlock と同じ穴。
func TestCapabilityBuffer_CloseWithoutStart(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)
	b.ObserveInboundAlg("noStart.example", model.SignatureAlgRSA)

	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start 無しの Close が deadlock した")
	}
	assert.Len(t, target.snapshot(), 1, "pending は best-effort で吐き出される")
}

func TestCapabilityBuffer_CloseIsIdempotent(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	b.Close()
	assert.NotPanics(t, b.Close, "2 度目の Close は no-op")
}

// ctx キャンセルでも最終 flush が走る。
func TestCapabilityBuffer_ContextCancelFlushes(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	b.Start(ctx)
	b.ObserveInboundAlg("ctx.example", model.SignatureAlgRSA)
	cancel()

	require.Eventually(t, func() bool {
		return len(target.snapshot()) > 0
	}, 2*time.Second, 5*time.Millisecond, "ctx キャンセル時に最終 flush が走る")
}

func TestCapabilityBuffer_DefaultFlushInterval(t *testing.T) {
	b := NewCapabilityBuffer(&fakeCapabilityTarget{}, 0)
	assert.Equal(t, time.Second, b.flushIn)
}

// 並行観測で race / 取りこぼしが起きない。
func TestCapabilityBuffer_ConcurrentObservations(t *testing.T) {
	target := &fakeCapabilityTarget{}
	b := NewCapabilityBuffer(target, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.ObserveInboundAlg("race.example", model.SignatureAlgEd25519)
			b.ObserveLDSignature("race.example")
		}()
	}
	wg.Wait()
	b.FlushNow()

	assert.ElementsMatch(t, []string{"inboundAlg", "ldSignature"}, target.signals())
}
