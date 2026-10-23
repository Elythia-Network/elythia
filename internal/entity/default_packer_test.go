package entity

import (
	"sync"
	"sync/atomic"
)

// このファイルは、このパッケージのテストだけが使う既定の Packer と、それを書き換える
// Set* を持つ (#3800)。
//
// 以前は本番の router が Set* で既定の Packer を配線し、パッケージ関数がそれを
// 読んでいた。#3790〜#3798 で部品が組み立てで作った Packer を受け取る形になり、
// 本番には読み手がいなくなったので、#3800 で本番から消した。このパッケージの
// テストは既定の Packer を読む形で 378 か所書かれているので、書き換えずに済む
// よう、テスト用の補助としてここへ移した。本番のコードはこれを使えない。

// defaultPacker is the Packer that this package's tests configure with Set*
// and read with currentPacker().
//
// 読み込みはリクエストごとに走るので atomic にし、Set* は写しを作って差し替える。
// 書き込みどうしは defaultPackerMu で直列にする (2 つの Set* が同時に写しを
// 作ると、片方の書き込みが消える)。
var (
	defaultPackerMu sync.Mutex
	defaultPacker   atomic.Pointer[Packer]
)

// currentPacker returns the default Packer, never nil.
func currentPacker() *Packer {
	if p := defaultPacker.Load(); p != nil {
		return p
	}
	return &Packer{}
}

// updateDefaultPacker replaces the default Packer with a copy changed by fn.
func updateDefaultPacker(fn func(*Packer)) {
	defaultPackerMu.Lock()
	defer defaultPackerMu.Unlock()
	next := *currentPacker()
	fn(&next)
	defaultPacker.Store(&next)
}

// SetCanChatLookup wires the role-policy lookup used by PackUserLite to
// derive canChat. Pass nil to clear.
//
// It changes only the test-only default Packer; a Packer built by NewPacker
// keeps its own dependencies.
func SetCanChatLookup(l CanChatLookup) {
	updateDefaultPacker(func(p *Packer) { p.canChat = l })
}

// SetAvatarDecorationLookup wires the catalog used by PackUserLite to embed
// `url` on each avatarDecorations entry. Pass nil to clear.
//
// It changes only the test-only default Packer; a Packer built by NewPacker
// keeps its own dependencies.
func SetAvatarDecorationLookup(l AvatarDecorationLookup) {
	updateDefaultPacker(func(p *Packer) { p.avatarDecoration = l })
}

// SetEmojiDecorationLookup wires the resolver used by PackUserLite for
// emoji-backed avatarDecorations entries. Pass nil to clear.
//
// **nil のときは絵文字由来のエントリを落とす** (fail-closed)。カタログ由来は
// 未配線でも `url` 抜きで残すが、こちらは「まだセンシティブでないか」を
// 判断する術がないので、配線されていない環境で出すわけにいかない。
//
// It changes only the test-only default Packer; a Packer built by NewPacker
// keeps its own dependencies.
func SetEmojiDecorationLookup(l EmojiDecorationLookup) {
	updateDefaultPacker(func(p *Packer) { p.emojiDecoration = l })
}

// SetMediaURLContext wires the media-proxy URL rewriter. Pass nil to clear.
//
// It changes only the test-only default Packer; a Packer built by NewPacker
// keeps its own dependencies.
func SetMediaURLContext(c *MediaURLContext) {
	updateDefaultPacker(func(p *Packer) { p.mediaURL = c })
}

// SetUserRolesLookup wires the role lookup used by PackUserLite and
// PackUserDetailed to populate the `roles` / `badgeRoles` fields. Pass nil
// to clear.
//
// It changes only the test-only default Packer; a Packer built by NewPacker
// keeps its own dependencies.
func SetUserRolesLookup(l UserRolesLookup) {
	updateDefaultPacker(func(p *Packer) { p.userRoles = l })
}

// SetShowRemoteBadgesLookup wires the meta.showRoleBadgesOfRemoteUsers
// accessor used by packBadgeRoles (#1653)。Pass nil to clear.
//
// It changes only the test-only default Packer; a Packer built by NewPacker
// keeps its own dependencies.
func SetShowRemoteBadgesLookup(fn func() bool) {
	updateDefaultPacker(func(p *Packer) { p.showRemoteBadges = fn })
}

// SetInstanceIconURLLookup wires the meta.iconUrl accessor used by IdenticonURL
// for the local-system-account avatarUrl special case (#1781)。Pass nil to clear.
//
// It changes only the test-only default Packer; a Packer built by NewPacker
// keeps its own dependencies.
func SetInstanceIconURLLookup(fn func() string) {
	updateDefaultPacker(func(p *Packer) { p.instanceIconURL = fn })
}

// SetSilencedLookup wires the role-policy lookup used by PackUserDetailed to
// derive isSilenced. Pass nil to clear.
//
// It changes only the test-only default Packer; a Packer built by NewPacker
// keeps its own dependencies.
func SetSilencedLookup(l SilencedLookup) {
	updateDefaultPacker(func(p *Packer) { p.silenced = l })
}
