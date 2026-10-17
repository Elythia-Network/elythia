package cachebus

import "github.com/elythia-network/elythia/internal/core/avatardecoration"

// AvatarDecorationInvalidation is the payload of TopicAvatarDecoration. It
// names which of the two decoration caches must be dropped.
type AvatarDecorationInvalidation struct {
	// Catalog drops the admin-managed decoration catalog
	// (avatardecoration.Resolver).
	Catalog bool `json:"catalog,omitempty"`
	// Emoji drops the map of emojis usable as decorations
	// (avatardecoration.EmojiResolver).
	Emoji bool `json:"emoji,omitempty"`
}

// ConnectAvatarDecorations makes the two decoration resolvers forward their
// invalidations to the other processes and apply theirs (#3664). Either
// resolver may be nil. sender identifies this process. It returns the
// function that stops receiving.
//
// 管理画面の操作 (デコレーション・絵文字) と絵文字の zip インポートが捨てるが、
// どちらも頻度は低い。通知を取りこぼしたプロセスでは、今までどおり TTL (30 秒) が
// 上限になる。
//
// 絵文字由来のデコレーションは、絵文字のキャッシュの通知でも捨てる
// (SubscribeEmojiCache)。
func ConnectAvatarDecorations(catalog *avatardecoration.Resolver, emoji *avatardecoration.EmojiResolver, bus Bus, sender string) func() {
	if (catalog == nil && emoji == nil) || bus == nil {
		return func() {}
	}
	if catalog != nil {
		catalog.SetInvalidationHook(func() {
			publishSync(bus, TopicAvatarDecoration, sender, AvatarDecorationInvalidation{Catalog: true})
		})
	}
	if emoji != nil {
		emoji.SetInvalidationHook(func() {
			publishSync(bus, TopicAvatarDecoration, sender, AvatarDecorationInvalidation{Emoji: true})
		})
	}
	apply := func(inv AvatarDecorationInvalidation) {
		// nil の resolver の InvalidateLocally は何もしない。
		if inv.Catalog {
			catalog.InvalidateLocally()
		}
		if inv.Emoji {
			emoji.InvalidateLocally()
		}
	}
	return subscribe(bus, TopicAvatarDecoration, sender, apply,
		func() { apply(AvatarDecorationInvalidation{Catalog: true, Emoji: true}) })
}
