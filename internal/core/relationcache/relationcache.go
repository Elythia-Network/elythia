// Package relationcache carries repository.RelationCache invalidations
// between processes over the internal pub/sub (#3607).
package relationcache

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/repository"
)

// Topic is the internal pub/sub channel the invalidations travel on.
const Topic = "relationCacheInvalidated"

// publishTimeout bounds one publish. The publish runs on the write path.
const publishTimeout = 2 * time.Second

// Bus is the subset of event.PubSubService used here.
type Bus interface {
	Publish(ctx context.Context, channel string, payload any) error
	Subscribe(ctx context.Context, channel string, handler func([]byte)) func()
}

// Message is the wire format on Topic.
type Message struct {
	From string                          `json:"from"`
	Inv  repository.RelationInvalidation `json:"inv"`
}

// Connect makes rc forward its local invalidations to the other processes and
// apply theirs. sender identifies this process; messages carrying it are
// ignored because the writer already dropped its own entries. It returns the
// function that stops receiving.
//
// **publish は書き込みの経路で同期に行う。** 非同期の待ち行列に積むと、溢れたとき
// や終了のときに通知が黙って消える。ミュートとブロックはすぐ効かないと
// プライバシーの問題になるので、書き込みが少し遅くなるほうを取る (書き込みは
// ミュート・ブロック・フォローの操作と連合の受信だけで、頻度は低い)。
//
// **通知を取りこぼしたプロセスでは、最大 TTL (1 分) 古い一覧が残る (既知)。**
// 取りこぼすのは、publish の失敗 (送り直さない)、Redis の購読の再接続の間、
// トピックの待ち行列の溢れ (event.PubSubService)。本家の CacheService もプロセス内
// に 1 分持ち、他のプロセスの書き込みはその間見えないので、本家より悪くはない。
// 購読の張り直しで全体を捨てるフックは、最初の確立と区別できず、張った直後の
// 正しい無効化と競合したので入れていない (#3607 のレビュー 2 周目)。
func Connect(rc *repository.RelationCache, bus Bus, sender string) func() {
	if rc == nil || bus == nil {
		return func() {}
	}
	rc.SetPublisher(func(inv repository.RelationInvalidation) {
		ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		if err := bus.Publish(ctx, Topic, Message{From: sender, Inv: inv}); err != nil {
			slog.Warn("relation cache: publish invalidation failed", "err", err)
		}
	})
	return bus.Subscribe(context.Background(), Topic, func(raw []byte) {
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			// 読めない通知は、何を捨てるべきか分からない。捨て損ねるより、
			// 全体を捨てる側に倒す。
			slog.Warn("relation cache: invalid invalidation message", "err", err)
			rc.ApplyRemote(repository.RelationInvalidation{All: true})
			return
		}
		if msg.From == sender {
			return
		}
		rc.ApplyRemote(msg.Inv)
	})
}
