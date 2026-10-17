package cachebus

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/avatardecoration"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEmojiCache stands for the receiving process's CachedEmojiRepository:
// ListLocal answers from the cached rows until an invalidation makes it read
// the fresh ones. during, when set, runs inside ApplyRemoteInvalidation before
// the cache is dropped.
type fakeEmojiCache struct {
	cached, fresh []*model.Emoji
	applied       []repository.EmojiCacheInvalidation
	during        func()
}

func (f *fakeEmojiCache) ListLocal() ([]*model.Emoji, error) { return f.cached, nil }

func (f *fakeEmojiCache) ApplyRemoteInvalidation(inv repository.EmojiCacheInvalidation) {
	if f.during != nil {
		d := f.during
		f.during = nil
		d()
	}
	f.applied = append(f.applied, inv)
	f.cached = f.fresh
}

var localEmoji = &model.Emoji{ID: "e1", Name: "e1", OriginalURL: "https://e/e1.png"}

// TestSubscribeEmojiCache_DropsEmojiCacheBeforeDecorations pins the order in
// one handler: the emoji cache first, then the decoration map. A lookup that
// lands in between then still answers from the old map and is dropped after,
// instead of rebuilding the map from the not yet dropped emoji cache and
// keeping that stale map for the TTL (#3664).
func TestSubscribeEmojiCache_DropsEmojiCacheBeforeDecorations(t *testing.T) {
	emojis := &fakeEmojiCache{fresh: []*model.Emoji{localEmoji}}
	decorations := avatardecoration.NewEmojiResolver(emojis)
	bus := newLoopBus()
	stop := SubscribeEmojiCache(emojis, decorations, bus, "B")
	defer stop()
	lookup := func() bool {
		_, _, ok := decorations.LookupEmojiDecoration("e1")
		return ok
	}
	require.False(t, lookup())

	// 2 つを捨てる間に、別の要求がデコレーションを引く。
	emojis.during = func() { lookup() }
	deliverMessage(t, bus, TopicEmoji, Message[repository.EmojiCacheInvalidation]{From: "A", Inv: repository.EmojiCacheInvalidation{IDs: []string{"e1"}}})
	assert.True(t, lookup(), "the map is rebuilt from the dropped emoji cache")
}

// TestSubscribeEmojiCache_AfterDecorationNotification reproduces the usual
// arrival order when an admin adds a local emoji: the decoration notification
// (published synchronously) comes before the emoji notification (published
// from a queue). The map rebuilt on the first one comes from the stale emoji
// cache; the emoji notification drops it again.
func TestSubscribeEmojiCache_AfterDecorationNotification(t *testing.T) {
	emojis := &fakeEmojiCache{fresh: []*model.Emoji{localEmoji}}
	decorations := avatardecoration.NewEmojiResolver(emojis)
	bus := newLoopBus()
	defer ConnectAvatarDecorations(nil, decorations, bus, "B")()
	defer SubscribeEmojiCache(emojis, decorations, bus, "B-emoji")()
	lookup := func() bool {
		_, _, ok := decorations.LookupEmojiDecoration("e1")
		return ok
	}
	require.False(t, lookup())

	deliverMessage(t, bus, TopicAvatarDecoration, Message[AvatarDecorationInvalidation]{From: "A", Inv: AvatarDecorationInvalidation{Emoji: true}})
	require.False(t, lookup(), "rebuilt from the stale emoji cache")
	deliverMessage(t, bus, TopicEmoji, Message[repository.EmojiCacheInvalidation]{From: "A-emoji", Inv: repository.EmojiCacheInvalidation{IDs: []string{"e1"}}})
	assert.True(t, lookup(), "the emoji notification drops the map again")
}

// TestSubscribeEmojiCache_Scope pins what each notification drops.
func TestSubscribeEmojiCache_Scope(t *testing.T) {
	emojis := &fakeEmojiCache{}
	decorations := avatardecoration.NewEmojiResolver(emojis)
	bus := &overflowLoopBus{loopBus: newLoopBus(), overflow: map[string]func(){}}
	stop := SubscribeEmojiCache(emojis, decorations, bus, "me")
	defer stop()
	present := func() bool {
		_, _, ok := decorations.LookupEmojiDecoration("e1")
		return ok
	}
	require.False(t, present())
	// 以降、絵文字のキャッシュを捨てると e1 が見えるようになる。
	emojis.fresh = []*model.Emoji{localEmoji}

	// 自分の通知は受け流す (書いたプロセスは自分で捨て済み)。
	deliverMessage(t, bus.loopBus, TopicEmoji, Message[repository.EmojiCacheInvalidation]{From: "me", Inv: repository.EmojiCacheInvalidation{IDs: []string{"e1"}}})
	assert.Empty(t, emojis.applied)
	assert.False(t, present())

	// リモートの絵文字だけの通知は、絵文字のキャッシュにだけ効く。
	remote := repository.EmojiCacheInvalidation{Keys: []model.EmojiKey{{Name: "x", Host: "remote.example"}}}
	deliverMessage(t, bus.loopBus, TopicEmoji, Message[repository.EmojiCacheInvalidation]{From: "other", Inv: remote})
	assert.Equal(t, []repository.EmojiCacheInvalidation{remote}, emojis.applied)
	assert.False(t, present(), "the decoration map is kept")

	// ローカルの絵文字に関わる通知は、map も捨てる。
	local := repository.EmojiCacheInvalidation{Keys: []model.EmojiKey{{Name: "e1"}}}
	deliverMessage(t, bus.loopBus, TopicEmoji, Message[repository.EmojiCacheInvalidation]{From: "other", Inv: local})
	assert.True(t, present())

	// 読めない通知と受け側の溢れは、どちらも全体を捨てる。
	for name, drop := range map[string]func(){
		"malformed": func() { bus.deliver(t, TopicEmoji, []byte("{not json")) },
		"overflow":  bus.overflow[TopicEmoji],
	} {
		emojis.fresh = nil
		require.True(t, present(), "%s: cached", name)
		n := len(emojis.applied)
		drop()
		assert.Equal(t, repository.EmojiCacheInvalidation{All: true}, emojis.applied[n], name)
		assert.False(t, present(), name)
		emojis.fresh = []*model.Emoji{localEmoji}
		emojis.cached = emojis.fresh
		decorations.InvalidateLocally()
	}
}

func TestSubscribeEmojiCache_NilIsNoop(t *testing.T) {
	SubscribeEmojiCache(nil, nil, newLoopBus(), "x")()
	SubscribeEmojiCache(&fakeEmojiCache{}, nil, nil, "x")()
	// デコレーションの resolver が無くても、絵文字のキャッシュには効く。
	emojis := &fakeEmojiCache{}
	bus := newLoopBus()
	defer SubscribeEmojiCache(emojis, nil, bus, "x")()
	deliverMessage(t, bus, TopicEmoji, Message[repository.EmojiCacheInvalidation]{From: "other", Inv: repository.EmojiCacheInvalidation{IDs: []string{"e1"}}})
	assert.Len(t, emojis.applied, 1)
}
