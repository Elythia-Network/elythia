package instance

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// CounterTarget is the narrow surface of the instance repository consumed by
// CounterBuffer. TouchTarget と同じ理由で切り出している (unit test がフルの
// repository を組まなくても済む)。
type CounterTarget interface {
	IncrementCount(host, column string, delta int) error
}

// Instance counter columns accepted by CounterBuffer.Add.
const (
	CounterNotesCount = "notesCount"
	CounterUsersCount = "usersCount"
)

type counterKey struct {
	host   string
	column string
}

// CounterBuffer coalesces instance counter increments (notesCount /
// usersCount) into one UPDATE per host and column per flush interval.
//
// 本家 NoteCreateService は instance.notesCount の加算を CollapsedQueue (本番
// 5 分窓) で集約している。リモートノートの取り込みは inbox の最頻ジョブで、
// 1 件ごとに同じ instance 行を UPDATE すると、同一ホストを並行に処理する worker
// どうしが行ロックで詰まる (TouchBuffer #569 と同じ形)。窓内の増減は合算し、
// 合計が 0 の組は書かない。
//
// 窓内の値はプロセスが落ちると失われる。本家の CollapsedQueue も同じで、集計列は
// best-effort の統計として扱う。Close は残りを書いてから戻る。
type CounterBuffer struct {
	target  CounterTarget
	mu      sync.Mutex
	pending map[counterKey]int
	stopCh  chan struct{}
	doneCh  chan struct{}
	flushIn time.Duration
	// started=true は Start() が既に bg goroutine を起動済であることを表す
	// (TouchBuffer #580 と同じく、Start 前の Close で deadlock しないため)。
	started atomic.Bool
}

// NewCounterBuffer returns a buffer that flushes every flushInterval.
// flushInterval <= 0 means one second (same as TouchBuffer).
func NewCounterBuffer(target CounterTarget, flushInterval time.Duration) *CounterBuffer {
	if flushInterval <= 0 {
		flushInterval = time.Second
	}
	return &CounterBuffer{
		target:  target,
		pending: make(map[counterKey]int),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		flushIn: flushInterval,
	}
}

// Add accumulates delta for the given host and counter column. Empty hosts,
// zero deltas and columns other than CounterNotesCount / CounterUsersCount are
// ignored.
func (b *CounterBuffer) Add(host, column string, delta int) {
	if host == "" || delta == 0 {
		return
	}
	// 列名は UPDATE 文へそのまま埋め込まれるので、既知の列だけを受け付ける。
	if column != CounterNotesCount && column != CounterUsersCount {
		return
	}
	b.mu.Lock()
	b.pending[counterKey{host: host, column: column}] += delta
	b.mu.Unlock()
}

// Start spawns the background flush goroutine. Calling it again is a no-op.
func (b *CounterBuffer) Start(ctx context.Context) {
	if !b.started.CompareAndSwap(false, true) {
		return
	}
	go b.runLoop(ctx)
}

// Close stops the background goroutine after a final flush. Without a prior
// Start it flushes the pending increments synchronously.
func (b *CounterBuffer) Close() {
	if !b.started.Load() {
		b.flushOnce()
		return
	}
	select {
	case <-b.stopCh:
		return
	default:
	}
	close(b.stopCh)
	<-b.doneCh
}

// FlushNow applies the pending increments synchronously.
func (b *CounterBuffer) FlushNow() {
	b.flushOnce()
}

func (b *CounterBuffer) runLoop(ctx context.Context) {
	defer close(b.doneCh)
	ticker := time.NewTicker(b.flushIn)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			b.flushOnce()
			return
		case <-b.stopCh:
			b.flushOnce()
			return
		case <-ticker.C:
			b.flushOnce()
		}
	}
}

func (b *CounterBuffer) flushOnce() {
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	pending := b.pending
	b.pending = make(map[counterKey]int, len(pending))
	b.mu.Unlock()

	// 窓の中の増減を合算してから 1 回だけ書くので、下限 0 (IncrementCount の
	// GREATEST) も合算後の値に掛かる。0 の行に -1, +1 が来ると、1 件ずつ書けば
	// 1 になるところが合算では 0 のまま (書かない) になる。best-effort の統計と
	// して許容している。
	for k, delta := range pending {
		if delta == 0 {
			continue
		}
		if err := b.target.IncrementCount(k.host, k.column, delta); err != nil {
			slog.Warn("instance counter buffer flush failed",
				"host", k.host, "column", k.column, "delta", delta, "err", err)
		}
	}
}
