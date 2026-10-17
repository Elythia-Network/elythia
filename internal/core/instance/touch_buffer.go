package instance

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// TouchTarget is the narrow surface of *Service consumed by TouchBuffer.
// 切り出すことで unit test がフルの Service deps を組まなくても済む。
type TouchTarget interface {
	MarkRequestReceived(host string) error
}

// TouchBuffer coalesces high-frequency MarkRequestReceived calls into one
// DB write per host per flush interval. inbox worker (#565 で per-request
// 同期から worker 側に移動) は同一 remote host から大量 inbox を受け取る
// 連合運用で per-call の DB UPDATE が drain time のボトルネックになる
// (#569)。in-memory aggregator に貯めて bg goroutine が定期 flush する
// ことで、同一 host の N 件が 1 UPDATE に縮退する。
//
// Stop しないと bg goroutine がリークするので、上位 (server.Server) は
// shutdown フローで Close() を必ず呼ぶ前提。
type TouchBuffer struct {
	target  TouchTarget
	clock   func() time.Time
	mu      sync.Mutex
	pending map[string]time.Time // host → 最新観測時刻
	stopCh  chan struct{}
	doneCh  chan struct{}
	flushIn time.Duration
	// started=true は Start() が既に bg goroutine を起動済であることを表す。
	// Close は started=false なら doneCh を待たずに pending だけ flush する
	// (#580 Devin BUG: docstring 「Start を呼んでいない場合の Close は no-op」
	// に対して旧実装は doneCh で deadlock していた)。
	started atomic.Bool
	// closed=true の後の MarkRequestReceived は pending に積まず、その場で書く。
	// 閉じた後の pending は誰も flush しないので、積むと黙って消える (#3662)。
	// mu で守る。
	closed bool
}

// NewTouchBuffer returns a buffer that flushes every flushInterval.
// flushInterval = 0 は default 1s 扱い。
func NewTouchBuffer(target TouchTarget, flushInterval time.Duration) *TouchBuffer {
	if flushInterval <= 0 {
		flushInterval = time.Second
	}
	return &TouchBuffer{
		target:  target,
		clock:   time.Now,
		pending: make(map[string]time.Time),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		flushIn: flushInterval,
	}
}

// Start spawns the background flush goroutine. 二度目以降の呼び出しは
// 何もせずに return する (idempotent)。
func (b *TouchBuffer) Start(ctx context.Context) {
	if !b.started.CompareAndSwap(false, true) {
		return
	}
	go b.runLoop(ctx)
}

// Close stops the background flush goroutine and triggers a final flush.
// graceful shutdown 用。Start を呼ばずに Close した場合は doneCh を待たず
// に pending を best-effort で flush して return する (旧実装は doneCh で
// deadlock した。#580 Devin BUG-1)。
func (b *TouchBuffer) Close() {
	b.markClosed()
	if !b.started.Load() {
		// Start を呼んでいないので runLoop は無く doneCh が close される
		// ことはない。pending に積まれているもの (テスト等で直接
		// MarkRequestReceived を叩いたケース) を best-effort で吐き出す。
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

// MarkRequestReceived stores the host into the pending map. inbox
// processor が per-request で呼ぶ hot path。After Close (or once the flush
// loop has stopped) it writes through to the target instead, since nothing
// would flush the pending map any more.
func (b *TouchBuffer) MarkRequestReceived(host string) error {
	if host == "" {
		return nil
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return b.target.MarkRequestReceived(host)
	}
	b.pending[host] = b.clock()
	b.mu.Unlock()
	return nil
}

// markClosed makes later writes bypass the pending map. 最後の flush より前に
// 呼ぶこと。後に呼ぶと、その間に積まれた分が残る。
func (b *TouchBuffer) markClosed() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
}

// FlushNow drains the pending map and applies updates synchronously.
// Test 用。production 経路は runLoop が定期 flush する。
func (b *TouchBuffer) FlushNow() {
	b.flushOnce()
}

func (b *TouchBuffer) runLoop(ctx context.Context) {
	defer close(b.doneCh)
	ticker := time.NewTicker(b.flushIn)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			b.markClosed()
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

func (b *TouchBuffer) flushOnce() {
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	hosts := make([]string, 0, len(b.pending))
	for host := range b.pending {
		hosts = append(hosts, host)
	}
	b.pending = make(map[string]time.Time, len(hosts))
	b.mu.Unlock()

	for _, host := range hosts {
		if err := b.target.MarkRequestReceived(host); err != nil {
			slog.Warn("instance touch buffer flush failed", "host", host, "err", err)
		}
	}
}
