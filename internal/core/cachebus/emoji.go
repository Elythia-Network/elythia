package cachebus

import (
	"github.com/elythia-network/elythia/internal/core/avatardecoration"
	"github.com/elythia-network/elythia/internal/repository"
)

// EmojiCache is the emoji cache side of TopicEmoji
// (repository.CachedEmojiRepository).
type EmojiCache interface {
	ApplyRemoteInvalidation(repository.EmojiCacheInvalidation)
}

// SubscribeEmojiCache applies the emoji cache invalidations other processes
// publish on TopicEmoji (#3383): to emojis, and then, when the invalidation
// may concern a local emoji, to the map of emojis usable as avatar
// decorations, which is built from emojis' ListLocal (#3664). sender is the
// id this process puts on its own emoji notifications; those are ignored
// because the writer dropped its entries itself. decorations may be nil. It
// returns the function that stops receiving.
//
// **1 つのハンドラの中で、絵文字のキャッシュ → デコレーションの map の順に捨てる。**
// 別々のハンドラにすると呼ばれる順が決まらず、map を先に捨てた隙に、まだ捨てて
// いない ListLocal のキャッシュから作り直した古い map が TTL (30 秒) 残りうる
// (#3664 のレビュー 2 周目)。この順なら、間に来た作り直しは読み込み中の無効化
// として書き戻されない。
//
// デコレーションの通知 (TopicAvatarDecoration) と絵文字の通知は届く順が揃わない。
// 管理画面の操作では、デコレーションの通知が同期の publish、絵文字の通知が非同期の
// 待ち行列なので、デコレーションの通知が先に届きやすい。そのとき受けたプロセスは
// 古い ListLocal から map を作り直すが、後から届く絵文字の通知でここが捨て直す。
//
// 読めない通知と、受け側の待ち行列の溢れでは、絵文字のキャッシュも map も全体を
// 捨てる。
func SubscribeEmojiCache(emojis EmojiCache, decorations *avatardecoration.EmojiResolver, bus Bus, sender string) func() {
	if emojis == nil || bus == nil {
		return func() {}
	}
	apply := func(inv repository.EmojiCacheInvalidation) {
		emojis.ApplyRemoteInvalidation(inv)
		// nil の resolver の InvalidateLocally は何もしない。
		if inv.MayAffectLocal() {
			decorations.InvalidateLocally()
		}
	}
	return subscribe(bus, TopicEmoji, sender, apply,
		func() { apply(repository.EmojiCacheInvalidation{All: true}) })
}
