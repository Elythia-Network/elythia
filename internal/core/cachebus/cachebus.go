// Package cachebus carries in-process cache invalidations between processes
// over the internal pub/sub (#3664).
//
// Each cache keeps dropping its own entries on a local write exactly as before;
// this package additionally forwards the invalidation to the other processes
// sharing the Redis and applies theirs. A cache exposes a hook that is called
// after its local invalidation and a "locally" variant that never calls the
// hook, so a received invalidation is not published again.
package cachebus

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Topics on the internal pub/sub (event.PubSubService with the "internal:"
// prefix). The names follow the existing *CacheInvalidated events
// (relationCacheInvalidated, emojiCacheInvalidated).
const (
	// TopicInstanceSuspension carries InstanceSuspensionInvalidation.
	TopicInstanceSuspension = "instanceSuspendCacheInvalidated"
)

// publishTimeout bounds one synchronous publish. It runs on the write path.
const publishTimeout = 2 * time.Second

// Bus is the subset of event.PubSubService used here.
type Bus interface {
	Publish(ctx context.Context, channel string, payload any) error
	Subscribe(ctx context.Context, channel string, handler func([]byte)) func()
}

// Message is the wire format of every topic: the sending process and the
// cache specific invalidation.
type Message[T any] struct {
	From string `json:"from"`
	Inv  T      `json:"inv"`
}

// publishSync publishes inv on topic and logs a failure. The local cache has
// already been dropped by the caller, so a failure only leaves the other
// processes on their TTL.
//
// **書き込みの経路で同期に送る。** 非同期の待ち行列は、溢れたときと終了のときに
// 通知が黙って消える形を作る。ここで使うキャッシュ (インスタンスの停止判定・
// リレー・デコレーション) の書き込みは管理画面の操作と連合の受信の一部だけで、
// 頻度が低いので、書き込みが Redis の 1 往復ぶん遅くなるほうを取る。
func publishSync[T any](bus Bus, topic, sender string, inv T) {
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if err := bus.Publish(ctx, topic, Message[T]{From: sender, Inv: inv}); err != nil {
		slog.Warn("cache invalidation: publish failed", "topic", topic, "err", err)
	}
}

// subscribe applies the invalidations other processes publish on topic.
// Messages from sender (this process) are ignored because the writer has
// already dropped its own entries. A message that cannot be decoded calls
// malformed, which must drop the whole cache.
//
// When bus can report that its receive queue dropped messages of topic
// (event.PubSubService), malformed is also called then: the dropped messages
// cannot be named, so the whole cache goes, as on the sending side.
func subscribe[T any](bus Bus, topic, sender string, apply func(T), malformed func()) func() {
	handler := func(raw []byte) {
		var msg Message[T]
		if err := json.Unmarshal(raw, &msg); err != nil {
			// 何を捨てるべきか分からない通知は、捨て損ねるより全体を捨てる側に倒す。
			slog.Warn("cache invalidation: malformed message", "topic", topic, "err", err)
			malformed()
			return
		}
		if msg.From == sender {
			return
		}
		apply(msg.Inv)
	}
	if ob, ok := bus.(overflowBus); ok {
		// 受け側の待ち行列が溢れて捨てた通知も、送る側の溢れと同じく全体を
		// 捨てて埋める (黙って捨てると、その分は TTL まで古いまま)。
		return ob.SubscribeWithOverflow(context.Background(), topic, handler, malformed)
	}
	return bus.Subscribe(context.Background(), topic, handler)
}

// overflowBus is a Bus that reports dropped messages (event.PubSubService).
type overflowBus interface {
	SubscribeWithOverflow(ctx context.Context, channel string, handler func([]byte), onOverflow func()) func()
}
