package stream

import (
	"encoding/json"
	"sync"
)

// statsRingBuffer は固定容量 + head pointer の circular buffer。append が O(1)
// で済むよう、unshift/append 系の slice prepend (O(n)) を避ける (#571 item 2
// review)。ServerStatsPublisher / QueueStatsPublisher で共有。
//
// 内部は容量 cap の slice を 1 度だけ確保し、head が次回書き込み位置を指す。
// Log(maxLen) は newest first で head-1 から逆走する。並行性は ring buffer
// 内蔵の sync.Mutex で取り、外部から追加の lock は不要。
type statsRingBuffer struct {
	mu    sync.Mutex
	buf   []json.RawMessage
	head  int // 次回書き込み index (0..cap-1)
	count int // 現在保持中の要素数 (0..cap)
	cap   int
}

// newStatsRingBuffer constructs a ring buffer with fixed capacity. capacity<=0
// は無意味なので panic させる (構築時に発覚させる方が runtime バグより安全)。
func newStatsRingBuffer(capacity int) *statsRingBuffer {
	if capacity <= 0 {
		panic("stream: ring buffer capacity must be > 0")
	}
	return &statsRingBuffer{
		buf: make([]json.RawMessage, capacity),
		cap: capacity,
	}
}

// Append appends a snapshot to the buffer. caller の raw を defensive copy
// してから格納するので、Append 後に caller が raw を mutate しても内部は
// 影響を受けない。容量超過時は最古エントリを上書きする (FIFO drop)。
// O(1)。
func (rb *statsRingBuffer) Append(raw json.RawMessage) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	copied := append(json.RawMessage(nil), raw...)
	rb.buf[rb.head] = copied
	rb.head = (rb.head + 1) % rb.cap
	if rb.count < rb.cap {
		rb.count++
	}
}

// Reset drops every stored entry.
func (rb *statsRingBuffer) Reset() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	clear(rb.buf)
	rb.head = 0
	rb.count = 0
}

// Log returns up to maxLen most-recent entries, newest first. maxLen<=0 か
// cap 超は cap に丸める。戻り値は内部 slot を defensive copy したものなので、
// caller が破壊的操作をしても内部 buffer は無影響。O(maxLen)。
func (rb *statsRingBuffer) Log(maxLen int) []json.RawMessage {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if maxLen <= 0 || maxLen > rb.cap {
		maxLen = rb.cap
	}
	n := rb.count
	if n > maxLen {
		n = maxLen
	}
	out := make([]json.RawMessage, n)
	// newest first: head-1 から逆走。head-1 < 0 のラップアラウンドは
	// (head - 1 - i + cap) % cap で safely 計算する。
	for i := 0; i < n; i++ {
		idx := (rb.head - 1 - i + rb.cap) % rb.cap
		out[i] = rb.buf[idx]
	}
	return out
}
